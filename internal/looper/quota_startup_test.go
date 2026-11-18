package looper

// T141: quota exhaustion during startup bootstrap. Bootstrap runs before
// the controller loop exists, so a closed reviewer quota window cannot gate
// on the loop's pause machinery. The start now waits out the window and
// bootstraps when it reopens: readiness reporting keeps its startup seed,
// the startup guards keep their refusals, and the operator's pause budget
// bounds the probed windows. A drain request or cancellation still ends the
// start at once, and a task file the operator supplies during the wait is
// adopted, never overwritten.

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bootstrapQuotaBacklog is the backlog the stub bootstrap returns: one task
// done from the start, so a once run needs no worker.
const bootstrapQuotaBacklog = `{"schema_version":1,"goal":"quota startup","source_files":[],"tasks":[{"id":"T001","title":"one","priority":1,"status":"done","details":"done from the start","depends_on":[],"tags":["project-done"]}]}`

// bootstrapQuotaPlan is the plan the stub returns for planning prompts.
const bootstrapQuotaPlan = `{"setup":"","verify":"true","tasks":[]}`

// bootstrapQuotaStub writes one stub agent for the fresh-project startup
// path. The bootstrap prompt answers with the done-from-start backlog,
// review prompts with a complete report, and planning prompts with a
// trivial plan. failBootstrap ("once" or "always") emits the observed quota
// rejection for bootstrap invocations before it starts answering; reset
// supplies the shell text baked into the message, like quotaStub.
func bootstrapQuotaStub(t *testing.T, name, failBootstrap, reset string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, name)
	script := `#!/bin/bash
out=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$a"; fi
  prev="$a"
done
p="$(cat)"
fail_quota() {
  msg="API Error: Request rejected (429) · [1308][Usage limit reached` + reset + `]"
  printf '%s\n' "{\"type\":\"error\",\"message\":\"$msg\"}"
  exit 1
}
result=''
if printf '%s' "$p" | grep -q "Bootstrap a development backlog"; then
  if [ "` + failBootstrap + `" != "no" ]; then
    mark="` + filepath.Join(dir, "bootstrap-failed") + `"
    if [ ! -f "$mark" ]; then
      touch "$mark"
      fail_quota
    fi
    if [ "` + failBootstrap + `" = "always" ]; then fail_quota; fi
  fi
  result='` + bootstrapQuotaBacklog + `'
elif printf '%s' "$p" | grep -q "review pass for Looper"; then
  result='` + reviewComplete + `'
elif printf '%s' "$p" | grep -q "Plan execution for Looper"; then
  result='` + bootstrapQuotaPlan + `'
fi
if [ -n "$result" ]; then
  printf '{"structured_output":%s}\n' "$result"
  if [ -n "$out" ]; then printf '%s' "$result" > "$out"; fi
fi
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// bootstrapExit carries one startup run's outcome.
type bootstrapExit struct {
	code int
	err  error
}

// bootstrapQuotaRun starts one controller on a fresh project — no task file
// — so initialization runs bootstrap inside Run. It returns the engine, its
// config, the exit channel, and stop. seed requests the detached start's
// startup seed, so a drain during the bootstrap wait records its receipt in
// the saved state a query reads.
func bootstrapQuotaRun(t *testing.T, bin string, seed bool, prepare func(c *Config)) (*Engine, Config, chan bootstrapExit, func()) {
	t.Helper()
	c := repoConfig(t, "run", "--once")
	c.CodexBin = bin
	c.ClaudeBin = bin
	c.Poll = 40 * time.Millisecond
	if prepare != nil {
		prepare(&c)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if seed {
		if err = e.seedStartupState(); err != nil {
			lock.Close()
			t.Fatalf("seed: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bootstrapExit, 1)
	exited := make(chan struct{})
	go func() {
		code, err := e.Run(ctx)
		done <- bootstrapExit{code, err}
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
		case <-time.After(testBudget(10 * time.Second)):
			t.Error("engine did not exit after cancel")
		}
		e.Store.Close()
		lock.Close()
	}
	t.Cleanup(stop)
	return e, c, done, stop
}

// bootstrapQuotaHeld waits until the bootstrap quota window closed and the
// startup wait holds the start.
func bootstrapQuotaHeld(t *testing.T, e *Engine) Event {
	t.Helper()
	var pause Event
	waitFor(t, "the bootstrap quota pause", func() bool {
		pauses := eventsOfKind(quotaSnapshot(t, e), "quota")
		if len(pauses) == 0 {
			return false
		}
		pause = pauses[0]
		return true
	})
	return pause
}

// TestBootstrapQuotaPauseResumesTheStart is the acceptance route: the
// bootstrap agent reports the observed quota rejection with a reset seconds
// away, the start waits instead of ending, and bootstrap retries when the
// window reopens. The once run then completes over the bootstrapped backlog,
// proving the controller reached its loop with every startup guard intact.
func TestBootstrapQuotaPauseResumesTheStart(t *testing.T) {
	bin := bootstrapQuotaStub(t, "quota-bootstrap", "once", syntheticReset)
	e, c, done, stop := bootstrapQuotaRun(t, bin, false, nil)
	defer stop()
	select {
	case exit := <-done:
		if exit.code != 0 || exit.err != nil {
			events := quotaSnapshot(t, e)
			stop()
			t.Fatalf("the start must survive the quota window and complete; exit %d, err %v; events: %+v", exit.code, exit.err, events)
		}
	case <-time.After(testBudget(30 * time.Second)):
		stop()
		t.Fatal("the start never completed after the quota window")
	}
	events := quotaSnapshot(t, e)
	saved, loadErr := e.Store.Load()
	stop()
	// The window closed once, was waited out, and reopened for the retry.
	pauses := eventsOfKind(events, "quota")
	if len(pauses) != 1 {
		t.Fatalf("quota pause events: %d", len(pauses))
	}
	data := quotaPausePayload(t, pauses[0])
	if data.Profile != c.ReviewAgent || data.Windows != 1 {
		t.Fatalf("pause payload: %+v", data)
	}
	if !strings.Contains(data.ResetText, "Your limit will reset at") {
		t.Fatalf("reset text not retained as reported: %q", data.ResetText)
	}
	if _, err := time.Parse(time.RFC3339, data.RetryAt); err != nil {
		t.Fatalf("retry_at not stored: %q", data.RetryAt)
	}
	resumes := eventsOfKind(events, "quota-resume")
	if len(resumes) != 1 || !strings.Contains(resumes[0].Message, "bootstrap retries") {
		t.Fatalf("quota-resume events: %+v", resumes)
	}
	// Bootstrap ran twice: the rejected attempt and the retry after the
	// window. The bootstrapped backlog is the saved baseline.
	attempts := eventsOfKind(events, "bootstrap")
	if len(attempts) != 2 {
		t.Fatalf("bootstrap events: %d; want one rejected attempt and one retry", len(attempts))
	}
	if saved == nil || loadErr != nil || saved.StartupSeed || len(saved.Backlog.Tasks) != 1 || saved.Backlog.Tasks[0].ID != "T001" {
		t.Fatalf("the bootstrapped backlog must be the saved baseline (load err %v): %+v", loadErr, saved)
	}
}

// TestBootstrapQuotaBudgetEndsTheStartForOperator bounds the wait: past the
// operator's pause budget (LOOPER_RATE_LIMIT_MAX) the start still ends with
// the provider's reset text in the error, the manual path T125 recorded.
func TestBootstrapQuotaBudgetEndsTheStartForOperator(t *testing.T) {
	bin := bootstrapQuotaStub(t, "quota-bootstrap-budget", "always", "")
	e, _, done, stop := bootstrapQuotaRun(t, bin, false, func(c *Config) {
		c.RetryLimit = 1
		c.RetryDelay = 40 * time.Millisecond
	})
	defer stop()
	select {
	case exit := <-done:
		if exit.code != 1 || exit.err == nil {
			stop()
			t.Fatalf("the spent budget must end the start with an error; exit %d, err %v", exit.code, exit.err)
		}
		if !strings.Contains(exit.err.Error(), "pause budget") || !strings.Contains(exit.err.Error(), "Usage limit reached") {
			stop()
			t.Fatalf("the budget exit must name the budget and the provider reset: %v", exit.err)
		}
	case <-time.After(testBudget(30 * time.Second)):
		stop()
		t.Fatal("the spent budget never ended the start")
	}
	events := quotaSnapshot(t, e)
	stop()
	closures := 0
	spent := 0
	for _, ev := range eventsOfKind(events, "quota") {
		if strings.Contains(string(ev.Data), "budget_spent") {
			spent++
			continue
		}
		closures++
	}
	if closures != 2 || spent != 1 {
		t.Fatalf("window closures %d and budget notices %d; want 2 probed windows and 1 budget notice", closures, spent)
	}
	// No task file may exist: the operator reruns after the window.
	if _, statErr := os.Stat(e.Config.Todo); !os.IsNotExist(statErr) {
		t.Fatal("the budget exit must leave no task file")
	}
}

// TestBootstrapQuotaWaitNamesTheDateWhenRetryIsNotToday covers the third
// human line (T145): when the recorded retry lands on another day, the
// bootstrap wait line names the date beside the clock, so an operator can
// tell which day "retries at 11:34:00" means.
func TestBootstrapQuotaWaitNamesTheDateWhenRetryIsNotToday(t *testing.T) {
	bin := bootstrapQuotaStub(t, "quota-bootstrap-date", "always", tomorrowReset)
	e, _, _, stop := bootstrapQuotaRun(t, bin, false, nil)
	defer stop()
	bootstrapQuotaHeld(t, e)
	waitFor(t, "the bootstrap wait line naming the date", func() bool {
		for _, ev := range eventsOfKind(quotaSnapshot(t, e), "waiting") {
			if strings.Contains(ev.Message, "the start waits and retries at") && quotaDateInMessage.MatchString(ev.Message) {
				return true
			}
		}
		return false
	})
}

// TestBootstrapQuotaDrainStopsTheStart keeps the stop path live through a
// long window: `looper stop` during the startup wait ends the start cleanly,
// records the applied receipt in the saved state a query reads, and leaves
// the fresh project ready for the next start.
func TestBootstrapQuotaDrainStopsTheStart(t *testing.T) {
	farReset := ` for 5 hour. Your limit will reset at $(date -d "+1 hour" "+%Y-%m-%d %H:%M:%S")`
	bin := bootstrapQuotaStub(t, "quota-bootstrap-drain", "always", farReset)
	e, c, done, stop := bootstrapQuotaRun(t, bin, true, nil)
	defer stop()
	bootstrapQuotaHeld(t, e)
	if err := writeStopRequest(e.Dir, stopRequest{Session: e.controller.Session}); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-done:
		if exit.code != 0 || exit.err != nil {
			stop()
			t.Fatalf("a drain during the bootstrap wait must exit cleanly; exit %d, err %v", exit.code, exit.err)
		}
	case <-time.After(testBudget(30 * time.Second)):
		stop()
		t.Fatal("the drain never ended the startup wait")
	}
	if _, err := os.Stat(stopRequestPath(e.Dir)); !os.IsNotExist(err) {
		stop()
		t.Fatal("the honored drain request must be removed")
	}
	if exit := lastShutdown(t, e.Store); exit.Reason != "drained" {
		stop()
		t.Fatalf("the session must record a drained exit: %+v", exit)
	}
	events := quotaSnapshot(t, e)
	stop()
	stopped := false
	for _, ev := range eventsOfKind(events, "stop") {
		if strings.Contains(ev.Message, "Stopped during startup bootstrap") {
			stopped = true
		}
	}
	if !stopped {
		t.Fatal("the bootstrap drain must journal its stop event")
	}
	// The applied receipt lands in the startup seed the detached path keeps
	// queryable; the seed stays a seed, so the next start bootstraps again.
	store, err := openObservedStore(e.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snap, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.State == nil || !snap.State.StartupSeed {
		t.Fatalf("the drained start must leave the startup seed, got %+v", snap.State)
	}
	applied := false
	for _, r := range snap.State.Requests {
		if r != nil && r.ID == "stop-"+e.controller.Session && r.State == RequestApplied {
			applied = true
		}
	}
	if !applied {
		t.Fatalf("the drain must record its applied receipt in the seed: %+v", snap.State.Requests)
	}
	if _, statErr := os.Stat(c.Todo); !os.IsNotExist(statErr) {
		t.Fatal("the drained start must leave no task file")
	}
}

// TestBootstrapQuotaCancelStopsTheStart checks the other exit an operator
// can take mid-wait: cancellation (SIGINT) ends the start at once, and no
// task file or stop request is left behind.
func TestBootstrapQuotaCancelStopsTheStart(t *testing.T) {
	farReset := ` for 5 hour. Your limit will reset at $(date -d "+1 hour" "+%Y-%m-%d %H:%M:%S")`
	bin := bootstrapQuotaStub(t, "quota-bootstrap-cancel", "always", farReset)
	e, c, done, stop := bootstrapQuotaRun(t, bin, false, nil)
	defer stop()
	bootstrapQuotaHeld(t, e)
	// stop cancels the context first and waits for the exit.
	go stop()
	select {
	case exit := <-done:
		if exit.code != 130 {
			stop()
			t.Fatalf("a cancelled bootstrap wait must exit 130, got %d (%v)", exit.code, exit.err)
		}
	case <-time.After(testBudget(10 * time.Second)):
		stop()
		t.Fatal("cancellation never ended the startup wait")
	}
	if _, statErr := os.Stat(c.Todo); !os.IsNotExist(statErr) {
		stop()
		t.Fatal("the cancelled start must leave no task file")
	}
}

// TestBootstrapQuotaAdoptsTaskFileSuppliedDuringWait covers the operator who
// hand-writes the task file while the window is closed: bootstrap exists to
// create that file, so the start adopts it instead of overwriting it, the
// recorded pause still gates the loop until the window reopens, and a
// supplied plan then follows the ordinary startup import against the
// operator's backlog.
func TestBootstrapQuotaAdoptsTaskFileSuppliedDuringWait(t *testing.T) {
	// A window a few seconds out: the file lands inside it, and the loop the
	// adoption reaches still proves it waited out the recorded pause.
	adoptableReset := ` for 5 hour. Your limit will reset at $(date -d "+3 seconds" "+%Y-%m-%d %H:%M:%S")`
	bin := bootstrapQuotaStub(t, "quota-bootstrap-adopt", "always", adoptableReset)
	plan := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(plan, []byte("# Extra requirement\nAdopt the operator backlog.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e, c, done, stop := bootstrapQuotaRun(t, bin, false, func(cfg *Config) {
		cfg.Plans = []string{plan}
	})
	defer stop()
	bootstrapQuotaHeld(t, e)
	// The staging record of the captured startup plan exists while bootstrap
	// owns it; the operator's file supersedes that ownership.
	staging := ""
	entries, _ := os.ReadDir(importsDir(e.Dir))
	for _, entry := range entries {
		if _, statErr := os.Stat(stagingPath(e.Dir, entry.Name())); statErr == nil {
			staging = stagingPath(e.Dir, entry.Name())
		}
	}
	if staging == "" {
		stop()
		t.Fatal("the captured startup plan must stage before the wait")
	}
	supplied := Backlog{Version: 1, Sources: []string{}, Goal: "operator backlog", Tasks: []Task{
		{ID: "T001", Title: "operator task", Priority: 1, Status: "done", Details: "supplied during the wait", Tags: []string{"project-done"}},
	}}
	if err := writeJSON(c.Todo, supplied); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-done:
		if exit.code != 0 || exit.err != nil {
			stop()
			t.Fatalf("the supplied task file must complete the start; exit %d, err %v", exit.code, exit.err)
		}
	case <-time.After(testBudget(30 * time.Second)):
		stop()
		t.Fatal("the supplied task file never completed the start")
	}
	events := quotaSnapshot(t, e)
	s, loadErr := e.Store.Load()
	stop()
	adopted := false
	for _, ev := range eventsOfKind(events, "bootstrap") {
		if strings.Contains(ev.Message, "adopts the operator's file") {
			adopted = true
		}
	}
	if !adopted {
		t.Fatal("the adoption must journal its bootstrap event")
	}
	// The operator's bytes stay authoritative: the goal survives to the saved
	// baseline, and no bootstrap attempt ever ran to completion over them.
	data, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	var saved Backlog
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Goal != "operator backlog" || len(saved.Tasks) != 1 || saved.Tasks[0].Title != "operator task" {
		t.Fatalf("the operator backlog must remain: %s", data)
	}
	if _, statErr := os.Stat(staging); !os.IsNotExist(statErr) {
		t.Fatal("the superseded staging record must be removed")
	}
	// The plan follows the ordinary startup import against that backlog.
	if loadErr != nil || s == nil || len(s.Imports) == 0 {
		t.Fatalf("the supplied plan must reach the ordinary import records (load err %v): %+v", loadErr, s)
	}
}
