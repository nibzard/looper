package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// claudeStreamStub prints one final stream line from CLAUDE_STUB_RESULT. With
// bulkMiB above zero it first writes that much non-JSON output, so the log
// crosses the 64 MiB retention limit before the envelope arrives.
func claudeStreamStub(t *testing.T, envelope string, bulkMiB int) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude.sh")
	script := "#!/bin/bash\n"
	if bulkMiB > 0 {
		script += fmt.Sprintf("dd if=/dev/zero bs=1048576 count=%d 2>/dev/null\nprintf '\\n'\n", bulkMiB)
	}
	script += "printf '%s\\n' \"$CLAUDE_STUB_RESULT\"\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_STUB_RESULT", envelope)
	return bin
}

func claudeEnvelope(t *testing.T, fields map[string]any) string {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func runClaude(t *testing.T, bin, dir string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return (Agent{Config: Config{ClaudeBin: bin, RetryLimit: 1}}).Run(ctx, "claude", dir, dir, "Complete task T1", true)
}

// A single Claude result line larger than the 64 MiB log limit is discarded
// by tail retention. The remaining truncation marker is itself one valid JSON
// object; it must be rejected instead of becoming the task result.
func TestClaudeOversizedResultLineIsRejected(t *testing.T) {
	// The stub generates the bulk itself; a 70 MiB payload cannot travel
	// through the environment. The single line of roughly 73 MiB leaves only
	// the marker once the tail is retained.
	bin := filepath.Join(t.TempDir(), "claude.sh")
	script := "#!/bin/bash\n" +
		"printf '%s' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"'\n" +
		"dd if=/dev/zero bs=1048576 count=70 2>/dev/null | tr '\\0' 'x'\n" +
		"printf '%s\\n' '\",\"structured_output\":null}'\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	_, err := runClaude(t, bin, dir)
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != "invalid_result" {
		t.Fatalf("want invalid_result, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "result.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rejected result must not write a success artifact: %v", statErr)
	}
	retained, err := readBounded(filepath.Join(dir, "events-00.jsonl"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"type":"looper.log_truncated","message":"Event log truncated; only the tail is retained"}` + "\n")
	if !bytes.Equal(retained, want) {
		t.Fatalf("expected the marker alone to remain, got %d bytes", len(retained))
	}
}

// Claude results require a complete envelope line carrying result or
// structured_output. Missing, malformed, and error envelopes are invalid
// results that never write result.json.
func TestClaudeResultEnvelopeRequired(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
	}{
		{"missing envelope", `{"type":"system","subtype":"init"}`},
		{"null structured output", `{"type":"result","subtype":"success","structured_output":null}`},
		{"non-string result", `{"type":"result","subtype":"success","result":42}`},
		{"empty result text", `{"type":"result","subtype":"success","result":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := claudeStreamStub(t, tc.stream, 0)
			dir := t.TempDir()
			_, err := runClaude(t, bin, dir)
			var agentErr *AgentError
			if !errors.As(err, &agentErr) || agentErr.Kind != "invalid_result" {
				t.Fatalf("want invalid_result, got %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "result.json")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid envelope must not write a success artifact: %v", statErr)
			}
		})
	}

	// An error envelope stays an execution failure, not a fabricated result.
	bin := claudeStreamStub(t, claudeEnvelope(t, map[string]any{"type": "result", "is_error": true, "result": "the tool failed"}), 0)
	dir := t.TempDir()
	_, err := runClaude(t, bin, dir)
	var agentErr *AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != "execution" {
		t.Fatalf("want execution, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "result.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("error envelope must not write a success artifact: %v", statErr)
	}
}

// Complete Claude envelopes keep working: structured output and text results
// that surround the JSON object with prose or fences both apply.
func TestClaudeCompleteEnvelopesApply(t *testing.T) {
	object := `{"task_id":"T1","status":"done","summary":"finished","files":["completed.txt"],"blockers":[],"followups":[],"unblock_when":""}`
	for _, tc := range []struct {
		name     string
		envelope string
	}{
		{"structured envelope", claudeEnvelope(t, map[string]any{"type": "result", "subtype": "success", "structured_output": json.RawMessage(object)})},
		{"text envelope", claudeEnvelope(t, map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "Summary prose.\n\n```json\n" + object + "\n```"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := claudeStreamStub(t, tc.envelope, 0)
			dir := t.TempDir()
			data, err := runClaude(t, bin, dir)
			if err != nil {
				t.Fatalf("valid envelope rejected: %v", err)
			}
			if string(data) != object {
				t.Fatalf("got %s", data)
			}
			saved, err := os.ReadFile(filepath.Join(dir, "result.json"))
			if err != nil || string(saved) != object+"\n" {
				t.Fatalf("success artifact missing or wrong: %q, %v", saved, err)
			}
		})
	}
}

// Large logs before a small final envelope stay successful: retention keeps
// the complete envelope and the marker does not disqualify it.
func TestClaudeResultSurvivesOversizedPrecedingLog(t *testing.T) {
	object := `{"task_id":"T1","status":"done","summary":"finished","files":["completed.txt"],"blockers":[],"followups":[],"unblock_when":""}`
	envelope := claudeEnvelope(t, map[string]any{"type": "result", "subtype": "success", "structured_output": json.RawMessage(object)})
	bin := claudeStreamStub(t, envelope, 100)
	dir := t.TempDir()
	data, err := runClaude(t, bin, dir)
	if err != nil {
		t.Fatalf("retained envelope rejected: %v", err)
	}
	if string(data) != object {
		t.Fatalf("got %s", data)
	}
	retained, err := readBounded(filepath.Join(dir, "events-00.jsonl"), 64<<20)
	if err != nil || !bytes.HasPrefix(retained, []byte(`{"type":"looper.log_truncated"`)) || !bytes.HasSuffix(retained, []byte(envelope+"\n")) {
		t.Fatalf("retained log must carry the marker and the complete envelope: %v", err)
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
