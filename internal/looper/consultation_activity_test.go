package looper

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// waitForEvents blocks until the journal holds an event satisfying want.
func waitForEvents(t *testing.T, e *Engine, want func([]Event) bool) []Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events, err := e.Store.Events(0)
		if err != nil {
			t.Fatal(err)
		}
		if want(events) {
			return events
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected journal evidence was not recorded")
	return nil
}

func eventsOfKind(events []Event, kind string) []Event {
	var matched []Event
	for _, e := range events {
		if e.Kind == kind {
			matched = append(matched, e)
		}
	}
	return matched
}

// A long repair diagnosis publishes the same activity evidence as ordinary
// planning: activity state, agent identity, stream path, last-output age, and
// readable agent actions, plus controller heartbeats, while the consultation
// agent is still running. The agent keeps its normal review contract.
func TestDiagnosePublishesActivityEvidence(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	argsPath := filepath.Join(t.TempDir(), "args.txt")
	bin := gatedActivityAgent(t, release, argsPath)
	t.Setenv("LOOPER_FAKE_RESULT", diagnosisProposal)
	e, a := diagnosisEngineWithAgent(t, bin)
	// One heartbeat per tick keeps the controller event observable in a
	// bounded test; production keeps the 30-second planner cadence.
	e.consultHeartbeat = time.Second
	done := make(chan bool, 1)
	go func() {
		_, ok := e.diagnose(context.Background(), a, "check failed: T001.txt missing")
		done <- ok
	}()
	v := waitForActivityView(t, e.Dir, func(v *ui.ViewSnapshot) bool {
		return v.State == "diagnosing" && v.ActivityAgent == "claude" && strings.Contains(v.ActivityLog, "diagnose-")
	})
	v = waitForActivityView(t, e.Dir, func(v *ui.ViewSnapshot) bool {
		return v.ActivityDetail == "Read to-do.json" && v.ActivityUpdated != ""
	})
	select {
	case <-done:
		t.Fatal("consultation finished before activity evidence was observed")
	default:
	}
	// The dashboard opens the consultation stream from the published path.
	log, err := plannerLog(e.Dir, v.ActivityLog)
	if err != nil || !strings.Contains(log, "Read to-do.json") {
		t.Fatalf("dashboard cannot open the diagnosis stream: %q %v", log, err)
	}
	if err = os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("the gated consultation did not return its diagnosis")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("consultation did not finish after release")
	}
	if e.err != nil {
		t.Fatal(e.err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(args, []byte("--tools")) || bytes.Contains(args, []byte("--json-schema")) {
		t.Fatalf("diagnosis inherited the planning contract: %s", args)
	}
	after := LoadView(e.Dir)
	if after == nil || after.State != "" || after.ActivityLog != "" || after.ActivityAgent != "" || after.ActivityDetail != "" || after.ActivityUpdated != "" {
		t.Fatalf("diagnosis left stale activity: %+v", after)
	}
	events := waitForEvents(t, e, func(events []Event) bool {
		return len(eventsOfKind(events, "diagnosis")) == 1
	})
	if opened := eventsOfKind(events, "diagnosis-activity"); len(opened) == 0 ||
		!strings.Contains(opened[0].Message, a.Task.ID) || !strings.Contains(opened[0].Message, "Log: ") {
		t.Fatalf("consultation start not journaled: %+v", opened)
	}
	if actions := eventsOfKind(events, "diagnosis-activity"); !eventMessage(actions, "Read to-do.json") {
		t.Fatalf("agent stream actions were not journaled: %+v", actions)
	}
	beats := eventsOfKind(events, "diagnosis-heartbeat")
	if len(beats) == 0 || !strings.Contains(beats[len(beats)-1].Message, "the repair diagnosis") {
		t.Fatalf("no controller heartbeat while the consultation ran: %+v", beats)
	}
}

func eventMessage(events []Event, text string) bool {
	for _, e := range events {
		if strings.Contains(e.Message, text) {
			return true
		}
	}
	return false
}

// The diagnosis runs on the owner goroutine while other workers may still be
// in flight. Worker evidence keeps publishing through the consultation, so a
// long diagnosis never hides parallel work.
func TestDiagnoseKeepsPublishingWorkerEvidence(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	argsPath := filepath.Join(t.TempDir(), "args.txt")
	bin := gatedActivityAgent(t, release, argsPath)
	t.Setenv("LOOPER_FAKE_RESULT", diagnosisProposal)
	e, a := diagnosisEngineWithAgent(t, bin)
	e.consultHeartbeat = time.Second
	// A parallel worker with a live stream. Nothing else polls it in this
	// test, so any journaled activity proves the consultation published it.
	wdir := filepath.Join(e.Dir, identifier("task"))
	if err := os.MkdirAll(filepath.Join(wdir, "work-00"), 0700); err != nil {
		t.Fatal(err)
	}
	stream := filepath.Join(wdir, "work-00", "events-00.jsonl")
	if err := os.WriteFile(stream, []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e.State.Attempts = append(e.State.Attempts, &Attempt{ID: filepath.Base(wdir), Task: Task{ID: "T002", Title: "parallel"}, Phase: "running", Directory: wdir})
	done := make(chan bool, 1)
	go func() {
		_, ok := e.diagnose(context.Background(), a, "check failed: T001.txt missing")
		done <- ok
	}()
	v := waitForActivityView(t, e.Dir, func(v *ui.ViewSnapshot) bool {
		return v.State == "diagnosing" && v.Counters.InFlight >= 1
	})
	if len(v.Attempts) < 2 {
		t.Fatalf("the running worker disappeared from the view: %+v", v.Attempts)
	}
	_ = waitForEvents(t, e, func(events []Event) bool {
		return eventMessage(eventsOfKind(events, "worker_activity"), "T002")
	})
	select {
	case <-done:
		t.Fatal("the consultation ended before worker evidence was published")
	default:
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("consultation did not finish after release")
	}
	if e.err != nil {
		t.Fatal(e.err)
	}
}

// A long run-recovery consultation publishes the same activity evidence, so a
// drained controller waiting on the review agent reads as a live consultation
// instead of a hang.
func TestConsultRunRepairPublishesActivityEvidence(t *testing.T) {
	e, _, p := recoveryEngine(t)
	release := filepath.Join(t.TempDir(), "release")
	argsPath := filepath.Join(t.TempDir(), "args.txt")
	bin := gatedActivityAgent(t, release, argsPath)
	t.Setenv("LOOPER_FAKE_RESULT", proposalJSON(t, p))
	e.Config.CodexBin, e.Config.ClaudeBin = bin, bin
	e.consultHeartbeat = time.Second
	done := make(chan struct{}, 1)
	go func() {
		e.recoverRun(context.Background())
		done <- struct{}{}
	}()
	v := waitForActivityView(t, e.Dir, func(v *ui.ViewSnapshot) bool {
		return v.State == "recovering" && v.ActivityAgent == "claude" && strings.Contains(v.ActivityLog, "run-repair-")
	})
	if _, err := plannerLog(e.Dir, v.ActivityLog); err != nil {
		t.Fatalf("dashboard cannot open the run-recovery stream: %v", err)
	}
	events := waitForEvents(t, e, func(events []Event) bool {
		return len(eventsOfKind(events, "run-recovery-heartbeat")) > 0
	})
	if !eventMessage(eventsOfKind(events, "run-recovery-activity"), "retained orchestration failures") {
		t.Fatalf("consultation start not journaled: %+v", eventsOfKind(events, "run-recovery-activity"))
	}
	select {
	case <-done:
		t.Fatal("consultation finished before a heartbeat was observed")
	default:
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("consultation did not finish after release")
	}
	if e.err != nil {
		t.Fatal(e.err)
	}
	after := LoadView(e.Dir)
	if after == nil || after.State != "" || after.ActivityLog != "" {
		t.Fatalf("consultation left stale activity: %+v", after)
	}
	if e.State.Backlog.Task("T001").Status != "todo" || len(e.State.Backlog.Sources) != 0 {
		t.Fatalf("recovery outcome changed: %s %v", e.State.Backlog.Task("T001").Status, e.State.Backlog.Sources)
	}
	if len(e.State.Recoveries) == 0 || e.State.Recoveries[0].Outcome != "applied" || len(e.State.Recoveries[0].Tasks) != 1 {
		t.Fatalf("recovery not applied: %+v", e.State.Recoveries)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(args, []byte("--tools")) || bytes.Contains(args, []byte("--json-schema")) {
		t.Fatalf("run recovery inherited the planning contract: %s", args)
	}
}

// The dashboard log reader opens a repair diagnosis stream inside its attempt
// directory and a run-recovery stream at the runtime root, and still rejects
// paths outside the controller's own streams.
func TestDashboardReadsConsultationLogs(t *testing.T) {
	dir := t.TempDir()
	stream := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"verify.log"}}]}}`+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	diagnosis := filepath.Join(dir, "task-0123456789abcdef", "diagnose-01-0123456789abcdef", "events-00.jsonl")
	recovery := filepath.Join(dir, "run-repair-0123456789abcdef", "events-01.jsonl")
	stream(diagnosis)
	stream(recovery)
	for _, path := range []string{diagnosis, recovery} {
		text, err := plannerLog(dir, path)
		if err != nil || !strings.Contains(text, "Read verify.log") {
			t.Fatalf("consultation log %q: %q %v", path, text, err)
		}
	}
	for _, path := range []string{
		filepath.Join(dir, "worktrees", "task-0123456789abcdef", "diagnose-01-0123456789abcdef", "events-00.jsonl"),
		filepath.Join(dir, "not-a-task", "diagnose-01-0123456789abcdef", "events-00.jsonl"),
		filepath.Join(dir, "imports", "events-00.jsonl"),
	} {
		if _, err := plannerLog(dir, path); err == nil {
			t.Fatalf("accepted a non-controller stream: %q", path)
		}
	}
}
