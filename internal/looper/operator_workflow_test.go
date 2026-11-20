package looper

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The OW-11 combined acceptance fixture. The seven steps of the operator
// workflow run in order against a temporary repository and stub agents:
// headless configuration, one accepted task beside a typed quota failure,
// checked publication, a provider switch through fresh requeue, a restart
// that dispatches under the new worker, doctor's structured classes, and a
// second publication with a clean resume. The crash variants of steps 3 and
// 4 live in the publish and requeue suites (prepared and committed journal
// resolution, export-failure rollback); this fixture covers the intact path.

// operatorWorkflowWorker writes one stub agent that finishes worker prompts.
// The task identifier in the prompt selects the reported task, each answer
// creates one file in the attempt worktree, and the quota task fails with
// the account's own quota text — the typed evidence a configuration failure
// carries.
func operatorWorkflowWorker(t *testing.T, name, quotaTask string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), name)
	script := `#!/bin/bash
out=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$a"; fi
  prev="$a"
done
p="$(cat)"
result=""
if printf '%s' "$p" | grep -q "review pass for Looper"; then
  result='{"status":"reviewed","summary":"no findings","findings":[],"complete":false}'
elif printf '%s' "$p" | grep -q "Complete task "; then
  tid="$(printf '%s' "$p" | grep -oE 'T[0-9]{3}' | head -n1)"
  if [ -z "$tid" ]; then exit 1; fi
  if [ "$tid" = "` + quotaTask + `" ]; then
    printf '%s\n' '{"type":"error","message":"usage limit reached: this account quota is exhausted"}'
    exit 1
  fi
  printf 'accepted work\n' > "work-$tid-` + name + `.txt"
  result="{\"task_id\":\"$tid\",\"status\":\"done\",\"summary\":\"implemented\",\"files\":[\"work-$tid-` + name + `.txt\"],\"blockers\":[],\"followups\":[],\"unblock_when\":\"\"}"
fi
if [ -n "$result" ]; then
  # The Claude adapter reads the result from the event stream; Codex reads
  # the last-message file. Serve both.
  printf '{"type":"structured_output","structured_output":%s}\n' "$result"
  if [ -n "$out" ]; then printf '%s' "$result" > "$out"; fi
fi
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return bin
}

type operatorWorkflow struct {
	c   Config
	dir string
}

// workflowCLI drives one command through Main and fails the test on a
// nonzero exit.
func (w *operatorWorkflow) cli(t *testing.T, args ...string) string {
	t.Helper()
	var out, stderr strings.Builder
	if code := Main(args, &out, &stderr); code != exitOK {
		t.Fatalf("%s exited %d\nstdout: %s\nstderr: %s", args, code, out.String(), stderr.String())
	}
	return out.String()
}

// parseAgain re-reads the project configuration from the same working
// directory, so the saved profile decides the roles for the next run.
func (w *operatorWorkflow) parseAgain(t *testing.T) {
	t.Helper()
	c, err := Parse([]string{"run", "--once"})
	if err != nil {
		t.Fatal(err)
	}
	c.RetryLimit, c.RetryDelay = 1, time.Millisecond
	w.c = c
	_, w.dir = projectPaths(c)
}

// runEngine starts one controller on the saved run, installs a plan that
// covers every current task so no planner runs, runs to the --once
// boundary, and returns the exit code. The store and repository lock close
// before it returns, so later CLI steps see a stopped run.
func (w *operatorWorkflow) runEngine(t *testing.T, timeout time.Duration) int {
	t.Helper()
	e, lock, err := NewEngine(w.c, io.Discard)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	closed := false
	closeAll := func() {
		if closed {
			return
		}
		closed = true
		e.Store.Close()
		lock.Close()
	}
	t.Cleanup(closeAll)
	if err = e.initialize(context.Background()); err != nil {
		closeAll()
		t.Fatalf("initialize: %v", err)
	}
	scopes := make([]Scope, 0, len(e.State.Backlog.Tasks))
	contracts := make([]string, 0, len(e.State.Backlog.Tasks))
	for _, task := range e.State.Backlog.Tasks {
		scopes = append(scopes, Scope{TaskID: task.ID, Paths: []string{"."}})
		contracts = append(contracts, task.Contract())
	}
	e.State.Plan = &Plan{Verify: "true", Tasks: scopes}
	e.State.PlanSignature = hashJSON(struct {
		Contracts     []string
		Setup, Verify string
	}{contracts, e.Config.Setup, e.Config.Verify})
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := runCtx(t, timeout)
	defer cancel()
	code, err := e.Run(ctx)
	if err != nil && ctx.Err() == nil {
		closeAll()
		t.Fatalf("run: %v", err)
	}
	closeAll()
	return code
}

// state reads the saved run through the store a stopped run keeps.
func (w *operatorWorkflow) state(t *testing.T) *State {
	t.Helper()
	return savedState(t, w.dir)
}

func TestOperatorWorkflowAcceptanceFixture(t *testing.T) {
	// The repository lives for the whole fixture: repoConfig's working
	// directory must outlive every subtest, so it is taken here, not inside
	// a step whose cleanup would restore the previous directory.
	w := &operatorWorkflow{}
	repoConfig(t)
	w.parseAgain(t)
	root := w.c.Root
	for _, kv := range [][2]string{{"user.name", "Fixture Owner"}, {"user.email", "owner@example.test"}} {
		if _, err := gitText(root, "config", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "seed.md"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "--", "seed.md"}, {"commit", "-q", "-m", "seed"}} {
		if _, err := gitText(root, args...); err != nil {
			t.Fatal(err)
		}
	}
	// stub-a accepts work but fails the quota task with typed evidence;
	// stub-b is the provider the operator switches to. Both live for the
	// whole fixture, because the machine catalog records their paths.
	binA := operatorWorkflowWorker(t, "stub-a", "T002")
	binB := operatorWorkflowWorker(t, "stub-b", "")

	t.Run("define profiles headlessly", func(t *testing.T) {
		out := w.cli(t, "config", "--scope", "machine",
			"--set", "agents.stub-a.adapter=claude", "--set", "agents.stub-a.command="+binA,
			"--set", "agents.stub-b.adapter=claude", "--set", "agents.stub-b.command="+binB,
			"--contract", "v2", "--json")
		var machineDoc struct {
			Kind     string               `json:"kind"`
			Outcomes []configPatchOutcome `json:"outcomes"`
		}
		if err := json.Unmarshal([]byte(out), &machineDoc); err != nil {
			t.Fatal(err)
		}
		if machineDoc.Kind != configSavedKind || len(machineDoc.Outcomes) != 4 {
			t.Fatalf("machine save: %s", out)
		}
		for _, o := range machineDoc.Outcomes {
			if o.Outcome != "saved-restart-required" {
				t.Fatalf("machine outcome %s: %+v", o.Field, o)
			}
		}
		saved := w.cli(t, "config", "--set", "worker=stub-a", "--set", "reviewer=stub-b", "--contract", "v2", "--json")
		var projectDoc struct {
			Kind     string               `json:"kind"`
			Revision string               `json:"revision"`
			Outcomes []configPatchOutcome `json:"outcomes"`
		}
		if err := json.Unmarshal([]byte(saved), &projectDoc); err != nil {
			t.Fatal(err)
		}
		if projectDoc.Kind != configSavedKind || len(projectDoc.Outcomes) != 2 || len(projectDoc.Revision) != 64 {
			t.Fatalf("project save: %s", saved)
		}
		// Parse again: the saved profile must decide the roles for the run.
		w.parseAgain(t)
		if w.c.Agent != "stub-a" || w.c.ReviewAgent != "stub-b" {
			t.Fatalf("saved roles: worker %q reviewer %q", w.c.Agent, w.c.ReviewAgent)
		}
	})

	t.Run("accepted work beside a typed quota failure", func(t *testing.T) {
		b := Backlog{Version: 1, Sources: []string{}, Goal: "operator workflow", Tasks: []Task{
			{ID: "T001", Title: "accepted work", Priority: 1, Status: "todo"},
			{ID: "T002", Title: "quota limited work", Priority: 1, Status: "todo"},
			{ID: "T003", Title: "dependent work", Priority: 1, Status: "todo", DependsOn: []string{"T002"}},
		}}
		if err := writeJSON(w.c.Todo, b); err != nil {
			t.Fatal(err)
		}
		if code := w.runEngine(t, 120*time.Second); code != 2 {
			t.Fatalf("run exited %d; want 2 with retained blocked work", code)
		}
		s := w.state(t)
		if task := s.Backlog.Task("T001"); task.Status != "done" {
			t.Fatalf("T001: %s", task.Status)
		}
		blocked := s.Backlog.Task("T002")
		if blocked.Status != "blocked" || blocked.BlockReason == nil {
			t.Fatalf("T002: %+v", blocked)
		}
		if blocked.BlockReason.Class != blockClassFailure {
			t.Fatalf("T002 reason: %+v", blocked.BlockReason)
		}
		quotaAttempts := 0
		for _, a := range s.Attempts {
			if a.Task.ID == "T002" {
				quotaAttempts++
			}
		}
		if quotaAttempts != 1 {
			t.Fatalf("the quota failure retried: %d attempts", quotaAttempts)
		}
		if task := s.Backlog.Task("T003"); task.Status != "todo" {
			t.Fatalf("the gated dependent dispatched: %+v", task)
		}
	})

	t.Run("publish the accepted tree", func(t *testing.T) {
		doc, code, raw := publishPreviewDoc(t, w.c)
		if code != exitOK {
			t.Fatalf("preview exit %d: %s", code, raw)
		}
		if !doc.Publishable {
			guards, _ := json.Marshal(doc.Guards)
			t.Fatalf("preview is not publishable: %s", guards)
		}
		if !containsString(doc.Changes.Paths, "work-T001-stub-a.txt") {
			t.Fatalf("changed paths: %v", doc.Changes.Paths)
		}
		head, err := gitText(w.c.Root, "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if code, out, stderr := publishRunCLI(t, "--apply", "--token", doc.Token, "--message", "Publish accepted work", "--contract", "v2", "--json"); code != exitOK {
			t.Fatalf("apply exit %d: %s%s", code, out, stderr)
		}
		newHead, err := gitText(w.c.Root, "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if newHead == head {
			t.Fatal("publication did not move the branch")
		}
		parents, err := gitText(w.c.Root, "rev-list", "--parents", "-n", "1", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Fields(parents)[1] != head {
			t.Fatalf("publication parent: %s", parents)
		}
		status, err := gitText(w.c.Root, "status", "--porcelain")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(status, "work-T001") {
			t.Fatalf("the accepted change stayed uncommitted: %s", status)
		}
		tracked, err := gitText(w.c.Root, "ls-files")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(tracked, filepath.Base(w.c.Todo)) {
			t.Fatal("the untracked task file entered the project commit")
		}
		// The failed attempt stays retained evidence after publication.
		kept := false
		for _, a := range w.state(t).Attempts {
			if a.Task.ID == "T002" && a.Phase == "blocked" {
				kept = true
			}
		}
		if !kept {
			t.Fatal("publication lost the retained failed attempt")
		}
	})

	t.Run("switch provider and fresh requeue", func(t *testing.T) {
		var artifacts []string
		for _, a := range w.state(t).Attempts {
			if a.Task.ID == "T002" && a.Directory != "" {
				artifacts = attemptArtifactList(t, a.Directory)
			}
		}
		if len(artifacts) == 0 {
			t.Fatal("the failed attempt kept no artifacts to compare")
		}
		w.cli(t, "config", "--set", "worker=stub-b", "--contract", "v2", "--json")
		w.cli(t, "requeue", "T002", "--fresh", "--contract", "v2", "--json")
		s := w.state(t)
		task := s.Backlog.Task("T002")
		if task.Status != "todo" || len(task.Blockers) != 0 {
			t.Fatalf("requeue did not reopen T002: %+v", task)
		}
		retired := 0
		for _, a := range s.Attempts {
			if a.Task.ID == "T002" && a.Retired != nil && a.Retired.Reason == "fresh_requeue" {
				retired++
				if after := attemptArtifactList(t, a.Directory); !slices.Equal(after, artifacts) {
					t.Fatalf("retirement changed the artifacts:\nbefore: %v\nafter: %v", artifacts, after)
				}
			}
		}
		if retired != 1 {
			t.Fatalf("retired attempts: %d", retired)
		}
		if dep := s.Backlog.Task("T003"); len(dep.DependsOn) != 1 || dep.DependsOn[0] != "T002" {
			t.Fatalf("the dependent lost its connection: %+v", dep)
		}
	})

	t.Run("restart dispatches under the new worker", func(t *testing.T) {
		// The restart also imports the operator's blocked fixtures.
		tasks := append([]Task(nil), w.state(t).Backlog.Tasks...)
		tasks = append(tasks,
			Task{ID: "T004", Title: "operator gate", Priority: 1, Status: "blocked", Tags: []string{operatorTaskTag}, Blockers: []string{"an owner must provide credentials"}},
			Task{ID: "T005", Title: "deferred work", Priority: 1, Status: "blocked", Tags: []string{deferredTaskTag}, UnblockWhen: "the release ships"},
			Task{ID: "T006", Title: "opaque block", Priority: 1, Status: "blocked", Blockers: []string{"unclear"}},
		)
		b := Backlog{Version: 1, Sources: []string{}, Goal: "operator workflow", Tasks: tasks}
		if err := writeJSON(w.c.Todo, b); err != nil {
			t.Fatal(err)
		}
		w.parseAgain(t)
		if w.c.Agent != "stub-b" {
			t.Fatalf("the switched worker did not save: %q", w.c.Agent)
		}
		if code := w.runEngine(t, 120*time.Second); code != 2 {
			t.Fatalf("run exited %d; want 2 with the blocked fixtures retained", code)
		}
		s := w.state(t)
		var fresh *Attempt
		for i := range s.Attempts {
			if a := s.Attempts[i]; a.Task.ID == "T002" && a.Retired == nil && a.Phase == "done" {
				fresh = s.Attempts[i]
			}
		}
		if fresh == nil {
			t.Fatal("no fresh attempt completed T002")
		}
		if fresh.Agent != "stub-b" {
			t.Fatalf("the fresh attempt ran under %q, not stub-b", fresh.Agent)
		}
		if task := s.Backlog.Task("T003"); task.Status != "done" {
			t.Fatalf("the dependent did not finish after its gate: %+v", task)
		}
		if task := s.Backlog.Task("T001"); task.Status != "done" {
			t.Fatalf("the published task regressed: %+v", task)
		}
	})

	t.Run("doctor exposes structured classes", func(t *testing.T) {
		out := w.cli(t, "doctor", "--contract", "v2", "--json")
		var doc struct {
			Findings []struct {
				Code        string `json:"code"`
				Severity    string `json:"severity"`
				TaskID      string `json:"task_id"`
				ReasonClass string `json:"reason_class"`
				Retry       string `json:"retry"`
			} `json:"findings"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, f := range doc.Findings {
			seen[f.TaskID] = true
			switch f.TaskID {
			case "T004":
				if f.ReasonClass != blockClassOperator || f.Severity != "info" {
					t.Fatalf("operator finding: %+v", f)
				}
			case "T005":
				if f.ReasonClass != blockClassDeferred || f.Severity != "info" {
					t.Fatalf("deferred finding: %+v", f)
				}
			case "T006":
				if f.ReasonClass != blockClassUnknown || f.Severity != "warning" {
					t.Fatalf("unknown finding: %+v", f)
				}
			}
		}
		for _, id := range []string{"T004", "T005", "T006"} {
			if !seen[id] {
				t.Fatalf("doctor reported no finding for %s: %s", id, out)
			}
		}
	})

	t.Run("publish again and resume", func(t *testing.T) {
		doc, code, raw := publishPreviewDoc(t, w.c)
		if code != exitOK {
			t.Fatalf("preview exit %d: %s", code, raw)
		}
		if !doc.Publishable {
			guards, _ := json.Marshal(doc.Guards)
			t.Fatalf("second preview is not publishable: %s", guards)
		}
		if !containsString(doc.Changes.Paths, "work-T002-stub-b.txt") {
			t.Fatalf("second changed paths: %v", doc.Changes.Paths)
		}
		w.cli(t, "publish", "--apply", "--token", doc.Token, "--message", "Publish fresh work", "--contract", "v2", "--json")
		published, err := gitText(w.c.Root, "log", "--format=%s", "--grep=^Publish ")
		if err != nil {
			t.Fatal(err)
		}
		if got := len(nonEmptyLines(published)); got != 2 {
			t.Fatalf("publication commits: %d (%s)", got, published)
		}
		// One more resume must repeat no completed work and lose no history.
		w.parseAgain(t)
		if code := w.runEngine(t, 120*time.Second); code != 2 {
			t.Fatalf("resume exited %d; the blocked fixtures must stay retained", code)
		}
		s := w.state(t)
		counts := map[string]int{}
		for _, a := range s.Attempts {
			counts[a.Task.ID]++
		}
		if counts["T001"] != 1 || counts["T002"] != 2 || counts["T003"] != 1 {
			t.Fatalf("attempt counts after resume: %+v", counts)
		}
		for _, id := range []string{"T001", "T002", "T003"} {
			if task := s.Backlog.Task(id); task.Status != "done" {
				t.Fatalf("%s after resume: %+v", id, task)
			}
		}
		for _, id := range []string{"T004", "T005", "T006"} {
			if task := s.Backlog.Task(id); task.Status != "blocked" {
				t.Fatalf("%s after resume: %+v", id, task)
			}
		}
	})
}

// nonEmptyLines counts the message lines one git log listing returned.
func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// attemptArtifactList names the retained files of one attempt directory so a
// later step can prove retirement changed nothing.
func attemptArtifactList(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		out = append(out, rel+" "+strconv.FormatInt(info.Size(), 10))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}
