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

// The OW-02 and OW-03 acceptance fixtures. Every fixture drives the public
// CLI: a preview is one read-only document with a content-sensitive token, an
// application re-verifies that token under the locks and writes exactly one
// project commit, and every refused or crashed operation preserves both
// sides.

// publishRunCLI drives the publish command through Main and returns the exit
// code with both output streams.
func publishRunCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, stderr strings.Builder
	code := Main(append([]string{"publish"}, args...), &out, &stderr)
	return code, out.String(), stderr.String()
}

// publishableRunFixture saves one stopped run whose accepted work is ready to
// publish: the baseline matches, no publication waits, the task file matches
// the accepted export, and the working tree is exactly the accepted tree.
func publishableRunFixture(t *testing.T) Config {
	t.Helper()
	c := repoConfig(t)
	for _, kv := range [][2]string{{"user.name", "Fixture Owner"}, {"user.email", "owner@example.test"}} {
		if _, err := gitText(c.Root, "config", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(c.Root, "seed.md"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "add", "--", "seed.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "commit", "-q", "-m", "seed"); err != nil {
		t.Fatal(err)
	}
	b := Backlog{Version: 1, Sources: []string{}, Goal: "publish fixture", Tasks: []Task{
		{ID: "T001", Title: "accepted work", Priority: 1, Status: "doing"},
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
	// The accepted work: one new file and one symlink, both outside the
	// project branch until the publication commits them.
	if err = os.WriteFile(filepath.Join(c.Root, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("feature.txt", filepath.Join(c.Root, "feature-link")); err != nil {
		t.Fatal(err)
	}
	tree, err := e.Repo.SnapshotFrom(c.Root, e.State.Published)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := e.Repo.Commit(tree, []string{e.State.Tip}, "Accept T001")
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Repo.Ref(e.State.Ref, commit); err != nil {
		t.Fatal(err)
	}
	task := e.State.Backlog.Task("T001")
	task.Status = "done"
	snapshot := *task
	e.State.Attempts = append(e.State.Attempts, &Attempt{
		ID: "task-accepted", Task: snapshot, Agent: "claude", Base: e.State.Tip,
		Directory: filepath.Join(e.Dir, "task-accepted"), Phase: "done", ResultCommit: commit, Worktrees: []string{},
	})
	e.State.Tip = commit
	e.State.Published = tree
	e.State.Finished = true
	e.State.Publication = nil
	if err = e.export(); err != nil {
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

// publishPreviewDoc runs the version 2 preview and decodes its document.
func publishPreviewDoc(t *testing.T, c Config) (*publishPreviewDocument, int, string) {
	t.Helper()
	code, out, stderr := publishRunCLI(t, "--preview", "--contract", "v2", "--json")
	if code != exitOK {
		return nil, code, out + stderr
	}
	var doc publishPreviewDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("preview document: %v\n%s", err, out)
	}
	return &doc, code, out
}

// publishTokenOf returns the token of one publishable preview.
func publishTokenOf(t *testing.T, c Config) string {
	t.Helper()
	doc, code, raw := publishPreviewDoc(t, c)
	if code != exitOK {
		t.Fatalf("preview exit %d: %s", code, raw)
	}
	if !doc.Publishable || doc.Token == "" {
		guards, _ := json.Marshal(doc.Guards)
		t.Fatalf("preview is not publishable: %s", guards)
	}
	return doc.Token
}

func publishGuardVerdict(doc *publishPreviewDocument, code string) (string, bool) {
	for _, g := range doc.Guards {
		if g.Code == code {
			return g.Verdict, true
		}
	}
	return "", false
}

// The preview is one read-only document: every guard passes, the token binds
// the composition, and nothing on disk moves.
func TestPublishPreviewReadOnlyWithToken(t *testing.T) {
	c := publishableRunFixture(t)
	_, dir := projectPaths(c)
	before := snapshotProject(t, c, dir)
	doc, code, raw := publishPreviewDoc(t, c)
	if code != exitOK {
		t.Fatalf("preview exit %d: %s", code, raw)
	}
	if doc.Kind != publishPreviewKind || doc.ContractVersion != ContractVersionV2 {
		t.Fatalf("document identity: %+v", doc)
	}
	if !doc.Publishable || len(doc.Token) != 64 {
		t.Fatalf("publishable %v token %q", doc.Publishable, doc.Token)
	}
	for _, g := range doc.Guards {
		if g.Verdict != "pass" {
			t.Fatalf("guard %s: %s (%s)", g.Code, g.Verdict, g.Detail)
		}
	}
	if !containsString(doc.Changes.Paths, "feature.txt") || !containsString(doc.Changes.Paths, "feature-link") {
		t.Fatalf("changed paths: %v", doc.Changes.Paths)
	}
	if after := snapshotProject(t, c, dir); after != before {
		t.Fatal("the preview mutated the project")
	}
}

// The text preview renders the same facts for a terminal operator.
func TestPublishPreviewText(t *testing.T) {
	publishableRunFixture(t)
	code, out, _ := publishRunCLI(t, "--preview")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"Publish preview", "feature.txt", "Publishable: yes", "Token:", "Guards:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text preview lacks %q:\n%s", want, out)
		}
	}
}

// The version 2 document refuses a plain --json request.
func TestPublishPreviewJSONRequiresV2(t *testing.T) {
	publishableRunFixture(t)
	code, _, stderr := publishRunCLI(t, "--preview", "--json")
	if code == exitOK {
		t.Fatal("--json without --contract v2 must refuse")
	}
	if !strings.Contains(stderr, "contract") {
		t.Fatalf("refusal: %s", stderr)
	}
}

// Every refused situation keeps both sides untouched and reports the guard.
func TestPublishPreviewRefusals(t *testing.T) {
	staged := func(t *testing.T) Config {
		c := publishableRunFixture(t)
		if err := os.WriteFile(filepath.Join(c.Root, "extra.txt"), []byte("staged\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := gitText(c.Root, "add", "--", "extra.txt"); err != nil {
			t.Fatal(err)
		}
		return c
	}
	dirtyTaskFile := func(t *testing.T) Config {
		c := publishableRunFixture(t)
		b := Backlog{Version: 1, Sources: []string{}, Goal: "publish fixture", Tasks: []Task{
			{ID: "T001", Title: "retitled outside Looper", Priority: 1, Status: "done"},
		}}
		if err := writeJSON(c.Todo, b); err != nil {
			t.Fatal(err)
		}
		return c
	}
	divergedWork := func(t *testing.T) Config {
		c := publishableRunFixture(t)
		if err := os.WriteFile(filepath.Join(c.Root, "feature.txt"), []byte("edited\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return c
	}
	unrelatedFile := func(t *testing.T) Config {
		c := publishableRunFixture(t)
		if err := os.WriteFile(filepath.Join(c.Root, "scratch.txt"), []byte("unrelated\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return c
	}
	publicationPending := func(t *testing.T) Config {
		c := publishableRunFixture(t)
		_, dir := projectPaths(c)
		store, err := OpenStore(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		s, err := store.Load()
		if err != nil || s == nil {
			t.Fatal(err)
		}
		s.Publication = &Candidate{Base: s.Tip, Tree: s.Published, OK: true}
		if err = store.Save(s); err != nil {
			t.Fatal(err)
		}
		store.Close()
		return c
	}
	movedHead := func(t *testing.T) Config {
		c := publishableRunFixture(t)
		if err := os.WriteFile(filepath.Join(c.Root, "owner.md"), []byte("owner edit\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := gitText(c.Root, "add", "--", "owner.md"); err != nil {
			t.Fatal(err)
		}
		if _, err := gitText(c.Root, "commit", "-q", "-m", "owner edit"); err != nil {
			t.Fatal(err)
		}
		return c
	}
	missingIdentity := func(t *testing.T) Config {
		c := publishableRunFixture(t)
		if _, err := gitText(c.Root, "config", "--unset", "user.email"); err != nil {
			t.Fatal(err)
		}
		// The machine may carry a global identity; the guard reads merged
		// configuration, so isolate the merge to the repository alone.
		t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
		t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
		return c
	}
	for name, tc := range map[string]struct {
		setup func(*testing.T) Config
		guard string
	}{
		"staged index":        {staged, "index_clean"},
		"edited task file":    {dirtyTaskFile, "task_file"},
		"diverged working":    {divergedWork, "source_matches_accepted"},
		"unrelated untracked": {unrelatedFile, "no_unrelated_changes"},
		"publication pending": {publicationPending, "publication_pending"},
		"moved head":          {movedHead, "head_baseline"},
		"missing identity":    {missingIdentity, "commit_identity"},
	} {
		t.Run(name, func(t *testing.T) {
			c := tc.setup(t)
			doc, code, raw := publishPreviewDoc(t, c)
			if code != exitOK {
				t.Fatalf("preview exit %d: %s", code, raw)
			}
			if doc.Publishable || doc.Token != "" {
				t.Fatalf("refused preview must carry no token: %+v", doc)
			}
			verdict, ok := publishGuardVerdict(doc, tc.guard)
			if !ok || verdict != "fail" {
				t.Fatalf("guard %s verdict %q found %v", tc.guard, verdict, ok)
			}
		})
	}
}

// A repository without a saved run refuses the preview.
func TestPublishPreviewWithoutRun(t *testing.T) {
	repoConfig(t)
	code, _, stderr := publishRunCLI(t, "--preview")
	if code == exitOK {
		t.Fatal("preview without a saved run must refuse")
	}
	if !strings.Contains(stderr, "no saved run") {
		t.Fatalf("refusal: %s", stderr)
	}
}

// The application writes one project commit from the accepted tree, installs
// the index, and reconciles the saved baseline.
func TestPublishApplyCommitsAcceptedTree(t *testing.T) {
	c := publishableRunFixture(t)
	_, dir := projectPaths(c)
	before := snapshotProject(t, c, dir)
	token := publishTokenOf(t, c)
	oldHead, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := publishRunCLI(t, "--apply", "--token", token, "--message", "Publish accepted work", "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("apply exit %d: %s%s", code, out, stderr)
	}
	var doc publishAppliedDocument
	if err = json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("applied document: %v\n%s", err, out)
	}
	if doc.Kind != publishAppliedKind || doc.Duplicate {
		t.Fatalf("applied identity: %+v", doc)
	}
	newHead, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if newHead != doc.Branch.NewCommit || doc.Branch.OldCommit != oldHead {
		t.Fatalf("branch %s -> %s, document says %s -> %s", oldHead, newHead, doc.Branch.OldCommit, doc.Branch.NewCommit)
	}
	parent, err := gitText(c.Root, "rev-parse", "HEAD^")
	if err != nil {
		t.Fatal(err)
	}
	if parent != oldHead {
		t.Fatalf("the commit parents on %s, not the previous head %s", parent, oldHead)
	}
	// The accepted content is on the branch, modes included.
	lsTree, err := gitText(c.Root, "ls-tree", "HEAD", "--", "feature.txt", "feature-link")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lsTree, "feature.txt") || !strings.Contains(lsTree, "120000 blob") {
		t.Fatalf("accepted entries missing from the commit: %s", lsTree)
	}
	// The index installed: no staged or modified paths remain.
	status, err := gitText(c.Root, "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(status), "\n") {
		if line == "" {
			continue
		}
		// Untracked leftovers (the task file, the run base directory) are
		// the publication's preserved boundaries; nothing may sit modified
		// or staged after the index installed.
		if strings.HasPrefix(line, "??") {
			continue
		}
		t.Fatalf("unexpected worktree status after publish: %q", line)
	}
	// Working files were never touched.
	feature, err := os.ReadFile(filepath.Join(c.Root, "feature.txt"))
	if err != nil || string(feature) != "accepted feature\n" {
		t.Fatalf("feature file changed: %q %v", feature, err)
	}
	// The saved baseline reconciled to the new head.
	s := savedState(t, dir)
	if s.Head != newHead {
		t.Fatalf("saved head %s, branch head %s", s.Head, newHead)
	}
	if s.Published == "" || s.Tip == "" {
		t.Fatal("the application must not clear the run's accepted tree or tip")
	}
	// The journal closed and the event recorded.
	var j *publishJournal
	for _, candidate := range listPublishJournals(dir) {
		if candidate.ID == doc.RequestID {
			j = candidate
		}
	}
	if j == nil || j.State != publishDone {
		t.Fatalf("journal: %+v", j)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events, err := store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Kind == "publish" {
			found = true
		}
	}
	if !found {
		t.Fatal("no publish event was journaled")
	}
	// The only durable mutation is the intended one: the state row, the
	// event, the index, and the refs moved; the task file did not.
	after := snapshotProject(t, c, dir)
	if after.todo != before.todo {
		t.Fatal("the application rewrote the task file")
	}
}

// A stale token is refused without mutation.
func TestPublishApplyStaleTokenRefused(t *testing.T) {
	c := publishableRunFixture(t)
	token := publishTokenOf(t, c)
	// The working tree changed after the preview: the composition moved.
	if err := os.WriteFile(filepath.Join(c.Root, "feature.txt"), []byte("edited\n"), 0600); err != nil {
		t.Fatal(err)
	}
	head, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := publishRunCLI(t, "--apply", "--token", token, "--contract", "v2", "--json")
	if code == exitOK {
		t.Fatalf("stale apply must refuse: %s%s", out, stderr)
	}
	if !strings.Contains(out+stderr, "stale") && !strings.Contains(out+stderr, "not publishable") {
		t.Fatalf("refusal: %s%s", out, stderr)
	}
	now, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if now != head {
		t.Fatal("a refused application moved the branch")
	}
}

// A token from an earlier publication is stale once a later preview exists.
func TestPublishApplyOldTokenAfterPublish(t *testing.T) {
	c := publishableRunFixture(t)
	first := publishTokenOf(t, c)
	if code, out, stderr := publishRunCLI(t, "--apply", "--token", first, "--contract", "v2", "--json"); code != exitOK {
		t.Fatalf("first apply exit %d: %s%s", code, out, stderr)
	}
	head, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	publishTokenOf(t, c)
	code, out, stderr := publishRunCLI(t, "--apply", "--token", first, "--contract", "v2", "--json")
	if code == exitOK {
		t.Fatalf("the old token must be stale: %s%s", out, stderr)
	}
	if !strings.Contains(out+stderr, "stale") {
		t.Fatalf("refusal: %s%s", out, stderr)
	}
	now, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if now != head {
		t.Fatal("a stale application moved the branch")
	}
}

// A caller key answers from the journal that recorded it; the retry writes
// no second commit.
func TestPublishApplyIdempotencyKey(t *testing.T) {
	c := publishableRunFixture(t)
	token := publishTokenOf(t, c)
	code, out, stderr := publishRunCLI(t, "--apply", "--token", token, "--idempotency-key", "release-1", "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("first apply: %s%s", out, stderr)
	}
	var first publishAppliedDocument
	if err := json.Unmarshal([]byte(out), &first); err != nil {
		t.Fatal(err)
	}
	if first.Duplicate {
		t.Fatal("the first application is not a duplicate")
	}
	head, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr = publishRunCLI(t, "--apply", "--token", token, "--idempotency-key", "release-1", "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("replay apply: %s%s", out, stderr)
	}
	var replay publishAppliedDocument
	if err = json.Unmarshal([]byte(out), &replay); err != nil {
		t.Fatal(err)
	}
	if !replay.Duplicate || replay.Branch.NewCommit != first.Branch.NewCommit {
		t.Fatalf("replay: %+v", replay)
	}
	now, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if now != head {
		t.Fatal("the replay wrote a second commit")
	}
}

// A prepared journal left by a crash completes on the next application: the
// commit is built, the branch moves by compare-and-swap, and the index
// installs.
func TestPublishApplyResolvesPreparedJournal(t *testing.T) {
	c := publishableRunFixture(t)
	_, dir := projectPaths(c)
	doc, code, raw := publishPreviewDoc(t, c)
	if code != exitOK {
		t.Fatalf("preview: %s", raw)
	}
	// A crash between the journal write and the branch update.
	j := &publishJournal{
		ID: identifier("publish"), State: publishPrepared,
		Branch:         "refs/heads/" + doc.Branch.Name,
		OldCommit:      mustGitText(t, c.Root, "rev-parse", "HEAD"),
		SourceTree:     savedState(t, dir).Published,
		ProposedDigest: doc.Branch.ProposedTree,
		OldIndexDigest: publishLiveIndexDigest(t, c.Root),
		Token:          doc.Token, Message: publishDefaultMessage,
		RequestedAt: "2026-11-14T00:00:00Z",
	}
	if err := writePublishJournal(dir, j); err != nil {
		t.Fatal(err)
	}
	head, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := publishRunCLI(t, "--apply", "--token", doc.Token, "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("resolution exit %d: %s%s", code, out, stderr)
	}
	var applied publishAppliedDocument
	if err = json.Unmarshal([]byte(out), &applied); err != nil {
		t.Fatal(err)
	}
	if applied.Journal.State != publishDone {
		t.Fatalf("journal state: %+v", applied.Journal)
	}
	now, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if now == head {
		t.Fatal("the resolution did not move the branch")
	}
	if applied.Branch.OldCommit != head {
		t.Fatalf("the resolved commit parents on %s, the crash recorded %s", head, applied.Branch.OldCommit)
	}
}

// publishLiveIndexDigest digests the repository's staged index state.
func publishLiveIndexDigest(t *testing.T, root string) string {
	t.Helper()
	d, err := indexDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A committed journal left by a crash after the branch moved finishes the
// installation and reconciles the saved baseline.
func TestPublishApplyResolvesCommittedJournal(t *testing.T) {
	c := publishableRunFixture(t)
	_, dir := projectPaths(c)
	token := publishTokenOf(t, c)
	code, out, stderr := publishRunCLI(t, "--apply", "--token", token, "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("apply: %s%s", out, stderr)
	}
	var done publishAppliedDocument
	if err := json.Unmarshal([]byte(out), &done); err != nil {
		t.Fatal(err)
	}
	// Rewind the saved baseline and the journal to the crash point: the
	// branch moved, the index installed, the state did not reconcile.
	s := savedState(t, dir)
	s.Head = done.Branch.OldCommit
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	store.Close()
	journals := listPublishJournals(dir)
	var j *publishJournal
	for _, candidate := range journals {
		if candidate.ID == done.RequestID {
			j = candidate
		}
	}
	if j == nil {
		t.Fatal("the applied journal is missing")
	}
	j.State = publishCommitted
	if err = writePublishJournal(dir, j); err != nil {
		t.Fatal(err)
	}
	code, out, stderr = publishRunCLI(t, "--apply", "--token", token, "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("resolution exit %d: %s%s", code, out, stderr)
	}
	if s := savedState(t, dir); s.Head != done.Branch.NewCommit {
		t.Fatalf("reconciled head %s, want %s", s.Head, done.Branch.NewCommit)
	}
}

// A journal whose branch moved elsewhere stays a preserved conflict; the
// publication resets nothing.
func TestPublishApplyConflictPreservesBothSides(t *testing.T) {
	c := publishableRunFixture(t)
	_, dir := projectPaths(c)
	token := publishTokenOf(t, c)
	head, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	s := savedState(t, dir)
	j := &publishJournal{
		ID: identifier("publish"), State: publishPrepared,
		OldCommit: head, SourceTree: s.Published, Token: token,
		RequestedAt: "2026-11-14T00:00:00Z",
	}
	if err := writePublishJournal(dir, j); err != nil {
		t.Fatal(err)
	}
	// The project moved on without the publication.
	if err := os.WriteFile(filepath.Join(c.Root, "owner.md"), []byte("owner edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = gitText(c.Root, "add", "--", "owner.md"); err != nil {
		t.Fatal(err)
	}
	if _, err = gitText(c.Root, "commit", "-q", "-m", "owner edit"); err != nil {
		t.Fatal(err)
	}
	moved, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := publishRunCLI(t, "--apply", "--token", token, "--contract", "v2", "--json")
	combined := out + stderr
	if code == exitOK {
		t.Fatal("a conflicting journal must refuse")
	}
	if !strings.Contains(combined, "conflict") {
		t.Fatalf("refusal: %s", combined)
	}
	now, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if now != moved {
		t.Fatal("the conflict resolution moved or reset the branch")
	}
	var resolved *publishJournal
	for _, candidate := range listPublishJournals(dir) {
		if candidate.ID == j.ID {
			resolved = candidate
		}
	}
	if resolved == nil || resolved.State != publishConflict {
		t.Fatalf("journal: %+v", resolved)
	}
}

// Tracked control files keep their tracked place: the task file carries the
// accepted export bytes and the schema keeps the project's bytes.
func TestPublishPreservesTrackedControlFiles(t *testing.T) {
	c := repoConfig(t)
	for _, kv := range [][2]string{{"user.name", "Fixture Owner"}, {"user.email", "owner@example.test"}} {
		gitText(c.Root, "config", kv[0], kv[1])
	}
	rel, err := filepath.Rel(c.Root, c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	schemaRel := filepath.ToSlash(filepath.Join(filepath.Dir(rel), "to-do.schema.json"))
	b := Backlog{Version: 1, Sources: []string{}, Goal: "tracked control files", Tasks: []Task{
		{ID: "T001", Title: "accepted work", Priority: 1, Status: "doing"},
	}}
	if err = writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.Root, filepath.FromSlash(schemaRel)), []byte("{\"stale\":true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = gitText(c.Root, "add", "--", filepath.ToSlash(rel), schemaRel); err != nil {
		t.Fatal(err)
	}
	if _, err = gitText(c.Root, "commit", "-q", "-m", "seed with control files"); err != nil {
		t.Fatal(err)
	}
	// The fixture below seeds the run; reuse the publishable construction.
	c = publishableTrackedFixtureOf(t, c, b)
	token := publishTokenOf(t, c)
	accepted, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := publishRunCLI(t, "--apply", "--token", token, "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("apply: %s%s", out, stderr)
	}
	taskBlob, err := gitText(c.Root, "show", "HEAD:"+filepath.ToSlash(rel))
	if err != nil {
		t.Fatal(err)
	}
	// gitText trims the trailing newline the export carries.
	if taskBlob+"\n" != string(accepted) {
		t.Fatalf("the commit must carry the accepted task-file bytes: %q", taskBlob)
	}
	schemaBlob, err := gitText(c.Root, "show", "HEAD:"+schemaRel)
	if err != nil {
		t.Fatal(err)
	}
	if schemaBlob != "{\"stale\":true}" {
		t.Fatalf("the tracked schema bytes changed: %q", schemaBlob)
	}
}

// publishableTrackedFixtureOf saves a publishable run over a repository whose
// control files are tracked, mirroring publishableRunFixture.
func publishableTrackedFixtureOf(t *testing.T, c Config, b Backlog) Config {
	t.Helper()
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.Root, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tree, err := e.Repo.SnapshotFrom(c.Root, e.State.Published)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := e.Repo.Commit(tree, []string{e.State.Tip}, "Accept T001")
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Repo.Ref(e.State.Ref, commit); err != nil {
		t.Fatal(err)
	}
	task := e.State.Backlog.Task("T001")
	task.Status = "done"
	snapshot := *task
	e.State.Attempts = append(e.State.Attempts, &Attempt{
		ID: "task-accepted", Task: snapshot, Agent: "claude", Base: e.State.Tip,
		Directory: filepath.Join(e.Dir, "task-accepted"), Phase: "done", ResultCommit: commit, Worktrees: []string{},
	})
	e.State.Tip = commit
	e.State.Published = tree
	e.State.Finished = true
	e.State.Publication = nil
	if err = e.export(); err != nil {
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

// publishableRetainedFixture extends the publishable fixture with one
// blocked follow-up task whose attempt stays retained, so a resume runs the
// startup recovery path.
func publishableRetainedFixture(t *testing.T) Config {
	t.Helper()
	c := publishableRunFixture(t)
	_, dir := projectPaths(c)
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Load()
	if err != nil || s == nil {
		t.Fatal(err)
	}
	s.Backlog.Tasks = append(s.Backlog.Tasks, Task{ID: "T002", Title: "blocked follow-up", Priority: 1, Status: "blocked"})
	s.Attempts = append(s.Attempts, &Attempt{
		ID: "task-blocked", Task: s.Backlog.Tasks[1], Agent: "claude", Base: s.Tip,
		Directory: filepath.Join(dir, "task-blocked"), Phase: "blocked", Worktrees: []string{},
	})
	data, err := json.MarshalIndent(s.Backlog, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err = os.WriteFile(c.Todo, data, 0600); err != nil {
		t.Fatal(err)
	}
	s.ExportHash = digest(data)
	s.ExportSemantics = s.Backlog.semanticFingerprint()
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	return c
}

// The recovery preview names the checked publication when accepted content
// sits off the project branch, and the recovery application refuses the
// choice with the same pointer instead of applying a second transport.
func TestRecoveryPreviewNamesCommitPublished(t *testing.T) {
	publishableRunFixture(t)
	code, doc, raw := runPreviewCLI(t, "--contract", "v2", "--json")
	if code != exitOK || doc == nil {
		t.Fatalf("preview exit %d: %s", code, raw)
	}
	choice := doc.choice("commit-published")
	if choice.Name != "commit-published" || choice.Status != "available" {
		t.Fatalf("commit-published choice: %+v", choice)
	}
	if choice.Apply == nil || !strings.Contains(choice.Apply.Command, "looper publish") {
		t.Fatalf("the choice must point at the publication: %+v", choice.Apply)
	}
	code, out, _ := runApplyCLI(t, "commit-published", "--token", doc.Token.Value, "--contract", "v2", "--json")
	if code == exitOK {
		t.Fatal("the recovery transport applied a publication")
	}
	if !strings.Contains(out, "looper publish") {
		t.Fatalf("the refusal must name the publication: %s", out)
	}
}

// A resume after a crashed publication completes the journal before its
// guards run: the baseline reconciles and the run proceeds instead of
// retaining on a moved branch.
func TestStartupResolvesPendingPublishJournal(t *testing.T) {
	c := publishableRetainedFixture(t)
	_, dir := projectPaths(c)
	doc, code, raw := publishPreviewDoc(t, c)
	if code != exitOK {
		t.Fatalf("preview: %s", raw)
	}
	applyCode, applyOut, applyErr := publishRunCLI(t, "--apply", "--token", doc.Token, "--contract", "v2", "--json")
	if applyCode != exitOK {
		t.Fatalf("apply: %s%s", applyOut, applyErr)
	}
	var done publishAppliedDocument
	if err := json.Unmarshal([]byte(applyOut), &done); err != nil {
		t.Fatal(err)
	}
	// Rewind the saved baseline and the journal to the crash point.
	s := savedState(t, dir)
	s.Head = done.Branch.OldCommit
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	store.Close()
	for _, j := range listPublishJournals(dir) {
		if j.ID == done.RequestID {
			j.State = publishCommitted
			if err = writePublishJournal(dir, j); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The next start resolves the journal and accepts the published head.
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { e.Store.Close(); lock.Close() }()
	if err = e.initialize(context.Background()); err != nil {
		t.Fatalf("startup after a crashed publication: %v", err)
	}
	if e.State.Head != done.Branch.NewCommit {
		t.Fatalf("the reconciled baseline is %s, want %s", e.State.Head, done.Branch.NewCommit)
	}
	for _, j := range listPublishJournals(dir) {
		if j.ID == done.RequestID && j.State != publishDone {
			t.Fatalf("the startup left the journal unresolved: %s", j.State)
		}
	}
}
