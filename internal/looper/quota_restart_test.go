package looper

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// Restart coverage for the timed quota pause (T146): the pause map is engine
// memory, so a controller stopped inside a quota window must re-arm it from
// the saved state's quota-held tasks — the typed block reason, the blocked
// attempt's agent profile, and the unblock_when promise — instead of idling
// at the generic waiting line while the window reopens and the held task
// stays blocked for an operator requeue.

// restartReset reports a provider reset far enough out to observe the held
// task, stop the first controller mid-window, and restart before the window
// reopens.
const restartReset = ` for 5 hour. Your limit will reset at $(date -d "+10 seconds" "+%Y-%m-%d %H:%M:%S")`

// quotaFirstSession runs one controller whose worker fails once with the
// reported quota window, waits until the task is held blocked with the typed
// quota reason, then stops the controller the way an operator restart does.
// It returns the config, the recorded pause event, the held task's
// unblock_when promise, and the journal high-water mark the second session
// continues from.
func quotaFirstSession(t *testing.T, reset string) (Config, Event, string, int64) {
	t.Helper()
	bin := quotaStub(t, "quota-restart", "worker", false, reset)
	c := repoConfig(t, "run")
	c.CodexBin = bin
	c.ClaudeBin = bin
	c.Poll = 40 * time.Millisecond
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "quota restart", Tasks: []Task{
		{ID: "T1", Title: "timed work", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatalf("initialize: %v", err)
	}
	e.State.Plan = &Plan{Verify: "true", Tasks: []Scope{{TaskID: "T1", Paths: []string{"."}}}}
	var contracts []string
	for _, task := range e.State.Backlog.Tasks {
		contracts = append(contracts, task.Contract())
	}
	e.State.PlanSignature = hashJSON(struct {
		Contracts     []string
		Setup, Verify string
	}{contracts, e.Config.Setup, e.Config.Verify})
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(exited)
	}()
	if !quotaWait(t, 30*time.Second, func() bool { return quotaTaskStatus(t, c, "T1") == "blocked" }, "T1 quota-held") {
		cancel()
		t.Fatalf("the worker quota window did not hold T1; status %q", quotaTaskStatus(t, c, "T1"))
	}
	var pause Event
	var high int64
	for _, ev := range quotaSnapshot(t, e) {
		high = ev.ID
		if ev.Kind == "quota" {
			pause = ev
		}
	}
	if pause.Kind != "quota" {
		cancel()
		t.Fatal("the first session journaled no quota pause event")
	}
	data, err := os.ReadFile(c.Todo)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var b Backlog
	if err = json.Unmarshal(data, &b); err != nil {
		cancel()
		t.Fatal(err)
	}
	held := b.Task("T1")
	if held == nil || held.BlockReason == nil || held.BlockReason.Code != blockCodeQuotaWindow {
		cancel()
		t.Fatalf("the held task carries no typed quota reason: %+v", held)
	}
	// Stop inside the window: no loop boundary remains to resume the work, so
	// the second session starts from exactly the state an operator restart
	// sees.
	cancel()
	<-exited
	if err = e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	return c, pause, held.UnblockWhen, high
}

// quotaSecondSession restarts the controller on the saved state the first
// session left and returns the engine and a stop function that parks it.
func quotaSecondSession(t *testing.T, c Config) (*Engine, func()) {
	t.Helper()
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatalf("restart engine: %v", err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatalf("restart initialize: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(exited)
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-exited:
		case <-time.After(testBudget(15 * time.Second)):
			t.Error("the restarted controller did not exit after cancel")
		}
		e.Store.Close()
		lock.Close()
	}
	t.Cleanup(stop)
	return e, stop
}

// quotaRestorePayload decodes the structured data one quota-restore event
// carries; requeued marks the no-recorded-time path.
func quotaRestorePayload(t *testing.T, ev Event) struct {
	Profile  string `json:"profile"`
	RetryAt  string `json:"retry_at"`
	Requeued bool   `json:"requeued"`
} {
	t.Helper()
	var data struct {
		Profile  string `json:"profile"`
		RetryAt  string `json:"retry_at"`
		Requeued bool   `json:"requeued"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("quota-restore payload of event %d: %v %s", ev.ID, err, ev.Data)
	}
	return data
}

// TestQuotaRestartResumesHeldWorkAfterWindowReopens is the T146 acceptance
// route: the controller stops inside a quota window, restarts while the
// window is still closed, waits out the recorded retry, and requeues the
// held task without an operator — and never idles at the generic
// changed-requirements line while the quota-held work remains.
func TestQuotaRestartResumesHeldWorkAfterWindowReopens(t *testing.T) {
	c, pause, promise, high := quotaFirstSession(t, restartReset)
	if !strings.HasPrefix(promise, quotaUnblockCue) {
		t.Fatalf("the held task's promise records no retry time: %q", promise)
	}
	data := quotaPausePayload(t, pause)
	retryAt, err := time.Parse(time.RFC3339, data.RetryAt)
	if err != nil {
		t.Fatalf("pause retry_at not stored: %q", data.RetryAt)
	}
	if time.Now().After(retryAt) {
		t.Skip("the first session outlived the synthetic window; rerun")
	}
	e, stop := quotaSecondSession(t, c)
	defer stop()
	if !quotaWait(t, 30*time.Second, func() bool { return quotaTaskStatus(t, c, "T1") == "done" }, "T1 done after restart") {
		stop()
		t.Fatalf("the restart stranded the quota-held task; status %q", quotaTaskStatus(t, c, "T1"))
	}
	events := quotaSnapshot(t, e)
	stop()
	var restores, resumes []Event
	for _, ev := range events {
		if ev.ID <= high {
			continue
		}
		switch ev.Kind {
		case "quota-restore":
			restores = append(restores, ev)
		case "quota-resume":
			resumes = append(resumes, ev)
		}
	}
	if len(restores) != 1 {
		t.Fatalf("quota-restore events after the restart: %d", len(restores))
	}
	r := quotaRestorePayload(t, restores[0])
	if r.Profile != c.Agent || r.Requeued {
		t.Fatalf("quota-restore payload: %+v", r)
	}
	restoredAt, err := time.Parse(time.RFC3339, r.RetryAt)
	if err != nil {
		t.Fatalf("quota-restore retry_at not stored: %q", r.RetryAt)
	}
	if !restoredAt.Equal(retryAt) {
		t.Fatalf("the restored retry %v does not keep the recorded promise %v", restoredAt, retryAt)
	}
	if len(resumes) == 0 {
		t.Fatal("no quota-resume event after the restart")
	}
	// While the held work remained, the boundary must wait on the quota
	// window, not on changed requirements: the first resume ends that wait.
	resumed := resumes[0].ID
	quotaWaits := 0
	for _, ev := range events {
		if ev.ID <= high || ev.Kind != "waiting" || ev.ID > resumed {
			continue
		}
		if strings.Contains(ev.Message, "Waiting for changed requirements") {
			t.Fatalf("the restarted controller idled at the generic waiting line while quota-held work remained: %q", ev.Message)
		}
		if strings.Contains(ev.Message, "quota window closed") && strings.Contains(ev.Message, "retries at") {
			quotaWaits++
		}
	}
	if quotaWaits == 0 {
		t.Fatal("the restarted controller never reported the closed quota window while holding the work")
	}
	attempts := 0
	for _, a := range e.State.Attempts {
		if a.Task.ID == "T1" {
			attempts++
		}
	}
	if attempts != 1 {
		t.Fatalf("the restart must resume the retained attempt, not start a new one: %d attempts", attempts)
	}
}

// TestQuotaRestartResumesWorkWhenWindowAlreadyReopened covers the recorded
// evidence shape: the controller restarts after the window already reopened,
// so the first loop boundary requeues the held task instead of waiting.
func TestQuotaRestartResumesWorkWhenWindowAlreadyReopened(t *testing.T) {
	c, pause, _, high := quotaFirstSession(t, restartReset)
	data := quotaPausePayload(t, pause)
	retryAt, err := time.Parse(time.RFC3339, data.RetryAt)
	if err != nil {
		t.Fatalf("pause retry_at not stored: %q", data.RetryAt)
	}
	if !quotaWait(t, 15*time.Second, func() bool { return time.Now().After(retryAt.Add(time.Second)) }, "the recorded window to reopen") {
		t.Fatal("the synthetic window never reopened")
	}
	e, stop := quotaSecondSession(t, c)
	defer stop()
	if !quotaWait(t, 30*time.Second, func() bool { return quotaTaskStatus(t, c, "T1") == "done" }, "T1 done after restart") {
		stop()
		t.Fatalf("the restart stranded the quota-held task; status %q", quotaTaskStatus(t, c, "T1"))
	}
	events := quotaSnapshot(t, e)
	stop()
	var restores []Event
	resumes := 0
	for _, ev := range events {
		if ev.ID <= high {
			continue
		}
		if ev.Kind == "quota-restore" {
			restores = append(restores, ev)
		}
		if ev.Kind == "quota-resume" {
			resumes++
		}
	}
	if len(restores) != 1 {
		t.Fatalf("quota-restore events after the restart: %d", len(restores))
	}
	r := quotaRestorePayload(t, restores[0])
	restoredAt, err := time.Parse(time.RFC3339, r.RetryAt)
	if err != nil {
		t.Fatalf("quota-restore retry_at not stored: %q", r.RetryAt)
	}
	if !restoredAt.Equal(retryAt) {
		t.Fatalf("the restored retry %v must keep the recorded promise %v even though it already passed", restoredAt, retryAt)
	}
	if resumes == 0 {
		t.Fatal("the reopened window was not resumed at the restart's first boundary")
	}
}

// TestQuotaRestartRequeuesHoldWithoutRecordedTime covers a provider that
// reported no reset: the promise names no time, so the saved state cannot say
// whether the window is still closed. The restart requeues the held task, and
// a still-closed window re-arms from the provider's next failure.
func TestQuotaRestartRequeuesHoldWithoutRecordedTime(t *testing.T) {
	c, _, promise, high := quotaFirstSession(t, "")
	if promise != "Looper retries this task automatically when the provider window reopens." {
		t.Fatalf("the held task's promise: %q", promise)
	}
	e, stop := quotaSecondSession(t, c)
	defer stop()
	if !quotaWait(t, 30*time.Second, func() bool { return quotaTaskStatus(t, c, "T1") == "done" }, "T1 done after restart") {
		stop()
		t.Fatalf("the restart stranded the quota-held task with no recorded time; status %q", quotaTaskStatus(t, c, "T1"))
	}
	events := quotaSnapshot(t, e)
	stop()
	for _, ev := range events {
		if ev.ID <= high || ev.Kind != "quota-restore" {
			continue
		}
		r := quotaRestorePayload(t, ev)
		if r.Profile == c.Agent && r.Requeued {
			return
		}
	}
	t.Fatal("no quota-restore event recorded the requeue of the unrecorded-time hold")
}

// TestQuotaRetryFromUnblock pins the promise parse the restore relies on.
func TestQuotaRetryFromUnblock(t *testing.T) {
	cases := []struct {
		name    string
		promise string
		want    time.Time
		ok      bool
	}{
		{
			name:    "zoned promise",
			promise: "Looper retries this task automatically at 2026-11-18T11:50:00+01:00.",
			want:    time.Date(2026, 11, 18, 11, 50, 0, 0, time.FixedZone("", 3600)),
			ok:      true,
		},
		{
			name:    "utc promise",
			promise: "Looper retries this task automatically at 2026-11-18T10:50:00Z",
			want:    time.Date(2026, 11, 18, 10, 50, 0, 0, time.UTC),
			ok:      true,
		},
		{
			name:    "promise without a time",
			promise: "Looper retries this task automatically when the provider window reopens.",
		},
		{
			name:    "operator text",
			promise: "The operator grants access to the staging cluster.",
		},
		{
			name:    "cue followed by garbage",
			promise: "Looper retries this task automatically at your earliest convenience.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := quotaRetryFromUnblock(tc.promise)
			if ok != tc.ok {
				t.Fatalf("parsed = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if !got.Equal(tc.want) {
				t.Fatalf("parsed %v, want %v", got, tc.want)
			}
		})
	}
}

// TestQuotaHoldsWorkCoversHeldBlockedWork pins the boundary gate: a closed
// window that holds only blocked work — no ready task exists — still holds
// the idle boundary, and the wait message names the profile and its retry
// time instead of the generic changed-requirements line.
func TestQuotaHoldsWorkCoversHeldBlockedWork(t *testing.T) {
	c := Config{Agent: "claude", ReviewAgent: "codex"}
	task := Task{ID: "T1", Title: "held", Priority: 1, Status: "blocked",
		Blockers:    []string{"Usage limit reached"},
		UnblockWhen: "Looper retries this task automatically at " + time.Now().Add(time.Hour).Format(time.RFC3339) + ".",
		BlockReason: failureBlockReason(blockCodeQuotaWindow, blockRetryYes, "task-1")}
	e := &Engine{Config: c, State: &State{Backlog: Backlog{Version: 1, Sources: []string{}, Tasks: []Task{task}},
		Attempts: []*Attempt{{ID: "task-1", Task: task, Agent: "claude", Phase: "blocked"}}}}
	if len(e.State.Backlog.Ready()) != 0 {
		t.Fatal("the fixture must hold no ready work")
	}
	if e.quotaHoldsWork() {
		t.Fatal("an unarmed pause must not hold the boundary")
	}
	e.quotaPauses = map[string]*providerPause{"claude": {profile: "claude", retryAt: time.Now().Add(time.Hour)}}
	if !e.quotaHoldsWork() {
		t.Fatal("held blocked work alone must hold the idle boundary")
	}
	message := e.quotaWaitMessage()
	if !strings.Contains(message, "claude retries at") || !strings.Contains(message, "resumes automatically") {
		t.Fatalf("quota wait message: %q", message)
	}
	// A consumed pause stops holding the boundary even while the requeue is
	// pending elsewhere.
	e.quotaPauses["claude"].retryAt = time.Time{}
	if e.quotaHoldsWork() {
		t.Fatal("a consumed pause must not hold the boundary")
	}
}
