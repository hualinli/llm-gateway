package organization

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"

	"llm-gateway/internal/controlclient"
)

// 策略发布适配器单元测试：ControlPolicyClient 是 fake（真实实现有独立测试），
// PolicySource 也是 fake，因此这里断言的是「适配器提交了什么」，而不是数据库行为。
// PG + 真实 gRPC 的端到端用例在 publication_control_integration_test.go。

type fakePolicyClient struct {
	mu       sync.Mutex
	requests []controlclient.ApplyOrgPolicyRequest

	result controlclient.ApplyOrgPolicyResult
	err    error
}

func (f *fakePolicyClient) ApplyOrgPolicy(_ context.Context, req controlclient.ApplyOrgPolicyRequest) (controlclient.ApplyOrgPolicyResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.err != nil {
		return controlclient.ApplyOrgPolicyResult{}, f.err
	}
	return f.result, nil
}

func (f *fakePolicyClient) last(t *testing.T) controlclient.ApplyOrgPolicyRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("适配器没有调用 Control")
	}
	return f.requests[len(f.requests)-1]
}

type fakePolicySource struct {
	policy DesiredOrgPolicy
	err    error
	orgID  int64
}

func (f *fakePolicySource) DesiredOrgPolicy(_ context.Context, orgID int64) (DesiredOrgPolicy, error) {
	f.orgID = orgID
	if f.err != nil {
		return DesiredOrgPolicy{}, f.err
	}
	return f.policy, nil
}

func TestControlPublisherSendsTheCompleteDesiredPolicy(t *testing.T) {
	rpm := int32(600)
	burst := int32(60)
	allowlist := []string{"gpt-4o"}
	client := &fakePolicyClient{result: controlclient.ApplyOrgPolicyResult{ResultingVersion: 42}}
	source := &fakePolicySource{policy: DesiredOrgPolicy{
		OrgRevision:       7,
		ModelAllowlist:    &allowlist,
		RequestsPerMinute: &rpm,
		Burst:             &burst,
	}}
	publisher, err := NewControlPublisher(client, source, ControlPublisherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = publisher.PublishOrgPolicy(context.Background(), PublishRequest{
		PublicationID: 11,
		OperationID:   "policy:2:7:org:",
		OrgID:         2,
		OrgRevision:   7,
		Scope:         "org",
	})
	if err != nil {
		t.Fatalf("PublishOrgPolicy: %v", err)
	}
	if source.orgID != 2 {
		t.Fatalf("source read org %d, want 2", source.orgID)
	}
	request := client.last(t)
	if request.OperationID != "policy:2:7:org:" || request.OrgID != 2 || request.ActorOrgID != 2 {
		t.Fatalf("发布的幂等键/组织不正确: %+v", request)
	}
	if request.ActorUserID != 0 {
		t.Fatalf("发布是服务身份发起的，actor_user_id 必须为 0: %d", request.ActorUserID)
	}
	if request.RequestID != "publication:11" {
		t.Fatalf("request_id=%q, want publication:11", request.RequestID)
	}
	if request.ExpectedVersion != nil {
		t.Fatalf("发布不设置 expected_version（revision 才是排序令牌）: %d", *request.ExpectedVersion)
	}
	if request.ModelAllowlist == nil || strings.Join(*request.ModelAllowlist, ",") != "gpt-4o" {
		t.Fatalf("全量替换必须带上 allowlist: %v", request.ModelAllowlist)
	}
	if request.RequestsPerMinute == nil || *request.RequestsPerMinute != 600 {
		t.Fatalf("rpm=%v, want 600", request.RequestsPerMinute)
	}
	if request.Burst == nil || *request.Burst != 60 {
		t.Fatalf("burst=%v, want 60", request.Burst)
	}
}

func TestControlPublisherPreservesTriStateAndClearsDefaults(t *testing.T) {
	// 默认期望策略：无 allowlist（NULL = 继承）、无限制（清除）。
	client := &fakePolicyClient{}
	source := &fakePolicySource{policy: DesiredOrgPolicy{OrgRevision: 3}}
	publisher, err := NewControlPublisher(client, source, ControlPublisherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.PublishOrgPolicy(context.Background(), PublishRequest{OrgID: 2, OrgRevision: 3, OperationID: "op"}); err != nil {
		t.Fatalf("PublishOrgPolicy: %v", err)
	}
	request := client.last(t)
	if request.ModelAllowlist != nil {
		t.Fatalf("NULL allowlist 必须作为「继承」提交，得到 %v", *request.ModelAllowlist)
	}
	if request.RequestsPerMinute != nil || request.Burst != nil {
		t.Fatal("没有配置限制时，全量替换必须显式清除它们")
	}

	// 显式空集合是 deny-all，不能被折叠成 NULL。
	empty := []string{}
	source.policy = DesiredOrgPolicy{OrgRevision: 3, ModelAllowlist: &empty}
	if err := publisher.PublishOrgPolicy(context.Background(), PublishRequest{OrgID: 2, OrgRevision: 3, OperationID: "op-2"}); err != nil {
		t.Fatalf("PublishOrgPolicy: %v", err)
	}
	request = client.last(t)
	if request.ModelAllowlist == nil || len(*request.ModelAllowlist) != 0 {
		t.Fatalf("显式 deny-all 必须是「存在的空集合」，得到 %v", request.ModelAllowlist)
	}
}

func TestControlPublisherRejectsStaleAndUnsupportedPublications(t *testing.T) {
	client := &fakePolicyClient{}
	source := &fakePolicySource{policy: DesiredOrgPolicy{OrgRevision: 9}}
	publisher, err := NewControlPublisher(client, source, ControlPublisherOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// 组织 revision 已经前进：这次发布被取代，worker 会删除该行。
	err = publisher.PublishOrgPolicy(context.Background(), PublishRequest{OrgID: 2, OrgRevision: 8, OperationID: "op"})
	if !errors.Is(err, ErrPublicationSuperseded) {
		t.Fatalf("err=%v, want ErrPublicationSuperseded", err)
	}

	// 组织 revision 落后于发布行：状态不可能倒退，报错（进入退避重试与 last_error）。
	source.policy = DesiredOrgPolicy{OrgRevision: 7}
	err = publisher.PublishOrgPolicy(context.Background(), PublishRequest{OrgID: 2, OrgRevision: 8, OperationID: "op"})
	if err == nil || errors.Is(err, ErrPublicationSuperseded) {
		t.Fatalf("err=%v, want a real error for a regressed revision", err)
	}

	// 组织不存在。
	source.err = sql.ErrNoRows
	if err := publisher.PublishOrgPolicy(context.Background(), PublishRequest{OrgID: 2, OrgRevision: 8, OperationID: "op"}); !errors.Is(err, ErrPublicationSuperseded) {
		t.Fatalf("err=%v, want ErrPublicationSuperseded for a missing organization", err)
	}
	source.err = nil
	source.policy = DesiredOrgPolicy{OrgRevision: 8}

	// scope=key 目前没有对应的 RPC：显式报错，留在 last_error 里可见。
	err = publisher.PublishOrgPolicy(context.Background(), PublishRequest{
		OrgID: 2, OrgRevision: 8, Scope: "key", TargetID: "key_1", OperationID: "op",
	})
	if !errors.Is(err, ErrPublicationScopeUnsupported) {
		t.Fatalf("err=%v, want ErrPublicationScopeUnsupported", err)
	}
	if len(client.requests) != 0 {
		t.Fatal("被拒绝的发布不能调用 Control")
	}
}

func TestControlPublisherGeneratesADeterministicOperationID(t *testing.T) {
	client := &fakePolicyClient{}
	source := &fakePolicySource{policy: DesiredOrgPolicy{OrgRevision: 4}}
	publisher, err := NewControlPublisher(client, source, ControlPublisherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	request := PublishRequest{OrgID: 2, OrgRevision: 4, Scope: "org"}
	if err := publisher.PublishOrgPolicy(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	first := client.last(t).OperationID
	if err := publisher.PublishOrgPolicy(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if second := client.last(t).OperationID; second != first || first != "policy:2:4:org:" {
		t.Fatalf("operation_id=%q then %q, want the deterministic policy:2:4:org:", first, second)
	}
}

func TestControlPublisherRefusesToBeHalfConfigured(t *testing.T) {
	client := &fakePolicyClient{}
	if _, err := NewControlPublisher(nil, &fakePolicySource{}, ControlPublisherOptions{}); err == nil {
		t.Fatal("nil client must be refused")
	}
	if _, err := NewControlPublisher(client, nil, ControlPublisherOptions{}); err == nil {
		t.Fatal("nil source must be refused")
	}
	publisher, err := NewControlPublisher(client, &fakePolicySource{policy: DesiredOrgPolicy{OrgRevision: 1}}, ControlPublisherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.PublishOrgPolicy(context.Background(), PublishRequest{OrgID: 0, OrgRevision: 1}); err == nil {
		t.Fatal("a publication without an organization must be refused")
	}
	if len(client.requests) != 0 {
		t.Fatal("an invalid publication must not reach Control")
	}
}

func TestControlPublisherPropagatesControlErrors(t *testing.T) {
	client := &fakePolicyClient{err: errors.New("control is down")}
	source := &fakePolicySource{policy: DesiredOrgPolicy{OrgRevision: 1}}
	publisher, err := NewControlPublisher(client, source, ControlPublisherOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = publisher.PublishOrgPolicy(context.Background(), PublishRequest{OrgID: 2, OrgRevision: 1, OperationID: "op"})
	if err == nil || !strings.Contains(err.Error(), "control is down") {
		t.Fatalf("err=%v, want the wrapped Control failure", err)
	}
}
