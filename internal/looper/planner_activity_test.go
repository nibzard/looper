package looper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanningUsesInspectionToolsAndStreaming(t *testing.T) {
	e := snapshotEngine(t)
	bin := filepath.Join(t.TempDir(), "agent.sh")
	script := `#!/bin/sh
printf '%s\n' "$@" > args.txt
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"Cargo.toml"}}]}}'
printf '%s\n' '{"type":"result","result":"{\"tasks\":[]}"}'
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	e.Config.ClaudeBin, e.Config.ReviewAgent, e.Config.RetryLimit = bin, "claude", 1
	work := t.TempDir()
	raw, err := e.runPlanner(context.Background(), e.capture(), work, filepath.Join(e.Dir, "plan-stream"), "inspect")
	if err != nil || string(raw) != `{"tasks":[]}` {
		t.Fatalf("result: %s, %v", raw, err)
	}
	args, err := os.ReadFile(filepath.Join(work, "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--json-schema\n" + planSchema, "--output-format\nstream-json", "--verbose", "--tools\nRead,Glob,Grep", "--strict-mcp-config", "--mcp-config\n{\"mcpServers\":{}}", "--disable-slash-commands"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("missing %s: %s", want, args)
		}
	}
	events, err := e.Store.RecentEvents(-1, 30)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "planner-activity" && event.Message == "Read Cargo.toml" {
			found = true
		}
	}
	if !found {
		t.Fatal("stream activity was not published")
	}
}

func TestPlannerActivityPartialAndOversizedRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	reader := plannerActivityReader{}
	var actions []string
	emit := func(s string) { actions = append(actions, s) }
	f.WriteString(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep",`)
	if !reader.poll(path, emit) || len(actions) != 0 {
		t.Fatal("partial line parsed")
	}
	f.WriteString(`"input":{"pattern":"cargo","path":"scripts"}}]}}` + "\n")
	reader.poll(path, emit)
	if len(actions) != 1 || actions[0] != "Grep cargo in scripts" {
		t.Fatalf("actions: %v", actions)
	}
	f.WriteString(strings.Repeat("x", 2<<20) + "\n" + `{"type":"result"}` + "\n")
	for reader.poll(path, emit) {
	}
	if len(actions) != 2 || actions[1] != "Agent returned a result" {
		t.Fatalf("oversized record lost following event: %v", actions)
	}
}

func TestPlannerPromptDefersExpensiveWork(t *testing.T) {
	prompt := planPrompt(Backlog{}, agentContext{})
	for _, want := range []string{"inspection-only", "Do not run builds, tests, benchmarks", "Do not delegate work", "Do not execute them"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("missing instruction: %s", want)
		}
	}
}
