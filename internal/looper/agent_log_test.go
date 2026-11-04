package looper

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadAgentLog(t *testing.T) {
	for _, size := range []int{1023, 1024, 1025, 4096} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			original := append(bytes.Repeat([]byte{'x'}, size-3), []byte("\n{}")...)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			raw, err := readAgentLog(path, 1024)
			if err != nil {
				t.Fatal(err)
			}
			retained, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, retained) {
				t.Fatalf("retained log differs from returned log: %v", err)
			}
			if size <= 1024 {
				if !bytes.Equal(raw, original) {
					t.Fatal("log within limit changed")
				}
			} else if len(raw) > 1024 || !bytes.Contains(raw, []byte(`"type":"looper.log_truncated"`)) || !bytes.HasSuffix(raw, []byte("\n{}")) {
				t.Fatalf("expected bounded log with marker and final event: %q", raw)
			}
		})
	}
	if _, err := readAgentLog(filepath.Join(t.TempDir(), "missing"), 1024); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing log error: %v", err)
	}
}

// Add 100 MiB of output to a real agent invocation, after its result is written.
// Keep a final event to check that retention preserves the end of the log.
func oversizedAgentLog(t *testing.T, bin, ending string) {
	t.Helper()
	script, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	script = bytes.Replace(script, []byte("exit 0"), []byte(`if [ "$kind" = worker ]; then
  dd if=/dev/zero bs=1048576 count=100 2>/dev/null
  printf '\n'
  printf '{"structured_output":%s}\n' "$result"
  printf '\n%s\n' '{"type":"turn.completed"}'
`+ending+`
fi
exit 0`), 1)
	if err := os.WriteFile(bin, script, 0755); err != nil {
		t.Fatal(err)
	}
}

func TestCompletedCodexRunWithOversizedLogAppliesResult(t *testing.T) {
	b := Backlog{Version: 1, Sources: []string{}, Goal: "complete one task", Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
	}}
	e, _ := reviewEngine(t, reviewComplete, nil, b)
	t.Setenv("LOOPER_FAKE_SUMMARY", `{"task_id":"T1","status":"done","summary":"finished","files":["completed.txt"],"blockers":[],"followups":[],"unblock_when":""}`)
	t.Setenv("LOOPER_FAKE_PLAN", `{"setup":"","verify":"test -f completed.txt","tasks":[{"task_id":"T1","write_paths":["."],"resources":[]}]}`)
	oversizedAgentLog(t, e.Config.CodexBin, ": > completed.txt")
	e.Config.Once = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code, err := e.Run(ctx)
	if err != nil || code != 0 {
		t.Fatalf("run failed: code %d, %v", code, err)
	}
	if e.State.Backlog.Task("T1").Status != "done" || e.State.Integrated != 1 {
		t.Fatal("completed result was not applied")
	}
	if _, err := os.Stat(filepath.Join(e.Config.Root, "completed.txt")); err != nil {
		t.Fatalf("verified worker change was not published: %v", err)
	}
	a := e.State.Attempts[0]
	logs, err := filepath.Glob(filepath.Join(a.Directory, "work-*", "events-00.jsonl"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("expected one worker log: %v, %v", logs, err)
	}
	raw, err := readBounded(logs[0], 64<<20)
	if err != nil || !bytes.HasPrefix(raw, []byte(`{"type":"looper.log_truncated"`)) || !bytes.HasSuffix(raw, []byte("{\"type\":\"turn.completed\"}\n")) {
		t.Fatalf("retained log must be bounded with a marker and final event: %v", err)
	}
	var exported Backlog
	if err := readJSON(e.Config.Todo, &exported); err != nil || exported.Task("T1").Status != "done" {
		t.Fatalf("result was not exported: %v", err)
	}
}

func TestOversizedCodexLogDoesNotHideFailures(t *testing.T) {
	for _, tc := range []struct{ name, result, ending, want string }{
		{"invalid result", "{broken}", "", "invalid_result"},
		{"process failure", "{}", "printf '%s\\n' '{\"type\":\"error\",\"message\":\"permission denied\"}'\nexit 1", "configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := fakeAgent(t, tc.result)
			oversizedAgentLog(t, bin, tc.ending)
			dir := t.TempDir()
			_, err := (Agent{Config: Config{CodexBin: bin, RetryLimit: 1}}).Run(context.Background(), "codex", dir, dir, "Complete task T1", true)
			var agentErr *AgentError
			if !errors.As(err, &agentErr) || agentErr.Kind != tc.want {
				t.Fatalf("want %s, got %v", tc.want, err)
			}
			if tc.want == "configuration" && !strings.Contains(err.Error(), "permission denied") {
				t.Fatalf("missing final failure message: %v", err)
			}
		})
	}
}
