package looper

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Pricing data is a pinned MIT-licensed snapshot from pydantic/genai-prices.
// No network requests occur during a run or while rendering statistics.
//
//go:embed prices/data.json
var priceData []byte

//go:embed prices/source.json
var priceSource []byte

var catalogOnce sync.Once
var catalog []priceProvider

type priceProvider struct {
	ID     string       `json:"id"`
	Models []priceModel `json:"models"`
}
type priceModel struct {
	ID     string          `json:"id"`
	Match  priceMatch      `json:"match"`
	Prices json.RawMessage `json:"prices"`
}
type priceMatch struct {
	Equals     string       `json:"equals"`
	StartsWith string       `json:"starts_with"`
	EndsWith   string       `json:"ends_with"`
	Contains   string       `json:"contains"`
	Regex      string       `json:"regex"`
	Or         []priceMatch `json:"or"`
	And        []priceMatch `json:"and"`
}

func (m priceMatch) matches(value string) bool {
	if m.Equals != "" {
		return value == m.Equals
	}
	if m.StartsWith != "" {
		return strings.HasPrefix(value, m.StartsWith)
	}
	if m.EndsWith != "" {
		return strings.HasSuffix(value, m.EndsWith)
	}
	if m.Contains != "" {
		return strings.Contains(value, m.Contains)
	}
	if m.Regex != "" {
		matched, err := regexp.MatchString(m.Regex, value)
		return err == nil && matched
	}
	if len(m.Or) > 0 {
		for _, match := range m.Or {
			if match.matches(value) {
				return true
			}
		}
		return false
	}
	if len(m.And) > 0 {
		for _, match := range m.And {
			if !match.matches(value) {
				return false
			}
		}
		return true
	}
	return false
}

type priceCondition struct {
	Constraint struct {
		StartDate string `json:"start_date"`
		StartTime string `json:"start_time"`
		EndTime   string `json:"end_time"`
	} `json:"constraint"`
	Prices json.RawMessage `json:"prices"`
}

func applicablePrices(raw json.RawMessage, at time.Time) map[string]json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '[' {
		var conditions []priceCondition
		if json.Unmarshal(raw, &conditions) != nil {
			return nil
		}
		var selected json.RawMessage
		for _, c := range conditions {
			if c.Constraint.StartDate != "" && at.UTC().Format("2006-01-02") < c.Constraint.StartDate {
				continue
			}
			if c.Constraint.StartTime != "" {
				clock := at.UTC().Format("15:04:05")
				start, end := c.Constraint.StartTime, c.Constraint.EndTime
				if start <= end {
					if clock < start || clock >= end {
						continue
					}
				} else if clock < start && clock >= end {
					continue
				}
			}
			selected = c.Prices
		}
		raw = selected
	}
	var rates map[string]json.RawMessage
	if json.Unmarshal(raw, &rates) != nil {
		return nil
	}
	return rates
}

// Session totals cannot identify per-request context tiers. Do not infer a
// long-context price from cumulative input tokens. Such reports stay unpriced.
func estimateUsage(u UsageRecord, at time.Time) (*float64, string) {
	catalogOnce.Do(func() { _ = json.Unmarshal(priceData, &catalog) })
	model := strings.TrimSuffix(u.Model, "[1m]")
	provider := ""
	switch {
	case strings.HasPrefix(strings.ToLower(model), "glm-"):
		provider = "zai"
	case strings.HasPrefix(model, "claude-"):
		provider = "anthropic"
	case strings.HasPrefix(model, "gpt-") || regexp.MustCompile(`^o[134]`).MatchString(model):
		provider = "openai"
	}
	for _, p := range catalog {
		if p.ID != provider {
			continue
		}
		var selected *priceModel
		for i := range p.Models {
			m := &p.Models[i]
			if m.ID == model {
				selected = m
				break
			}
			if m.Match.matches(model) && (selected == nil || len(m.ID) > len(selected.ID)) {
				selected = m
			}
		}
		if selected != nil {
			m := *selected
			if m.ID != model && !m.Match.matches(model) {
				continue
			}
			rates := applicablePrices(m.Prices, at)
			if len(rates) == 0 {
				return nil, provider
			}
			total := 0.0
			counts := map[string]int64{
				"input_mtok":          u.Input - u.CacheRead - u.CacheWrite,
				"cache_read_mtok":     u.CacheRead,
				"cache_write_mtok":    u.CacheWrite - u.CacheWrite1h,
				"cache_write_1h_mtok": u.CacheWrite1h,
				"output_mtok":         u.Output,
			}
			for unit, count := range counts {
				if count < 0 {
					return nil, provider
				}
				if count == 0 {
					continue
				}
				raw, ok := rates[unit]
				if !ok {
					if unit == "cache_write_1h_mtok" {
						raw, ok = rates["cache_write_mtok"]
					}
					if !ok {
						return nil, provider
					}
				}
				var rate float64
				if json.Unmarshal(raw, &rate) != nil || rate < 0 {
					return nil, provider
				}
				total += float64(count) * rate / 1e6
			}
			if u.WebSearches > 0 {
				var rate float64
				if json.Unmarshal(rates["web_searches_kcount"], &rate) != nil {
					return nil, provider
				}
				total += float64(u.WebSearches) * rate / 1e3
			}
			return &total, provider
		}
	}
	return nil, provider
}
