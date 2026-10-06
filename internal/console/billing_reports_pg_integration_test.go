package console

import (
	"context"
	"testing"
	"time"

	"llm-gateway/internal/billing"
)

func TestPostgresBillingReportsSeparateSettledSalesPendingUsageAndUnknownCost(t *testing.T) {
	db := billingHTTPTestDB(t)
	orgID, ownerID := seedBillingOrg(t, db, "reports-formal")
	now := time.Now().UTC().Truncate(time.Millisecond)
	var memberID, membershipID int64
	if err := db.QueryRow(`INSERT INTO users
  (email, password_hash, status, platform_admin, auth_version, created_at, updated_at)
VALUES ('reports-member@example.test', 'hash', 'active', false, 1, $1, $1) RETURNING id`, now).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`INSERT INTO memberships
  (org_id, user_id, role, status, monthly_limit_micro, created_at, updated_at)
VALUES ($1, $2, 'member', 'active', 1000, $3, $3) RETURNING id`, orgID, memberID, now).Scan(&membershipID); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	prices := billing.NewPGPriceVersionStore(db)
	accounts := billing.NewPGAccountStore(db)
	knownInputCost, knownOutputCost := int64(500_000), int64(1_000_000)
	knownPrice, err := prices.CreatePriceVersion(ctx, billing.CreatePriceVersionParams{
		Model: "reports-known", InputMicroPerMillion: 1_000_000, OutputMicroPerMillion: 2_000_000,
		ProviderInputCostMicroPerMillion: &knownInputCost, ProviderOutputCostMicroPerMillion: &knownOutputCost,
		EffectiveAt: now.Add(-time.Hour), Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	unknownPrice, err := prices.CreatePriceVersion(ctx, billing.CreatePriceVersionParams{
		Model: "reports-unknown-cost", InputMicroPerMillion: 1_000_000, OutputMicroPerMillion: 2_000_000,
		EffectiveAt: now.Add(-time.Hour), Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	funding := billing.AdjustParams{
		OrgID: orgID, AmountMicro: 100_000, Direction: billing.DirectionCredit,
		Kind: billing.KindAdjustment, Reason: "report integration funding",
		BusinessID: "reports-funding", IdempotencyKey: "reports-funding", Now: now,
	}
	funding.PayloadDigest = billing.AdjustmentDigest(orgID, funding.AmountMicro,
		funding.Direction, funding.Kind, funding.Reason)
	if _, err := accounts.Adjust(ctx, funding); err != nil {
		t.Fatal(err)
	}

	reserve := func(id string, user, member int64, key string, price billing.PriceVersion, input, output int64) {
		t.Helper()
		params := billing.ReserveParams{
			ExecutionID: id, OrgID: orgID, UserID: user, MembershipID: member,
			KeyID: key, GatewayID: "reports-gateway", Model: price.Model, PriceVersionID: price.ID,
			InputTokensUpper: input, OutputTokensUpper: output, RequestID: "req-" + id, Now: now,
		}
		if _, err := accounts.Reserve(ctx, params); err != nil {
			t.Fatalf("reserve %s: %v", id, err)
		}
		if _, err := accounts.MarkExecuting(ctx, id, "req-"+id); err != nil {
			t.Fatalf("mark executing %s: %v", id, err)
		}
	}
	finalize := func(id, event string, userTokens, outputTokens int64) billing.Receipt {
		t.Helper()
		result, err := accounts.FinalizeReceipt(ctx, billing.FinalizeReceiptParams{
			ExecutionID: id, EventID: event, PromptTokens: userTokens, OutputTokens: outputTokens,
			TotalTokens: userTokens + outputTokens, RequestID: "req-" + id,
			OrgID: orgID, Model: knownPrice.Model, OccurredAt: now, Now: now,
		})
		if err != nil {
			t.Fatalf("finalize %s: %v", id, err)
		}
		return result.Receipt
	}

	// One member execution is fully charged. The owner has known usage but a
	// pending receipt, so its charge must remain null in the bill. A third call
	// reached the provider without valid usage and stays in manual review.
	reserve("exec_report_settled", memberID, membershipID, "key-member-private", knownPrice, 100, 10)
	settled := finalize("exec_report_settled", "evt_report_settled", 10, 2)
	settlement := billing.SettleReceiptParams{Identity: settled.Identity(), Digest: settled.Digest, Source: "reports-settle", Now: now.Add(time.Second)}
	if _, err := accounts.SettleReceipt(ctx, settlement); err != nil {
		t.Fatal(err)
	}

	ownerMembershipID := orgID
	reserve("exec_report_pending", ownerID, ownerMembershipID, "key-owner-private", knownPrice, 20, 0)
	finalize("exec_report_pending", "evt_report_pending", 5, 0)

	reserve("exec_report_unknown", memberID, membershipID, "key-member-private-2", unknownPrice, 20, 0)
	if _, err := accounts.RecordUsageException(ctx, billing.UsageExceptionParams{
		ExecutionID: "exec_report_unknown", EventID: "evt_report_unknown", Reason: "provider usage payload was absent",
	}); err != nil {
		t.Fatal(err)
	}

	month := billing.BillingMonthAt(now)
	reports := NewPGBillingReports(db)
	page, err := reports.QueryUsage(ctx, PaidUsageQuery{OrgID: orgID, Month: month, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if page.Totals.SalesMicro != 14 || page.Totals.SettledCount != 1 || page.Totals.PendingCount != 1 ||
		page.Totals.ReviewRequiredCount != 1 || page.Totals.PromptTokens != 10 ||
		page.Totals.CompletionTokens != 2 || page.Totals.TotalTokens != 12 || page.Totals.UnknownUsageCount != 1 {
		t.Fatalf("formal usage totals=%+v", page.Totals)
	}
	if page.Totals.ObservedPromptTokens != 15 || page.Totals.ObservedCompletionTokens != 2 || page.Totals.ObservedTotalTokens != 17 {
		t.Fatalf("observed token totals=%+v", page.Totals)
	}
	charges := make(map[string]*int64, len(page.Items))
	for _, item := range page.Items {
		charges[item.ExecutionID] = item.ChargeMicro
		if item.UserID != nil && *item.UserID != ownerID && item.KeyID == nil {
			t.Fatalf("internal paid usage row unexpectedly lost its owner key id: %+v", item)
		}
	}
	if charges["exec_report_settled"] == nil || *charges["exec_report_settled"] != 14 ||
		charges["exec_report_pending"] != nil || charges["exec_report_unknown"] != nil {
		t.Fatalf("sales row charges=%v, pending and unknown must not look like zero-cost executions", charges)
	}

	members, err := reports.MemberSummary(ctx, orgID, month)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("member summaries=%+v, want owner and member only", members)
	}
	var memberSummary MemberBillingSummary
	for _, summary := range members {
		if summary.UserID == memberID {
			memberSummary = summary
		}
	}
	if memberSummary.SalesMicro != 14 || memberSummary.SettledCount != 1 || memberSummary.ReviewRequiredCount != 1 ||
		memberSummary.PromptTokens != 10 || memberSummary.CompletionTokens != 2 || memberSummary.TotalTokens != 12 ||
		memberSummary.UnknownUsageCount != 1 {
		t.Fatalf("member summary=%+v", memberSummary)
	}

	platform, err := reports.PlatformSummary(ctx, month)
	if err != nil {
		t.Fatal(err)
	}
	if platform.SalesMicro != 14 || platform.SettledCount != 1 || platform.PendingCount != 1 ||
		platform.ReviewRequiredCount != 1 || platform.KnownProviderCostMicro == 0 ||
		platform.UnknownProviderCostCount != 1 || platform.UnknownUsageCount != 1 {
		t.Fatalf("platform summary=%+v", platform)
	}
}
