package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/appenv"
	"llm-gateway/internal/iam"
	"llm-gateway/internal/organization"
	"llm-gateway/migrations"
)

// Demo data contract (ADR-0002 §5): two enterprises, three members each. The
// owner is the platform administrator; the other two are one `admin` and one
// ordinary `member`. Trusted bootstrap AddMember writes a nil monthly cap, which
// explicitly means unlimited here; member-facing HTTP assignment requires a cap.
const (
	demoEnterpriseA = "demo-enterprise-a"
	demoEnterpriseB = "demo-enterprise-b"

	defaultDemoCredentialsFile = "data/console-demo-credentials.txt"
	demoPasswordBytes          = 24
	credentialsFileMode        = 0o600
)

type demoEnterprise struct {
	name  string
	users []demoUser
}

type demoUser struct {
	email string
	role  string
}

func demoEnterprises() []demoEnterprise {
	return []demoEnterprise{
		{name: demoEnterpriseA, users: []demoUser{
			{email: "demo-enterprise-a-admin@example.com", role: "admin"},
			{email: "demo-enterprise-a-member@example.com", role: "member"},
		}},
		{name: demoEnterpriseB, users: []demoUser{
			{email: "demo-enterprise-b-admin@example.com", role: "admin"},
			{email: "demo-enterprise-b-member@example.com", role: "member"},
		}},
	}
}

// bootstrapUsers is the slice of internal/iam.Service bootstrap needs. It is
// declared here so the bootstrap flow can be tested without PostgreSQL.
type bootstrapUsers interface {
	FindUserByEmail(ctx context.Context, email string) (iam.User, error)
	CreateUser(ctx context.Context, email, password string, platformAdmin bool) (iam.User, error)
}

// bootstrapOrganizations is the slice of internal/organization.Service bootstrap
// needs.
type bootstrapOrganizations interface {
	EnsurePersonalOrganization(ctx context.Context, userID int64, displayName string) (organization.Organization, error)
	CreateEnterprise(ctx context.Context, ownerUserID int64, name string) (organization.Organization, error)
	ListForUser(ctx context.Context, userID int64, limit int, cursor int64) ([]organization.MemberOrg, string, error)
	AddMember(ctx context.Context, orgID, userID int64, role string) (organization.Membership, error)
}

type bootstrapOptions struct {
	adminEmail    string
	adminPassword string
	demoData      bool
	// credentialsFile receives the generated demo passwords. It is written at
	// most once: an existing file is never overwritten.
	credentialsFile string
	// randomPassword is injectable for tests; nil means crypto/rand.
	randomPassword func() (string, error)
	now            func() time.Time
}

// bootstrapResult is what the operator is allowed to see: identities, counts and
// the path of the one-time credentials file. Never a password.
type bootstrapResult struct {
	AdminEmail          string
	AdminUserID         int64
	AdminCreated        bool
	PersonalOrgID       int64
	EnterprisesCreated  int
	EnterprisesReused   int
	DemoUsersCreated    int
	DemoUsersReused     int
	MembershipsUpserted int
	CredentialsFile     string
	CredentialsWritten  bool
	CredentialsSkipped  string
}

// runBootstrap loads config, opens the database and runs the idempotent
// bootstrap. It refuses demo data under prod: seeding a production database
// with known-shape demo organizations is not an acceptable accident.
func runBootstrap(profile appenv.Profile, args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	adminEmail := fs.String("admin-email", "", "platform administrator email (required)")
	adminPasswordFile := fs.String("admin-password-file", "", "file holding the platform administrator password (required when the user does not exist yet)")
	demoData := fs.Bool("demo-data", false, "also create the two demo enterprises and their demo users (refused under prod)")
	credentialsFile := fs.String("demo-credentials-file", defaultDemoCredentialsFile, "where the generated demo passwords are written once, mode 0600")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	email := strings.TrimSpace(*adminEmail)
	if email == "" {
		return errors.New("--admin-email is required")
	}
	if *demoData && profile.IsProduction() {
		return errors.New("--demo-data is refused under the prod profile: demo organizations and generated passwords must never be seeded into a production database")
	}

	password := ""
	if *adminPasswordFile != "" {
		raw, err := os.ReadFile(*adminPasswordFile)
		if err != nil {
			return fmt.Errorf("read --admin-password-file: %w", err)
		}
		password = strings.TrimRight(string(raw), "\r\n")
		if strings.TrimSpace(password) == "" {
			return errors.New("--admin-password-file is empty")
		}
	}

	cfg, err := loadConfig(profile)
	if err != nil {
		return err
	}
	db, err := openDB(cfg.PostgresDSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := migrations.Check(db); err != nil {
		return err
	}

	users := iam.NewService(db, iam.Options{SessionTTL: cfg.SessionTTL})
	orgs := organization.NewService(db)

	result, err := bootstrap(context.Background(), users, orgs, bootstrapOptions{
		adminEmail:      email,
		adminPassword:   password,
		demoData:        *demoData,
		credentialsFile: *credentialsFile,
	})
	if err != nil {
		return err
	}
	reportBootstrap(profile, result)
	return nil
}

// bootstrap is the idempotent core. Running it twice must not create a second
// personal organization, a second demo enterprise, or rewrite the credentials
// file.
func bootstrap(ctx context.Context, users bootstrapUsers, orgs bootstrapOrganizations, opts bootstrapOptions) (bootstrapResult, error) {
	result := bootstrapResult{AdminEmail: opts.adminEmail}

	admin, err := users.FindUserByEmail(ctx, opts.adminEmail)
	switch {
	case errors.Is(err, iam.ErrNotFound):
		if opts.adminPassword == "" {
			return bootstrapResult{}, fmt.Errorf("user %q does not exist yet: --admin-password-file is required to create it", opts.adminEmail)
		}
		admin, err = users.CreateUser(ctx, opts.adminEmail, opts.adminPassword, true)
		if err != nil {
			return bootstrapResult{}, fmt.Errorf("create platform administrator: %w", err)
		}
		result.AdminCreated = true
	case err != nil:
		return bootstrapResult{}, fmt.Errorf("look up platform administrator: %w", err)
	default:
		if !admin.PlatformAdmin {
			return bootstrapResult{}, fmt.Errorf("user %q already exists but is not a platform administrator; bootstrap will not silently promote an existing account", opts.adminEmail)
		}
	}
	result.AdminUserID = admin.ID

	personal, err := orgs.EnsurePersonalOrganization(ctx, admin.ID, admin.Email)
	if err != nil {
		return bootstrapResult{}, fmt.Errorf("ensure personal organization: %w", err)
	}
	result.PersonalOrgID = personal.ID

	if !opts.demoData {
		return result, nil
	}

	now := opts.now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	generate := opts.randomPassword
	if generate == nil {
		generate = randomPassword
	}

	existing, err := listAllOrganizations(ctx, orgs, admin.ID)
	if err != nil {
		return bootstrapResult{}, err
	}

	credentials := make([]demoCredential, 0, 4)
	for _, enterprise := range demoEnterprises() {
		org, created, err := ensureEnterprise(ctx, orgs, admin.ID, enterprise.name, existing)
		if err != nil {
			return bootstrapResult{}, err
		}
		if created {
			result.EnterprisesCreated++
			existing[enterprise.name] = org.ID
		} else {
			result.EnterprisesReused++
		}

		for _, demo := range enterprise.users {
			user, userCreated, err := ensureDemoUser(ctx, users, generate, demo.email)
			if err != nil {
				return bootstrapResult{}, err
			}
			if userCreated {
				result.DemoUsersCreated++
				credentials = append(credentials, demoCredential{Email: demo.email, Password: user.password})
			} else {
				result.DemoUsersReused++
			}
			if _, err := orgs.EnsurePersonalOrganization(ctx, user.user.ID, user.user.Email); err != nil {
				return bootstrapResult{}, fmt.Errorf("ensure personal organization for %s: %w", demo.email, err)
			}
			// This trusted seed path deliberately keeps monthly_limit_micro nil (unlimited).
			if _, err := orgs.AddMember(ctx, org.ID, user.user.ID, demo.role); err != nil {
				return bootstrapResult{}, fmt.Errorf("add %s to %s: %w", demo.email, enterprise.name, err)
			}
			result.MembershipsUpserted++
		}
	}

	if len(credentials) == 0 {
		result.CredentialsSkipped = "no demo user was created in this run; existing passwords are unchanged"
		return result, nil
	}
	path, written, err := writeDemoCredentials(opts.credentialsFile, now(), credentials)
	if err != nil {
		return bootstrapResult{}, err
	}
	result.CredentialsFile = path
	result.CredentialsWritten = written
	return result, nil
}

type demoCredential struct {
	Email    string
	Password string
}

type demoUserResult struct {
	user     iam.User
	password string
}

// listAllOrganizations reads every organization the administrator can see, so
// an existing demo enterprise can be reused by name. The frozen organization
// service has no lookup-by-name, and adding one is out of scope for T02.
func listAllOrganizations(ctx context.Context, orgs bootstrapOrganizations, userID int64) (map[string]int64, error) {
	byName := map[string]int64{}
	cursor := int64(0)
	for {
		items, next, err := orgs.ListForUser(ctx, userID, 100, cursor)
		if err != nil {
			return nil, fmt.Errorf("list organizations: %w", err)
		}
		for _, item := range items {
			byName[item.Organization.Name] = item.Organization.ID
		}
		if strings.TrimSpace(next) == "" {
			return byName, nil
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(next), 10, 64)
		if err != nil || parsed <= cursor {
			// Defensive: a cursor that does not move forward would loop forever.
			return byName, nil
		}
		cursor = parsed
	}
}

func ensureEnterprise(ctx context.Context, orgs bootstrapOrganizations, ownerUserID int64, name string, existing map[string]int64) (organization.Organization, bool, error) {
	if id, ok := existing[name]; ok {
		return organization.Organization{ID: id, Name: name}, false, nil
	}
	org, err := orgs.CreateEnterprise(ctx, ownerUserID, name)
	if err != nil {
		return organization.Organization{}, false, fmt.Errorf("create enterprise %s: %w", name, err)
	}
	return org, true, nil
}

func ensureDemoUser(ctx context.Context, users bootstrapUsers, generate func() (string, error), email string) (demoUserResult, bool, error) {
	user, err := users.FindUserByEmail(ctx, email)
	if err == nil {
		return demoUserResult{user: user}, false, nil
	}
	if !errors.Is(err, iam.ErrNotFound) {
		return demoUserResult{}, false, fmt.Errorf("look up demo user %s: %w", email, err)
	}
	password, err := generate()
	if err != nil {
		return demoUserResult{}, false, fmt.Errorf("generate demo password: %w", err)
	}
	created, err := users.CreateUser(ctx, email, password, false)
	if err != nil {
		return demoUserResult{}, false, fmt.Errorf("create demo user %s: %w", email, err)
	}
	return demoUserResult{user: created, password: password}, true, nil
}

// writeDemoCredentials writes the generated passwords exactly once, with mode
// 0600. An existing file is never truncated: a re-run cannot invalidate the
// credentials an operator already saved.
func writeDemoCredentials(path string, now time.Time, credentials []demoCredential) (string, bool, error) {
	clean := strings.TrimSpace(path)
	if clean == "" {
		return "", false, nil
	}
	if dir := filepath.Dir(clean); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", false, fmt.Errorf("create credentials directory: %w", err)
		}
	}
	file, err := os.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_EXCL, credentialsFileMode)
	if errors.Is(err, os.ErrExist) {
		return clean, false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("create credentials file: %w", err)
	}
	defer func() { _ = file.Close() }()

	var builder strings.Builder
	fmt.Fprintf(&builder, "# Console demo credentials, generated %s.\n", now.UTC().Format(time.RFC3339))
	builder.WriteString("# Written once with mode 0600; do not commit this file.\n")
	for _, credential := range credentials {
		fmt.Fprintf(&builder, "%s\t%s\n", credential.Email, credential.Password)
	}
	if _, err := file.WriteString(builder.String()); err != nil {
		return "", false, fmt.Errorf("write credentials file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", false, fmt.Errorf("sync credentials file: %w", err)
	}
	return clean, true, nil
}

// randomPassword returns a 24-byte URL-safe random password.
func randomPassword() (string, error) {
	buf := make([]byte, demoPasswordBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// reportBootstrap prints identities, counts and paths. It never prints a
// password: the generated ones exist only in the 0600 file.
func reportBootstrap(profile appenv.Profile, result bootstrapResult) {
	log.Printf("console bootstrap: profile=%s admin_email=%s admin_user_id=%d admin_created=%t",
		profile, result.AdminEmail, result.AdminUserID, result.AdminCreated)
	log.Printf("console bootstrap: personal_organization_id=%d", result.PersonalOrgID)
	if result.EnterprisesCreated == 0 && result.EnterprisesReused == 0 {
		return
	}
	log.Printf("console bootstrap: enterprises created=%d reused=%d demo_users created=%d reused=%d memberships_upserted=%d",
		result.EnterprisesCreated, result.EnterprisesReused, result.DemoUsersCreated, result.DemoUsersReused, result.MembershipsUpserted)
	switch {
	case result.CredentialsWritten:
		log.Printf("console bootstrap: demo credentials written once to %s (mode 0600); passwords are not printed or logged", result.CredentialsFile)
	case result.CredentialsFile != "":
		log.Printf("console bootstrap: demo credentials file %s already exists and was left untouched", result.CredentialsFile)
	}
	if result.CredentialsSkipped != "" {
		log.Printf("console bootstrap: %s", result.CredentialsSkipped)
	}
}
