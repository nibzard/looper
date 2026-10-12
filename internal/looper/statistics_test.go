package looper

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func assertUSD(t *testing.T, value *float64, want float64) {
	t.Helper()
	if value == nil || math.Abs(*value-want) > 1e-9 {
		t.Fatalf("cost %v, want %v", value, want)
	}
}

func TestUsageClaudeTotalsAndCachePricing(t *testing.T) {
	raw := []byte(`{"type":"system","subtype":"init","model":"claude-sonnet-4-6"}
{"type":"assistant","message":{"usage":{"input_tokens":99999999}}}
{"type":"result","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":500,"cache_creation_input_tokens":200,"cache_creation":{"ephemeral_1h_input_tokens":100}},"total_cost_usd":0.5}
{"type":"result","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":500,"cache_creation_input_tokens":200,"cache_creation":{"ephemeral_1h_input_tokens":100}},"total_cost_usd":0.5}`)
	report := parseUsage("claude", "sonnet", raw, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	if len(report.Models) != 1 {
		t.Fatalf("duplicate results counted: %+v", report)
	}
	u := report.Models[0]
	if u.Input != 800 || u.Output != 20 || u.CacheRead != 500 || u.CacheWrite1h != 100 {
		t.Fatalf("bad normalization: %+v", u)
	}
	assertUSD(t, u.EstimatedUSD, (100*3+500*.3+100*3.75+100*6+20*15)/1e6)
	assertUSD(t, report.ReportedUSD, .5)
}

func TestUsageModelBreakdownReplacesOverallTotals(t *testing.T) {
	raw := []byte(`{"type":"result","usage":{"input_tokens":1000,"output_tokens":1000},"modelUsage":{"glm-5.3[1m]":{"inputTokens":100,"outputTokens":20,"cacheReadInputTokens":500,"cacheCreationInputTokens":0,"costUSD":1},"unknown-model":{"inputTokens":40,"outputTokens":10,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":2}},"total_cost_usd":3}`)
	report := parseUsage("claude", "", raw, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	if len(report.Models) != 2 {
		t.Fatal(report)
	}
	if report.Models[0].Input != 600 || report.Models[1].Input != 40 {
		t.Fatal("overall totals were double counted")
	}
	assertUSD(t, report.Models[0].EstimatedUSD, (100*1.4+500*.26+20*4.4)/1e6)
	if report.Models[1].EstimatedUSD != nil {
		t.Fatal("unknown model was assigned a price")
	}
	assertUSD(t, report.ReportedUSD, 3)
}

func TestUsageCodexCachedInputAndMultipleTurns(t *testing.T) {
	report := parseUsage("codex", "gpt-5.3-codex", []byte(`{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":100}}
{"type":"item.completed","usage":{"input_tokens":999999}}
{"type":"turn.completed","usage":{"input_tokens":200,"cached_input_tokens":0,"output_tokens":20}}`), time.Now())
	if len(report.Models) != 2 {
		t.Fatal(report)
	}
	assertUSD(t, report.Models[0].EstimatedUSD, (200*1.75+800*.175+100*14)/1e6)
	assertUSD(t, report.Models[1].EstimatedUSD, (200*1.75+20*14)/1e6)
	// A missing model must not be guessed from today's default.
	missing := parseUsage("codex", "", []byte(`{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":10}}`), time.Now())
	if len(missing.Models) != 1 || missing.Models[0].EstimatedUSD != nil {
		t.Fatal("unknown model was guessed")
	}
}

func TestUsageMissingAndInvalidCountsStayUnknown(t *testing.T) {
	for _, raw := range []string{
		`{"type":"result","result":"no usage"}`,
		`{"type":"result","usage":{"input_tokens":-1,"output_tokens":10}}`,
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":100,"output_tokens":1}}`,
	} {
		for _, agent := range []string{"claude", "codex"} {
			if r := parseUsage(agent, "", []byte(raw), time.Now()); len(r.Models) > 0 {
				t.Fatalf("invalid usage accepted: %+v", r)
			}
		}
	}
}

func TestPricingHistoricalConditionsAndUnknownTiers(t *testing.T) {
	old := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	current := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	record := UsageRecord{Model: "glm-5.3-flash", Input: 1_000_000, Output: 1_000_000}
	oldPrice, _ := estimateUsage(record, old)
	newPrice, _ := estimateUsage(record, current)
	assertUSD(t, oldPrice, .075+.25)
	assertUSD(t, newPrice, .15+.5)
	tiered := UsageRecord{Model: "gpt-6.1-sol", Input: 1_000_000, Output: 10}
	if price, _ := estimateUsage(tiered, current); price != nil {
		t.Fatal("cumulative usage guessed a per-request context tier")
	}
	if price, _ := estimateUsage(UsageRecord{Model: "glm-5.3", Input: 100, CacheRead: 200}, current); price != nil {
		t.Fatal("negative uncached usage received a price")
	}
}

func TestStatisticsDurableTotalsAndMidnightIntervals(t *testing.T) {
	s := timingStore(t)
	now := time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC)
	start, end := "2026-01-01T23:30:00Z", "2026-01-02T00:30:00Z"
	if _, err := s.db.Exec(`INSERT INTO processes VALUES('p','','task-a','agent','agent.jsonl',123,'identity',?,?,'success','recorded')`, start, end); err != nil {
		t.Fatal(err)
	}
	p := ProcessObservation{Started: start, Ended: end}
	artifact := filepath.Join(s.dir, "task-a", "work", "events-00.jsonl")
	raw := []byte(`{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":500,"output_tokens":100}}
{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":500,"output_tokens":100}}`)
	for i := 0; i < 2; i++ {
		if err := s.saveUsage(artifact, p, "codex", "gpt-5.3-codex", "recorded", raw); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Event("result", "T1  done | complete", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Check("task-a", "rev", "executable", "pass", "log"); err != nil {
		t.Fatal(err)
	}
	report, err := s.Statistics(now)
	if err != nil {
		t.Fatal(err)
	}
	if report.UsageReports != 1 || report.Tokens != 2200 || report.Input != 2000 || report.CacheRead != 1000 || report.ChecksPassed != 1 || report.Integrations != 1 {
		t.Fatalf("incorrect totals: %+v", report)
	}
	if len(report.Models) != 1 || report.Models[0].Reports != 1 || report.Models[0].Priced != 1 {
		t.Fatal("turns masqueraded as usage reports")
	}
	if report.Days[12].WorkerSeconds != 1800 || report.Days[13].WorkerSeconds != 1800 || report.Days[13].Tokens != 2200 {
		t.Fatal("interval was not split at UTC midnight")
	}
	assertUSD(t, report.EstimatedUSD, (1000*1.75+1000*.175+200*14)/1e6)
	if report.ReportedUSD != nil {
		t.Fatal("estimated cost became a reported bill")
	}
}

func TestUsageImportAndNativeReportDoNotDuplicate(t *testing.T) {
	s := timingStore(t)
	artifact := filepath.Join(s.dir, "task-a", "work", "events-00.jsonl")
	p := ProcessObservation{Started: timingAt(1), Ended: timingAt(2)}
	raw := []byte(`{"type":"result","usage":{"input_tokens":100,"output_tokens":10}}`)
	if err := s.saveUsage(artifact, p, "claude", "unknown", "recorded", raw); err != nil {
		t.Fatal(err)
	}
	if err := s.importUsage(artifact, p); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM usage_reports").Scan(&count); err != nil || count != 1 {
		t.Fatal("native and imported reports duplicated")
	}
	r, err := s.Statistics(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r.UsageReports != 1 || r.EstimatedUSD != nil || r.PricedReports != 0 {
		t.Fatal("unpriced usage was fabricated")
	}
}

func TestPricingSnapshotHasProvenanceAndSupportedModels(t *testing.T) {
	var providers []priceProvider
	if err := json.Unmarshal(priceData, &providers); err != nil {
		t.Fatal(err)
	}
	var source map[string]string
	if err := json.Unmarshal(priceSource, &source); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(source["url"], source["commit"]) || source["sha256"] == "" {
		t.Fatal("pricing snapshot has no pinned provenance")
	}
	checksum := sha256.Sum256(priceData)
	if hex.EncodeToString(checksum[:]) != source["sha256"] {
		t.Fatal("pricing bytes do not match their provenance")
	}
	if len(providers) < 3 {
		t.Fatal("missing provider data")
	}
}

func TestUsageHistoricalImportIsReadableWithoutWrites(t *testing.T) {
	s := timingStore(t)
	artifact := filepath.Join(s.dir, "task-old", "work-session", "events-00.jsonl")
	p := ProcessObservation{Started: "2026-10-10T12:00:00Z", Ended: "2026-10-10T12:01:00Z"}
	if err := writeJSON(artifact+".process.json", p); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(filepath.Dir(artifact), "invocation.json"), map[string]string{"agent": "claude", "model": "glm-5.3"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, []byte(`{"type":"result","usage":{"input_tokens":100,"cache_read_input_tokens":500,"output_tokens":20},"total_cost_usd":0.01}`), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.importProcessTimes(); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := OpenStoreReadonly(filepath.Join(s.dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	report, err := reader.Statistics(now)
	if err != nil {
		t.Fatal(err)
	}
	if report.UsageReports != 1 || report.AgentRuns != 1 || report.Tokens != 620 || report.Days[11].Reports != 1 {
		t.Fatalf("imported reports missing or duplicated: %+v", report)
	}
	assertUSD(t, report.EstimatedUSD, (100*1.4+500*.26+20*4.4)/1e6)
	assertUSD(t, report.ReportedUSD, 0.01)
}

func TestStatisticsKeepsAttemptAndRepairHistory(t *testing.T) {
	s := timingStore(t)
	if _, err := s.db.Exec(`INSERT INTO processes VALUES('old','','task-old','agent','old.jsonl',123,'identity',?,?,'success','recorded')`, timingAt(1), timingAt(2)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(&State{Version: 2, Attempts: []*Attempt{{ID: "task-new"}}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.Event("repair", "repair old task", nil); err != nil {
			t.Fatal(err)
		}
	}
	report, err := s.Statistics(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Attempts != 2 || report.Repairs != 2 {
		t.Fatalf("current state erased effort history: %+v", report)
	}
}
