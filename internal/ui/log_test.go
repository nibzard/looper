package ui

import (
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
)

func TestReadableLogPreservesEvidenceAndCompactsSpacing(t *testing.T) {
	input := "Run Test Suite: res://test.gd\n\n\n  test PASSED 8ms\n\nOverall Summary: 1 test cases | 0 failures\nExit code: 0\nOpen HTML Report at: file:///tmp/report.html"
	text := ansi.Strip(readableLog(input))
	if strings.Contains(text, "\n\n\n") {
		t.Fatal("repeated blank lines remain")
	}
	for _, line := range strings.Split(input, "\n") {
		if line != "" && !strings.Contains(text, line) {
			t.Fatalf("lost evidence %q", line)
		}
	}
	m := workspaceFixture()
	m.mode = ModeLog
	footer := ansi.Strip(m.workspaceFooter())
	if !strings.Contains(footer, "scroll") || !strings.Contains(footer, "back") {
		t.Fatalf("missing log navigation: %s", footer)
	}
}

// hasControl reports any character a terminal would act on rather than print.
// Newlines separate viewport lines and are the one control character a log may
// keep.
func hasControl(s string) bool {
	for _, r := range s {
		if r != '\n' && (r < 0x20 || r == 0x7f) {
			return true
		}
	}
	return false
}

func TestReadableLogStripsTerminalControls(t *testing.T) {
	// Agent tool names, commands, and paths arrive from the event stream, and
	// the raw fallback carries stored bytes. Both reach this one viewport.
	input := "Bash \x1b[31mrm -rf /tmp/x\x1b[0m\n" +
		"\x1b]0;stolen title\x07Grep pattern in src\n" +
		"vertical\x0btab and bel\x07\n" +
		"unterminated \x1b]0;osc\n" +
		"delete\x7f me\n" +
		"clean line"
	text := ansi.Strip(readableLog(input))
	if hasControl(text) {
		t.Fatalf("terminal controls reached the log viewport: %q", text)
	}
	for _, want := range []string{"rm -rf /tmp/x", "Grep pattern in src", "clean line"} {
		if !strings.Contains(text, want) {
			t.Fatalf("lost readable evidence %q in %q", want, text)
		}
	}
	if strings.Contains(text, "stolen title") || strings.Contains(text, "osc") {
		t.Fatalf("escape payload survived: %q", text)
	}
}
