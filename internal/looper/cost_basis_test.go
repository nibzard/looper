package looper

import (
	"encoding/json"
	"testing"
	"time"
)

func TestReportedCostRetainsUnknownBasis(t *testing.T) {
	raw := []byte(`{"type":"result","total_cost_usd":1.485591,"modelUsage":{"glm-5.3[1m]":{"inputTokens":43181,"outputTokens":14342,"cacheReadInputTokens":1822272,"cacheCreationInputTokens":0,"costUSD":1.485591,"provider":"firstParty","costBasis":"unknown"}}}`)
	r := parseUsage("claude", "", raw, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err = json.Unmarshal(encoded, &saved); err != nil {
		t.Fatal(err)
	}
	u := saved["models"].([]any)[0].(map[string]any)
	if u["reported_cost_basis"] != "unknown" || u["agent_provider"] != "firstParty" {
		t.Fatalf("lost agent cost provenance: %s", encoded)
	}
	assertUSD(t, r.Models[0].EstimatedUSD, 0.59734892)
	assertUSD(t, r.ReportedUSD, 1.485591)
}

func TestCostBasisAggregationPreservesMissingAndMixedHistory(t *testing.T) {
	s := timingStore(t)
	for i, basis := range []string{"", "provider_api"} {
		value := 1.0
		report := UsageReport{Models: []UsageRecord{{Model: "glm-5.3", Input: 1, ReportedUSD: &value, ReportedCostBasis: basis}}, ReportedUSD: &value, ReportedCostBasis: basis}
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec("INSERT INTO usage_reports VALUES(?,?,?,?,?)", basis, basis, timingAt(i), timingAt(i), string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	report, err := s.Statistics(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.ReportedCostBasis != "mixed" || report.Models[0].ReportedCostBasis != "mixed" {
		t.Fatalf("missing basis became trusted: %+v", report)
	}
	assertUSD(t, report.ReportedUSD, 2)
}
