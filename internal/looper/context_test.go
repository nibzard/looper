package looper

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestContextKeepsLargeInputsOutOfPrompt(t *testing.T) {
	e, _ := idleEngine(t, reviewComplete)
	b := clone(e.State.Backlog)
	b.Goal = strings.Repeat("project goal ", 1000)
	b.Tasks = nil
	for i := 0; i < 3000; i++ {
		b.Tasks = append(b.Tasks, Task{ID: fmt.Sprintf("T%04d", i), Title: strings.Repeat("界", 100), Status: "todo", Priority: 1, Details: strings.Repeat("acceptance detail ", 30)})
	}
	work, err := e.Repo.Workspace(identifier("context"), e.State.Tip, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(work) })
	before, err := e.Repo.Snapshot(work)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Dir, identifier("plan"))
	task := b.Tasks[0]
	task.Details = strings.Repeat("large task detail ", 20000)
	feedback := strings.Repeat("failure evidence ", 20000)
	c, err := prepareAgentContext(e.Repo, work, dir, b, &task, feedback, []contextDocument{{Import: "import-one", Source: "plan.md", Text: []byte("required behavior")}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	prompt := planPrompt(b, c)
	if len(prompt) > 4<<10 || !utf8.ValidString(prompt) || strings.Contains(prompt, "acceptance detail") {
		t.Fatalf("planner prompt: %d bytes", len(prompt))
	}
	worker := workerPrompt(b, task, c)
	if strings.Contains(worker, "large task detail") || strings.Contains(worker, "failure evidence") || !strings.Contains(worker, c.Task) || !strings.Contains(worker, c.Feedback) {
		t.Fatal("worker context is embedded or missing")
	}
	var snapshot Backlog
	if err := readJSON(c.Backlog, &snapshot); err != nil || hashJSON(snapshot) != hashJSON(b) {
		t.Fatalf("snapshot lost data: %v", err)
	}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	after, err := e.Repo.Snapshot(work)
	if err != nil || after != before {
		t.Fatalf("context entered candidate tree: %s %s %v", before, after, err)
	}
	// Agents can add ignore rules for generated context. Git must still
	// capture the product, and context must stay out of the candidate tree.
	if err := os.WriteFile(filepath.Join(work, ".gitignore"), []byte("/"+e.Repo.contextPath()+"/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Repo.Snapshot(work); err != nil {
		t.Fatalf("ignored context prevented capture: %v", err)
	}
	if err := os.WriteFile(c.Requirements, []byte("omit requirement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.validate(); err == nil {
		t.Fatal("context mutation accepted")
	}
}

func TestSmallTaskKeepsAcceptanceCriteriaInline(t *testing.T) {
	task := Task{ID: "T1", Title: "Add output", Status: "todo", Details: "Output must contain an exact newline."}
	prompt := workerPrompt(Backlog{Goal: "ship"}, task, agentContext{Backlog: "backlog.json", Task: "task.json", Requirements: "requirements.md"})
	if !strings.Contains(prompt, task.Details) || !strings.Contains(prompt, "requirements.md") {
		t.Fatal("focused worker inputs missing")
	}
}

func TestMissingAppliedContextPreventsWorkAndCompletion(t *testing.T) {
	for _, operation := range []string{"plan", "worker", "review"} {
		t.Run(operation, func(t *testing.T) {
			e, _ := idleEngine(t, reviewComplete)
			e.State.Imports = append(e.State.Imports, &ImportRecord{RequestID: "missing-import", Status: ImportApplied, Artifacts: []string{filepath.Join(t.TempDir(), "missing.md")}})
			before := hashJSON(e.State.Backlog)
			var err error
			switch operation {
			case "plan":
				err = e.plan(context.Background())
			case "worker":
				_, err = e.start(context.Background(), e.State.Backlog.Tasks[0], make(chan workerResult, 1))
			case "review":
				e.State.Finished = false
				err = e.review(context.Background(), true)
			}
			if err == nil || !strings.Contains(err.Error(), "missing-import") {
				t.Fatalf("missing import accepted: %v", err)
			}
			if hashJSON(e.State.Backlog) != before || len(e.State.Attempts) != 0 {
				t.Fatal("missing context changed backlog or launched work")
			}
			if operation == "review" && e.State.Finished {
				t.Fatal("missing context completed goal")
			}
		})
	}
}

func TestAppliedContextRejectsUnreadableOrChangedArtifact(t *testing.T) {
	e, _ := idleEngine(t, reviewComplete)
	path := filepath.Join(t.TempDir(), "requirement.md")
	if err := os.WriteFile(path, []byte("accepted"), 0600); err != nil {
		t.Fatal(err)
	}
	e.State.Imports = append(e.State.Imports, &ImportRecord{RequestID: "accepted-import", Status: ImportApplied, Artifacts: []string{path}, Digests: []string{digest([]byte("accepted"))}})
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.appliedContext(); err == nil {
		t.Fatal("changed artifact accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.appliedContext(); err == nil {
		t.Fatal("unreadable artifact accepted")
	}
}

func TestContextDoesNotOverwriteProjectDirectory(t *testing.T) {
	e, _ := idleEngine(t, reviewComplete)
	work, err := e.Repo.Workspace(identifier("context"), e.State.Tip, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(work) })
	path := filepath.Join(work, e.Repo.contextPath())
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareAgentContext(e.Repo, work, filepath.Join(e.Dir, "context-collision"), e.State.Backlog, nil, "", nil, nil); err == nil {
		t.Fatal("project directory overwritten")
	}
}

func TestContextOwnershipCannotBeChangedByAgent(t *testing.T) {
	e, _ := idleEngine(t, reviewComplete)
	work, err := e.Repo.Workspace(identifier("context"), e.State.Tip, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(work) })
	c, err := prepareAgentContext(e.Repo, work, filepath.Join(e.Dir, "owner-check"), e.State.Backlog, nil, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, e.Repo.contextPath(), ".owner"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.validate(); err == nil {
		t.Fatal("changed context ownership accepted")
	}
}

func TestLargeTaskRunsFromCompleteSnapshot(t *testing.T) {
	e, c, _ := assessRunEngine(t, "", 0)
	t.Setenv("LOOPER_TEST_ORIGINAL_AGENT", e.Config.CodexBin)
	bin := filepath.Join(t.TempDir(), "context-agent.sh")
	script := `#!/bin/bash
p=$(cat)
if [[ "$p" == 'Complete task '* ]]; then
  python3 -c 'import json,os; from pathlib import Path; t=json.loads((Path(os.environ["LOOPER_ARTIFACT_DIR"])/"context/task.json").read_text()); assert len(t["details"]) > 300000 and t["details"].endswith("must-be-present")' || exit 9
  printf 'verified task snapshot\n' > T1.txt
fi
printf '%s' "$p" | "$LOOPER_TEST_ORIGINAL_AGENT" "$@"
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	e.Config.CodexBin = bin
	e.Config.Verify = "test -s T1.txt"
	b := Backlog{Version: 1, Sources: []string{}, Goal: "large task", Tasks: []Task{{ID: "T1", Title: "one", Priority: 1, Status: "todo", Details: strings.Repeat("acceptance detail ", 20000) + "must-be-present"}}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	code, err := e.Run(context.Background())
	if code != 0 || err != nil {
		t.Fatalf("large task run: %d %v", code, err)
	}
	if data, err := os.ReadFile(filepath.Join(c.Root, "T1.txt")); err != nil || string(data) != "verified task snapshot\n" {
		t.Fatalf("task did not publish: %q %v", data, err)
	}
}

func TestFeedbackRemainsSeparateAcrossSessions(t *testing.T) {
	e, _ := idleEngine(t, reviewComplete)
	work, err := e.Repo.Workspace(identifier("context"), e.State.Tip, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(work) })
	first, err := prepareAgentContext(e.Repo, work, filepath.Join(e.Dir, "work-00-session-one"), e.State.Backlog, nil, "first evidence", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepareAgentContext(e.Repo, work, filepath.Join(e.Dir, "work-00-session-two"), e.State.Backlog, nil, "second evidence", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Feedback == second.Feedback {
		t.Fatal("sessions share feedback path")
	}
	if err := first.validate(); err != nil {
		t.Fatal(err)
	}
	if err := second.validate(); err != nil {
		t.Fatal(err)
	}
}
