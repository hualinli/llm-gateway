package metrics

import (
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// 标签集合是有界契约：入口指标只有 route/result_class（不含 tenant/key/request id），
// first_event 只有 model。改动这里等于改动告警契约，必须同步文档。
func TestEntryAndFirstEventMetricLabels(t *testing.T) {
	// 先创建子序列，否则 Gather 看不到标签名。
	EntryRequestsTotal.WithLabelValues("chat_completions", "ok").Inc()
	FirstEventSeconds.WithLabelValues("m1").Observe(0.01)
	RequestsTotal.WithLabelValues("tenant-a", "m1", "200").Inc()
	TTFTSeconds.WithLabelValues("tenant-a", "m1").Observe(0.01)
	LatencySeconds.WithLabelValues("tenant-a", "m1").Observe(0.01)

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	labels := make(map[string][]string, len(families))
	help := make(map[string]string, len(families))
	for _, family := range families {
		var names []string
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				// le 是 histogram 的分桶边界，不是调用方维度的标签。
				if label.GetName() == "le" {
					continue
				}
				names = append(names, label.GetName())
			}
		}
		sort.Strings(names)
		labels[family.GetName()] = dedupe(names)
		help[family.GetName()] = family.GetHelp()
	}

	tests := []struct {
		name       string
		wantLabels []string
	}{
		{"gateway_entry_requests_total", []string{"result_class", "route"}},
		{"gateway_first_event_seconds", []string{"model"}},
		{"gateway_requests_total", []string{"model", "status", "tenant"}},
		{"gateway_ttft_seconds", []string{"model", "tenant"}},
		{"gateway_latency_seconds", []string{"model", "tenant"}},
	}
	for _, tt := range tests {
		got, ok := labels[tt.name]
		if !ok {
			t.Fatalf("metric %s is not registered", tt.name)
		}
		if strings.Join(got, ",") != strings.Join(tt.wantLabels, ",") {
			t.Fatalf("%s labels=%v, want %v", tt.name, got, tt.wantLabels)
		}
		if strings.TrimSpace(help[tt.name]) == "" {
			t.Fatalf("%s has no help text", tt.name)
		}
	}

	// entry 指标必须明确覆盖 auth/limiter 提前返回，且说明标签是有界的。
	entryHelp := help["gateway_entry_requests_total"]
	for _, want := range []string{"auth", "limiter", "bounded"} {
		if !strings.Contains(entryHelp, want) {
			t.Fatalf("gateway_entry_requests_total help=%q, want it to mention %q", entryHelp, want)
		}
	}
	// TTFT 只是 first_event 的历史别名，且明确不是首内容 token。
	ttftHelp := help["gateway_ttft_seconds"]
	for _, want := range []string{"gateway_first_event_seconds", "NOT"} {
		if !strings.Contains(ttftHelp, want) {
			t.Fatalf("gateway_ttft_seconds help=%q, want it to mention %q", ttftHelp, want)
		}
	}
}

// 本任务明确不新增 first_content 序列（网关还没有内容 delta 判定）。
func TestNoFirstContentMetricYet(t *testing.T) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if strings.Contains(family.GetName(), "first_content") {
			t.Fatalf("unexpected first_content metric %s: measurement is deferred to T01B", family.GetName())
		}
	}
}

func dedupe(values []string) []string {
	out := make([]string, 0, len(values))
	for i, value := range values {
		if i > 0 && values[i-1] == value {
			continue
		}
		out = append(out, value)
	}
	return out
}
