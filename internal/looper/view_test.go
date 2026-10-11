package looper

import (
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/nibzard/looper/internal/ui"
)

func snapshotEngine(t *testing.T) *Engine {
	e := protocolEngine(t)
	e.State = &State{
		Finished: true,
		Backlog: Backlog{Version: 1, Sources: []string{}, Goal: "ship it", Tasks: []Task{
			{ID: "T1", Title: "one", Priority: 1, Status: "done"},
			{ID: "T2", Title: "two", Priority: 1, Status: "todo"},
			{ID: "T3", Title: "stuck", Priority: 1, Status: "blocked", Blockers: []string{"no credentials"}},
			{ID: "T4", Title: "unclaimed", Priority: 1, Status: "todo"},
		}},
		Attempts: []*Attempt{
			{ID: "a1", Task: Task{ID: "T2", Title: "two"}, Phase: "running", Agent: "codex", Base: "b1", Worktree: "/tmp/w1"},
			{ID: "a2", Task: Task{ID: "T1", Title: "one"}, Phase: "done"},
		},
	}
	return e
}

func TestSnapshotCopiesPresentationState(t *testing.T) {
	e := snapshotEngine(t)
	v := e.snapshot(false)
	if v.Session != e.controller.Session || v.Mode != ControllerModeForeground {
		t.Fatalf("identity missing: %+v", v)
	}
	if !v.Finished || v.Goal != "ship it" {
		t.Fatalf("state missing: %+v", v)
	}
	if v.Counters.Done != 1 || v.Counters.Total != 4 || v.Counters.Blocked != 1 {
		t.Fatalf("counters wrong: %+v", v.Counters)
	}
	if len(v.Attempts) != 1 || v.Attempts[0].ID != "a1" || v.Attempts[0].Phase != "running" {
		t.Fatalf("attempts must skip finished work: %+v", v.Attempts)
	}
	if v.Attempts[0].Worktree != "/tmp/w1" || v.Attempts[0].Base != "b1" {
		t.Fatalf("diff inputs missing: %+v", v.Attempts[0])
	}
	// Ready excludes work claimed by an active attempt: T2 runs as a1, so
	// only T4 is ready.
	if len(v.Ready) != 1 || v.Ready[0].ID != "T4" {
		t.Fatalf("ready wrong: %+v", v.Ready)
	}
	if len(v.Blocked) != 1 || v.Blocked[0].Reasons[0] != "no credentials" {
		t.Fatalf("blocked wrong: %+v", v.Blocked)
	}
	// Mutating the engine after the copy must not change the snapshot.
	e.State.Backlog.Tasks[3].Status = "done"
	if v.Ready[0].ID != "T4" {
		t.Fatal("snapshot aliases mutable state")
	}
}

func TestPublishViewCoalesces(t *testing.T) {
	e := snapshotEngine(t)
	e.publishView(false)
	e.publishView(false)
	if len(e.viewNotify) > 1 {
		t.Fatalf("bounded channel must coalesce: %d", len(e.viewNotify))
	}
	loaded := LoadView(e.Dir)
	if loaded == nil || loaded.Session != e.controller.Session {
		t.Fatal("view.json not published atomically with identity")
	}
	// A slow reader gets only the latest snapshot.
	first := <-e.viewNotify
	e.publishView(true)
	second := <-e.viewNotify
	if !first.Finished == second.Finished && first.Draining == second.Draining {
		t.Fatal("coalesced snapshot is not the latest")
	}
	if !second.Draining {
		t.Fatal("latest drain state lost")
	}
	clearView(e.Dir)
	if LoadView(e.Dir) != nil {
		t.Fatal("clearView left a stale view")
	}
}

func TestRecentEventsPaging(t *testing.T) {
	e := protocolEngine(t)
	for i := 0; i < 50; i++ {
		e.event("progress", itoa(i))
	}
	tail, err := e.Store.RecentEvents(-1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 10 || tail[0].Message != "40" || tail[9].Message != "49" {
		t.Fatalf("bounded tail wrong: %s..%s", tail[0].Message, tail[9].Message)
	}
	fresh, err := e.Store.RecentEvents(tail[4].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 5 || fresh[0].Message != "45" {
		t.Fatalf("paging wrong: %d from %s", len(fresh), fresh[0].Message)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestReadonlyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	if _, err := OpenStoreReadonly(path); err == nil {
		t.Fatal("readonly attach must fail without a database")
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Event("x", "y", nil)
	s.Close()
	ro, err := OpenStoreReadonly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err = ro.db.Exec("CREATE TABLE nope(x)"); err == nil {
		t.Fatal("readonly connection must not write")
	}
	events, err := ro.RecentEvents(-1, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("readonly reads broken: %v %d", err, len(events))
	}
}

func TestAttemptDiffBoundedAndSafe(t *testing.T) {
	e := snapshotEngine(t)
	e.publishView(false)
	// The fake worktree does not exist; removal is reported, not fatal.
	if _, err := attemptDiff(e.Dir, "a1"); err == nil {
		t.Fatal("removed worktree must be reported")
	}
	// Unknown attempts and missing views are errors too.
	if _, err := attemptDiff(e.Dir, "nobody"); err == nil {
		t.Fatal("unknown attempt accepted")
	}
	clearView(e.Dir)
	if _, err := attemptDiff(e.Dir, "a1"); err == nil {
		t.Fatal("missing view must fail")
	}
}

func TestObserverEvents(t *testing.T) {
	e := protocolEngine(t)
	e.event("start", "begin")
	obs, err := openObserver(e.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer obs.Close()
	events := obs.events(-1, 10)
	if len(events) != 1 || events[0].Message != "begin" {
		t.Fatalf("observer paging broken: %+v", events)
	}
	if fresh := obs.events(99, 10); len(fresh) != 0 {
		t.Fatalf("observer must return no events after the cursor: %d", len(fresh))
	}
}

// The dashboard package holds no engine imports; this compiles the shared
// contract types both sides use.
func TestViewContractTypes(t *testing.T) {
	var v ui.ViewSnapshot
	if err := json.Unmarshal([]byte(`{"version":1,"session":"s","counters":{"done":1,"total":2}}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.Version != ui.ViewVersion || v.Counters.Total != 2 {
		t.Fatalf("contract types broken: %+v", v)
	}
	if ui.PhaseLabel("verifying") != "checking" || ui.PhaseLabel("running") != "implementing" {
		t.Fatal("phase labels broken")
	}
	_ = io.Discard
}
