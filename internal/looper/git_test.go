package looper

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Snapshot trees exclude control files, but prompts name the backlog and its
// schema as source files. A workspace that carries only the backlog sends
// every reader of the schema into a missing-file tool error.
// A project may ignore its task file and schema. git add rejects an exclude
// pathspec that names an ignored file, so staging must remove control files
// from the temporary index instead of excluding them by pathspec.
func TestSnapshotSucceedsWithIgnoredTaskFile(t *testing.T) {
	c := repoConfig(t, "run")
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{
		{ID: "T001", Title: "existing", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Root, ".gitignore"), []byte("to-do.json\nto-do.schema.json\n"), 0644); err != nil {
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
	tree, err := e.Repo.Snapshot(c.Root)
	if err != nil {
		t.Fatalf("snapshot must succeed when the task file is ignored: %v", err)
	}
	names, err := gitText(c.Root, "ls-tree", "-r", "--name-only", tree)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(names, "to-do") {
		t.Fatalf("the snapshot tree must exclude control files, got:\n%s", names)
	}
	if !strings.Contains(names, ".gitignore") {
		t.Fatalf("the snapshot tree must carry project files, got:\n%s", names)
	}
}

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
