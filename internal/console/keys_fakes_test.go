package console

import (
	"context"
	"errors"
	"sync"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/controlclient"
)

// fakeControl stands in for internal/controlclient in handler tests. Every call
// is recorded so a test can assert what actually reached Control — in
// particular which owner filter scoped a list, and that a refused request never
// became a command.
type fakeControl struct {
	mu sync.Mutex

	createFn func(ctx context.Context, req controlclient.CreateApiKeyRequest) (controlclient.CreateApiKeyResult, error)
	revokeFn func(ctx context.Context, req controlclient.RevokeApiKeyRequest) (controlclient.RevokeApiKeyResult, error)
	listFn   func(ctx context.Context, req controlclient.ListApiKeysRequest) (controlclient.ListApiKeysResult, error)
	getOpFn  func(ctx context.Context, req controlclient.GetOperationRequest) (controlclient.OperationResult, error)

	createCalls []controlclient.CreateApiKeyRequest
	revokeCalls []controlclient.RevokeApiKeyRequest
	listCalls   []controlclient.ListApiKeysRequest
	getOpCalls  []controlclient.GetOperationRequest
}

func (f *fakeControl) CreateApiKey(ctx context.Context, req controlclient.CreateApiKeyRequest) (controlclient.CreateApiKeyResult, error) {
	f.mu.Lock()
	f.createCalls = append(f.createCalls, req)
	f.mu.Unlock()
	if f.createFn != nil {
		return f.createFn(ctx, req)
	}
	return controlclient.CreateApiKeyResult{}, errors.New("control CreateApiKey not configured")
}

func (f *fakeControl) RevokeApiKey(ctx context.Context, req controlclient.RevokeApiKeyRequest) (controlclient.RevokeApiKeyResult, error) {
	f.mu.Lock()
	f.revokeCalls = append(f.revokeCalls, req)
	f.mu.Unlock()
	if f.revokeFn != nil {
		return f.revokeFn(ctx, req)
	}
	return controlclient.RevokeApiKeyResult{}, errors.New("control RevokeApiKey not configured")
}

func (f *fakeControl) ListApiKeys(ctx context.Context, req controlclient.ListApiKeysRequest) (controlclient.ListApiKeysResult, error) {
	f.mu.Lock()
	f.listCalls = append(f.listCalls, req)
	f.mu.Unlock()
	if f.listFn != nil {
		return f.listFn(ctx, req)
	}
	return controlclient.ListApiKeysResult{}, nil
}

func (f *fakeControl) GetOperation(ctx context.Context, req controlclient.GetOperationRequest) (controlclient.OperationResult, error) {
	f.mu.Lock()
	f.getOpCalls = append(f.getOpCalls, req)
	f.mu.Unlock()
	if f.getOpFn != nil {
		return f.getOpFn(ctx, req)
	}
	return controlclient.OperationResult{}, controlclient.ErrNotFound
}

func (f *fakeControl) calls() (creates, revokes, lists, operations int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.createCalls), len(f.revokeCalls), len(f.listCalls), len(f.getOpCalls)
}

func (f *fakeControl) lastCreate(t interface{ Fatalf(string, ...any) }) controlclient.CreateApiKeyRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.createCalls) == 0 {
		t.Fatalf("no CreateApiKey call reached Control")
	}
	return f.createCalls[len(f.createCalls)-1]
}

func (f *fakeControl) lastList(t interface{ Fatalf(string, ...any) }) controlclient.ListApiKeysRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.listCalls) == 0 {
		t.Fatalf("no ListApiKeys call reached Control")
	}
	return f.listCalls[len(f.listCalls)-1]
}

func (f *fakeControl) lastRevoke(t interface{ Fatalf(string, ...any) }) controlclient.RevokeApiKeyRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.revokeCalls) == 0 {
		t.Fatalf("no RevokeApiKey call reached Control")
	}
	return f.revokeCalls[len(f.revokeCalls)-1]
}

// fakeUsage records the resolved usage query so scoping assertions are made on
// what the store was actually asked for.
type fakeUsage struct {
	mu                sync.Mutex
	paidQueries       []PaidUsageQuery
	paidQueryFn       func(ctx context.Context, query PaidUsageQuery) (PaidUsagePage, error)
	quotaFn           func(ctx context.Context, orgID, userID int64, month billing.BillingMonth) (billing.MemberQuota, error)
	memberSummaryFn   func(ctx context.Context, orgID int64, month billing.BillingMonth) ([]MemberBillingSummary, error)
	platformSummaryFn func(ctx context.Context, month billing.BillingMonth) (PlatformBillingSummary, error)
}

func (f *fakeUsage) QueryUsage(ctx context.Context, query PaidUsageQuery) (PaidUsagePage, error) {
	f.mu.Lock()
	f.paidQueries = append(f.paidQueries, query)
	fn := f.paidQueryFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, query)
	}
	return PaidUsagePage{Items: []PaidUsageRow{}}, nil
}

func (f *fakeUsage) GetMemberQuota(ctx context.Context, orgID, userID int64, month billing.BillingMonth) (billing.MemberQuota, error) {
	if f.quotaFn != nil {
		return f.quotaFn(ctx, orgID, userID, month)
	}
	return billing.MemberQuota{OrgID: orgID, UserID: userID, Month: month}, nil
}

func (f *fakeUsage) MemberSummary(ctx context.Context, orgID int64, month billing.BillingMonth) ([]MemberBillingSummary, error) {
	if f.memberSummaryFn != nil {
		return f.memberSummaryFn(ctx, orgID, month)
	}
	return []MemberBillingSummary{}, nil
}

func (f *fakeUsage) PlatformSummary(ctx context.Context, month billing.BillingMonth) (PlatformBillingSummary, error) {
	if f.platformSummaryFn != nil {
		return f.platformSummaryFn(ctx, month)
	}
	return PlatformBillingSummary{Month: month}, nil
}

func (f *fakeUsage) lastPaid(t interface{ Fatalf(string, ...any) }) PaidUsageQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.paidQueries) == 0 {
		t.Fatalf("no paid usage query reached the report store")
	}
	return f.paidQueries[len(f.paidQueries)-1]
}

func (f *fakeUsage) paidCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.paidQueries)
}

// sampleKey is the metadata shape Control returns; tests only override the
// fields they assert on.
func sampleKey(keyID string, orgID int64, owner int64) controlclient.KeyMetadata {
	ownerUserID := owner
	return controlclient.KeyMetadata{
		KeyID:        keyID,
		Name:         "Example key",
		KeyPrefix:    "sk-prefix...",
		OrgID:        orgID,
		MembershipID: orgID*100 + owner,
		OwnerUserID:  &ownerUserID,
		CreatedBy:    &ownerUserID,
		Status:       "active",
		CreatedAt:    time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC),
		TenantID:     "org:" + formatID(orgID),
	}
}

// twoEnterpriseMemberships is the fixture the key/usage tests share: actors in
// one organization with a role each, plus an outsider who belongs elsewhere.
//
//	org 2:  10 member, 11 admin, 12 billing_viewer, 13 owner
//	org 3:  99 owner (the outsider)
func twoEnterpriseMemberships() map[int64]map[int64]string {
	return map[int64]map[int64]string{
		10: {2: "member"},
		11: {2: "admin"},
		12: {2: "billing_viewer"},
		13: {2: "owner"},
		99: {3: "owner"},
	}
}
