package ui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Dashboard modes and exit reasons.
const (
	ModeBoard = iota
	ModeTail
	ModeDiff
	ModeHelp
	ModeLog
)

// ExitAction reports why the dashboard stopped. The wrapper may run a form
// and restart the dashboard for form actions.
type ExitAction struct {
	Kind string // "", "settings", "add-plan"
}

// Controls connects the dashboard to a controller without importing the
// engine package. Controlling mode supplies Snapshot; observer mode supplies
// Poll. Callbacks return errors for display; they never crash the view.
type Controls struct {
	Snapshot <-chan ViewSnapshot
	Poll     func() *ViewSnapshot
	Events   func(after int64, limit int) []JournalEvent
	Drain    func() error
	Diff     func(attemptID string) (string, error)
	Log      func(path string) (string, error)
	AddPlan  func(path string) error
	// Done reports that the local controller exited (controlling mode).
	Done func() bool
	// Cancel stops the local controller with retained work (controlling mode).
	Cancel   func()
	ReadOnly bool
	Session  string
	// Notice shows in the message area on start (for example a settings
	// acknowledgement after the form flow).
	Notice string
}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	busyStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	blockedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	selectStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("8"))
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	sectionStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("7"))
	dialogStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(1, 2)
	attemptStyles = map[string]lipgloss.Style{
		"running": busyStyle, "verifying": busyStyle, "ready": dimStyle,
		"resume": busyStyle, "integrated": busyStyle,
	}
)

type tickMsg struct{}

type model struct {
	ctrl       Controls
	snap       *ViewSnapshot
	staleView  bool
	events     []JournalEvent
	cursor     int64
	mode       int
	width      int
	height     int
	selected   string // attempt ID, preserved across row changes
	scroll     int
	diffText   string
	diffFor    string
	diffView   viewport.Model
	message    string
	err        string
	dialog     string // "", "drain", "exit", "add-plan"
	drainAsked bool
	planPath   string
	tailView   viewport.Model
	logPath    string
	logText    string
}

func RunDashboard(c Controls) (ExitAction, error) {
	m := model{ctrl: c, mode: ModeBoard, cursor: -1, message: c.Notice, diffView: viewport.New(0, 0), tailView: viewport.New(0, 0)}
	if c.Snapshot == nil && c.Poll == nil {
		return ExitAction{}, fmt.Errorf("dashboard needs a snapshot source")
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	out, err := p.Run()
	if err != nil {
		return ExitAction{}, err
	}
	finished, ok := out.(model)
	if !ok {
		return ExitAction{}, nil
	}
	switch finished.dialog {
	case "settings":
		return ExitAction{Kind: "settings"}, nil
	case "add-plan":
		return ExitAction{Kind: "add-plan"}, nil
	}
	return ExitAction{}, nil
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.waitSnapshot(), tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} }))
}

func (m model) waitSnapshot() tea.Cmd {
	if m.ctrl.Snapshot != nil {
		ch := m.ctrl.Snapshot
		return func() tea.Msg { return <-ch }
	}
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.sizeViewports()
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.diffView.Width, m.tailView.Width = msg.Width, msg.Width
		m.sizeViewports()
		if m.mode == ModeDiff {
			m.diffView.SetContent(lipgloss.NewStyle().Width(m.width).Render(m.diffText))
		}
		if m.mode == ModeHelp {
			m.tailView.SetContent(lipgloss.NewStyle().Width(m.width).Render(helpText))
		}
		if m.mode == ModeTail {
			m.tailView.SetContent(lipgloss.NewStyle().Width(m.width).Render(m.eventText()))
		}
		if m.mode == ModeLog {
			m.refreshLog()
		}
		return m, nil
	case ViewSnapshot:
		m.snap = &msg
		m.staleView = false
		return m, m.waitSnapshot()
	case tickMsg:
		if m.ctrl.Done != nil && m.ctrl.Done() {
			return m, tea.Quit
		}
		if m.ctrl.Poll != nil {
			if snap := m.ctrl.Poll(); snap != nil {
				m.snap = snap
				m.staleView = false
			} else {
				m.staleView = true
			}
		}
		if m.ctrl.Events != nil {
			if fresh := m.ctrl.Events(m.cursor, 200); len(fresh) > 0 {
				m.events = append(m.events, fresh...)
				m.cursor = fresh[len(fresh)-1].ID
				if len(m.events) > 500 {
					m.events = m.events[len(m.events)-500:]
				}
			}
		}
		if m.mode == ModeTail {
			bottom := m.tailView.AtBottom()
			m.tailView.SetContent(lipgloss.NewStyle().Width(m.width).Render(m.eventText()))
			if bottom {
				m.tailView.GotoBottom()
			}
		}
		if m.mode == ModeLog {
			m.refreshLog()
		}
		return m, tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
	case tea.KeyMsg:
		return m.keys(msg)
	}
	return m, nil
}

func (m model) keys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.dialog != "" {
		return m.dialogKeys(msg)
	}
	switch msg.String() {
	case "ctrl+c":
		if m.ctrl.ReadOnly || m.ctrl.Snapshot == nil {
			return m, tea.Quit
		}
		// Controlling mode keeps interrupt-and-retain semantics.
		m.message = "Interrupted. Work is retained."
		if m.ctrl.Cancel != nil {
			m.ctrl.Cancel()
		}
		return m, tea.Quit
	case "q":
		if m.ctrl.ReadOnly || m.ctrl.Snapshot == nil {
			return m, tea.Quit // observers detach; work continues
		}
		m.dialog = "exit"
		return m, nil
	case "x":
		if m.ctrl.ReadOnly {
			m.err = "Read-only observers cannot drain work."
			return m, nil
		}
		m.dialog = "drain"
		return m, nil
	case "s":
		if m.ctrl.ReadOnly {
			m.err = "Read-only observers cannot change settings."
			return m, nil
		}
		m.dialog = "settings"
		return m, nil
	case "a":
		if m.ctrl.ReadOnly {
			m.err = "Read-only observers cannot import plans."
			return m, nil
		}
		m.dialog = "add-plan"
		m.planPath = ""
		return m, nil
	case "d":
		if sel := m.selectedAttempt(); sel != "" {
			text, err := m.ctrl.Diff(sel)
			if err != nil {
				m.err = err.Error()
				return m, nil
			}
			m.diffFor, m.mode, m.err = sel, ModeDiff, ""
			m.diffText = text
			m.diffView.SetContent(lipgloss.NewStyle().Width(m.width).Render(text))
			m.diffView.GotoTop()
			return m, nil
		}
	case "l", "L":
		if m.snap != nil && m.snap.ActivityLog != "" && m.ctrl.Log != nil {
			m.mode, m.logPath = ModeLog, m.snap.ActivityLog
			m.refreshLog()
			m.tailView.GotoBottom()
		} else {
			m.message = "No planner log is available."
		}
		return m, nil
	case "t":
		m.mode = ModeTail
		m.tailView.SetContent(lipgloss.NewStyle().Width(m.width).Render(m.eventText()))
		m.tailView.GotoBottom()
		return m, nil
	case "b":
		m.mode = ModeBoard
		return m, nil
	case "?":
		if m.mode == ModeHelp {
			m.mode = ModeBoard
		} else {
			m.mode = ModeHelp
			m.tailView.SetContent(lipgloss.NewStyle().Width(m.width).Render(helpText))
			m.tailView.GotoTop()
		}
		return m, nil
	case "esc":
		m.mode, m.err, m.message = ModeBoard, "", ""
		return m, nil
	case "up", "k":
		if m.mode == ModeDiff {
			m.diffView.LineUp(1)
		} else if m.mode == ModeTail || m.mode == ModeLog || m.mode == ModeHelp {
			m.tailView.LineUp(1)
		} else {
			m.moveSelection(-1)
			m.scroll = max(0, m.scroll-1)
		}
		return m, nil
	case "down", "j":
		if m.mode == ModeDiff {
			m.diffView.LineDown(1)
		} else if m.mode == ModeTail || m.mode == ModeLog || m.mode == ModeHelp {
			m.tailView.LineDown(1)
		} else {
			m.moveSelection(1)
			m.scroll = min(m.maxBoardScroll(), m.scroll+1)
		}
		return m, nil
	case "pgup":
		if m.mode == ModeBoard {
			m.scroll = max(0, m.scroll-max(1, m.height/2))
		} else if m.mode == ModeDiff {
			m.diffView.HalfViewUp()
		} else {
			m.tailView.HalfViewUp()
		}
	case "pgdown":
		if m.mode == ModeBoard {
			m.scroll = min(m.maxBoardScroll(), m.scroll+max(1, m.height/2))
		} else if m.mode == ModeDiff {
			m.diffView.HalfViewDown()
		} else {
			m.tailView.HalfViewDown()
		}
	}
	return m, nil
}

func (m model) dialogKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.dialog {
	case "exit":
		switch msg.String() {
		case "d":
			if m.ctrl.Drain != nil {
				if err := m.ctrl.Drain(); err != nil {
					m.err, m.dialog = err.Error(), ""
					return m, nil
				}
				m.drainAsked, m.dialog = true, ""
				m.message = "Drain requested. Waiting for active work..."
			}
			return m, nil
		case "c":
			if m.ctrl.Cancel != nil {
				m.ctrl.Cancel()
			}
			return m, tea.Quit
		case "esc", "q":
			m.dialog = ""
		}
		return m, nil
	case "drain":
		switch msg.String() {
		case "y", "enter":
			if m.ctrl.Drain != nil {
				if err := m.ctrl.Drain(); err != nil {
					m.err = err.Error()
				} else {
					m.drainAsked, m.message = true, "Drain requested. Active work finishes first."
				}
			}
			m.dialog = ""
			return m, nil
		default:
			m.dialog = ""
			return m, nil
		}
	case "settings":
		// The wrapper suspends the dashboard, runs the form, and restarts.
		return m, tea.Quit
	case "add-plan":
		switch {
		case msg.String() == "enter":
			path := strings.TrimSpace(m.planPath)
			if path == "" {
				m.dialog, m.err = "", "a plan file is required"
				return m, nil
			}
			if m.ctrl.AddPlan == nil {
				m.dialog, m.err = "", "plan import is not available in this build"
				return m, nil
			}
			if err := m.ctrl.AddPlan(path); err != nil {
				m.err = err.Error()
			} else {
				m.message = "Plan queued: " + path
			}
			m.dialog = ""
			return m, nil
		case msg.String() == "esc":
			m.dialog = ""
			return m, nil
		case msg.String() == "backspace":
			if len(m.planPath) > 0 {
				m.planPath = m.planPath[:len(m.planPath)-1]
			}
			return m, nil
		case len(msg.String()) == 1:
			m.planPath += msg.String()
			return m, nil
		}
	}
	return m, nil
}

func (m *model) moveSelection(delta int) {
	if m.snap == nil || len(m.snap.Attempts) == 0 {
		m.selected = ""
		return
	}
	index := 0
	for i, a := range m.snap.Attempts {
		if a.ID == m.selected {
			index = i
		}
	}
	index = max(0, min(len(m.snap.Attempts)-1, index+delta))
	m.selected = m.snap.Attempts[index].ID
}

func (m model) selectedAttempt() string {
	if m.selected == "" && m.snap != nil && len(m.snap.Attempts) > 0 {
		return m.snap.Attempts[0].ID
	}
	return m.selected
}

// StatusLabel reports controller state separately from its current activity.
// Text labels remain useful when the terminal disables color.
func StatusLabel(v *ViewSnapshot, stale bool) (string, lipgloss.Style) {
	if stale || (v != nil && v.Stale) {
		return "STOPPED", dimStyle.Bold(true)
	}
	if v == nil {
		return "CONNECTING", blockedStyle.Bold(true)
	}
	if v.Draining {
		return "STOPPING", blockedStyle.Bold(true)
	}
	working := len(v.Attempts) > 0 || v.Counters.InFlight+v.Counters.Checking > 0
	if v.State != "" || working {
		return "RUNNING", okStyle.Bold(true)
	}
	if v.Finished && v.Counters.Ready == 0 {
		return "COMPLETE", okStyle.Bold(true)
	}
	if v.Counters.Ready == 0 && v.Counters.Blocked > 0 {
		return "BLOCKED", errorStyle.Bold(true)
	}
	if v.Counters.Ready == 0 {
		return "WAITING", blockedStyle.Bold(true)
	}
	return "RUNNING", okStyle.Bold(true)
}

func activityLabel(v *ViewSnapshot, stale bool) string {
	if v == nil || stale || v.Stale {
		return ""
	}
	if v.Draining {
		return "Finishing active work"
	}
	if v.State == "planning" {
		return "PLANNING"
	}
	if v.State == "reviewing" {
		return "REVIEWING"
	}
	if v.Counters.InFlight > 0 && v.Counters.Checking > 0 {
		return "IMPLEMENTING / CHECKING"
	}
	if v.Counters.Checking > 0 {
		return "CHECKING"
	}
	if v.Counters.InFlight > 0 {
		return "IMPLEMENTING"
	}
	if len(v.Attempts) > 0 {
		return strings.ToUpper(PhaseLabel(v.Attempts[0].Phase))
	}
	if v.Finished && v.Counters.Ready == 0 {
		return "Waiting for new work"
	}
	if v.Counters.Ready == 0 && v.Counters.Blocked > 0 {
		return "Needs attention"
	}
	if v.Counters.Ready == 0 {
		return "No ready tasks"
	}
	return "SCHEDULING"
}

func (m model) View() string {
	if m.width == 0 {
		return "starting..."
	}
	if m.dialog != "" {
		return m.dialogView()
	}
	switch m.mode {
	case ModeHelp:
		return m.helpView()
	case ModeDiff:
		return m.diffScreen()
	case ModeTail:
		return m.tailScreen()
	case ModeLog:
		return m.logScreen()
	default:
		return m.boardScreen()
	}
}

func (m model) header() string {
	label, style := StatusLabel(m.snap, m.staleView)
	role := "OBSERVING"
	if m.ctrl.Snapshot != nil {
		role = "CONTROLLING"
	}
	parts := []string{titleStyle.Render("LOOPER") + "  " + style.Render(label)}
	if activity := activityLabel(m.snap, m.staleView); activity != "" {
		parts = append(parts, busyStyle.Render(activity))
	}
	if m.snap != nil {
		parts = append(parts, dimStyle.Render(fmt.Sprintf("%s elapsed", m.elapsed())), dimStyle.Render(fmt.Sprintf("%d/%d complete", m.snap.Counters.Done, m.snap.Counters.Total)))
		if m.snap.Counters.InFlight+m.snap.Counters.Checking > 0 {
			parts = append(parts, busyStyle.Render(fmt.Sprintf("%d implementing · %d checking", m.snap.Counters.InFlight, m.snap.Counters.Checking)))
		}
	}
	if m.snap != nil && m.snap.State == "planning" && m.snap.ActivityLog != "" {
		output := "No agent output yet"
		if at, err := time.Parse(time.RFC3339, m.snap.ActivityUpdated); err == nil {
			output = fmt.Sprintf("Last output %s ago", max(time.Duration(0), time.Since(at)).Round(time.Second))
		}
		parts = append(parts, dimStyle.Render(output))
	}
	parts = append(parts, dimStyle.Render(role))
	return wrapItems(parts, "   ", m.width)
}

// Wrap at item boundaries so key labels and counters remain together.
func wrapItems(items []string, separator string, width int) string {
	var lines []string
	line := ""
	for _, item := range items {
		if line != "" && lipgloss.Width(line+separator+item) > width {
			lines = append(lines, line)
			line = ""
		}
		if line != "" {
			line += separator
		}
		line += item
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(lines, "\n"))
}

func (m model) elapsed() string {
	v := *m.snap
	if !m.staleView && !v.Stale {
		v.Now = time.Now().UTC().Format(time.RFC3339)
	}
	return elapsed(&v)
}

func elapsed(v *ViewSnapshot) string {
	start, err := time.Parse(time.RFC3339, v.Started)
	if err != nil {
		return "unknown"
	}
	ref := time.Now()
	if now, err := time.Parse(time.RFC3339, v.Now); err == nil {
		ref = now
	}
	d := max(time.Duration(0), ref.Sub(start))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func (m model) boardScreen() string { return m.fitBoard(m.boardContent()) }

func (m model) boardContent() string {
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n\n")
	if m.snap == nil {
		b.WriteString(dimStyle.Render("No snapshot yet. Waiting for the controller...\n"))
		return b.String()
	}
	if m.snap.State == "planning" {
		b.WriteString(busyStyle.Render("Latest: "+m.snap.ActivityDetail) + "\n")
		b.WriteString(dimStyle.Render("Planning task scopes and checks.") + "\n\n")
	}
	if len(m.snap.Attempts) > 0 || m.staleView {
		b.WriteString(sectionStyle.Render("Workers"))
		b.WriteString("\n")
		for _, a := range m.snap.Attempts {
			phase := PhaseLabel(a.Phase)
			line := fmt.Sprintf("%s %s  %s", a.TaskID, phase, a.Title)
			if a.Repairs > 0 {
				line += fmt.Sprintf("repair %d", a.Repairs)
			}
			if a.ID == m.selectedAttempt() {
				b.WriteString(selectStyle.Render(" " + line))
			} else {
				style := dimStyle
				if s, ok := attemptStyles[a.Phase]; ok {
					style = s
				}
				b.WriteString(" " + style.Render(line))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if len(m.snap.Ready) > 0 {
		b.WriteString(sectionStyle.Render("Ready"))
		b.WriteString("\n")
		titles := make([]string, 0, len(m.snap.Ready))
		for _, t := range m.snap.Ready {
			titles = append(titles, t.ID+" "+t.Title)
		}
		b.WriteString(dimStyle.Render("  " + strings.Join(titles, "\n  ")))
		b.WriteString("\n\n")
	}
	if len(m.snap.Blocked) > 0 {
		b.WriteString(blockedStyle.Render("Blocked"))
		b.WriteString("\n")
		for _, t := range m.snap.Blocked {
			reason := "blocked"
			if len(t.Reasons) > 0 {
				reason = strings.Join(t.Reasons, "; ")
			}
			b.WriteString(blockedStyle.Render(fmt.Sprintf("  %s — %s\n", t.ID, reason)))
		}
		b.WriteString("\n")
	}
	b.WriteString(sectionStyle.Render("Events"))
	b.WriteString("\n")
	b.WriteString(m.eventTail(5))
	b.WriteString("\n")
	if m.err != "" {
		b.WriteString(errorStyle.Render("! "+truncate(m.err, m.width-2)) + "\n")
	} else if m.message != "" {
		b.WriteString(okStyle.Render(truncate(m.message, m.width-2)) + "\n")
	}
	return b.String()
}

func (m model) eventTail(n int) string {
	if len(m.events) == 0 {
		return dimStyle.Render("  no events yet\n")
	}
	start := max(0, len(m.events)-n)
	var b strings.Builder
	for _, e := range m.events[start:] {
		stamp := ""
		if t, err := time.Parse(time.RFC3339Nano, e.Time); err == nil {
			stamp = t.Local().Format("15:04:05")
		}
		b.WriteString(dimStyle.Render("  "+stamp+" ") + truncate(e.Message, m.width-12) + "\n")
	}
	return b.String()
}

func (m model) eventText() string {
	var b strings.Builder
	for _, e := range m.events {
		stamp := e.Time
		if t, err := time.Parse(time.RFC3339Nano, e.Time); err == nil {
			stamp = t.Local().Format("15:04:05")
		}
		b.WriteString(stamp + "  " + e.Message + "\n")
	}
	return b.String()
}

func (m model) footer() string {
	keys := "j/k scroll · a add plan · s settings · t tail · [L] logs · d diff · x drain · ? help · q exit"
	if m.ctrl.ReadOnly {
		keys = "j/k scroll · t tail · [L] logs · d diff · ? help · q detach (read-only)"
	}
	if m.height < 14 {
		keys = "j/k scroll · [L] logs · ? help · q exit"
	}
	return dimStyle.Render(wrapItems(strings.Split(keys, " · "), " · ", m.width))
}

func (m model) tailScreen() string {
	m.tailView.SetContent(lipgloss.NewStyle().Width(m.width).Render(m.eventText()))
	footer := wrapItems([]string{"b board", "j/k scroll", "? help"}, " · ", m.width)
	m.tailView.Height = max(1, m.height-lipgloss.Height(m.header())-lipgloss.Height(footer)-3)
	return m.header() + "\n\n" + m.tailView.View() + "\n" + dimStyle.Render(footer)
}

func (m model) diffScreen() string {
	footer := wrapItems([]string{"diff " + m.diffFor, "j/k scroll", "esc back"}, " · ", m.width)
	m.diffView.Height = max(1, m.height-lipgloss.Height(m.header())-lipgloss.Height(footer)-3)
	return m.header() + "\n\n" + m.diffView.View() + "\n" + dimStyle.Render(footer)
}

const helpText = `Run state
  RUNNING    controller is working (green)
  WAITING    no ready tasks (yellow)
  STOPPING   finishing active work (yellow)
  BLOCKED    needs attention (red)
  COMPLETE   goal met; waiting for new work (green)
  STOPPED    no active controller (gray)

RUNNING confirms the controller is active. It does not prove agent progress.

keys
  j / k, arrows     move selection or scroll
  d                 diff of the selected attempt's worktree
  t / b             tail and board views
  l / L             planner activity log (refreshes each second)
  x                 request a drain (active work finishes)
  s                 settings (controlling mode)
  a                 add a plan file (controlling mode)
  q                 exit (observers detach; the controller keeps running)
  ctrl+c            observers detach; controlling mode interrupts and retains work
`

func (m model) helpView() string {
	m.tailView.SetContent(lipgloss.NewStyle().Width(m.width).Render(helpText))
	footer := wrapItems([]string{"b board", "j/k scroll", "esc back"}, " · ", m.width)
	m.tailView.Height = max(1, m.height-lipgloss.Height(m.header())-lipgloss.Height(footer)-3)
	return m.header() + "\n\n" + m.tailView.View() + "\n" + dimStyle.Render(footer)
}

func (m model) dialogView() string {
	switch m.dialog {
	case "exit":
		return m.dialogBox("Exit controller?\n\n d  drain, then exit\n c  interrupt; retain work\n esc  stay")
	case "drain":
		return m.dialogBox("Drain work?\n\n y  finish active work\n any other key  cancel")
	case "add-plan":
		return m.dialogBox("Add plan\n\n path: " + truncate(m.planPath, max(1, m.width-14)) + "_\n\n enter submit · esc cancel")
	}
	return ""
}

func (m model) dialogBox(text string) string {
	return dialogStyle.Width(max(1, m.width-2)).Render(text)
}

func truncate(s string, width int) string {
	return ansi.Truncate(s, max(1, width), "…")
}

// Keep controls visible even when a phone keyboard reduces terminal height.
func (m model) boardLines(content string) ([]string, []string, int) {
	wrapped := lipgloss.NewStyle().Width(m.width).Render(strings.TrimRight(content, "\n"))
	lines := strings.Split(wrapped, "\n")
	headerSize := min(len(lines), lipgloss.Height(m.header())+1)
	available := max(1, m.height-lipgloss.Height(m.footer())-headerSize-1)
	return lines[:headerSize], lines[headerSize:], available
}

func (m model) maxBoardScroll() int {
	_, body, available := m.boardLines(m.boardContent())
	return max(0, len(body)-available)
}

func (m model) fitBoard(content string) string {
	header, body, available := m.boardLines(content)
	offset := min(m.scroll, max(0, len(body)-available))
	end := min(len(body), offset+available)
	lines := append(header[:len(header):len(header)], body[offset:end]...)
	return strings.Join(lines, "\n") + "\n\n" + m.footer()
}

func (m *model) refreshLog() {
	if m.ctrl.Log == nil {
		return
	}
	if m.snap != nil && m.snap.ActivityLog != "" {
		m.logPath = m.snap.ActivityLog
	}
	text, err := m.ctrl.Log(m.logPath)
	if err != nil {
		m.logText = err.Error()
	} else {
		m.logText = text
	}
	if m.logText == "" {
		m.logText = "No output yet. The agent may buffer output until it finishes."
	}
	m.sizeViewports()
	bottom := m.tailView.AtBottom()
	m.tailView.SetContent(lipgloss.NewStyle().Width(m.width).Render(m.logText))
	if bottom {
		m.tailView.GotoBottom()
	}
}

func (m model) logHeader() string {
	return m.header() + "\n" + wrapItems([]string{"Planner log", filepath.Base(m.logPath)}, " · ", m.width)
}

func (m model) logScreen() string {
	header := m.logHeader()
	footer := wrapItems([]string{"b board", "j/k scroll", "esc back"}, " · ", m.width)
	m.tailView.Height = max(1, m.height-lipgloss.Height(header)-lipgloss.Height(footer)-2)
	return header + "\n" + m.tailView.View() + "\n" + dimStyle.Render(footer)
}

// Store the same viewport size used for rendering so scrolling reaches the end.
func (m *model) sizeViewports() {
	if m.width <= 0 {
		return
	}
	footer := wrapItems([]string{"b board", "j/k scroll", "esc back"}, " · ", m.width)
	m.tailView.Height = max(1, m.height-lipgloss.Height(m.header())-lipgloss.Height(footer)-3)
	diffFooter := wrapItems([]string{"diff " + m.diffFor, "j/k scroll", "esc back"}, " · ", m.width)
	m.diffView.Height = max(1, m.height-lipgloss.Height(m.header())-lipgloss.Height(diffFooter)-3)
	if m.mode == ModeLog {
		m.tailView.Height = max(1, m.height-lipgloss.Height(m.logHeader())-lipgloss.Height(footer)-2)
	}
}
