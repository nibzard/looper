package looper

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const diagnosisProposal = `{"root_cause":"the check asserts T001.txt exists","evidence":"verify.log line 1","fix_approach":"create T001.txt inside the task worktree"}`

// diagnosisEngine returns an engine whose review agent answers diagnosis
// consultations with the canned proposal, plus an attempt that owns a real
// worktree.
func diagnosisEngine(t *testing.T, result string) (*Engine, *Attempt) {
	t.Helper()
	bin, _ := fakeAgent(t, result)
	c := repoConfig(t, "run")
	c.CodexBin = bin
	c.ClaudeBin = bin
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{
		{ID: "T001", Title: "existing", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	work, err := e.Repo.Workspace(identifier("task"), e.State.Tip, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(work) })
	dir := filepath.Join(e.Dir, "task-x")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	a := &Attempt{ID: "task-x", Task: todo.Tasks[0], Agent: "codex", Base: e.State.Tip, Worktree: work, Directory: dir, Ref: "refs/looper-tasks/task-x", Phase: "running", Worktrees: []string{work}}
	e.State.Attempts = append(e.State.Attempts, a)
	return e, a
}

func TestDiagnoseReturnsIndependentOpinion(t *testing.T) {
	e, a := diagnosisEngine(t, diagnosisProposal)
	t.Setenv("LOOPER_FAKE_DIAGNOSIS", diagnosisProposal)
	d, ok := e.diagnose(context.Background(), a, "check failed: T001.txt missing")
	if !ok {
		t.Fatal("diagnosis rejected")
	}
	if !strings.Contains(d.RootCause, "T001.txt") || !strings.Contains(d.FixApproach, "worktree") {
		t.Fatalf("diagnosis incomplete: %+v", d)
	}
}

func TestDiagnoseDiscardsResultThatChangesFiles(t *testing.T) {
	e, a := diagnosisEngine(t, diagnosisProposal)
	t.Setenv("LOOPER_FAKE_DIAGNOSIS", diagnosisProposal)
	t.Setenv("LOOPER_FAKE_TOUCH", filepath.Join(a.Worktree, "touched.txt"))
	if _, ok := e.diagnose(context.Background(), a, "check failed"); ok {
		t.Fatal("a consultation that changes files must be discarded")
	}
}

func TestDiagnoseFallsBackOnInvalidResult(t *testing.T) {
	e, a := diagnosisEngine(t, "not json")
	t.Setenv("LOOPER_FAKE_DIAGNOSIS", "not json")
	if _, ok := e.diagnose(context.Background(), a, "check failed"); ok {
		t.Fatal("an unusable result must be discarded")
	}
}

func TestRepairInjectsDiagnosisOnRepeatedFailure(t *testing.T) {
	summary := `{"task_id":"T001","status":"blocked","summary":"held","files":[],"blockers":["waiting"],"followups":[],"unblock_when":"retry"}`
	e, a := diagnosisEngine(t, summary)
	t.Setenv("LOOPER_FAKE_DIAGNOSIS", diagnosisProposal)
	e.State.Plan = &Plan{}
	feedback := "check failed: T001.txt missing"
	// Pre-seed the failure history so this repair is a repeated failure.
	a.FailureHash = digest([]byte(feedback))
	a.Repeats = 1
	results := make(chan workerResult, 1)
	if err := e.repair(context.Background(), a, feedback, results); err != nil {
		t.Fatal(err)
	}
	<-results
	e.wg.Wait()
	if !a.Diagnosed {
		t.Fatal("repeated failure must mark the attempt diagnosed")
	}
	if !strings.Contains(a.Feedback, "root cause") || !strings.Contains(a.Feedback, "T001.txt exists") {
		t.Fatalf("diagnosis missing from feedback: %q", a.Feedback)
	}
	// The same failure after a diagnosis holds the task instead of retrying.
	a.ResultCommit = ""
	if err := e.repair(context.Background(), a, feedback, results); err != nil {
		t.Fatal(err)
	}
	if a.Phase != "blocked" || !strings.Contains(a.Feedback, "persists after a fresh diagnosis") {
		t.Fatalf("hold after diagnosis broken: %s %q", a.Phase, a.Feedback)
	}
}
