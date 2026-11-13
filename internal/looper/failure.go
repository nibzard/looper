package looper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"
)

// Bounded failure summaries (AX-021). Worker, verification, launcher, and
// resource-cleanup failures journal one summary beside their structured event
// identity: the operation, the tool or agent that ran, the exit status when
// the process ran, a bounded safe context excerpt, and Looper-owned evidence
// references. The summaries answer severity questions in one focused query
// (`looper events --contract v2 --json --kind failure`), so no caller needs a
// private agent session file.
const (
	// failureEventKind is the one lifecycle kind every failure summary uses,
	// so one --kind filter retrieves every class.
	failureEventKind = "failure"
	// failureFile is the record a check goroutine retains beside its logs for
	// the owner goroutine to journal when the check result is consumed.
	failureFile = "failure.json"
	// Bounds for summary fields. A summary is part of an event payload, so its
	// bytes are part of a response; long logs can never overflow them.
	maxFailureContextBytes = 2048
	maxFailureContextLines = 24
	maxFailureCause        = 300
	maxFailureEvidence     = 6
	maxFailureMessage      = 240
)

// Failure operations name the class that failed. The agent roles come from the
// prompt budget, so a summary names the operation the engine really ran.
const (
	opWorker    = "worker"
	opPlanning  = "planning"
	opReview    = "review"
	opImport    = "import"
	opBootstrap = "bootstrap"
	opConsult   = "consultation"
	opLaunch    = "launch"
	opSetup     = "setup"
	opVerify    = "verify"
	opCleanup   = "resource-cleanup"

	outcomeExit          = "exit_failure"
	outcomeLaunch        = "launch_failed"
	outcomeAbsent        = "cleanup_already_absent"
	outcomeInvalidResult = "invalid_result"
	outcomeReported      = "error_reported"
	outcomeCancelled     = "cancelled"
	outcomeTimeout       = "timeout"
	outcomeTreeChanged   = "tree_changed"
	outcomeSignaled      = "signaled"
	outcomeRetryLimit    = "retry_limit"
	outcomeUnknown       = "unknown"
)

// FailureSummary is the bounded public evidence of one failed operation. It
// never carries command arguments, environment values, prompt text, tool
// inputs, or private agent reasoning; the context excerpt is the failing
// tool's own output, sanitized and scrubbed of credential shapes.
type FailureSummary struct {
	Operation  string            `json:"operation"`
	Tool       string            `json:"tool,omitempty"`
	ExitStatus *int              `json:"exit_status,omitempty"`
	Signal     string            `json:"signal,omitempty"`
	Outcome    string            `json:"outcome"`
	Cause      string            `json:"cause,omitempty"`
	Context    string            `json:"context,omitempty"`
	Evidence   []FailureEvidence `json:"evidence,omitempty"`
}

// FailureEvidence names one Looper-owned record a caller can read on its own.
// References stay inside the runtime directory; private agent session files
// are never referenced.
type FailureEvidence struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
}

// failureCarrier is the errors that already know their bounded summary.
type failureCarrier interface{ failureSummary() *FailureSummary }

func (e *AgentError) failureSummary() *FailureSummary { return e.Summary }

func (e *LaunchError) failureSummary() *FailureSummary {
	return (&FailureSummary{
		Operation: opLaunch, Tool: e.Stage, Outcome: outcomeLaunch,
		Cause: e.Err.Error(),
	}).bound()
}

// failureOf returns the summary an error carries, or nil when it carries none.
func failureOf(err error) *FailureSummary {
	var carrier failureCarrier
	if errors.As(err, &carrier) {
		return carrier.failureSummary()
	}
	return nil
}

// exitDetail reads the process outcome from an error chain. A negative code
// means no exit status exists: the process never started, was cancelled, or
// failed outside the target process.
func exitDetail(err error) (code int, signal string) {
	code = -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() >= 0 {
			code = exitErr.ExitCode()
		}
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			signal = status.Signal().String()
		}
	}
	return code, signal
}

// exitOf records the exit status when one exists, so a caller distinguishes
// "exited 1" from "never started".
func exitOf(code int) *int {
	if code < 0 {
		return nil
	}
	zero := code
	return &zero
}

// outcomeFor classifies how a failed operation ended.
func outcomeFor(err error, code int, signal string) string {
	switch {
	case errors.Is(err, context.Canceled):
		return outcomeCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return outcomeTimeout
	case code >= 0:
		return outcomeExit
	case signal != "":
		return outcomeSignaled
	}
	return outcomeUnknown
}

// operationOf maps a prompt-budget role onto the operation name its summary
// reports.
func operationOf(role string) string {
	switch role {
	case "planner":
		return opPlanning
	case "worker":
		return opWorker
	case "review":
		return opReview
	case "import":
		return opImport
	case "bootstrap":
		return opBootstrap
	case "consultation":
		return opConsult
	}
	return role
}

// bound clamps every summary field, so a summary is always bounded JSON
// regardless of what failed.
func (s *FailureSummary) bound() *FailureSummary {
	if s == nil {
		return nil
	}
	s.Operation = clampString(sanitize(s.Operation), 64)
	s.Tool = clampString(sanitize(s.Tool), 256)
	s.Outcome = clampString(sanitize(s.Outcome), 64)
	s.Cause = clampString(scrubSecrets(sanitize(s.Cause)), maxFailureCause)
	s.Context = clampContext(scrubSecrets(sanitize(s.Context)), maxFailureContextBytes)
	if len(s.Evidence) > maxFailureEvidence {
		s.Evidence = s.Evidence[:maxFailureEvidence]
	}
	for i := range s.Evidence {
		s.Evidence[i].Kind = clampString(s.Evidence[i].Kind, 32)
		s.Evidence[i].Reference = clampString(s.Evidence[i].Reference, maxErrorString)
	}
	if s.Operation == "" {
		s.Operation = outcomeUnknown
	}
	return s
}

// clampContext keeps the tail of a context excerpt: the end of a log is where
// a tool reports its failure.
func clampContext(text string, max int) string {
	if len(text) <= max {
		return text
	}
	cut := len(text) - (max - len("…"))
	for cut < len(text) && !utf8.RuneStart(text[cut]) {
		cut++
	}
	return "…" + text[cut:]
}

// boundedFailureContext reads the tail of one Looper-owned log: sanitized,
// scrubbed, line-bounded, and byte-bounded, so a long log cannot overflow the
// response that carries it.
func boundedFailureContext(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return boundedContextOf(data)
}

func boundedContextOf(data []byte) string {
	const window = maxFailureContextBytes * 2
	if len(data) > window {
		data = data[len(data)-window:]
	}
	text := sanitize(string(data))
	lines := strings.Split(text, "\n")
	if len(lines) > maxFailureContextLines {
		lines = lines[len(lines)-maxFailureContextLines:]
	}
	return strings.TrimSpace(clampContext(scrubSecrets(strings.Join(lines, "\n")), maxFailureContextBytes))
}

// scrubSecrets replaces credential shapes in excerpted text. Summaries quote
// tool output, and tool output can echo a secret; the shapes below cover the
// common carriers (headers, assignments, URL user information, well-known key
// prefixes, private key blocks).
var secretScrubbers = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----(?:.|\n)*?-----END [A-Z ]*PRIVATE KEY-----`), "[redacted private key]"},
	{regexp.MustCompile(`(?i)\b(authorization|proxy-authorization)(\s*:\s*)(\S+)`), "$1$2[redacted]"},
	{regexp.MustCompile(`(?i)\b(bearer)(?:\s+|%20|\+)([A-Za-z0-9._+/=-]{8,})`), "$1[redacted]"},
	{regexp.MustCompile(`(?i)\b([A-Z0-9_-]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API[-_]?KEY|CREDENTIAL)[A-Z0-9_-]*)(\s*[=:]\s*)(["']?)[A-Za-z0-9._+/=-]{3,}(["']?)`), "$1$2$3[redacted]$4"},
	{regexp.MustCompile(`(?i)(\s)(-u|--user|--username)(=|\s+)([^/\s:]+:[^/\s]*)`), "$1$2$3[redacted]"},
	{regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)([^/\s:]+):([^@\s/]+)@`), "$1[redacted]@"},
	{regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{6,}|gh[pousr]_[A-Za-z0-9]{16,}|AKIA[0-9A-Z]{16}|xox[bp]-[A-Za-z0-9-]{10,})`), "[redacted]"},
}

func scrubSecrets(s string) string {
	for _, scrubber := range secretScrubbers {
		s = scrubber.pattern.ReplaceAllString(s, scrubber.replacement)
	}
	return s
}

// Cleanup classification: a cleanup that failed only because its targets were
// already absent — the retry case for a script that finished its removals —
// must never read as an exit failure of the cleanup logic itself.
var absentPathError = regexp.MustCompile(`(?i)no such file or directory`)
var otherCleanupError = regexp.MustCompile(`(?i)permission denied|read-only file system|operation not permitted|is a directory|directory not empty|device or resource busy|input/output error|cannot connect|connection refused`)

// cleanupOutcome classifies a cleanup failure from its bounded log excerpt:
// already-absent paths when every observed error names a missing target, an
// exit failure when anything else went wrong, and an unknown cause when the
// log recorded nothing.
func cleanupOutcome(context string) string {
	if context == "" {
		return outcomeUnknown
	}
	if absentPathError.MatchString(context) && !otherCleanupError.MatchString(context) {
		return outcomeAbsent
	}
	return outcomeExit
}

// failureMessage renders the human line beside the structured payload: the
// operation, the exit status, and one bounded cause line. Message text stays
// for people; the payload is the machine record.
func failureMessage(taskID string, s *FailureSummary) string {
	if s == nil {
		s = &FailureSummary{Outcome: outcomeUnknown}
	}
	var b strings.Builder
	if taskID != "" {
		b.WriteString(taskID + "  ")
	}
	operation := s.Operation
	if operation == "" {
		operation = outcomeUnknown
	}
	b.WriteString(operation)
	switch {
	case s.ExitStatus != nil:
		fmt.Fprintf(&b, " failed: exited %d", *s.ExitStatus)
	case s.Signal != "":
		fmt.Fprintf(&b, " failed: killed by %s", s.Signal)
	default:
		b.WriteString(" failed (" + s.Outcome + ")")
	}
	cause := firstLine(s.Cause)
	if cause == "" {
		cause = firstLine(s.Context)
	}
	if cause != "" {
		b.WriteString(": " + clampString(cause, 160))
	}
	return clampString(b.String(), maxFailureMessage)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// failureEvent journals one bounded failure summary as a lifecycle event with
// its structured identity. It runs on the owner goroutine only.
func (e *Engine) failureEvent(a *Attempt, s *FailureSummary) {
	if s == nil {
		s = &FailureSummary{Outcome: outcomeUnknown}
	}
	payload := map[string]any{"failure": s.bound()}
	id := e.identity()
	if a != nil {
		id = e.attemptIdentity(a)
	}
	e.eventData(failureEventKind, failureMessage(attemptTask(a), s), id.merge(payload))
}

func attemptTask(a *Attempt) string {
	if a == nil {
		return ""
	}
	return a.Task.ID
}

// workerFailureEvent journals one worker failure. Errors that carry their own
// summary keep it; anything else still reports the exit detail that exists,
// so a hold or a run exit always leaves a bounded record.
func (e *Engine) workerFailureEvent(a *Attempt, err error) {
	s := failureOf(err)
	if s == nil {
		code, signal := exitDetail(err)
		s = &FailureSummary{Operation: opWorker, Tool: agentToolOf(e.Config, a.Agent), ExitStatus: exitOf(code), Signal: signal, Outcome: outcomeFor(err, code, signal), Cause: err.Error()}
	}
	e.failureEvent(a, s)
}

// writeFailureSummary retains one check failure beside its logs. The check
// goroutine cannot journal, so the summary waits here for the owner.
func writeFailureSummary(dir string, s *FailureSummary) {
	_ = writeJSON(filepath.Join(dir, failureFile), s.bound())
}

// commandFailureSummary builds the summary of one failed setup or verify
// command: the script identity, the exit status, the bounded tail of the
// command's own log, and the Looper-owned evidence beside it.
func commandFailureSummary(operation string, err error, log string) *FailureSummary {
	code, signal := exitDetail(err)
	script := strings.TrimSuffix(log, ".log") + ".sh"
	return (&FailureSummary{
		Operation: operation, Tool: filepath.Base(script), ExitStatus: exitOf(code), Signal: signal,
		Outcome: outcomeFor(err, code, signal), Cause: err.Error(), Context: boundedFailureContext(log),
		Evidence: []FailureEvidence{{Kind: "file", Reference: script}, {Kind: "log", Reference: log}},
	}).bound()
}

// emitCheckFailure journals the summary a failed check retained beside its
// logs. The goroutine that ran the check cannot write events, so the owner
// reads failure.json when it consumes the result; a missing record, such as an
// interrupted run between the check and this boundary, still reports the
// failure itself.
func (e *Engine) emitCheckFailure(a *Attempt, c *Candidate) {
	var retained FailureSummary
	if readJSON(filepath.Join(a.Directory, "checks", c.Commit, failureFile), &retained) == nil {
		e.failureEvent(a, &retained)
		return
	}
	if c.LaunchFailure != nil {
		if s := failureOf(c.LaunchFailure); s != nil {
			e.failureEvent(a, s)
			return
		}
	}
	e.failureEvent(a, &FailureSummary{Operation: opVerify, Outcome: outcomeExit})
}

// cleanupFailureEvent journals the bounded summary of one failed resource
// cleanup. A cleanup that failed only on already-absent paths reports
// outcome cleanup_already_absent, so it stays distinguishable from an exit
// failure of the cleanup logic.
func (e *Engine) cleanupFailureEvent(lease ResourceLease, err error) {
	code, signal := exitDetail(err)
	log := filepath.Join(lease.Directory, "cleanup.log")
	excerpt := boundedFailureContext(log)
	outcome := cleanupOutcome(excerpt)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		outcome = outcomeTimeout
	case errors.Is(err, context.Canceled):
		outcome = outcomeCancelled
	}
	s := &FailureSummary{
		Operation: opCleanup, Tool: resourceCleanupFile, ExitStatus: exitOf(code), Signal: signal,
		Outcome: outcome, Cause: err.Error(), Context: excerpt,
		Evidence: []FailureEvidence{
			{Kind: "log", Reference: log},
			{Kind: "file", Reference: filepath.Join(lease.Directory, resourceLeaseFile)},
			{Kind: "file", Reference: filepath.Join(lease.Directory, resourceCleanupFile)},
		},
	}
	e.failureEvent(nil, s)
}

// agentToolOf names the agent kind with the binary the configuration
// resolves. Arguments stay out: a command line can carry values no summary
// should repeat.
func agentToolOf(c Config, kind string) string {
	bin := agentBinaryOf(c, kind)
	if bin == "" || bin == kind {
		return kind
	}
	return kind + " (" + bin + ")"
}

// toolErrorExcerpt extracts one bounded, scrubbed line of a tool's own error
// output from a tool_result content value, so live activity can state severity
// without opening the raw session log. Tool inputs never enter it.
func toolErrorExcerpt(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &text) != nil {
		text = ""
		if json.Unmarshal(raw, &blocks) == nil {
			var parts []string
			for _, b := range blocks {
				if b.Text != "" {
					parts = append(parts, b.Text)
				}
			}
			text = strings.Join(parts, " ")
		}
	}
	text = scrubSecrets(sanitize(text))
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	return clampString(strings.Join(strings.Fields(text), " "), 160)
}
