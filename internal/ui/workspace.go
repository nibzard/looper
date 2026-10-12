package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type workspaceState struct {
	initialized    bool
	selectedTask   string
	taskTable      table.Model
	search         textinput.Model
	searching      bool
	filter         int
	inspector      viewport.Model
	detail         *TaskDetail
	detailID       string
	detailErr      string
	detailAt       time.Time
	loading        bool
	sequence       int
	backMode       int
	returnMode     int
	tailPaused     bool
	tailNew        int
	requeuePending bool
	requeueTask    *TaskView
}

type taskDetailMsg struct {
	taskID   string
	sequence int
	detail   TaskDetail
	err      error
}
type requeueMsg struct {
	taskID string
	err    error
}

func (m model) hasWorkspace() bool {
	return m.snap != nil && (m.snap.Tasks != nil || m.snap.Project != "")
}

func (m *model) initWorkspace() {
	if m.workspace.initialized {
		return
	}
	m.workspace.initialized = true
	m.workspace.search = textinput.New()
	m.workspace.search.Placeholder = "Search tasks"
	m.workspace.search.Prompt = "/ "
	m.workspace.search.CharLimit = 256
	m.workspace.inspector = viewport.New(max(1, m.width), max(1, m.height-8))
	m.workspace.taskTable = table.New(table.WithFocused(true))
	styles := table.DefaultStyles()
	styles.Header = sectionStyle
	styles.Cell = textStyle
	styles.Selected = selectStyle
	m.workspace.taskTable.SetStyles(styles)
}

func taskPhase(t TaskView) string {
	if t.Status == "blocked" {
		return "blocked"
	}
	if t.Status == "done" {
		return "done"
	}
	if t.Attempt != nil && t.Attempt.Phase != "blocked" && t.Attempt.Phase != "done" {
		return PhaseLabel(t.Attempt.Phase)
	}
	if len(t.WaitingOn) > 0 {
		return "waiting"
	}
	if t.Status == "todo" {
		return "ready"
	}
	return t.Status
}

func taskRank(t TaskView) int {
	switch taskPhase(t) {
	case "blocked":
		return 0
	case "implementing", "checking", "queued", "resuming", "integrating":
		return 1
	case "ready":
		return 2
	case "waiting":
		return 3
	case "done":
		return 4
	default:
		return 2
	}
}

func (m model) orderedTasks() []TaskView {
	if m.snap == nil {
		return nil
	}
	tasks := append([]TaskView(nil), m.snap.Tasks...)
	sort.SliceStable(tasks, func(i, j int) bool { return taskRank(tasks[i]) < taskRank(tasks[j]) })
	return tasks
}

func (m model) filteredTasks() []TaskView {
	query := strings.ToLower(m.workspace.search.Value())
	var tasks []TaskView
	for _, t := range m.orderedTasks() {
		phase := taskPhase(t)
		switch m.workspace.filter {
		case 1:
			if taskRank(t) != 1 {
				continue
			}
		case 2:
			if phase != "blocked" {
				continue
			}
		case 3:
			if phase != "waiting" {
				continue
			}
		case 4:
			if phase != "done" {
				continue
			}
		}
		if query != "" && !strings.Contains(strings.ToLower(t.ID+" "+t.Title+" "+phase+" "+t.Details), query) {
			continue
		}
		tasks = append(tasks, t)
	}
	return tasks
}

func (m *model) preserveTaskSelection() {
	if !m.hasWorkspace() || m.workspace.selectedTask != "" {
		return
	}
	if t := m.workspaceTask(); t != nil {
		m.workspace.selectedTask = t.ID
	}
}

func (m model) workspaceTask() *TaskView {
	if m.snap == nil {
		return nil
	}
	id := m.workspace.selectedTask
	if m.mode == ModeInspect && m.workspace.detailID != "" {
		id = m.workspace.detailID
	}
	if m.mode == ModeTasks {
		tasks := m.filteredTasks()
		for _, t := range tasks {
			if t.ID == id {
				return &t
			}
		}
		if len(tasks) > 0 {
			return &tasks[0]
		}
		return nil
	}
	for _, t := range m.snap.Tasks {
		if t.ID == id {
			return &t
		}
	}
	if m.mode == ModeInspect {
		return nil
	}
	tasks := m.filteredTasks()
	if len(tasks) > 0 {
		return &tasks[0]
	}
	return nil
}

func (m *model) syncTasks() {
	if !m.hasWorkspace() {
		return
	}
	m.initWorkspace()
	tasks := m.filteredTasks()
	index := -1
	for i, t := range tasks {
		if t.ID == m.workspace.selectedTask {
			index = i
			break
		}
	}
	if index < 0 && len(tasks) > 0 {
		index = 0
		m.workspace.selectedTask = tasks[0].ID
	}
	listWidth := m.width
	if m.width >= 110 {
		listWidth = (m.width - 3) * 45 / 100
	}
	idWidth := 8
	for _, t := range tasks {
		idWidth = max(idWidth, min(14, lipgloss.Width(plainText(t.ID))))
	}
	phaseWidth := 13
	titleWidth := max(1, listWidth-idWidth-phaseWidth-6)
	if listWidth < 40 {
		idWidth = 6
		phaseWidth = 9
		titleWidth = max(1, listWidth-idWidth-phaseWidth-6)
	}
	m.workspace.taskTable.SetColumns([]table.Column{{Title: "Task", Width: idWidth}, {Title: "State", Width: phaseWidth}, {Title: "Title", Width: titleWidth}})
	rows := make([]table.Row, 0, len(tasks))
	for _, t := range tasks {
		rows = append(rows, table.Row{plainText(t.ID), taskPhase(t), plainText(t.Title)})
	}
	m.workspace.taskTable.SetRows(rows)
	if index >= 0 {
		m.workspace.taskTable.SetCursor(index)
	}
	m.workspace.taskTable.SetHeight(max(1, m.height-lipgloss.Height(m.header())-lipgloss.Height(m.workspaceFooter())-5))
}

func (m *model) loadTaskDetail(id string) tea.Cmd {
	if id == "" {
		return nil
	}
	m.workspace.detailID = id
	m.workspace.detailAt = time.Now()
	m.workspace.sequence++
	sequence := m.workspace.sequence
	m.workspace.loading = m.ctrl.Inspect != nil
	if m.workspace.detail != nil && m.workspace.detail.Task.ID != id {
		m.workspace.detail = nil
	}
	m.workspace.detailErr = ""
	if m.ctrl.Inspect == nil {
		m.refreshInspector()
		return nil
	}
	inspect := m.ctrl.Inspect
	return func() tea.Msg {
		d, err := inspect(id)
		return taskDetailMsg{taskID: id, sequence: sequence, detail: d, err: err}
	}
}

func (m model) workspaceKeys(msg tea.KeyMsg) (bool, tea.Model, tea.Cmd) {
	k := strings.ToLower(msg.String())
	switch k {
	case "u":
		m.mode = ModeTasks
		m.syncTasks()
		var cmd tea.Cmd
		if t := m.workspaceTask(); t != nil {
			cmd = m.loadTaskDetail(t.ID)
		}
		return true, m, cmd
	case "/":
		m.mode = ModeTasks
		m.workspace.searching = true
		m.workspace.search.Focus()
		m.syncTasks()
		return true, m, textinput.Blink
	case "tab":
		switch m.mode {
		case ModeBoard:
			m.mode = ModeTasks
		case ModeTasks:
			m.mode = ModeTail
			m.followActivity()
		case ModeTail:
			m.mode = ModeStatistics
			m.refreshStatistics()
			m.tailView.GotoTop()
		default:
			m.mode = ModeBoard
		}
		m.syncTasks()
		if m.mode == ModeTasks && m.width >= 110 {
			if t := m.workspaceTask(); t != nil {
				return true, m, m.loadTaskDetail(t.ID)
			}
		}
		return true, m, nil
	case "enter":
		if m.mode == ModeBoard || m.mode == ModeTasks {
			if t := m.workspaceTask(); t != nil {
				m.workspace.backMode = m.mode
				m.mode = ModeInspect
				m.workspace.inspector.GotoTop()
				cmd := m.loadTaskDetail(t.ID)
				m.refreshInspector()
				return true, m, cmd
			}
		}
	case "esc":
		if m.mode == ModeInspect {
			m.mode = m.workspace.backMode
			return true, m, nil
		}
		if m.mode == ModeLog || m.mode == ModeDiff || m.mode == ModeHelp {
			m.mode = m.workspace.returnMode
			return true, m, nil
		}
	case "r":
		if m.mode == ModeInspect {
			t := m.workspaceTask()
			if t == nil || t.Status != "blocked" {
				m.message = "Only blocked tasks can be requeued."
			} else if m.ctrl.ReadOnly || m.ctrl.Requeue == nil {
				m.err = "This view cannot requeue tasks. Resolve the blocker and set the task to todo while Looper is idle."
			} else if !m.workspace.requeuePending {
				copy := *t
				m.workspace.requeueTask = &copy
				m.dialog = "requeue"
			}
			return true, m, nil
		}
	case "f":
		if m.mode == ModeTasks {
			m.workspace.filter = (m.workspace.filter + 1) % 5
			m.syncTasks()
			if m.width >= 110 {
				if t := m.workspaceTask(); t != nil {
					return true, m, m.loadTaskDetail(t.ID)
				}
			}
			return true, m, nil
		}
	case "end":
		if m.mode == ModeTail {
			m.followActivity()
			return true, m, nil
		}
	}
	if m.mode == ModeInspect {
		switch k {
		case "up", "k":
			m.workspace.inspector.LineUp(1)
		case "down", "j":
			m.workspace.inspector.LineDown(1)
		case "pgup":
			m.workspace.inspector.HalfViewUp()
		case "pgdown":
			m.workspace.inspector.HalfViewDown()
		case "home":
			m.workspace.inspector.GotoTop()
		case "end":
			m.workspace.inspector.GotoBottom()
		default:
			return false, m, nil
		}
		return true, m, nil
	}
	if m.mode == ModeTasks || m.mode == ModeBoard {
		if m.mode == ModeBoard && (k == "pgup" || k == "pgdown") {
			delta := max(1, m.overviewAvailable()/2)
			if k == "pgup" {
				delta = -delta
			}
			m.scroll = max(0, min(max(0, len(m.overviewRows())-m.overviewAvailable()), m.scroll+delta))
			return true, m, nil
		}
		delta := 0
		switch k {
		case "up", "k":
			delta = -1
		case "down", "j":
			delta = 1
		case "pgup":
			delta = -max(1, m.height/2)
		case "pgdown":
			delta = max(1, m.height/2)
		}
		if delta != 0 {
			tasks := m.filteredTasks()
			if m.mode == ModeBoard {
				tasks = nil
				for _, t := range m.orderedTasks() {
					if taskPhase(t) != "done" {
						tasks = append(tasks, t)
					}
				}
			}
			if len(tasks) > 0 {
				index := 0
				for i, t := range tasks {
					if t.ID == m.workspace.selectedTask {
						index = i
						break
					}
				}
				index = max(0, min(len(tasks)-1, index+delta))
				m.workspace.selectedTask = tasks[index].ID
				m.syncTasks()
				if m.mode == ModeTasks && m.width >= 110 {
					return true, m, m.loadTaskDetail(tasks[index].ID)
				}
				m.keepOverviewSelectionVisible()
			}
			return true, m, nil
		}
	}
	return false, m, nil
}

func ageLabel(stamp string, now time.Time) string {
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return "unknown"
	}
	d := max(time.Duration(0), now.Sub(at))
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

func (m model) referenceTime() time.Time {
	if m.snap != nil && (m.staleView || m.snap.Stale) {
		if at, err := time.Parse(time.RFC3339Nano, m.snap.Now); err == nil {
			return at
		}
	}
	return time.Now()
}

func (m model) orientationHeader() string {
	v := m.snap
	label, style := StatusLabel(v, m.staleView)
	role := "Attached"
	if m.ctrl.Snapshot != nil {
		role = "Local controller"
	}
	fresh := "snapshot " + ageLabel(v.Now, time.Now()) + " ago"
	if m.staleView || v.Stale {
		fresh = "disconnected · last known state"
	}
	project := truncate(plainText(v.Project), max(1, min(28, m.width/3)))
	line := titleStyle.Render("LOOPER") + "  " + style.Render(label) + " · " + project
	lines := []string{truncate(line, m.width)}
	if m.width >= 100 {
		lines[0] = truncate(line+" · "+role+" · "+fresh, m.width)
	} else {
		lines = append(lines, truncate(role+" · "+fresh, m.width))
	}
	if v.Goal != "" {
		lines = append(lines, truncate("Goal: "+plainText(v.Goal), m.width))
	}
	operation := activityLabel(v, m.staleView)
	if operation == "" {
		operation = "Unknown"
	}
	if v.State != "" && v.State != "waiting" && v.ActivityDetail != "" {
		operation += " · " + plainText(v.ActivityDetail)
	}
	lines = append(lines, truncate("Operation: "+operation, m.width))
	summary := fmt.Sprintf("%d/%d complete · %d implementing · %d checking · %d blocked · %d ready", v.Counters.Done, v.Counters.Total, v.Counters.InFlight, v.Counters.Checking, v.Counters.Blocked, len(v.Ready))
	if m.width < 70 {
		summary = fmt.Sprintf("%d/%d complete · %d active · %d blocked", v.Counters.Done, v.Counters.Total, len(v.Attempts), v.Counters.Blocked)
	}
	if m.width < 45 {
		summary = fmt.Sprintf("%d/%d complete · %d blocked", v.Counters.Done, v.Counters.Total, v.Counters.Blocked)
	}
	lines = append(lines, truncate(summary, m.width))
	timing := "Session: " + m.elapsed()
	if v.Pace != nil {
		timing += fmt.Sprintf(" · %d integrated this session · %d in 1h · %d in 24h", v.Pace.Session, v.Pace.Hour, v.Pace.Day)
		if v.Pace.LastIntegration != "" && m.width >= 100 {
			timing += " · last " + ageLabel(v.Pace.LastIntegration, m.referenceTime()) + " ago"
		}
	}
	if m.height >= 20 {
		lines = append(lines, truncate(timing, m.width))
	}
	return strings.Join(lines, "\n")
}

type overviewRow struct {
	text   string
	taskID string
}

func (m model) overviewRows() []overviewRow {
	var rows []overviewRow
	add := func(s string) { rows = append(rows, overviewRow{text: truncate(s, m.width)}) }
	selected := ""
	if t := m.workspaceTask(); t != nil {
		selected = t.ID
	}
	taskRow := func(t TaskView, text string) {
		prefix := "  "
		style := textStyle
		if t.Status == "blocked" {
			style = blockedStyle
		}
		if selected == t.ID {
			prefix = "› "
			style = style.Bold(true)
		}
		rows = append(rows, overviewRow{text: style.Render(truncate(prefix+t.ID+" "+text, m.width)), taskID: t.ID})
	}
	window := func(tasks []TaskView, limit int) []TaskView {
		limit = max(1, limit)
		start := 0
		for i, t := range tasks {
			if t.ID == selected && i >= limit {
				start = i - limit + 1
				break
			}
		}
		return tasks[start:min(len(tasks), start+limit)]
	}
	if m.snap.State == "waiting" && m.snap.ActivityDetail != "" && !m.staleView && !m.snap.Stale {
		add(sectionStyle.Render("Task file needs correction"))
		detail := ansi.Wrap(plainText(m.snap.ActivityDetail), max(1, m.width-2), "")
		for _, line := range strings.Split(detail, "\n") {
			add(blockedStyle.Render("  " + line))
		}
		add("")
	}
	var attention, active []TaskView
	waiting := 0
	for _, t := range m.orderedTasks() {
		if t.Status == "blocked" || (t.Attempt != nil && t.Attempt.Repeats > 0) {
			attention = append(attention, t)
		}
		if taskRank(t) == 1 {
			active = append(active, t)
		}
		if taskPhase(t) == "waiting" {
			waiting++
		}
	}
	budget := m.overviewAvailable()
	attentionLimit := 2
	if budget >= 24 {
		attentionLimit = 4
	}
	if len(attention) > 0 {
		add(sectionStyle.Render("Attention · " + countLabel(len(attention), "task")))
		shown := window(attention, attentionLimit)
		for _, t := range shown {
			reason := strings.Join(t.Blockers, "; ")
			if t.Status != "blocked" {
				reason = fmt.Sprintf("Repeated failure · %d repairs · inspect evidence", t.Attempt.Repairs)
			}
			taskRow(t, reason)
		}
		if len(shown) < len(attention) {
			add(fmt.Sprintf("  %d more attention items · [u] Tasks", len(attention)-len(shown)))
		}
		add("")
	}
	if len(active) > 0 {
		heading := fmt.Sprintf("Active work · %d implementing · %d checking", m.snap.Counters.InFlight, m.snap.Counters.Checking)
		if m.staleView || m.snap.Stale {
			heading = "Last known work"
		}
		add(sectionStyle.Render(heading))
		limit := max(1, budget-len(rows)-7)
		shown := window(active, limit)
		for _, t := range shown {
			a := t.Attempt
			evidence := "output unknown"
			if a.LastOutput != "" {
				evidence = "output " + ageLabel(a.LastOutput, m.referenceTime()) + " ago"
			}
			if a.ProcessState != "" {
				evidence += " · process " + a.ProcessState
			}
			if a.Repairs > 0 {
				evidence += fmt.Sprintf(" · repair %d", a.Repairs)
			}
			if m.width >= 120 && a.ProcessStarted != "" {
				evidence += " · process age " + ageLabel(a.ProcessStarted, m.referenceTime())
			}
			taskRow(t, PhaseLabel(a.Phase)+" · "+evidence+" · "+plainText(a.Title))
		}
		if len(shown) < len(active) {
			add(fmt.Sprintf("  %d more active tasks · [u] Tasks", len(active)-len(shown)))
		}
	} else if m.snap.State != "waiting" || m.staleView || m.snap.Stale {
		add(sectionStyle.Render("Scheduling"))
		switch {
		case m.staleView || m.snap.Stale:
			add("  Current scheduling state is unknown.")
		case m.snap.State != "":
			add("  " + activityLabel(m.snap, false) + " is in progress.")
		case len(m.snap.Ready) > 0:
			add(fmt.Sprintf("  %d tasks ready for dispatch.", len(m.snap.Ready)))
		case m.snap.Counters.Blocked > 0:
			add("  Waiting for blocker resolution.")
		case m.snap.Finished:
			add("  Goal satisfied. Waiting for new work.")
		default:
			add("  No ready tasks. Inspect dependencies in Tasks.")
		}
	}
	queued := 0
	for _, a := range m.snap.Attempts {
		if a.Phase == "ready" {
			queued++
		}
	}
	capacity := fmt.Sprintf("Queue: %d ready · %d waiting for dependencies · %d queued for checks", len(m.snap.Ready), waiting, queued)
	add(truncate(capacity, m.width))
	if m.snap.WorkerLimit > 0 && m.width >= 80 && budget >= 18 {
		add(fmt.Sprintf("Capacity: %d/%d implementing · %d/%d checking", m.snap.Counters.InFlight, m.snap.WorkerLimit, m.snap.Counters.Checking, m.snap.CheckLimit))
	}
	if t := m.workspaceTask(); t != nil && taskPhase(*t) == "ready" {
		taskRow(*t, "ready · "+plainText(t.Title))
	}
	if m.snap.State == "planning" && m.snap.ActivityLog != "" {
		output := "No agent output yet"
		if m.snap.ActivityUpdated != "" {
			output = "Last output " + ageLabel(m.snap.ActivityUpdated, m.referenceTime()) + " ago"
		}
		add(output)
	}
	add("")
	add(sectionStyle.Render("Recent outcomes"))
	outcomes := m.outcomes()
	if len(outcomes) == 0 {
		add("  No outcomes recorded in this session yet.")
	}
	count := max(1, min(5, budget-len(rows)))
	for _, e := range outcomes[max(0, len(outcomes)-count):] {
		add(m.outcomeLine(e))
	}
	if m.err != "" {
		add(errorStyle.Render("! " + plainText(m.err)))
	} else if m.message != "" {
		add(okStyle.Render(plainText(m.message)))
	}
	return rows
}

func (m model) outcomes() []JournalEvent {
	var events []JournalEvent
	seen := map[int64]bool{}
	if m.snap != nil && m.snap.RecentOutcomes != nil {
		events = append(events, m.snap.RecentOutcomes...)
		for _, e := range events {
			seen[e.ID] = true
		}
	}
	for _, e := range m.visibleEvents(false) {
		if e.ID != 0 && seen[e.ID] {
			continue
		}
		switch e.Kind {
		case "result", "repair", "blocker", "hold", "requeue", "refresh", "finish", "review-error", "hook-error", "diagnosis-error", "import", "import-error":
			events = append(events, e)
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	if len(events) > 5 {
		events = events[len(events)-5:]
	}
	return events
}

func (m model) outcomeLine(e JournalEvent) string {
	stamp := ""
	if at, err := time.Parse(time.RFC3339Nano, e.Time); err == nil {
		stamp = at.Local().Format("15:04:05") + "  "
	}
	message := plainText(e.Message)
	if e.Kind == "result" {
		message = strings.Replace(message, "  done | ", " integrated · ", 1)
	}
	return "  " + dimStyle.Render(stamp) + eventStyle(e.Kind).Render(message)
}

func (m model) overviewAvailable() int {
	return max(1, m.height-lipgloss.Height(m.header())-lipgloss.Height(m.workspaceFooter())-2)
}

func (m *model) keepOverviewSelectionVisible() {
	if m.mode != ModeBoard {
		return
	}
	rows := m.overviewRows()
	for i, r := range rows {
		if r.taskID != m.workspace.selectedTask {
			continue
		}
		if i < m.scroll {
			m.scroll = i
		}
		if i >= m.scroll+m.overviewAvailable() {
			m.scroll = i - m.overviewAvailable() + 1
		}
		break
	}
}

func (m model) overviewScreen() string {
	rows := m.overviewRows()
	available := m.overviewAvailable()
	offset := min(m.scroll, max(0, len(rows)-available))
	var body []string
	for _, row := range rows[offset:min(len(rows), offset+available)] {
		body = append(body, row.text)
	}
	if offset > 0 || offset+available < len(rows) {
		hint := "[pgup/pgdn] More"
		if available > 1 {
			body = body[:min(len(body), available-1)]
			body = append(body, dimStyle.Render(truncate(hint, m.width)))
		}
	}
	return m.fixedScreen(m.header(), strings.Join(body, "\n"), m.workspaceFooter())
}

type workspaceBindings struct{ bindings []key.Binding }

func (b workspaceBindings) ShortHelp() []key.Binding  { return b.bindings }
func (b workspaceBindings) FullHelp() [][]key.Binding { return [][]key.Binding{b.bindings} }

func (m model) workspaceFooter() string {
	exit := "detach"
	if m.ctrl.Snapshot != nil && !m.ctrl.ReadOnly {
		exit = "exit"
	}
	hints := []struct{ key, label string }{{"b", "overview"}, {"u", "tasks"}, {"t", "activity"}, {"p", "statistics"}, {"?", "help"}, {"q", exit}}
	if m.mode == ModeStatistics {
		hints = []struct{ key, label string }{{"b", "overview"}, {"j/k", "scroll"}, {"t", "activity"}, {"?", "help"}, {"q", exit}}
	}
	if m.mode == ModeInspect {
		hints = []struct{ key, label string }{{"esc", "back"}}
		if t := m.workspaceTask(); t != nil {
			if t.Attempt != nil && m.ctrl.WorkerLog != nil {
				hints = append(hints, struct{ key, label string }{"l", "logs"})
			}
			if t.Attempt != nil && m.ctrl.Diff != nil {
				hints = append(hints, struct{ key, label string }{"d", "diff"})
			}
			if t.Status == "blocked" && !m.ctrl.ReadOnly && m.ctrl.Requeue != nil {
				hints = append(hints, struct{ key, label string }{"r", "requeue"})
			}
		}
		hints = append(hints, struct{ key, label string }{"?", "help"}, struct{ key, label string }{"q", exit})
	}
	if m.mode == ModeTasks {
		hints = []struct{ key, label string }{{"b", "overview"}, {"/", "search"}, {"f", "filter"}, {"enter", "inspect"}, {"?", "help"}, {"q", exit}}
	}
	if m.width < 50 {
		hints = []struct{ key, label string }{{"b", "overview"}, {"u", "tasks"}, {"?", "help"}, {"q", exit}}
	}
	var bindings []key.Binding
	for _, h := range hints {
		bindings = append(bindings, key.NewBinding(key.WithKeys(h.key), key.WithHelp(h.key, h.label)))
	}
	h := help.New()
	h.Width = m.width
	h.Styles.ShortKey = textStyle
	h.Styles.ShortDesc = dimStyle
	// Help can omit keys on a small terminal. Keep the exit action available.
	if m.width < 50 {
		exitLabel := "Detach"
		if exit == "exit" {
			exitLabel = "Exit"
		}
		context := "[u] Tasks"
		if m.mode == ModeTasks || m.mode == ModeTail || m.mode == ModeStatistics {
			context = "[b] Back"
		}
		if m.mode == ModeInspect || m.mode == ModeLog || m.mode == ModeHelp || m.mode == ModeDiff {
			context = "[esc] Back"
		}
		return truncate(context+" [?] Help [q] "+exitLabel, m.width)
	}
	if m.width < 80 {
		return h.View(workspaceBindings{bindings: bindings[:min(3, len(bindings))]}) + "\n" + "[?] Help · [q] " + exit
	}
	return h.View(workspaceBindings{bindings})
}

// fixedScreen anchors the footer and caps all content at the terminal size.
func (m model) fixedScreen(header, body, footer string) string {
	width, height := max(1, m.width), max(1, m.height)
	head := strings.Split(header, "\n")
	foot := strings.Split(footer, "\n")
	if len(foot) >= height {
		return truncate(strings.Join(foot[:height], "\n"), width)
	}
	head = head[:min(len(head), max(0, height-len(foot)-1))]
	available := max(0, height-len(head)-len(foot)-1)
	lines := strings.Split(body, "\n")
	if body == "" {
		lines = nil
	}
	lines = lines[:min(len(lines), available)]
	for len(lines) < available {
		lines = append(lines, "")
	}
	out := append(append(append([]string{}, head...), ""), lines...)
	out = append(out, foot...)
	for i, line := range out {
		out[i] = truncate(line, width)
	}
	return strings.Join(out, "\n")
}

func (m model) tasksScreen() string {
	m.initWorkspace()
	m.syncTasks()
	filters := []string{"All", "Active", "Blocked", "Waiting", "Done"}
	scope := fmt.Sprintf("Tasks · %s · %d shown   [f] Filter", filters[m.workspace.filter], len(m.filteredTasks()))
	if m.workspace.searching || m.workspace.search.Value() != "" {
		scope = m.workspace.search.View()
	}
	left := truncate(scope, m.width) + "\n" + m.workspace.taskTable.View()
	if len(m.filteredTasks()) == 0 {
		left += "\nNo matching tasks. [f] Change filter or / edit search."
	}
	if m.width >= 110 {
		listWidth := (m.width - 3) * 45 / 100
		inspectorWidth := m.width - listWidth - 3
		right := m.inspectorContent(inspectorWidth)
		paneHeight := max(1, m.height-lipgloss.Height(m.header())-lipgloss.Height(m.workspaceFooter())-2)
		rightLines := strings.Split(right, "\n")
		rightLines = rightLines[:min(len(rightLines), paneHeight)]
		right = strings.Join(rightLines, "\n")
		divider := strings.TrimSuffix(strings.Repeat(" │ \n", paneHeight), "\n")
		left = lipgloss.JoinHorizontal(lipgloss.Top, lipgloss.NewStyle().Width(listWidth).Render(left), divider, lipgloss.NewStyle().Width(inspectorWidth).Render(right))
	}
	return m.fixedScreen(m.header(), left, m.workspaceFooter())
}

func (m model) inspectorContent(width int) string {
	var d TaskDetail
	t := m.workspaceTask()
	if t == nil {
		return "The task is no longer available. [esc] Back."
	}
	d.Task = *t
	if m.workspace.detail != nil && m.workspace.detail.Task.ID == t.ID {
		d = *m.workspace.detail
		// Published status is more current than a cached inspector read.
		details := d.Task.Details
		d.Task = *t
		d.Task.Details = details
	}
	var lines []string
	add := func(label, value string) {
		if value != "" {
			lines = append(lines, sectionStyle.Render(label), plainText(value), "")
		}
	}
	lines = append(lines, titleStyle.Render(plainText(t.ID)+" · "+plainText(t.Title)), "State: "+taskPhase(*t), "")
	if m.workspace.requeuePending {
		lines = append(lines, "Requeue request pending. Waiting for the controller.", "")
	} else if m.err != "" {
		lines = append(lines, errorStyle.Render(plainText(m.err)), "")
	} else if m.message != "" {
		lines = append(lines, okStyle.Render(plainText(m.message)), "")
	}
	if m.workspace.loading && m.workspace.detail == nil {
		lines = append(lines, "Loading recorded evidence...", "")
	}
	if m.workspace.detailErr != "" {
		lines = append(lines, errorStyle.Render(plainText(m.workspace.detailErr)), "")
	}
	add("Purpose", d.Task.Details)
	if len(t.DependsOn) > 0 {
		add("Dependencies", strings.Join(t.DependsOn, ", "))
	}
	if len(t.WaitingOn) > 0 {
		add("Waiting for", strings.Join(t.WaitingOn, ", "))
	}
	add("Blockers", strings.Join(t.Blockers, "; "))
	add("Recovery", t.UnblockWhen)
	if t.Status == "blocked" {
		lines = append(lines, "Resolve the blocker, then requeue while Looper is idle.", "")
	}
	if a := t.Attempt; a != nil {
		add("Latest attempt", a.Agent+" · "+PhaseLabel(a.Phase)+fmt.Sprintf(" · %d repairs", a.Repairs))
		if a.LastOutput != "" {
			lines = append(lines, "Last output: "+ageLabel(a.LastOutput, m.referenceTime())+" ago")
		}
		if a.ProcessState != "" {
			lines = append(lines, "Process: "+a.ProcessState)
		}
		add("Retained work", a.Worktree)
	}
	add("Result", d.Summary)
	add("Changed files", strings.Join(d.Files, ", "))
	add("Verification command", d.VerifyCommand)
	if len(d.Checks) > 0 {
		lines = append(lines, sectionStyle.Render("Recorded checks"))
		for _, c := range d.Checks {
			stamp := c.Time
			if at, err := time.Parse(time.RFC3339Nano, c.Time); err == nil {
				stamp = at.Local().Format("Jan 02 15:04:05")
			}
			lines = append(lines, plainText(stamp+" · "+c.Kind+" · "+c.Result))
			if c.Artifact != "" {
				lines = append(lines, plainText(c.Artifact))
			}
		}
		lines = append(lines, "")
	} else if !m.workspace.loading && m.workspace.detail != nil {
		lines = append(lines, "No recorded checks for this task.", "")
	}
	add("Failure evidence", d.Feedback)
	if len(d.Attempts) > 1 {
		lines = append(lines, sectionStyle.Render("Attempt history"))
		for _, a := range d.Attempts {
			lines = append(lines, plainText(a.ID+" · "+a.Agent+" · "+PhaseLabel(a.Phase)))
		}
	}
	return ansi.Hardwrap(strings.Join(lines, "\n"), max(1, width), true)
}

func (m *model) refreshInspector() {
	m.initWorkspace()
	m.workspace.inspector.Width = max(1, m.width)
	m.workspace.inspector.Height = max(1, m.height-lipgloss.Height(m.header())-lipgloss.Height(m.workspaceFooter())-1)
	m.workspace.inspector.SetContent(m.inspectorContent(m.width))
}

func (m model) inspectorScreen() string {
	m.refreshInspector()
	return m.fixedScreen(m.header(), m.workspace.inspector.View(), m.workspaceFooter())
}

func (m model) workspaceViewportScreen(view viewport.Model, title string) string {
	view.Width = m.width
	view.Height = max(1, m.height-lipgloss.Height(m.header())-lipgloss.Height(m.workspaceFooter())-3)
	return m.fixedScreen(m.header(), truncate(title, m.width)+"\n"+view.View(), m.workspaceFooter())
}

func (m *model) pauseActivity() {
	if !m.workspace.tailPaused {
		m.workspace.tailNew = 0
	}
	m.workspace.tailPaused = true
}
func (m *model) followActivity() {
	m.workspace.tailPaused = false
	m.workspace.tailNew = 0
	m.tailView.SetContent(lipgloss.NewStyle().Width(m.width).Render(m.eventText()))
	m.tailView.GotoBottom()
}
func (m model) activityScreen() string {
	if !m.workspace.tailPaused {
		m.tailView.SetContent(lipgloss.NewStyle().Width(m.width).Render(m.eventText()))
	}
	footer := m.workspaceFooter()
	follow := "Following live"
	if m.workspace.tailPaused {
		follow = "Paused · " + countLabel(m.workspace.tailNew, "new event") + " · [end] Follow live"
	}
	m.tailView.Height = max(1, m.height-lipgloss.Height(m.header())-lipgloss.Height(footer)-3)
	m.tailView.Width = m.width
	return m.fixedScreen(m.header(), truncate(follow, m.width)+"\n"+m.tailView.View(), footer)
}

const workspaceHelp = `Workspace
  [b]             overview: state, attention, workers, outcomes
  [u]             all tasks, including completed and blocked work
  [p]             project statistics: time, tokens, cost, daily effort
  [enter]         inspect the selected task
  [/]             search task identifiers, titles, and states
  [f]             filter all, active, blocked, waiting, or done tasks
  [r]             requeue a blocked task after confirming its blocker is resolved
  [esc]           return from the inspector; finish search
  [tab]           cycle overview, tasks, activity, and statistics
  [end]           follow live activity after scrolling

Output age is evidence of silence, not a verdict that a worker is stuck.
Task completion counts can change when the backlog gains new tasks.
Requeue requests apply only while idle and retain the task's existing contract.`
