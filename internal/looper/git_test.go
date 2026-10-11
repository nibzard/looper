package looper

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Snapshot trees exclude control files, but prompts name the backlog and its
// schema as source files. A workspace that carries only the backlog sends
// every reader of the schema into a missing-file tool error.
func TestWorkspaceCarriesControlFiles(t *testing.T) {
	c := repoConfig(t, "run")
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{
		{ID: "T001", Title: "existing", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	work, err := e.Repo.Workspace(identifier("task"), e.State.Tip, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(work) })
	if _, err := os.Stat(filepath.Join(work, e.Repo.Excluded[0])); err != nil {
		t.Fatalf("the backlog must exist in the workspace: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(c.Root, e.Repo.Excluded[1]))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(work, e.Repo.Excluded[1]))
	if err != nil {
		t.Fatalf("the task schema must exist in the workspace: %v", err)
	}
	if string(got) != string(want) {
		t.Fatal("the workspace schema must match the project copy")
	}
}
