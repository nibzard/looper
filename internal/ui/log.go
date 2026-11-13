package ui

import "strings"

// plainLines passes multiline evidence through plainText one line at a time.
// The line structure survives; terminal control characters never reach a
// viewport. Log and diff text comes from agent output and patch content, so a
// worker can always write raw escapes into what the dashboard renders.
func plainLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = plainText(line)
	}
	return strings.Join(lines, "\n")
}

// Style the readable log without altering the retained evidence. Reduce repeated
// blank lines from test runners, while keeping every nonempty line available.
func readableLog(text string) string {
	text = plainLines(text)
	var lines []string
	blank := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		switch {
		case strings.HasPrefix(trimmed, "Run Test Suite:"), strings.HasPrefix(trimmed, "Overall Summary:"):
			line = titleStyle.Render(trimmed)
		case strings.Contains(trimmed, " FAILED"), strings.HasPrefix(trimmed, "FAIL"), strings.HasPrefix(trimmed, "Error:"), strings.HasPrefix(trimmed, "Exit code:") && trimmed != "Exit code: 0":
			line = errorStyle.Render(line)
		case strings.Contains(trimmed, " PASSED"), trimmed == "Exit code: 0":
			line = okStyle.Render(line)
		case strings.HasPrefix(trimmed, "API retry"):
			line = blockedStyle.Render(line)
		case strings.HasSuffix(trimmed, " STARTED"), strings.HasPrefix(trimmed, "Open "), strings.HasPrefix(trimmed, "Statistics:"):
			line = dimStyle.Render(line)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
