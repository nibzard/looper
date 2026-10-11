package looper

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The stub prints the stream-json shapes zcode emits — an event with a
// nested payload.response, then the one-line result terminator with the
// top-level response — and records its arguments, so tests cover the
// invocation and the parse.
func zcodeStub(t *testing.T, response string) (bin, argsPath string) {
	t.Helper()
	dir := t.TempDir()
	bin, argsPath = filepath.Join(dir, "zcode.sh"), filepath.Join(dir, "args")
	script := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" > " + argsPath + "\n" +
		"printf '{\"type\":\"turn.completed\",\"payload\":{\"response\":\"other\"}}\\n'\n" +
		"printf '{\"type\":\"result\",\"sessionId\":\"s1\",\"response\":%s,\"eventCount\":2}\\n' \"$ZCODE_STUB_RESPONSE\"\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZCODE_STUB_RESPONSE", response)
	return bin, argsPath
}

func TestZcodeWorkerInvocationAndParse(t *testing.T) {
	result := `{"task_id":"T001","status":"done","summary":"finished","files":["main.go"],"blockers":[],"followups":[],"unblock_when":""}`
	quoted, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	bin, argsPath := zcodeStub(t, string(quoted))
	cwd := t.TempDir()
	data, err := Agent{Config{ZcodeBin: bin, RetryLimit: 1}}.Run(context.Background(), "zcode", cwd, filepath.Join(t.TempDir(), "run"), "complete T001", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != result {
		t.Fatalf("got %s", data)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--cwd " + cwd, "--output-format stream-json", "--mode yolo", "-p complete T001"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("invocation %q missing %q", args, want)
		}
	}
}

// Planning keeps the worker invocation: zcode's plan mode answers with a
// prose summary and drops the JSON, so the inspection-only prompt is the
// planner's restriction.
func TestZcodePlannerKeepsWorkerMode(t *testing.T) {
	plan := `{"setup":"npm ci","verify":"npm test","tasks":[{"task_id":"T001","write_paths":["."],"resources":[]}]}`
	quoted, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	bin, argsPath := zcodeStub(t, string(quoted))
	data, err := Agent{Config{ZcodeBin: bin, RetryLimit: 1}}.run(context.Background(), "zcode", t.TempDir(), filepath.Join(t.TempDir(), "plan"), "plan the work", false, func(string, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != plan {
		t.Fatalf("got %s", data)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--mode yolo") || strings.Contains(string(args), "--mode plan") {
		t.Fatalf("planning must keep the worker mode: %q", args)
	}
}

// zcode has no schema flag; the prompt carries the shape, so a fenced
// object inside the response must still extract.
func TestZcodeParsesFencedResponse(t *testing.T) {
	object := `{"setup":"","verify":"go test ./...","tasks":[]}`
	fenced := "Explanation before the plan.\n```json\n" + object + "\n```\nNotes after."
	quoted, err := json.Marshal(fenced)
	if err != nil {
		t.Fatal(err)
	}
	bin, _ := zcodeStub(t, string(quoted))
	data, err := Agent{Config{ZcodeBin: bin, RetryLimit: 1}}.run(context.Background(), "zcode", t.TempDir(), filepath.Join(t.TempDir(), "plan"), "plan", false, func(string, string) {})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != object {
		t.Fatalf("got %s", data)
	}
}

func TestZcodeMissingSummaryFails(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "silent.sh")
	if err := os.WriteFile(bin, []byte("#!/bin/bash\necho progress noise\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	_, err := Agent{Config{ZcodeBin: bin, RetryLimit: 1}}.Run(context.Background(), "zcode", t.TempDir(), filepath.Join(dir, "run"), "prompt", true)
	agentErr, ok := err.(*AgentError)
	if !ok || agentErr.Kind != "invalid_result" {
		t.Fatalf("want invalid_result, got %v", err)
	}
}

func TestZcodeRequiresFinalResultEvent(t *testing.T) {
	for _, event := range []string{
		`{"type":"turn.completed","response":"{}"}`,
		`{"response":"{}"}`,
		`{"type":"turn.completed","payload":{"response":"{}"}}`,
	} {
		t.Run(event, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "agent.sh")
			t.Setenv("ZCODE_TEST_EVENT", event)
			if err := os.WriteFile(bin, []byte("#!/bin/bash\nprintf '%s\\n' \"$ZCODE_TEST_EVENT\"\n"), 0755); err != nil {
				t.Fatal(err)
			}
			_, err := Agent{Config{ZcodeBin: bin, RetryLimit: 1}}.Run(context.Background(), "zcode", t.TempDir(), filepath.Join(dir, "run"), "prompt", true)
			agentErr, ok := err.(*AgentError)
			if !ok || agentErr.Kind != "invalid_result" {
				t.Fatalf("want invalid_result without a final result event, got %v", err)
			}
		})
	}
}

func TestZcodeRejectsInvalidFinalResponses(t *testing.T) {
	for _, response := range []string{`null`, `""`, `42`, `"{broken}"`} {
		t.Run(response, func(t *testing.T) {
			bin, _ := zcodeStub(t, response)
			_, err := Agent{Config{ZcodeBin: bin, RetryLimit: 1}}.Run(context.Background(), "zcode", t.TempDir(), filepath.Join(t.TempDir(), "run"), "prompt", true)
			agentErr, ok := err.(*AgentError)
			if !ok || agentErr.Kind != "invalid_result" {
				t.Fatalf("want invalid_result, got %v", err)
			}
		})
	}
}

func TestZcodeProcessFailureRejectsResponse(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	script := "#!/bin/bash\nprintf '%s\\n' '{\"type\":\"result\",\"response\":\"{}\"}'\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	_, err := Agent{Config{ZcodeBin: bin, RetryLimit: 1}}.Run(context.Background(), "zcode", t.TempDir(), filepath.Join(dir, "run"), "prompt", true)
	agentErr, ok := err.(*AgentError)
	if !ok || agentErr.Kind != "execution" {
		t.Fatalf("want execution failure, got %v", err)
	}
}
