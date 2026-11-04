package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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

func TestSettingsOpensOnFirstKeypress(t *testing.T) {
	for _, key := range []string{"s", "S"} {
		for _, local := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/local=%t", key, local), func(t *testing.T) {
				m := model{width: 80, height: 24}
				if local {
					m.ctrl.Snapshot = make(chan ViewSnapshot)
				}
				before := m.View()
				next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
				m = next.(model)
				if m.dialog != "settings" {
					t.Fatalf("settings action missing: %q", m.dialog)
				}
				if cmd == nil {
					t.Fatal("first keypress did not hand control to the settings form")
				}
				if _, ok := cmd().(tea.QuitMsg); !ok {
					t.Fatal("settings did not exit the dashboard for the form")
				}
				if m.View() != before {
					t.Fatal("settings handoff replaced the dashboard before the form opened")
				}
			})
		}
	}
}

func TestSettingsRejectsReadOnlyKeypress(t *testing.T) {
	m := model{width: 80, height: 24, ctrl: Controls{ReadOnly: true}}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = next.(model)
	if cmd != nil || m.dialog != "" || m.err != "Read-only observers cannot change settings." {
		t.Fatalf("read-only settings action: dialog=%q error=%q cmd=%v", m.dialog, m.err, cmd)
	}
}

func TestDashboardOtherKeysKeepBehavior(t *testing.T) {
	for _, tt := range []struct {
		key, dialog string
		mode        int
	}{
		{"a", "add-plan", ModeBoard},
		{"x", "drain", ModeBoard},
		{"q", "exit", ModeBoard},
		{"?", "", ModeHelp},
		{"t", "", ModeTail},
		{"z", "", ModeBoard},
	} {
		t.Run(tt.key, func(t *testing.T) {
			m := model{ctrl: Controls{Snapshot: make(chan ViewSnapshot)}}
			next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tt.key)})
			m = next.(model)
			if cmd != nil || m.dialog != tt.dialog || m.mode != tt.mode {
				t.Fatalf("key %q: dialog=%q mode=%d cmd=%v", tt.key, m.dialog, m.mode, cmd)
			}
		})
	}
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
		for _, want := range []string{"PLANNING", "55/63 complete", "[q] Detach", "[l] Logs"} {
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
	if !strings.Contains(view, "PLANNING") || !strings.Contains(view, "Recent activity") || !strings.Contains(view, "[q] Detach") {
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
		{"loading", nil, false, "LOADING", "", "11"},
		{"bootstrapping", &ViewSnapshot{Bootstrapping: true}, false, "BOOTSTRAPPING", "Creating first tasks", "12"},
		{"planning", &ViewSnapshot{State: "planning"}, false, "RUNNING", "PLANNING", "10"},
		{"reviewing", &ViewSnapshot{State: "reviewing", Finished: true}, false, "RUNNING", "REVIEWING", "10"},
		{"implementing", &ViewSnapshot{Counters: ViewCounters{InFlight: 1}}, false, "RUNNING", "IMPLEMENTING", "10"},
		{"checking", &ViewSnapshot{Counters: ViewCounters{Checking: 1}}, false, "RUNNING", "CHECKING", "10"},
		{"waiting", &ViewSnapshot{}, false, "WAITING", "No ready tasks", "11"},
		{"input wait with ready work", &ViewSnapshot{State: "waiting", Counters: ViewCounters{Ready: 1}}, false, "WAITING", "Correct task file", "11"},
		{"input wait after completion", &ViewSnapshot{State: "waiting", Finished: true}, false, "WAITING", "Correct task file", "11"},
		{"drain during input wait", &ViewSnapshot{State: "waiting", Draining: true}, false, "STOPPING", "Finishing active work", "11"},
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

func TestBootstrapBoardShowsStreamEvidence(t *testing.T) {
	base := ViewSnapshot{
		Bootstrapping: true, Started: time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339),
		ActivityLog: "runtime/bootstrap-1/events-00.jsonl", BootstrapBytes: 24576, BootstrapIdleSeconds: 3,
	}
	m := model{width: 80, height: 24, snap: &base}
	m.spin = spinner.New(spinner.WithSpinner(spinner.Dot))
	view := m.View()
	for _, want := range []string{"BOOTSTRAPPING", "Bootstrapping", "stream 24.0 KB", "last output 3s ago", "[l] open the agent log"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q: %s", want, view)
		}
	}
	stalled := base
	stalled.BootstrapIdleSeconds = 250
	m.snap = &stalled
	view = m.View()
	if !strings.Contains(view, "no agent output for 4m10s") || !strings.Contains(view, "no output for 4m10s") {
		t.Fatalf("stalled stream must replace the steady line: %s", view)
	}
	if m.spinActive() {
		t.Fatal("a stalled stream must freeze the spinner")
	}
	// A dead controller stops all bootstrap rendering even with old evidence.
	m.staleView = true
	view = m.View()
	if strings.Contains(view, "Bootstrapping") || strings.Contains(view, "STOPPED") == false {
		t.Fatalf("stale view must report the stopped controller: %s", view)
	}
}

func TestBootstrapSpinnerFollowsStreamGrowth(t *testing.T) {
	current := &ViewSnapshot{Bootstrapping: true, BootstrapBytes: 100, BootstrapIdleSeconds: 1}
	m := model{ctrl: Controls{Poll: func() *ViewSnapshot { return current }}, width: 80, height: 24}
	m.spin = spinner.New(spinner.WithSpinner(spinner.Dot))
	next, _ := m.Update(tickMsg{})
	m = next.(model)
	if !m.spinActive() {
		t.Fatal("a growing stream must animate the spinner")
	}
	current.BootstrapBytes, current.BootstrapIdleSeconds = 100, 4
	next, _ = m.Update(tickMsg{})
	m = next.(model)
	if !m.spinActive() {
		t.Fatal("the grace period must bridge sparse writes")
	}
	current.BootstrapIdleSeconds = 60
	next, _ = m.Update(tickMsg{})
	m = next.(model)
	if m.spinActive() {
		t.Fatal("an idle stream must freeze the spinner")
	}
	current.BootstrapBytes = 5000
	next, _ = m.Update(tickMsg{})
	m = next.(model)
	if !m.spinActive() {
		t.Fatal("resumed writes must restart the spinner")
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

func TestWorkerLogUsesSelectedAttempt(t *testing.T) {
	m := model{width: 40, height: 18, snap: &ViewSnapshot{ActivityLog: "old-plan/events-00.jsonl", Attempts: []AttemptView{{ID: "task-a", TaskID: "T050"}, {ID: "task-b", TaskID: "T051"}}}, selected: "task-b", tailView: viewport.New(40, 8)}
	m.ctrl.Log = func(string) (string, error) { t.Fatal("opened planner log during implementation"); return "", nil }
	m.ctrl.WorkerLog = func(id string) (string, string, error) {
		if id != "task-b" {
			t.Fatalf("wrong task: %s", id)
		}
		return "work/events-01.jsonl", "Worker output", nil
	}
	next, _ := m.keys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'L'}})
	m = next.(model)
	if m.logAttempt != "task-b" || !strings.Contains(m.View(), "T051 worker log") || !strings.Contains(m.View(), "Worker output") {
		t.Fatalf("worker log missing: %s", m.View())
	}
	// Completing a different task must not switch the open log to a planner.
	m.snap.ActivityLog = "new-plan/events-00.jsonl"
	m.refreshLog()
	if m.logPath != "work/events-01.jsonl" {
		t.Fatal("planner replaced selected worker log")
	}
}

func TestCurrentRunEventsAndHistory(t *testing.T) {
	m := model{width: 50, height: 20, snap: &ViewSnapshot{Started: "2026-10-11T13:19:02Z"}, events: []JournalEvent{
		{Time: "2026-10-11T13:05:09Z", Kind: "progress", Message: "old run"},
		{Time: "2026-10-11T13:19:02Z", Kind: "resume", Message: "current run"},
	}}
	if strings.Contains(m.View(), "old run") || !strings.Contains(m.View(), "current run") {
		t.Fatalf("incorrect current events: %s", m.View())
	}
	m.mode = ModeTail
	next, _ := m.keys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = next.(model)
	if !strings.Contains(m.eventText(), "old run") || !strings.Contains(m.eventText(), "Recent history") {
		t.Fatal("history unavailable")
	}
	if eventStyle("resume").GetForeground() != lipgloss.Color("7") || eventStyle("done").GetForeground() != lipgloss.Color("10") || eventStyle("review-error").GetForeground() != lipgloss.Color("9") {
		t.Fatal("event colors do not convey outcome")
	}
}

func TestSelectedRowsStayVisibleAndResetStyles(t *testing.T) {
	m := model{width: 30, height: 14, snap: &ViewSnapshot{Counters: ViewCounters{InFlight: 4}}}
	for i := 0; i < 4; i++ {
		m.snap.Attempts = append(m.snap.Attempts, AttemptView{ID: fmt.Sprintf("task-%d", i), TaskID: fmt.Sprintf("T%03d", i), Phase: "running", Title: "Compose an optional tmux writing desk"})
	}
	for i := 0; i < 3; i++ {
		next, _ := m.keys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		m = next.(model)
		view := m.View()
		if !strings.Contains(view, fmt.Sprintf("› T%03d", i+1)) {
			t.Fatalf("selection scrolled away: %s", view)
		}
		if lipgloss.Width(view) > 30 || lipgloss.Height(view) > 14 {
			t.Fatalf("overflow: %s", view)
		}
		for _, line := range strings.Split(view, "\n") {
			if !strings.HasPrefix(line, "\x1b[0m") || !strings.HasSuffix(line, "\x1b[0m") {
				t.Fatal("style can leak across clipped rows")
			}
		}
	}
	// ANSI in an event must not control the color of the next line.
	m.events = []JournalEvent{{Message: "\x1b[42mgreen background\x1b[0m"}}
	if strings.Contains(m.eventTail(5), "\x1b[42m") {
		t.Fatal("event injected terminal styling")
	}
	m.snap.Attempts = m.snap.Attempts[:1]
	if m.selectedAttempt() != "task-0" {
		t.Fatal("selection did not recover after task removal")
	}
}

func TestCapitalShortcutsPreservePlanPathCase(t *testing.T) {
	m := model{width: 40, height: 18, tailView: viewport.New(40, 8)}
	next, _ := m.keys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}})
	m = next.(model)
	if m.mode != ModeTail {
		t.Fatal("capital T did not open events")
	}
	next, _ = m.keys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'B'}})
	m = next.(model)
	if m.mode != ModeBoard {
		t.Fatal("capital B did not return to board")
	}
	m.dialog = "add-plan"
	for _, key := range "MyPlan.md" {
		next, _ = m.keys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		m = next.(model)
	}
	if m.planPath != "MyPlan.md" {
		t.Fatalf("plan path case changed: %q", m.planPath)
	}
}

func TestBlockedTaskScrollHintsOnPhone(t *testing.T) {
	m := model{width: 44, height: 18, showDetails: true, snap: &ViewSnapshot{
		Blocked: []BlockedView{{ID: "T050", Title: "Add expiry cases", Reasons: []string{
			strings.Repeat("Expiry needs a configuration decision. ", 12),
		}}},
	}}
	top := m.View()
	for _, want := range []string{"Blocked · 1 task", "T050 Add expiry cases", "Reason:", "[j] More below"} {
		if !strings.Contains(top, want) {
			t.Fatalf("missing %q: %s", want, top)
		}
	}
	m.scroll = m.maxBoardScroll()
	bottom := m.View()
	if !strings.Contains(bottom, "[k] More above") || strings.Contains(bottom, "[j] More below") {
		t.Fatalf("incorrect end-of-content hint: %s", bottom)
	}
	for _, view := range []string{top, bottom} {
		if lipgloss.Width(view) > m.width || lipgloss.Height(view) > m.height {
			t.Fatalf("phone view overflows: %s", view)
		}
	}
}

// A bootstrapping snapshot must fit short terminals in both animation modes:
// fitBoard clips the bootstrap panel to the rows the footer leaves instead of
// pushing the controls off the bottom of the screen.
func TestBootstrapBoardFitsShortScreens(t *testing.T) {
	base := ViewSnapshot{
		Bootstrapping: true, Started: time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339),
		ActivityLog: "runtime/bootstrap-1/events-00.jsonl", BootstrapBytes: 24576, BootstrapIdleSeconds: 3,
	}
	for _, noAnim := range []bool{false, true} {
		for h := 8; h <= 16; h++ {
			m := model{width: 30, height: h, snap: &base, ctrl: Controls{NoAnimation: noAnim}}
			m.spin = spinner.New(spinner.WithSpinner(spinner.Dot))
			view := m.View()
			if lipgloss.Height(view) > h || lipgloss.Width(view) > 30 {
				t.Fatalf("no-animation=%v height=%d renders %dx%d:\n%s", noAnim, h, lipgloss.Width(view), lipgloss.Height(view), view)
			}
		}
	}
	// The reported 30x12 case: both modes stay inside 12 rows while the
	// status label, stream evidence, and controls remain readable.
	for _, noAnim := range []bool{false, true} {
		m := model{width: 30, height: 12, snap: &base, ctrl: Controls{NoAnimation: noAnim}}
		m.spin = spinner.New(spinner.WithSpinner(spinner.Dot))
		view := m.View()
		for _, want := range []string{"BOOTSTRAPPING", "stream 24.0 KB", "[q] Detach"} {
			if !strings.Contains(view, want) {
				t.Fatalf("no-animation=%v missing %q:\n%s", noAnim, want, view)
			}
		}
		// Scrolling through the clipped panel stays inside the screen too.
		m.scroll = m.maxBoardScroll()
		if scrolled := m.View(); lipgloss.Height(scrolled) > 12 || lipgloss.Width(scrolled) > 30 {
			t.Fatalf("no-animation=%v scrolled view overflows:\n%s", noAnim, scrolled)
		}
	}
	// Roomier screens keep the scroll hint that a cramped one gives up.
	m := model{width: 30, height: 14, snap: &base}
	m.spin = spinner.New(spinner.WithSpinner(spinner.Dot))
	if view := m.View(); !strings.Contains(view, "[j] More below") || lipgloss.Height(view) > 14 {
		t.Fatalf("hint lost on a screen that fits it:\n%s", view)
	}
}

func TestControllerViewReloadsAfterSettings(t *testing.T) {
	// No new channel notification is needed when a settings form closes.
	ch := make(chan ViewSnapshot)
	snapshot := &ViewSnapshot{Session: "current", Counters: ViewCounters{Done: 64, Total: 92}}
	m := model{ctrl: Controls{Snapshot: ch, Poll: func() *ViewSnapshot { return snapshot }}}
	if m.waitSnapshot() != nil {
		t.Fatal("polling view leaves a channel reader behind after settings")
	}
	next, _ := m.Update(tickMsg{})
	m = next.(model)
	if m.snap == nil || m.snap.Counters.Done != 64 || m.staleView {
		t.Fatal("controller snapshot did not reload")
	}
}

func TestCompactBoardPrioritizesTasksAndPreservesDetails(t *testing.T) {
	longTitle := "Audit every route for tenant and workspace isolation including public endpoints"
	longReason := strings.Repeat("Expiry needs a configuration decision. ", 12) + "END OF REASON"
	m := model{width: 58, height: 22, snap: &ViewSnapshot{
		State: "planning", ActivityDetail: "Read " + strings.Repeat("/runtime/worktrees/plan", 8) + "/fixture.ts",
		ActivityLog: "planner.log", ActivityUpdated: time.Now().Format(time.RFC3339),
		Counters: ViewCounters{Done: 65, Total: 93, Ready: 5, Blocked: 1},
		Blocked:  []BlockedView{{ID: "T050", Title: "Add review and expiry cases", Reasons: []string{longReason}}},
	}}
	for i := 0; i < 5; i++ {
		m.snap.Ready = append(m.snap.Ready, TaskView{ID: fmt.Sprintf("T%03d", 55+i), Title: longTitle})
		m.events = append(m.events, JournalEvent{Message: fmt.Sprintf("Activity %d", i)})
	}
	view := m.View()
	for _, want := range []string{"Blocked · 1 task", "Ready · 5 tasks", "T059", "Latest:", "Activity 4", "[v] Details", "[q] Detach"} {
		if !strings.Contains(view, want) {
			t.Fatalf("summary missing %q: %s", want, view)
		}
	}
	if m.maxBoardScroll() != 0 || lipgloss.Width(view) > 58 || lipgloss.Height(view) > 22 {
		t.Fatalf("summary does not fit: %s", view)
	}
	if strings.Contains(view, longTitle) || strings.Contains(view, "END OF REASON") || strings.Contains(view, "Activity 2") {
		t.Fatalf("summary contains expanded content: %s", view)
	}
	if strings.Index(view, "Blocked ·") > strings.Index(view, "Ready ·") || strings.Index(view, "Ready ·") > strings.Index(view, "Latest:") {
		t.Fatalf("tasks do not precede planner output: %s", view)
	}
	next, _ := m.keys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	m = next.(model)
	if m.maxBoardScroll() == 0 || !strings.Contains(m.boardContent(), "END OF REASON") || !strings.Contains(m.footer(), "[v] Summary") {
		t.Fatal("full details are not accessible")
	}
	m.scroll = m.maxBoardScroll()
	next, _ = m.keys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	m = next.(model)
	if m.scroll != 0 || m.showDetails {
		t.Fatal("summary toggle did not reset scrolling")
	}
	m.snap.Attempts = []AttemptView{{ID: "attempt", TaskID: "T001", Title: longTitle, Phase: "running"}}
	content := m.boardContent()
	if strings.Index(content, "Working") > strings.Index(content, "Blocked ·") {
		t.Fatal("active work must precede blocked tasks")
	}
	for _, size := range [][2]int{{40, 18}, {30, 14}, {100, 20}, {100, 40}, {58, 24}} {
		next, _ = m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m = next.(model)
		view = m.View()
		if lipgloss.Width(view) > size[0] || lipgloss.Height(view) > size[1] {
			t.Fatalf("%v: overflow: %s", size, view)
		}
		if size == [2]int{100, 40} && !strings.Contains(m.boardContent(), "END OF REASON") {
			t.Fatal("large window did not restore full details")
		}
	}
}

// Read-only observers keep every controlling action: drain, settings, and
// plan import all report a refusal instead of reaching the controller.
func TestReadOnlyRejectsControlKeys(t *testing.T) {
	for _, tt := range []struct{ key, want string }{
		{"x", "Read-only observers cannot drain work."},
		{"s", "Read-only observers cannot change settings."},
		{"a", "Read-only observers cannot import plans."},
	} {
		t.Run(tt.key, func(t *testing.T) {
			m := model{width: 80, height: 24, ctrl: Controls{ReadOnly: true}}
			next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tt.key)})
			m = next.(model)
			if cmd != nil || m.dialog != "" || m.err != tt.want {
				t.Fatalf("read-only %q: dialog=%q error=%q cmd=%v", tt.key, m.dialog, m.err, cmd)
			}
			// Uppercase forms are the same keys.
			next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{rune(tt.key[0] - 32)}})
			m = next.(model)
			if cmd != nil || m.dialog != "" || m.err != tt.want {
				t.Fatalf("read-only %q: dialog=%q error=%q cmd=%v", tt.key, m.dialog, m.err, cmd)
			}
		})
	}
}

func TestBoardNamesImportAnalysis(t *testing.T) {
	m := model{width: 100, height: 30, snap: &ViewSnapshot{
		State: "planning", ActivityDetail: "Read to-do.json",
		ActivityLog: "import-analysis-1/events-00.jsonl", ActivityUpdated: time.Now().Format(time.RFC3339),
		Imports: []ImportView{{RequestID: "import-1", Status: ImportPlanning}},
	}}
	view := m.View()
	if !strings.Contains(view, "Analyzing imported requirements.") || strings.Contains(view, "Planning task scopes and checks.") {
		t.Fatalf("import analysis not named: %s", view)
	}
	if !strings.Contains(view, "Last output") {
		t.Fatalf("missing output age: %s", view)
	}
	m.snap.Imports = nil
	if view = m.View(); !strings.Contains(view, "Planning task scopes and checks.") {
		t.Fatalf("ordinary planning note lost: %s", view)
	}
}

// A diagnosis consultation names itself on the board and reports output age,
// so a long consultation reads as a live agent instead of a hang.
func TestBoardNamesDiagnosisConsultation(t *testing.T) {
	m := model{width: 100, height: 30, snap: &ViewSnapshot{
		State: "diagnosing", ActivityDetail: "Read verify.log",
		ActivityLog:     "task-0123456789abcdef/diagnose-01-0123456789abcdef/events-00.jsonl",
		ActivityUpdated: time.Now().Format(time.RFC3339),
		Attempts:        []AttemptView{{ID: "task-1", TaskID: "T001", Phase: "verifying"}},
	}}
	if label := activityLabel(m.snap, false); label != "DIAGNOSING" {
		t.Fatalf("activity label %q, want DIAGNOSING", label)
	}
	if state, _ := StatusLabel(m.snap, false); state != "RUNNING" {
		t.Fatalf("state label %q, want RUNNING", state)
	}
	view := m.View()
	for _, want := range []string{"Latest: Read verify.log", "Consulting the review agent about a repeated failure.", "Last output"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q: %s", want, view)
		}
	}
	// The parallel worker stays visible beside the consultation.
	if !strings.Contains(view, "T001") {
		t.Fatalf("in-flight work disappeared during the consultation: %s", view)
	}
	// The overview names the operation and its agent.
	m.snap.ActivityAgent = "claude"
	if header := ansi.Strip(m.orientationHeader()); !strings.Contains(header, "Operation: DIAGNOSING · Agent: claude") {
		t.Fatalf("diagnosis operation missing from the overview: %s", header)
	}
	// The run-recovery consultation names itself distinctly.
	m.snap.State = "recovering"
	if label := activityLabel(m.snap, false); label != "RUN RECOVERY" {
		t.Fatalf("run-recovery label %q", label)
	}
	if view := m.View(); !strings.Contains(view, "Consulting the review agent about retained orchestration failures.") {
		t.Fatalf("run-recovery note missing: %s", view)
	}
	// Small screens stay within their bounds; the clipped detail remains
	// reachable through the scroll hint, like any longer board.
	for _, size := range [][2]int{{30, 12}, {40, 16}} {
		m.width, m.height = size[0], size[1]
		m.snap.State = "diagnosing"
		view := m.View()
		if lipgloss.Width(view) > size[0] || lipgloss.Height(view) > size[1] {
			t.Fatalf("%v: diagnosis board overflow:\n%s", size, view)
		}
		if !strings.Contains(view, "Read verify.log") && !strings.Contains(view, "More below") {
			t.Fatalf("%v: diagnosis detail unreachable:\n%s", size, view)
		}
	}
}
