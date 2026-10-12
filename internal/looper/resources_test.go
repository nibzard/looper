package looper

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const resourceSetupFixture = `set -e
touch "$LOOPER_ARTIFACT_DIR/database"
cat > "${LOOPER_RESOURCE_CLEANUP_FILE:-$LOOPER_ARTIFACT_DIR/cleanup-resources.sh}" <<'CLEANUP'
test -f "$LOOPER_ARTIFACT_DIR/database"
rm "$LOOPER_ARTIFACT_DIR/database"
echo cleaned > "$LOOPER_ARTIFACT_DIR/cleanup-proof"
CLEANUP
`

func TestVerifyCleansResourcesAfterSuccessAndFailure(t *testing.T) {
	for _, verify := range []string{"true", "exit 7"} {
		t.Run(verify, func(t *testing.T) {
			e, op, a, c, _ := assessmentFixture(t, "")
			dir := filepath.Join(a.Directory, "checks", c.Commit)
			result := e.verify(context.Background(), op, a, c, Plan{Setup: resourceSetupFixture, Verify: verify})
			if result.OK != (verify == "true") {
				t.Fatalf("cleanup changed check outcome: %+v", result)
			}
			if _, err := os.Stat(filepath.Join(dir, "database")); !os.IsNotExist(err) {
				t.Fatal("finished check retained its temporary database")
			}
			if _, err := os.Stat(filepath.Join(dir, "cleanup-proof")); err != nil {
				t.Fatal("check did not run registered cleanup")
			}
		})
	}
}

func TestWorkerCleansResources(t *testing.T) {
	e, _, _ := assessRunEngine(t, "", 0)
	e.Config.Setup = resourceSetupFixture
	if code, err := e.Run(context.Background()); code != 0 || err != nil {
		t.Fatalf("run failed: %d %v", code, err)
	}
	proofs, err := filepath.Glob(filepath.Join(e.Dir, "task-*", "work-*", "cleanup-proof"))
	if err != nil || len(proofs) != 1 {
		t.Fatalf("worker cleanup did not run: %v %v", proofs, err)
	}
}

func TestCancelledCheckRetainsResourcesUntilRecovery(t *testing.T) {
	e, op, a, c, _ := assessmentFixture(t, "")
	a.Directory = filepath.Join(e.Dir, "task-cancelled")
	a.Phase = "verifying"
	e.State.Attempts = append(e.State.Attempts, &a)
	dir := filepath.Join(a.Directory, "checks", c.Commit)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Candidate, 1)
	go func() { done <- e.verify(ctx, op, a, c, Plan{Setup: resourceSetupFixture, Verify: "sleep 30"}) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, resourceCleanupFile)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("setup did not register cleanup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled check did not stop")
	}
	e.sweepResources()
	if _, err := os.Stat(filepath.Join(dir, "database")); err != nil {
		t.Fatal("cancelled check lost its retained database")
	}
	var lease ResourceLease
	if err := readJSON(filepath.Join(dir, resourceLeaseFile), &lease); err != nil || lease.Status != "retained" {
		t.Fatalf("missing retained ownership: %+v %v", lease, err)
	}
	// Recovery completes the retained task without repeating implementation.
	a.Phase = "done"
	e.sweepResources()
	if _, err := os.Stat(filepath.Join(dir, "database")); !os.IsNotExist(err) {
		t.Fatal("completed recovery retained its database")
	}
}

func TestCleanupFailureIsDurableAndRetries(t *testing.T) {
	e, op, a, c, _ := assessmentFixture(t, "")
	a.Directory = filepath.Join(e.Dir, "task-cleanup-error")
	dir := filepath.Join(a.Directory, "checks", c.Commit)
	setup := resourceSetupFixture + `printf 'test -f "$LOOPER_ARTIFACT_DIR/unlock"\nrm -f "$LOOPER_ARTIFACT_DIR/database"\n' > "$LOOPER_RESOURCE_CLEANUP_FILE"`
	result := e.verify(context.Background(), op, a, c, Plan{Setup: setup, Verify: "true"})
	if !result.OK {
		t.Fatal("cleanup failure changed passing verification")
	}
	var lease ResourceLease
	if err := readJSON(filepath.Join(dir, resourceLeaseFile), &lease); err != nil || lease.Status != "cleanup_pending" || lease.Error == "" {
		t.Fatalf("cleanup failure was not retained: %+v %v", lease, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "unlock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	e.sweepResources()
	if _, err := os.Stat(filepath.Join(dir, "database")); !os.IsNotExist(err) {
		t.Fatal("restart did not retry cleanup")
	}
}

func TestRecoveryPreservesLiveOwnerAndUnfinishedWorker(t *testing.T) {
	e, _ := idleEngine(t, reviewComplete)
	a := &Attempt{ID: "task-retained", Directory: filepath.Join(e.Dir, "task-retained"), Phase: "resume"}
	e.State.Attempts = append(e.State.Attempts, a)
	dir := filepath.Join(a.Directory, "work-old")
	ctx, finish, err := beginResourceOperation(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = runShell(ctx, dir, dir, "setup", resourceSetupFixture, resourceEnv(dir)); err != nil {
		t.Fatal(err)
	}
	e.sweepResources()
	if _, err = os.Stat(filepath.Join(dir, "database")); err != nil {
		t.Fatal("recovery deleted resources of a live owner")
	}
	var lease ResourceLease
	if err = readJSON(filepath.Join(dir, resourceLeaseFile), &lease); err != nil {
		t.Fatal(err)
	}
	lease.Identity = "previous process with reused PID"
	if err = writeJSON(filepath.Join(dir, resourceLeaseFile), lease); err != nil {
		t.Fatal(err)
	}
	e.sweepResources()
	if _, err = os.Stat(filepath.Join(dir, "database")); err != nil {
		t.Fatal("recovery deleted an unfinished worker's database")
	}
	a.Phase = "done"
	e.sweepResources()
	if _, err = os.Stat(filepath.Join(dir, "database")); !os.IsNotExist(err) {
		t.Fatal("recovery did not clean the departed owner's resources")
	}
	// The deferred closure is not used: this fixture simulates a hard kill.
	_ = finish
}

func TestResourceCleanupRejectsChangedOwnership(t *testing.T) {
	dir := t.TempDir()
	ctx, _, err := beginResourceOperation(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = runShell(ctx, dir, dir, "setup", resourceSetupFixture, resourceEnv(dir)); err != nil {
		t.Fatal(err)
	}
	var lease ResourceLease
	if err = readJSON(filepath.Join(dir, resourceLeaseFile), &lease); err != nil {
		t.Fatal(err)
	}
	lease.Prefix = "another operation"
	if err = cleanResourceLease(&lease); err == nil {
		t.Fatal("cleanup accepted changed ownership")
	}
	if _, err = os.Stat(filepath.Join(dir, "database")); err != nil {
		t.Fatal("cleanup changed an unowned resource")
	}
}

func TestResourceCleanupDeadlineRetainsRetry(t *testing.T) {
	dir := t.TempDir()
	_, _, err := beginResourceOperation(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, resourceCleanupFile), []byte("sleep 30\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var lease ResourceLease
	if err = readJSON(filepath.Join(dir, resourceLeaseFile), &lease); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = cleanResourceLeaseWithin(&lease, 500*time.Millisecond)
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("cleanup did not honor its deadline: %v", err)
	}
	if err = readJSON(filepath.Join(dir, resourceLeaseFile), &lease); err != nil || lease.Status != "cleanup_pending" || lease.Error == "" {
		t.Fatalf("cleanup deadline lost its retry record: %+v %v", lease, err)
	}
}

func TestResumedCheckUsesConfiguredCommands(t *testing.T) {
	e, op, a, c, _ := assessmentFixture(t, "")
	op.config.Setup = resourceSetupFixture
	op.config.Verify = `test -f "$LOOPER_ARTIFACT_DIR/database"`
	result := e.verify(context.Background(), op, a, c, Plan{Setup: "exit 4", Verify: "exit 7"})
	if !result.OK {
		t.Fatalf("resumed check ignored configured commands: %s", result.Feedback)
	}
	if _, err := os.Stat(filepath.Join(a.Directory, "checks", c.Commit, "cleanup-proof")); err != nil {
		t.Fatal("configured setup did not register cleanup")
	}
}

func TestResumedWorkerUsesConfiguredSetup(t *testing.T) {
	e, _, _ := assessRunEngine(t, "", 0)
	if err := e.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	work, err := e.Repo.Workspace(identifier("task"), e.State.Tip, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(work) })
	a := Attempt{ID: "task-configured", Task: *e.State.Backlog.Task("T1"), Agent: e.Config.Agent, Base: e.State.Tip, Worktree: work, Directory: filepath.Join(e.Dir, "task-configured"), Ref: "refs/looper-tasks/task-configured"}
	op := e.capture()
	op.config.Setup = resourceSetupFixture
	result := e.execute(context.Background(), op, a, e.State.Backlog, Plan{Setup: "exit 4"}, nil)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	proofs, err := filepath.Glob(filepath.Join(a.Directory, "work-*", "cleanup-proof"))
	if err != nil || len(proofs) != 1 {
		t.Fatal("resumed worker ignored configured setup")
	}
}
