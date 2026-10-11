package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Dashboard modes and exit reasons.
const (
	ModeBoard = iota
	ModeTail
	ModeDiff
	ModeHelp
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
	diffFor    string
	diffView   viewport.Model
	message    string
	err        string
	dialog     string // "", "drain", "exit", "add-plan"
	drainAsked bool
	planPath   string
	tailView   viewport.Model
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
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.diffView.Width, m.tailView.Width = msg.Width, msg.Width
		m.diffView.Height = max(3, msg.Height-6)
		m.tailView.Height = max(3, msg.Height-6)
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
			} else if m.snap == nil {
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
			m.diffView.SetContent(text)
			m.diffView.GotoTop()
			return m, nil
		}
	case "t":
		m.mode = ModeTail
		m.tailView.SetContent(m.eventText())
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
		}
		return m, nil
	case "esc":
		m.mode, m.err, m.message = ModeBoard, "", ""
		return m, nil
	case "up", "k":
		if m.mode == ModeDiff {
			m.diffView.LineUp(1)
		} else if m.mode == ModeTail {
			m.tailView.LineUp(1)
		} else {
			m.moveSelection(-1)
		}
		return m, nil
	case "down", "j":
		if m.mode == ModeDiff {
			m.diffView.LineDown(1)
		} else if m.mode == ModeTail {
			m.tailView.LineDown(1)
		} else {
			m.moveSelection(1)
		}
		return m, nil
	case "pgup":
		if m.mode == ModeDiff {
			m.diffView.HalfViewUp()
		} else {
			m.tailView.HalfViewUp()
		}
	case "pgdown":
		if m.mode == ModeDiff {
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

// StatusLabel distinguishes success, blocked work, idle waiting, drain, and
// error states. A success banner never appears merely because workers are
// idle.
func StatusLabel(v *ViewSnapshot, stale bool) (string, lipgloss.Style) {
	if stale {
		return "NO ACTIVE RUN", blockedStyle
	}
	if v == nil {
		return "CONNECTING", dimStyle
	}
	if v.Draining {
		return "DRAINING", busyStyle
	}
	working := len(v.Attempts) > 0
	if v.Finished && !working && v.Counters.Ready == 0 {
		return "GOAL SATISFIED — waiting for new requirements", okStyle
	}
	if !working && v.Counters.Ready == 0 && v.Counters.Blocked > 0 {
		return "BLOCKED — work needs attention", blockedStyle
	}
	if !working && v.Counters.Ready == 0 {
		return "WAITING FOR WORK", dimStyle
	}
	return "ACTIVE", busyStyle
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
	head := titleStyle.Render("LOOPER") + "  " + style.Render(label)
	if m.snap != nil {
		head += dimStyle.Render(fmt.Sprintf("   %s elapsed   %d/%d complete", elapsed(m.snap), m.snap.Counters.Done, m.snap.Counters.Total))
		if m.snap.Counters.InFlight+m.snap.Counters.Checking > 0 {
			head += busyStyle.Render(fmt.Sprintf("   %d implementing · %d checking", m.snap.Counters.InFlight, m.snap.Counters.Checking))
		}
	}
	head += dimStyle.Render("   " + role)
	return head
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
	d := ref.Sub(start)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func (m model) boardScreen() string {
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString("\n\n")
	if m.snap == nil {
		b.WriteString(dimStyle.Render("No snapshot yet. Waiting for the controller...\n"))
	}
	compact := m.height < 18
	if len(m.snap.Attempts) > 0 || m.staleView {
		b.WriteString(sectionStyle.Render("Workers"))
		b.WriteString("\n")
		visible := 4
		if compact {
			visible = 2
		}
		for i, a := range m.snap.Attempts {
			if i >= visible {
				b.WriteString(dimStyle.Render(fmt.Sprintf("  … %d more (j/k to move, t for tail)\n", len(m.snap.Attempts)-visible)))
				break
			}
			phase := PhaseLabel(a.Phase)
			line := fmt.Sprintf("%s  %-28s %-12s", a.TaskID, truncate(a.Title, 28), phase)
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
	if !compact && len(m.snap.Ready) > 0 {
		b.WriteString(sectionStyle.Render("Ready"))
		b.WriteString("\n")
		titles := make([]string, 0, len(m.snap.Ready))
		for i, t := range m.snap.Ready {
			if i >= 5 {
				titles = append(titles, fmt.Sprintf("… %d more", len(m.snap.Ready)-5))
				break
			}
			titles = append(titles, t.ID+" "+truncate(t.Title, 30))
		}
		b.WriteString(dimStyle.Render("  " + strings.Join(titles, " · ")))
		b.WriteString("\n\n")
	}
	if !compact && len(m.snap.Blocked) > 0 {
		b.WriteString(blockedStyle.Render("Blocked"))
		b.WriteString("\n")
		for i, t := range m.snap.Blocked {
			if i >= 3 {
				b.WriteString(blockedStyle.Render(fmt.Sprintf("  … %d more\n", len(m.snap.Blocked)-3)))
				break
			}
			reason := "blocked"
			if len(t.Reasons) > 0 {
				reason = strings.Join(t.Reasons, "; ")
			}
			b.WriteString(blockedStyle.Render(fmt.Sprintf("  %s — %s\n", t.ID, truncate(reason, m.width-8))))
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
	b.WriteString(m.footer())
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
	keys := "a add plan · s settings · t tail · d diff · x drain · ? help · q exit"
	if m.ctrl.ReadOnly {
		keys = "t tail · d diff · ? help · q detach (read-only)"
	}
	return dimStyle.Render(keys)
}

func (m model) tailScreen() string {
	m.tailView.SetContent(m.eventText())
	return m.header() + "\n\n" + m.tailView.View() + "\n" + dimStyle.Render("b board · j/k scroll · ? help")
}

func (m model) diffScreen() string {
	return m.header() + "\n\n" + m.diffView.View() + "\n" + dimStyle.Render(fmt.Sprintf("diff %s · j/k scroll · esc back", m.diffFor))
}

func (m model) helpView() string {
	text := `keys
  j / k, arrows     move selection or scroll
  d                 diff of the selected attempt's worktree
  t / b             tail and board views
  x                 request a drain (active work finishes)
  s                 settings (controlling mode)
  a                 add a plan file (controlling mode)
  q                 exit (observers detach; the controller keeps running)
  ctrl+c            observers detach; controlling mode interrupts and retains work
`
	return m.header() + "\n\n" + dimStyle.Render(text)
}

func (m model) dialogView() string {
	switch m.dialog {
	case "exit":
		return dialogStyle.Render("The controller runs in this terminal and cannot detach.\n\n  d  drain active work, then exit\n  c  cancel work and exit (work is retained)\n  esc  stay")
	case "drain":
		return dialogStyle.Render("Request a drain?\n\n  y  stop new dispatch; active work finishes\n  any other key  cancel")
	case "add-plan":
		return dialogStyle.Render("Add plan\n\n  path: " + m.planPath + "_\n\n  enter submit · esc cancel")
	}
	return ""
}

func truncate(s string, width int) string {
	if width <= 0 {
		return s
	}
	cells := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		cells += w
		if cells > width {
			return s[:max(0, i-1)] + "…"
		}
	}
	return s
}
