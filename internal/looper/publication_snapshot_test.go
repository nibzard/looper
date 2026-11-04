package looper

import (
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

func publicationFixture(t *testing.T) (*Engine, *Candidate) {
	t.Helper()
	c := repoConfig(t, "run")
	b := Backlog{Version: 1, Sources: []string{}, Goal: "test publication", Tasks: []Task{
		{ID: "T001", Title: "retain evidence", Priority: 1, Status: "doing"},
	}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Root, ".gitignore"), []byte("/raw/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	if err = e.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	work, err := e.Repo.Workspace(identifier("task"), e.State.Tip, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(work) })
	if err = os.MkdirAll(filepath.Join(work, "raw"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(work, "raw", "sample.txt"), []byte("verified evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = gitText(work, "add", "-f", "--", "raw/sample.txt"); err != nil {
		t.Fatal(err)
	}
	tree, err := e.Repo.Snapshot(work)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := e.Repo.Commit(tree, []string{e.State.Tip}, "Add evidence")
	if err != nil {
		t.Fatal(err)
	}
	a := &Attempt{ID: "task-evidence", Task: *e.State.Backlog.Task("T001"), Phase: "verifying", Summary: &Summary{TaskID: "T001", Status: "done", Summary: "retained evidence"}}
	e.State.Attempts = append(e.State.Attempts, a)
	p := &Candidate{Base: e.State.Tip, Commit: commit, Tree: tree, Before: e.State.Published, AttemptID: a.ID, OK: true}
	e.State.Publication = p
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	return e, p
}

// TestPublishOverlapsConcurrentWorkerWrites reproduces the publication race
// recorded on 2026-11-04 in NOTES.md: while one task published, a concurrent
// worker's atomic writes renamed .looper-write-* temporaries under the runtime
// tree, the root scan of publish() stated a vanished name, git exited 128, and
// the controller ended with an error shutdown. The T002 worker keeps that
// churn alive across T001's publication. This is the overlap the earlier
// add-task fixture had to scope away with one shared write path.
func TestPublishOverlapsConcurrentWorkerWrites(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	hammering, release := filepath.Join(dir, "hammering"), filepath.Join(dir, "release")
	// The stub answers planning and review immediately. The T001 worker
	// returns only after T002 churns temporary files, so T001's publication
	// runs inside that churn. The T002 worker creates and renames
	// .looper-write-* temporaries in its artifact directory, the shape
	// atomicWrite produces, until the fixture releases it.
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
  if [ "$id" = T002 ]; then
    touch "` + hammering + `"
    for s in 1 2 3 4; do
      (
        h=0
        while [ ! -f "` + release + `" ]; do
          t="$LOOPER_ARTIFACT_DIR/.looper-write-$s-$h"
          echo payload > "$t" 2>/dev/null || break
          mv "$t" "$LOOPER_ARTIFACT_DIR/hammer-$s" 2>/dev/null || break
          h=$((h+1))
        done
      ) &
    done
    while [ ! -f "` + release + `" ]; do sleep 0.02; done
    wait
  else
    while [ ! -f "` + hammering + `" ]; do sleep 0.02; done
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
	// Separate write scopes keep both workers in flight at once, so T001's
	// publication overlaps T002's writes.
	b := Backlog{Version: 1, Sources: []string{}, Goal: "publish beside a running worker", Tasks: []Task{
		{ID: "T001", Title: "publishing task", Priority: 1, Status: "todo"},
		{ID: "T002", Title: "overlapping task", Priority: 1, Status: "todo"},
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
		case <-time.After(10 * time.Second):
			t.Error("run did not stop")
		}
	}()
	waitForAddTaskPath(t, hammering, "the overlapping worker to start")
	// T001 reaches done only after publish() and applyResult() complete, so
	// the exported task file marks the publication as finished. A run that
	// ends before that point failed the publication itself.
	deadline := time.Now().Add(30 * time.Second)
	for {
		data, err := os.ReadFile(c.Todo)
		if err == nil {
			var exported Backlog
			if json.Unmarshal(data, &exported) == nil {
				if task := exported.Task("T001"); task != nil && task.Status == "done" {
					break
				}
			}
		}
		select {
		case err = <-done:
			done <- err
			t.Fatalf("the run ended before T001 published: %v", err)
		default:
		}
		if !time.Now().Before(deadline) {
			t.Fatal("timed out waiting for T001 to publish beside the running worker")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err = os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("run timed out")
	}
	for _, id := range []string{"T001", "T002"} {
		if task := e.State.Backlog.Task(id); task == nil || task.Status != "done" {
			t.Fatalf("%s did not complete: %+v", id, task)
		}
	}
	// The publication ran strictly inside the second worker's attempt.
	second := eventID(t, e, `SELECT id FROM events WHERE kind='start' AND message LIKE 'T002  %'`)
	first := eventID(t, e, `SELECT id FROM events WHERE kind='result' AND message LIKE 'T001  %'`)
	if second > first {
		t.Fatalf("the publication ran outside the overlapping attempt: start %d, result %d", second, first)
	}
}

func TestPublishIncludesIgnoredCandidateFiles(t *testing.T) {
	for _, alreadyApplied := range []bool{false, true} {
		name := "first apply"
		if alreadyApplied {
			name = "restart after apply"
		}
		t.Run(name, func(t *testing.T) {
			e, p := publicationFixture(t)
			indexPath := filepath.Join(e.Config.Root, ".git", "index")
			index, err := os.ReadFile(indexPath)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if alreadyApplied {
				if err = e.Repo.Apply(p.Before, p.Tree); err != nil {
					t.Fatal(err)
				}
			}
			if err = os.MkdirAll(filepath.Join(e.Config.Root, "raw"), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(e.Config.Root, "raw", "scratch.txt"), []byte("untracked scratch\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err = e.publish(); err != nil {
				t.Fatalf("ignored evidence must publish: %v", err)
			}
			if e.State.Publication != nil || e.State.Published != p.Tree {
				t.Fatal("publication journal did not complete")
			}
			got, err := os.ReadFile(filepath.Join(e.Config.Root, "raw", "sample.txt"))
			if err != nil || string(got) != "verified evidence\n" {
				t.Fatalf("evidence changed: %q %v", got, err)
			}
			gotIndex, _ := os.ReadFile(indexPath)
			if string(gotIndex) != string(index) {
				t.Fatal("publication changed the user index")
			}
			if err = e.initialize(context.Background()); err != nil {
				t.Fatalf("published evidence must survive restart: %v", err)
			}
		})
	}
}

func TestPublishChecksDeletedIgnoredCandidateFile(t *testing.T) {
	for _, recreated := range []bool{false, true} {
		name := "delete ignored evidence"
		if recreated {
			name = "reject recreated evidence"
		}
		t.Run(name, func(t *testing.T) {
			e, added := publicationFixture(t)
			if err := e.publish(); err != nil {
				t.Fatal(err)
			}
			work, err := e.Repo.Workspace(identifier("delete"), added.Commit, e.State.Backlog)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { e.Repo.Remove(work) })
			if err = os.Remove(filepath.Join(work, "raw", "sample.txt")); err != nil {
				t.Fatal(err)
			}
			tree, err := e.Repo.Snapshot(work)
			if err != nil {
				t.Fatal(err)
			}
			commit, err := e.Repo.Commit(tree, []string{added.Commit}, "Delete evidence")
			if err != nil {
				t.Fatal(err)
			}
			p := &Candidate{Base: added.Commit, Commit: commit, Tree: tree, Before: added.Tree, AttemptID: added.AttemptID, OK: true}
			e.State.Publication = p
			path := filepath.Join(e.Config.Root, "raw", "sample.txt")
			if recreated {
				if err = e.Repo.Apply(p.Before, p.Tree); err != nil {
					t.Fatal(err)
				}
				if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, []byte("user replacement\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err = e.publish(); err == nil {
					t.Fatal("a recreated ignored file must prevent publication")
				}
				got, err := os.ReadFile(path)
				if err != nil || string(got) != "user replacement\n" {
					t.Fatal("publication changed the recreated file")
				}
			} else {
				if err = e.publish(); err != nil {
					t.Fatal(err)
				}
				if _, err = os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("ignored evidence was not deleted")
				}
			}
		})
	}
}

func TestPublishRejectsChangedIgnoredCandidateFile(t *testing.T) {
	e, p := publicationFixture(t)
	if err := e.Repo.Apply(p.Before, p.Tree); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.Config.Root, "raw", "sample.txt")
	if err := os.WriteFile(path, []byte("user edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.publish(); err == nil {
		t.Fatal("a changed ignored file must prevent publication")
	}
	if e.State.Publication == nil || e.State.Published != p.Before {
		t.Fatal("failed publication must retain its journal and baseline")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "user edit\n" {
		t.Fatal("failed publication overwrote the user edit")
	}
}

func TestPublishExplainsUnrelatedLocalEdits(t *testing.T) {
	for _, alreadyApplied := range []bool{false, true} {
		name := "before apply"
		if alreadyApplied {
			name = "after apply"
		}
		t.Run(name, func(t *testing.T) {
			e, p := publicationFixture(t)
			if alreadyApplied {
				if err := e.Repo.Apply(p.Before, p.Tree); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(e.Config.Root, "local.txt")
			if err := os.WriteFile(path, []byte("local work\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := gitText(e.Config.Root, "add", "--", "local.txt"); err != nil {
				t.Fatal(err)
			}
			indexPath := filepath.Join(e.Config.Root, ".git", "index")
			index, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			err = e.publish()
			if err == nil {
				t.Fatal("local edits must prevent publication")
			}
			for _, want := range []string{"T001", "local.txt", "accepted tree " + p.Before, "verified candidate tree " + p.Tree, e.Dir, "Preserve your local edits", "Git HEAD can be older", "without marking the task done"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("diagnostic does not contain %q: %v", want, err)
				}
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != "local work\n" {
				t.Fatal("publication changed local work")
			}
			gotIndex, err := os.ReadFile(indexPath)
			if err != nil || string(gotIndex) != string(index) {
				t.Fatal("publication changed the user index")
			}
			_, err = os.Stat(filepath.Join(e.Config.Root, "raw", "sample.txt"))
			if alreadyApplied && err != nil || !alreadyApplied && !os.IsNotExist(err) {
				t.Fatalf("publication changed candidate files: %v", err)
			}
			saved, err := e.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			for _, state := range []*State{e.State, saved} {
				if state.Publication == nil || state.Publication.Commit != p.Commit || state.Published != p.Before || state.Tip != p.Base || state.Attempts[len(state.Attempts)-1].Phase != "verifying" || state.Backlog.Task("T001").Status != "doing" {
					t.Fatal("failed publication did not retain unfinished work")
				}
			}
		})
	}
}
