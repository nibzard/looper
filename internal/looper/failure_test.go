package looper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/contractschema"
)

// cleanupAuditCase runs one stub cleanup script through the exact wrapper
// Looper generates for a resource lease, and through plain bash without that
// wrapper, so the audit observes both behaviors before anything is fixed.
func cleanupAuditCase(t *testing.T, name, script string) (wrapped, plain error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, resourceCleanupFile), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	wrapped = runShell(ctx, dir, dir, "cleanup", "set -e\nsource \"$LOOPER_RESOURCE_CLEANUP_FILE\"", resourceEnv(dir))
	cmd := exec.Command("bash", filepath.Join(dir, resourceCleanupFile))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LOOPER_ARTIFACT_DIR="+dir)
	plain = cmd.Run()
	return wrapped, plain
}

// TestCleanupWrapperAuditGuardIdiom reproduces the reported cleanup wrapper
// failure with stubs before any fix (AX-021): the generated wrapper runs the
// registered script under `set -e`, and the audit checks what that wrapper
// actually does to the documented check-before-remove idiom.
//
// Observed behavior, which the fix must match exactly:
//
//   - The guard idiom `[ -d path ] && rm -rf path` on an absent path does not
//     abort the script when commands follow it: bash exempts every part of an
//     AND list but its final command from `set -e`.
//   - The same idiom as the script's FINAL command exits 1 on a clean path,
//     and plain bash without the wrapper exits 1 identically: the status is
//     the script's own, not a wrapper artifact.
//   - A plain `rm` on an absent path aborts mid-script under the wrapper's
//     `set -e`, while plain bash continues and can mask the failure with a
//     later success. The wrapper keeps `set -e`: surfacing a failed removal
//     beats silently incomplete cleanup.
func TestCleanupWrapperAuditGuardIdiom(t *testing.T) {
	// The documented idiom, mid-script, with nothing left to remove.
	midScript := "[ -d \"$LOOPER_ARTIFACT_DIR/db\" ] && rm -rf \"$LOOPER_ARTIFACT_DIR/db\"\n" +
		"rm -f \"$LOOPER_ARTIFACT_DIR/database\"\n" +
		"echo cleaned > \"$LOOPER_ARTIFACT_DIR/cleanup-proof\"\n"
	wrapped, _ := cleanupAuditCase(t, "mid", midScript)
	if wrapped != nil {
		t.Fatalf("the documented guard idiom must survive mid-script on clean paths: %v", wrapped)
	}

	// The same idiom as the final command: the script reports its own status.
	finalScript := "rm -f \"$LOOPER_ARTIFACT_DIR/database\"\n" +
		"[ -d \"$LOOPER_ARTIFACT_DIR/db\" ] && rm -rf \"$LOOPER_ARTIFACT_DIR/db\"\n"
	wrapped, plain := cleanupAuditCase(t, "final", finalScript)
	if wrapped == nil || !strings.Contains(wrapped.Error(), "exit status 1") {
		t.Fatalf("the final-command idiom exits 1 under the wrapper: %v", wrapped)
	}
	if plain == nil || !strings.Contains(plain.Error(), "exit status 1") {
		t.Fatalf("plain bash exits 1 for the same script: set -e is not the cause: %v", plain)
	}

	// A non-idempotent removal aborts mid-script only under the wrapper.
	abortScript := "rm \"$LOOPER_ARTIFACT_DIR/database\"\necho done\n"
	wrapped, plain = cleanupAuditCase(t, "abort", abortScript)
	if wrapped == nil {
		t.Fatal("the wrapper must surface a failed removal")
	}
	if plain != nil {
		t.Fatalf("plain bash tolerates the absent path: %v", plain)
	}
}

// cleanupLeaseFixture registers one operation lease with the given cleanup
// script, exactly as a worker setup would, and returns the lease the sweep
// reads back from disk.
func cleanupLeaseFixture(t *testing.T, dir, script string) ResourceLease {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, resourceCleanupFile), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := resourceProcessIdentity(os.Getpid())
	if err != nil {
		t.Skipf("process identity unavailable: %v", err)
	}
	lease := ResourceLease{Version: 1, Directory: dir, Prefix: resourceEnv(dir)["LOOPER_RESOURCE_PREFIX"], PID: os.Getpid(), Identity: identity, Status: "retained"}
	if err = writeJSON(filepath.Join(dir, resourceLeaseFile), lease); err != nil {
		t.Fatal(err)
	}
	return lease
}

// failurePayloadOf decodes the failure summary one event journaled, beside
// the identity fields that ride at the top level of the same payload.
func failurePayloadOf(t *testing.T, ev Event) (FailureSummary, eventIdentity) {
	t.Helper()
	var payload struct {
		Failure FailureSummary `json:"failure"`
		eventIdentity
	}
	if err := json.Unmarshal(ev.Data, &payload); err != nil {
		t.Fatalf("event %d (%s) payload unreadable: %v (%s)", ev.ID, ev.Kind, err, ev.Data)
	}
	return payload.Failure, payload.eventIdentity
}

// TestCleanupFailuresDistinguishAbsentPathsFromExitFailures covers the AX-021
// acceptance: a cleanup that failed only on already-absent paths — the retry
// case for a script that finished its removals — and a cleanup that failed on
// a real error both journal bounded summaries, and the summaries distinguish
// the exit failure from the absent-path case by outcome, exit status, and
// excerpt.
func TestCleanupFailuresDistinguishAbsentPathsFromExitFailures(t *testing.T) {
	e, _ := diagnosisEngine(t, diagnosisProposal)
	absentDir := filepath.Join(e.Dir, "check-absentpaths")
	exitDir := filepath.Join(e.Dir, "check-realfailure")
	cleanupLeaseFixture(t, absentDir, "rm \"$LOOPER_ARTIFACT_DIR/database\"\n")
	cleanupLeaseFixture(t, exitDir, "echo \"rm: cannot remove 'db': Permission denied\" >&2\nexit 4\n")
	e.sweepResources()
	page, err := e.Store.EventPage(EventQuery{Kind: failureEventKind, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	summaries := map[string]FailureSummary{}
	notices := 0
	for _, ev := range page.Items {
		s, _ := failurePayloadOf(t, ev)
		summaries[s.Cause] = s
	}
	for _, ev := range eventsOf(t, e) {
		if ev.Kind == "resource-cleanup-error" {
			notices++
			if !strings.Contains(ev.Message, "Temporary resources remain in ") || !strings.Contains(ev.Message, "Cleanup retries at the next completion or restart.") {
				t.Fatalf("the cleanup notice message must stay unchanged: %s", ev.Message)
			}
		}
	}
	if notices != 2 {
		t.Fatalf("expected one unchanged notice per failed cleanup, got %d", notices)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected two distinct failure summaries, got %v", summaries)
	}
	absent, exit := false, false
	for _, s := range summaries {
		if s.Operation != opCleanup || s.Tool != resourceCleanupFile {
			t.Fatalf("cleanup summary identity wrong: %+v", s)
		}
		if len(s.Evidence) == 0 {
			t.Fatalf("cleanup summary carries no evidence: %+v", s)
		}
		switch {
		case s.Outcome == outcomeAbsent && s.ExitStatus != nil && *s.ExitStatus == 1:
			absent = true
			if !strings.Contains(s.Context, "No such file or directory") {
				t.Fatalf("the absent-path excerpt must name the missing target: %+v", s)
			}
		case s.Outcome == outcomeExit && s.ExitStatus != nil && *s.ExitStatus == 4:
			exit = true
			if !strings.Contains(s.Context, "Permission denied") {
				t.Fatalf("the exit-failure excerpt must name the denied removal: %+v", s)
			}
		default:
			t.Fatalf("unexpected cleanup summary: %+v", s)
		}
	}
	if !absent || !exit {
		t.Fatalf("the two cleanup failures must be distinguishable: absent %t exit %t (%v)", absent, exit, summaries)
	}
	// The leases stay cleanup_pending: an absent-path outcome reports what the
	// errors show, never a proof that every removal happened, so cleanup keeps
	// retrying instead of marking itself cleaned.
	for _, dir := range []string{absentDir, exitDir} {
		var lease ResourceLease
		if err = readJSON(filepath.Join(dir, resourceLeaseFile), &lease); err != nil {
			t.Fatal(err)
		}
		if lease.Status != "cleanup_pending" || lease.Error == "" {
			t.Fatalf("a failed cleanup must stay pending with its error: %+v", lease)
		}
	}
}

func eventsOf(t *testing.T, e *Engine) []Event {
	t.Helper()
	page, err := e.Store.EventPage(EventQuery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	return page.Items
}

// TestFailureContextBoundsAndScrubs pins the two bounds every summary keeps:
// an arbitrarily long log cannot overflow the excerpt, and credential shapes
// never survive into it.
func TestFailureContextBoundsAndScrubs(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "verify.log")
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&b, "filler output line %d with ordinary content\n", i)
	}
	b.WriteString("Authorization: Bearer sk-secretsecretsecret\n")
	b.WriteString("GITHUB_TOKEN=ghp_0123456789abcdefghijklmnop\n")
	b.WriteString("tail-marker: the failing assertion\n")
	if err := os.WriteFile(log, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
	context := boundedFailureContext(log)
	if len(context) > maxFailureContextBytes {
		t.Fatalf("context exceeds its bound: %d bytes", len(context))
	}
	if !strings.Contains(context, "tail-marker") {
		t.Fatalf("the tail keeps the failure the log ends with: %q", context)
	}
	for _, secret := range []string{"sk-secretsecretsecret", "ghp_0123456789", "Bearer sk-"} {
		if strings.Contains(context, secret) {
			t.Fatalf("credential shape %q survived into the excerpt: %q", secret, context)
		}
	}
	if !strings.Contains(context, "[redacted]") {
		t.Fatalf("the excerpt must mark redactions: %q", context)
	}
	if got := scrubSecrets("see https://user:hunter2@example.com/db and password=opensesame"); got != "see https://[redacted]@example.com/db and password=[redacted]" {
		t.Fatalf("url and assignment shapes must be scrubbed: %q", got)
	}
	// Encoded separators carry the same credential the plain space does: a
	// command line or URL can percent-encode or form-encode the space after
	// the scheme word, and the token must still go.
	for _, encoded := range []string{
		"Authorization=Bearer%20sk-secretsecretsecret",
		"Authorization=Bearer+sk-secretsecretsecret",
		"Authorization=Bearer\tsk-secretsecretsecret",
	} {
		if got := scrubSecrets(encoded); strings.Contains(got, "sk-secretsecretsecret") {
			t.Fatalf("encoded bearer shape survived scrubbing: %q -> %q", encoded, got)
		}
	}
	// Command arguments carry credentials outside assignments too: a curl
	// user flag and a hyphenated key header are credentials, not flags.
	for _, carrier := range []string{
		"curl -u deploy:hunter2-secret https://example.test/x",
		"curl --user=deploy:hunter2-secret https://example.test/x",
		"curl -H \"X-Api-Key: abcdef123456789\" https://example.test/x",
	} {
		if got := scrubSecrets(carrier); strings.Contains(got, "hunter2-secret") || strings.Contains(got, "abcdef123456789") {
			t.Fatalf("credential carrier survived scrubbing: %q -> %q", carrier, got)
		}
	}
	// The user-flag value must not swallow a URL: only the credential goes.
	if got := scrubSecrets("curl -u deploy:hunter2-secret https://example.test/x"); got != "curl -u [redacted] https://example.test/x" {
		t.Fatalf("the user flag redaction must keep the command shape: %q", got)
	}
	// A user flag without a colon is not a credential (git branch upstream).
	if got := scrubSecrets("git push -u origin main"); got != "git push -u origin main" {
		t.Fatalf("a colonless -u value is not a credential: %q", got)
	}
	if got := boundedContextOf([]byte("\x1b[31mred control text\x1b[0m\nsecond")); got != "red control text\nsecond" {
		t.Fatalf("escape sequences must be consumed: %q", got)
	}
}

// TestAgentFailureSummaryCarriesExitAndEvidence drives a real stub agent that
// exits 1 after reporting its own error: the summary names the worker
// operation, the agent identity, the exit status, the agent's reported cause,
// and the Looper-owned evidence. The raw session stream — including a tool
// input with a secret — never enters the summary.
func TestAgentFailureSummaryCarriesExitAndEvidence(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	script := `#!/bin/bash
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"SECRET=value do-work"}}]}}'
printf '%s\n' '{"type":"error","message":"stub provider outage"}'
exit 1
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	a := Agent{Config: Config{ClaudeBin: bin, RetryLimit: 1, RetryDelay: time.Millisecond}}
	_, err := a.Run(context.Background(), "claude", dir, dir, "prompt", true)
	var agent *AgentError
	if !errors.As(err, &agent) {
		t.Fatalf("expected AgentError, got %v", err)
	}
	if agent.Kind != "execution" {
		t.Fatalf("classification: %s", agent.Kind)
	}
	s := agent.Summary
	if s == nil {
		t.Fatal("worker failure carries no summary")
	}
	if s.Operation != opWorker || !strings.Contains(s.Tool, "claude") || !strings.Contains(s.Tool, bin) {
		t.Fatalf("summary identity wrong: %+v", s)
	}
	if s.ExitStatus == nil || *s.ExitStatus != 1 || s.Outcome != outcomeExit {
		t.Fatalf("exit status wrong: %+v", s)
	}
	if !strings.Contains(s.Cause, "stub provider outage") {
		t.Fatalf("the agent's own reported cause is missing: %+v", s)
	}
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "tool_use") {
		t.Fatalf("the raw session stream must not enter the summary: %s", raw)
	}
	var evidence bool
	for _, ev := range s.Evidence {
		if ev.Kind == "log" && strings.HasSuffix(ev.Reference, "events-00.jsonl") {
			evidence = true
		}
	}
	if !evidence {
		t.Fatalf("the summary must reference the retained event log: %+v", s.Evidence)
	}
	// The detail text keeps its historical shape beside the summary.
	if !strings.Contains(agent.Error(), "agent reported: stub provider outage") {
		t.Fatalf("detail text changed: %s", agent.Error())
	}
}

// TestWorkerHoldJournalsFailureSummary runs the real consumption path for a
// failed worker: the hold journals the bounded summary with the attempt
// identity, so the cause is retrievable without the private session file.
func TestWorkerHoldJournalsFailureSummary(t *testing.T) {
	e, a := diagnosisEngine(t, diagnosisProposal)
	one := 1
	failure := &AgentError{Kind: "execution", Detail: "exit status 1; see events-00.jsonl", Summary: (&FailureSummary{
		Operation: opWorker, Tool: "claude", ExitStatus: &one, Outcome: outcomeExit, Cause: "stub outage",
		Evidence: []FailureEvidence{{Kind: "log", Reference: filepath.Join(a.Directory, "events-00.jsonl")}},
	}).bound()}
	pending := []*Attempt{}
	if err := e.applyFinishedWorker(context.Background(), workerResult{ID: a.ID, Err: failure}, &pending); err != nil {
		t.Fatal(err)
	}
	if a.Phase != "blocked" {
		t.Fatalf("the attempt must stay held: %s", a.Phase)
	}
	page, err := e.Store.EventPage(EventQuery{Kind: failureEventKind, Task: a.Task.ID, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected one failure event, got %d", len(page.Items))
	}
	s, id := failurePayloadOf(t, page.Items[0])
	if id.TaskID != a.Task.ID || id.AttemptID != a.ID || id.Session != e.controller.Session {
		t.Fatalf("failure identity wrong: %+v", id)
	}
	if s.Operation != opWorker || s.ExitStatus == nil || *s.ExitStatus != 1 || s.Cause != "stub outage" {
		t.Fatalf("failure summary wrong: %+v", s)
	}
	if !strings.HasPrefix(page.Items[0].Message, a.Task.ID+"  worker failed: exited 1: stub outage") {
		t.Fatalf("failure message wrong: %q", page.Items[0].Message)
	}
}

// failureRunEngine builds a one-task finite run whose verification command
// fails on every attempt, so the run journals verification failure summaries
// and holds the task after its repairs.
func failureRunEngine(t *testing.T, verify string) (*Engine, Config) {
	t.Helper()
	bin, _ := fakeAgent(t, "{}")
	t.Setenv("LOOPER_FAKE_PLAN", fmt.Sprintf(`{"setup":"","verify":%q,"tasks":[{"task_id":"T1","write_paths":["."],"resources":[]}]}`, verify))
	t.Setenv("LOOPER_FAKE_SUMMARY", `{"task_id":"T1","status":"done","summary":"implemented","files":["T1.txt"],"blockers":[]}`)
	t.Setenv("LOOPER_FAKE_REVIEW", reviewComplete)
	c := repoConfig(t, "run")
	c.CodexBin = bin
	c.ClaudeBin = bin
	c.Once = true
	if err := os.WriteFile(filepath.Join(c.Root, "seed.txt"), []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "add", "seed.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "commit", "-qm", "fixture"); err != nil {
		t.Fatal(err)
	}
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "fail", Tasks: []Task{{ID: "T1", Title: "one", Priority: 1, Status: "todo"}}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	e.Config.MaxRepairs = 1
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	return e, c
}

// TestVerificationFailuresJournalBoundedSummaries runs the whole pipeline with
// a verification command that produces a long log with a secret and exits 1:
// every failed check journals a bounded summary with its identity, exit
// status, and scrubbed excerpt, retains failure.json beside its logs, and the
// summaries are retrievable in one focused query.
func TestVerificationFailuresJournalBoundedSummaries(t *testing.T) {
	verify := "for i in $(seq 1 600); do echo \"filler line $i\"; done; echo 'Authorization: Bearer sk-livelifelifelifelive'; echo 'tail-marker: assertion failed'; exit 1"
	e, c := failureRunEngine(t, verify)
	code, err := e.Run(context.Background())
	if code != 2 || err != nil {
		t.Fatalf("run: %d %v", code, err)
	}
	page, err := e.Store.EventPage(EventQuery{Kind: failureEventKind, Task: "T1", Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("expected one summary per failed check, got %d", len(page.Items))
	}
	for _, ev := range page.Items {
		s, id := failurePayloadOf(t, ev)
		if s.Operation != opVerify || s.Tool != "verify.sh" {
			t.Fatalf("verification summary identity wrong: %+v", s)
		}
		if s.ExitStatus == nil || *s.ExitStatus != 1 || s.Outcome != outcomeExit {
			t.Fatalf("verification exit status wrong: %+v", s)
		}
		if id.TaskID != "T1" || id.Phase != "verifying" || id.Session != e.controller.Session {
			t.Fatalf("verification failure identity wrong: %+v", id)
		}
		if len(s.Context) > maxFailureContextBytes {
			t.Fatalf("a long verify log overflowed the summary: %d bytes", len(s.Context))
		}
		if !strings.Contains(s.Context, "tail-marker") {
			t.Fatalf("the excerpt must keep the tail the failure ends with: %q", s.Context)
		}
		if strings.Contains(s.Context, "sk-livelifelifelifelive") {
			t.Fatalf("a credential shape entered the summary: %q", s.Context)
		}
		var log, script bool
		for _, ev := range s.Evidence {
			log = log || (ev.Kind == "log" && strings.HasSuffix(ev.Reference, "verify.log"))
			script = script || (ev.Kind == "file" && strings.HasSuffix(ev.Reference, "verify.sh"))
		}
		if !log || !script {
			t.Fatalf("the summary must reference the command and its log: %+v", s.Evidence)
		}
	}
	// The summary Looper journaled was retained beside the check logs. Checks
	// of an identical candidate commit share one directory and one record, so
	// the count tracks the distinct candidates the run verified.
	matches, _ := filepath.Glob(filepath.Join(e.Dir, "task-*", "checks", "*", failureFile))
	if len(matches) < 1 {
		t.Fatalf("expected the retained failure.json beside the failed check, got %v", matches)
	}
	for _, match := range matches {
		var retained FailureSummary
		if err := readJSON(match, &retained); err != nil {
			t.Fatalf("%s is unreadable: %v", match, err)
		}
		if retained.Operation != opVerify || retained.ExitStatus == nil || *retained.ExitStatus != 1 {
			t.Fatalf("retained summary wrong: %+v", retained)
		}
	}
	// The one focused query retrieves the summaries with their identity.
	schema, err := os.ReadFile(filepath.Join(legacyContractDocs, "schemas", "v2", "events-page.schema.json"))
	if err != nil {
		t.Skipf("events-page schema is absent: %v", err)
	}
	query := c
	query.Command, query.JSON, query.Contract, query.EventKind = "events", true, ContractVersionV2, failureEventKind
	doc := runEventsCommand(t, query)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = contractschema.ValidateJSON(schema, raw); err != nil {
		t.Fatalf("the failure page violates its schema: %v\n%s", err, raw)
	}
	if len(doc.Items) != 2 {
		t.Fatalf("the focused query returned %d items", len(doc.Items))
	}
	for _, item := range doc.Items {
		if item.Kind != failureEventKind || item.TaskID != "T1" {
			t.Fatalf("focused query item wrong: %+v", item)
		}
	}
}

// TestLauncherFailureJournalsSummaryAndMapsToErrorDoc covers the launcher
// class end to end: an agent binary that cannot start ends the run, the run
// journals a launch failure summary whose exit status is explicitly absent,
// and the version 2 error document names the class, the outcome, and the
// focused query instead of free-form text alone.
func TestLauncherFailureJournalsSummaryAndMapsToErrorDoc(t *testing.T) {
	c := repoConfig(t, "run")
	c.ClaudeBin = "/nonexistent/claude-agent"
	c.CodexBin = "/nonexistent/codex-agent"
	c.Once = true
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "launch", Tasks: []Task{{ID: "T1", Title: "one", Priority: 1, Status: "todo"}}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	code, runErr := e.Run(context.Background())
	if code != 1 || runErr == nil {
		t.Fatalf("run: %d %v", code, runErr)
	}
	page, err := e.Store.EventPage(EventQuery{Kind: failureEventKind, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	launches := 0
	for _, ev := range page.Items {
		s, _ := failurePayloadOf(t, ev)
		if s.Operation == opLaunch {
			launches++
			if s.ExitStatus != nil {
				t.Fatalf("a launch failure has no exit status to report: %+v", s)
			}
			if s.Outcome != outcomeLaunch {
				t.Fatalf("launch outcome wrong: %+v", s)
			}
		}
	}
	if launches == 0 {
		t.Fatalf("the run journaled no launch failure: %v", page.Items)
	}
	p := publicErrorFor(runErr)
	if p.Code != "launch_failed" {
		t.Fatalf("error code: %s (%s)", p.Code, p.Message)
	}
	if p.Message != runErr.Error() {
		t.Fatalf("the error message must keep its own text: %q", p.Message)
	}
	if p.Observed["outcome"] != outcomeLaunch || p.Observed["exit_status"] != nil {
		t.Fatalf("observed values wrong: %v", p.Observed)
	}
	if _, has := p.Observed["exit_status"]; has {
		t.Fatal("an absent exit status must stay absent, not null")
	}
	var route bool
	for _, ev := range p.Evidence {
		route = route || ev.Command == failureQueryCommand
	}
	if !route {
		t.Fatalf("the error document must name the focused query: %+v", p.Evidence)
	}
}

// TestWorkerLaunchFailureCarriesTaskIdentity covers the run-ending worker
// launch: the summary is journaled where the failure happened, with the
// attempt identity a task filter matches, and the run boundary does not
// repeat it without one.
func TestWorkerLaunchFailureCarriesTaskIdentity(t *testing.T) {
	bin, _ := fakeAgent(t, "{}")
	t.Setenv("LOOPER_FAKE_PLAN", `{"setup":"","verify":"true","tasks":[{"task_id":"T1","write_paths":["."],"resources":[]}]}`)
	c := repoConfig(t, "run")
	c.Agent, c.ReviewAgent = "claude", "codex"
	c.CodexBin = bin
	c.ClaudeBin = "/nonexistent/claude-agent"
	c.Once = true
	if err := os.WriteFile(filepath.Join(c.Root, "seed.txt"), []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "add", "seed.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "commit", "-qm", "fixture"); err != nil {
		t.Fatal(err)
	}
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "launch", Tasks: []Task{{ID: "T1", Title: "one", Priority: 1, Status: "todo"}}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	code, err := e.Run(context.Background())
	if code != 1 || err == nil {
		t.Fatalf("run: %d %v", code, err)
	}
	page, err := e.Store.EventPage(EventQuery{Kind: failureEventKind, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("the launch failure must journal exactly one summary, got %v", page.Items)
	}
	s, id := failurePayloadOf(t, page.Items[0])
	if s.Operation != opLaunch || s.Outcome != outcomeLaunch || s.ExitStatus != nil {
		t.Fatalf("worker launch summary wrong: %+v", s)
	}
	if id.TaskID != "T1" || id.AttemptID == "" || id.Session != e.controller.Session {
		t.Fatalf("the summary must carry the attempt identity a task filter matches: %+v", id)
	}
}

// TestWorkerActivityToolErrorExcerpt keeps the live activity useful: a failing
// tool's own error line rides beside the action, bounded and scrubbed, so
// severity is readable from the journal without the raw session log.
func TestWorkerActivityToolErrorExcerpt(t *testing.T) {
	line := []byte(`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"rm: cannot remove 'db': No such file or directory\nsecond line"}]}}` + "\n")
	actions := workerActivity(line)
	if len(actions) != 1 || actions[0] != "Tool reported an error: rm: cannot remove 'db': No such file or directory" {
		t.Fatalf("bounded first-line excerpt expected: %v", actions)
	}
	secret := []byte(`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":[{"type":"text","text":"upload failed for token=sk-abc123456789"}]}]}}` + "\n")
	actions = workerActivity(secret)
	if len(actions) != 1 || strings.Contains(actions[0], "sk-abc123456789") {
		t.Fatalf("tool error excerpt must scrub credentials: %v", actions)
	}
	if !strings.Contains(actions[0], "[redacted]") {
		t.Fatalf("tool error excerpt must mark the redaction: %v", actions)
	}
	// Tool inputs never enter the journal.
	input := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"SECRET=value do-work"}}]}}` + "\n")
	actions = workerActivity(input)
	if len(actions) != 1 || actions[0] != "Tool started: Bash" {
		t.Fatalf("tool input leaked or action changed: %v", actions)
	}
}
