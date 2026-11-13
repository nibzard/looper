package looper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// crashHelperBound caps the helper's final wait. The parent kills the
// helper at the crash boundary, but a runner can kill the whole suite
// first, and neither the parent's deferred kill nor its context
// cancellation runs then. Without a bound the helper sleeps forever while
// holding its claims, its lock, and its log files.
const crashHelperBound = 5 * time.Minute

// crashHelperPoll is how often the helper checks whether its parent still
// exists. It sets how long an orphaned helper outlives its suite.
const crashHelperPoll = 250 * time.Millisecond

// crashHelperDeadline honors the bound the parent passed through the
// environment, so a suite that dies before its own context fires still
// ends the helper. An unreadable value falls back to the default bound.
func crashHelperDeadline() time.Time {
	bound := time.Now().Add(crashHelperBound)
	if raw := os.Getenv("LOOPER_IMPORT_CRASH_DEADLINE"); raw != "" {
		if sec, err := strconv.ParseInt(raw, 10, 64); err == nil {
			bound = time.Unix(sec, 0)
		}
	}
	return bound
}

// pidAlive reports process existence, not process health.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// crashHelperParent returns the process the helper watches. The spawner
// names it through the environment, and that name is authoritative: a
// runner can kill the suite while the helper is still setting up, so the
// helper may already be reparented by the time it starts waiting, and its
// current parent would then be PID 1 for as long as the machine lives.
func crashHelperParent() int {
	if raw := os.Getenv("LOOPER_IMPORT_CRASH_PARENT_PID"); raw != "" {
		if pid, err := strconv.Atoi(raw); err == nil && pid > 0 {
			return pid
		}
	}
	return os.Getppid()
}

// withEnvParent names a parent for a spawned helper, replacing any name
// this process inherited, so a helper chain never watches a grandparent.
func withEnvParent(env []string, pid int) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "LOOPER_IMPORT_CRASH_PARENT_PID=") {
			out = append(out, kv)
		}
	}
	return append(out, "LOOPER_IMPORT_CRASH_PARENT_PID="+strconv.Itoa(pid))
}

// crashHelperHalt replaces an endless sleep: the helper ends itself when
// its parent is gone, or when the bound passes, whichever comes first.
// Getppid is a live syscall on Linux, so a parent that dies leaves the
// helper reparented and the comparison fires within one poll, whatever
// happened to the parent's own pid afterwards. The existence probe only
// ends the wait early on a clean miss: a reused or zombie parent pid still
// reads as alive there, and the comparison or the bound ends those waits.
func crashHelperHalt(parent int, bound time.Time) {
	for time.Now().Before(bound) {
		time.Sleep(crashHelperPoll)
		if os.Getppid() != parent || !pidAlive(parent) {
			return
		}
	}
}

// This helper stops at durable crash boundaries. The parent kills the
// process, so neither deferred cleanup nor a graceful controller shutdown
// can run. When the parent suite dies first, the helper ends itself
// through crashHelperHalt instead of waiting for a kill that never comes.
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
		e.acceptPlanImport(&SettingsRequest{ID: "prior-import", Paths: paths}, false)
		e.processPlanImports(context.Background())
	}
	id, err := submitPlanImport(e.Dir, e.controller.Session, "", paths)
	if err != nil {
		t.Fatal(err)
	}
	req, _, err := claimNextRequest(e.Dir, RequestTypePlanImport)
	if err != nil || req == nil || req.ID != id {
		t.Fatalf("claim: %+v, %v", req, err)
	}
	switch mode {
	case "captured":
		if _, err = capturePlans(e.Dir, id, paths); err != nil {
			t.Fatal(err)
		}
	case "queued", "applied", "removed":
		e.acceptPlanImport(&SettingsRequest{ID: id, Paths: paths}, false)
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
	crashHelperHalt(crashHelperParent(), crashHelperDeadline())
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
			cmd.Env = withEnvParent(os.Environ(), os.Getpid())
			cmd.Env = append(cmd.Env, "LOOPER_IMPORT_CRASH_CONFIG="+string(raw), "LOOPER_IMPORT_CRASH_MODE="+mode, "LOOPER_IMPORT_CRASH_PLAN="+plan, "LOOPER_IMPORT_CRASH_READY="+ready, "LOOPER_IMPORT_CRASH_DEADLINE="+strconv.FormatInt(time.Now().Add(crashHelperBound).Unix(), 10))
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
				// The helper creates the ready file and then writes the
				// request identifier into it, so a read between the two can
				// observe an empty file. An empty read means not ready yet;
				// a helper that died before writing still fails at the bound.
				if err == nil && len(id) > 0 {
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

// crashHelperEnded accepts a reaped pid miss and an unreaped zombie alike:
// the helper's exit already released its lock and its descriptors, so a
// corpse waiting for a parent that never reaps is still an ended orphan.
// It matches runIdentityCommand's reading of the state field.
func crashHelperEnded(pid int) bool {
	if !pidAlive(pid) {
		return true
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	text := string(b)
	i := strings.LastIndex(text, ")")
	return i < 0 || strings.HasPrefix(strings.TrimSpace(text[i+1:]), "Z")
}

// TestPlanImportCrashParentShim plays a suite binary that dies mid-run: it
// starts the crash helper the same way the recovery test does, reports the
// helper's process id, and then waits until the outer test kills it. It
// waits through crashHelperHalt, so an interruption one level up cannot
// orphan this shim either.
func TestPlanImportCrashParentShim(t *testing.T) {
	if os.Getenv("LOOPER_IMPORT_CRASH_PARENT") == "" {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPlanImportCrashHelper$")
	cmd.Env = withEnvParent(os.Environ(), os.Getpid())
	log, err := os.Create(os.Getenv("LOOPER_IMPORT_CRASH_PARENT_LOG"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if err = os.WriteFile(os.Getenv("LOOPER_IMPORT_CRASH_PARENT_READY"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	crashHelperHalt(crashHelperParent(), crashHelperDeadline())
}

// A runner can kill a suite while the crash helper holds its boundary, and
// the orphaned helper then sleeps forever with a deleted lock and log. This
// kills the direct parent, which reproduces that death without killing this
// test binary: the helper must end itself soon after, and the boundary it
// held must still recover.
func TestPlanImportCrashHelperEndsWithItsDeadSuite(t *testing.T) {
	c := repoConfig(t, "run")
	bin, _ := fakeAgent(t, additionProposal)
	c.CodexBin, c.ClaudeBin = bin, bin
	b := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{{ID: "T001", Title: "existing", Status: "done", Priority: 1}}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	plan := planFile(t, "Add telemetry")
	dir := t.TempDir()
	ready := filepath.Join(dir, "helper-ready")
	parentReady := filepath.Join(dir, "parent-ready")
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPlanImportCrashParentShim$")
	cmd.Env = withEnvParent(os.Environ(), os.Getpid())
	cmd.Env = append(cmd.Env, "LOOPER_IMPORT_CRASH_CONFIG="+string(raw), "LOOPER_IMPORT_CRASH_MODE=claimed", "LOOPER_IMPORT_CRASH_PLAN="+plan, "LOOPER_IMPORT_CRASH_READY="+ready, "LOOPER_IMPORT_CRASH_DEADLINE="+strconv.FormatInt(time.Now().Add(crashHelperBound).Unix(), 10), "LOOPER_IMPORT_CRASH_PARENT=1", "LOOPER_IMPORT_CRASH_PARENT_READY="+parentReady, "LOOPER_IMPORT_CRASH_PARENT_LOG="+filepath.Join(dir, "parent.log"))
	log, err := os.Create(filepath.Join(dir, "suite.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	var id, helperPid []byte
	for {
		if id, err = os.ReadFile(ready); err == nil {
			helperPid, err = os.ReadFile(parentReady)
			if err == nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			output, _ := os.ReadFile(filepath.Join(dir, "parent.log"))
			t.Fatalf("helper did not reach its boundary: %s", output)
		case <-time.After(10 * time.Millisecond):
		}
	}
	helper, err := strconv.Atoi(string(helperPid))
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err == nil {
		t.Fatal("expected an unclean suite exit")
	}
	deadline := time.Now().Add(testBudget(15 * time.Second))
	for !crashHelperEnded(helper) {
		if time.Now().After(deadline) {
			t.Fatalf("helper %d outlived its dead suite", helper)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The self-exit lost nothing a kill would have lost: a restart still
	// applies the import the helper held.
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
	}()
}

func TestClaimedPlanSurvivesAcknowledgementFailure(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	id, err := submitPlanImport(e.Dir, e.controller.Session, "", []string{planFile(t, "Add telemetry")})
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
	id, err := submitPlanImport(e.Dir, e.controller.Session, "", []string{planFile(t, "Add telemetry")})
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
	id, err := submitPlanImport(e.Dir, e.controller.Session, "", []string{plan})
	if err != nil {
		t.Fatal(err)
	}
	if req, _, err := claimNextRequest(e.Dir, RequestTypePlanImport); err != nil || req == nil {
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
