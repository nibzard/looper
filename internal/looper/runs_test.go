package looper

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/contractschema"
)

// The AX-025 acceptance fixtures. Every fixture drives the public CLI: the
// listing must answer through `looper runs` while changing nothing, and the
// missing, unreadable, stale, and large-count cases must stay bounded and
// actionable.

// runsSchema reads the published listing schema, anchored at this file's
// directory because fixtures chdir into temporary projects.
func runsSchema(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test file location is unavailable")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "contracts", "schemas", "v2", "runs.schema.json"))
	if err != nil {
		t.Skipf("runs schema is absent: %v", err)
	}
	return b
}

// runsBase prepares one shared base directory outside every project, the
// shape a real LOOPER_BASE_DIR has: projects point at it, it points at
// nothing inside them.
func runsBase(t *testing.T) string {
	t.Helper()
	clearSettingEnv(t)
	home := t.TempDir()
	base := filepath.Join(home, "base")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_BASE_DIR", base)
	return base
}

// chdirTest moves the test into a directory and restores the previous one.
func chdirTest(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

// runsProjectAt parses the runs command from one project root, so the
// derived runtime directory name matches what a controller would create.
func runsProjectAt(t *testing.T, root string) Config {
	t.Helper()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(root, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	chdirTest(t, root)
	c, err := Parse([]string{"runs"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// seedSavedRun writes one stopped run with real store records: a saved
// state, one controller session that began and drained cleanly, and the
// session build event. No controller metadata remains, exactly like a run
// whose controller shut down and removed its own record.
func seedSavedRun(t *testing.T, c Config, mutate func(*State)) string {
	t.Helper()
	logs, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := &State{
		Version: 1, Root: c.Root, Todo: c.Todo,
		Head: "0000000000000000000000000000000000000001", Branch: "main",
		Ref: "refs/looper/" + filepath.Base(c.Root),
	}
	if mutate != nil {
		mutate(s)
	}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	build := BuildInfo{Release: "0.9.1-seed", Revision: "seed1234abcd", Modified: false}
	info := &ControllerInfo{
		Version: ControllerVersion, Session: "run-seed" + filepath.Base(c.Root),
		PID: os.Getpid(), Root: c.Root, Mode: ControllerModeDetached,
		Started: time.Now().UTC().Format(time.RFC3339Nano), Build: &build,
	}
	if err = store.beginSession(info); err != nil {
		t.Fatal(err)
	}
	if err = store.endSession(sessionExit{Reason: "drained", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	return logs
}

// deadPID returns a process ID whose process has exited, so controller
// metadata naming it is stale.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// seedControllerRecord writes controller metadata for one runtime directory.
func seedControllerRecord(t *testing.T, dir string, info *ControllerInfo) {
	t.Helper()
	if err := writeJSON(controllerPath(dir), info); err != nil {
		t.Fatal(err)
	}
}

// runListingCLI drives `looper runs` through Main and returns the exit code
// with the decoded document. The JSON form must validate against the
// published schema, carry no human text, and stay within its byte budget.
func runListingCLI(t *testing.T, args ...string) (int, *runsDocument, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := Main(append([]string{"runs"}, args...), &out, &stderr)
	jsonForm := false
	for _, a := range args {
		if a == "--json" {
			jsonForm = true
		}
	}
	if !jsonForm {
		return code, nil, out.String()
	}
	if code != 0 {
		return code, nil, out.String()
	}
	if stderr.Len() > 0 {
		t.Fatalf("the listing JSON mixes human text into stdout: stderr=%q", stderr.String())
	}
	var doc runsDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not one listing document: %v\n%s", err, out.String())
	}
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	var first any
	if err := dec.Decode(&first); err != nil || dec.More() {
		t.Fatalf("stdout carries more than one JSON value: %v", err)
	}
	if err := contractschema.ValidateJSON(runsSchema(t), out.Bytes()); err != nil {
		t.Fatalf("the emitted listing violates its schema: %v\n%s", err, out.String())
	}
	if out.Len() > runsBytesBudget {
		t.Fatalf("listing document is %d bytes, budget %d", out.Len(), runsBytesBudget)
	}
	return code, &doc, out.String()
}

// snapshotBase hashes every file under the base directory, so a test proves
// the listing read the records without mutating one.
func snapshotBase(t *testing.T, base string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[path] = fmt.Sprintf("%d:%s", info.Size(), hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRunsListsSavedRunsAcrossProjects covers the core acceptance: several
// project roots share one configured base directory, and one bounded listing
// names each run's root, retained work, last outcome, controller freshness,
// build evidence, and the exact command that selects it — without changing
// any record.
func TestRunsListsSavedRunsAcrossProjects(t *testing.T) {
	base := runsBase(t)
	home := filepath.Dir(base)
	alpha := runsProjectAt(t, filepath.Join(home, "alpha"))
	seedSavedRun(t, alpha, func(s *State) {
		s.Backlog = Backlog{Version: 1, Sources: []string{}, Goal: "alpha", Tasks: []Task{
			{ID: "T001", Title: "held", Priority: 1, Status: "doing"},
			{ID: "T002", Title: "done", Priority: 1, Status: "done"},
		}}
		s.Attempts = append(s.Attempts, &Attempt{
			ID: "task-alpha", Task: s.Backlog.Tasks[0], Agent: "claude", Phase: "running",
		})
	})
	// A second project with a stale controller record: the process named by
	// its metadata is gone, so ownership freshness must say stale while the
	// session outcome stays the recorded one.
	beta := runsProjectAt(t, filepath.Join(home, "beta"))
	betaLogs := seedSavedRun(t, beta, nil)
	seedControllerRecord(t, filepath.Join(betaLogs, "runtime"), &ControllerInfo{
		Version: ControllerVersion, Session: "run-seedbeta", PID: deadPID(t),
		Root: beta.Root, Mode: ControllerModeForeground,
		Started: time.Now().UTC().Format(time.RFC3339Nano),
		Build:   &BuildInfo{Release: "0.8.3", Revision: "stale0000beef", Modified: true},
	})
	before := snapshotBase(t, base)
	chdirTest(t, alpha.Root)
	code, doc, raw := runListingCLI(t, "--contract", "v2", "--json")
	if code != 0 {
		t.Fatalf("runs exit %d: %s", code, raw)
	}
	if doc.RunTotal != 2 || len(doc.Runs) != 2 {
		t.Fatalf("two projects must list two runs: %+v", doc)
	}
	byName := map[string]runsEntry{}
	for _, r := range doc.Runs {
		byName[r.Name] = r
	}
	alphaEntry := byName[filepath.Base(projectName(t, alpha))]
	if !alphaEntry.CurrentProject {
		t.Errorf("the invocation's own project must be marked: %+v", alphaEntry)
	}
	if alphaEntry.Root != alpha.Root || alphaEntry.TodoPath != alpha.Todo {
		t.Errorf("the entry must name the saved root and task path: %+v", alphaEntry)
	}
	if alphaEntry.RootExists == nil || !*alphaEntry.RootExists {
		t.Errorf("the saved root exists and must say so: %+v", alphaEntry)
	}
	if !alphaEntry.Retained || alphaEntry.RetainedTaskTotal != 1 {
		t.Errorf("the retained attempt must surface: %+v", alphaEntry)
	}
	if len(alphaEntry.RetainedTasks) != 1 || alphaEntry.RetainedTasks[0] != "T001" {
		t.Errorf("the retained task identifiers must surface: %+v", alphaEntry.RetainedTasks)
	}
	if alphaEntry.TasksTotal != 2 || alphaEntry.TasksDone != 1 {
		t.Errorf("the backlog counts must surface: %+v", alphaEntry)
	}
	if alphaEntry.SessionsTotal != 1 {
		t.Errorf("the retained session count must surface: %+v", alphaEntry)
	}
	if alphaEntry.LastSession.Outcome != "stopped" || alphaEntry.LastSession.Reason != "drained" {
		t.Errorf("the last known outcome must surface: %+v", alphaEntry.LastSession)
	}
	if alphaEntry.LastSession.ExitCode == nil || *alphaEntry.LastSession.ExitCode != 0 {
		t.Errorf("the recorded exit code must surface: %+v", alphaEntry.LastSession)
	}
	if alphaEntry.Controller.State != "stopped" {
		t.Errorf("a clean shutdown leaves no metadata, so freshness must say stopped: %+v", alphaEntry.Controller)
	}
	if alphaEntry.Controller.Build == nil || alphaEntry.Controller.Build.Release != "0.9.1-seed" {
		t.Errorf("the session event must keep the build evidence: %+v", alphaEntry.Controller)
	}
	want := "cd " + alpha.Root + " && looper status --contract v2 --json"
	if len(alphaEntry.Select) == 0 || alphaEntry.Select[0] != want {
		t.Errorf("the selection hint must name the exact command: %+v", alphaEntry.Select)
	}
	if len(alphaEntry.Select) < 2 || !strings.Contains(alphaEntry.Select[1], "recovery --preview") {
		t.Errorf("a retained run must also hint the recovery preview: %+v", alphaEntry.Select)
	}
	betaEntry := byName[filepath.Base(projectName(t, beta))]
	if betaEntry.CurrentProject {
		t.Errorf("a foreign project must not be marked current: %+v", betaEntry)
	}
	if betaEntry.Controller.State != "stale" {
		t.Errorf("metadata naming a dead process must say stale: %+v", betaEntry.Controller)
	}
	if betaEntry.Controller.Build == nil || betaEntry.Controller.Build.Release != "0.8.3" || !betaEntry.Controller.Build.Modified {
		t.Errorf("the controller record's build must surface: %+v", betaEntry.Controller)
	}
	if betaEntry.Controller.AgeSeconds == nil {
		t.Errorf("freshness must carry the metadata age: %+v", betaEntry.Controller)
	}
	if after := snapshotBase(t, base); len(after) != len(before) {
		t.Fatalf("the listing changed the base directory: before %d files, after %d", len(before), len(after))
	} else {
		for path, sum := range before {
			if after[path] != sum {
				t.Fatalf("the listing mutated %s", path)
			}
		}
	}
}

// projectName returns the run directory name one config maps to.
func projectName(t *testing.T, c Config) string {
	t.Helper()
	logs, _ := projectPaths(c)
	return logs
}

// TestRunsOwningAndUnreadableControllerRecords covers the remaining
// freshness and unreadable-record cases: live metadata reports owning,
// unreadable metadata reports unknown with its reason, and an unreadable
// state database keeps the entry actionable with its runtime location.
func TestRunsOwningAndUnreadableControllerRecords(t *testing.T) {
	base := runsBase(t)
	home := filepath.Dir(base)
	live := runsProjectAt(t, filepath.Join(home, "live"))
	liveLogs := seedSavedRun(t, live, nil)
	seedControllerRecord(t, filepath.Join(liveLogs, "runtime"), &ControllerInfo{
		Version: ControllerVersion, Session: "run-live0001", PID: os.Getpid(),
		Root: live.Root, Mode: ControllerModeDetached,
		Started: time.Now().UTC().Format(time.RFC3339Nano),
		Build:   &BuildInfo{Release: "0.9.1-live", Revision: "live0000abcd", Modified: false},
	})
	// A run whose state database exists but is not a database, and whose
	// controller record is garbage: both stay evidence, never a failure.
	broken := filepath.Join(base, "broken-0badcafe", "runtime")
	if err := os.MkdirAll(broken, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "state.db"), []byte("not a database at all"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "controller.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	code, doc, raw := runListingCLI(t, "--contract", "v2", "--json")
	if code != 0 {
		t.Fatalf("runs exit %d: %s", code, raw)
	}
	if doc.RunTotal != 2 {
		t.Fatalf("the listing must keep both entries: %+v", doc)
	}
	for _, r := range doc.Runs {
		switch r.Name {
		case "live" + strings.TrimPrefix(projectName(t, live), "live"):
			if r.Controller.State != "owning" {
				t.Errorf("a live controller process must report owning: %+v", r.Controller)
			}
			if r.Controller.Session != "run-live0001" || r.Controller.Mode != "detached" {
				t.Errorf("the owning record must name its session and mode: %+v", r.Controller)
			}
		case "broken-0badcafe":
			if r.State != "unreadable" || r.StateError == "" {
				t.Errorf("an unreadable state database must keep its reason: %+v", r)
			}
			if r.Controller.State != "unknown" || r.Controller.Error == "" {
				t.Errorf("unreadable metadata must report unknown with its reason: %+v", r.Controller)
			}
			if r.LastSession.Outcome != "unknown" {
				t.Errorf("a run with no readable records keeps the outcome unknown: %+v", r.LastSession)
			}
			if len(r.Select) == 0 || !strings.Contains(r.Select[0], broken) {
				t.Errorf("an unreadable run must still hint where its records stay: %+v", r.Select)
			}
			if r.Root != "" {
				t.Errorf("a root the listing cannot read must stay absent, not guessed: %+v", r)
			}
		}
	}
}

// TestRunsMissingStateAndSkippedEntries covers a runtime directory with no
// state records and the entries the scan refuses to treat as runs: foreign
// directories, files, and Looper-named directories without a runtime child.
// None of them is read.
func TestRunsMissingStateAndSkippedEntries(t *testing.T) {
	base := runsBase(t)
	home := filepath.Dir(base)
	only := runsProjectAt(t, filepath.Join(home, "only"))
	// This project has a runtime directory but no saved state: a start that
	// never reached its first save.
	_, onlyRuntime := projectPaths(only)
	if err := os.MkdirAll(onlyRuntime, 0700); err != nil {
		t.Fatal(err)
	}
	for _, foreign := range []string{"agent-sessions", "random", "noluck-aaaabbbb"} {
		if err := os.MkdirAll(filepath.Join(base, foreign), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "notes.txt"), []byte("not a run\n"), 0600); err != nil {
		t.Fatal(err)
	}
	code, doc, raw := runListingCLI(t, "--contract", "v2", "--json")
	if code != 0 {
		t.Fatalf("runs exit %d: %s", code, raw)
	}
	if doc.RunTotal != 1 || len(doc.Runs) != 1 {
		t.Fatalf("only the runtime directory is a run: %+v", doc)
	}
	entry := doc.Runs[0]
	if entry.State != "missing" {
		t.Errorf("a runtime directory without state records must say missing: %+v", entry)
	}
	if !entry.CurrentProject {
		t.Errorf("the current project's own entry must be marked: %+v", entry)
	}
	if doc.Searched.EntriesSkipped != 4 {
		t.Errorf("three directories, one file, and one name-matching directory without runtime must be skipped: %+v", doc.Searched)
	}
	if !strings.Contains(doc.Searched.ScanNote, "private agent session directories are never scanned") {
		t.Errorf("the scan note must state the private-directory rule: %q", doc.Searched.ScanNote)
	}
	if doc.Project.CurrentRunFound || doc.Project.CurrentRunState != "missing" {
		t.Errorf("this project's own runtime exists without state, so discovery must say so: %+v", doc.Project)
	}
}

// TestRunsBoundsLargeCounts covers many runs under one base: the default
// page is bounded and reports what it omitted, --limit raises and lowers it,
// an oversized --limit is a usage error, and the listing stays within its
// byte budget and newest-first order.
func TestRunsBoundsLargeCounts(t *testing.T) {
	base := runsBase(t)
	home := filepath.Dir(base)
	parent := filepath.Join(home, "many")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	const total = 25
	start := time.Now().Add(-time.Hour)
	for i := 0; i < total; i++ {
		root := filepath.Join(parent, fmt.Sprintf("p%02d", i))
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		c := runsProjectAt(t, root)
		logs := seedSavedRun(t, c, nil)
		// Distinct, increasing activity stamps: the newest run must list
		// first, and the omitted tail must be the oldest.
		stamp := start.Add(time.Duration(i) * time.Minute)
		statePath := filepath.Join(logs, "runtime", "state.db")
		if err := os.Chtimes(statePath, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		chdirTest(t, parent)
	}
	code, doc, raw := runListingCLI(t, "--contract", "v2", "--json")
	if code != 0 {
		t.Fatalf("runs exit %d: %s", code, raw)
	}
	if doc.RunTotal != total {
		t.Fatalf("run_total %d, want %d", doc.RunTotal, total)
	}
	// The page is bounded by the default limit and may shrink further under
	// the byte budget; either way the document states the bound that applied
	// and counts what it omitted.
	if len(doc.Runs) > runsDefaultLimit {
		t.Fatalf("the default page must bound the listing to %d, got %d", runsDefaultLimit, len(doc.Runs))
	}
	if len(doc.Runs) != doc.Limits.RunsMax {
		t.Fatalf("the page must state the bound that applied: %d entries, runs_max %d", len(doc.Runs), doc.Limits.RunsMax)
	}
	if doc.Limits.Omitted["runs"] != total-len(doc.Runs) {
		t.Fatalf("the omitted count must cover the rest: %+v", doc.Limits)
	}
	newest := fmt.Sprintf("p%02d", total-1)
	if !strings.Contains(doc.Runs[0].Name, newest) {
		t.Errorf("the newest run must list first: %s", doc.Runs[0].Name)
	}
	for i := 1; i < len(doc.Runs); i++ {
		if doc.Runs[i-1].LastActivity < doc.Runs[i].LastActivity {
			t.Fatalf("the listing must run newest first: %s before %s", doc.Runs[i-1].Name, doc.Runs[i].Name)
		}
	}
	code, doc, raw = runListingCLI(t, "--contract", "v2", "--json", "--limit", "5")
	if code != 0 {
		t.Fatalf("runs --limit 5 exit %d: %s", code, raw)
	}
	if len(doc.Runs) != 5 || doc.Limits.Omitted["runs"] != total-5 {
		t.Fatalf("a smaller page must bound the listing: %+v", doc.Limits)
	}
	var out, stderr bytes.Buffer
	code = Main([]string{"runs", "--limit", fmt.Sprint(maxRunsPageLimit + 1)}, &out, &stderr)
	if code != exitUsage {
		t.Fatalf("an oversized page must be a usage error, got %d", code)
	}
	if !strings.Contains(stderr.String(), fmt.Sprintf("at most %d", maxRunsPageLimit)) {
		t.Fatalf("the refusal must name the bound: %q", stderr.String())
	}
	// The largest page parses and reports the complete count honestly.
	code, doc, raw = runListingCLI(t, "--contract", "v2", "--json", "--limit", "100")
	if code != 0 || doc.RunTotal != total {
		t.Fatalf("the largest page must parse and count honestly: %d %+v", code, doc)
	}
}

// TestRunsCommandContract pins the command surface: the version 2 document
// is the only JSON form, plain text renders under both contracts, and an
// invalid saved profile never blocks the read-only listing.
func TestRunsCommandContract(t *testing.T) {
	base := runsBase(t)
	home := filepath.Dir(base)
	c := runsProjectAt(t, filepath.Join(home, "contract"))
	var out, stderr bytes.Buffer
	code := Main([]string{"runs", "--json"}, &out, &stderr)
	if code != exitOperationFailed {
		t.Fatalf("legacy --json must refuse, got %d: %s%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "contract version 2") {
		t.Fatalf("the refusal must name the versioned document: %q", stderr.String())
	}
	for _, args := range [][]string{{"runs"}, {"runs", "--contract", "legacy"}, {"runs", "--contract", "v2"}} {
		out.Reset()
		stderr.Reset()
		code = Main(args, &out, &stderr)
		if code != 0 || !strings.HasPrefix(out.String(), "Saved runs (read-only") {
			t.Fatalf("plain runs %v must render the listing (exit %d):\n%s", args, code, out.String())
		}
		if !strings.Contains(out.String(), "No saved runs") {
			t.Fatalf("an empty base directory is a bounded result with a start hint:\n%s", out.String())
		}
	}
	// A read-only listing never creates this project's runtime directory.
	if _, err := os.Stat(filepath.Join(c.BaseDir, filepath.Base(projectName(t, c)), "runtime")); err == nil {
		t.Fatal("runs created a runtime directory for its own project")
	}
	// A corrupt profile blocks a start, not a read-only listing. Writing it
	// creates this project's runtime directory, so the empty-base assertions
	// above run first.
	if err := os.MkdirAll(filepath.Dir(c.ProfilePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.ProfilePath, []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	code = Main([]string{"runs", "--contract", "v2"}, &out, &stderr)
	if code != 0 || !strings.HasPrefix(out.String(), "Saved runs (read-only") {
		t.Fatalf("an invalid profile must not block the read-only listing (exit %d):\n%s%s", code, out.String(), stderr.String())
	}
	// The page bounds stay usage errors with the action naming the largest
	// page; the runs command has no --full export to suggest.
	out.Reset()
	stderr.Reset()
	code = Main([]string{"runs", "--limit", "0"}, &out, &stderr)
	if code != exitUsage {
		t.Fatalf("--limit 0 must be a usage error, got %d", code)
	}
}

// TestNoRunErrorsNameSearchedLocations pins the AX-025 lookup-error rule:
// every no-saved-run error names the resolved project root, the task path,
// and the searched base and runtime locations, and offers discovery as the
// next action. The legacy text and exit code stay unchanged.
func TestNoRunErrorsNameSearchedLocations(t *testing.T) {
	runsBase(t)
	c := runsProjectAt(t, t.TempDir())
	base := c.BaseDir
	var out, stderr bytes.Buffer
	code := Main([]string{"status", "--contract", "v2", "--json"}, &out, &stderr)
	if code != exitUnavailable {
		t.Fatalf("exit %d, want %d", code, exitUnavailable)
	}
	var doc errorDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
	}
	if doc.Error.Code != "run_not_found" {
		t.Fatalf("code %q", doc.Error.Code)
	}
	paths := strings.Join(doc.Error.Paths, "\n")
	for _, want := range []string{
		filepath.Join(projectName(t, c), "runtime", "state.db"),
		base, c.Root, c.Todo,
	} {
		if !strings.Contains(paths, want) {
			t.Fatalf("the no-run error must name %s; paths were %v", want, doc.Error.Paths)
		}
	}
	if observed, _ := doc.Error.Observed["base_dir"].(string); observed != base {
		t.Fatalf("observed.base_dir must name the searched base: %v", doc.Error.Observed)
	}
	actions := ""
	for _, a := range doc.Error.NextActions {
		actions += a.Command
	}
	if !strings.Contains(actions, "looper runs --contract v2 --json") {
		t.Fatalf("discovery must be a next action: %+v", doc.Error.NextActions)
	}
	// The same enrichment reaches stop, recovery, and inspect.
	for _, args := range [][]string{
		{"stop", "--contract", "v2", "--json"},
		{"recovery", "--preview", "--contract", "v2", "--json"},
		{"inspect", "T001", "--contract", "v2", "--json"},
	} {
		out.Reset()
		stderr.Reset()
		code = Main(args, &out, &stderr)
		if code != exitUnavailable {
			t.Fatalf("%v exit %d, want %d", args, code, exitUnavailable)
		}
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("%v stdout is not one error document: %v", args, err)
		}
		joined := strings.Join(doc.Error.Paths, "\n")
		if !strings.Contains(joined, base) || !strings.Contains(joined, c.Root) || !strings.Contains(joined, c.Todo) {
			t.Fatalf("%v must name the root, task file, and searched locations: %v", args, doc.Error.Paths)
		}
	}
	// The legacy contract keeps its pinned text and exit code.
	out.Reset()
	stderr.Reset()
	code = Main([]string{"stop"}, &out, &stderr)
	if code != 1 || strings.TrimSpace(stderr.String()) != "No saved Looper run exists in this project." {
		t.Fatalf("legacy stop text changed: exit %d, stderr %q", code, stderr.String())
	}
}

// TestRunsUnreadableBaseDirectory covers a base directory the command
// cannot read: the structured error names it instead of an empty listing.
func TestRunsUnreadableBaseDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root ignores directory permissions")
	}
	base := runsBase(t)
	locked := filepath.Join(base, "locked")
	if err := os.MkdirAll(locked, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_BASE_DIR", locked)
	if err := os.Chmod(locked, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0700) })
	runsProjectAt(t, t.TempDir())
	var out, stderr bytes.Buffer
	code := Main([]string{"runs", "--contract", "v2", "--json"}, &out, &stderr)
	if code != exitUnavailable {
		t.Fatalf("exit %d, want %d: %s%s", code, exitUnavailable, out.String(), stderr.String())
	}
	var doc errorDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not one error document: %v", err)
	}
	if doc.Error.Code != "base_unreadable" || len(doc.Error.Paths) != 1 || doc.Error.Paths[0] != locked {
		t.Fatalf("the refusal must name the unreadable base: %+v", doc.Error)
	}
}

// TestRunsOrderSurvivesEqualActivity keeps the listing deterministic when
// several runs share one activity stamp: the name breaks the tie.
func TestRunsOrderSurvivesEqualActivity(t *testing.T) {
	base := runsBase(t)
	// The slug cannot collide with a real root here, so hand-build the
	// runtime directories. Creation alone does not give equal stamps: the
	// filesystem timestamps each directory separately, and when those
	// stamps differ the newest-first sort wins before the name tie-break
	// runs. Pin one shared mtime so the activity is equal by construction.
	stamp := time.Now().Add(-time.Minute)
	for _, name := range []string{"zebra-11111111", "alpha-11111111", "mid-11111111"} {
		runtime := filepath.Join(base, name, "runtime")
		if err := os.MkdirAll(runtime, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(runtime, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	runsProjectAt(t, t.TempDir())
	code, doc, raw := runListingCLI(t, "--contract", "v2", "--json")
	if code != 0 {
		t.Fatalf("runs exit %d: %s", code, raw)
	}
	if doc.RunTotal != 3 {
		t.Fatalf("three equally-stamped runs must all list: %+v", doc)
	}
	for i := 1; i < len(doc.Runs); i++ {
		if doc.Runs[i-1].Name > doc.Runs[i].Name {
			t.Fatalf("equal activity must fall back to name order: %s after %s", doc.Runs[i-1].Name, doc.Runs[i].Name)
		}
	}
}

// newRunsEscapeFixture seeds the base directory with saved runs whose every
// recorded identity is escape-heavy — ampersand-heavy roots, refs, heads,
// branches, retained task identifiers, session rows, controller records, and
// one unparseable controller record — the shape that expanded each raw-byte
// clamp sixfold. It returns the config of the first project, the invocation
// point the listing answers from.
func newRunsEscapeFixture(t *testing.T, runs int) Config {
	t.Helper()
	base := runsBase(t)
	home := filepath.Dir(base)
	esc := preflightEscapeHeavy
	var first Config
	for i := 0; i < runs; i++ {
		root := filepath.Join(home, fmt.Sprintf("proj-amp-%02d", i)+esc(4))
		c := runsProjectAt(t, root)
		if i == 0 {
			first = c
		}
		logs := seedSavedRun(t, c, func(s *State) {
			task := Task{ID: fmt.Sprintf("T%04d", i) + esc(20), Title: esc(60), Priority: 1, Status: "doing"}
			s.Root, s.Todo = esc(60), esc(60)
			s.Head, s.Branch = esc(40), "refs/heads/"+esc(20)
			s.Ref, s.ExportHash = "refs/looper/"+esc(30), esc(40)
			s.Backlog = Backlog{Version: 1, Sources: []string{}, Goal: esc(60), Tasks: []Task{
				task, {ID: fmt.Sprintf("D%04d", i), Title: "done work", Priority: 1, Status: "done"},
			}}
			s.Attempts = append(s.Attempts, &Attempt{
				ID: esc(20) + fmt.Sprintf("-%04d", i), Task: task, Agent: esc(10), Phase: "running",
			})
		})
		// One stopped run keeps a stale controller record with escape-heavy
		// identity; the last one carries an unparseable record instead, so
		// the freshness error channel carries content too.
		if i == runs-1 {
			if err := os.WriteFile(controllerPath(filepath.Join(logs, "runtime")), []byte(esc(60)), 0600); err != nil {
				t.Fatal(err)
			}
		} else {
			seedControllerRecord(t, filepath.Join(logs, "runtime"), &ControllerInfo{
				Version: ControllerVersion, Session: esc(30) + fmt.Sprintf("-%04d", i), PID: deadPID(t),
				Root: esc(60), Mode: ControllerModeDetached, Started: esc(40),
				Build: &BuildInfo{Release: esc(40), Revision: esc(40), Modified: true},
			})
		}
	}
	chdirTest(t, first.Root)
	return first
}

// assertRunsStringsBounded checks the fit's structural discipline: every
// content-derived string obeys the bound the document states, measured in the
// bytes the JSON encoder writes.
func assertRunsStringsBounded(t *testing.T, doc *runsDocument, max int) {
	t.Helper()
	bounded := func(label, s string) {
		t.Helper()
		if jsonEscapedLen(s) > max {
			t.Errorf("%s exceeds the stated %d byte bound: %d emitted", label, max, jsonEscapedLen(s))
		}
	}
	bounded("project root", doc.Project.Root)
	bounded("project todo path", doc.Project.TodoPath)
	bounded("base dir", doc.Project.BaseDir)
	bounded("runtime dir", doc.Project.RuntimeDir)
	bounded("searched base dir", doc.Searched.BaseDir)
	bounded("scan note", doc.Searched.ScanNote)
	for i, r := range doc.Runs {
		bounded(fmt.Sprintf("run %d name", i), r.Name)
		bounded(fmt.Sprintf("run %d directory", i), r.Directory)
		bounded(fmt.Sprintf("run %d runtime dir", i), r.RuntimeDir)
		bounded(fmt.Sprintf("run %d run id", i), r.RunID)
		bounded(fmt.Sprintf("run %d root", i), r.Root)
		bounded(fmt.Sprintf("run %d todo path", i), r.TodoPath)
		bounded(fmt.Sprintf("run %d head", i), r.Head)
		bounded(fmt.Sprintf("run %d branch", i), r.Branch)
		bounded(fmt.Sprintf("run %d state error", i), r.StateError)
		for j, id := range r.RetainedTasks {
			bounded(fmt.Sprintf("run %d retained task %d", i, j), id)
		}
		bounded(fmt.Sprintf("run %d session id", i), r.LastSession.ID)
		bounded(fmt.Sprintf("run %d session started", i), r.LastSession.Started)
		bounded(fmt.Sprintf("run %d session ended", i), r.LastSession.Ended)
		bounded(fmt.Sprintf("run %d session outcome", i), r.LastSession.Outcome)
		bounded(fmt.Sprintf("run %d session reason", i), r.LastSession.Reason)
		bounded(fmt.Sprintf("run %d controller session", i), r.Controller.Session)
		bounded(fmt.Sprintf("run %d controller mode", i), r.Controller.Mode)
		bounded(fmt.Sprintf("run %d controller started", i), r.Controller.Started)
		bounded(fmt.Sprintf("run %d controller error", i), r.Controller.Error)
		if r.Controller.Build != nil {
			bounded(fmt.Sprintf("run %d build release", i), r.Controller.Build.Release)
			bounded(fmt.Sprintf("run %d build revision", i), r.Controller.Build.Revision)
		}
		for j, hint := range r.Select {
			bounded(fmt.Sprintf("run %d select hint %d", i, j), hint)
		}
	}
}

// TestRunsBoundsEscapeHeavyIdentities pins the published budget against
// escape-heavy identities, the probe that measured a 10 136-byte listing
// against the 8 192-byte budget it reported: the listing must stay one
// schema-valid document at or under its budget, and limits.bytes_budget must
// be a true statement about the emitted bytes.
func TestRunsBoundsEscapeHeavyIdentities(t *testing.T) {
	newRunsEscapeFixture(t, 4)
	_, doc, raw := runListingCLI(t, "--contract", "v2", "--json")
	if doc.Limits.BytesBudget < len(raw) {
		t.Fatalf("limits report a %d byte budget for a %d byte document", doc.Limits.BytesBudget, len(raw))
	}
	assertRunsStringsBounded(t, doc, doc.Limits.StringMax)
	// The listing stays useful, not merely small: every run stays counted,
	// the page keeps entries, and the omission the budget caused is stated.
	if doc.RunTotal != 4 || len(doc.Runs) == 0 {
		t.Fatalf("the bounded listing lost runs: total %d, shown %d", doc.RunTotal, len(doc.Runs))
	}
	if omitted := doc.Limits.Omitted["runs"]; len(doc.Runs)+omitted != doc.RunTotal {
		t.Fatalf("the omitted count must close the gap: shown %d, omitted %d, total %d",
			len(doc.Runs), omitted, doc.RunTotal)
	}
	if doc.Runs[0].State == "" || doc.Runs[0].Controller.State == "" {
		t.Fatalf("the first entry must keep its state and freshness: %+v", doc.Runs[0])
	}
	// The plain rendering reads the same structure and stays bounded too.
	code, _, plain := runListingCLI(t, "--contract", "v2")
	if code != 0 || !strings.Contains(plain, "Saved runs") {
		t.Fatalf("plain listing must render the same entries (exit %d):\n%s", code, plain)
	}
	if len(plain) > 4*runsBytesBudget {
		t.Fatalf("plain rendering of the bounded document is not bounded: %d bytes", len(plain))
	}
}

// TestRunsTerminalFitBoundsAnyContent pins the structural guarantee behind
// the budget: the ladder's terminal fit bounds every string to one rune at
// worst and keeps only the newest run, so the document it renders is
// structure alone whatever the recorded identities hold. Every earlier fit
// obeys the string bound it states in emitted bytes, and the ladder's chosen
// document fits whatever the earlier fits measured.
func TestRunsTerminalFitBoundsAnyContent(t *testing.T) {
	for _, tc := range []struct{ runs, limit int }{
		{2, 1}, {6, runsDefaultLimit}, {12, maxRunsPageLimit},
	} {
		newRunsEscapeFixture(t, tc.runs)
		ev, err := collectRunsEvidence(currentRunsConfig(t), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, fit := range runsFitsFor(tc.limit) {
			assertRunsStringsBounded(t, renderRunsDocument(ev, tc.limit, fit), fit.stringMax)
		}
		terminal := renderRunsDocument(ev, tc.limit, runsTerminalFit)
		if size := runsEncoded(terminal); size > runsBytesBudget {
			t.Fatalf("%d runs, limit %d: the terminal fit renders %d bytes, over the %d byte budget",
				tc.runs, tc.limit, size, runsBytesBudget)
		}
		remarshaled, err := json.Marshal(terminal)
		if err != nil {
			t.Fatal(err)
		}
		if err = contractschema.ValidateJSON(runsSchema(t), remarshaled); err != nil {
			t.Fatalf("%d runs, limit %d: the terminal document violates its schema: %v", tc.runs, tc.limit, err)
		}
		if chosen := buildRunsDocument(ev, tc.limit); runsEncoded(chosen) > runsBytesBudget {
			t.Fatalf("%d runs, limit %d: the chosen document exceeds the budget it reports: %d bytes",
				tc.runs, tc.limit, runsEncoded(chosen))
		}
	}
}

// currentRunsConfig parses the runs command from the directory the test sits
// in, the invocation the terminal-fit proof answers from.
func currentRunsConfig(t *testing.T) Config {
	t.Helper()
	c, err := Parse([]string{"runs"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
