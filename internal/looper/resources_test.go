package looper

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resourceSetupFixture follows the resource contract: it registers the
// cleanup script before creating the external resource. A setup that
// created resources first would lose them to a cancellation during
// registration.
const resourceSetupFixture = `set -e
cat > "${LOOPER_RESOURCE_CLEANUP_FILE:-$LOOPER_ARTIFACT_DIR/cleanup-resources.sh}" <<'CLEANUP'
test -f "$LOOPER_ARTIFACT_DIR/database"
rm "$LOOPER_ARTIFACT_DIR/database"
echo cleaned > "$LOOPER_ARTIFACT_DIR/cleanup-proof"
CLEANUP
touch "$LOOPER_ARTIFACT_DIR/database"
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
	// The database is the setup's last write, so waiting for it also proves
	// the cleanup registration is complete: a cancellation can no longer tear
	// the registered script between its creation and its content.
	deadline := time.Now().Add(testBudget(10 * time.Second))
	for {
		if _, err := os.Stat(filepath.Join(dir, "database")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("setup did not create its resource")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(testBudget(10 * time.Second)):
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
		var lease ResourceLease
		leaseErr := readJSON(filepath.Join(dir, resourceLeaseFile), &lease)
		script, _ := os.ReadFile(filepath.Join(dir, resourceCleanupFile))
		t.Fatalf("completed recovery retained its database: lease status %q error %q (read %v), registered cleanup script %q",
			lease.Status, lease.Error, leaseErr, script)
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

// cleanupEventsOfKind returns the journaled events of one cleanup kind.
func cleanupEventsOfKind(t *testing.T, e *Engine, kind string) []Event {
	t.Helper()
	var out []Event
	for _, ev := range eventsOf(t, e) {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// cleanupFailureSummaries returns the bounded failure summaries the engine
// journaled for resource cleanup, so a test can prove the structured event
// does not duplicate every retry either.
func cleanupFailureSummaries(t *testing.T, e *Engine) []FailureSummary {
	t.Helper()
	page, err := e.Store.EventPage(EventQuery{Kind: failureEventKind, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	var out []FailureSummary
	for _, ev := range page.Items {
		if s, _ := failurePayloadOf(t, ev); s.Operation == opCleanup {
			out = append(out, s)
		}
	}
	return out
}

// TestRepeatedCleanupFailuresGroupIntoOneWarningAndSummary covers T138: an
// artifact whose cleanup fails identically at every retry journals one notice
// and one bounded failure summary, not one of each per retry, and the
// session's closing sweep states the repeat count in one summary.
func TestRepeatedCleanupFailuresGroupIntoOneWarningAndSummary(t *testing.T) {
	e, _ := diagnosisEngine(t, diagnosisProposal)
	dir := filepath.Join(e.Dir, "check-cleanup-repeat")
	cleanupLeaseFixture(t, dir, "exit 4\n")
	for i := 0; i < 4; i++ {
		e.sweepResources()
	}
	notices := cleanupEventsOfKind(t, e, "resource-cleanup-error")
	if len(notices) != 1 {
		t.Fatalf("identical repeats must keep one notice, got %d: %v", len(notices), notices)
	}
	if !strings.Contains(notices[0].Message, "Temporary resources remain in "+dir) || !strings.Contains(notices[0].Message, "Cleanup retries at the next completion or restart.") {
		t.Fatalf("the initial warning must keep its message: %s", notices[0].Message)
	}
	if failures := cleanupFailureSummaries(t, e); len(failures) != 1 {
		t.Fatalf("identical repeats must keep one failure summary, got %v", failures)
	}
	if summaries := cleanupEventsOfKind(t, e, "resource-cleanup-summary"); len(summaries) != 0 {
		t.Fatalf("repeats below the interval stay silent before shutdown: %v", summaries)
	}
	// The lease kept retrying: every sweep re-ran the failing script.
	var lease ResourceLease
	if err := readJSON(filepath.Join(dir, resourceLeaseFile), &lease); err != nil || lease.Status != "cleanup_pending" {
		t.Fatalf("grouping must not stop the retries: %+v %v", lease, err)
	}
	// The closing sweep states the repeats that never reached the interval:
	// four identical failures, the first announced by the warning itself.
	e.summarizeCleanupFailures()
	summaries := cleanupEventsOfKind(t, e, "resource-cleanup-summary")
	if len(summaries) != 1 || !strings.Contains(summaries[0].Message, "4 identical cleanup failures this session") {
		t.Fatalf("the closing summary must state the repeat count: %v", summaries)
	}
	if !strings.Contains(summaries[0].Message, dir) {
		t.Fatalf("the closing summary must name the artifact: %s", summaries[0].Message)
	}
	// A second closing sweep reports nothing new.
	e.summarizeCleanupFailures()
	if summaries = cleanupEventsOfKind(t, e, "resource-cleanup-summary"); len(summaries) != 1 {
		t.Fatalf("a summary without new repeats would itself be noise: %v", summaries)
	}
}

// TestCleanupFailureIntervalSummaryBoundsTheSilence covers the interval half
// of T138: after a failure was reported in full, the first interval of
// identical repeats journals one summary with the count, and the closing
// sweep has nothing left to add.
func TestCleanupFailureIntervalSummaryBoundsTheSilence(t *testing.T) {
	e, _ := diagnosisEngine(t, diagnosisProposal)
	cleanupLeaseFixture(t, filepath.Join(e.Dir, "check-cleanup-interval"), "exit 4\n")
	for i := 0; i < cleanupSummaryInterval+1; i++ {
		e.sweepResources()
	}
	e.summarizeCleanupFailures()
	if notices := cleanupEventsOfKind(t, e, "resource-cleanup-error"); len(notices) != 1 {
		t.Fatalf("the interval must not re-issue the warning, got %d", len(notices))
	}
	summaries := cleanupEventsOfKind(t, e, "resource-cleanup-summary")
	if len(summaries) != 1 || !strings.Contains(summaries[0].Message, fmt.Sprintf("%d identical cleanup failures this session", cleanupSummaryInterval+1)) {
		t.Fatalf("the interval summary must state the repeat count: %v", summaries)
	}
}

// TestCleanupFailureGroupsPerArtifactAndError covers the two ways a repeat
// stops being identical (T138): another artifact fails on its own, and the
// same artifact's error changes, which reports at once because a changed
// cause is new information.
func TestCleanupFailureGroupsPerArtifactAndError(t *testing.T) {
	e, _ := diagnosisEngine(t, diagnosisProposal)
	first := filepath.Join(e.Dir, "check-cleanup-one")
	second := filepath.Join(e.Dir, "check-cleanup-two")
	cleanupLeaseFixture(t, first, "exit 4\n")
	cleanupLeaseFixture(t, second, "exit 4\n")
	for i := 0; i < 3; i++ {
		e.sweepResources()
	}
	if notices := cleanupEventsOfKind(t, e, "resource-cleanup-error"); len(notices) != 2 {
		t.Fatalf("a different artifact keeps its own warning, got %d: %v", len(notices), notices)
	}
	if failures := cleanupFailureSummaries(t, e); len(failures) != 2 {
		t.Fatalf("a different artifact keeps its own failure summary, got %v", failures)
	}
	e.summarizeCleanupFailures()
	summaries := cleanupEventsOfKind(t, e, "resource-cleanup-summary")
	if len(summaries) != 2 {
		t.Fatalf("each artifact's repeats close with their own summary, got %v", summaries)
	}
	for _, dir := range []string{first, second} {
		found := false
		for _, ev := range summaries {
			if strings.Contains(ev.Message, dir) && strings.Contains(ev.Message, "3 identical cleanup failures this session") {
				found = true
			}
		}
		if !found {
			t.Fatalf("artifact %s must carry its own count: %v", dir, summaries)
		}
	}

	// The same artifact's error changes: the new cause reports at once, in
	// both the notice and the failure summary, and its repeats group behind it.
	if err := os.WriteFile(filepath.Join(first, resourceCleanupFile), []byte("exit 5\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e.sweepResources()
	e.sweepResources()
	if notices := cleanupEventsOfKind(t, e, "resource-cleanup-error"); len(notices) != 3 {
		t.Fatalf("a changed error must report immediately, got %d notices", len(notices))
	} else if !strings.Contains(notices[2].Message, "exit status 5") {
		t.Fatalf("the changed-error warning must carry the new cause: %s", notices[2].Message)
	}
	failures := cleanupFailureSummaries(t, e)
	if len(failures) != 3 {
		t.Fatalf("a changed error must journal its own summary, got %v", failures)
	}
	exitFour, exitFive := false, false
	for _, s := range failures {
		exitFour = exitFour || strings.Contains(s.Cause, "exit status 4")
		exitFive = exitFive || strings.Contains(s.Cause, "exit status 5")
	}
	if !exitFour || !exitFive {
		t.Fatalf("the summaries must distinguish the causes: %v", failures)
	}
	e.summarizeCleanupFailures()
	// The first artifact's new cause repeated twice; the second artifact's
	// original cause was already summarized above and stays quiet.
	var changed int
	for _, ev := range cleanupEventsOfKind(t, e, "resource-cleanup-summary") {
		if strings.Contains(ev.Message, first) && strings.Contains(ev.Message, "exit status 5") {
			changed++
			if !strings.Contains(ev.Message, "2 identical cleanup failures this session") {
				t.Fatalf("the new cause's count restarts at its first failure: %s", ev.Message)
			}
		}
	}
	if changed != 1 {
		t.Fatalf("the changed error must close with one fresh summary, got %d", changed)
	}
}

// TestCleanupSuccessClearsLeaseAndResetsGrouping covers the retry half of
// T138: a failing cleanup keeps retrying at every sweep, a later success
// clears the lease exactly as before, and a new failure of the same artifact
// is a new episode that reports on its own.
func TestCleanupSuccessClearsLeaseAndResetsGrouping(t *testing.T) {
	e, _ := diagnosisEngine(t, diagnosisProposal)
	dir := filepath.Join(e.Dir, "check-cleanup-recovered")
	cleanupLeaseFixture(t, dir, "test -f \"$LOOPER_ARTIFACT_DIR/unlock\"\nrm -f \"$LOOPER_ARTIFACT_DIR/database\"\n")
	if err := os.WriteFile(filepath.Join(dir, "database"), []byte("private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e.sweepResources()
	if notices := cleanupEventsOfKind(t, e, "resource-cleanup-error"); len(notices) != 1 {
		t.Fatalf("the locked cleanup must warn once, got %d", len(notices))
	}
	// Restoring the missing dependency lets the retry succeed.
	if err := os.WriteFile(filepath.Join(dir, "unlock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	e.sweepResources()
	if notices := cleanupEventsOfKind(t, e, "resource-cleanup-error"); len(notices) != 1 {
		t.Fatal("a successful retry must not warn")
	}
	if _, err := os.Stat(filepath.Join(dir, "database")); !os.IsNotExist(err) {
		t.Fatal("the successful retry did not remove the resource")
	}
	var lease ResourceLease
	if err := readJSON(filepath.Join(dir, resourceLeaseFile), &lease); err != nil || lease.Status != "cleaned" {
		t.Fatalf("a later success must clear the lease as before: %+v %v", lease, err)
	}
	e.summarizeCleanupFailures()
	if summaries := cleanupEventsOfKind(t, e, "resource-cleanup-summary"); len(summaries) != 0 {
		t.Fatalf("a cleared artifact has no closing summary, got %v", summaries)
	}
	// A new lease in the same artifact directory fails again: the success
	// ended the previous group, so the new failure reports in full.
	os.Remove(filepath.Join(dir, "unlock"))
	cleanupLeaseFixture(t, dir, "test -f \"$LOOPER_ARTIFACT_DIR/unlock\"\nrm -f \"$LOOPER_ARTIFACT_DIR/database\"\n")
	if err := os.WriteFile(filepath.Join(dir, "database"), []byte("private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e.sweepResources()
	if notices := cleanupEventsOfKind(t, e, "resource-cleanup-error"); len(notices) != 2 {
		t.Fatalf("a new episode must report on its own, got %d notices", len(notices))
	}
}

// TestRunSummarizesRepeatedCleanupFailuresAtShutdown covers the shutdown half
// of T138 through the real run path: a lease that fails cleanup at every
// boundary of a session journals one notice and one failure summary, and the
// run's closing sweep states the repeat count once.
func TestRunSummarizesRepeatedCleanupFailuresAtShutdown(t *testing.T) {
	e, _, _ := assessRunEngine(t, "", 0)
	dir := filepath.Join(e.Dir, "check-cleanup-run")
	cleanupLeaseFixture(t, dir, "exit 4\n")
	code, err := e.Run(context.Background())
	if code != 0 || err != nil {
		t.Fatalf("run failed beside a failing cleanup lease: %d %v", code, err)
	}
	if notices := cleanupEventsOfKind(t, e, "resource-cleanup-error"); len(notices) != 1 {
		t.Fatalf("a whole session must keep one notice, got %d: %v", len(notices), notices)
	}
	if failures := cleanupFailureSummaries(t, e); len(failures) != 1 {
		t.Fatalf("a whole session must keep one failure summary, got %v", failures)
	}
	summaries := cleanupEventsOfKind(t, e, "resource-cleanup-summary")
	if len(summaries) != 1 {
		t.Fatalf("the shutdown sweep must state the repeats once, got %v", summaries)
	}
	if !strings.Contains(summaries[0].Message, dir) || !strings.Contains(summaries[0].Message, "identical cleanup failures this session") {
		t.Fatalf("the shutdown summary must name the artifact and its count: %s", summaries[0].Message)
	}
	var lease ResourceLease
	if err := readJSON(filepath.Join(dir, resourceLeaseFile), &lease); err != nil || lease.Status != "cleanup_pending" {
		t.Fatalf("the failing lease must stay pending for the next session: %+v %v", lease, err)
	}
}

// unreadableLeaseFixture leaves one ownership record that cannot parse, the
// way an interrupted external write or corruption leaves it, and returns the
// record path the sweep names in its warning.
func unreadableLeaseFixture(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, resourceLeaseFile)
	if err := os.WriteFile(path, []byte("not an ownership record\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// invalidLeaseFixture writes one ownership record that parses but fails
// validation, the way a hand edit from another operation does.
func invalidLeaseFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	lease := ResourceLease{Version: 2, Directory: dir, PID: os.Getpid(), Identity: "test identity", Status: "retained"}
	if err := writeJSON(filepath.Join(dir, resourceLeaseFile), lease); err != nil {
		t.Fatal(err)
	}
}

// TestRepeatedLeaseReadWarningsGroupIntoOneWarningAndSummary covers T143: an
// ownership record that stays unreadable across sweep boundaries journals one
// notice, not one per boundary, and the session's closing sweep states the
// repeat count in one summary. Read warnings carry no structured failure
// event, so none appears beside the notice.
func TestRepeatedLeaseReadWarningsGroupIntoOneWarningAndSummary(t *testing.T) {
	e, _ := diagnosisEngine(t, diagnosisProposal)
	dir := filepath.Join(e.Dir, "check-lease-read")
	path := unreadableLeaseFixture(t, dir)
	for i := 0; i < 4; i++ {
		e.sweepResources()
	}
	notices := cleanupEventsOfKind(t, e, "resource-cleanup-error")
	if len(notices) != 1 {
		t.Fatalf("identical repeats must keep one notice, got %d: %v", len(notices), notices)
	}
	if !strings.Contains(notices[0].Message, "Cannot read resource ownership in "+path) {
		t.Fatalf("the initial warning must keep its message: %s", notices[0].Message)
	}
	if failures := cleanupFailureSummaries(t, e); len(failures) != 0 {
		t.Fatalf("a read warning must not journal a structured failure summary: %v", failures)
	}
	if summaries := cleanupEventsOfKind(t, e, "resource-cleanup-summary"); len(summaries) != 0 {
		t.Fatalf("repeats below the interval stay silent before shutdown: %v", summaries)
	}
	// The closing sweep states the repeats that never reached the interval:
	// four identical warnings, the first announced by the notice itself.
	e.summarizeCleanupFailures()
	summaries := cleanupEventsOfKind(t, e, "resource-cleanup-summary")
	if len(summaries) != 1 || !strings.Contains(summaries[0].Message, "4 identical warnings this session") {
		t.Fatalf("the closing summary must state the repeat count: %v", summaries)
	}
	if !strings.Contains(summaries[0].Message, dir) || !strings.Contains(summaries[0].Message, "Cannot read resource ownership in "+dir) {
		t.Fatalf("the closing summary must name the artifact and its problem: %s", summaries[0].Message)
	}
	// A second closing sweep reports nothing new.
	e.summarizeCleanupFailures()
	if summaries = cleanupEventsOfKind(t, e, "resource-cleanup-summary"); len(summaries) != 1 {
		t.Fatalf("a summary without new repeats would itself be noise: %v", summaries)
	}
}

// TestRepeatedLeaseValidateWarningsGroupAndRecover covers the validate half of
// T143 and its recovery: an ownership record that stays invalid journals one
// notice with the unchanged validation error, and a record that reads and
// validates again drops its group, so a later problem of the same artifact is
// a new episode that reports on its own.
func TestRepeatedLeaseValidateWarningsGroupAndRecover(t *testing.T) {
	e, _ := diagnosisEngine(t, diagnosisProposal)
	dir := filepath.Join(e.Dir, "check-lease-invalid")
	invalidLeaseFixture(t, dir)
	for i := 0; i < 3; i++ {
		e.sweepResources()
	}
	notices := cleanupEventsOfKind(t, e, "resource-cleanup-error")
	if len(notices) != 1 || !strings.Contains(notices[0].Message, "invalid resource ownership record") {
		t.Fatalf("the unchanged validation error must warn once, got %v", notices)
	}
	if failures := cleanupFailureSummaries(t, e); len(failures) != 0 {
		t.Fatalf("a validate warning must not journal a structured failure summary: %v", failures)
	}
	e.summarizeCleanupFailures()
	summaries := cleanupEventsOfKind(t, e, "resource-cleanup-summary")
	if len(summaries) != 1 || !strings.Contains(summaries[0].Message, "3 identical warnings this session") {
		t.Fatalf("the closing summary must state the repeat count: %v", summaries)
	}
	if !strings.Contains(summaries[0].Message, "Cannot validate resource ownership in "+dir) {
		t.Fatalf("the closing summary must name its problem: %s", summaries[0].Message)
	}
	// The record recovers into a valid lease the sweep can clean: the group
	// drops when the record validates again, and the recovered episode never
	// reaches another notice or summary.
	cleanupLeaseFixture(t, dir, "true\n")
	e.sweepResources()
	if notices = cleanupEventsOfKind(t, e, "resource-cleanup-error"); len(notices) != 1 {
		t.Fatalf("a recovered record must not warn again, got %v", notices)
	}
	var lease ResourceLease
	if err := readJSON(filepath.Join(dir, resourceLeaseFile), &lease); err != nil || lease.Status != "cleaned" {
		t.Fatalf("the recovered record must clean as before: %+v %v", lease, err)
	}
	e.summarizeCleanupFailures()
	if summaries = cleanupEventsOfKind(t, e, "resource-cleanup-summary"); len(summaries) != 1 {
		t.Fatalf("a recovered artifact has no new closing summary, got %v", summaries)
	}
	// A new problem of the same artifact reports in full: the success ended
	// the previous episode.
	unreadableLeaseFixture(t, dir)
	e.sweepResources()
	if notices = cleanupEventsOfKind(t, e, "resource-cleanup-error"); len(notices) != 2 {
		t.Fatalf("a new episode must report on its own, got %d notices", len(notices))
	}
}

// TestLeaseWarningIntervalAndChangedCause covers the remaining T143 bounds:
// the first interval of identical read repeats journals one summary with the
// count, and a record whose problem changes reports at once because a changed
// cause is new information.
func TestLeaseWarningIntervalAndChangedCause(t *testing.T) {
	e, _ := diagnosisEngine(t, diagnosisProposal)
	dir := filepath.Join(e.Dir, "check-lease-interval")
	unreadableLeaseFixture(t, dir)
	for i := 0; i < cleanupSummaryInterval+1; i++ {
		e.sweepResources()
	}
	e.summarizeCleanupFailures()
	if notices := cleanupEventsOfKind(t, e, "resource-cleanup-error"); len(notices) != 1 {
		t.Fatalf("the interval must not re-issue the warning, got %d notices", len(notices))
	}
	summaries := cleanupEventsOfKind(t, e, "resource-cleanup-summary")
	if len(summaries) != 1 || !strings.Contains(summaries[0].Message, fmt.Sprintf("%d identical warnings this session", cleanupSummaryInterval+1)) {
		t.Fatalf("the interval summary must state the repeat count: %v", summaries)
	}
	// The record changes from unreadable to invalid: the changed cause reports
	// at once, and its repeats group behind it.
	invalidLeaseFixture(t, dir)
	e.sweepResources()
	e.sweepResources()
	notices := cleanupEventsOfKind(t, e, "resource-cleanup-error")
	if len(notices) != 2 || !strings.Contains(notices[1].Message, "invalid resource ownership record") {
		t.Fatalf("a changed problem must report immediately, got %v", notices)
	}
	e.summarizeCleanupFailures()
	// The changed cause repeated twice — its first failure was announced by
	// the notice itself — so it closes with its own count while the read
	// cause was already summarized at the interval.
	summaries = cleanupEventsOfKind(t, e, "resource-cleanup-summary")
	if len(summaries) != 2 || !strings.Contains(summaries[1].Message, "2 identical warnings this session") {
		t.Fatalf("the changed problem must close with its own count: %v", summaries)
	}
	if !strings.Contains(summaries[1].Message, "Cannot validate resource ownership in "+dir) {
		t.Fatalf("the changed problem's summary must name its problem: %s", summaries[1].Message)
	}
}

// TestRunSummarizesRepeatedLeaseReadWarningsAtShutdown covers the shutdown
// half of T143 through the real run path: an ownership record that stays
// unreadable across a whole session journals one notice, and the run's
// closing sweep states the repeat count once.
func TestRunSummarizesRepeatedLeaseReadWarningsAtShutdown(t *testing.T) {
	e, _, _ := assessRunEngine(t, "", 0)
	dir := filepath.Join(e.Dir, "check-lease-run")
	unreadableLeaseFixture(t, dir)
	code, err := e.Run(context.Background())
	if code != 0 || err != nil {
		t.Fatalf("run failed beside an unreadable ownership record: %d %v", code, err)
	}
	if notices := cleanupEventsOfKind(t, e, "resource-cleanup-error"); len(notices) != 1 {
		t.Fatalf("a whole session must keep one notice, got %d: %v", len(notices), notices)
	}
	summaries := cleanupEventsOfKind(t, e, "resource-cleanup-summary")
	if len(summaries) != 1 {
		t.Fatalf("the shutdown sweep must state the repeats once, got %v", summaries)
	}
	if !strings.Contains(summaries[0].Message, dir) || !strings.Contains(summaries[0].Message, "identical warnings this session") {
		t.Fatalf("the shutdown summary must name the artifact and its count: %s", summaries[0].Message)
	}
}
