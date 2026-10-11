package ui

import (
	"fmt"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"strings"
	"testing"
	"time"
)

func TestDashboardBeforeFirstSnapshot(t *testing.T) {
	m := model{width: 80, height: 24}
	view := m.View()
	if !strings.Contains(view, "Waiting for the controller") {
		t.Fatalf("missing waiting state: %s", view)
	}
	// The same model must render the first snapshot when it arrives.
	m.snap = &ViewSnapshot{}
	_ = m.View()
}

func TestPlanningOnPhoneTerminal(t *testing.T) {
	for _, size := range [][2]int{{58, 22}, {40, 18}, {30, 14}, {80, 24}} {
		m := model{width: size[0], height: size[1], snap: &ViewSnapshot{
			State: "planning", ActivityDetail: "claude attempt 1/3",
			Started:  time.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339),
			Now:      time.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339),
			Counters: ViewCounters{Done: 55, Total: 63, Ready: 8},
			Ready:    []TaskView{{ID: "T050", Title: "Compose an optional tmux writer"}},
		}}
		view := m.View()
		for _, want := range []string{"PLANNING", "55/63 complete", "q exit", "[L] logs"} {
			if !strings.Contains(view, want) {
				t.Fatalf("%v: missing %q: %s", size, want, view)
			}
		}
		if lipgloss.Width(view) > size[0] || lipgloss.Height(view) > size[1] {
			t.Fatalf("%v: overflow %dx%d", size, lipgloss.Width(view), lipgloss.Height(view))
		}
		if m.elapsed() == "0s" {
			t.Fatal("live timer froze on snapshot time")
		}
		m.staleView = true
		if m.elapsed() != "0s" {
			t.Fatal("stale timer keeps moving")
		}
	}
}

func TestMissingSnapshotMarksObserverStale(t *testing.T) {
	m := model{snap: &ViewSnapshot{State: "planning"}, ctrl: Controls{Poll: func() *ViewSnapshot { return nil }}}
	next, _ := m.Update(tickMsg{})
	label, _ := StatusLabel(next.(model).snap, next.(model).staleView)
	if label != "STOPPED" {
		t.Fatalf("stale observer: %s", label)
	}
}

func TestPlannerLogFollowsRetry(t *testing.T) {
	m := model{width: 40, height: 18, snap: &ViewSnapshot{ActivityLog: "plan/events-00.jsonl"}, tailView: viewport.New(40, 8)}
	m.ctrl.Log = func(path string) (string, error) { return path, nil }
	next, _ := m.keys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = next.(model)
	if m.mode != ModeLog || !strings.Contains(m.logText, "events-00") {
		t.Fatal("log did not open")
	}
	m.snap.ActivityLog = "plan/events-01.jsonl"
	next, _ = m.Update(tickMsg{})
	m = next.(model)
	if !strings.Contains(m.logText, "events-01") {
		t.Fatal("log did not follow retry")
	}
}

func TestPhoneBoardScrollAndResize(t *testing.T) {
	m := model{width: 40, height: 16, snap: &ViewSnapshot{State: "planning", ActivityDetail: "claude attempt 1/3", Counters: ViewCounters{Ready: 12}}}
	for i := 0; i < 12; i++ {
		m.snap.Ready = append(m.snap.Ready, TaskView{ID: fmt.Sprintf("T%03d", i), Title: "A long task title that must remain accessible"})
	}
	for i := 0; i < 100; i++ {
		next, _ := m.keys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		m = next.(model)
	}
	view := m.View()
	if !strings.Contains(view, "PLANNING") || !strings.Contains(view, "Events") || !strings.Contains(view, "q exit") {
		t.Fatalf("missing fixed status or scroll content: %s", view)
	}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 30, Height: 12})
	m = next.(model)
	view = m.View()
	if lipgloss.Width(view) > 30 || lipgloss.Height(view) > 12 {
		t.Fatalf("resize overflow: %s", view)
	}
	for i := 0; i < 100; i++ {
		next, _ := m.keys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
		m = next.(model)
	}
	if m.scroll != 0 {
		t.Fatal("cannot return to top")
	}
}

func TestPhoneLogLeavesRoomForOutput(t *testing.T) {
	m := model{width: 30, height: 12, mode: ModeLog, snap: &ViewSnapshot{State: "planning"}, logPath: "/home/agent/.looper/project-123/runtime/plan-123/events-00.jsonl", tailView: viewport.New(30, 8)}
	m.ctrl.Log = func(string) (string, error) { return "Planner is inspecting the project", nil }
	m.refreshLog()
	view := m.View()
	if !strings.Contains(view, "Planner is inspecting") || !strings.Contains(view, "events-00.jsonl") {
		t.Fatalf("log path hid output: %s", view)
	}
	if lipgloss.Height(view) > 12 || lipgloss.Width(view) > 30 {
		t.Fatalf("log overflow: %s", view)
	}
}

func TestPhoneDialogsFit(t *testing.T) {
	for _, dialog := range []string{"exit", "drain", "add-plan"} {
		m := model{width: 30, height: 12, dialog: dialog, planPath: strings.Repeat("long-path/", 20)}
		view := m.View()
		if lipgloss.Width(view) > 30 || lipgloss.Height(view) > 12 {
			t.Fatalf("%s overflows: %s", dialog, view)
		}
	}
}

func TestRunStateAndActivity(t *testing.T) {
	cases := []struct {
		name                   string
		v                      *ViewSnapshot
		stale                  bool
		state, activity, color string
	}{
		{"connecting", nil, false, "CONNECTING", "", "11"},
		{"planning", &ViewSnapshot{State: "planning"}, false, "RUNNING", "PLANNING", "10"},
		{"reviewing", &ViewSnapshot{State: "reviewing", Finished: true}, false, "RUNNING", "REVIEWING", "10"},
		{"implementing", &ViewSnapshot{Counters: ViewCounters{InFlight: 1}}, false, "RUNNING", "IMPLEMENTING", "10"},
		{"checking", &ViewSnapshot{Counters: ViewCounters{Checking: 1}}, false, "RUNNING", "CHECKING", "10"},
		{"waiting", &ViewSnapshot{}, false, "WAITING", "No ready tasks", "11"},
		{"blocked", &ViewSnapshot{Counters: ViewCounters{Blocked: 1}}, false, "BLOCKED", "Needs attention", "9"},
		{"complete", &ViewSnapshot{Finished: true}, false, "COMPLETE", "Waiting for new work", "10"},
		{"stopping", &ViewSnapshot{State: "planning", Draining: true}, false, "STOPPING", "Finishing active work", "11"},
		{"stopped", &ViewSnapshot{State: "planning"}, true, "STOPPED", "", "8"},
		{"stale snapshot", &ViewSnapshot{State: "planning", Stale: true}, false, "STOPPED", "", "8"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			state, style := StatusLabel(tt.v, tt.stale)
			if state != tt.state || activityLabel(tt.v, tt.stale) != tt.activity || style.GetForeground() != lipgloss.Color(tt.color) {
				t.Fatalf("unexpected state %s / %s / %v", state, activityLabel(tt.v, tt.stale), style.GetForeground())
			}
		})
	}
}

func TestPhonePlannerOutputAge(t *testing.T) {
	m := model{width: 30, height: 12, snap: &ViewSnapshot{State: "planning", ActivityLog: "plan/events-00.jsonl", ActivityDetail: "Read Cargo.toml", ActivityUpdated: time.Now().Add(-20 * time.Second).UTC().Format(time.RFC3339)}}
	view := m.View()
	if !strings.Contains(view, "Last output") || !strings.Contains(view, "Read Cargo.toml") {
		t.Fatalf("missing observed activity: %s", view)
	}
	if lipgloss.Width(view) > 30 || lipgloss.Height(view) > 12 {
		t.Fatalf("activity overflow: %s", view)
	}
	m.snap.ActivityUpdated = ""
	if !strings.Contains(m.View(), "No agent output yet") {
		t.Fatal("invented activity before output")
	}
}
