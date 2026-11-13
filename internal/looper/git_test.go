package looper

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// A root snapshot must never stat the runtime tree. The controller and its
// workers rename .looper-write-* temporaries into place there while a
// publication snapshots the root, and git exits 128 when a temporary vanishes
// between its directory read and its stat. A visible runtime tree is pruned
// from the scan by an exclude pathspec; scans from anywhere else never reach
// it, so they keep the plain command.
func TestSnapshotSurvivesConcurrentRuntimeWrites(t *testing.T) {
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
	if e.Repo.Runtime != "base" {
		t.Fatalf("the runtime tree must be recorded, got %q", e.Repo.Runtime)
	}
	if got := e.Repo.scanExcludes(c.Root); len(got) != 1 || got[0] != ":(exclude)base" {
		t.Fatalf("a root scan must prune the runtime tree, got %v", got)
	}
	if got := e.Repo.scanExcludes(filepath.Join(c.Root, "worktree")); got != nil {
		t.Fatalf("a worktree scan never reaches the runtime tree, got %v", got)
	}
	// The concurrent writers use the real atomicWrite with production-scale
	// payloads: a temporary's lifetime covers its write and fsync, which is
	// the window the snapshot must never observe. Several writers cover the
	// files a runtime tree churns during a run.
	stop := make(chan struct{})
	payload := bytes.Repeat([]byte("lease\n"), 32<<10)
	var group sync.WaitGroup
	for _, name := range []string{"lease.json", "view.json", "prompt.txt", "context.json"} {
		group.Add(1)
		go func(name string) {
			defer group.Done()
			target := filepath.Join(e.Dir, "churn", name)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if atomicWrite(target, payload, 0600) != nil {
					return
				}
			}
		}(name)
	}
	for i := 0; i < 25; i++ {
		if _, err = e.Repo.SnapshotFrom(c.Root, "HEAD"); err != nil {
			close(stop)
			group.Wait()
			t.Fatalf("snapshot %d failed under concurrent runtime writes: %v", i, err)
		}
	}
	close(stop)
	group.Wait()
	if _, err = os.Stat(filepath.Join(e.Dir, "churn", "lease.json")); err != nil {
		t.Fatalf("the concurrent writes never ran: %v", err)
	}
}

// A project may ignore the runtime tree itself. Naming an ignored path in an
// exclude pathspec makes git add fail on it, so the scan must fall back to
// the plain command: git already skips an ignored directory without a stat.
func TestSnapshotSkipsIgnoredRuntimeTree(t *testing.T) {
	c := repoConfig(t, "run")
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{
		{ID: "T001", Title: "existing", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Root, ".gitignore"), []byte("/base/\n"), 0644); err != nil {
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
	if got := e.Repo.scanExcludes(c.Root); got != nil {
		t.Fatalf("an ignored runtime tree must keep the plain scan, got %v", got)
	}
	tree, err := e.Repo.Snapshot(c.Root)
	if err != nil {
		t.Fatalf("snapshot must succeed with an ignored runtime tree: %v", err)
	}
	names, err := gitText(c.Root, "ls-tree", "-r", "--name-only", tree)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(names, "base") || strings.Contains(names, "to-do") {
		t.Fatalf("the snapshot tree must exclude the runtime tree and control files, got:\n%s", names)
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

// customTaskRepo prepares a repository that runs on a task file with a
// non-default name, so the derived schema name cannot be a constant.
func customTaskRepo(t *testing.T) (Config, Backlog) {
	t.Helper()
	c := repoConfig(t, "run", filepath.Join("plans", "board.json"))
	if err := os.MkdirAll(filepath.Dir(c.Todo), 0755); err != nil {
		t.Fatal(err)
	}
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{
		{ID: "T001", Title: "existing", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.SchemaPath(), []byte(backlogSchema), 0644); err != nil {
		t.Fatal(err)
	}
	return c, todo
}

// The control files of a custom task file are its own derived names: the
// snapshot must exclude both, and a project file must survive beside them.
func TestCustomTaskFileExcludesItsDerivedSchema(t *testing.T) {
	c, _ := customTaskRepo(t)
	if got := c.SchemaPath(); got != filepath.Join(c.Root, "plans", "board.schema.json") {
		t.Fatalf("schema name must follow the task file, got %s", got)
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
	if got := strings.Join(e.Repo.Excluded[:2], ","); got != "plans/board.json,plans/board.schema.json" {
		t.Fatalf("excluded control files %v", e.Repo.Excluded)
	}
	if err := os.WriteFile(filepath.Join(c.Root, "README.md"), []byte("project\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tree, err := e.Repo.Snapshot(c.Root)
	if err != nil {
		t.Fatal(err)
	}
	names, err := gitText(c.Root, "ls-tree", "-r", "--name-only", tree)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(names, "board.json") || strings.Contains(names, "board.schema.json") {
		t.Fatalf("a custom task file and its schema must stay out of snapshots:\n%s", names)
	}
	if !strings.Contains(names, "README.md") {
		t.Fatalf("project files must survive the exclusion:\n%s", names)
	}
}

// The symlink guard covers the derived schema name too, not just the task
// file, because a symlinked schema can redirect the control file elsewhere.
func TestCustomTaskFileGuardsSchemaSymlink(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(outside, "real-schema.json")
	if err := os.WriteFile(target, []byte(backlogSchema), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, victim string
	}{
		{"schema", "schema"},
		{"task file", "task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := customTaskRepo(t)
			victim := c.SchemaPath()
			if tc.victim == "task" {
				victim = c.Todo
			}
			if err := os.Remove(victim); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, victim); err != nil {
				t.Fatal(err)
			}
			_, err := OpenRepository(c, filepath.Join(t.TempDir(), "runtime"))
			if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
				t.Fatalf("a symlinked %s must be rejected, got %v", tc.victim, err)
			}
		})
	}
}

// A failed merge is a content conflict only when the worktree index holds
// unmerged paths. A missing object and a killed invocation keep the conflict
// label off while the original error survives both classification and
// feedback, so the operator is never sent to resolve conflicts that no
// repository evidence shows.
func TestMergeClassifiesConflictFromOtherFailures(t *testing.T) {
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
	// One committed side per divergent edit, exactly as an accepted
	// integration and a worker result diverge from one base.
	side := func(content string) string {
		work, err := e.Repo.Workspace(identifier("check"), e.State.Tip, e.State.Backlog)
		if err != nil {
			t.Fatal(err)
		}
		defer e.Repo.Remove(work)
		if err = os.WriteFile(filepath.Join(work, "shared.txt"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		tree, err := e.Repo.Snapshot(work)
		if err != nil {
			t.Fatal(err)
		}
		commit, err := e.Repo.Commit(tree, []string{e.State.Tip}, "Diverge "+content)
		if err != nil {
			t.Fatal(err)
		}
		return commit
	}
	worker := side("worker implementation\n")
	integrated := side("integrated implementation\n")
	work, err := e.Repo.Workspace(identifier("check"), integrated, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(work) })

	err = e.Repo.Merge(work, worker)
	var conflict *MergeError
	if !errors.As(err, &conflict) {
		t.Fatalf("a conflicting merge must return a MergeError, got %v", err)
	}
	if !conflict.Checked || len(conflict.Unmerged) != 1 || conflict.Unmerged[0] != "shared.txt" {
		t.Fatalf("conflict not verified from the worktree index: %+v", conflict)
	}
	feedback := mergeFailureFeedback("task", err)
	for _, want := range []string{"conflicts with the integrated project", "shared.txt", "CONFLICT"} {
		if !strings.Contains(feedback, want) {
			t.Fatalf("conflict feedback lacks %q: %s", want, feedback)
		}
	}

	// A Git failure that leaves no unmerged paths is not a conflict.
	missing := strings.Repeat("0123456789", 4)
	fresh, err := e.Repo.Workspace(identifier("check"), integrated, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(fresh) })
	err = e.Repo.Merge(fresh, missing)
	var failure *MergeError
	if !errors.As(err, &failure) {
		t.Fatalf("a failed merge must return a MergeError, got %v", err)
	}
	if !failure.Checked || len(failure.Unmerged) != 0 {
		t.Fatalf("a missing object must verify as conflict-free: %+v", failure)
	}
	if !strings.Contains(failure.Err.Error(), missing) {
		t.Fatalf("the original Git error was lost: %v", failure.Err)
	}
	feedback = mergeFailureFeedback("task", err)
	if !strings.Contains(feedback, "failed without a content conflict") || !strings.Contains(feedback, missing) {
		t.Fatalf("failure feedback must name the operation and the actual error: %s", feedback)
	}
	if strings.Contains(feedback, "conflicts with the integrated project") {
		t.Fatalf("a non-conflict failure reads as a conflict: %s", feedback)
	}

	// A killed invocation cannot be checked; the conflict status stays unknown
	// instead of asserted in either direction.
	previous := gitTimeout
	gitTimeout = time.Nanosecond
	defer func() { gitTimeout = previous }()
	err = e.Repo.Merge(fresh, worker)
	var killed *MergeError
	if !errors.As(err, &killed) {
		t.Fatalf("a killed merge must return a MergeError, got %v", err)
	}
	if killed.Checked || len(killed.Unmerged) != 0 {
		t.Fatalf("a killed merge must stay unverified: %+v", killed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the original timeout error was lost: %v", err)
	}
	feedback = mergeFailureFeedback("task", err)
	if !strings.Contains(feedback, "could not be checked for content conflicts") {
		t.Fatalf("unverified feedback must say so: %s", feedback)
	}
	if strings.Contains(feedback, "conflicts with the integrated project") {
		t.Fatalf("an unverified failure reads as a conflict: %s", feedback)
	}
}
