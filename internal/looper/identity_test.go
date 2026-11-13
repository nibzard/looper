package looper

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// savedStateEngine bootstraps a fresh project once, so the store holds saved
// state for the given task file and the project keeps that file on disk.
func savedStateEngine(t *testing.T, todo string) (Config, []string) {
	t.Helper()
	valid, err := json.Marshal(testBacklog())
	if err != nil {
		t.Fatal(err)
	}
	bin, _ := fakeAgent(t, string(valid))
	c := repoConfig(t, "run", todo)
	c.CodexBin, c.ClaudeBin = bin, bin
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	dir := e.Dir
	bootstraps, _ := filepath.Glob(filepath.Join(dir, "bootstrap-*"))
	if err = e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	return c, bootstraps
}

// agentCalls counts recorded fake-agent invocations for a later engine.
func agentCalls(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "calls")
	t.Setenv("LOOPER_FAKE_LOG", path)
	t.Cleanup(func() {
		if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
			t.Errorf("unexpected agent invocations: %q", data)
		}
	})
	return path
}

func TestStartupRejectsTaskFileMismatchBeforeBootstrap(t *testing.T) {
	// The saved run used a task file with a different name; the mismatch is
	// the point, and the name carries a .json suffix so the positional is a
	// task file rather than an unknown command (AX-026).
	c, bootstraps := savedStateEngine(t, "init.json")
	agentCalls(t)
	selected := c
	selected.Todo = filepath.Join(c.Root, "to-do.json")
	e, lock, err := NewEngine(selected, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	err = e.initialize(context.Background())
	if err == nil || !strings.Contains(err.Error(), `init.json"`) || !strings.Contains(err.Error(), `to-do.json"`) {
		t.Fatalf("expected a task-file identity error naming both paths, got %v", err)
	}
	if !strings.Contains(err.Error(), "rerun with the saved task file") || strings.Contains(err.Error(), "move it") {
		t.Fatalf("identity error must recommend the saved task path, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(c.Root, "to-do.json")); !os.IsNotExist(statErr) {
		t.Fatal("identity mismatch created a new task file")
	}
	after, _ := filepath.Glob(filepath.Join(e.Dir, "bootstrap-*"))
	if len(after) != len(bootstraps) {
		t.Fatalf("identity mismatch ran bootstrap: %v", after)
	}
}

func TestStartupResumesMatchingSavedState(t *testing.T) {
	c, bootstraps := savedStateEngine(t, "to-do.json")
	agentCalls(t)
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	if err = e.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.State == nil || e.State.Todo != c.Todo || len(e.State.Backlog.Tasks) != 4 {
		t.Fatalf("matching state not resumed: %+v", e.State)
	}
	after, _ := filepath.Glob(filepath.Join(e.Dir, "bootstrap-*"))
	if len(after) != len(bootstraps) {
		t.Fatalf("matching state bootstrapped again: %v", after)
	}
}

func TestStartupRejectsMissingSavedTaskFileWithoutBootstrap(t *testing.T) {
	c, bootstraps := savedStateEngine(t, "to-do.json")
	if err := os.Remove(c.Todo); err != nil {
		t.Fatal(err)
	}
	agentCalls(t)
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	err = e.initialize(context.Background())
	if err == nil || !strings.Contains(err.Error(), "saved task file") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected a clear missing-task-file error, got %v", err)
	}
	if _, statErr := os.Stat(c.Todo); !os.IsNotExist(statErr) {
		t.Fatal("missing task file was rebuilt by bootstrap")
	}
	after, _ := filepath.Glob(filepath.Join(e.Dir, "bootstrap-*"))
	if len(after) != len(bootstraps) {
		t.Fatalf("missing task file ran bootstrap: %v", after)
	}
}

func TestStartupRejectsForeignSavedState(t *testing.T) {
	c, _ := savedStateEngine(t, "to-do.json")
	for name, mutate := range map[string]func(*State){
		"version": func(s *State) { s.Version = 3 },
		"root":    func(s *State) { s.Root = filepath.Join(c.Root, "elsewhere") },
	} {
		t.Run(name, func(t *testing.T) {
			e, lock, err := NewEngine(c, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			s, err := e.Store.Load()
			if err != nil || s == nil {
				lock.Close()
				t.Fatalf("saved state unreadable: %v", err)
			}
			mutate(s)
			if err = e.Store.Save(s); err != nil {
				lock.Close()
				t.Fatal(err)
			}
			if err = e.Store.Close(); err != nil {
				t.Fatal(err)
			}
			if err = lock.Close(); err != nil {
				t.Fatal(err)
			}
			agentCalls(t)
			e, lock, err = NewEngine(c, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			defer e.Store.Close()
			err = e.initialize(context.Background())
			if err == nil || !strings.Contains(err.Error(), "another project or version") {
				t.Fatalf("expected a foreign-state error, got %v", err)
			}
		})
	}
}

func TestFreshProjectBootstrapsTheSelectedTaskFile(t *testing.T) {
	valid, err := json.Marshal(testBacklog())
	if err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("LOOPER_FAKE_LOG", calls)
	bin, _ := fakeAgent(t, string(valid))
	c := repoConfig(t, "run")
	c.CodexBin, c.ClaudeBin = bin, bin
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	if err = e.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(c.Todo); statErr != nil {
		t.Fatalf("fresh bootstrap wrote no task file: %v", statErr)
	}
	if e.State == nil || e.State.Todo != c.Todo || len(e.State.Backlog.Tasks) != 4 {
		t.Fatalf("fresh state incomplete: %+v", e.State)
	}
	if data, readErr := os.ReadFile(calls); readErr != nil || strings.Count(string(data), "\n") != 1 {
		t.Fatalf("fresh bootstrap agent calls: %q %v", data, readErr)
	}
}
