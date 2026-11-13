package looper

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImportWaitEventDoesNotRepeat(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	e.Config.Plans = []string{planFile(t, "Add telemetry")}
	e.startupImportPlans()
	e.State.Attempts = append(e.State.Attempts, &Attempt{ID: "active", Phase: "running"})
	for i := 0; i < 10; i++ {
		e.processPlanImports(context.Background())
	}
	if e.err != nil {
		t.Fatal(e.err)
	}
	var count int
	if err := e.Store.db.QueryRow("SELECT count(*) FROM events WHERE kind='import' AND message LIKE '%waits for active work%'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("got %d wait events for one transition, want 1", count)
	}
}

func TestImportAtCompletedCheckBoundary(t *testing.T) {
	for _, mode := range []string{"apply", "failed", "drain"} {
		t.Run(mode, func(t *testing.T) {
			proposal := `{"additions":[],"covered":[{"requirement":"new requirement","task_id":"T002","evidence":"already scheduled"}],"conflicts":[]}`
			if mode == "failed" {
				proposal = `{"additions":[],"covered":[],"conflicts":["unsupported requirement"]}`
			}
			bin, _ := fakeAgent(t, proposal)
			t.Setenv("LOOPER_FAKE_PLAN", `{"setup":"","verify":"true","tasks":[{"task_id":"T001","write_paths":["."]},{"task_id":"T002","write_paths":["."]}]}`)
			t.Setenv("LOOPER_FAKE_SUMMARY", `{"task_id":"T001","status":"done","summary":"verified","files":[],"blockers":[],"followups":[]}`)
			c := repoConfig(t, "run", "--jobs", "1", "--max-iterations", "1")
			c.CodexBin, c.ClaudeBin = bin, bin
			c.ReviewEvery = 100
			release := filepath.Join(t.TempDir(), "release")
			c.Verify = fmt.Sprintf("while ! test -f %q; do sleep 0.02; done", release)
			b := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{
				{ID: "T001", Title: "first", Priority: 1, Status: "todo"},
				{ID: "T002", Title: "second", Priority: 1, Status: "todo", DependsOn: []string{"T001"}},
			}}
			if err := writeJSON(c.Todo, b); err != nil {
				t.Fatal(err)
			}
			e, lock, err := NewEngine(c, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			defer e.Store.Close()
			// The run context bounds failure, not the healthy path: it must
			// outlast two event waits plus completion, which concurrent
			// suites on this host stretch past a minute (the T106 record).
			// The waits size themselves to host pressure, so the context
			// sizes itself the same way.
			ctx, cancel := context.WithTimeout(context.Background(), 2*testBudget(importBoundaryEventWait)+time.Minute)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				code, err := e.Run(ctx)
				if err == nil && code != 2 && !(mode == "drain" && code == 0) {
					err = fmt.Errorf("unexpected exit %d", code)
				}
				done <- err
			}()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(testBudget(5 * time.Second)):
					t.Error("run did not stop")
				}
			}()
			waitImportBoundaryEvent(t, e, ctx, "Checking T001")
			id, err := submitPlanImport(e.Dir, e.controller.Session, "", []string{planFile(t, "new requirement")})
			if err != nil {
				t.Fatal(err)
			}
			secondID, err := submitPlanImport(e.Dir, e.controller.Session, "", []string{planFile(t, "another requirement")})
			if err != nil {
				t.Fatal(err)
			}
			waitImportBoundaryEvent(t, e, ctx, "waits for active work")
			if mode == "drain" {
				if err = os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err = os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				done <- err
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("run timed out")
			}
			saved, err := e.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if saved.Backlog.Task("T001").Status != "done" {
				t.Fatal("first task did not integrate")
			}
			var record *ImportRecord
			for _, r := range saved.Imports {
				if r.RequestID == id {
					record = r
				}
			}
			if record == nil {
				t.Fatal("missing receipt")
			}
			want := ImportApplied
			if mode == "failed" {
				want = ImportFailed
			}
			if mode == "drain" {
				want = ImportWaiting
			}
			if record.Status != want {
				t.Fatalf("import status %s, want %s", record.Status, want)
			}
			var second *ImportRecord
			for _, r := range saved.Imports {
				if r.RequestID == secondID {
					second = r
				}
			}
			if second == nil || second.Status != want {
				t.Fatalf("second import not processed: %+v, want %s", second, want)
			}

			rows, err := e.Store.db.Query("SELECT kind,message FROM events ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			completed := false
			for rows.Next() {
				var kind, msg string
				if err = rows.Scan(&kind, &msg); err != nil {
					t.Fatal(err)
				}
				if kind == "result" && strings.Contains(msg, "T001") {
					completed = true
				}
				if completed && kind == "plan" && strings.Contains(msg, "Planning independent") {
					t.Fatal("ordinary planning preceded pending import")
				}
			}
			if err = rows.Err(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// importBoundaryEventWait bounds the wait for one boundary event. It is a
// failure bound against a condition that never arrives, not a budget the
// healthy path must outrun: reaching the first check takes a planning round, a
// worker run, and a check launch, which stays under one second on an idle
// host. The IO pressure of concurrent suites on this host has stretched that
// chain past 30 s, and a 5 s bound failed a full-suite run (the T106 record),
// so the wait applies it through testBudget and grows further with the
// pressure the host reports while the suite runs.
const importBoundaryEventWait = 120 * time.Second

// waitImportBoundaryEvent blocks until the journal holds an event whose message
// contains text. A fired run context ends the wait early: a cancelled run can
// no longer emit the event, so polling on would only hide the cause.
func waitImportBoundaryEvent(t *testing.T, e *Engine, ctx context.Context, text string) {
	t.Helper()
	count := func() int {
		var n int
		if err := e.Store.db.QueryRow("SELECT count(*) FROM events WHERE instr(message,?)>0", text).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	deadline := time.Now().Add(testBudget(importBoundaryEventWait))
	for time.Now().Before(deadline) {
		if count() > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("run ended before an event containing %q arrived: %v", text, ctx.Err())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Re-check after the deadline: an event that landed between the last poll
	// and the bound must not read as a miss.
	if count() > 0 {
		return
	}
	t.Fatalf("no event containing %q", text)
}

// pauseAgent writes a canned JSON result like fakeAgent, but holds the T001
// and T002 workers until per-task release files appear, so one run can hold
// two ordinary workers active while it accepts an import. The prompt on
// standard input selects the response: planning answers LOOPER_PAUSE_PLAN,
// workers answer a per-task done report, and every other prompt — the import
// analysis among them — answers LOOPER_PAUSE_RESULT, which carries the
// analysis proposal.
func pauseAgent(t *testing.T, proposal string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "agent.sh")
	script := `#!/bin/bash
out=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$a"; fi
  prev="$a"
done
p="$(cat)"
result="$LOOPER_PAUSE_RESULT"
case "$p" in
  *"Plan execution for Looper"*) result="$LOOPER_PAUSE_PLAN" ;;
  *"Complete task "*)
    tid="$(printf '%s' "$p" | sed -n 's/^Complete task \(T[0-9]*\):.*/\1/p')"
    case "$tid" in
      T001) while ! test -f "$LOOPER_PAUSE_RELEASE_T1"; do sleep 0.05; done ;;
      T002) while ! test -f "$LOOPER_PAUSE_RELEASE_T2"; do sleep 0.05; done ;;
    esac
    result="$(printf '{"task_id":"%s","status":"done","summary":"done","files":[],"blockers":[],"followups":[]}' "$tid")"
    ;;
esac
if [ -n "$out" ]; then printf '%s' "$result" > "$out"; fi
if [ -n "$result" ]; then printf '{"structured_output":%s}\n' "$result"; fi
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_PAUSE_RESULT", proposal)
	return bin
}

// importStartCount reports how often one task's dispatch began.
func importStartCount(t *testing.T, e *Engine, task string) int {
	t.Helper()
	var n int
	if err := e.Store.db.QueryRow("SELECT count(*) FROM events WHERE kind='start' AND instr(message,?)>0", task).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// importBoundaryEventID returns the first event identifier whose kind and
// message match, so ordering between two boundaries stays checkable.
func importBoundaryEventID(t *testing.T, e *Engine, kind, text string) int64 {
	t.Helper()
	var id int64
	if err := e.Store.db.QueryRow("SELECT id FROM events WHERE kind=? AND instr(message,?)>0 ORDER BY id LIMIT 1", kind, text).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// savedReceipt finds one import receipt in a loaded state snapshot.
func savedReceipt(s *State, requestID string) *ImportRecord {
	for _, r := range s.Imports {
		if r.RequestID == requestID {
			return r
		}
	}
	return nil
}

// TestImportPauseHoldsNewDispatch covers the T044 acceptance fixture: a run
// with two active workers accepts an import, one worker finishes, and the
// paused dispatch must not start the next ordinary task while the import
// waits for the other worker. The old loop dispatched that task in the same
// pass that consumed the finished worker's result. Only the import's own
// outcome — applied or failed analysis — releases the pause.
func TestImportPauseHoldsNewDispatch(t *testing.T) {
	for _, mode := range []string{"applies", "fails"} {
		t.Run(mode, func(t *testing.T) {
			proposal := additionProposal
			if mode == "fails" {
				proposal = `{"additions":[],"covered":[],"conflicts":["replaces the existing scope"]}`
			}
			bin := pauseAgent(t, proposal)
			t.Setenv("LOOPER_PAUSE_PLAN", `{"setup":"","verify":"true","tasks":[{"task_id":"T001","write_paths":["one/"]},{"task_id":"T002","write_paths":["two/"]},{"task_id":"T003","write_paths":["three/"]}]}`)
			c := repoConfig(t, "run", "--jobs", "2")
			c.CodexBin, c.ClaudeBin = bin, bin
			c.ReviewEvery = 100
			b := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{
				{ID: "T001", Title: "first", Priority: 1, Status: "todo"},
				{ID: "T002", Title: "second", Priority: 1, Status: "todo"},
				{ID: "T003", Title: "third", Priority: 1, Status: "todo"},
			}}
			if err := writeJSON(c.Todo, b); err != nil {
				t.Fatal(err)
			}
			releaseT1 := filepath.Join(t.TempDir(), "release-t1")
			releaseT2 := filepath.Join(t.TempDir(), "release-t2")
			t.Setenv("LOOPER_PAUSE_RELEASE_T1", releaseT1)
			t.Setenv("LOOPER_PAUSE_RELEASE_T2", releaseT2)
			e, lock, err := NewEngine(c, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			defer e.Store.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*testBudget(importBoundaryEventWait)+time.Minute)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				code, err := e.Run(ctx)
				if err == nil && code != 0 {
					err = fmt.Errorf("unexpected exit %d", code)
				}
				done <- err
			}()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(testBudget(5 * time.Second)):
					t.Error("run did not stop")
				}
			}()
			waitImportBoundaryEvent(t, e, ctx, "T001  first")
			waitImportBoundaryEvent(t, e, ctx, "T002  second")
			id, err := submitPlanImport(e.Dir, e.controller.Session, "", []string{planFile(t, "pause requirement")})
			if err != nil {
				t.Fatal(err)
			}
			waitImportBoundaryEvent(t, e, ctx, "waits for active work")
			// T001 finishes while T002 stays held. The import stays waiting,
			// so T003 must not start: with the missing guard it began in the
			// very pass that consumed T001's worker result, which is before
			// the result event waited for here.
			if err = os.WriteFile(releaseT1, nil, 0600); err != nil {
				t.Fatal(err)
			}
			waitImportBoundaryEvent(t, e, ctx, "T001  done |")
			if n := importStartCount(t, e, "T003"); n != 0 {
				t.Fatalf("T003 started %d time(s) while T002 runs and the import waits", n)
			}
			saved, err := e.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if record := savedReceipt(saved, id); record == nil || record.Status != ImportWaiting {
				t.Fatalf("import must stay waiting while T002 runs: %+v", record)
			}
			// The pause, not boundary timing, holds T003 back.
			select {
			case <-ctx.Done():
				t.Fatalf("run ended during the pause: %v", ctx.Err())
			case <-time.After(testBudget(2 * time.Second)):
			}
			if n := importStartCount(t, e, "T003"); n != 0 {
				t.Fatalf("T003 started %d time(s) during the pause grace period", n)
			}
			// T002 finishes, the import reaches its outcome at the free
			// boundary, and only then does ordinary dispatch resume.
			if err = os.WriteFile(releaseT2, nil, 0600); err != nil {
				t.Fatal(err)
			}
			outcome, outcomeKind := "Applied plan import", "import"
			if mode == "fails" {
				outcome, outcomeKind = id+" failed", "import-error"
			}
			waitImportBoundaryEvent(t, e, ctx, outcome)
			waitImportBoundaryEvent(t, e, ctx, "T003  third")
			if started, resolved := importBoundaryEventID(t, e, "start", "T003"), importBoundaryEventID(t, e, outcomeKind, outcome); started < resolved {
				t.Fatalf("T003 dispatched at event %d before the import outcome at %d", started, resolved)
			}
			if err = os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				done <- err
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("run timed out")
			}
			saved, err = e.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			record := savedReceipt(saved, id)
			if record == nil {
				t.Fatal("missing receipt")
			}
			for _, task := range []string{"T001", "T002", "T003"} {
				if saved.Backlog.Task(task).Status != "done" {
					t.Fatalf("%s did not finish through the pause: %s", task, saved.Backlog.Task(task).Status)
				}
			}
			if mode == "applies" {
				if record.Status != ImportApplied || len(record.AddedTasks) != 1 {
					t.Fatalf("import not applied after the boundary: %+v", record)
				}
				if added := saved.Backlog.Task(record.AddedTasks[0]); added == nil || added.Status != "todo" {
					t.Fatalf("addition missing or dispatched before the stop: %+v", added)
				}
			} else if record.Status != ImportFailed || len(record.Conflicts) != 1 || !strings.Contains(record.Error, "conflicts with existing scope") {
				t.Fatalf("failed analysis must fail through its conflicts and leave the pause released: %+v", record)
			} else if len(saved.Backlog.Tasks) != 3 {
				t.Fatalf("failed import must leave the backlog unchanged: %+v", saved.Backlog)
			}
		})
	}
}

func TestDrainingImportsDoNotEmitRepeatedOutcomes(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	e.Config.Plans = []string{planFile(t, "Add telemetry")}
	e.startupImportPlans()
	if err := os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		e.processPlanImports(context.Background())
	}
	if e.err != nil {
		t.Fatal(e.err)
	}
	if e.State.Imports[0].Status != ImportQueued {
		t.Fatal("drain must preserve queued imports")
	}
	var n int
	if err := e.Store.db.QueryRow("SELECT count(*) FROM events WHERE message LIKE '%controller is draining%'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n > 1 {
		t.Fatalf("drain emitted %d repeated import outcomes", n)
	}
}
