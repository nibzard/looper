package looper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This helper stops at durable crash boundaries. The parent kills the process,
// so neither deferred cleanup nor a graceful controller shutdown can run.
func TestPlanImportCrashHelper(t *testing.T) {
	raw := os.Getenv("LOOPER_IMPORT_CRASH_CONFIG")
	if raw == "" {
		return
	}
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	if err = e.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	mode := os.Getenv("LOOPER_IMPORT_CRASH_MODE")
	paths := []string{os.Getenv("LOOPER_IMPORT_CRASH_PLAN")}
	if mode == "digest" {
		e.acceptPlanImport("prior-import", paths, false)
		e.processPlanImports(context.Background())
	}
	id, err := submitPlanImport(e.Dir, e.controller.Session, paths)
	if err != nil {
		t.Fatal(err)
	}
	req, err := claimNextRequest(e.Dir, RequestTypePlanImport)
	if err != nil || req == nil || req.ID != id {
		t.Fatalf("claim: %+v, %v", req, err)
	}
	switch mode {
	case "captured":
		if _, err = capturePlans(e.Dir, id, paths); err != nil {
			t.Fatal(err)
		}
	case "queued", "applied", "removed":
		e.acceptPlanImport(id, paths, false)
		if mode != "queued" {
			e.processPlanImports(context.Background())
		}
		// Lose the acknowledgement, as when the process dies before writing it.
		if err = os.Remove(filepath.Join(ackDir(e.Dir), id+".json")); err != nil {
			t.Fatal(err)
		}
		if mode == "removed" {
			if err = os.Remove(filepath.Join(processingDir(e.Dir), id+".json")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if e.err != nil {
		t.Fatal(e.err)
	}
	if err = os.WriteFile(os.Getenv("LOOPER_IMPORT_CRASH_READY"), []byte(id), 0600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestPlanImportRecoversAfterControllerKill(t *testing.T) {
	for _, mode := range []string{"claimed", "captured", "queued", "applied", "removed", "digest"} {
		t.Run(mode, func(t *testing.T) {
			c := repoConfig(t, "run")
			bin, _ := fakeAgent(t, additionProposal)
			c.CodexBin, c.ClaudeBin = bin, bin
			b := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{{ID: "T001", Title: "existing", Status: "done", Priority: 1}}}
			if err := writeJSON(c.Todo, b); err != nil {
				t.Fatal(err)
			}
			plan := planFile(t, "Add telemetry")
			ready := filepath.Join(t.TempDir(), "ready")
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPlanImportCrashHelper$")
			cmd.Env = append(os.Environ(), "LOOPER_IMPORT_CRASH_CONFIG="+string(raw), "LOOPER_IMPORT_CRASH_MODE="+mode, "LOOPER_IMPORT_CRASH_PLAN="+plan, "LOOPER_IMPORT_CRASH_READY="+ready)
			log, err := os.Create(filepath.Join(t.TempDir(), "child.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			cmd.Stdout, cmd.Stderr = log, log
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cmd.Process.Kill(); cmd.Wait() }()
			var id []byte
			for {
				id, err = os.ReadFile(ready)
				if err == nil {
					break
				}
				select {
				case <-ctx.Done():
					output, _ := os.ReadFile(log.Name())
					t.Fatalf("helper did not reach %s: %s", mode, output)
				case <-time.After(10 * time.Millisecond):
				}
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = cmd.Wait(); err == nil {
				t.Fatal("expected an unclean exit")
			}
			// Accepted imports recover from captured bytes even if the source is gone.
			if mode == "queued" || mode == "applied" || mode == "removed" {
				if err = os.Remove(plan); err != nil {
					t.Fatal(err)
				}
			}
			for restart := 0; restart < 2; restart++ {
				e, lock, err := NewEngine(c, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				func() {
					defer lock.Close()
					defer e.Store.Close()
					if err = e.initialize(context.Background()); err != nil {
						t.Fatal(err)
					}
					e.processPlanImports(context.Background())
					if e.err != nil {
						t.Fatal(e.err)
					}
					ack, err := waitForPlanImport(e.Dir, string(id), time.Second)
					if err != nil || ack.Status != ImportApplied || fmt.Sprint(ack.AddedTasks) != "[T002]" {
						t.Fatalf("poll: %+v, %v", ack, err)
					}
					if len(e.State.Imports) != 1 || len(e.State.Backlog.Tasks) != 2 {
						t.Fatalf("duplicate or lost plan: %d receipts, %d tasks", len(e.State.Imports), len(e.State.Backlog.Tasks))
					}
					if _, err = os.Stat(filepath.Join(processingDir(e.Dir), string(id)+".json")); !os.IsNotExist(err) {
						t.Fatalf("claim remains: %v", err)
					}
				}()
			}
		})
	}
}

func TestClaimedPlanSurvivesAcknowledgementFailure(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	id, err := submitPlanImport(e.Dir, e.controller.Session, []string{planFile(t, "Add telemetry")})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(ackDir(e.Dir), []byte("block directory creation"), 0600); err != nil {
		t.Fatal(err)
	}
	e.claimPlanImports()
	if e.err == nil {
		t.Fatal("expected acknowledgement failure")
	}
	if _, err = os.Stat(filepath.Join(processingDir(e.Dir), id+".json")); err != nil {
		t.Fatalf("lost claim: %v", err)
	}
	if err = os.Remove(ackDir(e.Dir)); err != nil {
		t.Fatal(err)
	}
	e.err = nil
	if err = e.recoverPlanImports(); err != nil {
		t.Fatal(err)
	}
	e.processPlanImports(context.Background())
	ack, err := waitForPlanImport(e.Dir, id, time.Second)
	if err != nil || ack.Status != ImportApplied {
		t.Fatalf("poll: %+v, %v", ack, err)
	}
}

func TestClaimedPlanSurvivesReceiptFailure(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	id, err := submitPlanImport(e.Dir, e.controller.Session, []string{planFile(t, "Add telemetry")})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	e.claimPlanImports()
	if e.err == nil {
		t.Fatal("expected receipt save failure")
	}
	if _, err = os.Stat(filepath.Join(processingDir(e.Dir), id+".json")); err != nil {
		t.Fatalf("lost claim: %v", err)
	}
	store, err := OpenStore(filepath.Join(e.Dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e.Store, e.err = store, nil
	e.State, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err = e.recoverPlanImports(); err != nil {
		t.Fatal(err)
	}
	e.processPlanImports(context.Background())
	ack, err := waitForPlanImport(e.Dir, id, time.Second)
	if err != nil || ack.Status != ImportApplied || len(e.State.Imports) != 1 {
		t.Fatalf("poll: %+v, %v", ack, err)
	}
}

func TestOrphanedPlanWithMissingSourceResolvesPoll(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	plan := planFile(t, "Add telemetry")
	id, err := submitPlanImport(e.Dir, e.controller.Session, []string{plan})
	if err != nil {
		t.Fatal(err)
	}
	if req, err := claimNextRequest(e.Dir, RequestTypePlanImport); err != nil || req == nil {
		t.Fatalf("claim: %+v, %v", req, err)
	}
	if err = os.Remove(plan); err != nil {
		t.Fatal(err)
	}
	if err = e.recoverPlanImports(); err != nil {
		t.Fatal(err)
	}
	ack, err := waitForPlanImport(e.Dir, id, time.Second)
	if err != nil || ack.Status != AckError || ack.Error == "" {
		t.Fatalf("poll: %+v, %v", ack, err)
	}
	clearSessionFiles(e.Dir)
	ack, err = waitForPlanImport(e.Dir, id, time.Second)
	if err != nil || ack.Status != AckError {
		t.Fatalf("lost failure after restart: %+v, %v", ack, err)
	}
	if len(e.State.Imports) != 0 {
		t.Fatal("unavailable content was accepted")
	}
}
