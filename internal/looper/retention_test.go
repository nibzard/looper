package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/looper/internal/contractschema"
)

// The AX-015 acceptance fixtures: formatting-only, actual contract, unrelated
// project edit, collision, and unborn HEAD. Every fixture must leave the
// guard refusing and the retained result intact, while the version 2 error
// document carries evidence distinct to that fixture.

// retentionDoc renders one guard failure through the version 2 emitter and
// validates the document against the published error schema.
func retentionDoc(t *testing.T, c Config, command string, err error) *errorDocument {
	t.Helper()
	var out, stderr strings.Builder
	if code := emitError(v2Config(c, command, true), err, &out, &stderr); code != exitOperationFailed {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var doc errorDocument
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = contractschema.ValidateJSON(errorSchema(t), data); err != nil {
		t.Fatalf("the emitted error violates its schema: %v\n%s", err, data)
	}
	if doc.Command != "looper "+command+" --contract v2" {
		t.Fatalf("command label %q", doc.Command)
	}
	return &doc
}

// stringList reads one observed value that must be a string list.
func stringList(t *testing.T, doc *errorDocument, key string) []string {
	t.Helper()
	value, ok := doc.Error.Observed[key].([]any)
	if !ok {
		t.Fatalf("observed %q is not a list: %v", key, doc.Error.Observed[key])
	}
	out := make([]string, 0, len(value))
	for _, item := range value {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("observed %q holds a non-string: %v", key, value)
		}
		out = append(out, s)
	}
	return out
}

// startupBacklogFixture saves a run with one retained attempt and an accepted
// task file, mirroring an interrupted session, and returns the config and the
// saved digest. The saved backlog is the parsed task file, exactly as a real
// run would have accepted it.
func startupBacklogFixture(t *testing.T) (Config, string) {
	t.Helper()
	c := errorProject(t)
	tasks := []Task{{ID: "T001", Title: "one", Priority: 1, Status: "doing"}}
	if err := writeJSON(c.Todo, Backlog{Version: 1, Sources: []string{}, Tasks: tasks}); err != nil {
		t.Fatal(err)
	}
	// One commit gives the saved run a real baseline. The task file itself
	// stays untracked, exactly as the engine's snapshots keep it, so the
	// working-tree comparison after an accepted edit still matches.
	if err := os.WriteFile(filepath.Join(c.Root, "seed.md"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "add", "--", "seed.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "commit", "-q", "-m", "seed", "--", "seed.md"); err != nil {
		t.Fatal(err)
	}
	head, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	baselineTree, err := gitText(c.Root, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	// The saved run records the branch this repository actually uses. A
	// host's init.defaultBranch decides it at init time, so the fixture
	// reads it back instead of assuming Git's built-in default.
	branch, err := gitText(c.Root, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	var backlog Backlog
	if err = json.Unmarshal(accepted, &backlog); err != nil {
		t.Fatal(err)
	}
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	saved := digestFile(c.Todo)
	if err = store.Save(&State{
		Version: 2, Root: c.Root, Todo: c.Todo, Head: head, Branch: "refs/heads/" + branch,
		Ref: "refs/looper/run-x", Tip: head, Published: baselineTree, ExportHash: saved,
		Backlog:  backlog,
		Attempts: []*Attempt{{ID: "a1", Task: backlog.Tasks[0], Phase: "running", Worktrees: []string{}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	return c, saved
}

// startupGuard drives one startup refusal through a fresh engine.
func startupGuard(t *testing.T, c Config) error {
	t.Helper()
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	err = e.initializeState(context.Background())
	if err == nil {
		t.Fatal("the changed backlog must stop startup")
	}
	var retained *retentionError
	if !errors.As(err, &retained) {
		t.Fatalf("the guard must be a retention error: %v", err)
	}
	// The retained result stays intact: the edit is never imported and the
	// saved digest keeps pointing at the accepted bytes.
	saved, loadErr := e.Store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	currentHead, _ := e.Repo.Head()
	if saved.ExportHash == digestFile(c.Todo) || saved.Head != currentHead {
		t.Fatal("the guard changed the retained state")
	}
	return err
}

// TestFormattingOnlyEditIsAdoptedAtStartup rewrites the task file with the
// same content in different bytes while work is retained. Startup adopts the
// equivalent bytes instead of refusing (AX-018): the run continues, the
// operator's formatting stays on the file, the accepted digest moves to the
// observed bytes, and one event records both digests as recovery evidence.
func TestFormattingOnlyEditIsAdoptedAtStartup(t *testing.T) {
	c, saved := startupBacklogFixture(t)
	tasks := []Task{{ID: "T001", Title: "one", Priority: 1, Status: "doing"}}
	compact, err := json.Marshal(Backlog{Version: 1, Sources: []string{}, Tasks: tasks})
	if err != nil {
		t.Fatal(err)
	}
	edited := append(compact, '\n')
	if err = os.WriteFile(c.Todo, edited, 0600); err != nil {
		t.Fatal(err)
	}
	observed := digest(edited)
	if observed == saved {
		t.Fatal("the fixture must change the file bytes")
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	// The retained attempt phase reaches the resume flow; the equivalent
	// edit never stopped it.
	if err = e.initializeState(context.Background()); err != nil {
		t.Fatalf("an equivalent edit must not stop startup: %v", err)
	}
	if e.State.ExportHash != observed {
		t.Fatalf("the accepted digest must move to the observed bytes: %s", e.State.ExportHash)
	}
	// The operator's bytes stay on the file: adoption never rewrites them.
	if data, err := os.ReadFile(c.Todo); err != nil || !bytes.Equal(data, edited) {
		t.Fatalf("the edit was rewritten: %q %v", data, err)
	}
	// One journal note records the accepted and observed digests, so
	// recovery keeps the evidence of what changed.
	s, err := OpenStoreReadonly(filepath.Join(e.Dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events, err := s.RecentEvents(-1, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Kind != "backlog-format" {
			continue
		}
		found = strings.Contains(ev.Message, "formatting") &&
			strings.Contains(ev.Message, "equivalent content") &&
			strings.Contains(string(ev.Data), saved) && strings.Contains(string(ev.Data), observed)
	}
	if !found {
		t.Fatal("the adoption must record one backlog-format event with both digests")
	}
	// The retained work stays exactly as it was.
	loaded, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Attempts) != 1 || loaded.Attempts[0].Phase != "resume" {
		t.Fatalf("the retained work changed: %+v", *loaded.Attempts[0])
	}
}

// TestActualContractChangeNamesTheChangedTask edits a task title in the file.
// The evidence names the changed task identity, distinguishing the fixture
// from a formatting-only edit.
func TestActualContractChangeNamesTheChangedTask(t *testing.T) {
	c, saved := startupBacklogFixture(t)
	tasks := []Task{{ID: "T001", Title: "renamed outside Looper", Priority: 1, Status: "doing"}}
	if err := writeJSON(c.Todo, Backlog{Version: 1, Sources: []string{}, Tasks: tasks}); err != nil {
		t.Fatal(err)
	}
	guard := startupGuard(t, c)
	doc := retentionDoc(t, c, "run", guard)
	if doc.Error.Code != "backlog_contract_changed" {
		t.Fatalf("code %q", doc.Error.Code)
	}
	if doc.Error.Observed["task_file_digest"] == saved {
		t.Fatalf("digest evidence %v", doc.Error.Observed)
	}
	changes := stringList(t, doc, "task_contract_changes")
	if len(changes) != 1 || changes[0] != "T001" {
		t.Fatalf("the changed task identity must be named: %v", changes)
	}
	if parsed, _ := doc.Error.Observed["parsed_backlog"].(string); !strings.HasPrefix(parsed, "differs") {
		t.Fatalf("parsed backlog evidence %v", doc.Error.Observed["parsed_backlog"])
	}
	inspectRoute := false
	for _, ev := range doc.Error.Evidence {
		if ev.Command == "looper inspect T001 --json" {
			inspectRoute = true
		}
	}
	if !inspectRoute {
		t.Fatalf("no route to the changed task contract: %+v", doc.Error.Evidence)
	}
}

// TestUnrelatedProjectEditReportsChangedPaths edits one project file while
// work is retained. The startup tree guard names the changed path, its
// complete count, and the read-only command that lists every difference.
func TestUnrelatedProjectEditReportsChangedPaths(t *testing.T) {
	e, _ := publicationFixture(t)
	e.State.Publication = nil
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	published := e.State.Published
	if err := os.WriteFile(filepath.Join(e.Config.Root, "notes.md"), []byte("unrelated edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err := e.initializeState(context.Background())
	if err == nil {
		t.Fatal("the unrelated edit must stop startup")
	}
	var retained *retentionError
	if !errors.As(err, &retained) {
		t.Fatalf("the guard must be a retention error: %v", err)
	}
	doc := retentionDoc(t, e.Config, "run", err)
	if doc.Error.Code != "project_tree_changed" {
		t.Fatalf("code %q", doc.Error.Code)
	}
	if doc.Error.Expected["project_tree"] != published {
		t.Fatalf("expected tree %v", doc.Error.Expected)
	}
	if paths := stringList(t, doc, "project_tree_changes"); len(paths) != 1 || paths[0] != "notes.md" {
		t.Fatalf("changed-path evidence %v", paths)
	}
	if doc.Error.Observed["project_tree_changes_count"] != float64(1) {
		t.Fatalf("complete count %v", doc.Error.Observed["project_tree_changes_count"])
	}
	observed := doc.Error.Observed["project_tree"]
	route := fmt.Sprintf("git diff --name-only %v %v", published, observed)
	found := false
	for _, ev := range doc.Error.Evidence {
		if ev.Command == route {
			found = true
		}
	}
	if !found {
		t.Fatalf("no complete-detail route %q: %+v", route, doc.Error.Evidence)
	}
	if ids := stringList(t, doc, "retained_tasks"); len(ids) != 1 || ids[0] != "T001" {
		t.Fatalf("retained task identities %v", ids)
	}
	// The edit survives; the guard never overwrote it.
	if data, err := os.ReadFile(filepath.Join(e.Config.Root, "notes.md")); err != nil || string(data) != "unrelated edit\n" {
		t.Fatalf("the edit was changed: %q %v", data, err)
	}
}

// TestCollisionReportsBothTreesAndChangedPaths stages a local edit that
// matches neither the accepted tree nor the verified candidate. The collision
// guard carries both expected trees, the bounded changed paths for each, the
// verified task identity, and its detail routes.
func TestCollisionReportsBothTreesAndChangedPaths(t *testing.T) {
	e, p := publicationFixture(t)
	if err := os.WriteFile(filepath.Join(e.Config.Root, "local.txt"), []byte("local work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(e.Config.Root, "add", "--", "local.txt"); err != nil {
		t.Fatal(err)
	}
	err := e.publish()
	if err == nil {
		t.Fatal("the collision must prevent publication")
	}
	var retained *retentionError
	if !errors.As(err, &retained) {
		t.Fatalf("the guard must be a retention error: %v", err)
	}
	doc := retentionDoc(t, e.Config, "run", err)
	if doc.Error.Code != "project_tree_changed" {
		t.Fatalf("code %q", doc.Error.Code)
	}
	if doc.Error.Expected["accepted_tree"] != p.Before || doc.Error.Expected["verified_tree"] != p.Tree {
		t.Fatalf("both trees must be expected: %v", doc.Error.Expected)
	}
	// Against the accepted tree only the local edit differs. Against the
	// verified candidate its own ignored evidence file is also missing,
	// because the fixture never applied the candidate.
	if paths := stringList(t, doc, "accepted_tree_changes"); len(paths) != 1 || paths[0] != "local.txt" {
		t.Fatalf("accepted_tree_changes evidence %v", paths)
	}
	if paths := stringList(t, doc, "verified_tree_changes"); len(paths) != 2 || paths[0] != "local.txt" || paths[1] != "raw/sample.txt" {
		t.Fatalf("verified_tree_changes evidence %v", paths)
	}
	if doc.Error.Observed["verified_tree_changes_count"] != float64(2) {
		t.Fatalf("complete count %v", doc.Error.Observed["verified_tree_changes_count"])
	}
	if ids := stringList(t, doc, "retained_tasks"); len(ids) != 1 || ids[0] != "T001" {
		t.Fatalf("the verified task identity must be retained: %v", ids)
	}
	if len(doc.Error.UnknownEvidence) == 0 {
		t.Fatal("the guard must name which evidence is unknown")
	}
	// The retained publication survives the refusal.
	saved, loadErr := e.Store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if saved.Publication == nil || saved.Published != p.Before {
		t.Fatal("the collision changed the retained result")
	}
}

// TestUnbornHeadIsReportedAsUnborn deletes the saved branch so HEAD points at
// a branch with no commits. The branch guard states the unborn HEAD instead
// of asserting that the branch moved, and keeps the changed-path evidence
// unknown with its reason.
func TestUnbornHeadIsReportedAsUnborn(t *testing.T) {
	e, _ := publicationFixture(t)
	seedRecoveryHead(t, e)
	_, branch := e.Repo.Head()
	if _, err := gitText(e.Config.Root, "update-ref", "-d", branch); err != nil {
		t.Fatal(err)
	}
	err := e.initializeState(context.Background())
	if err == nil {
		t.Fatal("the unborn HEAD must stop startup")
	}
	var retained *retentionError
	if !errors.As(err, &retained) {
		t.Fatalf("the guard must be a retention error: %v", err)
	}
	if !strings.Contains(err.Error(), "unborn") || strings.Contains(err.Error(), "branch moved") {
		t.Fatalf("the message must state the unborn HEAD, not branch movement: %v", err)
	}
	doc := retentionDoc(t, e.Config, "run", err)
	if doc.Error.Code != "branch_moved" {
		t.Fatalf("code %q", doc.Error.Code)
	}
	if doc.Error.Observed["head"] != headUnborn || doc.Error.Observed["unborn_head"] != true {
		t.Fatalf("observed head evidence %v", doc.Error.Observed)
	}
	if doc.Error.Expected["head"] != e.State.Head {
		t.Fatalf("expected head evidence %v", doc.Error.Expected)
	}
	if doc.Error.Observed["committed_changes"] != "unknown" {
		t.Fatalf("changed-path evidence must stay unknown: %v", doc.Error.Observed["committed_changes"])
	}
	if len(doc.Error.UnknownEvidence) == 0 {
		t.Fatal("the unknown evidence must be named")
	}
	// The retained baseline survives the refusal.
	saved, loadErr := e.Store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if saved.Head != e.State.Head {
		t.Fatal("the guard changed the retained baseline")
	}
}

// TestGuardFixturesGiveDistinctEvidence compares the five acceptance
// fixtures on the keys a caller reads to tell them apart: no two fixtures
// share the same distinguishing evidence.
func TestGuardFixturesGiveDistinctEvidence(t *testing.T) {
	type fixture struct {
		name string
		run  func(t *testing.T) *errorDocument
	}
	fixtures := []fixture{
		{"task data only", func(t *testing.T) *errorDocument {
			// A status edit changes task data without changing the contract
			// hash: the guard refuses and the evidence names the task
			// through task_data_changes (AX-018). A formatting-only edit no
			// longer refuses, so it cannot serve as a refusal fixture.
			c, _ := startupBacklogFixture(t)
			tasks := []Task{{ID: "T001", Title: "one", Priority: 1, Status: "done"}}
			if err := writeJSON(c.Todo, Backlog{Version: 1, Sources: []string{}, Tasks: tasks}); err != nil {
				t.Fatal(err)
			}
			return retentionDoc(t, c, "run", startupGuard(t, c))
		}},
		{"actual contract", func(t *testing.T) *errorDocument {
			c, _ := startupBacklogFixture(t)
			tasks := []Task{{ID: "T001", Title: "renamed outside Looper", Priority: 1, Status: "doing"}}
			if err := writeJSON(c.Todo, Backlog{Version: 1, Sources: []string{}, Tasks: tasks}); err != nil {
				t.Fatal(err)
			}
			return retentionDoc(t, c, "run", startupGuard(t, c))
		}},
		{"unrelated project edit", func(t *testing.T) *errorDocument {
			e, _ := publicationFixture(t)
			e.State.Publication = nil
			if err := e.save(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(e.Config.Root, "notes.md"), []byte("unrelated edit\n"), 0600); err != nil {
				t.Fatal(err)
			}
			err := e.initializeState(context.Background())
			if err == nil {
				t.Fatal("the unrelated edit must stop startup")
			}
			return retentionDoc(t, e.Config, "run", err)
		}},
		{"collision", func(t *testing.T) *errorDocument {
			e, _ := publicationFixture(t)
			if err := os.WriteFile(filepath.Join(e.Config.Root, "local.txt"), []byte("local work\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := gitText(e.Config.Root, "add", "--", "local.txt"); err != nil {
				t.Fatal(err)
			}
			err := e.publish()
			if err == nil {
				t.Fatal("the collision must prevent publication")
			}
			return retentionDoc(t, e.Config, "run", err)
		}},
		{"unborn HEAD", func(t *testing.T) *errorDocument {
			e, _ := publicationFixture(t)
			seedRecoveryHead(t, e)
			_, branch := e.Repo.Head()
			if _, err := gitText(e.Config.Root, "update-ref", "-d", branch); err != nil {
				t.Fatal(err)
			}
			err := e.initializeState(context.Background())
			if err == nil {
				t.Fatal("the unborn HEAD must stop startup")
			}
			return retentionDoc(t, e.Config, "run", err)
		}},
	}
	// The signature keys one fixture from the document: the guard code plus
	// the values that separate the fixtures inside one code.
	signature := func(t *testing.T, doc *errorDocument) string {
		keys := []string{doc.Error.Code}
		for _, key := range []string{
			"parsed_backlog", "task_contract_changes", "project_tree_changes",
			"accepted_tree_changes", "head", "committed_changes",
		} {
			if value, ok := doc.Error.Observed[key]; ok {
				keys = append(keys, fmt.Sprintf("%s=%v", key, value))
			}
		}
		return strings.Join(keys, " ")
	}
	seen := map[string]string{}
	for _, f := range fixtures {
		doc := f.run(t) // the per-fixture behavior is asserted in the tests above
		sig := signature(t, doc)
		if other, ok := seen[sig]; ok {
			t.Fatalf("%s and %s share one evidence signature: %s", f.name, other, sig)
		}
		seen[sig] = f.name
	}
}

// TestLegacyGuardRenderingIsUnchanged pins the default contract for the
// enriched guards: the same message text on stderr with exit code 1.
func TestLegacyGuardRenderingIsUnchanged(t *testing.T) {
	c, _ := startupBacklogFixture(t)
	tasks := []Task{{ID: "T001", Title: "renamed outside Looper", Priority: 1, Status: "doing"}}
	if err := writeJSON(c.Todo, Backlog{Version: 1, Sources: []string{}, Tasks: tasks}); err != nil {
		t.Fatal(err)
	}
	guard := startupGuard(t, c)
	var out, stderr strings.Builder
	code := emitError(c, guard, &out, &stderr)
	if code != exitOperationFailed {
		t.Fatalf("exit %d", code)
	}
	if out.Len() > 0 {
		t.Fatalf("a legacy failure writes nothing to stdout: %q", out.String())
	}
	if !strings.HasPrefix(stderr.String(), "Error: the backlog changed while work was retained; restore its saved contract before resuming\n") {
		t.Fatalf("legacy text changed: %q", stderr.String())
	}
}
