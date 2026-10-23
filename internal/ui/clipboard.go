package ui

import (
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type clipboardResult struct{ err error }

// Serialize terminal output through Bubble Tea so the renderer cannot split
// an OSC 52 sequence. The terminal owns the clipboard, including remote use.
type clipboardCommand struct {
	text string
	out  io.Writer
}

func (c *clipboardCommand) SetStdin(io.Reader)    {}
func (c *clipboardCommand) SetStderr(io.Writer)   {}
func (c *clipboardCommand) SetStdout(w io.Writer) { c.out = w }
func (c *clipboardCommand) Run() error {
	_, err := io.WriteString(c.out, osc52.New(c.text).String())
	return err
}
func copyToClipboard(text string) tea.Cmd {
	return tea.Exec(&clipboardCommand{text: text}, func(err error) tea.Msg { return clipboardResult{err} })
}

// Copy data, not a rendered viewport: terminal width must not lose evidence.
func (m model) copyText() string {
	var b strings.Builder
	b.WriteString("Diagnose this Looper run and fix issues within the existing task scope.\n")
	b.WriteString("Read the repository instructions and verify live state before making changes.\n")
	b.WriteString("Preserve active work and required acceptance criteria. Report the cause, changes, and verification.\n\n")
	if m.snap != nil {
		v := m.snap
		if v.Root != "" {
			fmt.Fprintf(&b, "Project root: %s\n", v.Root)
		} else {
			b.WriteString("Project root: unavailable in this snapshot\n")
		}
		fmt.Fprintf(&b, "Event cursor: %d\n", v.EventCursor)
		fmt.Fprintf(&b, "Looper · %s\nSession: %s\nSnapshot: %s\nGoal: %s\nOperation: %s\n%d/%d complete · %d active · %d blocked\n", v.Project, v.Session, v.Now, v.Goal, activityLabel(v, false), v.Counters.Done, v.Counters.Total, v.Counters.InFlight, v.Counters.Blocked)
		if m.staleView || v.Stale {
			b.WriteString("Snapshot is stale; current controller state is unknown.\n")
		}
		if v.ActivityDetail != "" {
			fmt.Fprintln(&b, v.ActivityDetail)
		}
	} else {
		b.WriteString("Looper: no current snapshot\n")
	}
	switch m.mode {
	case ModeLog:
		fmt.Fprintf(&b, "\nLog: %s\n%s", m.logPath, handoffExcerpt(m.logText, true))
	case ModeDiff:
		fmt.Fprintf(&b, "\nDiff: %s\n%s", m.diffFor, handoffExcerpt(m.diffText, false))
	case ModeTail:
		events := m.visibleEvents(m.showHistory)
		for _, e := range events[max(0, len(events)-8):] {
			fmt.Fprintf(&b, "\n%d %s [%s] %s", e.ID, e.Time, e.Kind, handoffExcerpt(e.Message, false))
		}
	case ModeTasks, ModeInspect:
		if t := m.workspaceTask(); t != nil {
			fmt.Fprintf(&b, "\n%s · %s\nState: %s\n", t.ID, t.Title, taskPhase(*t))
			details := t.Details
			if d := m.workspace.detail; d != nil && d.Task.ID == t.ID && d.Task.Revision == t.Revision {
				details = d.Task.Details
			}
			if details != "" {
				fmt.Fprintf(&b, "Purpose: %s\n", details)
			}
			if len(t.WaitingOn) > 0 {
				fmt.Fprintf(&b, "Waiting on: %s\n", strings.Join(t.WaitingOn, ", "))
			}
			for _, reason := range t.Blockers {
				fmt.Fprintf(&b, "Blocker: %s\n", reason)
			}
			if t.UnblockWhen != "" {
				fmt.Fprintf(&b, "Resume when: %s\n", t.UnblockWhen)
			}
		}
	default:
		if m.snap != nil {
			for _, a := range m.snap.Attempts {
				fmt.Fprintf(&b, "\n%s · %s · %s", a.TaskID, a.Title, PhaseLabel(a.Phase))
			}
			for _, t := range m.snap.Ready {
				fmt.Fprintf(&b, "\nReady: %s · %s", t.ID, t.Title)
			}
			for _, t := range m.snap.Blocked {
				fmt.Fprintf(&b, "\n%s · %s\nBlockers: %s\nResume when: %s\n", t.ID, t.Title, strings.Join(t.Reasons, "; "), t.UnblockWhen)
			}
		}
	}

	if m.snap != nil && (m.mode == ModeTasks || m.mode == ModeInspect) {
		if t := m.workspaceTask(); t != nil && t.Attempt != nil {
			a := t.Attempt
			fmt.Fprintf(&b, "\nAttempt: %s\nPhase: %s\nWorktree: %s\nLogs: %s\n", a.ID, a.Phase, a.Worktree, a.LogDir)
		}
	}
	if m.mode != ModeTail {
		events := m.visibleEvents(false)
		if len(events) > 0 {
			b.WriteString("\nRecent events (up to 8):\n")
		}
		for _, e := range events[max(0, len(events)-8):] {
			fmt.Fprintf(&b, "%d %s [%s] %s\n", e.ID, e.Time, e.Kind, handoffExcerpt(e.Message, false))
		}
	}
	b.WriteString("\nVerify from the project root:\nlooper status --json\nlooper doctor --json\n")
	if m.mode == ModeTasks || m.mode == ModeInspect {
		if t := m.workspaceTask(); t != nil {
			fmt.Fprintf(&b, "looper inspect '%s' --json\n", strings.ReplaceAll(t.ID, "'", "'\"'\"'"))
		}
	}
	if m.snap != nil {
		fmt.Fprintf(&b, "looper events --json --after %d\n", m.snap.EventCursor)
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, ansi.Strip(b.String()))
}

// Bound large evidence while keeping the task contract and blockers complete.
func handoffExcerpt(text string, tail bool) string {
	const limit = 12000
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if tail {
		return "[Earlier content omitted; latest excerpt follows]\n" + string(runes[len(runes)-limit:])
	}
	return string(runes[:limit]) + "\n[Remaining content omitted; inspect the full artifact locally]"
}
