package looper

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
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
