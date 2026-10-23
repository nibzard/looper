package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
	for _, kind := range []string{"committed", "working", "staged", "branch", "backlog"} {
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
