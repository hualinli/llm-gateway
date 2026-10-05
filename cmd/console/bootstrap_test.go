package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/iam"
	"llm-gateway/internal/organization"
)

// ---------------------------------------------------------------------------
// Fakes for the bootstrap flow (the concrete services are exercised by the
// PG-gated end-to-end test in console_e2e_test.go).
// ---------------------------------------------------------------------------

type fakeUsers struct {
	mu        sync.Mutex
	nextID    int64
	byMail    map[string]iam.User
	byID      map[int64]iam.User
	passwords map[string]string
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{nextID: 1, byMail: map[string]iam.User{}, byID: map[int64]iam.User{}, passwords: map[string]string{}}
}

func (f *fakeUsers) FindUserByEmail(_ context.Context, email string) (iam.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user, ok := f.byMail[strings.ToLower(email)]
	if !ok {
		return iam.User{}, iam.ErrNotFound
	}
	return user, nil
}

func (f *fakeUsers) CreateUser(_ context.Context, email, password string, platformAdmin bool) (iam.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.ToLower(email)
	if _, exists := f.byMail[key]; exists {
		return iam.User{}, iam.ErrEmailTaken
	}
	user := iam.User{ID: f.nextID, Email: key, Status: "active", PlatformAdmin: platformAdmin, AuthVersion: 1, CreatedAt: time.Now().UTC()}
	f.nextID++
	f.byMail[key] = user
	f.byID[user.ID] = user
	f.passwords[key] = password
	return user, nil
}

func (f *fakeUsers) password(email string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.passwords[strings.ToLower(email)]
}

type fakeOrgs struct {
	mu      sync.Mutex
	nextID  int64
	byName  map[string]organization.Organization
	members map[int64][]organization.Membership
}

func newFakeOrgs() *fakeOrgs {
	return &fakeOrgs{nextID: 1, byName: map[string]organization.Organization{}, members: map[int64][]organization.Membership{}}
}

func (f *fakeOrgs) EnsurePersonalOrganization(_ context.Context, userID int64, displayName string) (organization.Organization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := "personal:" + displayName
	if org, ok := f.byName[key]; ok {
		return org, nil
	}
	org := organization.Organization{ID: f.nextID, Type: "personal", Name: displayName, Status: "active", CreatedAt: time.Now().UTC()}
	f.nextID++
	f.byName[key] = org
	f.members[org.ID] = append(f.members[org.ID], organization.Membership{OrgID: org.ID, UserID: userID, Role: "owner", Status: "active"})
	return org, nil
}

func (f *fakeOrgs) CreateEnterprise(_ context.Context, ownerUserID int64, name string) (organization.Organization, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	org := organization.Organization{ID: f.nextID, Type: "enterprise", Name: name, Status: "active", CreatedAt: time.Now().UTC()}
	f.nextID++
	f.byName[name] = org
	f.members[org.ID] = append(f.members[org.ID], organization.Membership{OrgID: org.ID, UserID: ownerUserID, Role: "owner", Status: "active"})
	return org, nil
}

func (f *fakeOrgs) ListForUser(_ context.Context, userID int64, limit int, cursor int64) ([]organization.MemberOrg, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var items []organization.MemberOrg
	for _, org := range f.byName {
		for _, membership := range f.members[org.ID] {
			if membership.UserID == userID && org.ID > cursor {
				items = append(items, organization.MemberOrg{Organization: org, Role: membership.Role})
			}
		}
	}
	return items, "", nil
}

func (f *fakeOrgs) AddMember(_ context.Context, orgID, userID int64, role string) (organization.Membership, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, membership := range f.members[orgID] {
		if membership.UserID == userID {
			f.members[orgID][i].Role = role
			return f.members[orgID][i], nil
		}
	}
	membership := organization.Membership{OrgID: orgID, UserID: userID, Role: role, Status: "active"}
	f.members[orgID] = append(f.members[orgID], membership)
	return membership, nil
}

func (f *fakeOrgs) memberCount(orgName string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	org, ok := f.byName[orgName]
	if !ok {
		return 0
	}
	return len(f.members[org.ID])
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestBootstrapDemoDataCreatesTwoEnterprisesWithThreeMembers(t *testing.T) {
	users, orgs := newFakeUsers(), newFakeOrgs()
	credentials := filepath.Join(t.TempDir(), "demo-credentials.txt")

	result, err := bootstrap(context.Background(), users, orgs, bootstrapOptions{
		adminEmail:      "admin@example.com",
		adminPassword:   "admin-password",
		demoData:        true,
		credentialsFile: credentials,
		now:             func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.AdminCreated || result.AdminUserID == 0 {
		t.Fatalf("admin result=%+v", result)
	}
	if result.PersonalOrgID == 0 {
		t.Fatal("bootstrap must ensure the administrator's personal organization")
	}
	if result.EnterprisesCreated != 2 || result.EnterprisesReused != 0 {
		t.Fatalf("enterprises created=%d reused=%d, want 2/0", result.EnterprisesCreated, result.EnterprisesReused)
	}
	if result.DemoUsersCreated != 4 || result.MembershipsUpserted != 4 {
		t.Fatalf("demo users created=%d memberships=%d, want 4/4", result.DemoUsersCreated, result.MembershipsUpserted)
	}
	for _, name := range []string{demoEnterpriseA, demoEnterpriseB} {
		if got := orgs.memberCount(name); got != 3 {
			t.Fatalf("%s has %d members, want 3 (owner + admin + billing_viewer)", name, got)
		}
	}

	// The credentials file is 0600, contains the generated demo passwords and
	// nothing is printed.
	info, err := os.Stat(credentials)
	if err != nil {
		t.Fatalf("credentials file: %v", err)
	}
	if info.Mode().Perm() != credentialsFileMode {
		t.Fatalf("credentials mode=%#o, want %#o", info.Mode().Perm(), credentialsFileMode)
	}
	raw, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, email := range []string{
		"demo-enterprise-a-admin@example.com",
		"demo-enterprise-a-billing@example.com",
		"demo-enterprise-b-admin@example.com",
		"demo-enterprise-b-billing@example.com",
	} {
		if !strings.Contains(content, email) {
			t.Fatalf("credentials file does not list %s:\n%s", email, content)
		}
		password := users.password(email)
		if password == "" {
			t.Fatalf("no password was recorded for %s", email)
		}
		if !strings.Contains(content, password) {
			t.Fatalf("credentials file does not contain the password for %s", email)
		}
		if len(password) < 24 {
			t.Fatalf("generated password for %s is too short (%d chars)", email, len(password))
		}
	}
	if strings.Count(content, "\t") != 4 {
		t.Fatalf("expected exactly 4 credential lines:\n%s", content)
	}
}

func TestBootstrapIsIdempotent(t *testing.T) {
	users, orgs := newFakeUsers(), newFakeOrgs()
	credentials := filepath.Join(t.TempDir(), "demo-credentials.txt")
	options := bootstrapOptions{
		adminEmail:      "admin@example.com",
		adminPassword:   "admin-password",
		demoData:        true,
		credentialsFile: credentials,
	}

	first, err := bootstrap(context.Background(), users, orgs, options)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatal(err)
	}

	// Second run: no password is available and none should be needed.
	second, err := bootstrap(context.Background(), users, orgs, bootstrapOptions{
		adminEmail:      "admin@example.com",
		demoData:        true,
		credentialsFile: credentials,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.AdminCreated {
		t.Fatal("second run must reuse the existing administrator")
	}
	if second.AdminUserID != first.AdminUserID || second.PersonalOrgID != first.PersonalOrgID {
		t.Fatalf("second run changed identities: first=%+v second=%+v", first, second)
	}
	if second.EnterprisesCreated != 0 || second.EnterprisesReused != 2 {
		t.Fatalf("second run created=%d reused=%d, want 0/2", second.EnterprisesCreated, second.EnterprisesReused)
	}
	if second.DemoUsersCreated != 0 || second.DemoUsersReused != 4 {
		t.Fatalf("second run demo users created=%d reused=%d, want 0/4", second.DemoUsersCreated, second.DemoUsersReused)
	}
	for _, name := range []string{demoEnterpriseA, demoEnterpriseB} {
		if got := orgs.memberCount(name); got != 3 {
			t.Fatalf("%s has %d members after the second run, want 3", name, got)
		}
	}
	after, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("the credentials file must be written at most once")
	}
	if second.CredentialsWritten {
		t.Fatal("second run must not claim to have written credentials")
	}
}

func TestBootstrapRequiresAdminPasswordOnlyForNewUsers(t *testing.T) {
	users, orgs := newFakeUsers(), newFakeOrgs()
	_, err := bootstrap(context.Background(), users, orgs, bootstrapOptions{adminEmail: "admin@example.com", demoData: false})
	if err == nil || !strings.Contains(err.Error(), "--admin-password-file") {
		t.Fatalf("err=%v, want an explicit password requirement", err)
	}

	if _, err := bootstrap(context.Background(), users, orgs, bootstrapOptions{
		adminEmail: "admin@example.com", adminPassword: "pw", demoData: false,
	}); err != nil {
		t.Fatal(err)
	}
	// Existing admin: a password file is no longer required.
	if _, err := bootstrap(context.Background(), users, orgs, bootstrapOptions{adminEmail: "admin@example.com"}); err != nil {
		t.Fatalf("existing admin must not require a password: %v", err)
	}
}

func TestBootstrapRefusesNonPlatformAdmin(t *testing.T) {
	users, orgs := newFakeUsers(), newFakeOrgs()
	if _, err := users.CreateUser(context.Background(), "member@example.com", "pw", false); err != nil {
		t.Fatal(err)
	}
	_, err := bootstrap(context.Background(), users, orgs, bootstrapOptions{adminEmail: "member@example.com", adminPassword: "pw"})
	if err == nil || !strings.Contains(err.Error(), "platform administrator") {
		t.Fatalf("err=%v, want a refusal to promote an existing account", err)
	}
}

func TestBootstrapWithoutDemoDataTouchesNothingElse(t *testing.T) {
	users, orgs := newFakeUsers(), newFakeOrgs()
	result, err := bootstrap(context.Background(), users, orgs, bootstrapOptions{
		adminEmail: "admin@example.com", adminPassword: "pw",
		credentialsFile: filepath.Join(t.TempDir(), "creds.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.EnterprisesCreated != 0 || result.DemoUsersCreated != 0 {
		t.Fatalf("bootstrap without --demo-data created demo state: %+v", result)
	}
	if _, err := os.Stat(result.CredentialsFile); !os.IsNotExist(err) {
		t.Fatal("no credentials file may be created without demo users")
	}
}

func TestBootstrapDemoDataRefusedInProduction(t *testing.T) {
	// runBootstrap parses flags and checks the profile before touching the
	// database, so a prod refusal is observable without PostgreSQL.
	err := runBootstrap(appenv.Prod, []string{"--admin-email=admin@example.com", "--demo-data"})
	if err == nil {
		t.Fatal("--demo-data must be refused under prod")
	}
	if !strings.Contains(err.Error(), "prod") {
		t.Fatalf("err=%v, want an explicit prod refusal", err)
	}

	if err := runBootstrap(appenv.Prod, nil); err == nil || !strings.Contains(err.Error(), "--admin-email") {
		t.Fatalf("err=%v, want a missing --admin-email error", err)
	}
}

func TestWriteDemoCredentialsNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "creds.txt")
	first := []demoCredential{{Email: "a@example.com", Password: "first-secret"}}
	written, ok, err := writeDemoCredentials(path, time.Now(), first)
	if err != nil || !ok || written != path {
		t.Fatalf("writeDemoCredentials=%q,%t,%v", written, ok, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != credentialsFileMode {
		t.Fatalf("mode=%#o, want %#o", info.Mode().Perm(), credentialsFileMode)
	}

	_, ok, err = writeDemoCredentials(path, time.Now(), []demoCredential{{Email: "b@example.com", Password: "second-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("an existing credentials file must never be rewritten")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "second-secret") || !strings.Contains(string(raw), "first-secret") {
		t.Fatalf("credentials file was modified:\n%s", raw)
	}
}

func TestRandomPasswordIsLongAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		password, err := randomPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(password) < 24 {
			t.Fatalf("password %q is too short", password)
		}
		if seen[password] {
			t.Fatalf("duplicate generated password %q", password)
		}
		seen[password] = true
	}
}

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		args        []string
		wantCommand string
		wantArgs    int
	}{
		{nil, "serve", 0},
		{[]string{"-h"}, "serve", 1},
		{[]string{"serve"}, "serve", 0},
		{[]string{"bootstrap", "--admin-email=a@b.c"}, "bootstrap", 1},
	}
	for _, tt := range tests {
		command, args := splitCommand(tt.args)
		if command != tt.wantCommand || len(args) != tt.wantArgs {
			t.Fatalf("splitCommand(%v)=(%q,%v), want (%q,%d)", tt.args, command, args, tt.wantCommand, tt.wantArgs)
		}
	}
}
