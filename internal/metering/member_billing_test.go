package metering

import "testing"

func TestMemberBillsAreTenantScopedAndIdempotent(t *testing.T) {
	ledger := NewMemoryLedger()
	for _, event := range []Event{
		{EventID: "a", TenantID: "enterprise", UserID: 10, KeyID: "key-1", CostCents: 7},
		{EventID: "b", TenantID: "enterprise", UserID: 10, KeyID: "key-2", CostCents: 3},
		{EventID: "c", TenantID: "enterprise", UserID: 11, CostCents: 5},
		{EventID: "d", TenantID: "personal", UserID: 10, CostCents: 2},
		{EventID: "e", TenantID: "enterprise", CostCents: 4},
	} {
		if added, err := ledger.Add(event); err != nil || !added {
			t.Fatalf("add: %v %v", added, err)
		}
	}
	if added, _ := ledger.Add(Event{EventID: "a", TenantID: "enterprise", UserID: 10, CostCents: 7}); added {
		t.Fatal("duplicate charged twice")
	}
	if total, _ := ledger.Balance("enterprise"); total != 19 {
		t.Fatalf("tenant total=%d", total)
	}
	if total, _ := ledger.MemberBalance("enterprise", 10); total != 10 {
		t.Fatalf("member total=%d", total)
	}
	if total, _ := ledger.MemberBalance("personal", 10); total != 2 {
		t.Fatalf("personal total=%d", total)
	}
	bills, err := ledger.MemberBills("enterprise")
	if err != nil || len(bills) != 2 || bills[0].UserID != 10 || bills[0].CostCents != 10 || bills[0].EventCount != 2 || bills[1].UserID != 11 {
		t.Fatalf("member bills: %+v %v", bills, err)
	}
}
