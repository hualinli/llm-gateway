package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/metering"
	"llm-gateway/internal/metrics"

	"github.com/prometheus/client_golang/prometheus"
)

func settlementLookup(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// TestSettlementConfigFromEnvDefaultsAndModes pins the knob parsing: an explicit
// off/on is honored, an unknown mode is refused instead of silently treated as
// auto, and the defaults match the dispatcher's documented bounds.
func TestSettlementConfigFromEnvDefaultsAndModes(t *testing.T) {
	base, err := settlementConfigFromEnv(settlementLookup(nil))
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if base.Mode != settlementModeAuto || base.BatchSize != billing.DefaultDispatchBatchSize ||
		base.Concurrency != billing.DefaultDispatchConcurrency || base.MaxAttempts != billing.DefaultMaxDispatchAttempts {
		t.Fatalf("defaults=%+v", base)
	}
	if base.StaleAfter <= 0 || base.BacklogInterval <= 0 {
		t.Fatalf("defaults=%+v", base)
	}

	off, err := settlementConfigFromEnv(settlementLookup(map[string]string{"METERING_SETTLEMENT": "off"}))
	if err != nil || off.Mode != settlementModeOff {
		t.Fatalf("off=(%+v, %v)", off, err)
	}
	on, err := settlementConfigFromEnv(settlementLookup(map[string]string{"METERING_SETTLEMENT": "ON"}))
	if err != nil || on.Mode != settlementModeOn {
		t.Fatalf("on=(%+v, %v)", on, err)
	}
	if _, err := settlementConfigFromEnv(settlementLookup(map[string]string{"METERING_SETTLEMENT": "of"})); err == nil {
		t.Fatal("a typo in METERING_SETTLEMENT must be refused, not silently defaulted")
	}

	tuned, err := settlementConfigFromEnv(settlementLookup(map[string]string{
		"METERING_SETTLEMENT_BATCH":        "16",
		"METERING_SETTLEMENT_CONCURRENCY":  "4",
		"METERING_SETTLEMENT_LEASE":        "45s",
		"METERING_SETTLEMENT_MAX_ATTEMPTS": "9",
	}))
	if err != nil {
		t.Fatalf("tuned: %v", err)
	}
	if tuned.BatchSize != 16 || tuned.Concurrency != 4 || tuned.Lease != 45*time.Second || tuned.MaxAttempts != 9 {
		t.Fatalf("tuned=%+v", tuned)
	}

	// Out-of-range values are refused rather than clamped silently.
	if _, err := settlementConfigFromEnv(settlementLookup(map[string]string{"METERING_SETTLEMENT_BATCH": "0"})); err == nil {
		t.Fatal("batch=0 must be refused")
	}
	if _, err := settlementConfigFromEnv(settlementLookup(map[string]string{"METERING_SETTLEMENT_LEASE": "-1s"})); err == nil {
		t.Fatal("a negative lease must be refused")
	}
}

// TestStartSettlementRefusesToPretendWithoutDurability: with an in-memory ledger
// the pipeline is explicitly disabled (auto) or a startup error (on / production),
// never a silently missing settlement path.
func TestStartSettlementRefusesToPretendWithoutDurability(t *testing.T) {
	ledger := metering.NewMemoryLedger()
	ctx := context.Background()

	disabled, err := startSettlement(ctx, nil, ledger, settlementConfig{Mode: settlementModeOff})
	if err != nil || disabled != nil {
		t.Fatalf("off=(%+v, %v), want a disabled runtime", disabled, err)
	}
	auto, err := startSettlement(ctx, nil, ledger, settlementConfig{Mode: settlementModeAuto})
	if err != nil || auto != nil {
		t.Fatalf("auto with an in-memory ledger=(%+v, %v), want disabled", auto, err)
	}
	if _, err := startSettlement(ctx, nil, ledger, settlementConfig{Mode: settlementModeOn}); err == nil {
		t.Fatal("METERING_SETTLEMENT=on without PostgreSQL must fail closed")
	}
	if _, err := startSettlement(ctx, nil, ledger, settlementConfig{Mode: settlementModeAuto, RequirePostgres: true}); err == nil {
		t.Fatal("production (METERING_REQUIRE_POSTGRES) without PostgreSQL must fail closed")
	}
}

// TestSettlementReadinessWithoutAPipeline: a process that never enabled
// settlement reports ready (an explicit operator choice), not degraded.
func TestSettlementReadinessWithoutAPipeline(t *testing.T) {
	previous := settlementRef.Load()
	settlementRef.Store(nil)
	t.Cleanup(func() { settlementRef.Store(previous) })
	if err := settlementReadiness(); err != nil {
		t.Fatalf("disabled pipeline must not degrade readiness: %v", err)
	}
}

// TestSettlementObserverPublishesBoundedMetrics is the observable-metrics claim:
// the dispatcher's outcomes and the settlement results exist as labelled series
// with a closed label vocabulary.
func TestSettlementObserverPublishesBoundedMetrics(t *testing.T) {
	observer := settlementObserver{}
	for _, result := range billing.DispatchResults {
		observer.ObserveDispatch(result, 2, time.Millisecond)
	}
	// The settlement-side families are exercised by the consumer; touch them so
	// this test asserts on registered series rather than on absent families.
	metrics.MeteringSettlementTotal.WithLabelValues("settled").Add(0)
	metrics.MeteringSettlementLagSeconds.Observe(0.001)
	metrics.MeteringReceiptExceptionTotal.WithLabelValues("state").Add(0)
	metrics.MeteringReceiptSettledTotal.Add(0)
	metrics.MeteringReceiptDispatchAttemptsTotal.Add(0)
	metrics.MeteringReceiptOldestDueSeconds.Set(0)
	metrics.ReceiptBacklog.WithLabelValues("due").Set(0)
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	found := map[string]map[string]float64{}
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "metering_receipt_") && !strings.HasPrefix(family.GetName(), "metering_settlement") {
			continue
		}
		values := map[string]float64{}
		for _, metric := range family.GetMetric() {
			key := ""
			for _, label := range metric.GetLabel() {
				if key != "" {
					key += ","
				}
				key += label.GetName() + "=" + label.GetValue()
			}
			switch {
			case metric.GetCounter() != nil:
				values[key] = metric.GetCounter().GetValue()
			case metric.GetGauge() != nil:
				values[key] = metric.GetGauge().GetValue()
			}
		}
		found[family.GetName()] = values
	}
	for _, result := range billing.DispatchResults {
		values, ok := found["metering_receipt_dispatch_total"]
		if !ok {
			t.Fatal("metering_receipt_dispatch_total is not registered")
		}
		if _, ok := values["result="+result]; !ok {
			t.Fatalf("dispatch result %q has no series: %v", result, values)
		}
	}
	if values := found["metering_receipt_dispatch_total"]; values["result=published"] != 2 {
		t.Fatalf("published count=%v, want 2", values["result=published"])
	}
	if _, ok := found["metering_receipt_oldest_due_age_seconds"]; !ok {
		t.Fatal("metering_receipt_oldest_due_age_seconds is not registered")
	}
	if _, ok := found["metering_settlement_lag_seconds"]; !ok {
		t.Fatal("metering_settlement_lag_seconds is not registered")
	}
	// The label vocabulary stays bounded: no execution/org/event ids.
	for name, values := range found {
		for key := range values {
			for _, forbidden := range []string{"execution_id", "org_id", "event_id", "receipt"} {
				if strings.Contains(key, forbidden+"=") {
					t.Fatalf("%s carries an unbounded label: %s", name, key)
				}
			}
		}
	}
}

// TestSettlementMetricsAreRegisteredWithBoundedLabels checks the settlement
// counters/histograms exist with only closed label names.
func TestSettlementMetricsAreRegisteredWithBoundedLabels(t *testing.T) {
	// Prometheus only exposes a family once it has at least one series, so touch
	// each one first: the contract under test is the label vocabulary.
	metrics.MeteringSettlementTotal.WithLabelValues("settled").Add(0)
	metrics.MeteringReceiptExceptionTotal.WithLabelValues("state").Add(0)
	metrics.MeteringReceiptDispatchTotal.WithLabelValues("published").Add(0)
	metrics.ReceiptBacklog.WithLabelValues("due").Set(0)
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string][]string{
		"metering_receipt_dispatch_total":  {"result"},
		"metering_receipt_exception_total": {"reason"},
		"metering_settlement_total":        {"result"},
		"metering_receipt_backlog":         {"status"},
	}
	seen := map[string][]string{}
	for _, family := range families {
		names := []string{}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "le" {
					continue
				}
				names = append(names, label.GetName())
			}
		}
		seen[family.GetName()] = names
	}
	for name, labels := range want {
		if _, ok := seen[name]; !ok {
			// A counter/gauge with no observed child yet is still registered, but
			// prometheus only exposes families with at least one series; create
			// one so the contract is checked rather than skipped.
			t.Fatalf("%s is not registered (observe one sample first)", name)
		}
		for _, want := range labels {
			found := false
			for _, got := range seen[name] {
				if got == want {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s labels=%v, want %v", name, seen[name], labels)
			}
		}
	}
}
