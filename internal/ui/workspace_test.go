package ui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func workspaceFixture() model {
	now := time.Now().UTC()
	active := AttemptView{ID: "attempt-2", TaskID: "T002", Title: "Implement queue recovery", Phase: "running", Agent: "codex", LastOutput: now.Add(-12 * time.Second).Format(time.RFC3339), ProcessState: "running", ProcessStarted: now.Add(-6 * time.Minute).Format(time.RFC3339)}
	blocked := TaskView{ID: "T004", Title: "Add Redis integration tests", Status: "blocked", Revision: "revision", Blockers: []string{"Redis is unavailable"}, UnblockWhen: "Install Redis and configure the test connection."}
	m := model{width: 100, height: 24, cursor: 3, snap: &ViewSnapshot{Project: "task-queue", Goal: "Build a durable task queue", Session: "session", Now: now.Format(time.RFC3339), Started: now.Add(-14 * time.Hour).Format(time.RFC3339), WorkerLimit: 4, CheckLimit: 1,
		Counters: ViewCounters{Done: 1, Total: 5, InFlight: 1, Blocked: 1}, Attempts: []AttemptView{active}, Ready: []TaskView{{ID: "T003", Title: "Document operations"}},
		Tasks: []TaskView{{ID: "T001", Title: "Build module layout", Status: "done"}, {ID: "T002", Title: active.Title, Status: "doing", Attempt: &active}, {ID: "T003", Title: "Document operations", Status: "todo"}, blocked, {ID: "T005", Title: "Exercise the public API", Status: "todo", DependsOn: []string{"T004"}, WaitingOn: []string{"T004"}}},
		Pace:  &PaceView{Session: 1, Hour: 1, Day: 1, LastIntegration: now.Add(-4 * time.Minute).Format(time.RFC3339)}},
		events: []JournalEvent{{ID: 1, Time: now.Add(-4 * time.Minute).Format(time.RFC3339), Kind: "result", Message: "T001  done | Module layout verified"}, {ID: 2, Time: now.Add(-3 * time.Minute).Format(time.RFC3339), Kind: "progress", Message: "noisy counter"}, {ID: 3, Time: now.Format(time.RFC3339), Kind: "worker_activity", Message: "Agent returned a result"}}, tailView: viewport.New(100, 12), diffView: viewport.New(100, 12)}
	m.initWorkspace()
	m.syncTasks()
	m.sizeViewports()
	return m
}

func press(m model, key string) (model, tea.Cmd) {
	types := map[string]tea.KeyType{"enter": tea.KeyEnter, "esc": tea.KeyEsc, "up": tea.KeyUp, "down": tea.KeyDown, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "end": tea.KeyEnd, "tab": tea.KeyTab}
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	if typ, ok := types[key]; ok {
		msg = tea.KeyMsg{Type: typ}
	}
	next, cmd := m.keys(msg)
	return next.(model), cmd
}

func TestWorkspaceOverviewOrientation(t *testing.T) {
	m := workspaceFixture()
	view := ansi.Strip(m.View())
	for _, want := range []string{"Build a durable task queue", "Operation: IMPLEMENTING", "1/5 complete", "Redis is unavailable", "output 12s ago", "Recent outcomes", "T001 integrated", "Session: 14h", "1 integrated this session"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "noisy counter") || strings.Contains(view, "Agent returned") {
		t.Fatal("internal activity displaced meaningful outcomes")
	}
	if lipgloss.Height(view) != 24 {
		t.Fatal("footer is not anchored")
	}
	if os.Getenv("LOOPER_RENDER_REVIEW") != "" {
		os.WriteFile(os.Getenv("LOOPER_RENDER_REVIEW"), []byte(view), 0600)
	}
}

func TestWorkspaceShowsInputWaitWithoutEventHistory(t *testing.T) {
	for _, size := range [][2]int{{100, 24}, {50, 16}, {30, 12}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m := workspaceFixture()
			m.width, m.height = size[0], size[1]
			m.snap.State = "waiting"
			m.snap.ActivityDetail = "/repo/to-do.json: invalid status. Correct the file to continue."
			m.snap.Attempts = nil
			m.snap.Tasks = []TaskView{{ID: "T1", Title: "ready work", Status: "todo"}}
			m.snap.Counters = ViewCounters{Total: 1, Ready: 1}
			m.events, m.snap.RecentOutcomes = nil, nil
			m.syncTasks()
			m.sizeViewports()
			view := ansi.Strip(m.View())
			if !strings.Contains(view, "WAITING") || !strings.Contains(view, "Correct task file") || strings.Contains(view, "SCHEDULING") {
				t.Fatalf("the overview does not explain the input wait:\n%s", view)
			}
			var lines []string
			for _, row := range m.overviewRows() {
				lines = append(lines, ansi.Strip(row.text))
			}
			detail := strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
			if strings.Contains(detail, "Scheduling") || strings.Contains(detail, "in progress") {
				t.Fatalf("the overview presents paused dispatch as active: %s", detail)
			}
			for _, want := range []string{"/repo/to-do.json", "invalid status", "Correct the file to continue."} {
				if !strings.Contains(detail, want) {
					t.Fatalf("the scrollable overview omits %q: %s", want, detail)
				}
			}
			if lipgloss.Width(m.View()) > m.width || lipgloss.Height(m.View()) != m.height {
				t.Fatal("the input wait overflows the terminal")
			}
		})
	}
}

func TestWorkspaceNavigationAcrossTaskStates(t *testing.T) {
	m := workspaceFixture()
	m.workspace.selectedTask = "T004"
	var inspected string
	m.ctrl.Inspect = func(id string) (TaskDetail, error) {
		inspected = id
		return TaskDetail{Task: m.snap.Tasks[3], Summary: "Partial work retained", Checks: []CheckView{{Kind: "executable", Result: "fail"}}, Feedback: "connection refused"}, nil
	}
	next, cmd := press(m, "enter")
	if next.mode != ModeInspect || cmd == nil {
		t.Fatal("blocked task cannot be inspected")
	}
	result := cmd()
	out, _ := next.Update(result)
	next = out.(model)
	if inspected != "T004" {
		t.Fatal("wrong task inspected")
	}
	for _, want := range []string{"Install Redis", "connection refused", "Recorded checks", "Partial work retained"} {
		if !strings.Contains(next.inspectorContent(100), want) {
			t.Fatalf("inspector missing %s", want)
		}
	}
	next, _ = press(next, "esc")
	next, _ = press(next, "u")
	next.workspace.selectedTask = "T001"
	next.syncTasks()
	next, cmd = press(next, "enter")
	if next.mode != ModeInspect || next.workspace.detailID != "T001" || cmd == nil {
		t.Fatal("completed task cannot be inspected")
	}
}

func TestWorkspaceSelectionSurvivesCompletionAndReordering(t *testing.T) {
	m := workspaceFixture()
	m.mode = ModeTasks
	m.workspace.selectedTask = "T002"
	m.syncTasks()
	snapshot := *m.snap
	snapshot.Tasks = append([]TaskView(nil), m.snap.Tasks...)
	snapshot.Tasks[1].Status = "done"
	snapshot.Tasks[1].Attempt = nil
	snapshot.Attempts = nil
	next, _ := m.Update(snapshot)
	m = next.(model)
	if m.workspaceTask().ID != "T002" {
		t.Fatal("completion moved selection to another task")
	}
	if row := m.workspace.taskTable.SelectedRow(); len(row) == 0 || row[0] != "T002" {
		t.Fatalf("table selection changed: %+v", row)
	}
	snapshot.Tasks = append(snapshot.Tasks[:1], snapshot.Tasks[2:]...)
	next, _ = m.Update(snapshot)
	m = next.(model)
	if m.workspaceTask() == nil || m.workspaceTask().ID == "T002" {
		t.Fatal("removed task did not yield a valid selection")
	}
}

func TestWorkspaceSearchAndFilters(t *testing.T) {
	m := workspaceFixture()
	m, _ = press(m, "/")
	for _, r := range "Redis" {
		m, _ = press(m, string(r))
	}
	if tasks := m.filteredTasks(); len(tasks) != 1 || tasks[0].ID != "T004" {
		t.Fatalf("search failed: %+v", tasks)
	}
	m, _ = press(m, "esc")
	m.workspace.search.SetValue("")
	m.workspace.filter = 3
	m.syncTasks()
	if tasks := m.filteredTasks(); len(tasks) != 1 || tasks[0].ID != "T005" {
		t.Fatal("dependency-wait filter is wrong")
	}
	m.workspace.filter = 4
	m.syncTasks()
	if tasks := m.filteredTasks(); len(tasks) != 1 || tasks[0].ID != "T001" {
		t.Fatal("completed tasks inaccessible")
	}
}

func TestWorkspaceStaleSnapshotAndQuietOutput(t *testing.T) {
	m := workspaceFixture()
	m.ctrl.Poll = func() *ViewSnapshot { return nil }
	next, _ := m.Update(tickMsg{})
	m = next.(model)
	view := ansi.Strip(m.View())
	for _, want := range []string{"UNKNOWN", "disconnected", "Last known work", "1/5 complete"} {
		if !strings.Contains(view, want) {
			t.Fatalf("stale view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "stuck") {
		t.Fatal("silence invents a stalled worker")
	}
}

func TestWorkspaceActivityPauseAndResume(t *testing.T) {
	m := workspaceFixture()
	for i := 4; i <= 50; i++ {
		m.events = append(m.events, JournalEvent{ID: int64(i), Time: m.snap.Now, Message: fmt.Sprintf("event %d", i)})
	}
	m.cursor = 50
	m, _ = press(m, "t")
	m, _ = press(m, "up")
	offset := m.tailView.YOffset
	m.ctrl.Events = func(int64, int) []JournalEvent {
		return []JournalEvent{{ID: 51, Time: m.snap.Now, Message: "new event"}}
	}
	next, _ := m.Update(tickMsg{})
	m = next.(model)
	if !m.workspace.tailPaused || m.tailView.YOffset != offset || !strings.Contains(ansi.Strip(m.View()), "Paused · 1 new event") {
		t.Fatal("activity did not preserve scroll position with incoming events")
	}
	m, _ = press(m, "end")
	if m.workspace.tailPaused || !m.tailView.AtBottom() || !strings.Contains(ansi.Strip(m.View()), "Following live") {
		t.Fatal("cannot resume live activity")
	}
}

func TestWorkspaceRejectsOldInspectorResults(t *testing.T) {
	m := workspaceFixture()
	m.ctrl.Inspect = func(id string) (TaskDetail, error) { return TaskDetail{Task: TaskView{ID: id}}, nil }
	old := m.loadTaskDetail("T001")
	latest := m.loadTaskDetail("T004")
	next, _ := m.Update(latest())
	m = next.(model)
	next, _ = m.Update(old())
	m = next.(model)
	if m.workspace.detail.Task.ID != "T004" {
		t.Fatal("late result replaced current inspector")
	}
}

func TestWorkspaceAttachedQuitAndRequeueConfirmation(t *testing.T) {
	m := workspaceFixture()
	cancelled := false
	requeued := false
	m.ctrl.Cancel = func() { cancelled = true }
	_, cmd := press(m, "q")
	if cmd == nil || cancelled {
		t.Fatal("attached quit stopped the controller")
	}
	m.mode = ModeInspect
	m.workspace.detailID = "T004"
	m.ctrl.Requeue = func(id, revision string) error {
		if id != "T004" || revision != "revision" {
			t.Fatal("requeue lost task identity")
		}
		requeued = true
		return nil
	}
	m, cmd = press(m, "r")
	if m.dialog != "requeue" || cmd != nil || requeued {
		t.Fatal("requeued without confirmation")
	}
	m, _ = press(m, "esc")
	if requeued {
		t.Fatal("cancelled requeue mutated task")
	}
	m, _ = press(m, "r")
	m, cmd = press(m, "y")
	if cmd == nil || !m.workspace.requeuePending {
		t.Fatal("missing pending acknowledgement")
	}
	next, _ := m.Update(cmd())
	m = next.(model)
	if !requeued || m.workspace.requeuePending || !strings.Contains(m.message, "T004 requeued") {
		t.Fatal("requeue acknowledgement missing")
	}
	m.ctrl.ReadOnly = true
	m, cmd = press(m, "r")
	if cmd != nil || m.dialog != "" || m.err == "" {
		t.Fatal("read-only observer can requeue")
	}
}

func TestWorkspaceScreensFitAndKeepGlobalControls(t *testing.T) {
	for _, size := range [][2]int{{30, 14}, {40, 18}, {58, 22}, {80, 24}, {120, 32}, {160, 45}} {
		for _, mode := range []int{ModeBoard, ModeTasks, ModeInspect, ModeTail, ModeHelp, ModeLog, ModeDiff} {
			m := workspaceFixture()
			m.width = size[0]
			m.height = size[1]
			m.mode = mode
			m.workspace.detailID = "T004"
			m.snap.Goal = strings.Repeat("長いゴール ", 80)
			m.snap.Tasks[3].UnblockWhen = strings.Repeat("長い回復条件 ", 80)
			m.refreshInspector()
			m.syncTasks()
			view := ansi.Strip(m.View())
			if lipgloss.Width(view) > size[0] || lipgloss.Height(view) > size[1] {
				t.Fatalf("%v mode %d overflows %dx%d:\n%s", size, mode, lipgloss.Width(view), lipgloss.Height(view), view)
			}
			if !strings.Contains(strings.ToLower(view), "detach") || !strings.Contains(strings.ToLower(view), "help") {
				t.Fatalf("%v mode %d lost global controls:\n%s", size, mode, view)
			}
		}
	}
}

func TestWorkspaceLargeBacklogDoesNotHideOutcomes(t *testing.T) {
	m := workspaceFixture()
	for i := 6; i <= 1000; i++ {
		t := TaskView{ID: fmt.Sprintf("T%04d", i), Status: "todo", Title: "Waiting work"}
		m.snap.Tasks = append(m.snap.Tasks, t)
		m.snap.Ready = append(m.snap.Ready, t)
	}
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "Recent outcomes") || !strings.Contains(view, "T001 integrated") {
		t.Fatal("large ready queue displaced the orientation summary")
	}
}

// Run the real terminal renderer without starting agents or a controller.
func TestWorkspaceTerminalPreview(t *testing.T) {
	if os.Getenv("LOOPER_TUI_PREVIEW") != "1" {
		t.Skip("set LOOPER_TUI_PREVIEW=1 in a terminal for a visual review")
	}
	fixture := workspaceFixture()
	controls := Controls{ReadOnly: true, Poll: func() *ViewSnapshot { return fixture.snap }, Events: func(after int64, _ int) []JournalEvent {
		var out []JournalEvent
		for _, e := range fixture.events {
			if e.ID > after {
				out = append(out, e)
			}
		}
		return out
	},
		Inspect: func(id string) (TaskDetail, error) {
			for _, task := range fixture.snap.Tasks {
				if task.ID == id {
					return TaskDetail{Task: task, Summary: "Recorded task result", Files: []string{"internal/queue.go", "internal/queue_test.go"}, VerifyCommand: "go test ./...", Checks: []CheckView{{Time: fixture.snap.Now, Kind: "executable", Result: "pass"}}}, nil
				}
			}
			return TaskDetail{}, fmt.Errorf("unknown task")
		}}
	if _, err := RunDashboard(controls); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceShortcutLabelsAreLowercase(t *testing.T) {
	m := workspaceFixture()
	for _, dialog := range []string{"", "exit", "drain", "add-plan", "requeue"} {
		m.dialog = dialog
		m.workspace.requeueTask = &m.snap.Tasks[3]
		view := ansi.Strip(m.View())
		for _, label := range []string{"[Q]", "[U]", "[L]", "[D]", "[Y]", "[Esc]", "[Enter]", "[PgUp/PgDn]"} {
			if strings.Contains(view, label) {
				t.Fatalf("uppercase key label %q: %s", label, view)
			}
		}
	}
}

func TestWorkspacePausedActivitySurvivesBufferEviction(t *testing.T) {
	m := workspaceFixture()
	for i := 4; i <= 500; i++ {
		m.events = append(m.events, JournalEvent{ID: int64(i), Time: m.snap.Now, Message: fmt.Sprintf("original event %d", i)})
	}
	m.cursor = 500
	m, _ = press(m, "t")
	m, _ = press(m, "pgup")
	before := m.tailView.View()
	m.ctrl.Events = func(int64, int) []JournalEvent {
		var events []JournalEvent
		for i := 501; i <= 700; i++ {
			events = append(events, JournalEvent{ID: int64(i), Time: m.snap.Now, Message: fmt.Sprintf("incoming event %d", i)})
		}
		return events
	}
	next, _ := m.Update(tickMsg{})
	m = next.(model)
	if m.tailView.View() != before || len(m.events) != 500 || m.workspace.tailNew != 200 {
		t.Fatal("bounded buffer eviction replaced text being read")
	}
	m, _ = press(m, "end")
	if !strings.Contains(m.tailView.View(), "incoming event 700") {
		t.Fatal("resume did not show latest event")
	}
}

func TestWorkspaceInspectorRetainsOnDemandPurpose(t *testing.T) {
	m := workspaceFixture()
	m.mode = ModeInspect
	m.workspace.detailID = "T004"
	task := m.snap.Tasks[3]
	task.Details = "Check queue recovery against a real Redis instance."
	m.workspace.detail = &TaskDetail{Task: task}
	if !strings.Contains(m.inspectorContent(100), task.Details) {
		t.Fatal("published summary erased on-demand acceptance text")
	}
}
