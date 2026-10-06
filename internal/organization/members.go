package organization

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Member 是“组织成员 + 用户可见字段”的对外视图，不含 password_hash、Key 或 Key 策略。
type Member struct {
	MembershipID      int64
	UserID            int64
	Email             string
	Role              string
	Status            string
	MonthlyLimitMicro *int64
	CreatedAt         time.Time
}

// errNoChange 由 mutate 回调返回，表示“校验通过但无需写库”（幂等重复请求）。
// 事务照常提交，但不会递增 org_revision，也不会插入 policy_publications。
var errNoChange = errors.New("organization: membership unchanged")

// ListMembers 列出组织成员。
//
// 可见性规则：owner/admin 可以看到成员清单；member 只能看到自己——返回
// **只含调用者本人**的单元素列表，而不是 403。这样普通成员可以确认自己的
// 角色，又无法枚举同事（选“单元素列表”而非“拒绝”，因为它对 UI 更友好且不
// 泄露任何额外信息）。非成员返回 ErrNotFound，不泄露组织存在性。
func (s *Service) ListMembers(ctx context.Context, actorUserID, orgID int64) ([]Member, error) {
	actor, err := s.actorMembership(ctx, orgID, actorUserID)
	if err != nil {
		return nil, err
	}
	// Verify membership before inspecting account type/status. This prevents a
	// non-member from using personal-vs-enterprise behavior as an enumeration
	// oracle for another user's account.
	if err := ensureEnterprise(ctx, s.db, orgID); err != nil {
		return nil, err
	}
	if !Allowed(actor.Role, ActionManageMembers) {
		self, err := s.member(ctx, orgID, actorUserID)
		if err != nil {
			return nil, err
		}
		return []Member{self}, nil
	}

	rows, err := s.db.QueryContext(ctx, queryListMembers, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]Member, 0, 8)
	for rows.Next() {
		var m Member
		var cap sql.NullInt64
		if err := rows.Scan(&m.MembershipID, &m.UserID, &m.Email, &m.Role, &m.Status, &cap, &m.CreatedAt); err != nil {
			return nil, err
		}
		if cap.Valid {
			value := cap.Int64
			m.MonthlyLimitMicro = &value
		}
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// ChangeMemberRole 修改成员角色。整个操作在组织级串行化短事务里执行：
// 先 SELECT ... FOR UPDATE 锁住 organizations 行，再校验角色矩阵与“至少一个
// owner”，因此并发的降级/移除不会同时看到旧 owner 数量。
//
// 拒绝的情况（均为类型化错误，HTTP 层映射 403/404/409）：
//   - 调用者不是 active 成员：ErrNotFound；
//   - 角色没有 manage_members：ErrForbidden；
//   - 目标不是 active 成员：ErrMemberNotFound；
//   - admin 试图改动 owner/admin：ErrForbidden；
//   - 授予 owner（除 TransferOwnership）：ErrOwnerGrant；
//   - 授予 platform_admin：ErrPlatformRole（平台角色不是组织角色）；
//   - 授予 admin 但调用者不是 owner（create_admin）：ErrForbidden；
//   - 把最后一个 owner 降级（含自我降级）：ErrLastOwner。
//
// 成功时在同一事务里递增 org_revision 并写入一行 pending 的
// policy_publications(scope=org)，供数据面后续撤销/收敛。
func (s *Service) ChangeMemberRole(ctx context.Context, actorUserID, orgID, targetUserID int64, role string) error {
	return s.ChangeMemberRoleWithLimit(ctx, actorUserID, orgID, targetUserID, role, nil, false)
}

// ChangeMemberRoleWithLimit allows an owner to appoint/dismiss admins. A role
// transition to ordinary member must carry the cap field, with nil meaning
// explicitly unlimited. Admins may only change ordinary members' role fields.
func (s *Service) ChangeMemberRoleWithLimit(ctx context.Context, actorUserID, orgID, targetUserID int64, role string, monthlyLimit *int64, monthlyLimitProvided bool) error {
	clean := strings.TrimSpace(role)
	if clean == RolePlatformAdmin {
		return ErrPlatformRole
	}
	if !validRole(clean) {
		return ErrRoleInvalid
	}
	if monthlyLimit != nil && *monthlyLimit < 0 {
		return errors.New("organization: monthly limit must not be negative")
	}

	return s.mutateMembership(ctx, actorUserID, orgID, ActionManageMembers, func(tx *sql.Tx, actorRole string) error {
		if err := checkRoleGrant(actorRole, clean); err != nil {
			return err
		}
		target, err := memberInTx(ctx, tx, orgID, targetUserID)
		if err != nil {
			return err
		}
		if (actorRole == roleAdmin && (target.UserID == actorUserID || target.Role != roleMember)) ||
			(target.UserID != actorUserID && !canActOnTarget(actorRole, target.Role)) {
			return ErrForbidden
		}
		if target.Role == clean {
			return errNoChange
		}
		owners, err := countActiveOwnersInTx(ctx, tx, orgID)
		if err != nil {
			return err
		}
		if err := checkLastOwner(target.Role, clean, owners); err != nil {
			return err
		}
		if clean == roleMember && target.Role != roleMember && !monthlyLimitProvided {
			return ErrLimitRequired
		}
		limit := target.MonthlyLimitMicro
		if monthlyLimitProvided {
			limit = monthlyLimit
		} else if clean == roleAdmin && target.Role != roleAdmin {
			limit = nil
		}
		result, err := tx.ExecContext(ctx, queryUpdateMemberRoleAndLimit, orgID, targetUserID, clean, limit, time.Now().UTC())
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err == nil && affected == 0 {
			return ErrMemberNotFound
		}
		return nil
	})
}

// ChangeMemberMonthlyLimit applies a monthly cap to one current membership. Admins may
// change ordinary members only; the owner may change any membership, including their own.
// This transaction never locks billing account or usage rows.
func (s *Service) ChangeMemberMonthlyLimit(ctx context.Context, actorUserID, orgID, targetUserID int64, limit *int64) (Membership, error) {
	if limit != nil && *limit < 0 {
		return Membership{}, errors.New("organization: monthly limit must not be negative")
	}
	var changed Membership
	err := s.mutateMembership(ctx, actorUserID, orgID, ActionChangeMemberPolicy, func(tx *sql.Tx, actorRole string) error {
		target, err := memberInTx(ctx, tx, orgID, targetUserID)
		if err != nil {
			return err
		}
		if actorRole == roleAdmin && (targetUserID == actorUserID || target.Role != roleMember) {
			return ErrForbidden
		}
		if targetUserID != actorUserID && !canActOnTarget(actorRole, target.Role) {
			return ErrForbidden
		}
		if sameLimit(target.MonthlyLimitMicro, limit) {
			changed = Membership{ID: target.MembershipID, OrgID: orgID, UserID: targetUserID,
				Role: target.Role, Status: target.Status, MonthlyLimitMicro: cloneLimit(limit), CreatedAt: target.CreatedAt}
			return errNoChange
		}
		return scanMembership(tx.QueryRowContext(ctx, queryUpdateMemberMonthlyLimit,
			orgID, targetUserID, limit, time.Now().UTC()).Scan, &changed)
	})
	if err != nil {
		return Membership{}, err
	}
	return changed, nil
}

// ChangeMemberStatus pauses or resumes a current member. A personal account
// cannot use this team-management path, and the enterprise owner stays active.
func (s *Service) ChangeMemberStatus(ctx context.Context, actorUserID, orgID, targetUserID int64, nextStatus string) error {
	if nextStatus != memberActive && nextStatus != memberSuspended {
		return ErrMemberStatusInvalid
	}
	return s.mutateMembership(ctx, actorUserID, orgID, ActionManageMembers, func(tx *sql.Tx, actorRole string) error {
		target, err := memberInTx(ctx, tx, orgID, targetUserID)
		if err != nil {
			return err
		}
		if actorRole == roleAdmin && (targetUserID == actorUserID || target.Role != roleMember) {
			return ErrForbidden
		}
		if targetUserID != actorUserID && !canActOnTarget(actorRole, target.Role) {
			return ErrForbidden
		}
		if target.Role == roleOwner && nextStatus != memberActive {
			return ErrLastOwner
		}
		if target.Status == nextStatus {
			return errNoChange
		}
		result, err := tx.ExecContext(ctx, queryUpdateMemberStatus, orgID, targetUserID, nextStatus, time.Now().UTC())
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err == nil && affected == 0 {
			return ErrMemberNotFound
		}
		return nil
	})
}

// RemoveMember 把成员置为 removed（不删除行：审计与历史需要）。规则与
// ChangeMemberRole 相同：admin 不能移除 owner/admin，最后一个 owner 不能离开
// （包括 owner 移除自己）。
func (s *Service) RemoveMember(ctx context.Context, actorUserID, orgID, targetUserID int64) error {
	return s.mutateMembership(ctx, actorUserID, orgID, ActionManageMembers, func(tx *sql.Tx, actorRole string) error {
		target, err := memberInTx(ctx, tx, orgID, targetUserID)
		if err != nil {
			return err
		}
		if target.UserID != actorUserID && !canActOnTarget(actorRole, target.Role) {
			return ErrForbidden
		}
		owners, err := countActiveOwnersInTx(ctx, tx, orgID)
		if err != nil {
			return err
		}
		if err := checkLastOwner(target.Role, "", owners); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, queryRemoveMember, orgID, targetUserID, time.Now().UTC())
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err == nil && affected == 0 {
			return ErrMemberNotFound
		}
		return nil
	})
}

// TransferOwnership 是授予 owner 的唯一路径：目标成员在同一事务里升为 owner，
// 调用者降为 admin（owner 数量不变，因此不需要“最后 owner”判断）。只有 owner
// 能调用（transfer_owner）。目标必须是组织的 active 成员，且不能是自己。
//
// 校验顺序是有约束的：调用者的成员关系与权限矩阵先判（mutateMembership 已完
// 成），目标合法性后判。非成员必须拿到 ErrNotFound（不泄露组织存在性）、plain
// admin 必须拿到 ErrForbidden（矩阵拒绝），不能因为“目标是调用者自己”这种目标
// 侧问题先返回 ErrTransferTarget——否则同一请求对非成员/无权限者会变成 400，
// 既泄露了目标校验细节，也绕过了 404/403 契约。
func (s *Service) TransferOwnership(ctx context.Context, actorUserID, orgID, targetUserID int64) error {
	return s.mutateMembership(ctx, actorUserID, orgID, ActionTransferOwner, func(tx *sql.Tx, actorRole string) error {
		// 目标校验排在 actor 成员关系 + 矩阵之后：合法的 owner 转移到自己仍
		// 是 400（ErrTransferTarget），但非成员/admin 在那之前就已被拒绝。
		if targetUserID == actorUserID {
			return ErrTransferTarget
		}
		target, err := memberInTx(ctx, tx, orgID, targetUserID)
		if err != nil {
			return err
		}
		if target.Status != memberActive {
			return ErrMemberNotFound
		}
		if target.Role == roleOwner {
			// 目标已经是 owner：无需提升，也不把调用者静默降级。
			return ErrTargetAlreadyOwner
		}
		now := time.Now().UTC()
		if err := setMemberRoleInTx(ctx, tx, orgID, actorUserID, roleAdmin, now); err != nil {
			return err
		}
		if err := setMemberRoleInTx(ctx, tx, orgID, targetUserID, roleOwner, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, queryUpdateOrganizationOwner, orgID, targetUserID, now); err != nil {
			return err
		}
		return nil
	})
}

// mutateMembership 是所有成员变更共用的组织级串行化协议：
//
//  1. 短事务开始；
//  2. SELECT id FROM organizations WHERE id=$1 FOR UPDATE —— 组织锁，两个并发
//     的 owner 移除在这里排队，后到者一定看到先到者提交后的 owner 数量；
//  3. 在同一事务里读取调用者的 active membership 并执行矩阵检查；
//  4. 执行 mutate 回调（读目标、最后 owner 判断、写 memberships）；
//  5. 递增 organizations.org_revision 并插入 policy_publications(scope=org,
//     status=pending)，与成员变更同事务提交；
//  6. 提交。任何一步出错都回滚，revision 与发布记录不会与状态脱节。
func (s *Service) mutateMembership(ctx context.Context, actorUserID, orgID int64, action Action, mutate func(tx *sql.Tx, actorRole string) error) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var lockedID int64
	err = tx.QueryRowContext(ctx, queryLockOrganization, orgID).Scan(&lockedID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	actor, err := actorMembershipInTx(ctx, tx, orgID, actorUserID)
	if err != nil {
		return err
	}
	if err := ensureEnterpriseInTx(ctx, tx, orgID); err != nil {
		return err
	}
	if !Allowed(actor.Role, action) {
		return ErrForbidden
	}
	if err := mutate(tx, actor.Role); err != nil {
		if errors.Is(err, errNoChange) {
			// 幂等重复：不递增 revision、不写发布记录。
			return tx.Commit()
		}
		return err
	}

	var revision int64
	if err := tx.QueryRowContext(ctx, queryBumpOrgRevision, orgID, now).Scan(&revision); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, queryInsertPolicyPublication, orgID, revision, now); err != nil {
		return err
	}
	return tx.Commit()
}

// memberInTx 读取组织里的成员（含非 active 行），并把“不是 active 成员”统一
// 归一为 ErrMemberNotFound：对调用者来说，removed 与 never-existed 没有区别。
func memberInTx(ctx context.Context, tx *sql.Tx, orgID, userID int64) (Member, error) {
	var m Member
	err := scanMember(tx.QueryRowContext(ctx, queryGetMember, orgID, userID).Scan, &m)
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, ErrMemberNotFound
	}
	if err != nil {
		return Member{}, err
	}
	if m.Status == memberRemoved {
		return Member{}, ErrMemberNotFound
	}
	return m, nil
}

func (s *Service) member(ctx context.Context, orgID, userID int64) (Member, error) {
	var m Member
	err := scanMember(s.db.QueryRowContext(ctx, queryGetMember, orgID, userID).Scan, &m)
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, ErrMemberNotFound
	}
	if err != nil {
		return Member{}, err
	}
	return m, nil
}

func scanMember(scan func(dest ...any) error, member *Member) error {
	var cap sql.NullInt64
	if err := scan(&member.MembershipID, &member.UserID, &member.Email, &member.Role,
		&member.Status, &cap, &member.CreatedAt); err != nil {
		return err
	}
	if cap.Valid {
		value := cap.Int64
		member.MonthlyLimitMicro = &value
	}
	return nil
}

func countActiveOwnersInTx(ctx context.Context, tx *sql.Tx, orgID int64) (int, error) {
	var owners int
	if err := tx.QueryRowContext(ctx, queryCountActiveOwners, orgID).Scan(&owners); err != nil {
		return 0, err
	}
	return owners, nil
}

func setMemberRoleInTx(ctx context.Context, tx *sql.Tx, orgID, userID int64, role string, now time.Time) error {
	result, err := tx.ExecContext(ctx, queryUpdateMemberRole, orgID, userID, role, now)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		return ErrMemberNotFound
	}
	return nil
}

func sameLimit(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func cloneLimit(limit *int64) *int64 {
	if limit == nil {
		return nil
	}
	value := *limit
	return &value
}
