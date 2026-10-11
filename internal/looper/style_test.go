package looper

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func fixedPresenter(mode OutputMode, color bool, width int) (*Presenter, *bytes.Buffer) {
	out := &bytes.Buffer{}
	p := NewPresenter(mode, out, func() int { return width })
	p.Color = color
	p.now = func() time.Time { return time.Date(2026, 10, 11, 14, 2, 11, 0, time.UTC) }
	return p, out
}

func TestSanitizeStripsControlCharacters(t *testing.T) {
	in := "ok\x1b[31mred\x1b[0m bell\x07 cr\r line\n tab\t del\x7f"
	got := sanitize(in)
	if strings.ContainsAny(got, "\x1b\x07\r\x7f") {
		t.Fatalf("control characters survived: %q", got)
	}
	if !strings.Contains(got, "line") || !strings.Contains(got, "tab\t") {
		t.Fatalf("content lost: %q", got)
	}
}

func TestWrapRespectsDisplayWidth(t *testing.T) {
	text := "Add SQLite store with WAL mode and a schema migration"
	wrapped := wrapText(text, 20)
	if len(wrapped) < 2 {
		t.Fatalf("expected wrapping: %q", wrapped)
	}
	for _, line := range wrapped {
		if displayWidth(line) > 20 {
			t.Fatalf("line too wide (%d): %q", displayWidth(line), line)
		}
	}
	// Lines that already fit keep their exact spacing.
	fit := wrapText("T003  done | Add SQLite store", 80)
	if len(fit) != 1 || fit[0] != "T003  done | Add SQLite store" {
		t.Fatalf("spacing not preserved: %q", fit)
	}
	// Wide characters count two cells; unbreakable runs break by cells.
	wide := strings.Repeat("数据库迁移脚本文件", 4) + " then more words follow here"
	for _, line := range wrapText(wide, 16) {
		if displayWidth(line) > 16 {
			t.Fatalf("wide line too wide (%d): %q", displayWidth(line), line)
		}
	}
	// Very small budgets keep one line per input line instead of scrambling.
	narrow := wrapText("short words", 3)
	if len(narrow) != 1 || narrow[0] != "short words" {
		t.Fatalf("narrow fallback broken: %q", narrow)
	}
}

func TestContinuationLinesAlignUnderMessage(t *testing.T) {
	p, out := fixedPresenter(ModeInteractive, false, 30)
	p.Event("result", "T003 done | Add SQLite store with WAL mode")
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected wrapping at width 30: %q", lines)
	}
	for i, line := range lines {
		if displayWidth(line) > 30 {
			t.Fatalf("line %d exceeds width (%d): %q", i, displayWidth(line), line)
		}
	}
	indent := displayWidth("14:02:11") + 1 + displayWidth("✓") + 2
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, strings.Repeat(" ", indent)) {
			t.Fatalf("continuation not aligned: %q", line)
		}
	}
}

func TestPlainEventHasNoEscapes(t *testing.T) {
	p, out := fixedPresenter(ModePlain, false, 80)
	p.Event("result", "T003  done | Add SQLite store")
	s := out.String()
	if strings.Contains(s, "\x1b") || strings.Contains(s, "✓") {
		t.Fatalf("plain output must not decorate: %q", s)
	}
	if !strings.Contains(s, "T003  done | Add SQLite store") {
		t.Fatalf("plain output lost the message: %q", s)
	}
}

func TestInteractiveEventLine(t *testing.T) {
	p, out := fixedPresenter(ModeInteractive, false, 80)
	p.Event("result", "T003  done | Add SQLite store")
	got := out.String()
	// No color: the timestamp, glyph, and message still appear in order.
	want := "14:02:11 ✓  T003  done | Add SQLite store\n"
	if got != want {
		t.Fatalf("interactive line mismatch:\n got %q\nwant %q", got, want)
	}
	// Color mode paints the same content; color is never the only indicator.
	p2, out2 := fixedPresenter(ModeInteractive, true, 80)
	p2.Event("result", "x")
	if !strings.Contains(out2.String(), "✓") || !strings.Contains(out2.String(), "\x1b[32m") {
		t.Fatalf("colored line broken: %q", out2.String())
	}
}

func TestInteractiveWrapsLongMessages(t *testing.T) {
	p, out := fixedPresenter(ModeInteractive, false, 30)
	p.Event("result", "T003  done | Add SQLite store with WAL mode")
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if displayWidth(line) > 30 {
			t.Fatalf("line exceeds the terminal (%d): %q", displayWidth(line), line)
		}
	}
}

func TestStatusBoardSections(t *testing.T) {
	s := &State{Backlog: Backlog{Version: 1, Sources: []string{}, Tasks: []Task{
		{ID: "T001", Title: "one", Priority: 1, Status: "done"},
		{ID: "T002", Title: "two", Priority: 1, Status: "todo"},
		{ID: "T003", Title: "three", Priority: 1, Status: "todo", DependsOn: []string{"T002"}},
		{ID: "T004", Title: "four", Priority: 1, Status: "blocked", Blockers: []string{"missing credentials"}},
	}}, Attempts: []*Attempt{
		{ID: "a1", Task: Task{ID: "T002", Title: "two"}, Phase: "running"},
	}}
	p, out := fixedPresenter(ModeInteractive, false, 80)
	p.StatusBoard(s, "/tmp/state.db")
	got := out.String()
	for _, want := range []string{"1/4 complete", "In flight", "T002 implementing", "Ready", "T002 two", "Blocked", "T004 — missing credentials", "/tmp/state.db"} {
		if !strings.Contains(got, want) {
			t.Fatalf("board missing %q:\n%s", want, got)
		}
	}
	// A task waiting on a dependency is not blocked; the ready section
	// excludes it and the blocked section lists only explicit blockers.
	if strings.Contains(got, "T003") {
		t.Fatalf("dependency wait rendered: %s", got)
	}
}

func TestEmptyStatusBoard(t *testing.T) {
	s := &State{Backlog: Backlog{Version: 1, Sources: []string{}, Tasks: []Task{}}}
	p, out := fixedPresenter(ModeInteractive, false, 80)
	p.StatusBoard(s, "state.db")
	got := out.String()
	if !strings.Contains(got, "0/0 complete") || strings.Contains(got, "In flight") || strings.Contains(got, "Ready") {
		t.Fatalf("empty board broken: %q", got)
	}
}

func TestEventJSONIsByteStable(t *testing.T) {
	// The machine stream must stay byte-identical: field order id, time,
	// kind, message with an RFC3339 timestamp.
	b, err := json.Marshal(Event{Time: "2026-10-11T14:02:11Z", Kind: "result", Message: "T003  done | x"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":0,"time":"2026-10-11T14:02:11Z","kind":"result","message":"T003  done | x"}`
	if string(b) != want {
		t.Fatalf("event JSON changed:\n got %s\nwant %s", b, want)
	}
}

func TestDoctorHints(t *testing.T) {
	p, out := fixedPresenter(ModePlain, false, 80)
	p.DoctorLine(false, "codex", "install codex or set CODEX_BIN")
	if !strings.Contains(out.String(), "Missing: codex") || !strings.Contains(out.String(), "Fix: install codex") {
		t.Fatalf("plain doctor hint broken: %q", out.String())
	}
	out.Reset()
	p.DoctorLine(true, "/usr/bin/git", "")
	if strings.Contains(out.String(), "Fix") {
		t.Fatalf("success needs no hint: %q", out.String())
	}
}

func TestProgressShowsElapsed(t *testing.T) {
	p, out := fixedPresenter(ModeInteractive, false, 0)
	start := time.Date(2026, 10, 11, 14, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return start }
	p.Event("progress", "2 implementing | 3/12 complete")
	p.now = func() time.Time { return start.Add(12 * time.Minute) }
	p.Event("progress", "2 implementing | 4/12 complete")
	got := out.String()
	if !strings.Contains(got, "12m0s") {
		t.Fatalf("elapsed missing: %q", got)
	}
}

func TestSanitizeConsumesStringSequences(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b]0;owned-title\x07next", "next"},
		{"\x1b]8;;http://example\x1b\\link text", "link text"},
		{"\x1bP$q;payload\x1b\\rest", "rest"},
		{"\x1b_alert\x07after", "after"},
		{"\x1b[2Jcleared", "cleared"},
		{"lone\x1b", "lone?"},
		{"unclosed\x1b]0;title", "unclosed"},
	}
	for _, c := range cases {
		if got := sanitize(c.in); got != c.want {
			t.Fatalf("sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
