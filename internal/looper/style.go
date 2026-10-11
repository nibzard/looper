package looper

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/mattn/go-runewidth"
)

// OutputMode selects the rendering path for human output. Machine output
// through --json is separate from both modes.
type OutputMode int

const (
	// ModePlain carries no ANSI escapes, animation, forms, or glyphs.
	ModePlain OutputMode = iota
	// ModeInteractive renders labels, color, glyphs, and timestamps.
	ModeInteractive
)

// isTTY reports whether a file is an interactive terminal.
func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// writerTTY reports whether an arbitrary writer is a terminal. Writers that
// are not files (tests, pipes, captures) are never terminals.
func writerTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isTTY(f)
}

// terminalInteractive reports whether the terminal type supports full-screen
// or cursor-addressed interaction. TERM=dumb gets plain output.
func terminalInteractive() bool {
	term := os.Getenv("TERM")
	return term != "" && term != "dumb"
}

// DetectMode picks the output mode for a stream pair. Explicit --plain wins
// over a terminal. Color is a separate decision: NO_COLOR keeps interactive
// layout but drops color, and color is never the only status indicator.
func DetectMode(in *os.File, out io.Writer, plain bool) OutputMode {
	if plain {
		return ModePlain
	}
	if !isTTY(in) || !writerTTY(out) {
		return ModePlain
	}
	if !terminalInteractive() {
		return ModePlain
	}
	return ModeInteractive
}

// ANSI attributes used by the style layer.
const (
	ansiReset  = "\x1b[0m"
	ansiGreen  = "\x1b[32m"
	ansiBlue   = "\x1b[34m"
	ansiYellow = "\x1b[33m"
	ansiDim    = "\x1b[2m"
)

// eventStyle maps an event kind to its glyph and color class.
type eventStyle struct {
	glyph string
	color string
	label string
}

// Glyphs: done check, failure cross, repair loop, hold pause, start arrow,
// progress dots. Colors: green for done, blue for doing, yellow for blocked.
func styleFor(kind string) eventStyle {
	switch kind {
	case "result":
		return eventStyle{"✓", ansiGreen, ""}
	case "finish":
		return eventStyle{"✓", ansiGreen, ""}
	case "start":
		return eventStyle{"▸", ansiBlue, ""}
	case "check":
		return eventStyle{"…", ansiBlue, "checking"}
	case "plan", "bootstrap", "review":
		return eventStyle{"…", ansiBlue, ""}
	case "repair", "diagnosis":
		return eventStyle{"↻", ansiYellow, ""}
	case "blocker", "review-error", "hook-error", "diagnosis-error":
		return eventStyle{"✗", ansiYellow, ""}
	case "waiting", "limit", "stop":
		return eventStyle{"⏸", ansiYellow, ""}
	case "progress":
		return eventStyle{"…", "", ""}
	default:
		return eventStyle{"·", "", ""}
	}
}

// sanitize removes terminal control characters from task text and logs in
// human views. Raw stored records and JSON output keep the original bytes.
// Escape sequences are consumed whole: CSI (... final letter), string
// sequences terminated by BEL or ST (OSC, DCS, SOS, PM, APC), and two-byte
// escapes, so their payloads never leak into visible text.
func sanitize(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\x1b':
			if n := escapeLength(runes[i:]); n > 0 {
				i += n - 1 // consume the whole sequence
			} else {
				b.WriteRune('?')
			}
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeLength reports how many runes the escape sequence starting at the ESC
// rune occupies, or zero for a lone ESC at end of input.
func escapeLength(runes []rune) int {
	if len(runes) < 2 {
		return 0
	}
	switch runes[1] {
	case '[': // CSI: parameters and intermediates, then a final byte 0x40-0x7E
		for j := 2; j < len(runes); j++ {
			if runes[j] >= 0x40 && runes[j] <= 0x7e {
				return j + 1
			}
		}
		return len(runes)
	case ']', 'P', 'X', '^', '_': // OSC, DCS, SOS, PM, APC: until BEL or ST
		for j := 2; j < len(runes); j++ {
			if runes[j] == '\x07' {
				return j + 1
			}
			if runes[j] == '\x1b' && j+1 < len(runes) && runes[j+1] == '\\' {
				return j + 2
			}
		}
		return len(runes)
	default: // two-byte escape
		return 2
	}
}

// displayWidth measures cells, counting wide characters as two columns.
func displayWidth(s string) int { return runewidth.StringWidth(s) }

// takeCells returns the longest prefix of s fitting the cell budget and its
// byte length.
func takeCells(s string, budget int) (string, int) {
	used := 0
	for i, r := range s {
		w := runewidth.RuneWidth(r)
		if used+w > budget {
			return s[:i], i
		}
		used += w
	}
	return s, len(s)
}

// wrapText breaks text so every line fits the cell width. Lines that already
// fit keep their exact spacing; long unbreakable runs break by display
// width. Callers indent continuation lines to align under the message.
func wrapText(text string, width int) []string {
	if width < 4 {
		return strings.Split(text, "\n")
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if displayWidth(line) <= width {
			out = append(out, line)
			continue
		}
		var current strings.Builder
		avail := width
		for _, word := range strings.Fields(line) {
			for current.Len() > 0 && displayWidth(word) > avail-1 {
				out = append(out, current.String())
				current.Reset()
				avail = width
			}
			for displayWidth(word) > avail {
				prefix, n := takeCells(word, avail)
				current.WriteString(prefix)
				out = append(out, current.String())
				current.Reset()
				word = word[n:]
				avail = width
			}
			if current.Len() > 0 {
				current.WriteString(" ")
				avail--
			}
			current.WriteString(word)
			avail -= displayWidth(word)
		}
		if current.Len() > 0 || len(out) == 0 {
			out = append(out, current.String())
		}
	}
	return out
}

// Presenter renders human output for one output mode. It never starts
// engines or writes profiles; the event journal and JSON output stay
// authoritative and untouched.
type Presenter struct {
	Mode    OutputMode
	Color   bool
	Out     io.Writer
	Width   func() int
	now     func() time.Time
	started time.Time
}

// NewPresenter builds a presenter for the detected mode. Color is disabled by
// NO_COLOR and in plain mode.
func NewPresenter(mode OutputMode, out io.Writer, width func() int) *Presenter {
	return &Presenter{Mode: mode, Color: mode == ModeInteractive && os.Getenv("NO_COLOR") == "", Out: out, Width: width, now: time.Now}
}

func (p *Presenter) paint(color, s string) string {
	if !p.Color || color == "" {
		return s
	}
	return color + s + ansiReset
}

// fileOf extracts a file writer, returning nil for non-file writers.
func fileOf(w io.Writer) *os.File {
	f, _ := w.(*os.File)
	return f
}

// Event renders one stored event for humans. The message text is the stored
// single source of truth; only decoration differs per mode. Progress lines
// carry the elapsed time of the current controller session; the session clock
// starts at the first rendered event, never inventing historical timing.
func (p *Presenter) Event(kind, message string) {
	if p.started.IsZero() {
		p.started = p.now()
	}
	stamp := p.now().Format("15:04:05")
	text := sanitize(message)
	if p.Mode == ModeInteractive && kind == "progress" {
		text += "  " + p.paint(ansiDim, shortDuration(p.now().Sub(p.started)))
	}
	p.line(kind, text, stamp)
}

// StoredEvent renders a journaled event with its recorded time.
func (p *Presenter) StoredEvent(e Event) {
	stamp := e.Time
	if t, err := time.Parse(time.RFC3339Nano, e.Time); err == nil {
		stamp = t.Local().Format("15:04:05")
	} else if t, err := time.Parse(time.RFC3339, e.Time); err == nil {
		stamp = t.Local().Format("15:04:05")
	}
	p.line(e.Kind, sanitize(e.Message), stamp)
}

func (p *Presenter) line(kind, text, stamp string) {
	switch p.Mode {
	case ModeInteractive:
		style := styleFor(kind)
		timestamp := p.paint(ansiDim, stamp)
		glyph := p.paint(style.color, style.glyph)
		label := style.label
		if label != "" {
			label = " " + p.paint(style.color, label)
		}
		prefix := timestamp + " " + glyph + label
		labelCells := displayWidth(style.label)
		if labelCells > 0 {
			labelCells++ // the rendered label carries one leading space
		}
		prefixCells := displayWidth(stamp) + 1 + displayWidth(style.glyph) + labelCells + 2
		if width := p.Width(); width > prefixCells+4 {
			first := true
			for _, line := range wrapText(text, width-prefixCells-1) {
				if first {
					fmt.Fprintln(p.Out, prefix+"  "+line)
					first = false
				} else {
					fmt.Fprintln(p.Out, strings.Repeat(" ", prefixCells)+line)
				}
			}
			return
		}
		fmt.Fprintln(p.Out, prefix+"  "+text)
	default:
		for _, line := range strings.Split(text, "\n") {
			fmt.Fprintln(p.Out, line)
		}
	}
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Truncate(time.Second).String()
	case d < time.Hour:
		return d.Truncate(time.Minute).String()
	default:
		return d.Truncate(time.Minute).String()
	}
}

// StatusBoard renders the compact interactive status view. Deriving ready
// work from Backlog.Ready keeps dependency waits distinct from explicit
// blocked status.
func (p *Presenter) StatusBoard(s *State, dbPath string) {
	done, blocked, total := 0, 0, 0
	for _, t := range s.Backlog.Tasks {
		if t.Marker() {
			continue
		}
		total++
		switch t.Status {
		case "done":
			done++
		case "blocked":
			blocked++
		}
	}
	barWidth := 16
	filled := 0
	if total > 0 {
		filled = barWidth * done / total
	}
	bar := p.paint(ansiGreen, strings.Repeat("█", filled)) + strings.Repeat("░", barWidth-filled)
	fmt.Fprintf(p.Out, "Backlog  %s  %d/%d complete\n", bar, done, total)
	var inFlight, ready, blockedLines []string
	for _, a := range s.Attempts {
		switch a.Phase {
		case "running":
			inFlight = append(inFlight, sanitize(a.Task.ID)+" "+p.paint(ansiBlue, "implementing"))
		case "verifying":
			inFlight = append(inFlight, a.Task.ID+" "+p.paint(ansiBlue, "checking"))
		case "ready", "resume":
			inFlight = append(inFlight, a.Task.ID+" "+p.paint(ansiBlue, "queued"))
		}
	}
	for _, t := range s.Backlog.Ready() {
		ready = append(ready, t.ID+" "+sanitize(t.Title))
	}
	for _, t := range s.Backlog.Tasks {
		if t.Status == "blocked" {
			reason := "blocked"
			if len(t.Blockers) > 0 {
				reasons := make([]string, 0, len(t.Blockers))
				for _, b := range t.Blockers {
					reasons = append(reasons, sanitize(b))
				}
				reason = strings.Join(reasons, "; ")
			}
			blockedLines = append(blockedLines, t.ID+" — "+reason)
		}
	}
	section := func(marker, title string, rows []string, color string) {
		if len(rows) == 0 {
			return
		}
		fmt.Fprintf(p.Out, "%s %-12s %s\n", p.paint(color, marker), title, strings.Join(rows, "   "))
	}
	section("▸", "In flight", inFlight, ansiBlue)
	section("▶", "Ready", ready, "")
	section("⏸", "Blocked", blockedLines, ansiYellow)
	fmt.Fprintln(p.Out, "State   ", dbPath)
}

// DoctorLine renders one doctor check result with a fix hint for failures.
func (p *Presenter) DoctorLine(ok bool, name, hint string) {
	if p.Mode == ModePlain {
		if ok {
			fmt.Fprintf(p.Out, "Found: %s\n", name)
		} else {
			fmt.Fprintf(p.Out, "Missing: %s\n  Fix: %s\n", name, hint)
		}
		return
	}
	if ok {
		fmt.Fprintf(p.Out, "%s %s\n", p.paint(ansiGreen, "✓"), name)
	} else {
		fmt.Fprintf(p.Out, "%s %s\n  %s\n", p.paint(ansiYellow, "✗"), name, p.paint(ansiDim, "fix: "+hint))
	}
}
