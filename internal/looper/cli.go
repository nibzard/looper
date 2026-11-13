package looper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var Version = "dev"

const help = `Looper — continuous local development

Usage: looper [run] [task-file] [options]
       looper start [--plan <path>...]
       looper tui [--plan <path>...]
       looper attach
       looper add-task <file.json> [--json] [--wait]
       looper add-plan <path> [...] [--wait]
       looper cancel <task-id> [--cascade] [--json] [--wait]
       looper amend <task-id> <task.json> [--json] [--wait]
       looper status [--json] [--full]
       looper runs [--limit N] [--json]
       looper requests [id] [--after <request-id>] [--limit N]
       looper wait (--cursor N | --task T [--status S] | --request ID | --run)
                   --timeout <seconds>
       looper stop [--wait]
       looper tail [--follow] [--after <event-id>] [--limit N]
       looper events [--json] [--follow] [--after <event-id>] [--limit N]
                     [--task <task-id>] [--kind <kind>] [--full]
       looper inspect <task-id> [--json] [--after N] [--checks-after N]
                     [--limit N] [--full]
       looper recovery --preview [--json]
       looper recovery --apply <choice> --token <token> [--json] [--wait]
       looper doctor [--json]
       looper preflight [--contract v2] [--json]
       looper version [--json] [--contract legacy|v2]
       looper config [--reset] [--wait]
       looper help [<command>]

Run again to resume retained work. The default run waits for new work when idle.
--once finishes after the current goal is checked or all remaining work is blocked.
Pass a command name for its own help block: looper help status, or looper
status --help. A word that names no command and no file is a usage error, so a
mistyped command cannot start a run.
Mutations return an immediate receipt naming the durable request — add-task,
add-plan, cancel, amend, config, and stop publish the request and return at once; query the
outcome with looper requests <id>. --wait waits explicitly and bounded
(--wait-timeout <seconds>, default 30; add-plan waits up to 60) for the
applied acknowledgement, and its timeout leaves the request running.
--idempotency-key makes a mutation retry-safe: the same key and content
returns the original request, changed content under the same key is
rejected, and distinct keys with identical content both apply.
looper cancel <task-id> cancels open work through a checked request: the
task revision the caller read must match, live dependents are refused by
name until --cascade takes them along, and a task with active work records
a pending cancellation that applies when the work finishes — accepted
results are never discarded, and cancelled work is never marked done.
looper amend <task-id> <task.json> amends a task contract through a checked
request: the file carries the complete amended task, its revision must match
what the caller read, and dependency cycles and invalid write paths are
refused before anything commits. Work that holds the task — running,
verifying, or retained — is refused with the boundary named, because its
contract cannot change under a worker. A done task is never rewritten: the
changed requirements create a follow-up task and the accepted result stays.
looper recovery --apply <choice> --token <token> applies one choice the
recovery preview enumerated: restore-task-file returns the accepted bytes,
import-backlog applies the external edit (equivalent formatting is adopted,
a real edit imports through the retained-backlog import with every retained
result replaying). A live controller applies the request at its boundary; a
stopped run applies under the repository lock without launching agents. The
token is re-derived first, so a stale preview is refused without mutation,
and unsupported situations keep the retained work and report the limitation.
looper runs lists the saved runs under LOOPER_BASE_DIR (default: ~/.looper)
with their project roots, retained work, last outcomes, controller freshness,
and build evidence, and names the command that selects each; it changes
nothing and never scans private agent session directories.
looper requests reads the run's durable request records — add-task, add-plan,
settings, requeue, and stop submissions with their state, age, wait reason,
affected tasks, plan coverage, and error evidence — one record by identifier
or one bounded page; the --contract v2 --json document stays bounded and
answers without reading the acknowledgement directory or the state database.
looper preflight shows the execution configuration a start would use — agents,
models and their sources, executables, verify and setup, launch mode, once
semantics — without starting a controller or an agent.
looper wait observes one predicate for an explicit finite bound and returns one
outcome document: --cursor N until events are committed past that position,
--task T [--status S] until the task reaches the status (any change without
--status), --request ID until the durable request reaches a terminal state, or
--run until the bound controller session ends. A timeout is an outcome of the
wait (exit 5) and cancels nothing; an interruption is exit 6 with the cursor
reached. Streaming callers keep looper events --follow.
The first interactive run configures agent commands and project roles.
Run looper config to edit machine agents and runtime/profile.json.

Options:
  --plan <path>          Add Markdown requirements at startup; repeatable
  --jobs auto|N          Maximum concurrent workers (default: auto)
  --check-jobs N         Concurrent setup and checks (default: 1)
  --agent NAME           Worker agent profile (built-ins: codex, claude, zcode)
  --review-agent NAME   Planning and review agent (default: coding agent)
  --model NAME          Explicit model; otherwise use the agent's configuration
  --effort NAME         Codex reasoning effort
  --max-iterations N    Dispatch limit; 0 is unlimited (default: 0)
  --review-every N      Check product behavior after N integrations (default: 5)
  --limit N             Events per page for events/tail (default: 50; max: 500);
                    attempts or checks per page for inspect (max: 100);
                    runs per page for runs and requests (default: 20; max: 100)
  --after <id>          Continue after an event ID (events, tail), after an
                    attempt number (inspect), or after a request ID (requests)
  --checks-after <id>   Continue inspect checks after a check ID
  --task <task-id>      Only events whose recorded payload names this task, or
                    the task one wait observes
  --kind <kind>         Only events of this kind
  --cursor <event-id>   Wait until events are committed past this position
                    (looper wait)
  --status <status>     Target status of one task wait: todo, doing, blocked,
                    done, or cancelled (looper wait --task)
  --request <request-id> Wait until this durable request reaches a terminal
                    state (looper wait)
  --run                 Wait until the bound controller session ends
                    (looper wait)
  --timeout <seconds>   Explicit bound of one wait, at least one second
                    (looper wait); a bare number counts seconds and 30s names
                    its unit
  --full                Complete event or inspect history, or the complete
                    status document instead of the compact summary
  --preview            Read-only recovery preview for a saved run (looper
                    recovery); changes nothing
  --apply <choice>     Apply one checked recovery choice (looper recovery):
                    restore-task-file or import-backlog; the controller or
                    the stopped-run lock re-checks the preview token first
  --token <token>      Preview token that binds one application to the state
                    it observed (looper recovery --apply)
  --once                Exit when no runnable work remains after review
  --cascade             Cancel the live dependents of the cancelled task too
                    (looper cancel)
  --wait                Wait for the mutation's outcome (add-task, add-plan,
                    cancel, amend, config, recovery, stop); bounded by
                    --wait-timeout; stop's wait ends when the drain's durable
                    result lands, bound to the stopped controller session
  --wait-timeout <s>    Seconds one --wait may run (default 30; add-plan 60);
                    a timeout leaves the request running
  --idempotency-key <k> Caller key that makes one mutation retry-safe
                    (add-task, add-plan, cancel, amend, config, recovery): same
                    key and content returns the original request, changed
                    content is rejected; 1-128 bytes, no control characters
  --plain               Plain text only; no forms, color, or glyphs
  --no-animation        Dashboard without animated motion; status stays readable
  --yes                 Skip the first-run form and use defaults
  --json                Emit machine-readable progress
  --verbose             Show the run configuration at start
  --contract legacy|v2  Machine contract version for this command (default: legacy);
                    version 2 returns one structured error document for any failure
  --version             Show the executable version; same as the version command
  --help                Show this overview; looper help <command> shows one
                    command's examples, documents, and semantics

Project commands can be supplied through LOOPER_SETUP_COMMAND,
LOOPER_VERIFY_COMMAND, and optional LOOPER_ASSESS_COMMAND.
Runtime records live in LOOPER_BASE_DIR (default: ~/.looper).
Git commands time out after LOOPER_GIT_TIMEOUT_SECONDS (default: 600; 0 waits).
The machine contract version can also be selected with LOOPER_CONTRACT; see
docs/AGENT-CONTRACT-MIGRATION.md for the versioned documents and the
compatibility policy.
`

func Main(args []string, out, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "__exec" {
		return RunChild(args[1:])
	}
	if len(args) > 0 && args[0] == "__daemon" {
		if len(args) < 2 {
			fmt.Fprintln(stderr, "the daemon entry point needs a configuration path")
			return 2
		}
		ready := io.Writer(os.Stderr)
		if f := os.NewFile(3, "daemon-readiness"); f != nil {
			ready = f
		}
		return RunDaemon(args[1], ready)
	}

	c, err := Parse(args)
	if err != nil {
		return emitParseError(args, err, out, stderr)
	}
	if c.Command == "help" {
		// Contextual help: a topic names one command's block, and no topic
		// serves the overview (AX-026). The topic is validated during
		// parsing, so the block always exists here.
		if c.HelpTopic != "" {
			fmt.Fprint(out, renderCommandHelp(c.HelpTopic))
			return 0
		}
		fmt.Fprint(out, help)
		return 0
	}
	if c.Command == "version" {
		if c.Contract == ContractVersionV2 {
			if c.JSON {
				json.NewEncoder(out).Encode(capabilitiesDocumentFor(c))
			} else {
				printCapabilitiesSummary(c, out)
			}
			return 0
		}
		if c.JSON {
			json.NewEncoder(out).Encode(currentBuild())
		} else {
			fmt.Fprintln(out, currentBuild())
		}
		return 0
	}
	switch c.Command {
	case "add-task":
		return runAddTask(c, out, stderr)
	case "add-plan":
		return runAddPlan(c, out, stderr)
	case "cancel":
		return runCancel(c, out, stderr)
	case "amend":
		return runAmend(c, out, stderr)
	case "tui":
		return runTUI(args, c, out, stderr)
	case "attach":
		if len(c.Plans) > 0 {
			p := usageFailure("usage_invalid", "read-only attach does not accept --plan; use looper tui or looper add-plan")
			return emitError(c, p, out, stderr)
		}
		return runAttach(c, out, stderr, false)
	}
	// Only read-only commands remain in this body. The controller commands
	// install their own handlers: runCLIController for run and start, and
	// runDashboardController for the tui path dispatched above.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if c.Command == "doctor" {
		if c.JSON {
			return doctorJSON(c, out)
		}
		return doctor(c, out)
	}
	if c.Command == "config" {
		return runConfig(c, os.Stdin, out, stderr)
	}
	if c.Command == "preflight" {
		return runPreflight(c, out, stderr)
	}
	if c.Command == "runs" {
		return runRunsListing(c, out, stderr)
	}
	if c.Command == "requests" {
		return runRequestsQuery(c, out, stderr)
	}
	if c.Command == "wait" {
		return runWait(ctx, c, out, stderr)
	}
	_, dir := projectPaths(c)
	if c.Command == "stop" {
		return runStop(c, dir, out, stderr)
	}
	if c.Command == "status" || c.Command == "tail" || c.Command == "events" || c.Command == "inspect" {
		if err = inspect(ctx, c, out); err != nil {
			return emitError(c, err, out, stderr)
		}
		return 0
	}
	if c.Command == "recovery" {
		if !c.Preview {
			// The checked application (AX-017): the preview stays the
			// read-only operation above, and an application routes through
			// the controller request protocol or the stopped-run lock.
			return runRecoveryApply(c, out, stderr)
		}
		if err = runRecoveryPreview(c, out); err != nil {
			return emitError(c, err, out, stderr)
		}
		return 0
	}
	return runCLIController(args, c, out, stderr)
}

// runCLIController starts a controller with CLI output for the run and start
// commands. The tui fallback reaches it with the parsed configuration, so an
// invocation like `looper tui --plain` keeps its flags instead of re-parsing
// a bare command line. SIGTERM drains like `looper stop`; SIGINT cancels with
// retained work.
func runCLIController(args []string, c Config, out, stderr io.Writer) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	installControllerSignals(c, cancel)
	var start bool
	var err error
	c, start, err = maybeFirstRun(args, c, os.Stdin, out, stderr)
	if err != nil {
		return emitParseError(args, err, out, stderr)
	}
	if !start {
		return 0
	}
	defaultAgentNotice(c, out)
	if c.Command == "start" {
		// The detached path refuses startup plans under a live controller
		// before it spawns, so the owner never receives a doomed child and
		// the plans are never dropped without the add-plan suggestion.
		if code, refused := refusePlansUnderOwner(c, out, stderr); refused {
			return code
		}
		return StartDetached(c, out, stderr)
	}
	// The foreground launch description precedes any controller, agent, or
	// runtime record: it reports what a start is about to use (AX-023).
	emitLaunchDescription(c, out)
	if code, refused := refusePlansUnderOwner(c, out, stderr); refused {
		return code
	}
	profileAtStart := profileDigest(c.ProfilePath)
	e, lock, err := NewEngine(c, out)
	if err != nil {
		return emitError(c, err, out, stderr)
	}
	defer lock.Close()
	defer e.Store.Close()
	// Recheck after acquiring the lock: a profile written while the form or
	// startup ran is reported, never silently replaced.
	if profileAtStart != "" && profileDigest(c.ProfilePath) != profileAtStart && !c.JSON {
		fmt.Fprintln(out, "Note: the saved profile changed while this run was starting; the new values apply through looper config or the next run.")
	}
	code, err := e.Run(ctx)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "Interrupted. Work is retained; run looper to resume.")
		} else if c.Contract == ContractVersionV2 {
			return emitError(c, err, out, stderr)
		} else {
			fmt.Fprintln(stderr, "Error:", err)
		}
	}
	return code
}

// refusePlansUnderOwner reports the double-start refusal both launch paths
// share: a live controller cannot adopt startup plans, so the caller learns
// the plans were not submitted and how to submit them (docs/UI-PLAN.md). The
// run path and the start path emit the same suggestion, and the second return
// is false when the invocation may proceed.
func refusePlansUnderOwner(c Config, out, stderr io.Writer) (int, bool) {
	if len(c.Plans) == 0 || !repositoryOwned(c) {
		return 0, false
	}
	p := operationFailure("controller_owns_repository", "A controller already owns this repository.")
	p.Actions = []ErrorAction{{
		Description: "Submit the plans to the active controller instead of starting another one.",
		Command:     "looper add-plan <path>",
	}}
	p.withLegacy("A controller already owns this repository.\nSubmit the plans with: looper add-plan <path>", 1)
	return emitError(c, p, out, stderr), true
}

// runStop requests a drain from the controller that owns the repository and
// returns the immediate receipt (AX-011). A stop without a live owner must
// not affect a future run: the request binds to the owning session, and
// nothing is written while no controller holds the repository lock. The
// version 2 response names the stop request's derived identity. --wait waits
// for the durable stop result instead: the finite, session-bound drain wait
// AX-022 owns.
func runStop(c Config, dir string, out, stderr io.Writer) int {
	if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
		p := runNotFoundForRun(c, filepath.Join(dir, "state.db"), "looper")
		p.withLegacy("No saved Looper run exists in this project.", 1)
		return emitError(c, p, out, stderr)
	}
	if !repositoryOwned(c) {
		p := operationFailure("controller_not_running", "No controller is running. Nothing was stopped; a later start runs normally.")
		p.Retry = retryNo
		p.Observed = map[string]any{"controller_owned": false}
		p.Actions = []ErrorAction{{Description: "Start a controller when a run is wanted.", Command: "looper"}}
		p.withLegacyStdout("No controller is running. Nothing was stopped; a later start runs normally.", 1)
		return emitError(c, p, out, stderr)
	}
	if c.Interrupt {
		return runStopInterrupt(c, dir, out, stderr)
	}
	req := stopRequest{}
	if info, err := readController(dir); err == nil {
		// Refuse before publication when the live controller advertises that
		// it never answers stop requests; no stop file is written.
		if err = validateControllerRequest(info, PublicRequestStop); err != nil {
			return emitError(c, err, out, stderr)
		}
		req.Session, req.PID = info.Session, info.PID
	}
	if err := writeStopRequest(dir, req); err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	stoppedEarly := func() bool {
		// The owner can exit between the ownership probe and the write.
		// Its shutdown cleanup consumes the request, so report that
		// outcome instead of promising a drain nothing will perform.
		if req.Session == "" {
			return false
		}
		info, err := readController(dir)
		return err != nil || info.Session != req.Session
	}
	if c.Contract == ContractVersionV2 {
		if stoppedEarly() {
			fmt.Fprintln(out, "The controller stopped while the request was written. No run is left draining.")
			return 0
		}
		if c.Wait {
			return reportStopWait(c, dir, &req, out, stderr)
		}
		return emitMutationReceipt(c,
			newMutationReceipt(stopRequestID(&req), PublicRequestStop, RequestSubmitted,
				time.Now().UTC().Format(time.RFC3339Nano), req.Session),
			"", "Active work will drain; pass --wait to wait for the durable stop result.", out)
	}
	if stoppedEarly() {
		fmt.Fprintln(out, "The controller stopped while the request was written. No run is left draining.")
		return 0
	}
	if !c.Wait {
		if req.Session != "" {
			fmt.Fprintf(out, "Stop requested for session %s. Active work will drain.\n", req.Session)
			return 0
		}
		// The lock is held but controller metadata is unreadable: the live owner
		// still honors a request that arrived during its lifetime.
		fmt.Fprintln(out, "Stop requested. Active work will drain.")
		return 0
	}
	// The legacy wait keeps the frozen submission line, then reports the
	// durable outcome the bounded wait observed.
	if req.Session != "" {
		fmt.Fprintf(out, "Stop requested for session %s. Active work will drain.\n", req.Session)
	} else {
		fmt.Fprintln(out, "Stop requested. Active work will drain.")
	}
	return reportStopWait(c, dir, &req, out, stderr)
}

// runStopInterrupt sends the owning controller the same interrupt as Ctrl+C:
// child processes stop and the controller retains partial work for the next
// run. signalController checks the recorded session and process start
// identity first, so a reused process ID is never signaled.
func runStopInterrupt(c Config, dir string, out, stderr io.Writer) int {
	if c.Contract == ContractVersionV2 {
		p := usageFailure("option_conflict", "--interrupt has no version 2 document; omit --contract v2 and run looper status to observe the exit")
		p.Observed = map[string]any{"--interrupt": true, "--contract": "v2"}
		return emitError(c, p, out, stderr)
	}
	info, err := readController(dir)
	if err == nil {
		err = signalController(dir, syscall.SIGINT)
	}
	if err != nil {
		p := operationFailure("controller_not_signaled", "The controller could not be interrupted: "+err.Error())
		p.Actions = []ErrorAction{{Description: "Request a drain instead.", Command: "looper stop"}}
		p.withLegacy("The controller could not be interrupted: "+err.Error(), 1)
		return emitError(c, p, out, stderr)
	}
	fmt.Fprintf(out, "Interrupt sent to session %s. Active work stops and is retained; run looper to resume.\n", info.Session)
	return 0
}

// stopResultPollInterval is the observation cadence of one durable stop wait.
const stopResultPollInterval = 100 * time.Millisecond

// reportStopWait waits for the durable stop result of the request runStop
// published (AX-022). The wait is finite and bound to the controller session
// the request targeted: only that request's terminal receipt ends it — the
// honored drain, the controller's own exit before honoring, or a replacement
// controller's startup sweep — so a new run never substitutes its state for
// the answer and is never stopped by this wait. A timeout is an outcome of
// the wait: the drain keeps running and the stop request stays durable.
func reportStopWait(c Config, dir string, req *stopRequest, out, stderr io.Writer) int {
	rec, err := waitForStopResult(dir, req, c.waitBound())
	if err != nil {
		return emitError(c, requestSubmitError(err), out, stderr)
	}
	if c.Contract == ContractVersionV2 {
		return emitWaitRecord(c, rec, stopRequestID(req), out, stderr)
	}
	switch rec.state {
	case RequestApplied:
		if rec.summary != "" {
			fmt.Fprintln(out, rec.summary)
		} else {
			fmt.Fprintf(out, "Session %s drained.\n", req.Session)
		}
	case RequestRejected, RequestFailed:
		fmt.Fprintln(stderr, rec.state+": "+rec.errText)
	default:
		fmt.Fprintf(stderr, "%s: %s\n", rec.state, rec.errText)
	}
	return exitOK
}

// waitForStopResult polls the run's durable request evidence until the stop
// request bound to the original controller session reaches a terminal state
// or the bound expires. A final read after the deadline keeps a decision that
// raced it; a wait that ends without one reports the stop as still pending.
func waitForStopResult(dir string, req *stopRequest, timeout time.Duration) (*requestQueryRecord, error) {
	id := stopRequestID(req)
	deadline := time.Now().Add(timeout)
	for {
		if rec, ok := stopResultRecord(dir, id); ok {
			return rec, nil
		}
		if !time.Now().Before(deadline) {
			if rec, ok := stopResultRecord(dir, id); ok {
				return rec, nil
			}
			return nil, &pendingError{
				message: fmt.Sprintf("stop request %s is still pending; the controller has not finished draining", id),
				request: id, waitKind: "durable stop result",
			}
		}
		time.Sleep(stopResultPollInterval)
	}
}

// stopResultRecord reads one stop request's durable record from the saved
// state and its request evidence. While the request is undelivered the scoped
// stop file is its submitted evidence; once decided, the receipt the honored
// drain, the shutdown release, or a replacement controller's sweep recorded
// is the terminal answer (AX-009).
func stopResultRecord(dir, id string) (*requestQueryRecord, bool) {
	store, err := openObservedStore(dir)
	if err != nil {
		return nil, false
	}
	defer store.Close()
	snap, err := store.Snapshot()
	if err != nil || snap.State == nil {
		return nil, false
	}
	rec := collectRequestEvidence(dir, snap.State, wallClock()).byID[id]
	if rec == nil || !requestStateTerminal(rec.state) {
		return nil, false
	}
	return rec, true
}

// planImportWaitJSON bounds the add-plan --wait for a terminal import
// acknowledgement. A timeout reports a pending request; it never cancels or
// rejects the import. A variable so tests can exercise the timeout quickly.
var planImportWaitJSON = 60 * time.Second

// runAddPlan submits additional requirements to the active controller and
// returns the immediate receipt (AX-011): the import is queued durably and
// the caller queries the record, so a long planning or check phase never
// silences the caller. --wait opts into one bounded wait for the applied
// acknowledgement. Without a controller it suggests the startup path.
func runAddPlan(c Config, out, stderr io.Writer) int {
	if len(c.Plans) == 0 {
		p := usageFailure("usage_invalid", "usage: looper add-plan <plan-file> [...] [--wait]")
		p.withLegacy("usage: looper add-plan <plan-file> [...] [--wait]", 2)
		return emitError(c, p, out, stderr)
	}
	if !repositoryOwned(c) {
		p := runNotFoundError("", "looper --plan <path>")
		p.Message = "No active run. Start one with: looper"
		p.Paths = nil
		p.Actions = []ErrorAction{
			{Description: "Start a controller, optionally with the plan at startup.", Command: "looper --plan <path>"},
		}
		p.withLegacy("No active run. Start one with: looper\nOr add the plan at startup: looper --plan <path>", 1)
		return emitError(c, p, out, stderr)
	}
	_, dir := projectPaths(c)
	info, err := readController(dir)
	if err != nil {
		p := controllerMetadataError(err)
		p.withLegacy(fmt.Sprintf("A controller owns this repository, but its metadata cannot be verified: %s", err), 1)
		return emitError(c, p, out, stderr)
	}
	// A caller key addresses the content, never the path: the digest covers
	// the plan bytes, so the same key returns the original import wherever
	// the same content was submitted from (AX-012).
	payloadDigest := ""
	if c.IdempotencyKey != "" {
		plans, planErr := readPlans(c.Plans)
		if planErr != nil {
			return emitError(c, requestSubmitError(planErr), out, stderr)
		}
		payloadDigest = requestPayloadDigest(&SettingsRequest{Type: RequestTypePlanImport}, planContentDigests(plans))
	}
	if code, handled := answerFromRetainedKey(c, dir, PublicRequestAddPlan, c.IdempotencyKey, payloadDigest, out, stderr); handled {
		return code
	}
	req, err := submitPlanImportRequest(dir, info.Session, c.IdempotencyKey, c.Plans)
	if err != nil {
		return emitError(c, requestSubmitError(err), out, stderr)
	}
	if c.Wait {
		return reportPlanImportWait(c, dir, req.ID, out, stderr)
	}
	return emitMutationReceipt(c, keyedMutationReceipt(newMutationReceipt(req.ID, PublicRequestAddPlan, RequestSubmitted, req.CreatedAt, info.Session),
		c.IdempotencyKey, payloadDigest, false),
		"Queued "+strings.Join(c.Plans, ", "), drainingImportNote(dir, info), out)
}

// reportPlanImportWait renders the terminal acknowledgement one add-plan
// --wait received. Version 2 reports the request record — added task
// identifiers, coverage, adjustments, and gates ride beside the lifecycle
// state; the legacy contract keeps the shapes it printed before AX-011. A
// timeout is an outcome of the wait: the import keeps running and the
// envelope survives.
func reportPlanImportWait(c Config, dir, id string, out, stderr io.Writer) int {
	ack, err := waitForPlanImport(dir, id, c.waitBound())
	if err != nil {
		var pending *pendingError
		if errors.As(err, &pending) && c.Contract == ContractVersionV2 {
			return emitError(c, err, out, stderr)
		}
		if c.JSON {
			json.NewEncoder(out).Encode(planImportAck{ID: id, Status: ImportQueued, Error: err.Error()})
			return 0
		}
		fmt.Fprintln(out, err)
		return 0
	}
	if c.Contract == ContractVersionV2 {
		if ack.Status != ImportApplied {
			return emitError(c, importAckError(ack), out, stderr)
		}
		return emitWaitRecord(c, importAckRecordView(ack), ack.ID, out, stderr)
	}
	if c.JSON {
		json.NewEncoder(out).Encode(*ack)
		if ack.Status == ImportApplied {
			return 0
		}
		return 1
	}
	switch ack.Status {
	case ImportApplied:
		fmt.Fprintf(out, "Applied: %d task(s) added", len(ack.AddedTasks))
		if len(ack.AddedTasks) > 0 {
			fmt.Fprintln(out, ":", strings.Join(ack.AddedTasks, ", "))
		} else {
			fmt.Fprintln(out)
		}
		for _, adjustment := range ack.Adjustments {
			fmt.Fprintln(out, "Adjustment:", adjustment)
		}
		for _, gate := range ack.Gates {
			fmt.Fprintf(out, "Gate: %s depends on %s\n", gate.TaskID, strings.Join(gate.DependsOn, ", "))
		}
		for _, ref := range ack.Covered {
			fmt.Fprintf(out, "Covered: %s -> %s %s\n", ref.Requirement, ref.TaskID, ref.Evidence)
		}
		return 0
	default:
		fmt.Fprintln(stderr, "Import", ack.Status+":", ack.Error)
		for _, conflict := range ack.Conflicts {
			fmt.Fprintln(stderr, " conflict:", conflict)
		}
		return 1
	}
}

// requestSubmitError classifies a refused or unfinished request submission.
// Input rejections are usage failures because nothing was attempted or
// written; capability and session refusals are incompatibilities; a bounded
// wait that ended without an answer is a timeout the caller can retry; a
// failed envelope write is a persistence failure.
func requestSubmitError(err error) error {
	var input *inputError
	if errors.As(err, &input) {
		return input
	}
	var unsupported *unsupportedRequestError
	if errors.As(err, &unsupported) {
		return unsupported
	}
	var stale *staleSessionError
	if errors.As(err, &stale) {
		return stale
	}
	var pending *pendingError
	if errors.As(err, &pending) {
		return pending
	}
	return persistenceError(err.Error())
}

// importAckError maps a terminal import acknowledgement to its structured
// failure. Conflict and stale outcomes carry the request identity so the
// caller can distinguish changed input from an unavailable controller.
func importAckError(ack *planImportAck) *PublicError {
	switch ack.Status {
	case AckStale:
		p := incompatibleFailure("controller_session_stale", ack.Error)
		p.Observed = map[string]any{"request_id": ack.ID, "controller_decision": ack.Status}
		p.Evidence = []ErrorEvidence{{Kind: "request", Reference: "controller acknowledgement " + ack.ID}}
		p.Actions = []ErrorAction{{Description: "Resubmit the plan to the controller that owns the repository now.", Command: "looper add-plan <path>"}}
		return p
	case ImportFailed, AckError:
		if len(ack.Conflicts) > 0 {
			p := operationFailure("import_conflict", ack.Error)
			p.Retry = retryAfterChange
			p.Observed = map[string]any{"request_id": ack.ID, "conflicts": clampString(strings.Join(ack.Conflicts, "; "), maxErrorString)}
			p.Evidence = []ErrorEvidence{{Kind: "request", Reference: "controller acknowledgement " + ack.ID}}
			p.Actions = []ErrorAction{{Description: "Adjust the plan so it no longer conflicts with existing scope, then resubmit."}}
			return p
		}
		p := operationFailure("import_failed", ack.Error)
		p.Observed = map[string]any{"request_id": ack.ID, "controller_decision": ack.Status}
		p.Evidence = []ErrorEvidence{{Kind: "request", Reference: "controller acknowledgement " + ack.ID}}
		p.Actions = []ErrorAction{{Description: "Address the reported failure, then resubmit the plan."}}
		return p
	default:
		p := operationFailure("import_failed", fmt.Sprintf("Import %s.", ack.Status))
		p.Observed = map[string]any{"request_id": ack.ID, "controller_decision": ack.Status}
		p.Evidence = []ErrorEvidence{{Kind: "request", Reference: "controller acknowledgement " + ack.ID}}
		return p
	}
}

// printDoctorController names a live controller and warns when it runs a
// different build than this executable. A controller keeps its build until
// it restarts, so a long-lived run can miss fixes that are already installed.
// The warning is advice: it does not change the exit code.
func printDoctorController(o Observation, out io.Writer) {
	if !o.ControllerOwned || o.Controller == nil {
		return
	}
	line := fmt.Sprintf("Controller: running | session %s | pid %d", sanitize(o.Controller.Session), o.Controller.PID)
	if o.Controller.Started != "" {
		line += " | since " + sanitize(o.Controller.Started)
	}
	fmt.Fprintln(out, line)
	if o.BuildMismatch {
		fmt.Fprintf(out, "  Warning: the controller runs %s; this executable is %s\n", o.Controller.Build, o.Executable)
		fmt.Fprintln(out, "  Fix: run looper stop, then start looper again to use this build")
	}
}

func doctor(c Config, out io.Writer) int {
	mode := DetectMode(os.Stdin, out, c.Plain)
	if c.JSON {
		mode = ModePlain
	}
	p := NewPresenter(mode, out, func() int {
		if f, ok := out.(*os.File); ok {
			return terminalWidth(f)
		}
		return 0
	})
	code := 0
	checks := []struct{ name, hint string }{
		{"git", "install Git and make sure git is on PATH"},
		{"bash", "install Bash or point LOOPER_* commands at another shell"},
		{agentBinaryOf(c, c.Agent), agentInstallHint(c, c.Agent)},
	}
	if c.ReviewAgent != c.Agent {
		checks = append(checks, struct{ name, hint string }{agentBinaryOf(c, c.ReviewAgent), agentInstallHint(c, c.ReviewAgent)})
	}
	for _, check := range checks {
		path, err := exec.LookPath(check.name)
		if err != nil {
			p.DoctorLine(false, check.name, check.hint)
			code = 1
		} else {
			p.DoctorLine(true, path, check.hint)
		}
	}
	var b Backlog
	if err := readJSON(c.Todo, &b); os.IsNotExist(err) {
		fmt.Fprintln(out, "Backlog: created from project requirements on first run")
	} else if err != nil {
		fmt.Fprintln(out, "Backlog:", err)
		fmt.Fprintln(out, "  Fix: repair the task file or remove it to plan from requirements")
		code = 1
	} else if err = b.Validate(); err != nil {
		fmt.Fprintln(out, "Backlog:", err)
		fmt.Fprintln(out, "  Fix: repair the reported task or dependency in", c.Todo)
		code = 1
	} else {
		fmt.Fprintf(out, "Backlog: %d tasks, %d ready\n", len(b.Tasks), len(b.Ready()))
	}
	// The bounded pending-request summary (AX-010): a plain doctor caller
	// learns what the run owes an outcome to without the JSON document. The
	// read is best-effort — a run without records reports nothing, and a
	// health failure stays a finding, never a doctor crash.
	_, dir := projectPaths(c)
	if _, err := os.Stat(filepath.Join(dir, "state.db")); err == nil {
		if store, err := openObservedStore(dir); err == nil {
			if snap, err := store.Snapshot(); err == nil {
				printDoctorController(observe(c, snap.State, snap), out)
				summary := collectDoctorRequestsSummary(dir, snap.State, wallClock())
				if summary.Pending > 0 || summary.Waiting > 0 {
					oldest := ""
					if summary.OldestAgeSeconds != nil {
						oldest = fmt.Sprintf(" | oldest %s ago", statusDuration(*summary.OldestAgeSeconds))
					}
					fmt.Fprintf(out, "Requests: %d queued | %d waiting%s\n", summary.Pending, summary.Waiting, oldest)
					for _, w := range summary.WaitingOn {
						age := ""
						if w.AgeSeconds != nil {
							age = fmt.Sprintf(" for %s", statusDuration(*w.AgeSeconds))
						}
						fmt.Fprintf(out, "  %s %s [%s] waits on %s%s\n", w.ID, w.Type, w.State, w.Reason, age)
					}
					if summary.WaitingOmitted > 0 {
						fmt.Fprintf(out, "  %d more waiting request(s) not shown; read them with: looper requests --contract v2 --json\n", summary.WaitingOmitted)
					}
					fmt.Fprintln(out, "  Detail: looper requests --contract v2 --json")
				}
			}
			store.Close()
		}
	}
	// Under contract version 2 the doctor's exit code follows the transport
	// registry: findings are observation content, so a completed command
	// exits 0 however unhealthy the run looks. The legacy contract keeps its
	// frozen exit 1 on failing checks.
	if c.Contract == ContractVersionV2 {
		return exitOK
	}
	return code
}
func inspect(ctx context.Context, c Config, out io.Writer) error {
	_, dir := projectPaths(c)
	path := filepath.Join(dir, "state.db")
	if _, err := os.Stat(path); err != nil {
		p := runNotFoundForRun(c, path, "looper")
		p.Message = "no saved run; start looper first"
		return p
	}
	// One-shot reads attach without creating SQLite sidecar files beside a
	// cleanly stopped run's database. A followed stream holds this
	// connection across the controllers that may start later, so it keeps
	// the through-WAL attach.
	var store *Store
	var err error
	if c.Follow {
		store, err = OpenStoreReadonly(path)
	} else {
		store, err = openObservedStore(dir)
	}
	if err != nil {
		return stateUnreadableError(path, err)
	}
	defer store.Close()
	if c.Command == "inspect" {
		// The negotiated compact inspection (AX-005) is the version 2
		// default; the legacy document stays the default contract's response
		// and the explicit --full response under version 2.
		if c.Contract == ContractVersionV2 && !c.Full {
			return compactInspectTask(c, store, out)
		}
		return inspectTask(c, store, out)
	}
	if c.Command == "events" || c.Command == "tail" {
		return runEvents(ctx, c, store, out)
	}
	if c.Command == "status" {
		// One snapshot pairs the reported state with its continuation cursor,
		// so a concurrent commit between this read and the caller's next
		// events read cannot be missed (AX-003).
		snap, err := store.Snapshot()
		if err != nil {
			return stateUnreadableError(path, err)
		}
		// A store with no saved state still answers the compact contract while
		// a live controller bootstraps its first backlog; without a controller
		// there is no run to report.
		if c.JSON && c.Contract == ContractVersionV2 && !c.Full {
			if snap.State == nil && !repositoryOwned(c) {
				p := runNotFoundForRun(c, path, "looper")
				p.Message = "no saved run"
				return p
			}
			doc, err := compactStatus(c, store, snap)
			if err != nil {
				return stateUnreadableError(path, err)
			}
			return json.NewEncoder(out).Encode(doc)
		}
		if snap.State == nil {
			p := runNotFoundForRun(c, path, "looper")
			p.Message = "no saved run"
			return p
		}
		s := snap.State
		if c.JSON {
			// The complete document, also served under version 2 through
			// --full: the compact summary is the default, and the full state
			// stays one explicit flag away.
			timing, err := store.Timing(wallClock().UTC())
			if err != nil {
				return stateUnreadableError(path, err)
			}
			statistics, err := store.Statistics(wallClock().UTC())
			if err != nil {
				return stateUnreadableError(path, err)
			}
			return json.NewEncoder(out).Encode(statusReport(c, snap, timing, statistics))
		}
		if c.ListStatus != "" {
			for _, t := range s.Backlog.Tasks {
				if t.Status == c.ListStatus {
					fmt.Fprintf(out, "%s  %s  %s\n", sanitize(t.ID), t.Status, sanitize(t.Title))
				}
			}
		}
		if DetectMode(os.Stdin, out, c.Plain) == ModeInteractive {
			timing, err := store.Timing(time.Now().UTC())
			if err != nil {
				return stateUnreadableError(path, err)
			}
			printTiming(out, timing)
			NewPresenter(ModeInteractive, out, func() int { return terminalWidth(fileOf(out)) }).StatusBoard(s, path)
			return nil
		}
		// A piped plain status renders the same summary as the compact
		// document: liveness, in-flight work, and the reason for waiting stay
		// visible without a dashboard, and the response stays bounded.
		doc, err := compactStatus(c, store, snap)
		if err != nil {
			return stateUnreadableError(path, err)
		}
		renderCompactStatus(doc, path, out)
		return nil
	}
	return nil
}
