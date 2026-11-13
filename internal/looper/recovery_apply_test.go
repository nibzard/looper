package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/looper/internal/ui"
)

// The AX-017 acceptance fixtures. Every fixture drives the public CLI: an
// application must answer through `looper recovery --apply`, refuse a stale
// preview without mutation, preserve concurrent edits, leave a recoverable
// outcome after a crash, replay retained results without repeating
// implementation, and report a concrete limitation for every unsupported
// situation.

// runApplyCLI drives one recovery application through Main and returns the
// exit code with the combined output.
func runApplyCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := Main(append([]string{"recovery", "--apply"}, args...), &out, &stderr)
	return code, out.String(), stderr.String()
}

// previewToken runs the read-only preview against a stopped run and returns
// its document.
func previewToken(t *testing.T) *recoveryPreviewDocument {
	t.Helper()
	_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
	if doc == nil || doc.Token.Value == "" {
		t.Fatal("the preview carries no token")
	}
	return doc
}

// editTaskFileExternally replaces the task file with one real edit: a task
// title the saved run never accepted.
func editTaskFileExternally(t *testing.T, c Config, title string) []byte {
	t.Helper()
	b := Backlog{Version: 1, Sources: []string{}, Goal: "preview fixture", Tasks: []Task{
		{ID: "T001", Title: title, Priority: 1, Status: "doing"},
	}}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err = os.WriteFile(c.Todo, data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

// savedState reopens the stopped run's saved state.
func savedState(t *testing.T, dir string) *State {
	t.Helper()
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s, err := store.Load()
	if err != nil || s == nil {
		t.Fatalf("saved state: %v %v", s, err)
	}
	return s
}

// A restore application returns the accepted bytes, records the applied
// receipt, and journals the outcome — all under the repository lock.
func TestRecoveryApplyRestoresTaskFile(t *testing.T) {
	c := retainedRunFixture(t)
	accepted, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	edited := editTaskFileExternally(t, c, "edited outside Looper")
	doc := previewToken(t)
	restore := doc.choice("restore-task-file")
	if restore.Status != "available" || restore.Apply.Mode != "command" {
		t.Fatalf("restore choice: %+v", restore)
	}
	code, out, stderr := runApplyCLI(t, "restore-task-file", "--token", doc.Token.Value, "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("apply exit %d: %s%s", code, out, stderr)
	}
	restored, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	if digest(restored) != digest(accepted) || digest(restored) == digest(edited) {
		t.Fatal("the task file did not return to the accepted bytes")
	}
	// The receipt is queryable and the outcome journaled.
	_, dir := projectPaths(c)
	s := savedState(t, dir)
	var rec *RequestRecord
	for _, r := range s.Requests {
		if r != nil && r.Type == PublicRequestRecovery {
			rec = r
		}
	}
	if rec == nil || rec.State != RequestApplied || rec.Summary == "" {
		t.Fatalf("applied receipt: %+v", rec)
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
		if ev.Kind == "recovery-applied" {
			found = true
			var payload map[string]any
			if err = json.Unmarshal(ev.Data, &payload); err != nil || payload["request_id"] != rec.ID {
				t.Fatalf("applied event identity: %s", ev.Data)
			}
		}
	}
	if !found {
		t.Fatal("no recovery-applied event")
	}
	// The retained attempt is untouched.
	if s.Attempts[0].Phase != "running" || s.Attempts[0].ResultCommit != "" {
		t.Fatalf("retained attempt changed: %+v", s.Attempts[0])
	}
	// The applied outcome reports as one request record document.
	if !strings.Contains(out, `"applied"`) {
		t.Fatalf("the applied record is not reported: %s", out)
	}
}

// A stale preview rejects without mutation: nothing the caller previewed
// survives into the application, so nothing may change.
func TestRecoveryApplyStalePreviewRefusesWithoutMutation(t *testing.T) {
	c := retainedRunFixture(t)
	editTaskFileExternally(t, c, "first edit")
	doc := previewToken(t)
	// The run moves after the preview: a second, different edit lands.
	second := editTaskFileExternally(t, c, "second edit")
	code, out, _ := runApplyCLI(t, "restore-task-file", "--token", doc.Token.Value, "--contract", "v2", "--json")
	if code == exitOK {
		t.Fatal("a stale preview applied")
	}
	if !strings.Contains(out, "stale") && !strings.Contains(out, "conflict") {
		t.Fatalf("the refusal must name the stale preview: %s", out)
	}
	kept, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	if digest(kept) != digest(second) {
		t.Fatal("the refusal mutated the concurrent edit")
	}
	_, dir := projectPaths(c)
	s := savedState(t, dir)
	var rec *RequestRecord
	for _, r := range s.Requests {
		if r != nil && r.Type == PublicRequestRecovery {
			rec = r
		}
	}
	if rec == nil || rec.State != RequestRejected || rec.Error == "" {
		t.Fatalf("rejected receipt: %+v", rec)
	}
}

// Concurrent edits are preserved: an edit that lands between the preview and
// the application keeps its bytes, whatever choice the preview enumerated.
func TestRecoveryApplyPreservesConcurrentEdits(t *testing.T) {
	for _, choice := range []string{"restore-task-file", "import-backlog"} {
		t.Run(choice, func(t *testing.T) {
			c := retainedRunFixture(t)
			editTaskFileExternally(t, c, "previewed edit")
			doc := previewToken(t)
			concurrent := editTaskFileExternally(t, c, "concurrent edit")
			code, _, _ := runApplyCLI(t, choice, "--token", doc.Token.Value, "--contract", "v2", "--json")
			if code == exitOK {
				t.Fatalf("%s applied over a concurrent edit", choice)
			}
			kept, err := os.ReadFile(c.Todo)
			if err != nil {
				t.Fatal(err)
			}
			if digest(kept) != digest(concurrent) {
				t.Fatalf("%s overwrote the concurrent edit", choice)
			}
		})
	}
}

// A replay-safe retained edit imports through the T058 path: the application
// applies the same import the next startup would run, and the resume after it
// replays the retained result without repeating implementation.
func TestRecoveryApplyImportBacklogReplaysRetainedResults(t *testing.T) {
	c, dir, commit := replayableRetainedFixture(t)
	doc := previewToken(t)
	imp := doc.choice("import-backlog")
	if imp.Status != "available" || imp.Apply.Mode != "command" {
		t.Fatalf("import choice: %+v", imp)
	}
	before := savedState(t, dir)
	code, out, stderr := runApplyCLI(t, "import-backlog", "--token", doc.Token.Value, "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("apply exit %d: %s%s", code, out, stderr)
	}
	s := savedState(t, dir)
	data, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	if s.ExportHash != digest(data) || s.ExportHash == before.ExportHash {
		t.Fatalf("the edited bytes are not the accepted export: %s vs %s", s.ExportHash, digest(data))
	}
	if s.Tip == before.Tip || s.Head != before.Head {
		t.Fatalf("the import did not move the baseline: tip %s -> %s", before.Tip, s.Tip)
	}
	// The retained result is preserved for replay, not discarded.
	if s.Attempts[0].ResultCommit != commit || s.Attempts[0].Task.ID != "T001" {
		t.Fatalf("retained result changed: %+v", s.Attempts[0])
	}
	// The resume replays: startup accepts the imported bytes and re-prepares
	// the retained result instead of demanding a restore or relaunching work.
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close(); _ = e.Store.Close() }()
	// The worker must never run: any agent invocation repeats implementation.
	stub := filepath.Join(c.BaseDir, "fail-if-called.sh")
	if err = os.WriteFile(stub, []byte("#!/bin/sh\necho \"agent invoked\" >&2\nexit 9\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_BIN", stub)
	if err = e.initialize(context.Background()); err != nil {
		t.Fatalf("resume after the applied import: %v", err)
	}
	if digestFile(c.Todo) != e.State.ExportHash {
		t.Fatal("the resume did not keep the imported bytes")
	}
	a := e.attempt(e.State.Attempts[0].ID)
	if a.Phase != "ready" || a.ResultCommit != commit {
		t.Fatalf("the retained result does not replay: %+v", a)
	}
	if e.State.Attempts[0].Phase == "resume" {
		t.Fatal("a replayable result must not become restartable work")
	}
}

// replayableRetainedFixture saves a stopped run whose one retained attempt
// holds a verified result commit, then edits the task file for real: every
// retained result replays, so the edit imports (T058).
func replayableRetainedFixture(t *testing.T) (Config, string, string) {
	t.Helper()
	c := retainedRunFixture(t)
	_, dir := projectPaths(c)
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	e.State = s
	work, err := e.Repo.Workspace(identifier("task"), s.Tip, s.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Repo.Remove(work)
	if err = os.WriteFile(filepath.Join(work, "impl.txt"), []byte("implementation\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = gitText(work, "add", "-f", "--", "impl.txt"); err != nil {
		t.Fatal(err)
	}
	tree, err := e.Repo.Snapshot(work)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := e.Repo.Commit(tree, []string{s.Tip}, "Complete T001")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range s.Attempts {
		a.ResultCommit = commit
		a.Phase = "verifying"
	}
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	if err = e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	// One real edit the import applies: a new task beside the retained one.
	b := Backlog{Version: 1, Sources: []string{}, Goal: "preview fixture", Tasks: []Task{
		{ID: "T001", Title: "retained work", Priority: 1, Status: "doing"},
		{ID: "T002", Title: "added outside Looper", Priority: 1, Status: "todo"},
	}}
	if err = writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	return c, dir, commit
}

// An equivalent formatting edit applies through the checked application: the
// observed bytes become the accepted export and no content moves.
func TestRecoveryApplyAdoptsEquivalentFormatting(t *testing.T) {
	c := retainedRunFixture(t)
	compact, err := json.Marshal(Backlog{Version: 1, Sources: []string{}, Goal: "preview fixture", Tasks: []Task{
		{ID: "T001", Title: "retained work", Priority: 1, Status: "doing"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	edited := append(compact, '\n')
	if err = os.WriteFile(c.Todo, edited, 0600); err != nil {
		t.Fatal(err)
	}
	doc := previewToken(t)
	if imp := doc.choice("import-backlog"); imp.Status != "available" {
		t.Fatalf("equivalent formatting must apply: %+v", imp)
	}
	code, out, stderr := runApplyCLI(t, "import-backlog", "--token", doc.Token.Value, "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("apply exit %d: %s%s", code, out, stderr)
	}
	_, dir := projectPaths(c)
	s := savedState(t, dir)
	if s.ExportHash != digest(edited) {
		t.Fatalf("the equivalent bytes were not adopted: %s", s.ExportHash)
	}
	if kept, _ := os.ReadFile(c.Todo); digest(kept) != digest(edited) {
		t.Fatal("the operator's formatting did not stay on the file")
	}
}

// Unsupported situations retain work and report a concrete limitation: the
// force discard, the two resume-owned choices, an unknown choice, a
// non-replayable edit, and a restore without a byte-exact source.
func TestRecoveryApplyUnsupportedChoicesRetainWork(t *testing.T) {
	t.Run("discard-retained", func(t *testing.T) {
		c := retainedRunFixture(t)
		editTaskFileExternally(t, c, "edited outside Looper")
		doc := previewToken(t)
		if d := doc.choice("discard-retained"); d.Status != "unsupported" || d.Apply.Mode != "none" {
			t.Fatalf("discard choice: %+v", d)
		}
		code, out, _ := runApplyCLI(t, "discard-retained", "--token", doc.Token.Value, "--contract", "v2", "--json")
		if code == exitOK {
			t.Fatal("a force discard applied")
		}
		if !strings.Contains(out, "no supported discard") {
			t.Fatalf("the limitation is not concrete: %s", out)
		}
		// The refused choice keeps a queryable receipt: the ack alone is
		// swept transport, the record is the durable answer.
		_, dir := projectPaths(c)
		s := savedState(t, dir)
		var rec *RequestRecord
		for _, r := range s.Requests {
			if r != nil && r.Type == PublicRequestRecovery {
				rec = r
			}
		}
		if rec == nil || rec.State != RequestRejected || rec.Error == "" {
			t.Fatalf("the refused choice lost its receipt: %+v", rec)
		}
	})
	t.Run("resume and publish", func(t *testing.T) {
		c := retainedRunFixture(t)
		editTaskFileExternally(t, c, "edited outside Looper")
		doc := previewToken(t)
		for _, choice := range []string{"resume", "publish-retained"} {
			code, out, _ := runApplyCLI(t, choice, "--token", doc.Token.Value, "--contract", "v2", "--json")
			if code == exitOK {
				t.Fatalf("%s applied through the request protocol", choice)
			}
			if !strings.Contains(out, "looper") {
				t.Fatalf("%s must name its supported route: %s", choice, out)
			}
		}
	})
	t.Run("unknown choice", func(t *testing.T) {
		retainedRunFixture(t)
		doc := previewToken(t)
		code, out, _ := runApplyCLI(t, "unknown-choice", "--token", doc.Token.Value, "--contract", "v2", "--json")
		if code == exitOK {
			t.Fatal("an unknown choice applied")
		}
		if !strings.Contains(out, "unknown recovery choice") {
			t.Fatalf("the refusal must name the choice: %s", out)
		}
	})
	t.Run("non-replayable edit", func(t *testing.T) {
		// The retained attempt holds no result and no receipt, so an import
		// would rerun implementation against the moved baseline.
		c := retainedRunFixture(t)
		editTaskFileExternally(t, c, "edited outside Looper")
		doc := previewToken(t)
		imp := doc.choice("import-backlog")
		if imp.Status != "blocked" {
			t.Fatalf("the import must be blocked: %+v", imp)
		}
		if len(imp.MissingEvidence) == 0 || !strings.Contains(imp.MissingEvidence[0], "replayable") {
			t.Fatalf("the limitation is not named: %+v", imp)
		}
		code, out, _ := runApplyCLI(t, "import-backlog", "--token", doc.Token.Value, "--contract", "v2", "--json")
		if code == exitOK {
			t.Fatal("a non-replayable edit imported")
		}
		if !strings.Contains(out, "replayable") {
			t.Fatalf("the refusal must name the replay limitation: %s", out)
		}
		if kept, _ := os.ReadFile(c.Todo); !strings.Contains(string(kept), "edited outside Looper") {
			t.Fatal("the refused import discarded the edit")
		}
	})
	t.Run("edit changing a retained contract", func(t *testing.T) {
		// Every result replays, but the edit retitles the retained task
		// itself: the next startup would strand it at the contract guard, so
		// the import refuses and the preview reports the same limitation.
		c, dir, _ := replayableRetainedFixture(t)
		_ = dir
		b := Backlog{Version: 1, Sources: []string{}, Goal: "preview fixture", Tasks: []Task{
			{ID: "T001", Title: "retitled outside Looper", Priority: 1, Status: "doing"},
			{ID: "T002", Title: "added outside Looper", Priority: 1, Status: "todo"},
		}}
		if err := writeJSON(c.Todo, b); err != nil {
			t.Fatal(err)
		}
		doc := previewToken(t)
		imp := doc.choice("import-backlog")
		if imp.Status != "blocked" {
			t.Fatalf("a contract-breaking import must stay blocked: %+v", imp)
		}
		if len(imp.BlockedBy) == 0 || !strings.Contains(imp.BlockedBy[0], "T001") {
			t.Fatalf("the blocked reason must name the retained task: %+v", imp)
		}
		code, out, _ := runApplyCLI(t, "import-backlog", "--token", doc.Token.Value, "--contract", "v2", "--json")
		if code == exitOK {
			t.Fatal("a contract-breaking edit imported")
		}
		if !strings.Contains(out, "contract") {
			t.Fatalf("the refusal must name the contract limitation: %s", out)
		}
		if kept, _ := os.ReadFile(c.Todo); !strings.Contains(string(kept), "retitled outside Looper") {
			t.Fatal("the refused import discarded the edit")
		}
	})
	t.Run("restore without a byte-exact source", func(t *testing.T) {
		c := retainedRunFixture(t)
		editTaskFileExternally(t, c, "edited outside Looper")
		_, dir := projectPaths(c)
		// A pending export mutated the saved backlog, so its projection is a
		// different digest; without the retained copy no restore source holds
		// the accepted bytes.
		store, err := OpenStore(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		s, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		s.ExportPending = true
		s.Backlog.Tasks[0].Title = "mutated by the pending export"
		if err = store.Save(s); err != nil {
			t.Fatal(err)
		}
		store.Close()
		if err = os.Remove(filepath.Join(dir, "exports", "accepted.json")); err != nil {
			t.Fatal(err)
		}
		doc := previewToken(t)
		restore := doc.choice("restore-task-file")
		if restore.Status != "blocked" || len(restore.MissingEvidence) == 0 {
			t.Fatalf("a sourceless restore must stay blocked: %+v", restore)
		}
		code, out, _ := runApplyCLI(t, "restore-task-file", "--token", doc.Token.Value, "--contract", "v2", "--json")
		if code == exitOK {
			t.Fatal("a sourceless restore applied")
		}
		if !strings.Contains(out, "no restore source") {
			t.Fatalf("the limitation is not concrete: %s", out)
		}
		if kept, _ := os.ReadFile(c.Todo); !strings.Contains(string(kept), "edited outside Looper") {
			t.Fatal("the refused restore discarded the edit")
		}
	})
}

// A live controller receives the application through the common request
// protocol: the immediate receipt returns at once, the controller applies the
// same checked application at its boundary, and an older controller without
// the capability refuses before any envelope is published.
func TestRecoveryApplyLiveControllerRequestPath(t *testing.T) {
	t.Run("immediate receipt then applied claim", func(t *testing.T) {
		c, dir := liveRecoveryProject(t)
		b := testBacklog()
		if err := writeJSON(c.Todo, b); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(c.Todo)
		if err != nil {
			t.Fatal(err)
		}
		store, err := OpenStore(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		state := &State{Version: 2, Root: c.Root, Todo: c.Todo, Backlog: b, ExportHash: digest(data), Attempts: []*Attempt{}}
		if err = store.Save(state); err != nil {
			t.Fatal(err)
		}
		// One external edit the restore discards.
		edited := clone(b)
		edited.Tasks[0].Title = "edited outside Looper"
		if err = writeJSON(c.Todo, edited); err != nil {
			t.Fatal(err)
		}
		// The preview the caller holds.
		_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
		var out, stderr strings.Builder
		cc := v2Config(c, "recovery", true)
		cc.RecoveryChoice, cc.RecoveryToken = "restore-task-file", doc.Token.Value
		code := runRecoveryApply(cc, &out, &stderr)
		if code != exitOK {
			t.Fatalf("submit exit %d: %s%s", code, out.String(), stderr.String())
		}
		receipt := decodeReceipt(t, out.String(), stderr.String())
		if receipt.Request.Type != PublicRequestRecovery || receipt.Request.State != RequestSubmitted {
			t.Fatalf("receipt: %+v", receipt.Request)
		}
		// The controller claims and applies at its boundary.
		e := &Engine{Dir: dir, Output: io.Discard, Store: store, State: state, Config: c,
			controller: &ControllerInfo{Version: 1, Session: "run-live", PID: os.Getpid()},
			acks:       map[string]SettingsAck{}, viewNotify: make(chan ui.ViewSnapshot, 1)}
		e.processRecoveryRequests()
		if e.err != nil {
			t.Fatalf("claim failed: %v", e.err)
		}
		var ack SettingsAck
		if err = readJSON(filepath.Join(ackDir(dir), receipt.Request.ID+".json"), &ack); err != nil {
			t.Fatal(err)
		}
		if ack.Status != AckApplied {
			t.Fatalf("claim ack: %+v", ack)
		}
		if digestFile(c.Todo) != digest(data) {
			t.Fatal("the claimed application did not restore the accepted bytes")
		}
		// The decided record is queryable through the public route.
		queryCode, qdoc, _ := runRequestsCLI(t, receipt.Request.ID, "--contract", "v2", "--json")
		if queryCode != exitOK || qdoc.Request.State != RequestApplied {
			t.Fatalf("record after the claim: %d %+v", queryCode, qdoc.Request)
		}
	})
	t.Run("in-flight import stays with the controller boundary", func(t *testing.T) {
		c, dir := liveRecoveryProject(t)
		if err := writeJSON(c.Todo, testBacklog()); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(c.Todo)
		store, err := OpenStore(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		attempt := &Attempt{ID: "task-live", Task: testBacklog().Tasks[0], Phase: "running"}
		state := &State{Version: 2, Root: c.Root, Todo: c.Todo, Backlog: testBacklog(), ExportHash: digest(data), Attempts: []*Attempt{attempt}}
		if err = store.Save(state); err != nil {
			t.Fatal(err)
		}
		edited := clone(state.Backlog)
		edited.Tasks[0].Title = "edited outside Looper"
		if err = writeJSON(c.Todo, edited); err != nil {
			t.Fatal(err)
		}
		_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
		e := &Engine{Dir: dir, Output: io.Discard, Store: store, State: state, Config: c,
			controller: &ControllerInfo{Version: 1, Session: "run-live", PID: os.Getpid()},
			acks:       map[string]SettingsAck{}, viewNotify: make(chan ui.ViewSnapshot, 1)}
		req := &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("recovery"), Type: RequestTypeRecovery,
			Session: "run-live", RecoveryChoice: RecoveryChoiceImportBacklog, RecoveryToken: doc.Token.Value}
		ack := e.applyRecoveryRequest(req)
		if ack.Status != AckConflict || !strings.Contains(ack.Message, "in flight") {
			t.Fatalf("the in-flight guard must own the import: %+v", ack)
		}
		if digestFile(c.Todo) != digestFile(c.Todo) || state.ExportHash != digest(data) {
			t.Fatal("the refusal mutated the run")
		}
		if kept, _ := os.ReadFile(c.Todo); !strings.Contains(string(kept), "edited outside Looper") {
			t.Fatal("the refusal discarded the edit")
		}
	})
	t.Run("older controller refuses before publication", func(t *testing.T) {
		c, dir := liveRecoveryProject(t)
		if err := writeJSON(c.Todo, testBacklog()); err != nil {
			t.Fatal(err)
		}
		seedReceiptState(t, dir, func(s *State) {
			s.Root, s.Todo = c.Root, c.Todo
			data, _ := os.ReadFile(c.Todo)
			s.ExportHash = digest(data)
		})
		capabilities := newControllerCapabilities()
		capabilities.RequestTypes = []string{PublicRequestAddTask, PublicRequestSettings}
		writeControllerMetadata(t, dir, "run-old", capabilities)
		_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
		var out, stderr strings.Builder
		cc := v2Config(c, "recovery", true)
		cc.RecoveryChoice, cc.RecoveryToken = "restore-task-file", doc.Token.Value
		code := runRecoveryApply(cc, &out, &stderr)
		if code == exitOK {
			t.Fatal("an older controller accepted a recovery request")
		}
		if !strings.Contains(out.String()+stderr.String(), "does not accept") {
			t.Fatalf("the incompatibility is not specific: %s%s", out.String(), stderr.String())
		}
		if entries, _ := os.ReadDir(requestDir(dir)); len(entries) != 0 {
			t.Fatalf("no envelope may be queued for an older controller: %v", entries)
		}
	})
	t.Run("session change refuses without replay", func(t *testing.T) {
		c, dir := liveRecoveryProject(t)
		b := testBacklog()
		if err := writeJSON(c.Todo, b); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(c.Todo)
		store, err := OpenStore(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		state := &State{Version: 2, Root: c.Root, Todo: c.Todo, Backlog: b, ExportHash: digest(data), Attempts: []*Attempt{}}
		if err = store.Save(state); err != nil {
			t.Fatal(err)
		}
		_, doc, _ := runPreviewCLI(t, "--contract", "v2", "--json")
		// The envelope targeted the previous controller and was published
		// while it still owned the repository; the replacement owns it now.
		req := SettingsRequest{Version: SettingsRequestVersion, ID: identifier("recovery"),
			Type: RequestTypeRecovery, Session: "run-previous",
			RecoveryChoice: "import-backlog", RecoveryToken: doc.Token.Value,
			SubmitterPID: os.Getpid()}
		if err = writeJSON(filepath.Join(requestDir(dir), req.ID+".json"), req); err != nil {
			t.Fatal(err)
		}
		e := &Engine{Dir: dir, Output: io.Discard, Store: store, State: state, Config: c,
			controller: &ControllerInfo{Version: 1, Session: "run-live", PID: os.Getpid()},
			acks:       map[string]SettingsAck{}, viewNotify: make(chan ui.ViewSnapshot, 1)}
		e.processRecoveryRequests()
		var ack SettingsAck
		if err = readJSON(filepath.Join(ackDir(dir), req.ID+".json"), &ack); err != nil {
			t.Fatal(err)
		}
		if ack.Status != AckStale {
			t.Fatalf("a replaced session must refuse: %+v", ack)
		}
		if state.ExportHash != digest(data) {
			t.Fatal("a stale-session request mutated the run")
		}
	})
}

// recoveryProject builds a live-controller fixture whose command parses: a
// held repository lock, published controller metadata, and a parseable
// recovery invocation underneath.
func liveRecoveryProject(t *testing.T) (Config, string) {
	t.Helper()
	c := repoConfig(t, "recovery", "--apply", "restore-task-file", "--token", strings.Repeat("a", 64))
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeControllerMetadata(t, dir, "run-live", newControllerCapabilities())
	release := holdRepositoryLock(t, c)
	t.Cleanup(release)
	return c, dir
}

// A crash leaves a recoverable outcome. A restore that crashed between the
// file write and its receipt leaves the accepted bytes in place, so a retry
// refuses cleanly and the next preview reports the match; an envelope whose
// submitter died records a rejected receipt instead of a silent dead letter.
func TestRecoveryApplyCrashLeavesRecoverableOutcome(t *testing.T) {
	t.Run("restore interrupted before its receipt", func(t *testing.T) {
		c := retainedRunFixture(t)
		accepted, _ := os.ReadFile(c.Todo)
		editTaskFileExternally(t, c, "edited outside Looper")
		_, dir := projectPaths(c)
		// The crash: the file write completed, the receipt never did.
		store, err := OpenStore(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		s, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		e := &Engine{Config: c, Dir: dir, State: s, Store: store, Output: io.Discard, acks: map[string]SettingsAck{}}
		ev := &recoveryEvidence{todoDigest: digestFile(c.Todo), todoErr: nil}
		if _, err = e.applyRestoreTaskFile(ev); err != nil {
			t.Fatal(err)
		}
		store.Close()
		if digestFile(c.Todo) != digest(accepted) {
			t.Fatal("the interrupted restore did not leave the accepted bytes")
		}
		// The recovery: a fresh preview reports the match, and a retried
		// restore refuses without mutating anything.
		doc := previewToken(t)
		if tf := doc.Comparisons.TaskFile; tf.State != "match" {
			t.Fatalf("the preview must report the restored file: %+v", tf)
		}
		code, out, _ := runApplyCLI(t, "restore-task-file", "--token", doc.Token.Value, "--contract", "v2", "--json")
		if code == exitOK {
			t.Fatal("a restore with nothing to discard applied")
		}
		if !strings.Contains(out, "already matches") {
			t.Fatalf("the retry must explain itself: %s", out)
		}
	})
	t.Run("dead submitter envelope records a refusal", func(t *testing.T) {
		_, dir := liveRecoveryProject(t)
		dead := exec.Command("true")
		if err := dead.Run(); err != nil {
			t.Fatal(err)
		}
		req := SettingsRequest{Version: SettingsRequestVersion, ID: identifier("recovery"),
			Type: RequestTypeRecovery, Session: "run-live",
			RecoveryChoice: "restore-task-file", RecoveryToken: strings.Repeat("a", 64),
			SubmitterPID: dead.ProcessState.Pid()}
		if err := writeJSON(filepath.Join(requestDir(dir), req.ID+".json"), req); err != nil {
			t.Fatal(err)
		}
		if !submitterGone(&req) {
			t.Fatal("the dead submitter must be verifiably gone")
		}
		removed := clearSessionFiles(dir)
		if len(removed) != 1 || removed[0].id != req.ID {
			t.Fatalf("the sweep keeps a queryable outcome: %+v", removed)
		}
		if _, err := os.Stat(filepath.Join(requestDir(dir), req.ID+".json")); !os.IsNotExist(err) {
			t.Fatal("the swept envelope stayed")
		}
	})
}

// A caller key makes retries safe: the same key and token returns the
// original request, and a changed token under the same key is a conflict.
func TestRecoveryApplyIdempotencyKey(t *testing.T) {
	c := retainedRunFixture(t)
	editTaskFileExternally(t, c, "edited outside Looper")
	doc := previewToken(t)
	code, out, stderr := runApplyCLI(t, "restore-task-file", "--token", doc.Token.Value,
		"--idempotency-key", "retry-once", "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("apply exit %d: %s%s", code, out, stderr)
	}
	if !strings.Contains(out, `"applied"`) {
		t.Fatalf("the applied record is not reported: %s", out)
	}
	_, dir := projectPaths(c)
	before := savedState(t, dir)
	// The retry replays the original payload — the same token — after the
	// caller lost its first answer.
	code, out, stderr = runApplyCLI(t, "restore-task-file", "--token", doc.Token.Value,
		"--idempotency-key", "retry-once", "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("retry exit %d: %s%s", code, out, stderr)
	}
	after := savedState(t, dir)
	if len(after.Requests) != len(before.Requests)+1 {
		t.Fatalf("the retry recorded more than its mirror: %d -> %d", len(before.Requests), len(after.Requests))
	}
	// A changed payload under the same key is a conflict.
	code, out, _ = runApplyCLI(t, "restore-task-file", "--token", strings.Repeat("b", 64),
		"--idempotency-key", "retry-once", "--contract", "v2", "--json")
	if code == exitOK {
		t.Fatal("a changed payload under the same key applied")
	}
	if !strings.Contains(out, "idempotency") {
		t.Fatalf("the conflict must name the key: %s", out)
	}
}

// The application parses through the public surface: the operations are
// exclusive, an application requires its token, and both failures are usage
// errors that attempt nothing.
func TestRecoveryApplyOptionParsing(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"bare command", []string{"recovery"}, "two operations"},
		{"apply without token", []string{"recovery", "--apply", "restore-task-file"}, "token"},
		{"token without apply", []string{"recovery", "--token", strings.Repeat("a", 64)}, "--apply"},
		{"preview and apply", []string{"recovery", "--preview", "--apply", "restore-task-file", "--token", strings.Repeat("a", 64)}, "Select one operation"},
		{"malformed token", []string{"recovery", "--apply", "restore-task-file", "--token", "not-hex"}, "64-hex-digit"},
		{"two choices", []string{"recovery", "--apply", "restore-task-file", "--apply", "import-backlog"}, "one application choice"},
		{"preview with wait", []string{"recovery", "--preview", "--wait"}, "belong to looper recovery --apply"},
		{"preview with key", []string{"recovery", "--preview", "--idempotency-key", "k"}, "belong to looper recovery --apply"},
	}
	repoConfig(t, "run")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s parse: %v", tc.name, err)
			}
			var out, stderr bytes.Buffer
			if code := Main(tc.args, &out, &stderr); code == exitOK {
				t.Fatalf("%s ran", tc.name)
			}
		})
	}
	// The registered invocation parses.
	repoConfig(t, "recovery", "--apply", "restore-task-file", "--token", strings.Repeat("a", 64))
}

// A controller that never supports recovery envelopes leaves them refused at
// claim time, never silently queued: the queue-level compatibility check is
// the second gate after publication.
func TestRecoveryApplyClaimRefusesUnsupportedEnvelope(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(requestDir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	req := SettingsRequest{Version: SettingsRequestVersion, ID: "hand-written", Type: "recovery-unknown",
		RecoveryChoice: "restore-task-file", RecoveryToken: strings.Repeat("c", 64)}
	if err := writeJSON(filepath.Join(requestDir(dir), req.ID+".json"), req); err != nil {
		t.Fatal(err)
	}
	_, refusals, err := claimNextRequest(dir, RequestTypeRecovery)
	if err != nil {
		t.Fatal(err)
	}
	if len(refusals) != 1 || !strings.Contains(refusals[0].message, "unsupported request type") {
		t.Fatalf("the unknown envelope must be refused at claim time: %+v", refusals)
	}
}

// A stopped run with no saved state answers run_not_found, and a run owned by
// a live lock never applies outside the controller.
func TestRecoveryApplyRouting(t *testing.T) {
	t.Run("no saved run", func(t *testing.T) {
		repoConfig(t, "recovery", "--apply", "restore-task-file", "--token", strings.Repeat("a", 64))
		var out, stderr bytes.Buffer
		code := Main([]string{"recovery", "--apply", "restore-task-file", "--token", strings.Repeat("a", 64), "--contract", "v2", "--json"}, &out, &stderr)
		if code == exitOK {
			t.Fatal("an apply without a saved run succeeded")
		}
		if !strings.Contains(out.String(), "run_not_found") {
			t.Fatalf("the failure must be the structured run lookup: %s%s", out.String(), stderr.String())
		}
	})
	t.Run("ownership races the lock", func(t *testing.T) {
		c := retainedRunFixture(t)
		editTaskFileExternally(t, c, "edited outside Looper")
		doc := previewToken(t)
		_, dir := projectPaths(c)
		// A controller owns the repository but its metadata cannot be
		// verified: the request is refused before any envelope is published,
		// because no verifiable controller can claim it.
		repo, err := OpenRepository(c, dir)
		if err != nil {
			t.Fatal(err)
		}
		lock, err := lockRepository(repo.Common)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err = os.Remove(filepath.Join(dir, "controller.json")); err != nil {
			t.Fatal(err)
		}
		code, _, _ := runApplyCLI(t, "restore-task-file", "--token", doc.Token.Value, "--contract", "v2", "--json")
		if code == exitOK {
			t.Fatal("the application ran beside a live controller")
		}
		if kept, _ := os.ReadFile(c.Todo); !strings.Contains(string(kept), "edited outside Looper") {
			t.Fatal("the refused application mutated the file")
		}
		if entries, _ := os.ReadDir(requestDir(dir)); len(entries) != 0 {
			t.Fatalf("no envelope may wait for an unverifiable controller: %v", entries)
		}
	})
}

// The stopped-run application holds the repository lock for its whole
// duration: a controller start during the application cannot interleave.
func TestRecoveryApplyHoldsRepositoryLock(t *testing.T) {
	c := retainedRunFixture(t)
	editTaskFileExternally(t, c, "edited outside Looper")
	doc := previewToken(t)
	_, dir := projectPaths(c)
	repo, err := OpenRepository(c, dir)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockRepository(repo.Common)
	if err != nil {
		t.Fatal(err)
	}
	// The application cannot take the lock the controller holds.
	if _, err = lockRepository(repo.Common); err == nil {
		t.Fatal("the lock is not exclusive")
	}
	if !ownsRepository(repo.Common) {
		t.Fatal("ownership is not observable")
	}
	lock.Close()
	code, out, stderr := runApplyCLI(t, "restore-task-file", "--token", doc.Token.Value, "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("apply after release exit %d: %s%s", code, out, stderr)
	}
	if digestFile(c.Todo) != savedState(t, dir).ExportHash {
		t.Fatal("the restore did not return the accepted bytes")
	}
}
