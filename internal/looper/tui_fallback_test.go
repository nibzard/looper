package looper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// streamLines returns the non-empty lines a followed stream has written so far.
func streamLines(out *lockedBuilder) []string {
	var lines []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// waitForStreamLines polls a followed stream until want lines appeared.
func waitForStreamLines(t *testing.T, out *lockedBuilder, want int) []string {
	t.Helper()
	deadline := time.Now().Add(testBudget(20 * time.Second))
	for time.Now().Before(deadline) {
		if lines := streamLines(out); len(lines) >= want {
			return lines
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the followed stream produced fewer than %d lines: %s", want, out.String())
	return nil
}

// stopFollowedStream interrupts the fallback's followed stream and waits for
// its clean exit, the same signal the read-only commands install.
func stopFollowedStream(t *testing.T, done chan int) {
	t.Helper()
	syscall.Kill(os.Getpid(), syscall.SIGINT)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("an interrupted follow must exit 0, got %d", code)
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatal("the followed stream never stopped after SIGINT")
	}
}

// assertJSONEventLines checks that every streamed line is one stored event
// document, the shape `looper attach --json` must produce.
func assertJSONEventLines(t *testing.T, lines []string) {
	t.Helper()
	for _, line := range lines {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("the followed stream must carry JSON events, got %q", line)
		}
		if e.Kind == "" || e.Message == "" || e.ID == 0 {
			t.Fatalf("an event line is missing its fields: %q", line)
		}
	}
}

// TestAttachJSONFallbackStreamsJSONEvents pins T022's second acceptance:
// `looper attach --json` on a repository a controller owns follows the journal
// as newline-delimited JSON events, keeping the flag instead of re-parsing the
// bare `--tail --follow` command line that rendered plain text.
func TestAttachJSONFallbackStreamsJSONEvents(t *testing.T) {
	c := preparedRepo(t, "attach", "--json")
	_, dir := projectPaths(c)
	seedEventStore(t, filepath.Join(dir, "state.db"), 3)
	writeControllerMetadata(t, dir, "run-live", newControllerCapabilities())
	release := holdRepositoryLock(t, c)

	out := &lockedBuilder{}
	var stderr strings.Builder
	done := make(chan int, 1)
	go func() { done <- runAttach(c, out, &stderr, false) }()
	assertJSONEventLines(t, waitForStreamLines(t, out, 1))
	stopFollowedStream(t, done)
	release()
	if !strings.Contains(stderr.String(), "following events instead") {
		t.Fatalf("expected the attach fallback notice: %s", stderr.String())
	}
}

// TestTUIOwnedJSONFallbackStreamsJSONEvents covers the owned-repository tui
// fallback: `looper tui --json` follows the same JSON event stream, because
// the fallback threads the parsed configuration instead of dropping its flags.
func TestTUIOwnedJSONFallbackStreamsJSONEvents(t *testing.T) {
	c := preparedRepo(t, "tui", "--json")
	_, dir := projectPaths(c)
	seedEventStore(t, filepath.Join(dir, "state.db"), 3)
	release := holdRepositoryLock(t, c)

	out := &lockedBuilder{}
	var stderr strings.Builder
	done := make(chan int, 1)
	go func() { done <- runTUI([]string{"tui", "--json"}, c, out, &stderr) }()
	assertJSONEventLines(t, waitForStreamLines(t, out, 1))
	stopFollowedStream(t, done)
	release()
	if !strings.Contains(stderr.String(), "following events instead") {
		t.Fatalf("expected the tui fallback notice: %s", stderr.String())
	}
}

// waitForCondition polls until check holds or the test budget ends. Streams
// the fallback owns write asynchronously, so a test observes them by polling,
// never by sleeping a fixed interval.
func waitForCondition(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(testBudget(20 * time.Second))
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never happened", what)
}

// receiptLineFrom returns the one line of text that carries the receipt
// document, so a test can parse exactly what a caller would parse.
func receiptLineFrom(text string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, requestReceiptKind) {
			return line, true
		}
	}
	return "", false
}

// ownedFallbackRepo prepares the shared T116 scenario: a repository whose
// controller metadata advertises add-plan, a followed event store, and one
// startup plan. The caller holds the repository lock to make the ownership
// real.
func ownedFallbackRepo(t *testing.T, extraArgs ...string) (Config, string) {
	t.Helper()
	c := preparedRepo(t, extraArgs...)
	c.Plans = []string{planFile(t, "# Additional requirements\nAdd telemetry to the service.")}
	_, dir := projectPaths(c)
	seedEventStore(t, filepath.Join(dir, "state.db"), 3)
	writeControllerMetadata(t, dir, "run-live", newControllerCapabilities())
	return c, dir
}

// startupPlanEnvelope reads the single submitted startup-plan envelope and
// checks it against the submission, so each test can match its receipt
// against the durable request.
func startupPlanEnvelope(t *testing.T, dir string, plans []string) planImportRequest {
	t.Helper()
	entries, err := os.ReadDir(requestDir(dir))
	if err != nil {
		t.Fatalf("the startup-plan envelope is not durable: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one submitted envelope, got %d", len(entries))
	}
	var req planImportRequest
	if err = readJSON(filepath.Join(requestDir(dir), entries[0].Name()), &req); err != nil {
		t.Fatal(err)
	}
	if req.Session != "run-live" || strings.Join(req.Paths, ",") != strings.Join(plans, ",") {
		t.Fatalf("the envelope does not match the submission: %+v (plans %s)", req, strings.Join(plans, ","))
	}
	return req
}

// TestTUIOwnedFallbackSubmitsStartupPlans pins T116: the owned-repository
// non-interactive tui fallback submits --plan to the active controller before
// following events. The immediate receipt prints on stderr, so the followed
// JSON event stream on stdout stays one stored event per line while the
// receipt itself stays one parseable document naming the durable import.
func TestTUIOwnedFallbackSubmitsStartupPlans(t *testing.T) {
	c, dir := ownedFallbackRepo(t, "tui", "--json")
	release := holdRepositoryLock(t, c)

	out := &lockedBuilder{}
	errOut := &lockedBuilder{}
	done := make(chan int, 1)
	go func() { done <- runTUI([]string{"tui", "--json"}, c, out, errOut) }()
	waitForCondition(t, "the startup-plan receipt", func() bool {
		_, ok := receiptLineFrom(errOut.String())
		return ok
	})
	line, _ := receiptLineFrom(errOut.String())
	var receipt requestReceiptDocument
	if err := json.Unmarshal([]byte(line), &receipt); err != nil {
		t.Fatalf("the fallback receipt must be one parseable document: %v (stderr: %s)", err, errOut.String())
	}
	if receipt.Kind != requestReceiptKind || receipt.Request.Type != PublicRequestAddPlan ||
		receipt.Request.State != RequestSubmitted || receipt.Request.ControllerSession != "run-live" {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	if receipt.Query.Command == "" {
		t.Fatal("the receipt must name the command that queries the outcome")
	}
	envelope := startupPlanEnvelope(t, dir, c.Plans)
	if envelope.ID != receipt.Request.ID {
		t.Fatalf("the receipt names %s but the envelope is %s", receipt.Request.ID, envelope.ID)
	}
	// The followed stream stays pure: every stdout line is one stored event
	// document, and the receipt never leaks onto it.
	assertJSONEventLines(t, waitForStreamLines(t, out, 1))
	if strings.Contains(out.String(), requestReceiptKind) {
		t.Fatalf("the receipt polluted the followed JSON event stream: %s", out.String())
	}
	stopFollowedStream(t, done)
	release()
	if !strings.Contains(errOut.String(), "following events instead") {
		t.Fatalf("expected the tui fallback notice: %s", errOut.String())
	}
}

// TestTUIOwnedFallbackSubmitsStartupPlansPlain pins the plain-text half of
// T116: the receipt renders as text on stderr, and the followed plain event
// lines on stdout stay free of it.
func TestTUIOwnedFallbackSubmitsStartupPlansPlain(t *testing.T) {
	c, dir := ownedFallbackRepo(t, "tui", "--plain")
	release := holdRepositoryLock(t, c)

	out := &lockedBuilder{}
	errOut := &lockedBuilder{}
	done := make(chan int, 1)
	go func() { done <- runTUI([]string{"tui", "--plain"}, c, out, errOut) }()
	waitForCondition(t, "the plain startup-plan receipt", func() bool {
		return strings.Contains(errOut.String(), "Queued "+strings.Join(c.Plans, ", "))
	})
	if !strings.Contains(errOut.String(), "submitted to controller session run-live") {
		t.Fatalf("the receipt must name the owning controller session: %s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "Query it with: looper requests import-") {
		t.Fatalf("the receipt must name the query command: %s", errOut.String())
	}
	envelope := startupPlanEnvelope(t, dir, c.Plans)
	if !strings.Contains(errOut.String(), "Request "+envelope.ID+" ") {
		t.Fatalf("the plain receipt must name the envelope it published: %s", errOut.String())
	}
	for _, line := range waitForStreamLines(t, out, 1) {
		if strings.Contains(line, "Queued ") || strings.Contains(line, "Request import-") {
			t.Fatalf("the receipt polluted the plain event stream: %q", line)
		}
	}
	stopFollowedStream(t, done)
	release()
}

// TestTUIOwnedFallbackRefusesInsteadOfDroppingPlans pins T116's failure half:
// an owned repository whose controller metadata cannot be verified fails the
// submission loudly, without following events and without publishing an
// envelope, instead of silently dropping the plans like the old fallback.
func TestTUIOwnedFallbackRefusesInsteadOfDroppingPlans(t *testing.T) {
	c := preparedRepo(t, "tui", "--json")
	c.Plans = []string{planFile(t, "# Additional requirements\nAdd telemetry to the service.")}
	_, dir := projectPaths(c)
	seedEventStore(t, filepath.Join(dir, "state.db"), 3)
	// The lock claims ownership, but no controller metadata exists to address.
	release := holdRepositoryLock(t, c)

	out := &lockedBuilder{}
	var stderr strings.Builder
	done := make(chan int, 1)
	go func() { done <- runTUI([]string{"tui", "--json"}, c, out, &stderr) }()
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("an unverifiable controller must fail the submission, got exit %d", code)
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatal("the fallback never answered the failed submission")
	}
	release()
	if !strings.Contains(stderr.String(), "metadata cannot be verified") {
		t.Fatalf("expected the metadata failure: %s", stderr.String())
	}
	if text := out.String(); text != "" {
		t.Fatalf("no events may follow a refused submission: %s", text)
	}
	if entries, err := os.ReadDir(requestDir(dir)); err == nil && len(entries) > 0 {
		t.Fatalf("no envelope may be published for a refused submission: %v", entries)
	}
}

// deferredAdditionProposal makes the imported task deferred, so the applied
// addition never dispatches a worker and the run ends without agent-driven
// work beyond the import analysis and the review pass.
const deferredAdditionProposal = `{"additions":[{"id":"P1","title":"Add telemetry","priority":2,"status":"todo","details":"Acceptance: telemetry endpoint returns counters.","depends_on":[],"tags":["looper:deferred"]}],"covered":[],"conflicts":[]}`

// fallbackOutcome is one side of the tui-versus-run comparison: the exit code,
// the ordered journal kinds, the start and configuration events, and the
// exported backlog.
type fallbackOutcome struct {
	code    int
	kinds   []string
	start   string
	config  string
	backlog Backlog
}

// TestTUIFallbackAppliesPlanAndJobsLikeRun pins T022's first acceptance: on a
// non-terminal stream, `looper tui --plan p.md --jobs 2` applies the plan and
// the job count exactly like `looper run`, because the fallback threads the
// parsed configuration into the same CLI controller.
func TestTUIFallbackAppliesPlanAndJobsLikeRun(t *testing.T) {
	bin, _ := fakeAgent(t, deferredAdditionProposal)
	t.Setenv("LOOPER_FAKE_REVIEW", reviewComplete)
	plan := planFile(t, "# Additional requirements\nAdd telemetry to the service.")

	run := func(command string) fallbackOutcome {
		args := []string{command, "--plan", plan, "--jobs", "2", "--once", "--plain", "--verbose"}
		c := repoConfig(t, args...)
		// The entry point re-parses the arguments, so the fake agents are
		// selected through the environment both invocations read.
		t.Setenv("CODEX_BIN", bin)
		t.Setenv("CLAUDE_BIN", bin)
		todo := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{
			{ID: "T001", Title: "existing", Priority: 1, Status: "done", Tags: []string{"project-done"}},
		}}
		if err := writeJSON(c.Todo, todo); err != nil {
			t.Fatal(err)
		}
		var out, stderr strings.Builder
		// The entry point runs under a watchdog: a fallback that drops --once
		// keeps its controller waiting for work forever, and the test must
		// report that as a flag-preservation failure, not hang.
		finished := make(chan int, 1)
		go func() { finished <- Main(args, &out, &stderr) }()
		var code int
		select {
		case code = <-finished:
		case <-time.After(testBudget(2 * time.Minute)):
			t.Fatalf("%s never finished; its fallback must keep --once and the parsed flags (stderr: %s)", command, stderr.String())
		}
		outcome := fallbackOutcome{code: code}
		if command == "tui" && !strings.Contains(stderr.String(), "running with CLI output") {
			t.Fatalf("the tui fallback must report its CLI fallback: %s", stderr.String())
		}
		_, dir := projectPaths(c)
		store, err := openObservedStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		rows, err := store.db.Query("SELECT kind, message FROM events ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var kind, message string
			if err := rows.Scan(&kind, &message); err != nil {
				t.Fatal(err)
			}
			outcome.kinds = append(outcome.kinds, kind)
			switch kind {
			case "start":
				outcome.start = message
			case "config":
				outcome.config = message
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := readJSON(c.Todo, &outcome.backlog); err != nil {
			t.Fatal(err)
		}
		return outcome
	}

	runSide := run("run")
	tuiSide := run("tui")
	if tuiSide.code != runSide.code {
		t.Fatalf("the tui fallback exited %d; looper run exited %d", tuiSide.code, runSide.code)
	}
	if strings.Join(tuiSide.kinds, ",") != strings.Join(runSide.kinds, ",") {
		t.Fatalf("journal kinds diverge between tui and run:\ntui: %s\nrun: %s",
			strings.Join(tuiSide.kinds, ","), strings.Join(runSide.kinds, ","))
	}
	// The job flag reached the controller: the start banner reports the
	// requested concurrency, not the automatic default a re-parse rebuilt.
	for _, side := range []struct{ name, start, config string }{
		{"run", runSide.start, runSide.config}, {"tui", tuiSide.start, tuiSide.config},
	} {
		if !strings.Contains(side.start, "up to 2 implementing") {
			t.Fatalf("%s start banner lost the job flag: %s", side.name, side.start)
		}
		if !strings.Contains(side.config, "jobs 2") {
			t.Fatalf("%s configuration lost the job flag: %s", side.name, side.config)
		}
	}
	// The plan applied on both sides: the imported task is blocked by its
	// deferred condition and never dispatched.
	for _, side := range []struct {
		name    string
		backlog Backlog
	}{{"run", runSide.backlog}, {"tui", tuiSide.backlog}} {
		task := side.backlog.Task("T002")
		if task == nil || task.Status != "blocked" || !strings.Contains(task.Title, "telemetry") {
			t.Fatalf("%s did not apply the startup plan: %+v", side.name, side.backlog.Tasks)
		}
		if !task.Deferred() {
			t.Fatalf("%s applied the addition without its deferral: %+v", side.name, task)
		}
	}
}
