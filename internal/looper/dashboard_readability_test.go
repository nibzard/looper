package looper

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletedDiffExplainsCleanup(t *testing.T) {
	e := snapshotEngine(t)
	a := e.State.Attempts[1]
	a.Worktree = filepath.Join(t.TempDir(), "removed")
	a.Base = "base"
	e.publishView(false)
	text, err := attemptDiff(e.Dir, a.ID)
	if err != nil || !strings.Contains(text, "T1") || !strings.Contains(text, "completed") || !strings.Contains(text, "logs") {
		t.Fatalf("completed checkout cleanup should be contextual information: %q %v", text, err)
	}
}

func TestDashboardLogExplainsRetryWithoutRawJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events-00.jsonl")
	data := `{"type":"system","subtype":"thinking_tokens","private":"not for display"}` + "\n" + `{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"error":"unknown"}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	text, err := readDashboardLog(path)
	if err != nil || !strings.Contains(text, "API retry 1/10") || strings.Contains(text, "private") || strings.Contains(text, "{\"") {
		t.Fatalf("unreadable retry: %q %v", text, err)
	}
}

// dashboardControls reports any character a terminal would act on rather than
// print. Newlines are the one control character a human view may keep.
func dashboardControls(s string) string {
	for i, r := range s {
		if r != '\n' && (r < 0x20 || r == 0x7f) {
			return fmt.Sprintf("%q at %d", r, i)
		}
	}
	return ""
}

// Readable activity lines carry agent-chosen tool names, commands, and paths.
// The event stream is worker output, so those fields must not reach a viewport
// as live escape sequences.
func TestDashboardLogSanitizesReadableActions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events-00.jsonl")
	data := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"\u001b[31mrm -rf /\u001b[0m"}}]}}` + "\n" +
		`{"type":"event_msg","item":{"type":"command_execution","command":"printf '\u001b]0;owned title\u0007'"}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	text, err := readDashboardLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if bad := dashboardControls(text); bad != "" {
		t.Fatalf("terminal controls reached the log view: %s in %q", bad, text)
	}
	if !strings.Contains(text, "rm -rf /") || strings.Contains(text, "owned title") {
		t.Fatalf("readable actions lost their content or kept a payload: %q", text)
	}
}

// A diff renders patch content the worker wrote, so a hostile fixture file must
// not inject escape sequences through the dashboard's diff view.
func TestAttemptDiffSanitizesTerminalControls(t *testing.T) {
	root := t.TempDir()
	if _, err := gitText(root, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fixture.txt"), []byte("clean\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(root, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(root, "-c", "user.name=dashboard", "-c", "user.email=dash@localhost", "commit", "-qm", "base"); err != nil {
		t.Fatal(err)
	}
	base, err := gitText(root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	if _, err := gitText(root, "worktree", "add", "-q", "--detach", work, base); err != nil {
		t.Fatal(err)
	}
	hostile := "clean\n\x1b[31mred\x1b[0m\n\x1b]0;owned\x07bel\x07 vertical\x0b\n"
	if err := os.WriteFile(filepath.Join(work, "fixture.txt"), []byte(hostile), 0644); err != nil {
		t.Fatal(err)
	}

	e := snapshotEngine(t)
	a := e.State.Attempts[0]
	a.Worktree, a.Base = work, base
	e.publishView(false)
	text, err := attemptDiff(e.Dir, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bad := dashboardControls(text); bad != "" {
		t.Fatalf("terminal controls reached the diff view: %s in %q", bad, text)
	}
	if !strings.Contains(text, "red") || strings.Contains(text, "owned") {
		t.Fatalf("patch content lost or escape payload survived: %q", text)
	}
	// The retained worktree keeps its original bytes; only the view is neutral.
	kept, err := os.ReadFile(filepath.Join(work, "fixture.txt"))
	if err != nil || !bytes.Contains(kept, []byte("\x1b[31m")) {
		t.Fatalf("the worktree must keep raw evidence: %q %v", kept, err)
	}
}
