package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The animation opt-out must freeze the spinner without hiding any status:
// the bootstrap label, stream evidence, and stall reporting all remain.
func TestNoAnimationKeepsStatusReadableWithoutMotion(t *testing.T) {
	base := ViewSnapshot{
		Bootstrapping: true, Started: time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339),
		ActivityLog: "runtime/bootstrap-1/events-00.jsonl", BootstrapBytes: 24576, BootstrapIdleSeconds: 3,
	}
	m := model{width: 80, height: 24, snap: &base, ctrl: Controls{NoAnimation: true}}
	m.spin = spinner.New(spinner.WithSpinner(spinner.Dot))
	view := m.View()
	for _, want := range []string{"BOOTSTRAPPING", "Bootstrapping", "stream 24.0 KB", "last output 3s ago", "[l] open the agent log"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q: %s", want, view)
		}
	}
	if m.spinActive() {
		t.Fatal("the opt-out must freeze the spinner")
	}
	if !strings.Contains(m.bootstrapHeaderLine(), "•") {
		t.Fatalf("a static activity marker must replace the animated frames: %s", m.bootstrapHeaderLine())
	}
	// A growing stream still must not restart motion, and the poll tick keeps
	// updating evidence.
	base.BootstrapBytes, base.BootstrapIdleSeconds = 40960, 1
	next, _ := m.Update(tickMsg{})
	m = next.(model)
	if m.spinActive() {
		t.Fatal("stream growth restarted the spinner under the opt-out")
	}
	if !strings.Contains(m.View(), "stream 40.0 KB") {
		t.Fatalf("status stopped updating without animation: %s", m.View())
	}
	// A spinner tick addressed to this spinner must not advance the frame.
	frame := m.spin.View()
	tick, ok := m.spin.Tick().(spinner.TickMsg)
	if !ok {
		t.Fatalf("the spinner produced %T instead of a tick", m.spin.Tick())
	}
	next, cmd := m.Update(tick)
	stopped := next.(model)
	if cmd != nil {
		t.Fatal("the opt-out must not schedule further spinner ticks")
	}
	if stopped.spin.View() != frame {
		t.Fatal("the spinner advanced under the opt-out")
	}
	// Stall reporting survives without animation: text replaces the glyph.
	stalled := base
	stalled.BootstrapIdleSeconds = 250
	m.snap = &stalled
	if !strings.Contains(m.View(), "no agent output for 4m10s") {
		t.Fatalf("stall evidence missing without animation: %s", m.View())
	}
}

// Controls keep their behavior with animation disabled: views still switch,
// settings still hands off to the form, and observers still refuse actions.
func TestNoAnimationKeepsControls(t *testing.T) {
	m := model{width: 80, height: 24, ctrl: Controls{NoAnimation: true, Snapshot: make(chan ViewSnapshot)}}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	if next.(model).mode != ModeTail || cmd != nil {
		t.Fatalf("events view did not open: mode=%d cmd=%v", next.(model).mode, cmd)
	}
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	m = next.(model)
	if m.dialog != "settings" || cmd == nil {
		t.Fatalf("settings handoff missing without animation: dialog=%q cmd=%v", m.dialog, cmd)
	}
	observer := model{width: 80, height: 24, ctrl: Controls{NoAnimation: true, ReadOnly: true}}
	next, cmd = observer.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	m = next.(model)
	if cmd != nil || m.dialog != "" || m.err != "Read-only observers cannot drain work." {
		t.Fatalf("read-only rejection changed without animation: dialog=%q error=%q cmd=%v", m.dialog, m.err, cmd)
	}
	// The static marker keeps the spinner's placement on a small screen.
	// (fitBoard clips the bootstrap panel, so 12 rows fits in both modes.)
	m2 := model{width: 30, height: 12, snap: &ViewSnapshot{Bootstrapping: true, BootstrapBytes: 512, BootstrapIdleSeconds: 1}, ctrl: Controls{NoAnimation: true}}
	m2.spin = spinner.New(spinner.WithSpinner(spinner.Dot))
	if lipgloss.Width(m2.View()) > 30 || lipgloss.Height(m2.View()) > 12 {
		t.Fatalf("static marker overflows a small screen: %s", m2.View())
	}
}
