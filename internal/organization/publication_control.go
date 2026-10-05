package organization

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"llm-gateway/internal/controlclient"
)

// 策略发布适配器（ADR-0003 §2.6）：把 publication.go 的 PublishRequest 变成一次
// Control AdminService.ApplyOrgPolicy 调用。
//
// 三个必须说清楚的语义：
//
//  1. **全量替换**。ApplyOrgPolicy 不接受增量：它把组织策略整体替换为本次提交的
//     内容。因此适配器每次都提交「完整期望策略」（DesiredOrgPolicy），而不是只提交
//     变化的部分。缺省（nil）表示清除：allowlist 为 NULL（继承）、无 RPM/Burst 限制；
//     显式空集合（[]）表示 deny-all——两者在 proto 里必须是可区分的（§2.7）。
//  2. **期望状态来自 Console**。期望策略由 PolicySource 读取（默认读 organizations
//     行）。当前 schema（000004/000005）没有组织级 allowlist/RPM 列，所以默认实现的
//     期望策略是「无 allowlist、无限制」——这恰好是这些组织的真实期望状态，不是
//     「跳过发布」。将来 T03 §2.7 的组织级配额/白名单列落地时，只需要替换
//     PolicySource，不动发布协议。
//  3. **revision 是排序令牌**。发布前重新读取 organizations.org_revision：比本次
//     发布新 → 返回 ErrPublicationSuperseded（worker 会删除这条被取代的行，重试没有
//     意义）；比本次发布旧 → 报错（状态不可能倒退，说明读到了不一致的数据）。
//     expected_version 刻意留空：Control 快照版本由 Control 自己分配，Worker 只按
//     org_revision 收敛。

// ControlPolicyClient 是 Control AdminService 中发布所需的最小切片。真实实现是
// *controlclient.Client；测试用 fake。
type ControlPolicyClient interface {
	ApplyOrgPolicy(ctx context.Context, req controlclient.ApplyOrgPolicyRequest) (controlclient.ApplyOrgPolicyResult, error)
}

// DesiredOrgPolicy 是一个组织的**完整**数据面策略。
//
// ModelAllowlist 三态：nil = 继承（NULL），&[] = deny-all，非空 = 显式白名单。
// RequestsPerMinute/Burst 为 nil 表示没有限制。
type DesiredOrgPolicy struct {
	OrgRevision       int64
	ModelAllowlist    *[]string
	RequestsPerMinute *int32
	Burst             *int32
}

// PolicySource 读取权威的期望策略。sql.ErrNoRows 表示组织已不存在（没有可发布的
// 状态），调用方按「被取代」处理。
type PolicySource interface {
	DesiredOrgPolicy(ctx context.Context, orgID int64) (DesiredOrgPolicy, error)
}

var (
	// ErrPublicationSuperseded 表示这次发布已经被更新的组织状态取代：不发布，也不
	// 重试，直接丢弃这一行（publication.go 的 worker 会删除它）。
	ErrPublicationSuperseded = errors.New("organization: publication was superseded by a newer organization revision")
	// ErrPublicationScopeUnsupported 表示 scope 不是 'org'。首版只有组织级策略
	// RPC；按 key 的策略是 api_keys.model_allowlist，由 Control 在 Key 变更时投影，
	// 不走这里。这样的行会留在 policy_publications 里并带 last_error 重试，是可见的，
	// 不是静默丢弃。
	ErrPublicationScopeUnsupported = errors.New("organization: policy publication scope is not supported")
)

// ControlPublisherOptions 覆盖适配器默认值。
type ControlPublisherOptions struct {
	// Logger 记录跳过/失败的发布；nil 用 log.Default()。
	Logger *log.Logger
}

// ControlPublisher 实现 Publisher：org 策略发布 → Control AdminService。
type ControlPublisher struct {
	client ControlPolicyClient
	source PolicySource
	logger *log.Logger
}

// NewControlPublisher 组装适配器。client 与 source 都必须非 nil：一个「看起来在
// 发布、实际什么都没做」的适配器比直接报错更危险。
func NewControlPublisher(client ControlPolicyClient, source PolicySource, opts ControlPublisherOptions) (*ControlPublisher, error) {
	if client == nil {
		return nil, errors.New("organization: control publisher requires a policy client")
	}
	if source == nil {
		return nil, errors.New("organization: control publisher requires a policy source")
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &ControlPublisher{client: client, source: source, logger: logger}, nil
}

// PublishOrgPolicy 提交一次全量策略替换。它必须幂等：同一个 OperationID 重复调用
// 在 Control 侧返回已存结果（同 ID 同 digest），不会产生第二次效果。
func (p *ControlPublisher) PublishOrgPolicy(ctx context.Context, req PublishRequest) error {
	if p == nil || p.client == nil || p.source == nil {
		return errors.New("organization: control publisher is not configured")
	}
	if req.OrgID <= 0 {
		return fmt.Errorf("organization: publication requires an organization id, got %d", req.OrgID)
	}
	scope := strings.TrimSpace(req.Scope)
	if scope != "" && scope != "org" {
		return fmt.Errorf("%w: scope=%q target=%q", ErrPublicationScopeUnsupported, req.Scope, req.TargetID)
	}

	desired, err := p.source.DesiredOrgPolicy(ctx, req.OrgID)
	if errors.Is(err, sql.ErrNoRows) {
		// 组织已经不存在：级联删除会带走这一行，这里不发布也不重试。
		return ErrPublicationSuperseded
	}
	if err != nil {
		return fmt.Errorf("organization: read desired policy for org %d: %w", req.OrgID, err)
	}
	switch {
	case desired.OrgRevision > req.OrgRevision:
		return ErrPublicationSuperseded
	case desired.OrgRevision < req.OrgRevision:
		return fmt.Errorf("organization: organization %d revision %d is behind publication revision %d",
			req.OrgID, desired.OrgRevision, req.OrgRevision)
	}

	operationID := strings.TrimSpace(req.OperationID)
	if operationID == "" {
		// 与 publication.go 的确定性 operation_id 同一口径：即使调用方没给，
		// 重试也仍然是同一个幂等键。
		operationID = fmt.Sprintf("policy:%d:%d:%s:%s", req.OrgID, req.OrgRevision, scope, req.TargetID)
	}
	result, err := p.client.ApplyOrgPolicy(ctx, controlclient.ApplyOrgPolicyRequest{
		OperationID: operationID,
		// 发布是 Console（服务身份）发起的，不是某个用户：actor_user_id 留空，
		// actor_org_id 记录被发布的组织，request_id 关联到发布行。
		ActorUserID:       0,
		ActorOrgID:        req.OrgID,
		RequestID:         fmt.Sprintf("publication:%d", req.PublicationID),
		OrgID:             req.OrgID,
		ModelAllowlist:    desired.ModelAllowlist,
		RequestsPerMinute: desired.RequestsPerMinute,
		Burst:             desired.Burst,
	})
	if err != nil {
		return fmt.Errorf("organization: apply policy for org %d: %w", req.OrgID, err)
	}
	p.logger.Printf("console policy publication: org=%d revision=%d operation_id=%s version=%d replayed=%t",
		req.OrgID, req.OrgRevision, operationID, result.ResultingVersion, result.Replayed)
	return nil
}

// OrganizationPolicySource 是默认的 PolicySource：以 organizations 行作为期望状态的
// 权威来源。
//
// 当前期望策略 = 组织存在 + revision 匹配 + 无 allowlist（NULL = 继承）+ 无 RPM/Burst
// 限制。000005 没有组织级白名单/配额列；当它们落地时，只改这个类型。
type OrganizationPolicySource struct {
	db *sql.DB
}

// NewOrganizationPolicySource 绑定组织库。
func NewOrganizationPolicySource(db *sql.DB) *OrganizationPolicySource {
	return &OrganizationPolicySource{db: db}
}

const queryDesiredOrgPolicy = `SELECT org_revision FROM organizations WHERE id = $1`

// DesiredOrgPolicy 读取组织的权威 revision。
func (s *OrganizationPolicySource) DesiredOrgPolicy(ctx context.Context, orgID int64) (DesiredOrgPolicy, error) {
	if s == nil || s.db == nil {
		return DesiredOrgPolicy{}, errors.New("organization: policy source is not configured")
	}
	var revision int64
	if err := s.db.QueryRowContext(ctx, queryDesiredOrgPolicy, orgID).Scan(&revision); err != nil {
		return DesiredOrgPolicy{}, err
	}
	return DesiredOrgPolicy{OrgRevision: revision}, nil
}
