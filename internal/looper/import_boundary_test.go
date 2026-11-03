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
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
				case <-time.After(5 * time.Second):
					t.Error("run did not stop")
				}
			}()
			waitImportBoundaryEvent(t, e, "Checking T001")
			id, err := submitPlanImport(e.Dir, e.controller.Session, []string{planFile(t, "new requirement")})
			if err != nil {
				t.Fatal(err)
			}
			secondID, err := submitPlanImport(e.Dir, e.controller.Session, []string{planFile(t, "another requirement")})
			if err != nil {
				t.Fatal(err)
			}
			waitImportBoundaryEvent(t, e, "waits for active work")
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

func waitImportBoundaryEvent(t *testing.T, e *Engine, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := e.Store.db.QueryRow("SELECT count(*) FROM events WHERE instr(message,?)>0", text).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no event containing %q", text)
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
