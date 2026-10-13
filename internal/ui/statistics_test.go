package ui

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func statisticsFixture() model {
	m := workspaceFixture()
	number := func(v float64) *float64 { return &v }
	m.statistics = &StatisticsView{Version: 1, UpdatedAt: "2026-10-12T12:00:00Z", ElapsedSeconds: number(172800), ActiveSeconds: number(36000), WorkerSeconds: number(72000), ControllerSeconds: number(54000), HistoryComplete: false, UnknownEnds: 2,
		TasksDone: 61, TasksTotal: 83, Integrations: 12, Attempts: 15, Repairs: 3, ChecksPassed: 19, ChecksFailed: 2, Sessions: 6, UsageReports: 14, AgentRuns: 16, PricedReports: 12,
		Tokens: 4_400_000, Input: 4_000_000, Output: 400_000, CacheRead: 3_000_000, EstimatedUSD: number(21.35), ReportedUSD: number(23), PricingDate: "2026-10-12",
		Models: []ModelStatistics{{Model: "glm-5.3[1m]", Input: 4_000_000, Output: 400_000, CacheRead: 3_000_000, Reports: 14, Priced: 12, EstimatedUSD: number(21.35)}}}
	for i := 0; i < 14; i++ {
		m.statistics.Days = append(m.statistics.Days, DailyStatistics{Date: fmtDate(i), WorkerSeconds: float64((i % 4) * 3600), Integrations: i % 3, Tokens: int64(i * 10000), Reports: 1, EstimatedUSD: number(float64(i) / 10)})
	}
	m.mode = ModeStatistics
	m.refreshStatistics()
	m.sizeViewports()
	return m
}
func fmtDate(i int) string {
	return fmt.Sprintf("2026-10-%02d", i+1)
}

func TestStatisticsScreenFitsAndShowsEffort(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {100, 24}, {80, 24}, {50, 16}, {30, 12}} {
		m := statisticsFixture()
		m.width, m.height = size[0], size[1]
		m.sizeViewports()
		m.refreshStatistics()
		view := ansi.Strip(m.View())
		if lipgloss.Height(view) != size[1] {
			t.Fatalf("height at %v: %d", size, lipgloss.Height(view))
		}
		for _, line := range strings.Split(view, "\n") {
			if lipgloss.Width(line) > size[0] {
				t.Fatalf("overflow at %v: %q", size, line)
			}
		}
		if !strings.Contains(view, "Statistics") || !strings.Contains(view, "Elapsed") {
			t.Fatalf("missing orientation: %s", view)
		}
		if size[0] >= 100 && (!strings.Contains(view, "Daily effort") || !strings.Contains(view, "10-14") || !strings.Contains(view, "█")) {
			t.Fatalf("daily chart hidden at %v:\n%s", size, view)
		}
		if size[0] >= 100 && !strings.Contains(view, "$21.35") {
			t.Fatalf("cost hidden at %v", size)
		}
		if size[0] == 100 && os.Getenv("LOOPER_RENDER_STATISTICS") != "" {
			if err := os.WriteFile(os.Getenv("LOOPER_RENDER_STATISTICS"), []byte(view), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestStatisticsUnknownIsNotZero(t *testing.T) {
	m := workspaceFixture()
	m.mode = ModeStatistics
	m.statistics = &StatisticsView{Days: []DailyStatistics{{Date: "2026-10-12"}}}
	text := ansi.Strip(m.statisticsText())
	for _, want := range []string{"Elapsed unknown", "Workers unknown", "Token usage unknown", "Partial history", "? = unknown"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "$0.00") {
		t.Fatal("missing usage displayed as free work")
	}
}

func TestStatisticsNavigationAndPolling(t *testing.T) {
	m := statisticsFixture()
	m.mode = ModeBoard
	calls := 0
	m.ctrl.Statistics = func() (StatisticsView, error) { calls++; return *m.statistics, nil }
	m, cmd := press(m, "p")
	if m.mode != ModeStatistics || cmd == nil || !m.statisticsLoading {
		t.Fatal("statistics shortcut did not request a snapshot")
	}
	next, _ := m.Update(cmd())
	m = next.(model)
	if calls != 1 || m.statisticsLoading {
		t.Fatal("statistics request did not complete")
	}
	before := m.tailView.YOffset
	m, _ = press(m, "j")
	if m.tailView.YOffset <= before {
		t.Fatal("statistics does not scroll")
	}
	offset := m.tailView.YOffset
	updated, _ := m.Update(statisticsMsg{view: *m.statistics})
	m = updated.(model)
	if m.tailView.YOffset != offset {
		t.Fatal("live refresh reset statistics scrolling")
	}
	m, _ = press(m, "b")
	if m.mode != ModeBoard {
		t.Fatal("cannot return to overview")
	}
	m, _ = press(m, "tab")
	m, _ = press(m, "tab")
	m, _ = press(m, "tab")
	if m.mode != ModeStatistics {
		t.Fatal("tab cycle does not include statistics")
	}
}

func TestStatisticsReadOnlyAndRefreshFailure(t *testing.T) {
	m := statisticsFixture()
	m.ctrl.ReadOnly = true
	m, cmd := press(m, "p")
	if m.mode != ModeStatistics || cmd != nil {
		t.Fatal("read-only statistics cannot open without controls")
	}
	before := m.statistics
	next, _ := m.Update(statisticsMsg{err: errors.New("database unavailable")})
	m = next.(model)
	if m.statistics != before || !strings.Contains(m.statisticsText(), "Last refresh failed") {
		t.Fatal("refresh failure discarded the last statistics")
	}
	m.statistics = nil
	m.statisticsError = ""
	if !strings.Contains(m.statisticsText(), "Collecting") {
		t.Fatal("missing data is not explained")
	}
}

func TestStatisticsRefreshUsesAsyncCommand(t *testing.T) {
	m := statisticsFixture()
	m.ctrl.Statistics = func() (StatisticsView, error) {
		t.Fatal("database queried inside Update")
		return StatisticsView{}, nil
	}
	next, cmd := m.Update(tickMsg{})
	m = next.(model)
	if cmd == nil || !m.statisticsLoading {
		t.Fatal("tick did not schedule a statistics read")
	}
	// Existing request stays in flight; a second tick must not start another.
	next, _ = m.Update(tickMsg{})
	m = next.(model)
	if !m.statisticsLoading {
		t.Fatal("refresh overlap state was lost")
	}
}

func TestDailyChartColumnAlignment(t *testing.T) {
	days := []DailyStatistics{{Date: "2026-10-13", WorkerSeconds: 7200, Integrations: 5, Reports: 1, Tokens: 15200000}}
	lines := strings.Split(ansi.Strip(statisticsChart(days, 72, 40)), "\n")
	if strings.Index(lines[1], "Done") != lipgloss.Width(lines[2][:strings.Index(lines[2], "5 ")])-3 {
		t.Fatalf("daily columns must share widths:\n%s", strings.Join(lines, "\n"))
	}
}

func TestDailyChartSummarizesEmptyLeadIn(t *testing.T) {
	days := []DailyStatistics{{Date: "2026-10-11"}, {Date: "2026-10-12"}, {Date: "2026-10-13", WorkerSeconds: 7200, Reports: 1}}
	text := ansi.Strip(statisticsChart(days, 72, 40))
	if !strings.Contains(text, "2 earlier days: no recorded effort or usage") || strings.Contains(text, "10-11") || !strings.Contains(text, "10-13") {
		t.Fatalf("empty days obscure recorded work: %s", text)
	}
}
