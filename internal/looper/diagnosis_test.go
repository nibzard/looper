package looper

import (
	"context"
	"fmt"
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
	return diagnosisEngineWithAgent(t, bin)
}

// diagnosisEngineWithAgent builds the diagnosis fixture around a caller-chosen
// agent binary, so a test can gate or stream the consultation itself.
func diagnosisEngineWithAgent(t *testing.T, bin string) (*Engine, *Attempt) {
	t.Helper()
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
	// The attempt id keeps the production shape so its retained records,
	// including a diagnosis stream, sit where the dashboard expects them.
	id := identifier("task")
	dir := filepath.Join(e.Dir, id)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	a := &Attempt{ID: id, Task: todo.Tasks[0], Agent: "codex", Base: e.State.Tip, Worktree: work, Directory: dir, Ref: "refs/looper-tasks/" + id, Phase: "running", Worktrees: []string{work}}
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

// repairFeedbackFile returns the feedback file that the repair worker prompt
// in the given work directory references.
func repairFeedbackFile(t *testing.T, work string) string {
	t.Helper()
	prompt, err := os.ReadFile(filepath.Join(work, "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	marker := "Read repair feedback: "
	start := strings.Index(string(prompt), marker)
	if start < 0 {
		t.Fatalf("repair prompt has no feedback reference: %s", work+"/prompt.txt")
	}
	path := string(prompt[start+len(marker):])
	end := strings.IndexByte(path, '\n')
	if end >= 0 {
		path = path[:end]
	}
	return strings.TrimSpace(path)
}

func TestRepairFeedbackCarriesAttemptContext(t *testing.T) {
	e, a := diagnosisEngine(t, "{}")
	e.State.Plan = &Plan{}
	longReport := "first attempt report. " + strings.Repeat("verified detail ", 40)
	a.Summary = &Summary{TaskID: "T001", Status: "done", Summary: longReport, Files: []string{}, Blockers: []string{}}
	results := make(chan workerResult, 2)
	first := "Combined verification failed:\nCommand: exit 1\nError: exit status 1\nObserved output:\nexit 1\nArtifacts: /tmp/first-check/verify.log"
	if err := e.repair(context.Background(), a, first, results); err != nil {
		t.Fatal(err)
	}
	<-results
	e.wg.Wait()
	if !strings.Contains(a.Feedback, fmt.Sprintf("Repair 1 of at most %d.", e.Config.MaxRepairs)) {
		t.Fatalf("repair count missing: %q", a.Feedback)
	}
	// Agents receive the raw feedback: artifact paths stay in the saved copy.
	if !strings.Contains(a.Feedback, "Artifacts: /tmp/first-check/verify.log") {
		t.Fatalf("raw artifact path stripped from saved feedback: %q", a.Feedback)
	}
	if !strings.Contains(a.Feedback, "Earlier attempt records: "+a.Directory+".") {
		t.Fatalf("attempt directory missing: %q", a.Feedback)
	}
	report := "Latest worker report: "
	start := strings.Index(a.Feedback, report)
	if start < 0 {
		t.Fatalf("worker report missing: %q", a.Feedback)
	}
	line := a.Feedback[start+len(report):]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	if len(line) > 203 || !strings.HasSuffix(line, "...") || strings.Contains(line, longReport) {
		t.Fatalf("worker report not bounded to one line: %d %q", len(line), line)
	}
	if !strings.HasPrefix(line, "first attempt report.") {
		t.Fatalf("worker report lost its start: %q", line)
	}
	feedbackPath := repairFeedbackFile(t, repairDir(t, a, 1))
	saved, err := os.ReadFile(feedbackPath)
	if err != nil || !strings.Contains(string(saved), "Repair 1 of at most") || !strings.Contains(string(saved), "Artifacts: /tmp/first-check/verify.log") {
		t.Fatalf("repair 1 context not in work-01 feedback file: %v %s", err, feedbackPath)
	}
	// The saved feedback survives a state reload.
	loaded, err := e.Store.Load()
	if err != nil || loaded == nil || len(loaded.Attempts) != 1 || !strings.Contains(loaded.Attempts[0].Feedback, "Repair 1 of at most") {
		t.Fatalf("saved feedback lost: %v %+v", err, loaded)
	}
	// A different failure starts no diagnosis and increments the count.
	second := "Combined verification failed:\nCommand: exit 2\nError: exit status 2"
	if err = e.repair(context.Background(), a, second, results); err != nil {
		t.Fatal(err)
	}
	<-results
	e.wg.Wait()
	if a.Repairs != 2 || !strings.Contains(a.Feedback, "Repair 2 of at most") || !strings.Contains(a.Feedback, "exit 2") {
		t.Fatalf("second repair missing context: %d %q", a.Repairs, a.Feedback)
	}
	feedbackPath = repairFeedbackFile(t, repairDir(t, a, 2))
	saved, err = os.ReadFile(feedbackPath)
	if err != nil || !strings.Contains(string(saved), "Repair 2 of at most") {
		t.Fatalf("repair 2 header not in work-02 feedback file: %v", err)
	}
}

func repairDir(t *testing.T, a *Attempt, repair int) string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(a.Directory, fmt.Sprintf("work-%02d-*", repair), "prompt.txt"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("repair %d prompt missing: %v", repair, err)
	}
	return filepath.Dir(dirs[0])
}

func TestRepairFeedbackOmitsMissingWorkerReport(t *testing.T) {
	e, a := diagnosisEngine(t, "{}")
	e.State.Plan = &Plan{}
	if err := e.repair(context.Background(), a, "check failed: T001.txt missing", make(chan workerResult, 1)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.Feedback, "Repair 1 of at most") || strings.Contains(a.Feedback, "Latest worker report") {
		t.Fatalf("missing summary must omit the report line: %q", a.Feedback)
	}
	if !strings.Contains(a.Feedback, "check failed: T001.txt missing") {
		t.Fatalf("raw evidence missing: %q", a.Feedback)
	}
}

// A diagnosis consultation that fails or changes files must fall back to the
// recorded evidence: the repair still launches, no root cause is invented, and
// the attempt is marked diagnosed so the next repeat holds instead of
// consulting forever.
func TestRepairFallsBackToRecordedEvidenceWhenDiagnosisFails(t *testing.T) {
	summary := `{"task_id":"T001","status":"done","summary":"finished","files":["T001.txt"],"blockers":[],"followups":[],"unblock_when":""}`
	for _, mode := range []string{"failed consultation", "file-changing consultation"} {
		t.Run(mode, func(t *testing.T) {
			e, a := diagnosisEngine(t, summary)
			t.Setenv("LOOPER_FAKE_DIAGNOSIS", diagnosisProposal)
			if mode == "failed consultation" {
				t.Setenv("LOOPER_FAKE_DIAGNOSIS", "not json")
			} else {
				t.Setenv("LOOPER_FAKE_TOUCH", filepath.Join(a.Worktree, "touched.txt"))
			}
			e.State.Plan = &Plan{}
			feedback := "Combined verification failed:\nCommand: exit 1\nError: exit status 1"
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
				t.Fatal("a failed consultation must still mark the attempt diagnosed")
			}
			if a.Phase != "running" {
				t.Fatalf("the repair must launch instead of holding, got %s", a.Phase)
			}
			if strings.Contains(a.Feedback, "Independent diagnosis") || strings.Contains(a.Feedback, "root cause") {
				t.Fatalf("an unusable consultation must not inject a diagnosis: %q", a.Feedback)
			}
			if !strings.Contains(a.Feedback, "Fresh diagnosis required") {
				t.Fatalf("the fallback must say why it proceeded: %q", a.Feedback)
			}
			if !strings.Contains(a.Feedback, "exit status 1") {
				t.Fatalf("the recorded evidence must stay in the feedback: %q", a.Feedback)
			}
			var errorEvent bool
			for _, ev := range storedEvents(t, e) {
				if ev.Kind == "diagnosis-error" && strings.HasPrefix(ev.Message, a.Task.ID) {
					errorEvent = true
				}
			}
			if !errorEvent {
				t.Fatal("the discarded consultation must be reported as an event")
			}
		})
	}
}
