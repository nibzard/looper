package looper

import (
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
