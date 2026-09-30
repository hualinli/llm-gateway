package console

import (
	"context"
	"sync"
	"time"

	"llm-gateway/internal/billing"
)

// fakePrices records the price-version calls the handlers make.
type fakePrices struct {
	mu          sync.Mutex
	versions    []billing.PriceVersion
	createFn    func(context.Context, billing.CreatePriceVersionParams) (billing.PriceVersion, error)
	listFn      func(context.Context, billing.ListPriceVersionsParams) (billing.PriceVersionPage, error)
	createCalls []billing.CreatePriceVersionParams
	listCalls   []billing.ListPriceVersionsParams
}

func (f *fakePrices) CreatePriceVersion(ctx context.Context, params billing.CreatePriceVersionParams) (billing.PriceVersion, error) {
	f.mu.Lock()
	f.createCalls = append(f.createCalls, params)
	fn := f.createFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, params)
	}
	version := billing.PriceVersion{
		ID:                    int64(len(f.createCalls)),
		Model:                 params.Model,
		Currency:              params.Currency,
		InputMicroPerMillion:  params.InputMicroPerMillion,
		OutputMicroPerMillion: params.OutputMicroPerMillion,
		EffectiveAt:           params.EffectiveAt,
		CreatedAt:             params.Now,
	}
	if params.CreatedBy > 0 {
		createdBy := params.CreatedBy
		version.CreatedBy = &createdBy
	}
	return version, nil
}

func (f *fakePrices) ListPriceVersions(ctx context.Context, params billing.ListPriceVersionsParams) (billing.PriceVersionPage, error) {
	f.mu.Lock()
	f.listCalls = append(f.listCalls, params)
	fn := f.listFn
	versions := append([]billing.PriceVersion(nil), f.versions...)
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, params)
	}
	return billing.PriceVersionPage{Items: versions}, nil
}

// fakeAccounts records the account calls the handlers make.
type fakeAccounts struct {
	mu          sync.Mutex
	account     *billing.Account
	entries     []billing.Entry
	nextCursor  *billing.EntriesCursor
	getFn       func(context.Context, int64) (billing.Account, error)
	listFn      func(context.Context, billing.ListEntriesParams) (billing.EntryPage, error)
	adjustFn    func(context.Context, billing.AdjustParams) (billing.AdjustmentResult, error)
	getCalls    []int64
	listCalls   []billing.ListEntriesParams
	adjustCalls []billing.AdjustParams
}

func (f *fakeAccounts) GetAccount(ctx context.Context, orgID int64) (billing.Account, error) {
	f.mu.Lock()
	f.getCalls = append(f.getCalls, orgID)
	fn := f.getFn
	account := f.account
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, orgID)
	}
	if account == nil {
		return billing.Account{}, billing.ErrAccountNotFound
	}
	return *account, nil
}

func (f *fakeAccounts) ListEntries(ctx context.Context, params billing.ListEntriesParams) (billing.EntryPage, error) {
	f.mu.Lock()
	f.listCalls = append(f.listCalls, params)
	fn := f.listFn
	entries := append([]billing.Entry(nil), f.entries...)
	next := f.nextCursor
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, params)
	}
	return billing.EntryPage{Items: entries, NextCursor: next}, nil
}

func (f *fakeAccounts) Adjust(ctx context.Context, params billing.AdjustParams) (billing.AdjustmentResult, error) {
	f.mu.Lock()
	f.adjustCalls = append(f.adjustCalls, params)
	fn := f.adjustFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, params)
	}
	account := billing.Account{OrgID: params.OrgID, Currency: "CNY", Status: "active", Version: 2}
	if params.Direction == billing.DirectionCredit {
		account.PostedBalanceMicro = params.AmountMicro
	} else {
		account.PostedBalanceMicro = -params.AmountMicro
	}
	return billing.AdjustmentResult{
		Entry: billing.Entry{
			ID:          1,
			OrgID:       params.OrgID,
			Kind:        params.Kind,
			AmountMicro: params.AmountMicro,
			Direction:   params.Direction,
			BusinessID:  params.BusinessID,
			PostedAt:    params.Now,
			CreatedAt:   params.Now,
		},
		Account: account,
	}, nil
}

// sampleAccount is the fixture the billing read tests share.
func sampleAccount(orgID int64) billing.Account {
	return billing.Account{
		OrgID:              orgID,
		Currency:           "CNY",
		PostedBalanceMicro: 12_500_000,
		ReservedMicro:      2_500_000,
		Status:             "active",
		Version:            3,
		CreatedAt:          time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt:          time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC),
	}
}
