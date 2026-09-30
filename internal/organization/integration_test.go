package organization_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"

	"llm-gateway/internal/iam"
	"llm-gateway/internal/observability"
	"llm-gateway/internal/organization"
	"llm-gateway/migrations"
)

// isolatedDSN 为每个测试建独立 schema 并迁移到最新版本；永远不会碰业务库。
func isolatedDSN(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("IAM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set IAM_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("IAM_TEST_POSTGRES_DSN must be a PostgreSQL URL")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	id, err := observability.ResolveRequestID("")
	if err != nil {
		t.Fatal(err)
	}
	schema := "org_test_" + id
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	scoped := u.String()
	if err := migrations.Up(scoped); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, scoped
}

// createUser 通过 iam 服务建真实用户行（organization 依赖 users 外键）。
func createUser(t *testing.T, db *sql.DB, email string) iam.User {
	t.Helper()
	svc := iam.NewService(db, iam.Options{BcryptCost: bcrypt.MinCost})
	user, err := svc.CreateUser(context.Background(), email, "correct horse battery staple", false)
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return user
}

func countPersonalOrgs(t *testing.T, db *sql.DB, userID int64) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM organizations WHERE type = 'personal' AND owner_user_id = $1`, userID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestEnsurePersonalOrganizationIsIdempotentAndConcurrencySafe(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	user := createUser(t, db, "alice@example.com")

	// 两个 goroutine 同时首次创建：部分唯一索引 + ON CONFLICT 只允许留下一行。
	const goroutines = 2
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		results []organization.Organization
		errs    []error
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			org, err := svc.EnsurePersonalOrganization(ctx, user.ID, "Alice")
			mu.Lock()
			defer mu.Unlock()
			results = append(results, org)
			errs = append(errs, err)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if len(results) != goroutines {
		t.Fatalf("got %d results, want %d", len(results), goroutines)
	}
	if results[0].ID != results[1].ID {
		t.Fatalf("concurrent calls returned different organizations: %d vs %d", results[0].ID, results[1].ID)
	}
	if got := countPersonalOrgs(t, db, user.ID); got != 1 {
		t.Fatalf("personal organizations = %d, want exactly 1 row", got)
	}
	first := results[0]
	if first.Type != "personal" || first.Status != "active" || first.Name != "Alice" {
		t.Fatalf("unexpected personal organization: %+v", first)
	}
	// 个人组织也必须带 owner 成员关系，否则列表与按 id 查询都会看不到它。
	personalMembership, err := svc.Membership(ctx, user.ID, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if personalMembership.Role != "owner" || personalMembership.Status != "active" {
		t.Fatalf("personal membership = %+v, want role owner/status active", personalMembership)
	}
	if visible, err := svc.GetForUser(ctx, user.ID, first.ID); err != nil || visible.Role != "owner" {
		t.Fatalf("GetForUser(personal) = (%+v, %v), want role owner", visible, err)
	}

	// 顺序重复调用必须返回同一行（幂等），不会写入第二行。
	again, err := svc.EnsurePersonalOrganization(ctx, user.ID, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatalf("repeat call returned id %d, want %d", again.ID, first.ID)
	}
	if got := countPersonalOrgs(t, db, user.ID); got != 1 {
		t.Fatalf("personal organizations after repeat = %d, want 1", got)
	}

	// 空 displayName 用 email local part 兜底。
	bob := createUser(t, db, "bob@example.com")
	bobOrg, err := svc.EnsurePersonalOrganization(ctx, bob.ID, "   ")
	if err != nil {
		t.Fatal(err)
	}
	if bobOrg.Name != "bob" {
		t.Fatalf("personal org name = %q, want email local part %q", bobOrg.Name, "bob")
	}

	if _, err := svc.EnsurePersonalOrganization(ctx, 9999999, "Ghost"); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("unknown user = %v, want ErrNotFound", err)
	}
	long := make([]rune, 81)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := svc.EnsurePersonalOrganization(ctx, user.ID, string(long)); !errors.Is(err, organization.ErrNameInvalid) {
		t.Fatalf("81-rune display name = %v, want ErrNameInvalid", err)
	}
}

func TestCreateEnterpriseAddsOwnerMembership(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	owner := createUser(t, db, "owner@example.com")

	org, err := svc.CreateEnterprise(ctx, owner.ID, "  Acme  ")
	if err != nil {
		t.Fatal(err)
	}
	if org.Type != "enterprise" || org.Name != "Acme" || org.Status != "active" || org.LegacyTenantID != nil {
		t.Fatalf("unexpected enterprise organization: %+v", org)
	}

	membership, err := svc.Membership(ctx, owner.ID, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if membership.Role != "owner" || membership.Status != "active" {
		t.Fatalf("owner membership = %+v, want role owner/status active", membership)
	}

	listed, next, err := svc.ListForUser(ctx, owner.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Organization.ID != org.ID || listed[0].Role != "owner" {
		t.Fatalf("ListForUser = %+v (next %q), want the created organization as owner", listed, next)
	}
	if next != "" {
		t.Fatalf("next cursor = %q, want empty", next)
	}

	if _, err := svc.CreateEnterprise(ctx, owner.ID, "   "); !errors.Is(err, organization.ErrNameInvalid) {
		t.Fatalf("blank name = %v, want ErrNameInvalid", err)
	}
	if _, err := svc.CreateEnterprise(ctx, 9999999, "Ghost Inc"); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("unknown owner = %v, want ErrNotFound", err)
	}
}

func TestUserInTwoOrganizationsIsListedCorrectly(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	alice := createUser(t, db, "alice@example.com")
	bob := createUser(t, db, "bob@example.com")

	personal, err := svc.EnsurePersonalOrganization(ctx, alice.ID, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	acme, err := svc.CreateEnterprise(ctx, alice.ID, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := svc.CreateEnterprise(ctx, alice.ID, "Beta")
	if err != nil {
		t.Fatal(err)
	}

	// 固定按 id ASC 排序。
	listed, next, err := svc.ListForUser(ctx, alice.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if next != "" {
		t.Fatalf("next cursor = %q, want empty", next)
	}
	wantIDs := []int64{personal.ID, acme.ID, beta.ID}
	if len(listed) != len(wantIDs) {
		t.Fatalf("listed %d organizations, want %d: %+v", len(listed), len(wantIDs), listed)
	}
	for i, want := range wantIDs {
		if listed[i].Organization.ID != want {
			t.Fatalf("item %d = %d, want %d (id ASC)", i, listed[i].Organization.ID, want)
		}
		if listed[i].Role != "owner" {
			t.Fatalf("item %d role = %q, want owner", i, listed[i].Role)
		}
	}

	// 非成员看不到组织，也不能推断存在性。
	if _, err := svc.GetForUser(ctx, bob.ID, acme.ID); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("non-member GetForUser = %v, want ErrNotFound", err)
	}
	if _, err := svc.Membership(ctx, bob.ID, acme.ID); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("non-member Membership = %v, want ErrNotFound", err)
	}
	bobOrgs, bobNext, err := svc.ListForUser(ctx, bob.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if bobOrgs == nil || len(bobOrgs) != 0 || bobNext != "" {
		t.Fatalf("non-member list = %+v (next %q), want empty non-nil slice", bobOrgs, bobNext)
	}
	if _, err := svc.GetForUser(ctx, alice.ID, 9999999); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("unknown organization = %v, want ErrNotFound", err)
	}

	// 加入第二个组织后，两个用户各自看到自己的组织。
	member, err := svc.AddMember(ctx, acme.ID, bob.ID, "member")
	if err != nil {
		t.Fatal(err)
	}
	if member.Role != "member" || member.Status != "active" || member.OrgID != acme.ID || member.UserID != bob.ID {
		t.Fatalf("membership = %+v", member)
	}
	if got, err := svc.GetForUser(ctx, bob.ID, acme.ID); err != nil || got.Role != "member" {
		t.Fatalf("GetForUser after AddMember = (%+v, %v), want role member", got, err)
	}
	bobOrgs, _, err = svc.ListForUser(ctx, bob.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(bobOrgs) != 1 || bobOrgs[0].Organization.ID != acme.ID {
		t.Fatalf("bob organizations = %+v, want only Acme", bobOrgs)
	}

	// upsert：重复添加改角色并保持 created_at，不会产生第二行。
	time.Sleep(2 * time.Millisecond)
	updated, err := svc.AddMember(ctx, acme.ID, bob.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Role != "admin" {
		t.Fatalf("upserted role = %q, want admin", updated.Role)
	}
	if !updated.CreatedAt.Equal(member.CreatedAt) {
		t.Fatalf("created_at changed on upsert: %v -> %v", member.CreatedAt, updated.CreatedAt)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM memberships WHERE org_id = $1 AND user_id = $2`, acme.ID, bob.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("membership rows = %d, want 1", rows)
	}

	if _, err := svc.AddMember(ctx, acme.ID, bob.ID, "superuser"); err == nil {
		t.Fatal("AddMember(invalid role) = nil, want validation error")
	}
	if _, err := svc.AddMember(ctx, 9999999, bob.ID, "member"); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("AddMember(unknown org) = %v, want ErrNotFound", err)
	}
	if _, err := svc.AddMember(ctx, acme.ID, 9999999, "member"); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("AddMember(unknown user) = %v, want ErrNotFound", err)
	}
}

func TestListForUserLimitAndCursor(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	user := createUser(t, db, "alice@example.com")

	var created []int64
	for i := 0; i < 5; i++ {
		org, err := svc.CreateEnterprise(ctx, user.ID, "Org "+string(rune('A'+i)))
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, org.ID)
	}

	var paged []int64
	cursor := int64(0)
	pages := 0
	for {
		items, next, err := svc.ListForUser(ctx, user.ID, 2, cursor)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, item := range items {
			paged = append(paged, item.Organization.ID)
		}
		if next == "" {
			break
		}
		cursor, err = parseCursor(t, next)
		if err != nil {
			t.Fatal(err)
		}
		if pages > 5 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 3 {
		t.Fatalf("pages = %d, want 3 (5 items at limit 2)", pages)
	}
	if len(paged) != len(created) {
		t.Fatalf("paged %d organizations, want %d", len(paged), len(created))
	}
	for i := range created {
		if paged[i] != created[i] {
			t.Fatalf("paged[%d] = %d, want %d", i, paged[i], created[i])
		}
	}

	// limit <= 0 -> 默认 20；limit > 100 -> 100。两者都应一页装下 5 条。
	for _, limit := range []int{0, -3, 1000} {
		items, next, err := svc.ListForUser(ctx, user.ID, limit, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 5 || next != "" {
			t.Fatalf("limit %d returned %d items (next %q), want 5 and no cursor", limit, len(items), next)
		}
	}

	// cursor 是上一页最后一个 id：用最后一个 id 取下一页必然为空。
	items, next, err := svc.ListForUser(ctx, user.ID, 2, created[len(created)-1])
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 || next != "" {
		t.Fatalf("page after last id = %+v (next %q), want empty", items, next)
	}
}

func TestMembershipRemovedIsNotActive(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	alice := createUser(t, db, "alice@example.com")
	bob := createUser(t, db, "bob@example.com")
	org, err := svc.CreateEnterprise(ctx, alice.ID, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddMember(ctx, org.ID, bob.ID, "member"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE memberships SET status = 'removed' WHERE org_id = $1 AND user_id = $2`, org.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Membership(ctx, bob.ID, org.ID); !errors.Is(err, organization.ErrNotMember) {
		t.Fatalf("removed membership = %v, want ErrNotMember", err)
	}
	if _, err := svc.GetForUser(ctx, bob.ID, org.ID); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("removed member GetForUser = %v, want ErrNotFound", err)
	}
	items, _, err := svc.ListForUser(ctx, bob.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("removed membership still listed: %+v", items)
	}
}

func TestLinkLegacyTenant(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()
	alice := createUser(t, db, "alice@example.com")
	acme, err := svc.CreateEnterprise(ctx, alice.ID, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := svc.CreateEnterprise(ctx, alice.ID, "Beta")
	if err != nil {
		t.Fatal(err)
	}

	linked, err := svc.LinkLegacyTenant(ctx, acme.ID, " tenant-legacy-a ")
	if err != nil {
		t.Fatal(err)
	}
	if linked.LegacyTenantID == nil || *linked.LegacyTenantID != "tenant-legacy-a" {
		t.Fatalf("LegacyTenantID = %v, want tenant-legacy-a", linked.LegacyTenantID)
	}
	// 幂等：同一个组织再映射同一个 tenant 是允许的。
	if _, err := svc.LinkLegacyTenant(ctx, acme.ID, "tenant-legacy-a"); err != nil {
		t.Fatalf("idempotent relink = %v, want nil", err)
	}
	// 唯一索引兜底：同一 tenant 不能映射到第二个组织。
	if _, err := svc.LinkLegacyTenant(ctx, beta.ID, "tenant-legacy-a"); err == nil {
		t.Fatal("conflicting link = nil, want conflict error")
	} else if errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("conflicting link = %v, want a conflict (not ErrNotFound)", err)
	}
	var legacy sql.NullString
	if err := db.QueryRow(`SELECT legacy_tenant_id FROM organizations WHERE id = $1`, beta.ID).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Valid {
		t.Fatalf("conflicting link wrote %q to the second organization", legacy.String)
	}
	// 一个组织不能被改映射到别的 tenant。
	if _, err := svc.LinkLegacyTenant(ctx, acme.ID, "tenant-legacy-b"); err == nil {
		t.Fatal("re-link to another tenant = nil, want conflict error")
	}
	if _, err := svc.LinkLegacyTenant(ctx, 9999999, "tenant-legacy-c"); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("unknown organization = %v, want ErrNotFound", err)
	}
	if _, err := svc.LinkLegacyTenant(ctx, beta.ID, "   "); err == nil {
		t.Fatal("blank tenant = nil, want validation error")
	}
	// 迁移不伪造任何行：legacy 映射只写列，不建用户/成员。
	var members int
	if err := db.QueryRow(`SELECT count(*) FROM memberships WHERE org_id = $1`, acme.ID).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if members != 1 {
		t.Fatalf("memberships for linked org = %d, want only the creator", members)
	}
}

func parseCursor(t *testing.T, cursor string) (int64, error) {
	t.Helper()
	return strconv.ParseInt(cursor, 10, 64)
}
