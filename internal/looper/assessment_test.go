package looper

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// assessmentFixture builds an engine, a detached worktree, and a candidate
// for direct assessment runs.
func assessmentFixture(t *testing.T, command string) (*Engine, operation, Attempt, Candidate, string) {
	t.Helper()
	e, _ := idleEngine(t, reviewComplete)
	work, err := e.Repo.Workspace(identifier("assess"), e.State.Tip, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(work) })
	tree, err := e.Repo.Snapshot(work)
	if err != nil {
		t.Fatal(err)
	}
	op := e.capture()
	op.config.Assess = command
	dir := t.TempDir()
	a := Attempt{ID: identifier("attempt"), Task: Task{ID: "T1", Title: "one", Priority: 1}, Directory: dir}
	c := Candidate{Base: e.State.Tip, Commit: e.State.Tip, Tree: tree, Worktree: work, OK: true}
	return e, op, a, c, dir
}

func TestAssessmentDeadlineStopsProcessGroup(t *testing.T) {
	// The command leaves a grandchild in the process group and blocks on it,
	// so the deadline must kill the whole group, not only the shell.
	e, op, a, c, dir := assessmentFixture(t, `sleep 30 & echo $! > "$LOOPER_ARTIFACT_DIR/child.pid"; wait`)
	start := time.Now()
	e.assessWithin(context.Background(), op, a, c, dir, 500*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("a hanging assessment outlived its deadline: %s", elapsed)
	}
	var record ProcessObservation
	if err := readJSON(filepath.Join(dir, "assessment.log.process.json"), &record); err != nil {
		t.Fatal(err)
	}
	if record.Running || record.Ended == "" {
		t.Fatalf("the supervised process must record its end: %+v", record)
	}
	if err := syscall.Kill(record.PID, 0); err == nil {
		t.Fatalf("the assessment process survived the deadline: %d", record.PID)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(child, 0); err == nil {
		t.Fatalf("a process-group member survived the deadline: %d", child)
	}
	var advisory map[string]string
	if err := readJSON(filepath.Join(dir, "assessment-error.json"), &advisory); err != nil {
		t.Fatal(err)
	}
	if advisory["command"] == "" || advisory["log"] == "" {
		t.Fatalf("the advisory error must name the command and log: %v", advisory)
	}
	if _, err := os.Stat(filepath.Join(dir, "evidence.json")); err != nil {
		t.Fatal("the evidence packet must remain beside the checks")
	}
}

func TestAssessmentCancellationIsNotAdvisory(t *testing.T) {
	e, op, a, c, dir := assessmentFixture(t, "sleep 30")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.assessWithin(ctx, op, a, c, dir, time.Minute)
	if _, err := os.Stat(filepath.Join(dir, "assessment-error.json")); !os.IsNotExist(err) {
		t.Fatal("controller cancellation must not become an advisory timeout record")
	}
}

func TestAssessmentKeepsEvidenceOnSuccess(t *testing.T) {
	e, op, a, c, dir := assessmentFixture(t, `echo '{"outcome":"pass"}' > "$LOOPER_ASSESS_REPORT"`)
	e.assessWithin(context.Background(), op, a, c, dir, 30*time.Second)
	if _, err := os.Stat(filepath.Join(dir, "assessment-error.json")); !os.IsNotExist(err) {
		t.Fatal("a successful assessment must not record an advisory error")
	}
	for _, name := range []string{"evidence.json", "assessment.json", "assessment.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s must be retained: %v", name, err)
		}
	}
}

// assessRunEngine builds a one-task finite run around a real agent stub, so
// the whole plan, implement, check, assess, and review pipeline executes.
func assessRunEngine(t *testing.T, assess string, deadline time.Duration) (*Engine, Config, func()) {
	t.Helper()
	bin, _ := fakeAgent(t, "{}")
	t.Setenv("LOOPER_FAKE_PLAN", `{"setup":"","verify":"true","tasks":[{"task_id":"T1","write_paths":["."],"resources":[]}]}`)
	t.Setenv("LOOPER_FAKE_SUMMARY", `{"task_id":"T1","status":"done","summary":"implemented","files":["T1.txt"],"blockers":[]}`)
	t.Setenv("LOOPER_FAKE_REVIEW", reviewComplete)
	c := repoConfig(t, "run")
	c.CodexBin = bin
	c.ClaudeBin = bin
	c.Assess = assess
	c.Once = true
	// A tracked file makes assessment mutations observable.
	if err := os.WriteFile(filepath.Join(c.Root, "seed.txt"), []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "add", "seed.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "commit", "-qm", "fixture"); err != nil {
		t.Fatal(err)
	}
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "assess", Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	e.assessDeadline = deadline
	closed := false
	shutdown := func() {
		if closed {
			return
		}
		closed = true
		e.Store.Close()
		lock.Close()
	}
	t.Cleanup(shutdown)
	return e, c, shutdown
}

func firstAssessmentArtifact(t *testing.T, dir, name string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(dir, "task-*", "checks", "*", name))
		if len(matches) > 0 {
			return matches[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ""
}

func TestRunPublishesAfterAssessmentTimeout(t *testing.T) {
	e, _, _ := assessRunEngine(t, "sleep 30", 500*time.Millisecond)
	code, err := e.Run(context.Background())
	if code != 0 || err != nil {
		t.Fatalf("an unchanged candidate must publish after an assessment timeout: %d %v", code, err)
	}
	if !e.State.Finished {
		t.Fatal("the run must finish after an advisory assessment timeout")
	}
	advisory := firstAssessmentArtifact(t, e.Dir, "assessment-error.json")
	if advisory == "" {
		t.Fatal("the advisory timeout record must remain available for inspection")
	}
	var record map[string]string
	if err = readJSON(advisory, &record); err != nil {
		t.Fatal(err)
	}
	if record["command"] != "sleep 30" || record["log"] == "" {
		t.Fatalf("the advisory record must name the command and log: %v", record)
	}
}

func TestRunNonzeroAssessmentExitStillPublishes(t *testing.T) {
	e, _, _ := assessRunEngine(t, "exit 3", 30*time.Second)
	code, err := e.Run(context.Background())
	if code != 0 || err != nil {
		t.Fatalf("a nonzero assessment exit must stay advisory: %d %v", code, err)
	}
	if !e.State.Finished {
		t.Fatal("a passing result must publish despite an advisory failure")
	}
	if firstAssessmentArtifact(t, e.Dir, "assessment-error.json") == "" {
		t.Fatal("the advisory error record must be retained")
	}
}

func TestRunAssessmentMutationCannotPublish(t *testing.T) {
	e, c, _ := assessRunEngine(t, "echo changed > seed.txt; sleep 30", 500*time.Millisecond)
	code, _ := e.Run(context.Background())
	if code != 2 {
		t.Fatalf("a mutating assessment must hold the task instead of publishing: %d", code)
	}
	if raw, err := os.ReadFile(filepath.Join(c.Root, "seed.txt")); err != nil || string(raw) != "original\n" {
		t.Fatalf("the project file must stay unchanged: %q %v", raw, err)
	}
	if e.State.Finished {
		t.Fatal("a mutating assessment must not finish the goal")
	}
}

func TestRunWithoutAssessmentAddsNoProcess(t *testing.T) {
	e, _, _ := assessRunEngine(t, "", 500*time.Millisecond)
	code, err := e.Run(context.Background())
	if code != 0 || err != nil {
		t.Fatalf("an empty assessment command must not disturb the run: %d %v", code, err)
	}
	matches, _ := filepath.Glob(filepath.Join(e.Dir, "task-*", "checks", "*", "assessment.log"))
	if len(matches) != 0 {
		t.Fatalf("an empty assessment command must add no process: %v", matches)
	}
}

func TestCancelDuringAssessmentResumesVerification(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "runs.log")
	t.Setenv("LOOPER_FAKE_LOG", logPath)
	e, c, shutdown := assessRunEngine(t, "sleep 30", time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		code, _ := e.Run(ctx)
		done <- code
	}()
	if artifact := firstAssessmentArtifact(t, e.Dir, "assessment.log"); artifact == "" {
		t.Fatal("the assessment never started")
	}
	cancel()
	select {
	case code := <-done:
		if code != 130 {
			t.Fatalf("cancellation during the assessment must return 130, got %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not exit after cancellation")
	}
	shutdown()
	// Restart completes verification and publication without a second
	// implementation run.
	c.Assess = ""
	restarted, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Store.Close(); lock.Close() })
	code, err := restarted.Run(context.Background())
	if code != 0 || err != nil {
		t.Fatalf("restart must complete the retained verification: %d %v", code, err)
	}
	if !restarted.State.Finished {
		t.Fatal("restart must finish the goal")
	}
	if len(restarted.State.Attempts) != 1 || restarted.State.Attempts[0].Phase != "done" {
		t.Fatalf("restart must reuse the retained attempt: %+v", restarted.State.Attempts)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	workers := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "worker" {
			workers++
		}
	}
	if workers != 1 {
		t.Fatalf("implementation must not repeat after cancellation: %d worker runs", workers)
	}
}
