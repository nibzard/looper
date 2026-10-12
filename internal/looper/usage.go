package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type UsageRecord struct {
	Model             string   `json:"model"`
	Provider          string   `json:"provider,omitempty"`
	Input             int64    `json:"input_tokens"` // Includes cache reads and writes.
	Output            int64    `json:"output_tokens"`
	CacheRead         int64    `json:"cache_read_tokens"`
	CacheWrite        int64    `json:"cache_write_tokens"`
	CacheWrite1h      int64    `json:"cache_write_1h_tokens"`
	Reasoning         int64    `json:"reasoning_tokens"` // A subset of output, not extra tokens.
	WebSearches       int64    `json:"web_searches"`
	EstimatedUSD      *float64 `json:"estimated_usd"`
	ReportedUSD       *float64 `json:"reported_usd"`
	ReportedCostBasis string   `json:"reported_cost_basis,omitempty"`
	AgentProvider     string   `json:"agent_provider,omitempty"`
}

type UsageReport struct {
	Version           int           `json:"schema_version"`
	Agent             string        `json:"agent"`
	Started           string        `json:"started"`
	Time              string        `json:"time"`
	Models            []UsageRecord `json:"models"`
	ReportedUSD       *float64      `json:"reported_usd"`
	ReportedCostBasis string        `json:"reported_cost_basis,omitempty"`
	PricingRevision   string        `json:"pricing_revision"`
	PricingDate       string        `json:"pricing_date"`
	Source            string        `json:"source"`
}

func tokenField(m map[string]json.RawMessage, key string) (int64, bool) {
	raw, ok := m[key]
	if !ok {
		return 0, false
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil || value < 0 {
		return 0, false
	}
	return value, true
}
func usdField(raw json.RawMessage) *float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v float64
	if json.Unmarshal(raw, &v) != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func parseClaudeUsage(model string, raw json.RawMessage, camel bool) (UsageRecord, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return UsageRecord{}, false
	}
	in, out, read, write := "input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"
	if camel {
		in, out, read, write = "inputTokens", "outputTokens", "cacheReadInputTokens", "cacheCreationInputTokens"
	}
	input, hasIn := tokenField(fields, in)
	output, hasOut := tokenField(fields, out)
	if !hasIn || !hasOut {
		return UsageRecord{}, false
	}
	for _, key := range []string{read, write} {
		if _, exists := fields[key]; exists {
			if _, ok := tokenField(fields, key); !ok {
				return UsageRecord{}, false
			}
		}
	}
	u := UsageRecord{Model: model, Input: input, Output: output}
	u.CacheRead, _ = tokenField(fields, read)
	u.CacheWrite, _ = tokenField(fields, write)
	u.Input += u.CacheRead + u.CacheWrite
	if camel {
		u.ReportedUSD = usdField(fields["costUSD"])
		_ = json.Unmarshal(fields["costBasis"], &u.ReportedCostBasis)
		_ = json.Unmarshal(fields["provider"], &u.AgentProvider)
		if u.ReportedUSD != nil && u.ReportedCostBasis == "" {
			u.ReportedCostBasis = "unknown"
		}
		u.Reasoning, _ = tokenField(fields, "thinkingTokens")
		u.WebSearches, _ = tokenField(fields, "webSearchRequests")
	} else {
		var cache, details, tools map[string]json.RawMessage
		_ = json.Unmarshal(fields["cache_creation"], &cache)
		u.CacheWrite1h, _ = tokenField(cache, "ephemeral_1h_input_tokens")
		_ = json.Unmarshal(fields["output_tokens_details"], &details)
		u.Reasoning, _ = tokenField(details, "thinking_tokens")
		_ = json.Unmarshal(fields["server_tool_use"], &tools)
		u.WebSearches, _ = tokenField(tools, "web_search_requests")
	}
	return u, u.Input >= 0 && u.CacheWrite1h <= u.CacheWrite && u.Reasoning <= u.Output
}

// Read only public usage fields from known event types. Result totals replace
// message usage; repeated content blocks and result wrappers never add usage.
func parseUsage(agent, fallback string, raw []byte, at time.Time) UsageReport {
	report := UsageReport{Version: 1, Agent: agent, Time: at.UTC().Format(time.RFC3339Nano)}
	model := fallback
	var result map[string]json.RawMessage
	var codex []UsageRecord
	nonstandard := false
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		var event map[string]json.RawMessage
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		var typ string
		_ = json.Unmarshal(event["type"], &typ)
		if typ == "system" || typ == "thread.started" {
			var name string
			if json.Unmarshal(event["model"], &name) == nil && name != "" {
				model = name
			}
		}
		if typ == "result" {
			result = event
		}
		if agent == "codex" && typ == "turn.completed" {
			var fields map[string]json.RawMessage
			if json.Unmarshal(event["usage"], &fields) != nil {
				continue
			}
			input, okIn := tokenField(fields, "input_tokens")
			output, okOut := tokenField(fields, "output_tokens")
			if !okIn || !okOut {
				continue
			}
			cached, _ := tokenField(fields, "cached_input_tokens")
			if _, exists := fields["cached_input_tokens"]; exists {
				if _, ok := tokenField(fields, "cached_input_tokens"); !ok {
					continue
				}
			}
			if cached > input {
				continue
			}
			codex = append(codex, UsageRecord{Model: model, Input: input, Output: output, CacheRead: cached})
		}
	}
	if agent == "codex" {
		report.Models = codex
	} else if result != nil {
		report.ReportedUSD = usdField(result["total_cost_usd"])
		var models map[string]json.RawMessage
		_ = json.Unmarshal(result["modelUsage"], &models)
		var keys []string
		for key := range models {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if u, ok := parseClaudeUsage(key, models[key], true); ok {
				report.Models = append(report.Models, u)
			}
		}
		var usage map[string]json.RawMessage
		_ = json.Unmarshal(result["usage"], &usage)
		var tier, speed string
		_ = json.Unmarshal(usage["service_tier"], &tier)
		_ = json.Unmarshal(usage["speed"], &speed)
		nonstandard = (tier != "" && tier != "standard") || (speed != "" && speed != "standard")
		if len(report.Models) == 0 {
			if u, ok := parseClaudeUsage(model, result["usage"], false); ok {
				report.Models = append(report.Models, u)
			}
		}
		// Only the overall usage reports the cache write lifetime. With mixed
		// models, do not allocate one-hour writes to a model by guessing.
		var cache map[string]json.RawMessage
		_ = json.Unmarshal(usage["cache_creation"], &cache)
		hour, _ := tokenField(cache, "ephemeral_1h_input_tokens")
		if hour > 0 {
			if len(report.Models) == 1 && hour <= report.Models[0].CacheWrite {
				report.Models[0].CacheWrite1h = hour
			} else {
				nonstandard = true
			}
		}
	}
	var provenance struct {
		Commit    string `json:"commit"`
		Retrieved string `json:"retrieved_at"`
	}
	_ = json.Unmarshal(priceSource, &provenance)
	report.PricingRevision, report.PricingDate = provenance.Commit, provenance.Retrieved
	for i := range report.Models {
		u := &report.Models[i]
		if report.ReportedUSD != nil {
			mergeCostBasis(&report.ReportedCostBasis, u.ReportedCostBasis)
		}
		estimate, provider := estimateUsage(*u, at)
		u.Provider = provider
		if !nonstandard {
			u.EstimatedUSD = estimate
		}
	}
	if report.ReportedUSD != nil && report.ReportedCostBasis == "" {
		report.ReportedCostBasis = "unknown"
	}
	return report
}

// Missing provenance never becomes a verified rate basis during aggregation.
func mergeCostBasis(total *string, basis string) {
	if basis == "" {
		basis = "unknown"
	}
	if *total == "" {
		*total = basis
	} else if *total != basis {
		*total = "mixed"
	}
}

func (s *Store) saveUsage(artifact string, p ProcessObservation, agent, model, source string, raw []byte) error {
	at, err := time.Parse(time.RFC3339Nano, p.Started)
	if err != nil {
		return err
	}
	report := parseUsage(agent, model, raw, at)
	report.Started, report.Source = p.Started, source
	if p.Ended != "" {
		report.Time = p.Ended
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return err
	}
	id := digest([]byte(artifact + "\x00" + p.Started))
	_, err = s.db.Exec(`INSERT INTO usage_reports(id,artifact,started,recorded_at,data) VALUES(?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET recorded_at=excluded.recorded_at,data=excluded.data`, id, artifact, p.Started, report.Time, string(encoded))
	return err
}

func recordAgentUsage(ctx context.Context, artifact, agent, model string, raw []byte) {
	store, _ := ctx.Value(timingContextKey{}).(*Store)
	if store == nil {
		return
	}
	var p ProcessObservation
	if readJSON(artifact+".process.json", &p) != nil {
		return
	}
	if err := store.saveUsage(artifact, p, agent, model, "recorded", raw); err != nil {
		// Statistics cannot turn a successful task into an implementation failure.
		_ = writeJSON(artifact+".usage-error.json", map[string]string{"error": fmt.Sprint(err)})
	}
}

func (s *Store) importUsage(artifact string, p ProcessObservation) error {
	if filepath.Ext(artifact) != ".jsonl" {
		return nil
	}
	var exists int
	if err := s.db.QueryRow("SELECT count(*) FROM usage_reports WHERE artifact=? AND started=?", artifact, p.Started).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	var invocation struct {
		Agent string `json:"agent"`
		Model string `json:"model"`
	}
	if readJSON(filepath.Join(filepath.Dir(artifact), "invocation.json"), &invocation) != nil {
		return nil
	}
	if !strings.Contains("|codex|claude|zcode|", "|"+invocation.Agent+"|") {
		return nil
	}
	raw, err := readBounded(artifact, 64<<20)
	if err != nil {
		return nil
	}
	return s.saveUsage(artifact, p, invocation.Agent, invocation.Model, "imported", raw)
}
