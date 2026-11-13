package looper

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The T105 fixtures. A plain mode=ro attach to a cleanly closed WAL database
// materializes state.db-wal and state.db-shm beside it on the first read, so
// observing a stopped run left two files behind. The observation commands now
// route through the same live-writer and leftover-log decision run discovery
// uses (openObservedStore): a cleanly stopped run opens immutable and creates
// nothing, while a live controller or a leftover write-ahead log keeps the
// through-WAL read.

// assertNoSidecars fails when a SQLite coordination file sits beside the
// state database, naming the run directory it appeared in.
func assertNoSidecars(t *testing.T, runtime string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(runtime, "state.db"+suffix)); err == nil {
			t.Fatalf("state.db%s was created in %s", suffix, runtime)
		}
	}
}

// assertRuntimeUntouched compares a hash snapshot of one runtime directory
// against an earlier snapshot from snapshotBase, so a test proves the read
// changed no record and created no file.
func assertRuntimeUntouched(t *testing.T, runtime string, before map[string]string, what string) {
	t.Helper()
	after := snapshotBase(t, runtime)
	if len(after) != len(before) {
		t.Fatalf("%s changed the runtime directory: %d files before, %d after", what, len(before), len(after))
	}
	for path, sum := range before {
		if after[path] != sum {
			t.Fatalf("%s mutated %s", what, path)
		}
	}
}

// TestObservationCommandsLeaveStoppedRunUntouched covers the acceptance: on a
// cleanly stopped run, every one-shot observation command answers from the
// saved records while creating no file and changing no record in the runtime
// directory. A followed stream is absent here on purpose: it holds its
// connection across the controllers that may start later.
func TestObservationCommandsLeaveStoppedRunUntouched(t *testing.T) {
	base := runsBase(t)
	c := runsProjectAt(t, filepath.Join(filepath.Dir(base), "observed"))
	logs := seedSavedRun(t, c, func(s *State) {
		s.Backlog = Backlog{Version: 1, Sources: []string{}, Goal: "observed", Tasks: []Task{
			{ID: "T001", Title: "one", Priority: 1, Status: "todo"},
		}}
	})
	runtime := filepath.Join(logs, "runtime")
	// The premise: a cleanly closed store leaves only state.db. Without it
	// the commands below could not prove they create nothing.
	assertNoSidecars(t, runtime)
	before := snapshotBase(t, runtime)
	for _, args := range [][]string{
		{"status", "--json"},
		{"status", "--contract", "v2", "--json"},
		{"inspect", "T001", "--json"},
		{"inspect", "T001", "--contract", "v2", "--json"},
		{"events", "--contract", "v2", "--json"},
		{"tail"},
		{"doctor", "--json"},
		{"doctor", "--contract", "v2", "--json"},
		{"requests", "--contract", "v2", "--json"},
		// A satisfied wait is one read of the same shape: it attaches the way
		// a followed stream does and must still leave the stopped run alone.
		{"wait", "--cursor", "0", "--timeout", "1s", "--contract", "v2", "--json"},
		{"recovery", "--preview", "--contract", "v2", "--json"},
	} {
		var out, stderr bytes.Buffer
		code := Main(append([]string(nil), args...), &out, &stderr)
		if code != 0 {
			t.Fatalf("%v exit %d: %s%s", args, code, out.String(), stderr.String())
		}
		assertRuntimeUntouched(t, runtime, before, args[0])
	}
	// Preflight may exit nonzero on environment findings; its document still
	// proves the saved run was read.
	var out, stderr bytes.Buffer
	code := Main([]string{"preflight", "--contract", "v2", "--json"}, &out, &stderr)
	var doc preflightDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc.Kind != preflightKind {
		t.Fatalf("preflight did not serve its document (exit %d): %s%s", code, out.String(), stderr.String())
	}
	assertRuntimeUntouched(t, runtime, before, "preflight")
	assertNoSidecars(t, runtime)
}

// TestObservedStoreReadsThroughLiveController pins the live-writer branch: a
// controller record naming a live process keeps the through-WAL attach, so
// the read answers from a database a writer may hold.
func TestObservedStoreReadsThroughLiveController(t *testing.T) {
	base := runsBase(t)
	c := runsProjectAt(t, filepath.Join(filepath.Dir(base), "live"))
	logs := seedSavedRun(t, c, nil)
	runtime := filepath.Join(logs, "runtime")
	seedControllerRecord(t, runtime, &ControllerInfo{
		Version: ControllerVersion, Session: "run-live0001", PID: os.Getpid(),
		Root: c.Root, Mode: ControllerModeDetached,
		Started: time.Now().UTC().Format(time.RFC3339Nano),
	})
	store, err := openObservedStore(runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snap, err := store.Snapshot()
	if err != nil {
		t.Fatalf("a live controller's run must stay readable: %v", err)
	}
	if snap.State == nil {
		t.Fatal("the live controller's saved state must surface")
	}
}

// TestObservedStoreReadsLeftoverLog pins the leftover-log branch: a
// write-ahead log a crashed controller left behind must be read through, not
// ignored by the immutable attach, so the last committed records surface.
func TestObservedStoreReadsLeftoverLog(t *testing.T) {
	base := runsBase(t)
	c := runsProjectAt(t, filepath.Join(filepath.Dir(base), "crashed"))
	_, runtime := projectPaths(c)
	if err := os.MkdirAll(runtime, 0700); err != nil {
		t.Fatal(err)
	}
	writer, err := OpenStore(filepath.Join(runtime, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	state := &State{
		Version: 1, Root: c.Root, Todo: c.Todo,
		Head: "0000000000000000000000000000000000000002", Branch: "main",
		Ref: "refs/looper/" + filepath.Base(c.Root),
		Backlog: Backlog{Version: 1, Sources: []string{}, Goal: "crashed", Tasks: []Task{
			{ID: "T001", Title: "one", Priority: 1, Status: "doing"},
		}},
	}
	if err = writer.Save(state); err != nil {
		t.Fatal(err)
	}
	// The writer stays open, exactly like a controller process that crashed:
	// no controller metadata exists and the log holds committed records.
	if _, err = os.Stat(filepath.Join(runtime, "state.db-wal")); err != nil {
		t.Fatalf("the held-open writer must leave a write-ahead log: %v", err)
	}
	store, err := openObservedStore(runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved == nil || saved.Backlog.Goal != "crashed" {
		t.Fatalf("the leftover log's committed state must surface: %+v", saved)
	}
}
