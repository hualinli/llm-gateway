package organization

import (
	"context"
	"database/sql"
	"errors"
)

// 权限矩阵（ADR-0003 §2.3）是**服务端唯一执行点**：HTTP 层只做 status code
// 映射，任何绕过 HTTP 层的调用（gRPC、后台 loop、测试）都必须先过这里。
//
// 角色不叠加：一个用户在一个组织里恰有一个角色（memberships.role）；平台
// 管理员是用户级标志 users.platform_admin（ADR-0002 §2.4），不是组织角色，
// 也永远不会被 Allowed 授予任何组织操作。企业的 owner 与“个人组织创建者”
// 语义一致，但判定只看 memberships，不看 organizations.owner_user_id。
//
//	操作                                member  billing_viewer  admin  owner
//	use_model                             ✓          —            ✓      ✓
//	manage_own_keys                       ✓          —            ✓      ✓
//	view_own_usage                        ✓          —            ✓      ✓
//	view_org_usage                        —          ✓（只读）     ✓      ✓
//	manage_members                        —          —            ✓      ✓
//	change_member_policy                  —          —            ✓      ✓
//	create_admin                          —          —            —      ✓
//	transfer_owner                        —          —            —      ✓
//	platform_admin_ops                    —          —            —      —
type Action string

const (
	// ActionUseModel：使用获准模型发起请求。
	ActionUseModel Action = "use_model"
	// ActionManageOwnKeys：管理本人（owner_user_id = 自己）的 API Key。
	ActionManageOwnKeys Action = "manage_own_keys"
	// ActionViewOwnUsage：查看本人用量。
	ActionViewOwnUsage Action = "view_own_usage"
	// ActionViewOrgUsage：查看企业汇总与账单（billing_viewer 的只读财务视角）。
	ActionViewOrgUsage Action = "view_org_usage"
	// ActionManageMembers：邀请/移除普通成员、读取成员与邀请列表。
	ActionManageMembers Action = "manage_members"
	// ActionChangeMemberPolicy：修改成员策略（含成员级限制，首版仅角色语义）。
	ActionChangeMemberPolicy Action = "change_member_policy"
	// ActionCreateAdmin：把成员提升为 admin。
	ActionCreateAdmin Action = "create_admin"
	// ActionTransferOwner：移交 owner（仅 owner，且走专用路径）。
	ActionTransferOwner Action = "transfer_owner"
	// ActionPlatformAdminOps：平台级操作（Key/价格/Provider/额度）。平台管理员
	// 身份是 users.platform_admin，不属于组织角色，因此组织角色恒为 false；
	// 平台操作只允许经显式的平台入口（HTTP 层 withPlatformAdmin + 审计）执行。
	ActionPlatformAdminOps Action = "platform_admin_ops"
)

// Actions 按矩阵顺序列出全部组织操作，供文档与表驱动测试遍历。
var Actions = []Action{
	ActionUseModel,
	ActionManageOwnKeys,
	ActionViewOwnUsage,
	ActionViewOrgUsage,
	ActionManageMembers,
	ActionChangeMemberPolicy,
	ActionCreateAdmin,
	ActionTransferOwner,
	ActionPlatformAdminOps,
}

// permissionMatrix 是 ADR-0003 §2.3 的唯一表格实现。缺少的键即拒绝：
// 未列出的角色（包括 "platform_admin"、空串或任何自定义字符串）一律 false。
var permissionMatrix = map[string]map[Action]bool{
	roleMember: {
		ActionUseModel:      true,
		ActionManageOwnKeys: true,
		ActionViewOwnUsage:  true,
	},
	roleBillingViewer: {
		// 独立只读财务角色：不生成请求、不建 Key、不改成员，也不看别人的明细，
		// 只看企业汇总与账单。
		ActionViewOrgUsage: true,
	},
	roleAdmin: {
		ActionUseModel:           true,
		ActionManageOwnKeys:      true,
		ActionViewOwnUsage:       true,
		ActionViewOrgUsage:       true,
		ActionManageMembers:      true,
		ActionChangeMemberPolicy: true,
	},
	roleOwner: {
		ActionUseModel:           true,
		ActionManageOwnKeys:      true,
		ActionViewOwnUsage:       true,
		ActionViewOrgUsage:       true,
		ActionManageMembers:      true,
		ActionChangeMemberPolicy: true,
		ActionCreateAdmin:        true,
		ActionTransferOwner:      true,
	},
}

// Allowed 报告组织角色 role 是否允许执行 action。它是纯函数：不读数据库、
// 不看平台标志、不叠加角色。未知角色与未知操作都返回 false。
func Allowed(role string, action Action) bool {
	actions, ok := permissionMatrix[role]
	if !ok {
		return false
	}
	return actions[action]
}

// ValidRole 报告 role 是否是一个组织角色（memberships.role 的受限取值）。
// 平台管理员不在其中：授予 platform_admin 永远不会成功。
func ValidRole(role string) bool { return validRole(role) }

// Roles 返回 memberships.role 的全部合法取值（ADR-0002 §3 的冻结词表）。
// 角色不叠加：这些是仅有的四个，platform_admin 不在其中。
func Roles() []string { return []string{roleOwner, roleAdmin, roleMember, roleBillingViewer} }

// RolePlatformAdmin 是用户级平台标志的名字，刻意不是组织角色：它只用于
// 错误信息与文档，任何把它当 role 传入的写入都会被拒绝。
const RolePlatformAdmin = "platform_admin"

// 服务层拒绝原因。HTTP 层只做映射（403 / 404 / 409），不做判断。
var (
	// ErrForbidden：调用者是组织成员，但其角色不允许该操作。
	ErrForbidden = errors.New("organization: action is not permitted for the caller's role")
	// ErrRoleInvalid：角色取值不在 memberships.role 的受限集合内。
	ErrRoleInvalid = errors.New("organization: invalid organization role")
	// ErrPlatformRole：试图授予平台管理员——企业管理员不能创建平台管理员。
	ErrPlatformRole = errors.New("organization: platform_admin is not an organization role")
	// ErrOwnerGrant：owner 只能通过 TransferOwnership 授予。
	ErrOwnerGrant = errors.New("organization: owner may only be granted through ownership transfer")
	// ErrLastOwner：组织必须保留至少一个 active owner。
	ErrLastOwner = errors.New("organization: organization must keep at least one owner")
	// ErrMemberNotFound：目标用户不是该组织的 active 成员。
	ErrMemberNotFound = errors.New("organization: member not found")
	// ErrTransferTarget：owner 转移的目标非法（例如指向自己）。
	ErrTransferTarget = errors.New("organization: invalid ownership transfer target")
	// ErrTargetAlreadyOwner：转移目标已经是 owner，无需（也不应）重复授予。
	ErrTargetAlreadyOwner = errors.New("organization: transfer target is already an owner")
)

// AuthorizeOrgAction 解析调用者在 orgID 的 active 成员关系并执行矩阵检查。
// 非成员（含组织不存在）一律 ErrNotFound，不泄露存在性；成员但无权限返回
// ErrForbidden。只读入口（审计列表）用它做前置检查。
//
// 唯一不走这里的是 AcceptInvitation：接受者还不是成员，授权凭据是邀请 token +
// 创建时固定的 email（invitations.go），它的成员写入同样在单个事务里完成。
func (s *Service) AuthorizeOrgAction(ctx context.Context, actorUserID, orgID int64, action Action) (Membership, error) {
	membership, err := s.Membership(ctx, actorUserID, orgID)
	if err != nil {
		return Membership{}, err
	}
	if !Allowed(membership.Role, action) {
		return Membership{}, ErrForbidden
	}
	return membership, nil
}

// checkRoleGrant 判断 actorRole 是否可以把成员的角改变成 newRole。
// owner 只走 TransferOwnership；admin 需要 create_admin（仅 owner）；平台角色
// 与未知角色一律拒绝。
func checkRoleGrant(actorRole, newRole string) error {
	if newRole == RolePlatformAdmin {
		return ErrPlatformRole
	}
	if !validRole(newRole) {
		return ErrRoleInvalid
	}
	if newRole == roleOwner {
		return ErrOwnerGrant
	}
	if !Allowed(actorRole, ActionManageMembers) {
		return ErrForbidden
	}
	if newRole == roleAdmin && !Allowed(actorRole, ActionCreateAdmin) {
		return ErrForbidden
	}
	return nil
}

// canActOnTarget 判断 actorRole 是否有资格管理当前角色为 targetRole 的成员。
// admin 只能管理 member / billing_viewer：既不能删/降 owner，也不能动其他
// admin；owner 可以管理任何人（仍受最后 owner 与转移规则约束）。
func canActOnTarget(actorRole, targetRole string) bool {
	switch actorRole {
	case roleOwner:
		return true
	case roleAdmin:
		return targetRole == roleMember || targetRole == roleBillingViewer
	default:
		return false
	}
}

// checkLastOwner 判断把 currentRole 的成员改成 newRole（空串表示移除）之后，
// 组织是否仍至少有一个 active owner。activeOwners 必须在组织级锁内、与本次
// 变更同一个事务里读取。
func checkLastOwner(currentRole, newRole string, activeOwners int) error {
	if currentRole != roleOwner || newRole == roleOwner {
		return nil
	}
	if activeOwners <= 1 {
		return ErrLastOwner
	}
	return nil
}

// membershipInTx 在事务内读取成员关系，语义与 Membership 一致：没有行返回
// ErrNotFound（外部统一映射 404），不是 active 返回 ErrNotMember。
func membershipInTx(ctx context.Context, tx *sql.Tx, orgID, userID int64) (Membership, error) {
	var m Membership
	err := tx.QueryRowContext(ctx, queryGetMembership, orgID, userID).
		Scan(&m.OrgID, &m.UserID, &m.Role, &m.Status, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Membership{}, ErrNotFound
	}
	if err != nil {
		return Membership{}, err
	}
	if m.Status != memberActive {
		return Membership{}, ErrNotMember
	}
	return m, nil
}

// actorMembershipInTx 是“调用者自己”的成员关系读取：被移除（removed）与从未
// 存在对外必须是同一个答案，因此把 ErrNotMember 归一为 ErrNotFound，避免通过
// 状态码差异区分“你不是成员”和“你被移除了”。
func actorMembershipInTx(ctx context.Context, tx *sql.Tx, orgID, userID int64) (Membership, error) {
	m, err := membershipInTx(ctx, tx, orgID, userID)
	if errors.Is(err, ErrNotMember) {
		return Membership{}, ErrNotFound
	}
	return m, err
}

// actorMembership 是 actorMembershipInTx 的非事务版本（只读入口使用）。
func (s *Service) actorMembership(ctx context.Context, orgID, userID int64) (Membership, error) {
	m, err := s.Membership(ctx, userID, orgID)
	if errors.Is(err, ErrNotMember) {
		return Membership{}, ErrNotFound
	}
	return m, err
}
