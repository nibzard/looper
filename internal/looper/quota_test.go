package looper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// T125: timed provider quota exhaustion. The observed provider message
// (Ohya event 12262, Charlie event 8271) and the subscription five-hour
// wording classify as a timed quota window, not configuration or ordinary
// execution failure; untimed quota text keeps the operator-facing classes.

const observedQuotaReset = "API Error: Request rejected (429) · [1308][Usage limit reached for 5 hour. Your limit will reset at 2026-11-17 20:15:59][202611171850179bc1ad274a974b3a]"

func TestClassifyTimedQuotaFailures(t *testing.T) {
	for _, tc := range []struct {
		name, message, want string
	}{
		{"observed rejection with reset", observedQuotaReset, "quota"},
		{"observed rejection alone", "API Error: Request rejected (429)", "quota"},
		{"five-hour plan limit", "Your Claude Code plan has a five-hour usage limit, which you've hit. Your limit will reset at 3:14pm (in 2h 3m).", "quota"},
		{"untimed account quota", "usage limit reached: this account quota is exhausted", "configuration"},
		{"invalid credentials", "invalid API key provided", "configuration"},
		{"unsupported model", "The 'stub-astra' model is not supported when using Codex with a ChatGPT account.", "configuration"},
		{"ordinary rate limit", "stub upstream reports 429 rate limit; retry later", "transient"},
		{"ordinary failure", "test failed: exit status 1", "execution"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFailure("exit status 1\n" + tc.message); got != tc.want {
				t.Fatalf("classifyFailure(%q) = %s; want %s", tc.message, got, tc.want)
			}
		})
	}
}

func TestQuotaResetTimeShapes(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name, message string
		want          time.Time
		ok            bool
	}{
		{"observed wall clock", observedQuotaReset, time.Date(2026, 11, 17, 20, 15, 59, 0, time.Local), true},
		{"clock time", "Your limit will reset at 3:14pm (in 2h 3m)", time.Date(2026, 11, 18, 15, 14, 0, 0, time.Local), true},
		{"zoned timestamp", "resets at 2026-11-18T22:30:00Z", time.Date(2026, 11, 18, 22, 30, 0, 0, time.UTC), true},
		{"relative hours and minutes", "rate limited, retry in 1h 30m", now.Add(90 * time.Minute), true},
		{"relative minutes", "quota reached, resume in 45m", now.Add(45 * time.Minute), true},
		{"relative seconds", "rejected, retry after 90 seconds", now.Add(90 * time.Second), true},
		{"clock time already past names tomorrow", "Your limit will reset at 3:14am", time.Date(2026, 11, 19, 3, 14, 0, 0, time.Local), true},
		// T145: a reported clock only minutes past is provider clock skew or
		// delivery delay, not a window that closes tomorrow. The past value
		// stands and the pause's rate-limit fallback wait reopens it.
		{"clock time minutes past keeps today", "Your limit will reset at 9:58am", time.Date(2026, 11, 18, 9, 58, 0, 0, time.Local), true},
		{"clock time at the past tolerance keeps today", "Your limit will reset at 9:50am", time.Date(2026, 11, 18, 9, 50, 0, 0, time.Local), true},
		// T142: a reset cue governs the parse, so a datetime the message
		// reports beside the reset never overrides it.
		{"incident stamp never overrides reported clock", "API Error: Request rejected (429); incident began 2026-11-18 09:00:00; usage limit reached; your limit will reset at 23:55", time.Date(2026, 11, 18, 23, 55, 0, 0, time.Local), true},
		{"reported clock before incident stamp", "API Error: Request rejected (429); usage limit reached; your limit will reset at 23:55; incident began 2026-11-18 09:00:00", time.Date(2026, 11, 18, 23, 55, 0, 0, time.Local), true},
		{"request stamp never overrides cued reset", "request 2026-11-18T09:00:00Z rejected; limit resets at 2026-11-18T22:30:00Z", time.Date(2026, 11, 18, 22, 30, 0, 0, time.UTC), true},
		{"maintenance window never overrides reported reset", "maintenance window 2026-11-18 01:00:00 to 05:00:00; your limit will reset at 2026-11-19 07:30:00", time.Date(2026, 11, 19, 7, 30, 0, 0, time.Local), true},
		{"uncued stamp and clock stay below the cued clock", "request logged at 09:00 under id 2026-11-18 09:00:00; filler text separates them; your limit will reset at 3:14pm", time.Date(2026, 11, 18, 15, 14, 0, 0, time.Local), true},
		{"bare stamp without a reset cue", "quota window 2026-11-18 20:15:59 reported", time.Date(2026, 11, 18, 20, 15, 59, 0, time.Local), true},
		{"relative delay outranks a distant uncued stamp", "quota reached; request logged 2026-11-18 09:00:00 with a long trace id; resume in 45m", now.Add(45 * time.Minute), true},
		{"no reset reported", "usage limit reached: this account quota is exhausted", time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := quotaResetTime(tc.message, now)
			if ok != tc.ok {
				t.Fatalf("quotaResetTime(%q) ok = %v; want %v", tc.message, ok, tc.ok)
			}
			if ok && !got.Equal(tc.want) {
				t.Fatalf("quotaResetTime(%q) = %s; want %s", tc.message, got, tc.want)
			}
		})
	}
}

// TestQuotaRetryClockNamesTheDateWhenNotToday pins the human rendering of a
// pause's retry time (T145): the clock alone while the retry lands today,
// and the date beside it the moment it does not.
func TestQuotaRetryClockNamesTheDateWhenNotToday(t *testing.T) {
	now := time.Date(2026, 11, 18, 11, 39, 28, 0, time.Local)
	for _, tc := range []struct {
		name  string
		retry time.Time
		want  string
	}{
		{"later today", now.Add(2 * time.Hour), "13:39:28"},
		{"past today", now.Add(-2 * time.Minute), "11:37:28"},
		{"tomorrow", now.AddDate(0, 0, 1), "2026-11-19 11:39:28"},
		{"yesterday", now.AddDate(0, 0, -1), "2026-11-17 11:39:28"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaRetryClock(tc.retry, now); got != tc.want {
				t.Fatalf("quotaRetryClock(%s, %s) = %q; want %q", tc.retry, now, got, tc.want)
			}
		})
	}
}

// TestAgentQuotaFailureCarriesResetTime feeds the observed message through a
// stub agent: the run classifies it as quota, keeps the provider's reset
// time, and never burns an in-run transient retry on it.
func TestAgentQuotaFailureCarriesResetTime(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	script := "#!/bin/bash\n" +
		"printf '%s\\n' '{\"type\":\"error\",\"message\":\"" + observedQuotaReset + "\"}'\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	a := Agent{Config: Config{ClaudeBin: bin, RetryLimit: 3, RetryDelay: time.Millisecond}}
	_, err := a.Run(context.Background(), "claude", dir, dir, "prompt", true)
	var agent *AgentError
	if !errors.As(err, &agent) {
		t.Fatalf("expected AgentError, got %v", err)
	}
	if agent.Kind != failureQuota {
		t.Fatalf("classification: %s", agent.Kind)
	}
	want := time.Date(2026, 11, 17, 20, 15, 59, 0, time.Local)
	if !agent.Retry.Equal(want) {
		t.Fatalf("reset time: %s; want %s", agent.Retry, want)
	}
	// A quota window is never retried in-process: only the first attempt's
	// event log may exist.
	if _, statErr := os.Stat(filepath.Join(dir, "events-01.jsonl")); statErr == nil {
		t.Fatal("quota failure retried in-process: second attempt log exists")
	}
}

// TestAgentQuotaResetPrefersCuedResetTime feeds the T142 shape through a
// stub agent: a quota message that carries an unrelated incident stamp
// beside its reported reset clock. The parsed retry is the cued clock; the
// incident stamp never moves the pause the controller will record.
func TestAgentQuotaResetPrefersCuedResetTime(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	script := "#!/bin/bash\n" +
		"printf '%s\\n' '{\"type\":\"error\",\"message\":\"API Error: Request rejected (429); incident began 2026-11-18 09:00:00; usage limit reached; your limit will reset at 23:55\"}'\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	a := Agent{Config: Config{ClaudeBin: bin, RetryLimit: 3, RetryDelay: time.Millisecond}}
	_, err := a.Run(context.Background(), "claude", dir, dir, "prompt", true)
	var agent *AgentError
	if !errors.As(err, &agent) {
		t.Fatalf("expected AgentError, got %v", err)
	}
	if agent.Kind != failureQuota {
		t.Fatalf("classification: %s", agent.Kind)
	}
	// The agent layer parses against the live clock, so the reported clock
	// names today or tomorrow; its time of day is exact either way, and it
	// is never the incident stamp's 09:00:00.
	if h, m, s := agent.Retry.Clock(); h != 23 || m != 55 || s != 0 {
		t.Fatalf("reset time: %s; want the reported clock 23:55:00", agent.Retry)
	}
}

// T136: the classification layer reports the quota failure itself. A stub
// agent that emits the observed message returns a quota diagnostic — the
// class, the provider's reset wording as reported, and the recovery action —
// and the in-run loop still schedules no retry for it.
func TestAgentQuotaDiagnosticCarriesResetTextAndAction(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	script := "#!/bin/bash\n" +
		"printf '%s\\n' '{\"type\":\"error\",\"message\":\"" + observedQuotaReset + "\"}'\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	a := Agent{Config: Config{ClaudeBin: bin, RetryLimit: 3, RetryDelay: time.Millisecond}}
	_, err := a.Run(context.Background(), "claude", dir, dir, "prompt", true)
	var agent *AgentError
	if !errors.As(err, &agent) {
		t.Fatalf("expected AgentError, got %v", err)
	}
	if agent.Kind != failureQuota {
		t.Fatalf("classification: %s", agent.Kind)
	}
	s := agent.Summary
	if s == nil || s.Outcome != outcomeQuota {
		t.Fatalf("summary outcome: %+v", s)
	}
	// The provider's reset stays in the diagnostic exactly as reported,
	// timestamp with no time zone included.
	if !strings.Contains(s.Cause, "Your limit will reset at 2026-11-17 20:15:59") {
		t.Fatalf("reset text not retained as reported: %q", s.Cause)
	}
	// The recovery action names the resume time and the requeue path, and
	// keeps the reported wall-clock digits.
	if !strings.Contains(s.Action, "2026-11-17 20:15:59") || !strings.Contains(s.Action, "requeue") {
		t.Fatalf("recovery action missing reset or requeue: %q", s.Action)
	}
	if len(s.Action) > maxFailureAction {
		t.Fatalf("recovery action unbounded: %d bytes", len(s.Action))
	}
	// The journaled line names the class beside the provider's wording.
	if m := failureMessage("T1", s); !strings.Contains(m, "quota window") || !strings.Contains(m, "Your limit will reset at") {
		t.Fatalf("failure message: %q", m)
	}
	// The version 2 error document leads with the failure's own action.
	doc := failureErrorDoc(err, s)
	if doc == nil || len(doc.Actions) == 0 || doc.Actions[0].Description != s.Action {
		t.Fatalf("error document actions: %+v", doc)
	}
	// No in-run retry: the quota window is never retried here (AC3).
	if _, statErr := os.Stat(filepath.Join(dir, "events-01.jsonl")); statErr == nil {
		t.Fatal("quota failure retried in-process: second attempt log exists")
	}
}

// T136: a quota rejection without a reset keeps the quota diagnostic and
// names the wait-and-requeue action instead of a reset time.
func TestAgentQuotaDiagnosticWithoutReset(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	script := "#!/bin/bash\n" +
		"printf '%s\\n' '{\"type\":\"error\",\"message\":\"API Error: Request rejected (429)\"}'\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	a := Agent{Config: Config{ClaudeBin: bin, RetryLimit: 1, RetryDelay: time.Millisecond}}
	_, err := a.Run(context.Background(), "claude", dir, dir, "prompt", true)
	var agent *AgentError
	if !errors.As(err, &agent) {
		t.Fatalf("expected AgentError, got %v", err)
	}
	if agent.Kind != failureQuota || !agent.Retry.IsZero() {
		t.Fatalf("classification %s, retry %s", agent.Kind, agent.Retry)
	}
	if s := agent.Summary; s.Outcome != outcomeQuota || !strings.Contains(s.Action, "no reset reported") || !strings.Contains(s.Action, "requeue") {
		t.Fatalf("summary without reset: %+v", s)
	}
}

// T136: invalid credentials and ordinary rate limits keep their existing
// classes and carry no quota diagnostic.
func TestAgentQuotaDiagnosticKeepsOtherClasses(t *testing.T) {
	for _, tc := range []struct{ name, message, want string }{
		{"invalid credentials", "invalid API key provided", "configuration"},
		{"ordinary rate limit", "stub upstream reports 429 rate limit; retry later", "transient"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "agent.sh")
			script := "#!/bin/bash\n" +
				"printf '%s\\n' '{\"type\":\"error\",\"message\":\"" + tc.message + "\"}'\n" +
				"exit 1\n"
			if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			a := Agent{Config: Config{ClaudeBin: bin, RetryLimit: 1, RetryDelay: time.Millisecond}}
			_, err := a.Run(context.Background(), "claude", dir, dir, "prompt", true)
			var agent *AgentError
			if !errors.As(err, &agent) {
				t.Fatalf("expected AgentError, got %v", err)
			}
			if agent.Kind != tc.want {
				t.Fatalf("classification: %s; want %s", agent.Kind, tc.want)
			}
			if s := agent.Summary; s == nil || s.Outcome == outcomeQuota || s.Action != "" {
				t.Fatalf("unexpected quota diagnostic: %+v", s)
			}
		})
	}
}

// quotaStub writes one stub agent covering the worker, reviewer, and planner
// roles. failRole ("worker", "reviewer", or "planner") emits the observed
// quota rejection for its first invocation before it starts answering;
// alwaysFail keeps that role failing forever. reset supplies the shell text
// baked into the message, so a test can report a synthetic reset seconds
// away or none at all. Every invocation appends its role to roles.log.
func quotaStub(t *testing.T, name, failRole string, alwaysFail bool, reset string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, name)
	keepFailing := "no"
	if alwaysFail {
		keepFailing = "yes"
	}
	script := `#!/bin/bash
out=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$a"; fi
  prev="$a"
done
p="$(cat)"
role=""
if printf '%s' "$p" | grep -q "review pass for Looper"; then role="reviewer"
elif printf '%s' "$p" | grep -q "Plan execution for Looper"; then role="planner"
elif printf '%s' "$p" | grep -q "Complete task "; then role="worker"
fi
echo "$role" >> "` + filepath.Join(dir, "roles.log") + `"
fail_quota() {
  msg="API Error: Request rejected (429) · [1308][Usage limit reached` + reset + `]"
  printf '%s\n' "{\"type\":\"error\",\"message\":\"$msg\"}"
  exit 1
}
if [ "$role" = "` + failRole + `" ]; then
  mark="` + filepath.Join(dir, failRole+"-failed") + `"
  if [ ! -f "$mark" ]; then
    touch "$mark"
    fail_quota
  fi
  if [ "` + keepFailing + `" = "yes" ]; then fail_quota; fi
fi
result=""
if [ "$role" = "reviewer" ]; then
  result='{"status":"reviewed","summary":"no findings","findings":[],"complete":false}'
elif [ "$role" = "planner" ]; then
  result='{"setup":"","verify":"true","tasks":[{"task_id":"T1","write_paths":["."],"resources":[]}]}'
elif [ "$role" = "worker" ]; then
  tid="$(printf '%s' "$p" | grep -oE 'T[0-9]+' | head -n1)"
  printf 'quota work\n' > "quota-work-$tid.txt"
  result="{\"task_id\":\"$tid\",\"status\":\"done\",\"summary\":\"implemented\",\"files\":[\"quota-work-$tid.txt\"],\"blockers\":[],\"followups\":[],\"unblock_when\":\"\"}"
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

// quotaRun starts one controller around a single todo task T1 and returns
// the engine, its config, the loop's exit-code channel, and stop. withPlan
// pre-installs the execution plan so no planner runs. stop cancels the loop
// and waits for its exit; assertions then read engine state with the owner
// parked. The store stays readable while the loop runs, so pollers read
// events and the exported task file, never engine memory.
func quotaRun(t *testing.T, bin string, withPlan bool, prepare func(c *Config)) (*Engine, Config, chan int, func()) {
	t.Helper()
	c := repoConfig(t, "run")
	c.CodexBin = bin
	c.ClaudeBin = bin
	c.Poll = 40 * time.Millisecond
	if prepare != nil {
		prepare(&c)
	}
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "quota recovery", Tasks: []Task{
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
	if withPlan {
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
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		code, _ := e.Run(ctx)
		done <- code
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

// quotaWait polls until cond holds or the budget ends, and reports whether
// the condition held. The budget scales with the pressure the host reports
// while the wait runs, not only at entry: a stall that arrives mid-wait
// must extend an already-started wait the way testBudget would size a new
// one, because the recorded pressure average lags a burst by up to a
// minute (the 2026-11-20 check run lost a healthy chain at a ×3 budget
// sized before the stall deepened).
func quotaWait(t *testing.T, budget time.Duration, cond func() bool, label string) bool {
	t.Helper()
	start := time.Now()
	for time.Since(start) < testBudget(budget) {
		if cond() {
			return true
		}
		time.Sleep(30 * time.Millisecond)
	}
	return cond()
}

func quotaTaskStatus(t *testing.T, c Config, id string) string {
	t.Helper()
	data, err := os.ReadFile(c.Todo)
	if err != nil {
		return ""
	}
	var b Backlog
	if json.Unmarshal(data, &b) != nil {
		return ""
	}
	task := b.Task(id)
	if task == nil {
		return ""
	}
	return task.Status
}

// quotaSnapshot reads the whole journal through the store, which stays safe
// to query while the loop goroutine runs. Tests snapshot before stop parks
// the owner, because stop also closes the store. Filtering reuses
// eventsOfKind from the consultation tests.
func quotaSnapshot(t *testing.T, e *Engine) []Event {
	t.Helper()
	events, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// quotaPausePayload decodes the structured data one quota pause event
// carries.
func quotaPausePayload(t *testing.T, ev Event) struct {
	Profile   string `json:"profile"`
	RetryAt   string `json:"retry_at"`
	ResetText string `json:"reset_text"`
	Windows   int    `json:"windows"`
} {
	t.Helper()
	var data struct {
		Profile   string `json:"profile"`
		RetryAt   string `json:"retry_at"`
		ResetText string `json:"reset_text"`
		Windows   int    `json:"windows"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

// syntheticReset reports a provider reset a couple of seconds away, computed
// at invocation time so the window opens shortly after the failure.
const syntheticReset = ` for 5 hour. Your limit will reset at $(date -d "+2 seconds" "+%Y-%m-%d %H:%M:%S")`

// pastClockReset reports a provider reset clock a couple of minutes in the
// past, the observed incident shape (T145): provider clock skew or delivery
// delay makes the reported reset already past when the controller reads it.
const pastClockReset = ` for 5 hour. Your limit will reset at $(date -d "-2 minutes" "+%H:%M:%S")`

// tomorrowReset reports a provider reset beyond a day out, so the pause's
// retry lands on another calendar day and every human line must name the
// date (T145).
const tomorrowReset = ` for 5 hour. Your limit will reset at $(date -d "+25 hours" "+%Y-%m-%d %H:%M:%S")`

// quotaDateInMessage matches a date beside a clock, the shape a human quota
// line carries whenever its retry time is not today.
var quotaDateInMessage = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`)

// TestWorkerQuotaPauseResumesAutomatically exercises the acceptance route:
// the observed quota rejection closes the window, the controller pauses the
// provider, retains the attempt, and requeues the task automatically at the
// reported reset — without a repair worker or a configuration edit.
func TestWorkerQuotaPauseResumesAutomatically(t *testing.T) {
	bin := quotaStub(t, "quota-worker", "worker", false, syntheticReset)
	e, c, _, stop := quotaRun(t, bin, true, nil)
	defer stop()
	if !quotaWait(t, 30*time.Second, func() bool { return quotaTaskStatus(t, c, "T1") == "done" }, "T1 done") {
		stop()
		t.Fatalf("T1 did not complete after the quota window; status %q", quotaTaskStatus(t, c, "T1"))
	}
	events := quotaSnapshot(t, e)
	stop()
	pauses := eventsOfKind(events, "quota")
	if len(pauses) != 1 {
		t.Fatalf("quota pause events: %d", len(pauses))
	}
	data := quotaPausePayload(t, pauses[0])
	if data.Profile != c.Agent || data.Windows != 1 {
		t.Fatalf("pause payload: %+v", data)
	}
	if !strings.Contains(data.ResetText, "Your limit will reset at") {
		t.Fatalf("reset text not retained as reported: %q", data.ResetText)
	}
	if _, err := time.Parse(time.RFC3339, data.RetryAt); err != nil {
		t.Fatalf("retry_at not stored: %q", data.RetryAt)
	}
	// T136: the bounded failure summary the worker failure journaled is a
	// quota diagnostic: the class, the provider's reset text, and the
	// recovery action, beside the pause event above.
	quotaDiagnostics := 0
	for _, ev := range eventsOfKind(events, failureEventKind) {
		var payload struct {
			Failure *FailureSummary `json:"failure"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil || payload.Failure == nil {
			t.Fatalf("failure payload of event %d: %v %s", ev.ID, err, ev.Data)
		}
		if payload.Failure.Operation != opWorker {
			continue
		}
		if payload.Failure.Outcome != outcomeQuota || payload.Failure.Action == "" {
			t.Fatalf("worker failure summary is not a quota diagnostic: %+v", payload.Failure)
		}
		if !strings.Contains(payload.Failure.Action, "requeue") {
			t.Fatalf("quota diagnostic action: %q", payload.Failure.Action)
		}
		if !strings.Contains(ev.Message, "quota window") {
			t.Fatalf("quota failure message: %q", ev.Message)
		}
		quotaDiagnostics++
	}
	if quotaDiagnostics == 0 {
		t.Fatal("no worker failure summary journaled for the quota window")
	}
	if len(eventsOfKind(events, "quota-resume")) == 0 {
		t.Fatal("no quota-resume event")
	}
	attempts := 0
	for _, a := range e.State.Attempts {
		if a.Task.ID == "T1" {
			attempts++
		}
	}
	if attempts != 1 {
		t.Fatalf("the paused task restarted from a new attempt: %d attempts", attempts)
	}
}

// TestReviewQuotaPauseResumesAutomatically reproduces audit finding 3: the
// review agent hit the five-hour limit and the controller ended as a
// configuration failure. The closed window now pauses the review, which
// retries after the reset while the run keeps running.
func TestReviewQuotaPauseResumesAutomatically(t *testing.T) {
	bin := quotaStub(t, "quota-review", "reviewer", false, syntheticReset)
	e, c, _, stop := quotaRun(t, bin, true, func(cfg *Config) { cfg.ReviewEvery = 1 })
	defer stop()
	reviewed := func() bool {
		if quotaTaskStatus(t, c, "T1") != "done" {
			return false
		}
		for _, ev := range quotaSnapshot(t, e) {
			if ev.Kind == "review" && strings.Contains(ev.Message, "no findings") {
				return true
			}
		}
		return false
	}
	if !quotaWait(t, 30*time.Second, reviewed, "review completed") {
		stop()
		t.Fatalf("review did not complete after the quota window; task %q", quotaTaskStatus(t, c, "T1"))
	}
	events := quotaSnapshot(t, e)
	stop()
	if len(eventsOfKind(events, "quota")) != 1 {
		t.Fatalf("quota pause events: %d", len(eventsOfKind(events, "quota")))
	}
	if len(eventsOfKind(events, "quota-resume")) == 0 {
		t.Fatal("no quota-resume event")
	}
}

// TestPlanningQuotaPauseResumes covers the planning gate: the planner
// reports the same quota rejection, the loop waits out the window, and the
// rebuilt plan dispatches the retained work.
func TestPlanningQuotaPauseResumes(t *testing.T) {
	bin := quotaStub(t, "quota-plan", "planner", false, syntheticReset)
	e, c, _, stop := quotaRun(t, bin, false, nil)
	defer stop()
	if !quotaWait(t, 30*time.Second, func() bool { return quotaTaskStatus(t, c, "T1") == "done" }, "T1 done") {
		stop()
		t.Fatalf("planning did not recover after the quota window; status %q", quotaTaskStatus(t, c, "T1"))
	}
	events := quotaSnapshot(t, e)
	stop()
	if len(eventsOfKind(events, "quota")) != 1 {
		t.Fatalf("quota pause events: %d", len(eventsOfKind(events, "quota")))
	}
}

// TestCredentialFailureStillStopsTheRun keeps the operator boundary: an
// invalid API key classifies as configuration and ends the run for an
// operator action; it never pauses as a quota window.
func TestCredentialFailureStillStopsTheRun(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	script := "#!/bin/bash\n" +
		"printf '%s\\n' '{\"type\":\"error\",\"message\":\"invalid API key provided\"}'\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	e, _, done, stop := quotaRun(t, bin, true, func(c *Config) { c.ReviewEvery = 1 })
	defer stop()
	select {
	case code := <-done:
		if code != 1 {
			events := quotaSnapshot(t, e)
			stop()
			t.Fatalf("credential failure exited %d; want 1; events: %+v", code, events)
		}
	case <-time.After(testBudget(30 * time.Second)):
		stop()
		t.Fatal("credential failure did not end the run")
	}
	events := quotaSnapshot(t, e)
	stop()
	if len(eventsOfKind(events, "quota")) != 0 {
		t.Fatal("credential failure paused as a quota window")
	}
	for _, ev := range eventsOfKind(events, "shutdown") {
		if strings.Contains(ev.Message, "invalid API key") {
			return
		}
	}
	t.Fatalf("shutdown record: %+v", eventsOfKind(events, "shutdown"))
}

// TestQuotaPauseBudgetHoldsWorkForOperator bounds the automatic resume:
// past the operator's pause budget (LOOPER_RATE_LIMIT_MAX), quota-held work
// stays blocked with the recorded evidence instead of retrying every window.
func TestQuotaPauseBudgetHoldsWorkForOperator(t *testing.T) {
	bin := quotaStub(t, "quota-budget", "worker", true, "")
	e, c, _, stop := quotaRun(t, bin, true, func(cfg *Config) {
		cfg.RetryLimit = 1
		cfg.RetryDelay = 40 * time.Millisecond
	})
	defer stop()
	if !quotaWait(t, 30*time.Second, func() bool { return quotaTaskStatus(t, c, "T1") == "blocked" }, "T1 blocked") {
		stop()
		t.Fatalf("T1 never blocked; status %q", quotaTaskStatus(t, c, "T1"))
	}
	budgetSpent := func() bool {
		for _, ev := range quotaSnapshot(t, e) {
			if ev.Kind == "quota" && strings.Contains(string(ev.Data), "budget_spent") {
				return true
			}
		}
		return false
	}
	if !quotaWait(t, 30*time.Second, budgetSpent, "budget spent") {
		stop()
		t.Fatal("the pause budget was never reported as spent")
	}
	// No further window may be probed or requeued past the budget.
	time.Sleep(testBudget(500 * time.Millisecond))
	events := quotaSnapshot(t, e)
	stop()
	task := e.State.Backlog.Task("T1")
	if task.Status != "blocked" || task.BlockReason == nil || task.BlockReason.Code != blockCodeQuotaWindow {
		t.Fatalf("T1 after budget: %+v", task)
	}
	if task.BlockReason.Class != blockClassFailure || task.BlockReason.Retry != blockRetryYes {
		t.Fatalf("T1 reason after budget: %+v", task.BlockReason)
	}
	pauses := 0
	for _, ev := range eventsOfKind(events, "quota") {
		if !strings.Contains(string(ev.Data), "budget_spent") {
			pauses++
		}
	}
	if pauses != 2 {
		t.Fatalf("window pauses: %d; want one per probed window (2)", pauses)
	}
	// One requeued task journals twice: the provider-level resume and the
	// task-scoped requeue note a task filter finds.
	if len(eventsOfKind(events, "quota-resume")) != 2 {
		t.Fatalf("quota-resume events: %d; want one provider resume and one task requeue inside the budget", len(eventsOfKind(events, "quota-resume")))
	}
}

// TestQuotaPastClockReopensImminently is the observed incident (T145): the
// provider reports a reset clock that is already a couple of minutes past
// when the controller reads it. The pause must not read that clock as
// tomorrow and park the work for a day — it falls back to the configured
// rate-limit wait, reopens, and the retained work completes.
func TestQuotaPastClockReopensImminently(t *testing.T) {
	bin := quotaStub(t, "quota-past-clock", "worker", false, pastClockReset)
	e, c, _, stop := quotaRun(t, bin, true, func(cfg *Config) { cfg.RetryDelay = 40 * time.Millisecond })
	defer stop()
	// The terminal wait covers the whole chain — pause, fallback wait,
	// resume, requeue, and the worker's second run — so its bound must be
	// the largest in this file: every durable step the chain takes stalls
	// with the host, and the recorded flakes (2026-11-20 check run; the
	// live reproduction at PSI "full" ~85) expired a 30 s base while T1
	// was still progressing. The imminence itself is verified from the
	// journaled times below, which do not stretch with the host.
	if !quotaWait(t, 90*time.Second, func() bool { return quotaTaskStatus(t, c, "T1") == "done" }, "T1 done") {
		stop()
		t.Fatalf("a past reset clock must reopen imminently; T1 status %q", quotaTaskStatus(t, c, "T1"))
	}
	events := quotaSnapshot(t, e)
	stop()
	pauses := eventsOfKind(events, "quota")
	if len(pauses) != 1 {
		t.Fatalf("quota pause events: %d", len(pauses))
	}
	data := quotaPausePayload(t, pauses[0])
	retryAt, err := time.Parse(time.RFC3339, data.RetryAt)
	if err != nil {
		t.Fatalf("retry_at not stored: %q", data.RetryAt)
	}
	pauseAt, err := time.Parse(time.RFC3339, pauses[0].Time)
	if err != nil {
		t.Fatalf("pause event time not stored: %q", pauses[0].Time)
	}
	// The reported clock was minutes past: the journaled retry must sit one
	// fallback wait after the pause, never one day. retry_at carries whole
	// seconds, so it may parse a fraction of a second before the event.
	if wait := retryAt.Sub(pauseAt); wait < -2*time.Second || wait > time.Minute {
		t.Fatalf("retry_at %s sits %s after the pause; want the rate-limit fallback wait, not a day", data.RetryAt, wait)
	}
	if !strings.Contains(pauses[0].Message, "retries automatically at") {
		t.Fatalf("pause message: %q", pauses[0].Message)
	}
	if quotaDateInMessage.MatchString(pauses[0].Message) {
		t.Fatalf("a same-day fallback retry must render the clock alone: %q", pauses[0].Message)
	}
	if len(eventsOfKind(events, "quota-resume")) == 0 {
		t.Fatal("no quota-resume event")
	}
}

// TestQuotaMessagesNameTheDateWhenRetryIsNotToday covers the operator-facing
// lines once the retry lands on another day (T145): the quota pause event and
// the in-run waiting line name the date beside the clock, so an operator can
// tell which day the wait ends.
func TestQuotaMessagesNameTheDateWhenRetryIsNotToday(t *testing.T) {
	bin := quotaStub(t, "quota-tomorrow", "worker", false, tomorrowReset)
	e, c, _, stop := quotaRun(t, bin, true, func(cfg *Config) { cfg.RetryDelay = 40 * time.Millisecond })
	defer stop()
	messageNamesDate := func(kind, marker string) func() bool {
		return func() bool {
			for _, ev := range eventsOfKind(quotaSnapshot(t, e), kind) {
				if strings.Contains(ev.Message, marker) && quotaDateInMessage.MatchString(ev.Message) {
					return true
				}
			}
			return false
		}
	}
	if !quotaWait(t, 30*time.Second, messageNamesDate("quota", "retries automatically at"), "pause event naming the date") {
		stop()
		t.Fatalf("the pause event must name the date when the retry is not today")
	}
	if !quotaWait(t, 30*time.Second, messageNamesDate("waiting", "retries at"), "waiting line naming the date") {
		stop()
		t.Fatalf("the in-run waiting line must name the date when the retry is not today")
	}
	if s := quotaTaskStatus(t, c, "T1"); s != "blocked" {
		stop()
		t.Fatalf("T1 must stay held for the recorded window; status %q", s)
	}
}

// A quota resume whose export the external-edit guard refuses leaves the held
// tasks exactly as they were, so the re-armed pause still finds them at the
// next boundary instead of stranding half-requeued tasks in memory.
func TestQuotaRequeueRollsBackOnRefusedExport(t *testing.T) {
	e := guardEngine(t)
	task := &e.State.Backlog.Tasks[0]
	task.Status = "blocked"
	task.Blockers = []string{"quota window closed"}
	task.UnblockWhen = quotaUnblockCue + "2026-11-21T12:00:00Z."
	task.BlockReason = failureBlockReason(blockCodeQuotaWindow, blockRetryYes, "task-1")
	e.State.Attempts = append(e.State.Attempts, &Attempt{ID: "task-1", Task: *task, Agent: "claude", Phase: "blocked"})
	e.State.PlanSignature = "plan-sig"
	e.State.Finished = true
	if len(e.quotaHeldAttempts("claude")) != 1 {
		t.Fatal("fixture: the task must be quota-held")
	}
	if _, err := e.requeueQuotaHeld("claude"); err == nil {
		t.Fatal("fixture: the guard must refuse the export")
	}
	got := e.State.Backlog.Tasks[0]
	if got.Status != "blocked" || got.BlockReason == nil || got.UnblockWhen == "" || len(got.Blockers) != 1 {
		t.Fatalf("the refused resume left a half-requeued task: %+v", got)
	}
	if e.State.PlanSignature != "plan-sig" || !e.State.Finished {
		t.Fatalf("the refused resume left state flags changed: sig=%q finished=%v", e.State.PlanSignature, e.State.Finished)
	}
	if len(e.quotaHeldAttempts("claude")) != 1 {
		t.Fatal("the re-armed pause can no longer find the held work")
	}
}
