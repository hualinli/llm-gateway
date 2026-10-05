package organization

import (
	"errors"
	"testing"
)

// TestAllowedCoversEveryRoleAndAction 是 ADR-0003 §2.3 权限矩阵的完整表格断言：
// 每一个 (角色, 操作) 组合都必须显式列出期望值。新增操作或角色时，这个表必须
// 一起更新——这正是把矩阵当契约冻结的目的。
func TestAllowedCoversEveryRoleAndAction(t *testing.T) {
	type row struct {
		role   string
		action Action
		want   bool
	}
	cases := []row{
		// member：可以用模型、管理本人 Key、看本人用量；看不到企业维度。
		{roleMember, ActionUseModel, true},
		{roleMember, ActionManageOwnKeys, true},
		{roleMember, ActionViewOwnUsage, true},
		{roleMember, ActionViewOrgUsage, false},
		{roleMember, ActionManageMembers, false},
		{roleMember, ActionChangeMemberPolicy, false},
		{roleMember, ActionCreateAdmin, false},
		{roleMember, ActionTransferOwner, false},
		{roleMember, ActionPlatformAdminOps, false},

		// billing_viewer：独立只读财务角色，只有企业维度的只读视图。
		{roleBillingViewer, ActionUseModel, false},
		{roleBillingViewer, ActionManageOwnKeys, false},
		{roleBillingViewer, ActionViewOwnUsage, false},
		{roleBillingViewer, ActionViewOrgUsage, true},
		{roleBillingViewer, ActionManageMembers, false},
		{roleBillingViewer, ActionChangeMemberPolicy, false},
		{roleBillingViewer, ActionCreateAdmin, false},
		{roleBillingViewer, ActionTransferOwner, false},
		{roleBillingViewer, ActionPlatformAdminOps, false},

		// admin：member 的全部 + 企业视图 + 成员管理；不能创建 admin、不能转移。
		{roleAdmin, ActionUseModel, true},
		{roleAdmin, ActionManageOwnKeys, true},
		{roleAdmin, ActionViewOwnUsage, true},
		{roleAdmin, ActionViewOrgUsage, true},
		{roleAdmin, ActionManageMembers, true},
		{roleAdmin, ActionChangeMemberPolicy, true},
		{roleAdmin, ActionCreateAdmin, false},
		{roleAdmin, ActionTransferOwner, false},
		{roleAdmin, ActionPlatformAdminOps, false},

		// owner：组织内全部操作，但没有平台操作。
		{roleOwner, ActionUseModel, true},
		{roleOwner, ActionManageOwnKeys, true},
		{roleOwner, ActionViewOwnUsage, true},
		{roleOwner, ActionViewOrgUsage, true},
		{roleOwner, ActionManageMembers, true},
		{roleOwner, ActionChangeMemberPolicy, true},
		{roleOwner, ActionCreateAdmin, true},
		{roleOwner, ActionTransferOwner, true},
		{roleOwner, ActionPlatformAdminOps, false},
	}

	// 表必须是全量：4 个角色 × len(Actions) 个操作，一个不漏、一个不多。
	if want := len(permissionMatrix) * len(Actions); len(cases) != want {
		t.Fatalf("matrix table has %d cases, want %d (roles=%d actions=%d)",
			len(cases), want, len(permissionMatrix), len(Actions))
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		key := tc.role + "|" + string(tc.action)
		if seen[key] {
			t.Fatalf("duplicate case %s", key)
		}
		seen[key] = true
		if got := Allowed(tc.role, tc.action); got != tc.want {
			t.Errorf("Allowed(%q, %q) = %t, want %t", tc.role, tc.action, got, tc.want)
		}
	}
	for _, role := range Roles() {
		for _, action := range Actions {
			if !seen[role+"|"+string(action)] {
				t.Fatalf("matrix table is missing (%s, %s)", role, action)
			}
		}
	}
}

// TestAllowedRefusesNonOrganizationRoles：platform_admin 是用户级标志，不是组织
// 角色；角色不叠加，未知/空/大小写变体一律拒绝。
func TestAllowedRefusesNonOrganizationRoles(t *testing.T) {
	for _, role := range []string{"", "platform_admin", "superuser", "OWNER", "Owner", "owner ", " admin"} {
		for _, action := range Actions {
			if Allowed(role, action) {
				t.Errorf("Allowed(%q, %q) = true, want false for a non-organization role", role, action)
			}
		}
	}
	if ValidRole(RolePlatformAdmin) {
		t.Fatal("ValidRole(platform_admin) = true; the platform flag must never be an organization role")
	}
	for _, role := range Roles() {
		if !ValidRole(role) {
			t.Errorf("ValidRole(%q) = false, want true", role)
		}
	}
	// 角色不叠加：每个角色只有一张表，没有继承链。
	if len(permissionMatrix) != len(Roles()) {
		t.Fatalf("permissionMatrix has %d roles, want %d", len(permissionMatrix), len(Roles()))
	}
}

func TestCheckRoleGrant(t *testing.T) {
	cases := []struct {
		actor string
		grant string
		want  error
	}{
		{roleOwner, roleMember, nil},
		{roleOwner, roleBillingViewer, nil},
		{roleOwner, roleAdmin, nil},
		{roleOwner, roleOwner, ErrOwnerGrant},
		{roleAdmin, roleMember, nil},
		{roleAdmin, roleBillingViewer, nil},
		{roleAdmin, roleAdmin, ErrForbidden},
		{roleAdmin, roleOwner, ErrOwnerGrant},
		{roleMember, roleMember, ErrForbidden},
		{roleBillingViewer, roleMember, ErrForbidden},
		{roleOwner, RolePlatformAdmin, ErrPlatformRole},
		{roleAdmin, RolePlatformAdmin, ErrPlatformRole},
		{roleMember, RolePlatformAdmin, ErrPlatformRole},
		{roleOwner, "superuser", ErrRoleInvalid},
		{roleOwner, "", ErrRoleInvalid},
	}
	for _, tc := range cases {
		err := checkRoleGrant(tc.actor, tc.grant)
		if !errors.Is(err, tc.want) {
			t.Errorf("checkRoleGrant(%q, %q) = %v, want %v", tc.actor, tc.grant, err, tc.want)
		}
	}
}

func TestCanActOnTarget(t *testing.T) {
	cases := []struct {
		actor  string
		target string
		want   bool
	}{
		{roleOwner, roleOwner, true},
		{roleOwner, roleAdmin, true},
		{roleOwner, roleMember, true},
		{roleOwner, roleBillingViewer, true},
		{roleAdmin, roleMember, true},
		{roleAdmin, roleBillingViewer, true},
		{roleAdmin, roleAdmin, false}, // admin 不能动其他 admin
		{roleAdmin, roleOwner, false}, // admin 不能动 owner
		{roleMember, roleMember, false},
		{roleBillingViewer, roleMember, false},
	}
	for _, tc := range cases {
		if got := canActOnTarget(tc.actor, tc.target); got != tc.want {
			t.Errorf("canActOnTarget(%q, %q) = %t, want %t", tc.actor, tc.target, got, tc.want)
		}
	}
}

func TestCheckLastOwner(t *testing.T) {
	cases := []struct {
		current string
		next    string // "" 表示移除
		owners  int
		want    error
	}{
		{roleOwner, roleAdmin, 1, ErrLastOwner},  // 最后一个 owner 不能降级
		{roleOwner, roleMember, 1, ErrLastOwner}, // 最后一个 owner 不能自我降级
		{roleOwner, "", 1, ErrLastOwner},         // 最后一个 owner 不能离开
		{roleOwner, roleOwner, 1, nil},           // 保持 owner 是安全的
		{roleOwner, roleAdmin, 2, nil},           // 还有另一个 owner
		{roleOwner, "", 2, nil},                  // 还有另一个 owner
		{roleAdmin, roleMember, 1, nil},          // 与 owner 数量无关
		{roleAdmin, "", 1, nil},                  //
		{roleBillingViewer, "", 1, nil},          //
		{roleMember, "", 0, nil},                 // owner 数为 0 时也只约束 owner
	}
	for _, tc := range cases {
		err := checkLastOwner(tc.current, tc.next, tc.owners)
		if !errors.Is(err, tc.want) {
			t.Errorf("checkLastOwner(%q, %q, %d) = %v, want %v", tc.current, tc.next, tc.owners, err, tc.want)
		}
	}
}

// TestErrLastOwnerIsTypedForConflictMapping：HTTP 层必须能把“最后 owner”映射成
// 409 conflict，而不是 403 或 500，所以它必须是独立可判定的错误值。
func TestErrLastOwnerIsTypedForConflictMapping(t *testing.T) {
	err := checkLastOwner(roleOwner, "", 1)
	if !errors.Is(err, ErrLastOwner) {
		t.Fatalf("last-owner error = %v, want ErrLastOwner", err)
	}
	if errors.Is(err, ErrForbidden) {
		t.Fatal("ErrLastOwner must be distinguishable from ErrForbidden (403 vs 409)")
	}
}
