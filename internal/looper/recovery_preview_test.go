package looper

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/nibzard/looper/internal/contractschema"
)

// The AX-016 acceptance fixtures. Every fixture drives the public CLI: the
// preview must answer through `looper recovery --preview` while changing no
// file, database record, process, or Git ref, and every unavailable or
// unsafe case must explain the missing evidence it retained work with.

// recoveryPreviewSchema reads the published preview schema, anchored at this
// file's directory because fixtures chdir into temporary projects.
func recoveryPreviewSchema(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test file location is unavailable")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "contracts", "schemas", "v2", "recovery-preview.schema.json"))
	if err != nil {
		t.Skipf("recovery preview schema is absent: %v", err)
	}
	return b
}

// runPreviewCLI runs the recovery preview through Main and returns the exit
// code with the decoded document. The JSON form must validate against the
// published schema, carry no human text, and stay within its byte budget.
func runPreviewCLI(t *testing.T, args ...string) (int, *recoveryPreviewDocument, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := Main(append([]string{"recovery", "--preview"}, args...), &out, &stderr)
	jsonForm := false
	for _, a := range args {
		if a == "--json" {
			jsonForm = true
		}
	}
	if !jsonForm {
		if code != 0 {
			t.Fatalf("plain preview exit %d: %s%s", code, out.String(), stderr.String())
		}
		return code, nil, out.String()
	}
	if stderr.Len() > 0 {
		t.Fatalf("the preview JSON mixes human text into stdout: stderr=%q", stderr.String())
	}
	if code != 0 {
		t.Fatalf("json preview exit %d: %s", code, out.String())
	}
	var doc recoveryPreviewDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not one preview document: %v\n%s", err, out.String())
	}
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	var first any
	if err := dec.Decode(&first); err != nil || dec.More() {
		t.Fatalf("stdout carries more than one JSON value: %v", err)
	}
	if err := contractschema.ValidateJSON(recoveryPreviewSchema(t), out.Bytes()); err != nil {
		t.Fatalf("the emitted preview violates its schema: %v\n%s", err, out.String())
	}
	if out.Len() > recoveryPreviewBytesBudget {
		t.Fatalf("preview document is %d bytes, budget %d", out.Len(), recoveryPreviewBytesBudget)
	}
	return code, &doc, out.String()
}

// retainedRunFixture saves one interrupted run with real repository values:
// initialize accepts the written backlog, then one retained attempt joins
// the state. The engine closes before the preview runs, exactly like a run
// that ended with retained work and released the repository.
func retainedRunFixture(t *testing.T) Config {
	t.Helper()
	c := repoConfig(t, "run")
	// One commit before the run starts, so the saved run carries a real
	// baseline: the unborn-HEAD guard compares against it, and a run that
	// began on an unborn HEAD legitimately matches one today.
	if err := os.WriteFile(filepath.Join(c.Root, "seed.md"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "add", "--", "seed.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "commit", "-q", "-m", "seed"); err != nil {
		t.Fatal(err)
	}
	b := Backlog{Version: 1, Sources: []string{}, Goal: "preview fixture", Tasks: []Task{
		{ID: "T001", Title: "retained work", Priority: 1, Status: "doing"},
	}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	task := *e.State.Backlog.Task("T001")
	e.State.Attempts = append(e.State.Attempts, &Attempt{
		ID: "task-retained", Task: task, Agent: "claude", Base: e.State.Tip,
		Worktree: filepath.Join(e.Dir, "worktrees", "task-retained"), Directory: filepath.Join(e.Dir, "task-retained"),
		Phase: "running", Worktrees: []string{},
	})
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	if err = e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	return c
}

// retainedPublicationFixture saves a run holding one verified publication,
// mirroring the publication fixture with the engine released so the preview
// observes a stopped controller. The working tree still matches the accepted
// tree, so publishing applies at resume.
func retainedPublicationFixture(t *testing.T) (Config, *Candidate) {
	t.Helper()
	c := retainedRunFixture(t)
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	work, err := e.Repo.Workspace(identifier("task"), s.Tip, s.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Repo.Remove(work)
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
	commit, err := e.Repo.Commit(tree, []string{s.Tip}, "Add evidence")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range s.Attempts {
		if a.ID == "task-retained" {
			a.Phase = "verifying"
			a.ResultCommit = commit
			// A verifying attempt finished its worker, so its durable
			// receipt exists beside the retained work.
			if err = writeJSON(filepath.Join(a.Directory, "receipt.json"), resultReceipt{
				Base: a.Base, Contract: a.Task.Contract(), Repairs: 0,
				Summary: Summary{TaskID: a.Task.ID, Status: "done", Summary: "verified evidence", Files: []string{}, Blockers: []string{}},
				Commit:  commit,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	p := &Candidate{Base: s.Tip, Commit: commit, Tree: tree, Before: s.Published, AttemptID: "task-retained", OK: true}
	s.Publication = p
	if err = e.Store.Save(s); err != nil {
		t.Fatal(err)
	}
	if err = e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	return c, p
}

// mutationSnapshot captures everything the preview must not change: the
// state row, the newest event, the task-file bytes, the git index, every
// ref, the working-tree status, the runtime directory listing, and the lock
// files under the git directory.
type mutationSnapshot struct {
	state     string
	events    int64
	index     string
	refs      string
	status    string
	todo      string
	dirFiles  string
	lockFiles string
}

func snapshotProject(t *testing.T, c Config, dir string) mutationSnapshot {
	t.Helper()
	get := func(v string, err error) string {
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "state.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var state string
	if err = db.QueryRow("SELECT data FROM state WHERE id=1").Scan(&state); err != nil {
		t.Fatal(err)
	}
	var events int64
	if err = db.QueryRow("SELECT coalesce(max(id),0) FROM events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	common, err := gitRead(c.Root, "rev-parse", "--git-common-dir")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(c.Root, common)
	}
	var lockNames []string
	if entries, err := os.ReadDir(common); err == nil {
		for _, entry := range entries {
			if info, err := entry.Info(); err == nil {
				lockNames = append(lockNames, fmt.Sprintf("%s:%d:%d", entry.Name(), info.Size(), info.ModTime().UnixNano()))
			}
		}
	}
	var dirFiles []string
	if entries, err := os.ReadDir(dir); err == nil {
		for _, entry := range entries {
			dirFiles = append(dirFiles, entry.Name())
		}
	}
	indexDigest, indexErr := digestFileErr(filepath.Join(common, "index"))
	return mutationSnapshot{
		state: state, events: events,
		index:     get(indexDigest, indexErr),
		refs:      get(gitRead(c.Root, "for-each-ref")),
		status:    get(gitRead(c.Root, "status", "--porcelain")),
		todo:      digestFile(c.Todo),
		dirFiles:  strings.Join(dirFiles, ","),
		lockFiles: strings.Join(lockNames, ","),
	}
}

// digestFileErr reads and digests one file, reporting a missing file as an
// empty digest rather than an error.
func digestFileErr(path string) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return digest(data), nil
}

// TestRecoveryPreviewChangesNothing drives both preview renderings against a
// run with a retained publication, a live lock holder, and local
// working-tree changes, then proves nothing moved: no state row, no event,
// no index refresh, no ref, no lock file, no runtime file, and no
// working-tree byte.
func TestRecoveryPreviewChangesNothing(t *testing.T) {
	e, _ := publicationFixture(t)
	// A staged local edit makes the working-tree comparison real work.
	if err := os.WriteFile(filepath.Join(e.Config.Root, "local.txt"), []byte("local edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(e.Config.Root, "add", "--", "local.txt"); err != nil {
		t.Fatal(err)
	}
	before := snapshotProject(t, e.Config, e.Dir)
	// The plain form and the version 2 document both run while the fixture
	// engine still holds the repository lock.
	code, _, plain := runPreviewCLI(t, "--plain")
	if code != 0 || !strings.Contains(plain, "Recovery preview (read-only; nothing was changed)") {
		t.Fatalf("plain preview exit %d:\n%s", code, plain)
	}
	code, doc, raw := runPreviewCLI(t, "--contract", "v2", "--json")
	if code != 0 {
		t.Fatalf("json preview exit %d: %s", code, raw)
	}
	if doc.Comparisons.Publication == nil {
		t.Fatalf("publication comparison missing: %s", raw)
	}
	after := snapshotProject(t, e.Config, e.Dir)
	switch {
	case before.state != after.state:
		t.Fatal("the preview changed the saved state row")
	case before.events != after.events:
		t.Fatal("the preview wrote an event")
	case before.index != after.index:
		t.Fatal("the preview refreshed the git index")
	case before.refs != after.refs:
		t.Fatal("the preview changed a git ref")
	case before.status != after.status:
		t.Fatal("the preview changed the working tree")
	case before.todo != after.todo:
		t.Fatal("the preview changed the task file")
	case before.dirFiles != after.dirFiles:
		t.Fatalf("the preview changed the runtime directory: %q -> %q", before.dirFiles, after.dirFiles)
	case before.lockFiles != after.lockFiles:
		t.Fatalf("the preview changed a file under the git directory: %q -> %q", before.lockFiles, after.lockFiles)
	}
	// The preview answered while another process description held the lock,
	// so the document records ownership instead of taking it.
	if !doc.Controller.Owned {
		t.Fatalf("controller ownership: %+v", doc.Controller)
	}
}

// TestRecoveryPreviewEnumeratesChoicesAndPublication uses the retained
// publication fixture with the engine released: the working tree still
// matches the accepted tree, so publishing applies at resume, every guard
// passes, and the recovery choices state what each preserves and changes.
func TestRecoveryPreviewEnumeratesChoicesAndPublication(t *testing.T) {
	c, p := retainedPublicationFixture(t)
	_ = c
	code, doc, raw := runPreviewCLI(t, "--contract", "v2", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, raw)
	}
	if doc.Kind != recoveryPreviewKind || doc.ContractVersion != ContractVersionV2 {
		t.Fatalf("document identity: %+v", doc)
	}
	pub := doc.Comparisons.Publication
	if pub == nil || pub.Outcome != publicationApplies {
		t.Fatalf("publication outcome: %+v", pub)
	}
	if pub.Changes.Count != 1 || pub.Changes.Paths[0] != "raw/sample.txt" {
		t.Fatalf("publication changes: %+v", pub.Changes)
	}
	if pub.VerifiedCommit != p.Commit || pub.TaskID != "T001" {
		t.Fatalf("publication identity: %+v", pub)
	}
	// Every guard decided and passed: nothing failed and nothing stayed
	// unknown, so the resume choice is available.
	verdicts := map[string]string{}
	for _, g := range doc.Guards {
		verdicts[g.Code] = g.Verdict
	}
	for _, code := range []string{guardBacklogChanged, guardBranchMoved, guardProjectTreeChanged, guardTaskContract} {
		if verdicts[code] != verdictPass {
			t.Fatalf("guard %s verdict %q (guards: %+v)", code, verdicts[code], doc.Guards)
		}
	}
	if len(doc.Guards) < 4 {
		t.Fatalf("the guard set is incomplete: %+v", doc.Guards)
	}
	resume := doc.choice("resume")
	if resume.Status != "available" || resume.Apply.Mode != "command" || resume.Apply.Command != "looper" {
		t.Fatalf("resume choice: %+v", resume)
	}
	if len(resume.Preserves) == 0 || len(resume.Changes) == 0 {
		t.Fatalf("resume must state what it preserves and changes: %+v", resume)
	}
	publish := doc.choice("publish-retained")
	if publish.Status != "available" || len(publish.Preserves) == 0 || len(publish.Changes) == 0 {
		t.Fatalf("publish choice: %+v", publish)
	}
	// The discard choice is enumerated to be refused.
	if discard := doc.choice("discard-retained"); discard.Status != "unsupported" || discard.Apply.Mode != "none" {
		t.Fatalf("discard choice: %+v", discard)
	}
	// The token is a digest and the document names its detail routes.
	if len(doc.Token.Value) != 64 {
		t.Fatalf("token %q", doc.Token.Value)
	}
	if doc.Detail.Status == "" || doc.Detail.Events == "" {
		t.Fatalf("detail routes: %+v", doc.Detail)
	}
	if !doc.Run.PublicationHeld || !doc.Run.Retained || doc.Run.RetainedTaskTotal != 1 {
		t.Fatalf("run state: %+v", doc.Run)
	}
	if len(doc.Run.Attempts) != 1 || !doc.Run.Attempts[0].ContractHeld || !doc.Run.Attempts[0].ReceiptPresent {
		attempt := doc.Run.Attempts
		if len(attempt) == 1 {
			t.Fatalf("attempt evidence: %+v", attempt[0])
		}
		t.Fatalf("attempts: %+v", attempt)
	}
}

// TestRecoveryPreviewAcceptsOwnerCommitFold mirrors the T046 startup
// acceptance read-only: a retained run whose plan scopes its attempt away
// from an owner commit reports the branch recoverable through the fold, the
// working-tree differences as accepted owner content, and the held
// publication as requeued against the folded baseline.
func TestRecoveryPreviewAcceptsOwnerCommitFold(t *testing.T) {
	c, p := retainedPublicationFixture(t)
	// Scope the retained attempt away from the owner's path, exactly as a
	// plan does for work with a known write scope.
	store := openStateForEdit(t, c)
	s, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	s.Plan = &Plan{Verify: "true", Tasks: []Scope{{TaskID: "T001", Paths: []string{"internal/"}}}}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Root, "NOTES.md"), []byte("owner note\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "add", "--", "NOTES.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "commit", "-qm", "Owner saves a note"); err != nil {
		t.Fatal(err)
	}
	code, doc, raw := runPreviewCLI(t, "--contract", "v2", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, raw)
	}
	if branch := doc.Comparisons.Branch; branch.State != branchRecoverable || !strings.Contains(branch.Reason, "fold") {
		t.Fatalf("the branch comparison must name the fold acceptance: %+v", branch)
	}
	if wt := doc.Comparisons.WorkingTree; wt.State != "matches_published" || !wt.OwnerCommitAccepted {
		t.Fatalf("the working-tree comparison must count the owner content as accepted: %+v", wt)
	}
	if pub := doc.Comparisons.Publication; pub == nil || pub.Outcome != publicationRequeued || pub.VerifiedCommit != p.Commit {
		t.Fatalf("the held publication must requeue against the folded baseline: %+v", pub)
	}
	verdicts := map[string]string{}
	for _, g := range doc.Guards {
		verdicts[g.Code] = g.Verdict
	}
	for _, code := range []string{guardBranchMoved, guardProjectTreeChanged, guardTaskContract} {
		if verdicts[code] != verdictPass {
			t.Fatalf("guard %s verdict %q (guards: %+v)", code, verdicts[code], doc.Guards)
		}
	}
	resume := doc.choice("resume")
	if resume.Status != "available" {
		t.Fatalf("resume choice: %+v", resume)
	}
	if len(resume.Changes) == 0 || !strings.Contains(resume.Changes[0], "folds into the run baseline") {
		t.Fatalf("resume must state what the acceptance changes: %+v", resume.Changes)
	}
	publish := doc.choice("publish-retained")
	if publish.Status != "available" || !strings.Contains(publish.Changes[0], "verifies again") {
		t.Fatalf("publish choice: %+v", publish)
	}
	// The plain rendering reports the same acceptance.
	_, _, plain := runPreviewCLI(t, "--contract", "v2")
	if !strings.Contains(plain, "folds into it at resume") || !strings.Contains(plain, "recoverable") {
		t.Fatalf("plain rendering hides the acceptance: %s", plain)
	}
}

// openStateForEdit reopens the saved run's store for a fixture that adjusts
// the saved state before a preview observes it, mirroring how a stopped
// controller leaves state behind.
func openStateForEdit(t *testing.T, c Config) *Store {
	t.Helper()
	_, dir := projectPaths(c)
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestRecoveryPreviewExplainsTaskFileEdits covers the two backlog fixtures:
// a formatting-only edit reports an identical parsed backlog, passes the
// guard, and leaves resume available because startup adopts the equivalent
// bytes (AX-018); an actual contract change names the changed task and keeps
// resume blocked. Both document the restore choice.
func TestRecoveryPreviewExplainsTaskFileEdits(t *testing.T) {
	t.Run("formatting only", func(t *testing.T) {
		c := retainedRunFixture(t)
		b := Backlog{Version: 1, Sources: []string{}, Goal: "preview fixture", Tasks: []Task{
			{ID: "T001", Title: "retained work", Priority: 1, Status: "doing"},
		}}
		compact, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(c.Todo, append(compact, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
		tf := doc.Comparisons.TaskFile
		if tf.State != "changed" || tf.Parsed != "identical" {
			t.Fatalf("task file comparison: %+v", tf)
		}
		if len(tf.ChangedContracts) != 0 || len(tf.DataChanges) != 0 {
			t.Fatalf("no contract or task data changed: %+v", tf)
		}
		guard := doc.guard(guardBacklogChanged)
		if guard.Verdict != verdictPass || !strings.Contains(guard.Detail, "formatting") {
			t.Fatalf("guard verdicts: %+v", doc.Guards)
		}
		resume := doc.choice("resume")
		if resume.Status != "available" {
			t.Fatalf("an equivalent edit must not block resume: %+v", resume)
		}
		restore := doc.choice("restore-task-file")
		if restore.Status != "available" || restore.Apply.Mode != "command" {
			t.Fatalf("restore choice: %+v", restore)
		}
		if !strings.Contains(restore.Apply.Command, doc.Token.Value) {
			t.Fatalf("the checked application names the preview token: %+v", restore.Apply)
		}
		if len(restore.Preserves) == 0 || len(restore.Changes) == 0 {
			t.Fatalf("restore must state what it preserves and discards: %+v", restore)
		}
		if !strings.Contains(strings.Join(restore.Changes, "; "), "optional") {
			t.Fatalf("restore must state that the run accepts the equivalent bytes without it: %+v", restore)
		}
	})
	t.Run("actual contract change", func(t *testing.T) {
		c := retainedRunFixture(t)
		b := Backlog{Version: 1, Sources: []string{}, Goal: "preview fixture", Tasks: []Task{
			{ID: "T001", Title: "renamed outside Looper", Priority: 1, Status: "doing"},
		}}
		if err := writeJSON(c.Todo, b); err != nil {
			t.Fatal(err)
		}
		_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
		tf := doc.Comparisons.TaskFile
		if tf.Parsed != "differs" || len(tf.ChangedContracts) != 1 || tf.ChangedContracts[0] != "T001" {
			t.Fatalf("changed contract evidence: %+v", tf)
		}
		if doc.guard(guardBacklogChanged).Verdict != verdictFail {
			t.Fatalf("a contract change must keep the guard refusing: %+v", doc.Guards)
		}
		if resume := doc.choice("resume"); resume.Status != "blocked" {
			t.Fatalf("resume choice: %+v", resume)
		}
		if restore := doc.choice("restore-task-file"); restore.Status != "available" {
			t.Fatalf("restore choice: %+v", restore)
		}
	})
	t.Run("task data change without a contract change", func(t *testing.T) {
		c := retainedRunFixture(t)
		// A status edit changes task data its contract hash does not cover;
		// it is a real change, so the guard keeps refusing and the evidence
		// names the task (AX-018).
		b := Backlog{Version: 1, Sources: []string{}, Goal: "preview fixture", Tasks: []Task{
			{ID: "T001", Title: "retained work", Priority: 1, Status: "done"},
		}}
		if err := writeJSON(c.Todo, b); err != nil {
			t.Fatal(err)
		}
		_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
		tf := doc.Comparisons.TaskFile
		if tf.Parsed != "differs" || len(tf.ChangedContracts) != 0 {
			t.Fatalf("no contract changed: %+v", tf)
		}
		if len(tf.DataChanges) != 1 || tf.DataChanges[0] != "T001" {
			t.Fatalf("task data evidence must name the changed task: %+v", tf)
		}
		if doc.guard(guardBacklogChanged).Verdict != verdictFail {
			t.Fatalf("a task data change must keep the guard refusing: %+v", doc.Guards)
		}
		if resume := doc.choice("resume"); resume.Status != "blocked" {
			t.Fatalf("resume choice: %+v", resume)
		}
	})
}

// TestRecoveryPreviewMirrorsHeldEditImport mirrors the T058 startup decision
// read-only: a task-file edit beside retained work whose results can replay
// reports the guard as the import the next run performs, keeps resume
// available, and compares retained contracts against the file the resume
// imports; work that cannot replay keeps the refusal.
func TestRecoveryPreviewMirrorsHeldEditImport(t *testing.T) {
	// replayableRunFixture saves a retained attempt holding a result commit,
	// the shape a cancelled hold leaves behind.
	replayableRunFixture := func(t *testing.T) Config {
		c := retainedRunFixture(t)
		store := openStateForEdit(t, c)
		s, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		s.Attempts[0].Phase = "ready"
		s.Attempts[0].ResultCommit = s.Tip
		if err = store.Save(s); err != nil {
			t.Fatal(err)
		}
		if err = store.Close(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	edited := func(t *testing.T, c Config, title string) {
		t.Helper()
		b := Backlog{Version: 1, Sources: []string{}, Goal: "preview fixture", Tasks: []Task{
			{ID: "T001", Title: title, Priority: 1, Status: "doing"},
			{ID: "T002", Title: "added outside Looper", Priority: 1, Status: "todo"},
		}}
		if err := writeJSON(c.Todo, b); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("import at resume", func(t *testing.T) {
		c := replayableRunFixture(t)
		edited(t, c, "retained work")
		_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
		guard := doc.guard(guardBacklogChanged)
		if guard.Verdict != verdictPass || !strings.Contains(guard.Detail, "imports the edit at startup") {
			t.Fatalf("a replay-safe edit must report the startup import: %+v", guard)
		}
		if contract := doc.guard(guardTaskContract); contract.Verdict != verdictPass || !strings.Contains(contract.Detail, "the task file the next run imports") {
			t.Fatalf("the contract comparison must use the imported backlog: %+v", contract)
		}
		resume := doc.choice("resume")
		if resume.Status != "available" || !strings.Contains(resume.Changes[0], "imports at startup") {
			t.Fatalf("resume must stay available and name the import: %+v", resume)
		}
		restore := doc.choice("restore-task-file")
		if restore.Status != "available" || !strings.Contains(strings.Join(restore.Changes, "; "), "optional") {
			t.Fatalf("restore must state that the resume imports the edit without it: %+v", restore)
		}
	})
	t.Run("changed contract in the imported file", func(t *testing.T) {
		c := replayableRunFixture(t)
		edited(t, c, "renamed outside Looper")
		_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
		// Every import path refuses the edit (T119), so the mirror reports the
		// refusal at the task-file guard instead of an import that strands the
		// retained work at the next startup's contract check.
		guard := doc.guard(guardBacklogChanged)
		if guard.Verdict != verdictFail || !strings.Contains(guard.Detail, "changes the contract of retained task(s) T001") {
			t.Fatalf("a contract-breaking edit must report the refused import: %+v", guard)
		}
		if contract := doc.guard(guardTaskContract); contract.Verdict != verdictPass {
			t.Fatalf("a refused import never reaches the contract comparison: %+v", contract)
		}
		if resume := doc.choice("resume"); resume.Status != "blocked" {
			t.Fatalf("resume must stay blocked on the refused import: %+v", resume)
		}
		imp := doc.choice("import-backlog")
		if imp.Status != "blocked" || !strings.Contains(strings.Join(imp.BlockedBy, "; "), "retained task(s) T001") {
			t.Fatalf("the import choice must refuse naming the retained task: %+v", imp)
		}
	})
	t.Run("no replayable result keeps the refusal", func(t *testing.T) {
		c := retainedRunFixture(t)
		edited(t, c, "retained work")
		_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
		guard := doc.guard(guardBacklogChanged)
		if guard.Verdict != verdictFail || !strings.Contains(guard.Detail, "no result can replay") {
			t.Fatalf("a running attempt without a result must keep the refusal: %+v", guard)
		}
		if resume := doc.choice("resume"); resume.Status != "blocked" {
			t.Fatalf("resume must stay blocked: %+v", resume)
		}
	})
}

// TestRecoveryPreviewExplainsUnbornHeadAndUnreadableEvidence covers the
// cases whose evidence is missing: an unborn HEAD keeps the committed
// comparison unlisted, an unparseable task file makes the restore unsafe
// with the missing evidence named, and a missing task file names the restore
// requirement. No safe action is fabricated.
func TestRecoveryPreviewExplainsUnbornHeadAndUnreadableEvidence(t *testing.T) {
	t.Run("unborn head", func(t *testing.T) {
		c := retainedRunFixture(t)
		if _, err := gitText(c.Root, "update-ref", "-d", "HEAD"); err != nil {
			t.Fatal(err)
		}
		_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
		br := doc.Comparisons.Branch
		if br.State != branchUnborn || !br.Unborn {
			t.Fatalf("branch comparison: %+v", br)
		}
		guard := doc.guard(guardBranchMoved)
		if guard.Verdict != verdictFail || !strings.Contains(guard.Detail, "unborn") {
			t.Fatalf("branch guard: %+v", guard)
		}
		if br.CommittedChanges != nil {
			t.Fatalf("no committed comparison exists for an unborn head: %+v", br.CommittedChanges)
		}
		resume := doc.choice("resume")
		if resume.Status != "blocked" || !strings.Contains(strings.Join(resume.BlockedBy, "; "), guardBranchMoved) {
			t.Fatalf("resume choice: %+v", resume)
		}
	})
	t.Run("invalid task file makes restore unsafe", func(t *testing.T) {
		c := retainedRunFixture(t)
		if err := os.WriteFile(c.Todo, []byte("{not json"), 0600); err != nil {
			t.Fatal(err)
		}
		_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
		tf := doc.Comparisons.TaskFile
		if tf.State != "invalid" || tf.Parsed != "unknown" {
			t.Fatalf("task file comparison: %+v", tf)
		}
		if doc.guard(guardTaskFileInvalid).Verdict != verdictFail {
			t.Fatalf("guards: %+v", doc.Guards)
		}
		restore := doc.choice("restore-task-file")
		if restore.Status != "unsafe" || len(restore.MissingEvidence) == 0 {
			t.Fatalf("restore must stay unsafe with its missing evidence named: %+v", restore)
		}
	})
	t.Run("missing task file", func(t *testing.T) {
		c := retainedRunFixture(t)
		if err := os.Remove(c.Todo); err != nil {
			t.Fatal(err)
		}
		_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
		if doc.Comparisons.TaskFile.State != "missing" {
			t.Fatalf("task file comparison: %+v", doc.Comparisons.TaskFile)
		}
		if doc.guard(guardTaskFileMissing).Verdict != verdictFail {
			t.Fatalf("guards: %+v", doc.Guards)
		}
	})
}

// TestRecoveryPreviewBoundsChangedPathListings creates more changed paths
// than the listing bound: the document shows at most the bound, carries the
// complete count, names the detail route, and stays within its byte budget.
func TestRecoveryPreviewBoundsChangedPathListings(t *testing.T) {
	c := retainedRunFixture(t)
	for i := 0; i < 30; i++ {
		if err := os.WriteFile(filepath.Join(c.Root, fmt.Sprintf("file-%02d.txt", i)), []byte("edit\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Stage the edits so the listing under test is the working-tree diff
	// against the published tree; untracked paths land in their own listing.
	if _, err := gitText(c.Root, "add", "--", "."); err != nil {
		t.Fatal(err)
	}
	_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
	wt := doc.Comparisons.WorkingTree
	if wt.State != "changed" {
		t.Fatalf("working tree comparison: %+v", wt)
	}
	changed := wt.ChangedPaths
	if len(changed.Paths) > maxGuardChangedPaths || changed.Count != 30 {
		t.Fatalf("changed listing bound: %d shown of %d", len(changed.Paths), changed.Count)
	}
	if changed.Command == "" || !strings.Contains(changed.Command, "git diff --name-only") {
		t.Fatalf("detail route: %+v", changed)
	}
	if doc.guard(guardProjectTreeChanged).Verdict != verdictFail {
		t.Fatalf("tree guard: %+v", doc.Guards)
	}
	if resume := doc.choice("resume"); resume.Status != "blocked" {
		t.Fatalf("resume choice: %+v", resume)
	}
}

// TestRecoveryPreviewTokenBindsObservations proves the token is a staleness
// check: it is stable across repeated previews and changes when the task
// file or the project tree changes.
func TestRecoveryPreviewTokenBindsObservations(t *testing.T) {
	c := retainedRunFixture(t)
	_, first, _ := runPreviewCLI(t, "--contract", "v2", "--json")
	_, second, _ := runPreviewCLI(t, "--contract", "v2", "--json")
	if first.Token.Value != second.Token.Value {
		t.Fatalf("the token changed between two previews of the same state: %s != %s", first.Token.Value, second.Token.Value)
	}
	if err := os.WriteFile(filepath.Join(c.Root, "unrelated.txt"), []byte("edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, third, _ := runPreviewCLI(t, "--contract", "v2", "--json")
	if third.Token.Value == first.Token.Value {
		t.Fatal("a changed working tree must invalidate the token")
	}
	if err := os.WriteFile(c.Todo, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, fourth, _ := runPreviewCLI(t, "--contract", "v2", "--json")
	if fourth.Token.Value == third.Token.Value {
		t.Fatal("a changed task file must invalidate the token")
	}
}

// TestRecoveryPreviewCommandContract pins the command surface: the bare
// command is a usage error, --preview belongs to recovery alone, the JSON
// document needs contract version 2, and no saved run reports run_not_found.
func TestRecoveryPreviewCommandContract(t *testing.T) {
	t.Run("bare command is a usage error", func(t *testing.T) {
		retainedRunFixture(t)
		var out, stderr bytes.Buffer
		code := Main([]string{"recovery"}, &out, &stderr)
		if code != exitUsage {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "--preview") {
			t.Fatalf("the error must name the supported invocation: %q", stderr.String())
		}
		if out.Len() > 0 {
			t.Fatalf("a usage failure writes nothing to stdout: %q", out.String())
		}
	})
	t.Run("preview belongs to recovery", func(t *testing.T) {
		_, err := Parse([]string{"status", "--preview"})
		if err == nil {
			t.Fatal("--preview must be a usage error on other commands")
		}
		var p *PublicError
		if !errors.As(err, &p) || p.Code != "option_command_mismatch" {
			t.Fatalf("usage failure: %v", err)
		}
	})
	t.Run("json needs the version 2 contract", func(t *testing.T) {
		retainedRunFixture(t)
		var out, stderr bytes.Buffer
		code := Main([]string{"recovery", "--preview", "--json"}, &out, &stderr)
		if code != exitOperationFailed {
			t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "contract version 2") {
			t.Fatalf("the error must name the versioned document: %q", stderr.String())
		}
	})
	t.Run("no saved run reports one error document under version 2", func(t *testing.T) {
		repoConfig(t, "run")
		var out, stderr bytes.Buffer
		code := Main([]string{"recovery", "--preview", "--contract", "v2", "--json"}, &out, &stderr)
		if code != exitUnavailable {
			t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
		}
		var doc errorDocument
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("stdout is one error document: %v\n%s", err, out.String())
		}
		if doc.Error.Code != "run_not_found" {
			t.Fatalf("code %q", doc.Error.Code)
		}
	})
	t.Run("no saved run", func(t *testing.T) {
		repoConfig(t, "run")
		var out, stderr bytes.Buffer
		code := Main([]string{"recovery", "--preview", "--contract", "v2", "--json"}, &out, &stderr)
		if code != exitUnavailable {
			t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
		}
		var doc errorDocument
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("stdout is one error document: %v\n%s", err, out.String())
		}
		if doc.Error.Code != "run_not_found" {
			t.Fatalf("code %q", doc.Error.Code)
		}
	})
	t.Run("plain works under the legacy contract", func(t *testing.T) {
		retainedRunFixture(t)
		var out, stderr bytes.Buffer
		code := Main([]string{"recovery", "--preview", "--plain"}, &out, &stderr)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		for _, want := range []string{"Recovery preview", "Task file:", "Branch:", "Working tree:", "Guards:", "Choices:", "Token:"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("plain preview missing %q:\n%s", want, out.String())
			}
		}
	})
}

// TestRecoveryPreviewWithHeldLock blocks resume when a controller owns the
// repository: the preview itself still answers while another open file
// description holds the lock, and the blocked reason names the owner.
func TestRecoveryPreviewWithHeldLock(t *testing.T) {
	c := retainedRunFixture(t)
	common, err := gitRead(c.Root, "rev-parse", "--git-common-dir")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(c.Root, common)
	}
	f, err := os.OpenFile(filepath.Join(common, "looper.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
	if !doc.Controller.Owned {
		t.Fatalf("controller ownership: %+v", doc.Controller)
	}
	resume := doc.choice("resume")
	if resume.Status != "blocked" || !strings.Contains(strings.Join(resume.BlockedBy, "; "), "controller owns the repository") {
		t.Fatalf("resume choice: %+v", resume)
	}
	if !doc.Run.Retained {
		t.Fatalf("the preview still answers with the run state: %+v", doc.Run)
	}
}

// TestRecoveryPreviewWithoutRetainedWorkStartsFresh pins the startup rule
// for a saved run with nothing retained: the branch and tree guards cannot
// refuse, because the next start archives the saved run and plans a new
// baseline. A preview that reported those edits as blocking would misstate
// what `looper` does next.
func TestRecoveryPreviewWithoutRetainedWorkStartsFresh(t *testing.T) {
	c := repoConfig(t, "run")
	b := Backlog{Version: 1, Sources: []string{}, Goal: "finished fixture", Tasks: []Task{
		{ID: "T001", Title: "done work", Priority: 1, Status: "done", Tags: []string{"project-done"}},
	}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	// Local edits exist, but no retained work does.
	if err = os.WriteFile(filepath.Join(c.Root, "notes.md"), []byte("unrelated edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
	if doc.Run.Retained {
		t.Fatalf("run state: %+v", doc.Run)
	}
	for _, code := range []string{guardBranchMoved, guardProjectTreeChanged} {
		guard := doc.guard(code)
		if guard.Verdict != verdictPass || !strings.Contains(guard.Detail, "new baseline") {
			t.Fatalf("guard %s: %+v", code, guard)
		}
	}
	resume := doc.choice("resume")
	if resume.Status != "available" {
		t.Fatalf("resume choice: %+v", resume)
	}
	if !strings.Contains(strings.Join(resume.Changes, "; "), "archived") {
		t.Fatalf("resume must state the fresh baseline: %+v", resume)
	}
	if doc.choice("publish-retained").Name != "" || doc.choice("restore-task-file").Name != "" {
		t.Fatalf("retained-work choices must stay absent: %+v", doc.Choices)
	}
}

// TestGuardErrorsRouteToThePreview closes the loop AX-015 opened: every
// retained-work guard document names the read-only preview as its next
// action, so a caller that meets the guard learns the supported recovery
// operation instead of reading source or private state.
func TestGuardErrorsRouteToThePreview(t *testing.T) {
	c, _ := startupBacklogFixture(t)
	b := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{
		{ID: "T001", Title: "renamed outside Looper", Priority: 1, Status: "doing"},
	}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	doc := retentionDoc(t, c, "run", startupGuard(t, c))
	found := false
	for _, a := range doc.Error.NextActions {
		if a.Command == "looper recovery --preview --contract v2 --json" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the guard must route to the preview: %+v", doc.Error.NextActions)
	}
}

// guard returns one guard verdict by code.
func (d *recoveryPreviewDocument) guard(code string) recoveryGuard {
	for _, g := range d.Guards {
		if g.Code == code {
			return g
		}
	}
	return recoveryGuard{}
}

// choice returns one enumerated choice by name.
func (d *recoveryPreviewDocument) choice(name string) recoveryChoice {
	for _, ch := range d.Choices {
		if ch.Name == name {
			return ch
		}
	}
	return recoveryChoice{}
}

// newRecoveryEscapeFixture saves one interrupted run whose every recorded
// string the preview renders is escape-heavy: ampersand-heavy identity,
// digests, branch names, retained attempts, a retained publication over an
// ampersand-named verified file, committed and local changes, and a changed
// task file. The repository itself is real, so the path listings and guard
// decisions run exactly as they would for a genuine run.
func newRecoveryEscapeFixture(t *testing.T, adversarialRevisions bool) Config {
	t.Helper()
	c := repoConfig(t, "run")
	esc := preflightEscapeHeavy
	// revisionOf fills one digest- or revision-shaped field: schema-valid hex
	// for the document the CLI test validates, escape-heavy text for the
	// terminal-fit proof, which pins sizes without the schema.
	revisionOf := func(n int) string {
		if adversarialRevisions {
			return esc(n)
		}
		return strings.Repeat("ab", n/2)
	}
	commit := func(name, body, message string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(c.Root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := gitText(c.Root, "add", "--", name); err != nil {
			t.Fatal(err)
		}
		if _, err := gitText(c.Root, "commit", "-q", "-m", message); err != nil {
			t.Fatal(err)
		}
	}
	commit("seed.md", "baseline\n", "seed")
	head1 := strings.TrimSpace(mustGitText(t, c.Root, "rev-parse", "HEAD"))
	tree1 := strings.TrimSpace(mustGitText(t, c.Root, "rev-parse", "HEAD^{tree}"))
	commit("amp&<>-"+esc(2)+".txt", "verified content\n", "verified")
	head2 := strings.TrimSpace(mustGitText(t, c.Root, "rev-parse", "HEAD"))
	tree2 := strings.TrimSpace(mustGitText(t, c.Root, "rev-parse", "HEAD^{tree}"))
	// Local edits the saved baseline did not publish: a modified tracked file
	// and an untracked ampersand-named one.
	if err := os.WriteFile(filepath.Join(c.Root, "seed.md"), []byte("local edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Root, "untracked&amp.txt"), []byte("untracked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The observed task file parses but differs from the saved contract, so
	// the comparison reports changed contracts with escape-heavy identities.
	observed := Backlog{Version: 1, Sources: []string{}, Goal: "observed", Tasks: []Task{
		{ID: "T001", Title: "observed work", Priority: 1, Status: "doing"},
	}}
	if err := writeJSON(c.Todo, observed); err != nil {
		t.Fatal(err)
	}
	attempts := []*Attempt{}
	tasks := []Task{{ID: "T9", Title: "unrelated", Priority: 3, Status: "todo"}}
	for i := 0; i < 3; i++ {
		task := Task{ID: fmt.Sprintf("A%04d", i) + esc(20), Title: esc(60), Priority: 1, Status: "doing"}
		attempts = append(attempts, &Attempt{
			ID: esc(20) + fmt.Sprintf("-%04d", i), Task: task, Agent: esc(10), Base: esc(20),
			Directory: filepath.Join(c.Root, "taskwork", fmt.Sprintf("a%04d", i)+esc(10)),
			Worktree:  filepath.Join(c.Root, "taskwork", fmt.Sprintf("w%04d", i)+esc(10)),
			Ref:       "ref", Phase: "running", ResultCommit: revisionOf(40), Worktrees: []string{},
		})
		// The backlog holds a changed contract for every retained attempt,
		// so the task-contract guard refuses with every mismatched identity
		// joined into one detail.
		changed := task
		changed.Title = esc(60) + " changed"
		tasks = append(tasks, changed)
	}
	_, dir := projectPaths(c)
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Save(&State{
		Version: 2, Root: esc(40), Todo: esc(40), Head: head1, Branch: "refs/heads/" + esc(10),
		Ref: "refs/looper/" + esc(20), Tip: head1, Published: tree1, ExportHash: revisionOf(64),
		Backlog:  Backlog{Version: 1, Sources: []string{}, Goal: esc(60), Tasks: tasks},
		Attempts: attempts,
		Publication: &Candidate{Base: head1, Commit: revisionOf(40), Tree: tree2, Before: tree1,
			AttemptID: esc(20), OK: true},
	}); err != nil {
		t.Fatal(err)
	}
	_ = head2
	return c
}

// mustGitText runs one git query that must succeed.
func mustGitText(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := gitText(root, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assertRecoveryStringsBounded checks the fit's structural discipline: every
// content-derived string obeys the bound the document states, measured in the
// bytes the JSON encoder writes. Commands obey the wider of that bound and
// the route bound that gated an embedded identifier, because a truncated
// command would name work that does not exist.
func assertRecoveryStringsBounded(t *testing.T, doc *recoveryPreviewDocument, fitMax int) {
	t.Helper()
	routeMax := max(fitMax, len("looper inspect ")+statusRouteID+len(" --json"))
	bounded := func(label, s string, limit int) {
		t.Helper()
		if jsonEscapedLen(s) > limit {
			t.Errorf("%s exceeds the %d byte bound: %d emitted", label, limit, jsonEscapedLen(s))
		}
	}
	str := func(label, s string) { bounded(label, s, fitMax) }
	rev := func(label, s string) { bounded(label, s, max(fitMax, recoveryRevisionMax)) }
	cmd := func(label, s string) { bounded(label, s, routeMax) }
	str("project root", doc.Project.Root)
	str("project todo path", doc.Project.TodoPath)
	str("runtime dir", doc.Project.RuntimeDir)
	str("run id", doc.Run.ID)
	for i, id := range doc.Run.RetainedTasks {
		str(fmt.Sprintf("retained task %d", i), id)
	}
	for i, a := range doc.Run.Attempts {
		str(fmt.Sprintf("attempt %d id", i), a.AttemptID)
		str(fmt.Sprintf("attempt %d task id", i), a.TaskID)
		str(fmt.Sprintf("attempt %d phase", i), a.Phase)
		str(fmt.Sprintf("attempt %d agent", i), a.Agent)
		rev(fmt.Sprintf("attempt %d result commit", i), a.ResultCommit)
	}
	str("controller session", doc.Controller.Session)
	tf := doc.Comparisons.TaskFile
	rev("expected digest", tf.ExpectedDigest)
	rev("observed digest", tf.ObservedDigest)
	str("task file unknown", tf.Unknown)
	for i, id := range tf.ChangedContracts {
		str(fmt.Sprintf("changed contract %d", i), id)
	}
	br := doc.Comparisons.Branch
	rev("expected head", br.ExpectedHead)
	str("expected branch", br.ExpectedBranch)
	rev("observed head", br.ObservedHead)
	str("observed branch", br.ObservedBranch)
	str("branch reason", br.Reason)
	str("branch unknown", br.Unknown)
	listing := func(label string, l *boundedListing) {
		if l == nil {
			return
		}
		for i, p := range l.Paths {
			str(fmt.Sprintf("%s path %d", label, i), p)
		}
		cmd(fmt.Sprintf("%s command", label), l.Command)
	}
	listing("committed changes", br.CommittedChanges)
	listing("staged files", br.StagedFiles)
	wt := doc.Comparisons.WorkingTree
	rev("published tree", wt.PublishedTree)
	str("working tree unknown", wt.Unknown)
	str("working tree basis", wt.Basis)
	listing("changed paths", wt.ChangedPaths)
	listing("untracked paths", wt.UntrackedPaths)
	if pub := doc.Comparisons.Publication; pub != nil {
		rev("verified commit", pub.VerifiedCommit)
		str("publication attempt id", pub.AttemptID)
		str("publication task id", pub.TaskID)
		rev("accepted tree", pub.AcceptedTree)
		rev("verified tree", pub.VerifiedTree)
		str("publication unknown", pub.Unknown)
		listing("publication changes", pub.Changes)
		listing("publication local edits", pub.LocalEdits)
	}
	for i, g := range doc.Guards {
		str(fmt.Sprintf("guard %d detail", i), g.Detail)
		cmd(fmt.Sprintf("guard %d command", i), g.Command)
	}
	for i, ch := range doc.Choices {
		for j, v := range ch.Preserves {
			str(fmt.Sprintf("choice %d preserves %d", i, j), v)
		}
		for j, v := range ch.Changes {
			str(fmt.Sprintf("choice %d changes %d", i, j), v)
		}
		for j, v := range ch.BlockedBy {
			str(fmt.Sprintf("choice %d blocked by %d", i, j), v)
		}
		for j, v := range ch.MissingEvidence {
			str(fmt.Sprintf("choice %d missing evidence %d", i, j), v)
		}
		if ch.Apply != nil {
			str(fmt.Sprintf("choice %d apply description", i), ch.Apply.Description)
			cmd(fmt.Sprintf("choice %d apply command", i), ch.Apply.Command)
		}
	}
}

// TestRecoveryPreviewBoundsEscapeExpandedContent pins the published budget
// against saved records the JSON encoder expands sixfold: a preview whose
// every recorded string is escape-heavy must still answer one schema-valid
// document at or under the budget its own limits report.
func TestRecoveryPreviewBoundsEscapeExpandedContent(t *testing.T) {
	newRecoveryEscapeFixture(t, false)
	_, doc, raw := runPreviewCLI(t, "--contract", "v2", "--json")
	if doc.Limits.BytesBudget < len(raw) {
		t.Fatalf("limits report a %d byte budget for a %d byte document", doc.Limits.BytesBudget, len(raw))
	}
	assertRecoveryStringsBounded(t, doc, doc.Limits.StringMax)
	// The preview stays useful, not merely small: the guards decide, the
	// choices keep their shape, and the token binds the observation.
	if len(doc.Guards) == 0 || len(doc.Choices) == 0 || doc.Token.Value == "" {
		t.Fatalf("the bounded preview lost its guards, choices, or token: %+v", doc)
	}
	if !doc.Run.Retained || doc.Run.PublicationHeld != true {
		t.Fatalf("the retained work and held publication must surface: %+v", doc.Run)
	}
	// Every retained attempt's contract changed, so the task-contract guard
	// refuses with the mismatched identities joined into one bounded detail.
	contractGuard := -1
	for i, g := range doc.Guards {
		if g.Code == guardTaskContract {
			contractGuard = i
		}
	}
	if contractGuard < 0 || doc.Guards[contractGuard].Verdict != verdictFail {
		t.Fatalf("the changed task contracts must refuse: %+v", doc.Guards)
	}
	// The plain rendering reads the same structure and stays bounded too.
	code, _, plain := runPreviewCLI(t, "--contract", "v2")
	if code != 0 || !strings.Contains(plain, "Recovery preview") {
		t.Fatalf("plain preview must render the same facts (exit %d):\n%s", code, plain)
	}
	if len(plain) > 4*recoveryPreviewBytesBudget {
		t.Fatalf("plain rendering of the bounded document is not bounded: %d bytes", len(plain))
	}
}

// TestRecoveryPreviewTerminalFitBoundsAnyContent pins the structural
// guarantee behind the budget: the ladder's terminal fit bounds every string
// to one rune at worst, so the document it renders is structure alone
// whatever the saved records hold. Every earlier fit obeys the string bound
// it states in emitted bytes, and the ladder's chosen document fits whatever
// the earlier fits measured.
func TestRecoveryPreviewTerminalFitBoundsAnyContent(t *testing.T) {
	// The pattern-valid variant carries schema-valid hex in its digest- and
	// revision-shaped fields, so its terminal document must also validate;
	// the adversarial variant puts escape-heavy text in those fields, which
	// no rendering can make pattern-valid — for it the budget is the
	// guarantee, and the clampRevision bound keeps the garbage bounded.
	for _, adversarialRevisions := range []bool{false, true} {
		c := newRecoveryEscapeFixture(t, adversarialRevisions)
		_, dir := projectPaths(c)
		store, err := openObservedStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		snap, err := store.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		ev, err := collectRecoveryEvidence(c, dir, snap)
		if err != nil {
			t.Fatal(err)
		}
		store.Close()
		for _, fit := range recoveryFits {
			assertRecoveryStringsBounded(t, renderRecoveryDocument(ev, fit), fit.stringMax)
		}
		terminal := renderRecoveryDocument(ev, recoveryTerminalFit)
		if size := recoveryEncoded(terminal); size > recoveryPreviewBytesBudget {
			t.Fatalf("adversarial revisions %t: the terminal fit renders %d bytes, over the %d byte budget",
				adversarialRevisions, size, recoveryPreviewBytesBudget)
		}
		if !adversarialRevisions {
			remarshaled, err := json.Marshal(terminal)
			if err != nil {
				t.Fatal(err)
			}
			if err = contractschema.ValidateJSON(recoveryPreviewSchema(t), remarshaled); err != nil {
				t.Fatalf("the terminal document violates its schema (a pattern literal must never truncate): %v\n%s", err, remarshaled)
			}
		}
		chosen, err := buildRecoveryPreview(c, dir, snap)
		if err != nil {
			t.Fatal(err)
		}
		if recoveryEncoded(chosen) > recoveryPreviewBytesBudget {
			t.Fatalf("adversarial revisions %t: the chosen document exceeds the budget it reports: %d bytes",
				adversarialRevisions, recoveryEncoded(chosen))
		}
	}
}
