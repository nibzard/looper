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
