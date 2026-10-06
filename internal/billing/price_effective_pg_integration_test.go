package billing

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestListEffectivePricesReturnsOnlyCurrentVersionPerModel(t *testing.T) {
	db, _ := billingTestDB(t)
	store := NewPGPriceVersionStore(db)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	first, err := store.CreatePriceVersion(context.Background(), CreatePriceVersionParams{
		Model: "model-a", InputMicroPerMillion: 11, OutputMicroPerMillion: 12, EffectiveAt: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreatePriceVersion(context.Background(), CreatePriceVersionParams{
		Model: "model-a", InputMicroPerMillion: 21, OutputMicroPerMillion: 22, EffectiveAt: base.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreatePriceVersion(context.Background(), CreatePriceVersionParams{
		Model: "model-b", InputMicroPerMillion: 31, OutputMicroPerMillion: 32, EffectiveAt: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	prices, err := store.ListEffectivePrices(context.Background(), base.Add(90*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(prices) != 2 || prices[0].Model != "model-a" || prices[1].Model != "model-b" {
		t.Fatalf("effective prices = %+v", prices)
	}
	if prices[0].ID != first.ID || prices[0].InputMicroPerMillion != 11 || prices[1].InputMicroPerMillion != 31 {
		t.Fatalf("effective selection = %+v; future version should be ignored", prices)
	}

	prices, err = store.ListEffectivePrices(context.Background(), base.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := []int64{prices[0].InputMicroPerMillion, prices[1].InputMicroPerMillion}
	if !reflect.DeepEqual(got, []int64{21, 31}) {
		t.Fatalf("latest effective prices = %v, want [21 31]", got)
	}
}
