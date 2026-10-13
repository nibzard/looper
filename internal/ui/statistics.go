package ui

import (
	"fmt"
	"math"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// StatisticsView is a read-only projection of durable project records.
type StatisticsView struct {
	Version           int               `json:"schema_version"`
	UpdatedAt         string            `json:"updated_at"`
	FirstRecorded     string            `json:"first_recorded_at,omitempty"`
	ElapsedSeconds    *float64          `json:"elapsed_seconds"`
	ActiveSeconds     *float64          `json:"active_seconds"`
	WorkerSeconds     *float64          `json:"worker_seconds"`
	ControllerSeconds *float64          `json:"controller_seconds"`
	HistoryComplete   bool              `json:"history_complete"`
	UnknownEnds       int               `json:"unknown_ends"`
	TasksDone         int               `json:"tasks_done"`
	TasksTotal        int               `json:"tasks_total"`
	Integrations      int               `json:"integrations"`
	Attempts          int               `json:"attempts"`
	Repairs           int               `json:"repairs"`
	ChecksPassed      int               `json:"checks_passed"`
	ChecksFailed      int               `json:"checks_failed"`
	Sessions          int               `json:"sessions"`
	UsageReports      int               `json:"usage_reports"`
	AgentRuns         int               `json:"agent_runs"`
	PricedReports     int               `json:"priced_reports"`
	Tokens            int64             `json:"tokens"`
	Input             int64             `json:"input_tokens"`
	Output            int64             `json:"output_tokens"`
	CacheRead         int64             `json:"cache_read_tokens"`
	EstimatedUSD      *float64          `json:"estimated_usd"`
	ReportedUSD       *float64          `json:"reported_usd"`
	ReportedCostBasis string            `json:"reported_cost_basis,omitempty"`
	PricingDate       string            `json:"pricing_date,omitempty"`
	Models            []ModelStatistics `json:"models"`
	Days              []DailyStatistics `json:"days"`
}
type ModelStatistics struct {
	Model             string   `json:"model"`
	Input             int64    `json:"input_tokens"`
	Output            int64    `json:"output_tokens"`
	CacheRead         int64    `json:"cache_read_tokens"`
	CacheWrite        int64    `json:"cache_write_tokens"`
	EstimatedUSD      *float64 `json:"estimated_usd"`
	ReportedUSD       *float64 `json:"reported_usd"`
	ReportedCostBasis string   `json:"reported_cost_basis,omitempty"`
	Reports           int      `json:"reports"`
	Priced            int      `json:"priced_reports"`
}
type DailyStatistics struct {
	Date          string   `json:"date"`
	WorkerSeconds float64  `json:"worker_seconds"`
	Integrations  int      `json:"integrations"`
	Tokens        int64    `json:"tokens"`
	Reports       int      `json:"usage_reports"`
	EstimatedUSD  *float64 `json:"estimated_usd"`
}

func statDuration(seconds *float64) string {
	if seconds == nil {
		return "unknown"
	}
	d := time.Duration(*seconds * float64(time.Second)).Round(time.Second)
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd %dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
	}
	if d >= time.Hour {
		return fmt.Sprintf("%dh %dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm %ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return d.String()
}
func statCount(n int64) string {
	if n >= 1e9 {
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	}
	if n >= 1e6 {
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	}
	if n >= 1e3 {
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
func statUSD(value *float64) string {
	if value == nil {
		return "unknown"
	}
	if *value > 0 && *value < 0.01 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", *value)
}

func (m model) statisticsText() string {
	if m.statistics == nil {
		if m.statisticsError != "" {
			return "Statistics unavailable: " + plainText(m.statisticsError)
		}
		return "Collecting project statistics...\nOlder controllers may not have timing or usage records."
	}
	s := m.statistics
	lines := []string{
		titleStyle.Render("Project effort"),
		fmt.Sprintf("Elapsed %s   Active %s", statDuration(s.ElapsedSeconds), statDuration(s.ActiveSeconds)),
		fmt.Sprintf("Workers %s   Controller %s", statDuration(s.WorkerSeconds), statDuration(s.ControllerSeconds)),
		fmt.Sprintf("%d/%d tasks complete   %d integrations", s.TasksDone, s.TasksTotal, s.Integrations),
		fmt.Sprintf("%d attempts   %d repairs   %d sessions", s.Attempts, s.Repairs, s.Sessions),
		"Checks: " + okStyle.Render(fmt.Sprintf("%d passed", s.ChecksPassed)) + " / " + checkFailureText(s.ChecksFailed),
	}
	if m.statisticsError != "" {
		lines = append([]string{errorStyle.Render("Last refresh failed: " + plainText(m.statisticsError))}, lines...)
	}
	if !s.HistoryComplete {
		lines = append(lines, blockedStyle.Render(fmt.Sprintf("Partial history: older data / %d unknown ends", s.UnknownEnds)))
	}
	lines = append(lines, "", titleStyle.Render("Tokens and API cost"))
	if s.UsageReports == 0 {
		lines = append(lines, "Token usage unknown: no agent usage reports yet.")
	} else {
		lines = append(lines, fmt.Sprintf("%s tokens   In %s / Out %s", statCount(s.Tokens), statCount(s.Input), statCount(s.Output)))
		cache := "unknown"
		if s.Input > 0 {
			cache = fmt.Sprintf("%.0f%%", float64(s.CacheRead)*100/float64(s.Input))
		}
		lines = append(lines, fmt.Sprintf("Cache reads %s (%s of input)", statCount(s.CacheRead), cache))
		lines = append(lines, fmt.Sprintf("API estimate %s   %d/%d reports priced", statUSD(s.EstimatedUSD), s.PricedReports, s.UsageReports))
		if s.ReportedUSD != nil {
			lines = append(lines, "Agent-reported estimate "+statUSD(s.ReportedUSD))
			basis := s.ReportedCostBasis
			if basis == "" {
				basis = "unknown"
			}
			lines = append(lines, "Reported cost basis: "+plainText(basis))
		}
	}
	lines = append(lines, fmt.Sprintf("Usage coverage: %d reports / %d agent runs", s.UsageReports, s.AgentRuns))
	lines = append(lines, dimStyle.Render("API estimates are not invoices or subscription charges."))
	if s.PricingDate != "" {
		lines = append(lines, dimStyle.Render("Prices: Pydantic genai-prices · "+s.PricingDate))
	}

	chartWidth := m.width
	wide := m.width >= 100
	leftWidth := (m.width - 3) * 45 / 100
	if wide {
		chartWidth = m.width - leftWidth - 3
	}
	chart := statisticsChart(s.Days, chartWidth, m.height)
	if wide {
		summary := lipgloss.NewStyle().Width(leftWidth).Render(strings.Join(lines, "\n"))
		chart = lipgloss.NewStyle().Width(chartWidth).Render(chart)
		lines = []string{lipgloss.JoinHorizontal(lipgloss.Top, summary, "   ", chart)}
	} else {
		lines = append(lines, "", chart)
	}

	if len(s.Models) > 0 {
		lines = append(lines, "", titleStyle.Render("Models · recorded token usage"))
		for _, u := range s.Models {
			lines = append(lines, fmt.Sprintf("%s  %s tokens  %s API", plainText(u.Model), statCount(u.Input+u.Output), statUSD(u.EstimatedUSD)))
			lines = append(lines, dimStyle.Render(fmt.Sprintf("  In %s / Out %s / Cached %s · %d/%d priced", statCount(u.Input), statCount(u.Output), statCount(u.CacheRead), u.Priced, u.Reports)))
		}
	}
	lines = append(lines, "", dimStyle.Render("Active time merges overlapping processes. Workers sum their durations."))
	lines = append(lines, dimStyle.Render("Token totals come from reported usage, never from text length."))
	lines = append(lines, dimStyle.Render("Updated "+s.UpdatedAt))
	return strings.Join(lines, "\n")
}

func statisticsChart(days []DailyStatistics, width, height int) string {
	if height < 32 && len(days) > 7 {
		days = days[len(days)-7:]
	}
	lines := []string{titleStyle.Render(fmt.Sprintf("Daily effort · last %d days · UTC", len(days)))}
	emptyDays := 0
	for emptyDays < len(days)-1 {
		day := days[emptyDays]
		if day.WorkerSeconds != 0 || day.Integrations != 0 || day.Reports != 0 || day.Tokens != 0 || day.EstimatedUSD != nil {
			break
		}
		emptyDays++
	}
	if emptyDays > 0 {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("%d earlier days: no recorded effort or usage", emptyDays)))
		days = days[emptyDays:]
	}
	maxWork := 0.0
	for _, day := range days {
		maxWork = math.Max(maxWork, day.WorkerSeconds)
	}
	narrow := width < 45
	barWidth := max(3, min(20, width-37))
	if narrow {
		barWidth = max(3, min(12, width-23))
	} else {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("%-5s %-*s %6s %4s %6s %7s", "Date", barWidth, "Effort", "Hours", "Done", "Tokens", "API USD")))
	}
	for _, day := range days {
		bar := ""
		if maxWork > 0 && day.WorkerSeconds > 0 {
			bar = strings.Repeat("█", max(1, int(math.Round(day.WorkerSeconds/maxWork*float64(barWidth)))))
		}
		date := day.Date
		if len(date) > 5 {
			date = date[5:]
		}
		if narrow {
			lines = append(lines, fmt.Sprintf("%s %-*s %5.1fh", date, barWidth, bar, day.WorkerSeconds/3600))
		} else {
			tokens := "?"
			if day.Reports > 0 {
				tokens = statCount(day.Tokens)
			}
			cost := "?"
			if day.EstimatedUSD != nil {
				cost = statUSD(day.EstimatedUSD)
			}
			lines = append(lines, fmt.Sprintf("%s %s %5.1fh %4d %6s %7s", date, busyStyle.Render(fmt.Sprintf("%-*s", barWidth, bar)), day.WorkerSeconds/3600, day.Integrations, tokens, cost))
		}
	}
	lines = append(lines, dimStyle.Render("Bars: combined worker hours. ? = unknown."))
	lines = append(lines, dimStyle.Render("Tokens and USD follow the report's day."))
	return strings.Join(lines, "\n")
}

func (m model) statisticsScreen() string {
	m.tailView.SetContent(lipgloss.NewStyle().Width(max(1, m.width)).Render(m.statisticsText()))
	return m.workspaceViewportScreen(m.tailView, "Statistics · project totals · refreshes every 5s")
}

type statisticsMsg struct {
	view StatisticsView
	err  error
}

func (m model) loadStatistics() tea.Cmd {
	if m.ctrl.Statistics == nil {
		return nil
	}
	return func() tea.Msg {
		view, err := m.ctrl.Statistics()
		return statisticsMsg{view: view, err: err}
	}
}

func (m *model) refreshStatistics() {
	m.tailView.SetContent(lipgloss.NewStyle().Width(max(1, m.width)).Render(m.statisticsText()))
}

func checkFailureText(n int) string {
	text := fmt.Sprintf("%d failed", n)
	if n > 0 {
		return errorStyle.Render(text)
	}
	return dimStyle.Render(text)
}
