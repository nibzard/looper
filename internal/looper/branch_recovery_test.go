package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResumeAfterCommittingPublishedFiles(t *testing.T) {
	for _, candidate := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "candidate"}[candidate], func(t *testing.T) {
			e, p := publicationFixture(t)
			seedRecoveryHead(t, e)
			if candidate {
				if err := e.Repo.Apply(p.Before, p.Tree); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := gitText(e.Config.Root, "add", "-A", "--", ".", ":(exclude)base"); err != nil {
				t.Fatal(err)
			}
			if candidate {
				if _, err := gitText(e.Config.Root, "add", "-f", "raw/sample.txt"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := gitText(e.Config.Root, "commit", "--allow-empty", "-qm", "Save published work"); err != nil {
				t.Fatal(err)
			}
			head, _ := e.Repo.Head()
			if err := e.initializeState(context.Background()); err != nil {
				t.Fatal(err)
			}
			if e.State.Head != head || e.State.Publication != nil {
				t.Fatal("resume did not finish publication")
			}
			if current, _ := e.Repo.Head(); current != head {
				t.Fatal("recovery rewrote user commit")
			}
			data, err := os.ReadFile(filepath.Join(e.Config.Root, "raw/sample.txt"))
			if err != nil || string(data) != "verified evidence\n" {
				t.Fatalf("candidate lost: %q %v", data, err)
			}
		})
	}
}

func TestResumeRejectsChangedContentAndBranch(t *testing.T) {
	for _, kind := range []string{"committed", "working", "staged", "branch", "detached", "backlog"} {
		t.Run(kind, func(t *testing.T) {
			e, _ := publicationFixture(t)
			seedRecoveryHead(t, e)
			old := e.State.Head
			if kind == "committed" {
				os.WriteFile(filepath.Join(e.Config.Root, "change.txt"), []byte("user content"), 0600)
			}
			if _, err := gitText(e.Config.Root, "add", "-A", "--", ".", ":(exclude)base"); err != nil {
				t.Fatal(err)
			}
			if _, err := gitText(e.Config.Root, "commit", "--allow-empty", "-qm", "Save work"); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "working", "staged":
				os.WriteFile(filepath.Join(e.Config.Root, "change.txt"), []byte("user content"), 0600)
				if kind == "staged" {
					if _, err := gitText(e.Config.Root, "add", "change.txt"); err != nil {
						t.Fatal(err)
					}
				}
			case "branch":
				if _, err := gitText(e.Config.Root, "checkout", "-qb", "other"); err != nil {
					t.Fatal(err)
				}
			case "detached":
				if _, err := gitText(e.Config.Root, "checkout", "-q", "--detach"); err != nil {
					t.Fatal(err)
				}
			case "backlog":
				os.WriteFile(e.Config.Todo, []byte(`{"schema_version":1,"source_files":[],"tasks":[],"goal":"changed"}`), 0600)
			}
			if err := e.initializeState(context.Background()); err == nil {
				t.Fatal("unsafe resume accepted")
			}
			saved, err := e.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if saved.Head != old || saved.Publication == nil {
				t.Fatal("rejected recovery changed retained state")
			}
		})
	}
}

func seedRecoveryHead(t *testing.T, e *Engine) {
	t.Helper()
	if _, err := gitText(e.Config.Root, "commit", "--allow-empty", "-qm", "Initial commit"); err != nil {
		t.Fatal(err)
	}
	e.State.Head, e.State.Branch = e.Repo.Head()
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
}

// scopedFixture returns one retained run whose plan scopes the retained
// attempt away from the repository root, with a durable result commit beside
// the pending publication. An owner commit outside the scope qualifies for
// the T046 acceptance; one inside it competes with the retained work.
func scopedFixture(t *testing.T, scope string) (*Engine, *Candidate) {
	t.Helper()
	e, p := publicationFixture(t)
	if _, err := gitText(e.Config.Root, "commit", "--allow-empty", "-qm", "Initial commit"); err != nil {
		t.Fatal(err)
	}
	e.State.Head, e.State.Branch = e.Repo.Head()
	e.State.Plan = &Plan{Verify: "true", Tasks: []Scope{{TaskID: "T001", Paths: []string{scope}}}}
	for _, a := range e.State.Attempts {
		a.ResultCommit = p.Commit
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e, p
}

// ownerCommit writes one file, commits everything the project tracks, and
// returns the new head: the shape of an owner editing the project while a
// controller keeps the run live.
func ownerCommit(t *testing.T, e *Engine, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(e.Config.Root, path)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Config.Root, path), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(e.Config.Root, "add", "-A", "--", ".", ":(exclude)base"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(e.Config.Root, "commit", "-qm", "Owner saves work"); err != nil {
		t.Fatal(err)
	}
	head, _ := e.Repo.Head()
	return head
}

func TestStartupFoldsOwnerCommitOutsideWriteScopes(t *testing.T) {
	e, p := scopedFixture(t, "internal/")
	head := ownerCommit(t, e, "NOTES.md", "owner note\n")
	oldTip := e.State.Tip
	if err := e.initializeState(context.Background()); err != nil {
		t.Fatalf("resume must accept an owner commit outside every write scope: %v", err)
	}
	if e.State.Head != head {
		t.Fatal("resume did not accept the owner commit")
	}
	if e.State.Publication != nil {
		t.Fatal("the pending publication must requeue against the folded base")
	}
	last := e.State.Attempts[len(e.State.Attempts)-1]
	if last.Phase != "ready" || last.ResultCommit != p.Commit {
		t.Fatalf("the retained result must requeue ready with its commit, got phase %q", last.Phase)
	}
	// The fold records a merge of the chain and the owner commit, so later
	// worktrees built from the tip carry the owner content.
	parents, err := gitText(e.Config.Root, "rev-list", "--parents", "-n", "1", e.State.Tip)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parents, head) || !strings.Contains(parents, oldTip) {
		t.Fatalf("the folded tip must descend from the chain and the owner commit: %s", parents)
	}
	if out, err := gitText(e.Config.Root, "ls-tree", "--name-only", e.State.Published); err != nil || !strings.Contains(out, "NOTES.md") {
		t.Fatalf("the folded published tree must carry the owner content: %q %v", out, err)
	}
	eventID(t, e, "SELECT id FROM events WHERE kind='recovery' AND message LIKE 'Accepted the project commit %'")
	// The acceptance is journaled: a restart against the folded baseline
	// recovers without another acceptance decision.
	if err := e.initializeState(context.Background()); err != nil {
		t.Fatalf("the folded baseline must resume cleanly: %v", err)
	}
}

func TestStartupRefusesOwnerCommitInsideWriteScopes(t *testing.T) {
	for _, table := range []struct {
		name, path, content string
		extra               bool
		plan                bool
	}{
		{name: "inside the write scope", path: "internal/owner.go", content: "package internal\n"},
		{name: "uncommitted edits ride along", path: "NOTES.md", content: "owner note\n", extra: true},
		{name: "no plan scopes the work", path: "NOTES.md", content: "owner note\n", plan: false},
	} {
		t.Run(table.name, func(t *testing.T) {
			e, p := scopedFixture(t, "internal/")
			if !table.plan {
				e.State.Plan = nil
				if err := e.save(); err != nil {
					t.Fatal(err)
				}
			}
			head := ownerCommit(t, e, table.path, table.content)
			old := e.State.Head
			if table.extra {
				if err := os.WriteFile(filepath.Join(e.Config.Root, "uncommitted.txt"), []byte("draft\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.initializeState(context.Background()); err == nil {
				t.Fatal("a competing owner commit must not resume")
			}
			saved, err := e.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if saved.Head != old || saved.Publication == nil || saved.Publication.Commit != p.Commit || saved.Tip != e.State.Tip {
				t.Fatal("a refused fold changed retained state")
			}
			if head == old {
				t.Fatal("the fixture did not move the head")
			}
		})
	}
}

func TestRefreshFoldsOwnerCommitWithRetainedWork(t *testing.T) {
	for _, table := range []struct {
		name, path string
		accepted   bool
	}{
		{name: "outside the write scope", path: "NOTES.md", accepted: true},
		{name: "inside the write scope", path: "internal/owner.go", accepted: false},
	} {
		t.Run(table.name, func(t *testing.T) {
			e, p := scopedFixture(t, "internal/")
			// Drop the publication: an idle boundary with only retained work
			// reaches refreshIdle, which must fold an accepted commit instead
			// of refusing the moved baseline.
			e.State.Publication = nil
			for _, a := range e.State.Attempts {
				a.Phase = "resume"
			}
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			head := ownerCommit(t, e, table.path, "owner note\n")
			err := e.refreshIdle()
			if table.accepted {
				if err != nil {
					t.Fatalf("the idle boundary must fold the owner commit: %v", err)
				}
				if e.State.Head != head || e.State.Tip == p.Base {
					t.Fatal("the idle boundary did not fold the owner commit into the baseline")
				}
				return
			}
			if err == nil {
				t.Fatal("a competing owner commit must not fold at the idle boundary")
			}
			if !strings.Contains(err.Error(), "branch moved") {
				t.Fatalf("the refusal must name the competing commit: %v", err)
			}
		})
	}
}

// TestLiveRunSurvivesOwnerCommit reproduces the two controller deaths of
// 2026-11-04 (T046): the owner committed an unrelated file to the project
// while a run was live, the next publication guard retained everything with
// "the project branch moved", and the next start refused until the session
// state was archived by hand. Both workers hold until the fixture releases
// them, so the owner commit lands while the controller is live and before any
// boundary can publish; the next boundary must log the accepted commit and
// continue instead of ending the run.
func TestLiveRunSurvivesOwnerCommit(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	startedOne, releaseOne := filepath.Join(dir, "one"), filepath.Join(dir, "release-one")
	startedTwo, releaseTwo := filepath.Join(dir, "two"), filepath.Join(dir, "release-two")
	// The stub answers planning and review at once. Each worker reports its
	// start and holds until the fixture releases it, so the owner commit
	// lands while both attempts are live and no boundary runs beside it.
	script := `#!/bin/bash
out=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$a"; fi
  prev="$a"
done
p="$(cat)"
result="$LOOPER_FAKE_RESULT"
if printf '%s' "$p" | grep -q "Plan execution for Looper"; then
  result="$LOOPER_FAKE_PLAN"
elif printf '%s' "$p" | grep -q "^Complete task "; then
  id="$(printf '%s' "$p" | sed -n 's/^Complete task \(T[0-9][0-9A-Za-z_-]*\):.*/\1/p')"
  result="$(printf '{"task_id":"%s","status":"done","summary":"implemented","files":[],"blockers":[],"followups":[]}' "$id")"
  if [ "$id" = T001 ]; then
    touch "` + startedOne + `"
    while [ ! -f "` + releaseOne + `" ]; do sleep 0.02; done
  elif [ "$id" = T002 ]; then
    touch "` + startedTwo + `"
    while [ ! -f "` + releaseTwo + `" ]; do sleep 0.02; done
  fi
elif [ -n "$LOOPER_FAKE_REVIEW" ] && printf '%s' "$p" | grep -q "review pass for Looper"; then
  result="$LOOPER_FAKE_REVIEW"
fi
if [ -n "$out" ]; then printf '%s' "$result" > "$out"; fi
if [ -n "$result" ]; then printf '{"structured_output":%s}\n' "$result"; fi
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_FAKE_PLAN", `{"setup":"","verify":"true","tasks":[{"task_id":"T001","write_paths":["one/"]},{"task_id":"T002","write_paths":["two/"]}]}`)
	t.Setenv("LOOPER_FAKE_REVIEW", reviewComplete)
	c := repoConfig(t, "run", "--jobs", "2")
	c.CodexBin, c.ClaudeBin = bin, bin
	c.ReviewEvery = 100
	c.Once = true
	// One commit before the run starts, so the owner's commit is a
	// descendant of the retained baseline.
	if err := os.WriteFile(filepath.Join(c.Root, "seed.md"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "add", "--", "seed.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "commit", "-qm", "seed"); err != nil {
		t.Fatal(err)
	}
	b := Backlog{Version: 1, Sources: []string{}, Goal: "survive an owner commit", Tasks: []Task{
		{ID: "T001", Title: "first task", Priority: 1, Status: "todo"},
		{ID: "T002", Title: "second task", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), testBudget(90*time.Second))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("controller panic: %v", r)
			}
		}()
		code, err := e.Run(ctx)
		if err == nil && code != 0 {
			err = fmt.Errorf("unexpected exit %d", code)
		}
		done <- err
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testBudget(10 * time.Second)):
			t.Error("run did not stop")
		}
	}()
	waitForAddTaskPath(t, startedOne, "the first worker to start")
	waitForAddTaskPath(t, startedTwo, "the second worker to start")
	// The owner commits an unrelated file while the controller is live and
	// both workers hold: no boundary publishes beside the commit itself.
	if err := os.WriteFile(filepath.Join(c.Root, "NOTES.md"), []byte("owner note\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "add", "--", "NOTES.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "commit", "-qm", "Owner saves a note"); err != nil {
		t.Fatal(err)
	}
	head, _ := e.Repo.Head()
	if err := os.WriteFile(releaseOne, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(releaseTwo, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		done <- err
		if err != nil {
			t.Fatalf("the run ended instead of accepting the owner commit: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("run timed out")
	}
	for _, id := range []string{"T001", "T002"} {
		if task := e.State.Backlog.Task(id); task == nil || task.Status != "done" {
			t.Fatalf("%s did not complete: %+v", id, task)
		}
	}
	if e.State.Head != head {
		t.Fatalf("the run did not accept the owner commit: expected head %s, saved %s", head, e.State.Head)
	}
	// The next boundary logged what it accepted.
	accepted := eventID(t, e, "SELECT id FROM events WHERE kind='recovery' AND message LIKE 'Accepted the project commit %'")
	start := eventID(t, e, "SELECT id FROM events WHERE kind='start' AND message LIKE 'T001%'")
	if accepted < start {
		t.Fatalf("the accepted commit was not logged after the work started: %d < %d", accepted, start)
	}
	// The folded baseline carries the owner content into the published tree.
	if out, err := gitText(c.Root, "ls-tree", "--name-only", e.State.Published); err != nil || !strings.Contains(out, "NOTES.md") {
		t.Fatalf("the published tree lost the owner content: %q %v", out, err)
	}
	if data, err := os.ReadFile(filepath.Join(c.Root, "NOTES.md")); err != nil || string(data) != "owner note\n" {
		t.Fatalf("the owner file changed: %q %v", data, err)
	}
}

func TestDoctorExplainsRetainedBranchAndShutdown(t *testing.T) {
	e, _ := publicationFixture(t)
	seedRecoveryHead(t, e)
	if _, err := gitText(e.Config.Root, "commit", "--allow-empty", "-qm", "Move head"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.Event("shutdown", "branch moved", sessionExit{Session: "old", Reason: "error", ExitCode: 1}); err != nil {
		t.Fatal(err)
	}
	// Release the repository lock through a separate fixture config: doctor
	// observes the same saved state without starting a controller.
	c := e.Config
	c.Root = t.TempDir()
	if _, err := gitText(c.Root, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	_, dir := projectPaths(c)
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(e.State); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Event("shutdown", "branch moved", sessionExit{Session: "old", Reason: "error", ExitCode: 1}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := doctorJSON(c, &out); code != 0 {
		t.Fatalf("doctor exit %d: %s", code, out.String())
	}
	var report struct {
		Findings    []HealthFinding
		Observation Observation
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, f := range report.Findings {
		found[f.Code] = true
	}
	if !found["branch_moved"] || !found["last_shutdown"] {
		t.Fatalf("missing diagnosis: %s", out.String())
	}
	if len(report.Observation.Workers) != 1 || !report.Observation.Workers[0].Retained {
		t.Fatal("retained work appears active")
	}
}
