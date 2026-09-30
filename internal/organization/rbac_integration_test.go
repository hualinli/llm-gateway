package organization_test

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/organization"
)

// T03 的 PostgreSQL 集成测试：真实 SQL、真实事务、真实并发。它们复用
// integration_test.go 的 isolatedDSN / createUser helpers，并在
// IAM_TEST_POSTGRES_DSN 未设置时整包跳过。

// --- helpers ---------------------------------------------------------------

type publicationRow struct {
	ID          int64
	OrgRevision int64
	Scope       string
	TargetID    string
	Status      string
	Attempts    int
	LastError   sql.NullString
	NextRetryAt sql.NullTime
	OperationID sql.NullString
}

func listPublications(t *testing.T, db *sql.DB, orgID int64) []publicationRow {
	t.Helper()
	rows, err := db.Query(`SELECT id, org_revision, scope, target_id, status, attempts, last_error, next_retry_at, operation_id
		FROM policy_publications WHERE org_id = $1 ORDER BY org_revision, id`, orgID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []publicationRow
	for rows.Next() {
		var row publicationRow
		if err := rows.Scan(&row.ID, &row.OrgRevision, &row.Scope, &row.TargetID, &row.Status,
			&row.Attempts, &row.LastError, &row.NextRetryAt, &row.OperationID); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func orgRevisionOf(t *testing.T, db *sql.DB, orgID int64) int64 {
	t.Helper()
	var revision int64
	if err := db.QueryRow(`SELECT org_revision FROM organizations WHERE id = $1`, orgID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}

func activeOwnerCount(t *testing.T, db *sql.DB, orgID int64) int {
	t.Helper()
	var owners int
	if err := db.QueryRow(`SELECT count(*) FROM memberships WHERE org_id = $1 AND role = 'owner' AND status = 'active'`, orgID).Scan(&owners); err != nil {
		t.Fatal(err)
	}
	return owners
}

func membershipRole(t *testing.T, db *sql.DB, orgID, userID int64) (role, status string) {
	t.Helper()
	if err := db.QueryRow(`SELECT role, status FROM memberships WHERE org_id = $1 AND user_id = $2`, orgID, userID).Scan(&role, &status); err != nil {
		t.Fatal(err)
	}
	return role, status
}

// seedEnterprise 建一个企业并把 users 按 roles 加进去（第一个用户是创建者 owner）。
func seedEnterprise(t *testing.T, db *sql.DB, svc *organization.Service, name string, users []int64, roles []string) organization.Organization {
	t.Helper()
	ctx := context.Background()
	org, err := svc.CreateEnterprise(ctx, users[0], name)
	if err != nil {
		t.Fatal(err)
	}
	for i, userID := range users[1:] {
		if _, err := svc.AddMember(ctx, org.ID, userID, roles[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	return org
}

// --- 权限矩阵与可见性 ------------------------------------------------------

func TestListMembersVisibilityInTwoEnterprises(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()

	ownerA := createUser(t, db, "owner-a@example.com")
	adminA := createUser(t, db, "admin-a@example.com")
	memberA := createUser(t, db, "member-a@example.com")
	billingA := createUser(t, db, "billing-a@example.com")
	outsiderB := createUser(t, db, "owner-b@example.com")

	orgA := seedEnterprise(t, db, svc, "Acme", []int64{ownerA.ID, adminA.ID, memberA.ID, billingA.ID},
		[]string{"owner", "admin", "member", "billing_viewer"})
	orgB := seedEnterprise(t, db, svc, "Beta", []int64{outsiderB.ID}, []string{"owner"})

	// owner/admin/billing_viewer 看到完整成员列表（按 user_id ASC）。
	for _, actor := range []struct {
		name string
		id   int64
	}{{"owner", ownerA.ID}, {"admin", adminA.ID}, {"billing_viewer", billingA.ID}} {
		members, err := svc.ListMembers(ctx, actor.id, orgA.ID)
		if err != nil {
			t.Fatalf("%s ListMembers: %v", actor.name, err)
		}
		if len(members) != 4 {
			t.Fatalf("%s sees %d members, want 4: %+v", actor.name, len(members), members)
		}
		if members[0].Email != "owner-a@example.com" || members[0].Role != "owner" {
			t.Fatalf("%s first member = %+v", actor.name, members[0])
		}
	}

	// member 只能看到自己：单元素列表，不是 403（见 ListMembers 文档）。
	members, err := svc.ListMembers(ctx, memberA.ID, orgA.ID)
	if err != nil {
		t.Fatalf("member ListMembers: %v", err)
	}
	if len(members) != 1 || members[0].UserID != memberA.ID || members[0].Role != "member" {
		t.Fatalf("member sees %+v, want only themselves", members)
	}

	// 跨企业读取一律 not found，不泄露存在性（也不泄露成员身份）。
	for _, actorID := range []int64{ownerA.ID, memberA.ID, billingA.ID} {
		if _, err := svc.ListMembers(ctx, actorID, orgB.ID); !errors.Is(err, organization.ErrNotFound) {
			t.Fatalf("cross-org ListMembers = %v, want ErrNotFound", err)
		}
		if _, err := svc.ListInvitations(ctx, actorID, orgB.ID); !errors.Is(err, organization.ErrNotFound) {
			t.Fatalf("cross-org ListInvitations = %v, want ErrNotFound", err)
		}
		if err := svc.ChangeMemberRole(ctx, actorID, orgB.ID, outsiderB.ID, "member"); !errors.Is(err, organization.ErrNotFound) {
			t.Fatalf("cross-org ChangeMemberRole = %v, want ErrNotFound", err)
		}
		if err := svc.RemoveMember(ctx, actorID, orgB.ID, outsiderB.ID); !errors.Is(err, organization.ErrNotFound) {
			t.Fatalf("cross-org RemoveMember = %v, want ErrNotFound", err)
		}
		if err := svc.TransferOwnership(ctx, actorID, orgB.ID, outsiderB.ID); !errors.Is(err, organization.ErrNotFound) {
			t.Fatalf("cross-org TransferOwnership = %v, want ErrNotFound", err)
		}
	}

	// 矩阵：member 看不了企业维度（view_org_usage=false），也无法管理成员。
	if _, err := svc.AuthorizeOrgAction(ctx, memberA.ID, orgA.ID, organization.ActionViewOrgUsage); !errors.Is(err, organization.ErrForbidden) {
		t.Fatalf("member view_org_usage = %v, want ErrForbidden", err)
	}
	if _, err := svc.AuthorizeOrgAction(ctx, billingA.ID, orgA.ID, organization.ActionViewOrgUsage); err != nil {
		t.Fatalf("billing_viewer view_org_usage = %v, want nil", err)
	}
	if _, err := svc.AuthorizeOrgAction(ctx, billingA.ID, orgA.ID, organization.ActionManageOwnKeys); !errors.Is(err, organization.ErrForbidden) {
		t.Fatalf("billing_viewer manage_own_keys = %v, want ErrForbidden", err)
	}
	if _, err := svc.AuthorizeOrgAction(ctx, memberA.ID, orgA.ID, organization.ActionUseModel); err != nil {
		t.Fatalf("member use_model = %v, want nil", err)
	}
}

// --- 成员角色变更与最后 owner ----------------------------------------------

func TestChangeMemberRoleEnforcesMatrixAndLastOwner(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner@example.com")
	admin := createUser(t, db, "admin@example.com")
	member := createUser(t, db, "member@example.com")
	outsider := createUser(t, db, "outsider@example.com")
	org := seedEnterprise(t, db, svc, "Acme", []int64{owner.ID, admin.ID, member.ID},
		[]string{"owner", "admin", "member"})

	// owner 可以把 member 提升为 admin / billing_viewer。
	if err := svc.ChangeMemberRole(ctx, owner.ID, org.ID, member.ID, "admin"); err != nil {
		t.Fatalf("owner promotes member to admin: %v", err)
	}
	if role, status := membershipRole(t, db, org.ID, member.ID); role != "admin" || status != "active" {
		t.Fatalf("member role = %q/%q, want admin/active", role, status)
	}
	if err := svc.ChangeMemberRole(ctx, owner.ID, org.ID, member.ID, "billing_viewer"); err != nil {
		t.Fatalf("owner demotes admin to billing_viewer: %v", err)
	}

	// admin 不能创建 admin（create_admin 只有 owner 有）。
	if err := svc.ChangeMemberRole(ctx, admin.ID, org.ID, member.ID, "admin"); !errors.Is(err, organization.ErrForbidden) {
		t.Fatalf("admin grants admin = %v, want ErrForbidden", err)
	}
	// admin 不能动 owner，也不能动其他 admin。
	target2 := createUser(t, db, "admin2@example.com")
	if _, err := svc.AddMember(ctx, org.ID, target2.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ChangeMemberRole(ctx, admin.ID, org.ID, target2.ID, "member"); !errors.Is(err, organization.ErrForbidden) {
		t.Fatalf("admin demotes another admin = %v, want ErrForbidden", err)
	}
	if err := svc.ChangeMemberRole(ctx, admin.ID, org.ID, owner.ID, "member"); !errors.Is(err, organization.ErrForbidden) {
		t.Fatalf("admin demotes owner = %v, want ErrForbidden", err)
	}
	// member 没有 manage_members。
	if err := svc.ChangeMemberRole(ctx, member.ID, org.ID, target2.ID, "member"); !errors.Is(err, organization.ErrForbidden) {
		t.Fatalf("member changes roles = %v, want ErrForbidden", err)
	}
	// 非成员看到的是 not found。
	if err := svc.ChangeMemberRole(ctx, outsider.ID, org.ID, member.ID, "member"); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("non-member changes roles = %v, want ErrNotFound", err)
	}
	// owner 只能通过转移授予。
	if err := svc.ChangeMemberRole(ctx, owner.ID, org.ID, member.ID, "owner"); !errors.Is(err, organization.ErrOwnerGrant) {
		t.Fatalf("grant owner = %v, want ErrOwnerGrant", err)
	}
	// 平台角色永远不能授予。
	if err := svc.ChangeMemberRole(ctx, owner.ID, org.ID, member.ID, organization.RolePlatformAdmin); !errors.Is(err, organization.ErrPlatformRole) {
		t.Fatalf("grant platform_admin = %v, want ErrPlatformRole", err)
	}
	// 未知角色是校验错误。
	if err := svc.ChangeMemberRole(ctx, owner.ID, org.ID, member.ID, "superuser"); !errors.Is(err, organization.ErrRoleInvalid) {
		t.Fatalf("grant superuser = %v, want ErrRoleInvalid", err)
	}
	// 目标不是 active 成员。
	if err := svc.ChangeMemberRole(ctx, owner.ID, org.ID, outsider.ID, "member"); !errors.Is(err, organization.ErrMemberNotFound) {
		t.Fatalf("unknown target = %v, want ErrMemberNotFound", err)
	}
	// 最后一个 owner 不能自我降级（owner 只有一个）。
	if err := svc.ChangeMemberRole(ctx, owner.ID, org.ID, owner.ID, "admin"); !errors.Is(err, organization.ErrLastOwner) {
		t.Fatalf("last owner self-demotion = %v, want ErrLastOwner", err)
	}
	if role, _ := membershipRole(t, db, org.ID, owner.ID); role != "owner" {
		t.Fatalf("last owner role after refused demotion = %q, want owner", role)
	}
	// 重复设置为同一角色是幂等 no-op，不递增 revision。
	before := orgRevisionOf(t, db, org.ID)
	if err := svc.ChangeMemberRole(ctx, owner.ID, org.ID, target2.ID, "admin"); err != nil {
		t.Fatalf("idempotent role change: %v", err)
	}
	if after := orgRevisionOf(t, db, org.ID); after != before {
		t.Fatalf("idempotent change bumped revision: %d -> %d", before, after)
	}

	// 有两个 owner 时，其中一个是 owner 的调用者可以自我降级。
	second := createUser(t, db, "second-owner@example.com")
	if _, err := svc.AddMember(ctx, org.ID, second.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ChangeMemberRole(ctx, owner.ID, org.ID, owner.ID, "admin"); err != nil {
		t.Fatalf("self-demotion with a second owner: %v", err)
	}
	if err := svc.RemoveMember(ctx, second.ID, org.ID, second.ID); !errors.Is(err, organization.ErrLastOwner) {
		t.Fatalf("last owner self-removal = %v, want ErrLastOwner", err)
	}
}

func TestRemoveMemberRefusesOwnerRemovalByAdmin(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner@example.com")
	admin := createUser(t, db, "admin@example.com")
	member := createUser(t, db, "member@example.com")
	org := seedEnterprise(t, db, svc, "Acme", []int64{owner.ID, admin.ID, member.ID},
		[]string{"owner", "admin", "member"})

	if err := svc.RemoveMember(ctx, admin.ID, org.ID, owner.ID); !errors.Is(err, organization.ErrForbidden) {
		t.Fatalf("admin removes owner = %v, want ErrForbidden", err)
	}
	if err := svc.RemoveMember(ctx, admin.ID, org.ID, member.ID); err != nil {
		t.Fatalf("admin removes member: %v", err)
	}
	if _, err := svc.ListMembers(ctx, admin.ID, org.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Membership(ctx, member.ID, org.ID); !errors.Is(err, organization.ErrNotMember) {
		t.Fatalf("removed member Membership = %v, want ErrNotMember", err)
	}
	// 移除会递增 revision 并留下发布记录（数据面据此撤销 Key，见报告）。
	if orgRevisionOf(t, db, org.ID) != 2 {
		t.Fatalf("revision after one removal = %d, want 2 (create=1)", orgRevisionOf(t, db, org.ID))
	}
}

// TestConcurrentOwnerRemovalKeepsExactlyOneOwner 是 ADR-0003 §5 的并发用例：
// 两个 owner 同时试图移除对方（以及 owner 同时移除对方和自己），组织级
// SELECT ... FOR UPDATE 必须让其中恰好一个成功，另一个看到更新后的 owner 数量
// 并失败，最终恰好剩一个 owner。
func TestConcurrentOwnerRemovalKeepsExactlyOneOwner(t *testing.T) {
	scenarios := []struct {
		name string
		run  func(ctx context.Context, svc *organization.Service, orgID, ownerA, ownerB int64) (int, error)
	}{
		{
			name: "each owner removes the other",
			run: func(ctx context.Context, svc *organization.Service, orgID, ownerA, ownerB int64) (int, error) {
				return runConcurrently(
					func() error { return svc.RemoveMember(ctx, ownerA, orgID, ownerB) },
					func() error { return svc.RemoveMember(ctx, ownerB, orgID, ownerA) },
				)
			},
		},
		{
			name: "owner removes the other owner and leaves at the same time",
			run: func(ctx context.Context, svc *organization.Service, orgID, ownerA, ownerB int64) (int, error) {
				return runConcurrently(
					func() error { return svc.RemoveMember(ctx, ownerA, orgID, ownerB) },
					func() error { return svc.RemoveMember(ctx, ownerA, orgID, ownerA) },
				)
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			db, _ := isolatedDSN(t)
			svc := organization.NewService(db)
			ctx := context.Background()

			ownerA := createUser(t, db, "owner-a@example.com")
			ownerB := createUser(t, db, "owner-b@example.com")
			org := seedEnterprise(t, db, svc, "Acme", []int64{ownerA.ID, ownerB.ID}, []string{"owner", "owner"})
			if got := activeOwnerCount(t, db, org.ID); got != 2 {
				t.Fatalf("owners before = %d, want 2", got)
			}

			successes, err := scenario.run(ctx, svc, org.ID, ownerA.ID, ownerB.ID)
			if err != nil {
				t.Fatalf("concurrent removals: %v", err)
			}
			if successes != 1 {
				t.Fatalf("concurrent removals succeeded %d times, want exactly 1", successes)
			}
			if got := activeOwnerCount(t, db, org.ID); got != 1 {
				t.Fatalf("owners after concurrent removals = %d, want exactly 1", got)
			}
		})
	}
}

// runConcurrently 同时启动两个操作，返回成功次数。失败的一方必须是可解释的
// 竞争结果（目标已不是 active 成员，或被最后 owner 保护拒绝），否则把该错误
// 原样返回给调用方。
func runConcurrently(a, b func() error) (int, error) {
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make([]error, 2)
	)
	wg.Add(2)
	go func() { defer wg.Done(); <-start; errs[0] = a() }()
	go func() { defer wg.Done(); <-start; errs[1] = b() }()
	close(start)
	wg.Wait()

	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
			continue
		}
		if !isExpectedConcurrentFailure(err) {
			return successes, err
		}
	}
	return successes, nil
}

// isExpectedConcurrentFailure：输掉竞争的一方要么发现目标/自己已不是 active
// 成员（ErrNotFound），要么被最后 owner 保护拒绝（ErrLastOwner）。
func isExpectedConcurrentFailure(err error) bool {
	return errors.Is(err, organization.ErrLastOwner) || errors.Is(err, organization.ErrNotFound)
}

// --- owner 转移 ------------------------------------------------------------

func TestTransferOwnershipPromotesTargetAndBumpsRevision(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner@example.com")
	admin := createUser(t, db, "admin@example.com")
	member := createUser(t, db, "member@example.com")
	outsider := createUser(t, db, "outsider@example.com")
	org := seedEnterprise(t, db, svc, "Acme", []int64{owner.ID, admin.ID, member.ID},
		[]string{"owner", "admin", "member"})

	if err := svc.TransferOwnership(ctx, admin.ID, org.ID, member.ID); !errors.Is(err, organization.ErrForbidden) {
		t.Fatalf("admin transfers ownership = %v, want ErrForbidden", err)
	}
	if err := svc.TransferOwnership(ctx, owner.ID, org.ID, owner.ID); !errors.Is(err, organization.ErrTransferTarget) {
		t.Fatalf("transfer to self = %v, want ErrTransferTarget", err)
	}
	if err := svc.TransferOwnership(ctx, owner.ID, org.ID, outsider.ID); !errors.Is(err, organization.ErrMemberNotFound) {
		t.Fatalf("transfer to non-member = %v, want ErrMemberNotFound", err)
	}

	before := orgRevisionOf(t, db, org.ID)
	if err := svc.TransferOwnership(ctx, owner.ID, org.ID, member.ID); err != nil {
		t.Fatalf("TransferOwnership: %v", err)
	}
	if role, status := membershipRole(t, db, org.ID, member.ID); role != "owner" || status != "active" {
		t.Fatalf("target after transfer = %q/%q, want owner/active", role, status)
	}
	if role, _ := membershipRole(t, db, org.ID, owner.ID); role != "admin" {
		t.Fatalf("previous owner after transfer = %q, want admin", role)
	}
	if got := activeOwnerCount(t, db, org.ID); got != 1 {
		t.Fatalf("owners after transfer = %d, want 1", got)
	}
	after := orgRevisionOf(t, db, org.ID)
	if after != before+1 {
		t.Fatalf("revision after transfer = %d, want %d", after, before+1)
	}
	// 新 owner 可以转移回去，旧的 owner 不再能转移（已经不是 owner）。
	if err := svc.TransferOwnership(ctx, owner.ID, org.ID, admin.ID); !errors.Is(err, organization.ErrForbidden) {
		t.Fatalf("demoted owner transfers = %v, want ErrForbidden", err)
	}
	if err := svc.TransferOwnership(ctx, member.ID, org.ID, owner.ID); err != nil {
		t.Fatalf("new owner transfers back: %v", err)
	}
	if role, _ := membershipRole(t, db, org.ID, owner.ID); role != "owner" {
		t.Fatalf("role after transfer back = %q, want owner", role)
	}
	// 目标是既有 owner 时拒绝，而不是把调用者静默降级。
	second := createUser(t, db, "second-owner@example.com")
	if _, err := svc.AddMember(ctx, org.ID, second.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := svc.TransferOwnership(ctx, owner.ID, org.ID, second.ID); !errors.Is(err, organization.ErrTargetAlreadyOwner) {
		t.Fatalf("transfer to an existing owner = %v, want ErrTargetAlreadyOwner", err)
	}
	if role, _ := membershipRole(t, db, org.ID, owner.ID); role != "owner" {
		t.Fatalf("caller was demoted by a refused transfer: role = %q", role)
	}
}

// TestTransferOwnershipChecksActorBeforeTarget 回归 T03 黑盒验收发现的校验顺序
// 缺陷：当 target == caller 时，调用者的成员关系与权限矩阵必须先判，然后才判
// 目标。服务层错误码对应 HTTP 契约：非成员 → ErrNotFound（404，不泄露组织存在
// 性）、plain admin → ErrForbidden（403，矩阵拒绝）、合法 owner → ErrTransferTarget
// （400，目标确实非法）。修复前前两者都会被提前的自我转移检查吞成 400。
func TestTransferOwnershipChecksActorBeforeTarget(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner@example.com")
	admin := createUser(t, db, "admin@example.com")
	outsider := createUser(t, db, "outsider@example.com")
	org := seedEnterprise(t, db, svc, "Acme", []int64{owner.ID, admin.ID}, []string{"owner", "admin"})

	before := orgRevisionOf(t, db, org.ID)

	// 非成员把 target 指向自己：必须 404 语义，而不是 400——否则状态码差异会
	// 泄露“这个组织存在”。
	if err := svc.TransferOwnership(ctx, outsider.ID, org.ID, outsider.ID); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("non-member transfer to self = %v, want ErrNotFound", err)
	}
	// plain admin 把 target 指向自己：必须 403 语义（transfer_owner 只属于 owner）。
	if err := svc.TransferOwnership(ctx, admin.ID, org.ID, admin.ID); !errors.Is(err, organization.ErrForbidden) {
		t.Fatalf("admin transfer to self = %v, want ErrForbidden", err)
	}
	// 合法 owner 转移到自己：仍然是 400 语义（ErrTransferTarget）。
	if err := svc.TransferOwnership(ctx, owner.ID, org.ID, owner.ID); !errors.Is(err, organization.ErrTransferTarget) {
		t.Fatalf("owner transfer to self = %v, want ErrTransferTarget", err)
	}

	// 三次拒绝都不得改动任何角色，也不得推进 revision 或写发布记录：拒绝发生在
	// mutate 回调里，事务整体回滚。
	if role, status := membershipRole(t, db, org.ID, owner.ID); role != "owner" || status != "active" {
		t.Fatalf("owner after refused transfers = %q/%q, want owner/active", role, status)
	}
	if role, status := membershipRole(t, db, org.ID, admin.ID); role != "admin" || status != "active" {
		t.Fatalf("admin after refused transfers = %q/%q, want admin/active", role, status)
	}
	if got := activeOwnerCount(t, db, org.ID); got != 1 {
		t.Fatalf("owners after refused transfers = %d, want 1", got)
	}
	if after := orgRevisionOf(t, db, org.ID); after != before {
		t.Fatalf("revision after refused transfers = %d, want unchanged %d", after, before)
	}
	if _, err := svc.Membership(ctx, outsider.ID, org.ID); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("outsider membership after refused transfers = %v, want ErrNotFound", err)
	}
}

// --- 邀请 ------------------------------------------------------------------

func TestInvitationLifecycleIsSingleUseWithFixedEmailAndRole(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner@example.com")
	admin := createUser(t, db, "admin@example.com")
	invitee := createUser(t, db, "invitee@example.com")
	other := createUser(t, db, "other@example.com")
	org := seedEnterprise(t, db, svc, "Acme", []int64{owner.ID, admin.ID}, []string{"owner", "admin"})

	// 非成员不能创建邀请（404 语义），member 没有 manage_members。
	if _, _, err := svc.CreateInvitation(ctx, other.ID, org.ID, "x@example.com", "member", time.Hour); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("non-member CreateInvitation = %v, want ErrNotFound", err)
	}
	// admin 不能邀请 owner（只有 owner 可以）。
	if _, _, err := svc.CreateInvitation(ctx, admin.ID, org.ID, "x@example.com", "owner", time.Hour); !errors.Is(err, organization.ErrOwnerGrant) {
		t.Fatalf("admin invites owner = %v, want ErrOwnerGrant", err)
	}
	// admin 不能邀请 admin。
	if _, _, err := svc.CreateInvitation(ctx, admin.ID, org.ID, "x@example.com", "admin", time.Hour); !errors.Is(err, organization.ErrForbidden) {
		t.Fatalf("admin invites admin = %v, want ErrForbidden", err)
	}
	// 平台角色不能邀请。
	if _, _, err := svc.CreateInvitation(ctx, owner.ID, org.ID, "x@example.com", organization.RolePlatformAdmin, time.Hour); !errors.Is(err, organization.ErrPlatformRole) {
		t.Fatalf("invite platform_admin = %v, want ErrPlatformRole", err)
	}
	// 非法 email 不落库。
	if _, _, err := svc.CreateInvitation(ctx, owner.ID, org.ID, "not-an-email", "member", time.Hour); !errors.Is(err, organization.ErrInvitationEmailInvalid) {
		t.Fatalf("invalid email = %v, want ErrInvitationEmailInvalid", err)
	}

	revisionBefore := orgRevisionOf(t, db, org.ID)
	invitation, token, err := svc.CreateInvitation(ctx, owner.ID, org.ID, "  INVITEE@Example.COM ", "billing_viewer", 2*time.Hour)
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	if token == "" {
		t.Fatal("CreateInvitation did not return a raw token")
	}
	if invitation.Email != "invitee@example.com" || invitation.Role != "billing_viewer" || invitation.Status != "pending" {
		t.Fatalf("invitation = %+v, want normalized email + fixed role + pending", invitation)
	}
	if invitation.CreatedBy == nil || *invitation.CreatedBy != owner.ID {
		t.Fatalf("created_by = %v, want %d", invitation.CreatedBy, owner.ID)
	}
	// 库里只有 SHA-256，明文不落库。
	var storedHash string
	if err := db.QueryRow(`SELECT token_hash FROM invitations WHERE id = $1`, invitation.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == token || strings.Contains(storedHash, token) || len(storedHash) != 64 {
		t.Fatalf("stored token_hash = %q, want a 64-char SHA-256 digest that is not the raw token", storedHash)
	}

	// 邀请本身不改变成员状态，所以不递增 revision。
	if got := orgRevisionOf(t, db, org.ID); got != revisionBefore {
		t.Fatalf("CreateInvitation bumped revision: %d -> %d", revisionBefore, got)
	}

	// 列表只返回元数据。
	listed, err := svc.ListInvitations(ctx, owner.ID, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != invitation.ID || listed[0].Email != "invitee@example.com" {
		t.Fatalf("ListInvitations = %+v", listed)
	}

	// email 不匹配（大小写不敏感地比较）→ 拒绝。
	if _, err := svc.AcceptInvitation(ctx, token, other.ID, other.Email); !errors.Is(err, organization.ErrInvitationEmailMismatch) {
		t.Fatalf("wrong email accept = %v, want ErrInvitationEmailMismatch", err)
	}
	// 匹配（大小写/空格无关）→ 成功，角色来自邀请，不能被请求覆盖。
	membership, err := svc.AcceptInvitation(ctx, token, invitee.ID, "  Invitee@EXAMPLE.com ")
	if err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
	if membership.Role != "billing_viewer" || membership.Status != "active" || membership.OrgID != org.ID {
		t.Fatalf("accepted membership = %+v, want billing_viewer/active in the inviting org", membership)
	}
	if role, _ := membershipRole(t, db, org.ID, invitee.ID); role != "billing_viewer" {
		t.Fatalf("stored role = %q, want the invitation role", role)
	}
	if got := orgRevisionOf(t, db, org.ID); got != revisionBefore+1 {
		t.Fatalf("revision after accept = %d, want %d", got, revisionBefore+1)
	}

	// 单次使用：重放被拒。
	if _, err := svc.AcceptInvitation(ctx, token, invitee.ID, invitee.Email); !errors.Is(err, organization.ErrInvitationUsed) {
		t.Fatalf("replayed accept = %v, want ErrInvitationUsed", err)
	}
	// 伪造/未知 token。
	if _, err := svc.AcceptInvitation(ctx, token+"x", invitee.ID, invitee.Email); !errors.Is(err, organization.ErrInvitationNotFound) {
		t.Fatalf("tampered token = %v, want ErrInvitationNotFound", err)
	}

	// 过期：用 SQL 把 expires_at 拨到过去（HTTP/service 不能创建已过期邀请）。
	expired, expiredToken, err := svc.CreateInvitation(ctx, owner.ID, org.ID, "invitee@example.com", "member", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE invitations SET expires_at = $2 WHERE id = $1`, expired.ID, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AcceptInvitation(ctx, expiredToken, invitee.ID, invitee.Email); !errors.Is(err, organization.ErrInvitationExpired) {
		t.Fatalf("expired accept = %v, want ErrInvitationExpired", err)
	}

	// 撤销：admin 撤销后不能再接受；重复撤销幂等；非成员撤销是 404。
	revoked, revokedToken, err := svc.CreateInvitation(ctx, owner.ID, org.ID, "other@example.com", "member", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeInvitation(ctx, other.ID, org.ID, revoked.ID); !errors.Is(err, organization.ErrNotFound) {
		t.Fatalf("non-member revoke = %v, want ErrNotFound", err)
	}
	if err := svc.RevokeInvitation(ctx, admin.ID, org.ID, revoked.ID); err != nil {
		t.Fatalf("admin revoke: %v", err)
	}
	if err := svc.RevokeInvitation(ctx, admin.ID, org.ID, revoked.ID); err != nil {
		t.Fatalf("repeat revoke must be idempotent: %v", err)
	}
	if _, err := svc.AcceptInvitation(ctx, revokedToken, other.ID, other.Email); !errors.Is(err, organization.ErrInvitationRevoked) {
		t.Fatalf("accept revoked = %v, want ErrInvitationRevoked", err)
	}
	if err := svc.RevokeInvitation(ctx, admin.ID, org.ID, invitation.ID); !errors.Is(err, organization.ErrInvitationUsed) {
		t.Fatalf("revoke accepted = %v, want ErrInvitationUsed", err)
	}

	// 重新邀请一个已被移除的成员：接受后恢复 active 并使用邀请里的角色，
	// created_at 保留首次加入时间（upsert 的恢复语义）。
	if err := svc.RemoveMember(ctx, owner.ID, org.ID, invitee.ID); err != nil {
		t.Fatalf("remove billing_viewer before re-inviting: %v", err)
	}
	if _, err := svc.AcceptInvitation(ctx, token, invitee.ID, invitee.Email); !errors.Is(err, organization.ErrInvitationUsed) {
		t.Fatalf("removed member reused the old token = %v, want ErrInvitationUsed", err)
	}
	_, reToken, err := svc.CreateInvitation(ctx, owner.ID, org.ID, invitee.Email, "member", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reactivated, err := svc.AcceptInvitation(ctx, reToken, invitee.ID, invitee.Email)
	if err != nil {
		t.Fatalf("reactivate removed member: %v", err)
	}
	if reactivated.Role != "member" || reactivated.Status != "active" {
		t.Fatalf("reactivated membership = %+v, want member/active", reactivated)
	}

	// owner 可以邀请 owner；接受后成为 owner。
	ownerInvite, ownerToken, err := svc.CreateInvitation(ctx, owner.ID, org.ID, "other@example.com", "owner", time.Hour)
	if err != nil {
		t.Fatalf("owner invites owner: %v", err)
	}
	if _, err := svc.AcceptInvitation(ctx, ownerToken, other.ID, other.Email); err != nil {
		t.Fatalf("accept owner invitation: %v", err)
	}
	if role, _ := membershipRole(t, db, org.ID, other.ID); role != "owner" {
		t.Fatalf("accepted owner invitation role = %q, want owner", role)
	}
	_ = ownerInvite
}

// --- revision 与 policy_publications 的同事务性 ----------------------------

func TestRevisionAndPolicyPublicationAreWrittenInOneTransaction(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner@example.com")
	member := createUser(t, db, "member@example.com")
	org := seedEnterprise(t, db, svc, "Acme", []int64{owner.ID, member.ID}, []string{"owner", "member"})

	// 企业创建本身不写发布记录（那时还没有任何策略状态）。
	if rows := listPublications(t, db, org.ID); len(rows) != 0 {
		t.Fatalf("publications after create = %+v, want none", rows)
	}
	if got := orgRevisionOf(t, db, org.ID); got != 1 {
		t.Fatalf("revision after create = %d, want 1", got)
	}

	// 成功变更：revision +1 且恰好一条同一 revision 的 pending 行（同事务）。
	if err := svc.ChangeMemberRole(ctx, owner.ID, org.ID, member.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	rows := listPublications(t, db, org.ID)
	if len(rows) != 1 {
		t.Fatalf("publications = %+v, want exactly 1", rows)
	}
	if rows[0].OrgRevision != 2 || rows[0].Scope != "org" || rows[0].TargetID != "" || rows[0].Status != "pending" || rows[0].Attempts != 0 {
		t.Fatalf("publication row = %+v, want revision 2, scope org, target '', pending, attempts 0", rows[0])
	}
	if got := orgRevisionOf(t, db, org.ID); got != 2 {
		t.Fatalf("revision = %d, want 2", got)
	}

	// 失败变更（最后 owner 自我降级）：revision 与发布记录都不动。
	if err := svc.ChangeMemberRole(ctx, owner.ID, org.ID, owner.ID, "member"); !errors.Is(err, organization.ErrLastOwner) {
		t.Fatalf("self-demotion = %v, want ErrLastOwner", err)
	}
	if got := orgRevisionOf(t, db, org.ID); got != 2 {
		t.Fatalf("revision after failed change = %d, want 2 (unchanged)", got)
	}
	if rows := listPublications(t, db, org.ID); len(rows) != 1 {
		t.Fatalf("publications after failed change = %+v, want still exactly 1", rows)
	}

	// 邀请接受同样在同一个事务里递增 revision + 写发布记录。
	invitee := createUser(t, db, "invitee@example.com")
	_, token, err := svc.CreateInvitation(ctx, owner.ID, org.ID, invitee.Email, "member", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AcceptInvitation(ctx, token, invitee.ID, invitee.Email); err != nil {
		t.Fatal(err)
	}
	rows = listPublications(t, db, org.ID)
	if len(rows) != 2 || rows[1].OrgRevision != 3 || rows[1].Status != "pending" {
		t.Fatalf("publications after accept = %+v, want revision 3 pending", rows)
	}
	if got := orgRevisionOf(t, db, org.ID); got != 3 {
		t.Fatalf("revision after accept = %d, want 3", got)
	}
}

// --- 发布 worker -----------------------------------------------------------

// fakePublisher 记录每次发布请求；errs 按调用序号返回错误（缺省为 nil），
// 用来模拟“第一次失败、重试成功”。
type fakePublisher struct {
	mu       sync.Mutex
	requests []organization.PublishRequest
	errs     []error
}

func (f *fakePublisher) PublishOrgPolicy(_ context.Context, req organization.PublishRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if len(f.requests) <= len(f.errs) {
		return f.errs[len(f.requests)-1]
	}
	return nil
}

func (f *fakePublisher) calls() []organization.PublishRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]organization.PublishRequest(nil), f.requests...)
}

func TestPublicationWorkerPublishesAndSupersedes(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner@example.com")
	member := createUser(t, db, "member@example.com")
	org := seedEnterprise(t, db, svc, "Acme", []int64{owner.ID, member.ID}, []string{"owner", "member"})

	now := time.Now().UTC()
	worker := organization.NewPublicationWorker(db, nil, organization.PublicationOptions{
		Now: func() time.Time { return now },
	})
	if _, err := worker.RunOnce(ctx); err == nil {
		t.Fatal("RunOnce without a publisher = nil, want an error")
	}

	// 一次真实变更：revision 2 + pending。
	if err := svc.ChangeMemberRole(ctx, owner.ID, org.ID, member.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	// 手工插入一条被取代的旧 revision（1 < 当前 2）：worker 必须删除它且绝不
	// 调用 Publisher——旧 revision 不得覆盖新状态。
	if _, err := db.Exec(`INSERT INTO policy_publications
		(org_id, org_revision, scope, target_id, status, attempts, created_at, updated_at)
		VALUES ($1, 1, 'org', '', 'pending', 0, $2, $2)`, org.ID, now); err != nil {
		t.Fatal(err)
	}

	publisher := &fakePublisher{}
	worker = organization.NewPublicationWorker(db, publisher, organization.PublicationOptions{
		Now: func() time.Time { return now },
	})
	processed, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if processed != 2 {
		t.Fatalf("processed = %d, want 2 (the superseded row is deleted, the current one published)", processed)
	}
	calls := publisher.calls()
	if len(calls) != 1 {
		t.Fatalf("publisher calls = %+v, want exactly 1 (the current revision)", calls)
	}
	want := organization.PublishRequest{
		OrgID:       org.ID,
		OrgRevision: 2,
		Scope:       "org",
		TargetID:    "",
		OperationID: "policy:" + itoa(org.ID) + ":2:org:",
	}
	if calls[0].PublicationID == 0 || calls[0].OperationID != want.OperationID ||
		calls[0].OrgID != want.OrgID || calls[0].OrgRevision != want.OrgRevision ||
		calls[0].Scope != "org" || calls[0].TargetID != "" {
		t.Fatalf("publish request = %+v, want %+v", calls[0], want)
	}

	rows := listPublications(t, db, org.ID)
	if len(rows) != 1 || rows[0].Status != "published" || !rows[0].OperationID.Valid || rows[0].OperationID.String != want.OperationID {
		t.Fatalf("publications after publish = %+v, want a single published row carrying the operation id", rows)
	}
	if rows[0].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (the claim increments once)", rows[0].Attempts)
	}
	// 已发布的行不会被再次拾取。
	if processed, err := worker.RunOnce(ctx); err != nil || processed != 0 {
		t.Fatalf("second RunOnce = (%d, %v), want (0, nil)", processed, err)
	}
	if calls := publisher.calls(); len(calls) != 1 {
		t.Fatalf("publisher calls after second pass = %d, want 1", len(calls))
	}
}

func TestPublicationWorkerRetriesWithBoundedBackoff(t *testing.T) {
	db, _ := isolatedDSN(t)
	svc := organization.NewService(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner@example.com")
	member := createUser(t, db, "member@example.com")
	org := seedEnterprise(t, db, svc, "Acme", []int64{owner.ID, member.ID}, []string{"owner", "member"})
	if err := svc.ChangeMemberRole(ctx, owner.ID, org.ID, member.ID, "admin"); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	worker := organization.NewPublicationWorker(db, &fakePublisher{errs: []error{errors.New("control unavailable")}}, organization.PublicationOptions{
		Now:       func() time.Time { return now },
		RetryBase: time.Second,
		RetryMax:  4 * time.Second,
	})
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rows := listPublications(t, db, org.ID)
	if len(rows) != 1 || rows[0].Status != "failed" {
		t.Fatalf("publications = %+v, want one failed row", rows)
	}
	if !rows[0].LastError.Valid || !strings.Contains(rows[0].LastError.String, "control unavailable") {
		t.Fatalf("last_error = %v, want the publisher error", rows[0].LastError)
	}
	if !rows[0].NextRetryAt.Valid {
		t.Fatal("next_retry_at is NULL for a failed publication")
	}
	if delay := rows[0].NextRetryAt.Time.Sub(now); delay != time.Second {
		t.Fatalf("first backoff = %s, want 1s", delay)
	}
	// 退避期内不会被再次拾取。
	if processed, err := worker.RunOnce(ctx); err != nil || processed != 0 {
		t.Fatalf("RunOnce inside the backoff window = (%d, %v), want (0, nil)", processed, err)
	}

	// 退避到期后重试成功：attempts=2，状态 published。
	if _, err := db.Exec(`UPDATE policy_publications SET next_retry_at = $2 WHERE id = $1`, rows[0].ID, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	publisher := &fakePublisher{}
	worker = organization.NewPublicationWorker(db, publisher, organization.PublicationOptions{
		Now:       func() time.Time { return now },
		RetryBase: time.Second,
		RetryMax:  4 * time.Second,
	})
	if processed, err := worker.RunOnce(ctx); err != nil || processed != 1 {
		t.Fatalf("retry RunOnce = (%d, %v), want (1, nil)", processed, err)
	}
	rows = listPublications(t, db, org.ID)
	if rows[0].Status != "published" || rows[0].Attempts != 2 || rows[0].NextRetryAt.Valid {
		t.Fatalf("publication after retry = %+v, want published/attempts=2/no retry", rows[0])
	}
}

func itoa(value int64) string { return strconv.FormatInt(value, 10) }
