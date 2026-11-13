package looper

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// defaultAgent is the coding agent a controller uses when the operator never
// chose one by flag, environment, or saved profile.
const defaultAgent = "claude"

type Config struct {
	AgentProfiles                                                                                                                   map[string]AgentProfile
	AgentCatalogPath, DefaultWorker, DefaultReviewer                                                                                string
	TestAgent                                                                                                                       string
	Root, Todo, BaseDir, Agent, ReviewAgent, CodexBin, ClaudeBin, ZcodeBin, Model, ClaudeModel, Effort, Verify, Setup, Assess, Hook string
	InspectTask                                                                                                                     string
	RequestID                                                                                                                       string
	// CancelTask is the task one cancellation names; Cascade takes its live
	// dependents along (AX-019).
	CancelTask string
	Cascade    bool
	// Interrupt makes looper stop signal the owning controller instead of
	// requesting a drain.
	Interrupt bool
	// Foreground makes looper tui run the controller inside the dashboard
	// process instead of starting a daemon and attaching to it.
	Foreground bool
	// AmendTask and AmendFile name the task one amendment targets and the
	// file that carries the amended task (AX-020).
	AmendTask string
	AmendFile string
	// RecoveryChoice names the recovery choice one checked application
	// applies, and RecoveryToken binds it to the preview that enumerated the
	// choice (AX-017).
	RecoveryChoice string
	RecoveryToken  string
	// IdempotencyKey is the caller key one mutation carries (AX-012). It
	// scopes to the project and the operation: the same key and payload
	// returns the original request, and a changed payload is a conflict.
	IdempotencyKey string
	// The finite wait options (AX-013): exactly one predicate — a cursor
	// position, a task with an optional target status, a request identifier,
	// or the run outcome — observed for the explicit bound --timeout names.
	WaitCursor     int64
	WaitCursorSet  bool
	WaitRequestID  string
	WaitRunOutcome bool
	WaitTaskStatus string
	WaitFor        time.Duration
	WaitForSet     bool

	After         int64
	AfterSet      bool
	AfterID       string
	CheckAfter    int64
	CheckAfterSet bool
	Command       string
	ListStatus    string
	// HelpTopic names the command whose contextual help block `looper help
	// <command>` and `looper <command> --help` serve; empty serves the
	// overview (AX-026).
	HelpTopic                                                string
	Jobs, Checks, Limit, ReviewEvery, MaxRepairs, RetryLimit int
	EventLimit                                               int
	EventTask, EventKind                                     string
	Poll, RetryDelay, Timeout, GitTimeout                    time.Duration
	WaitTimeout                                              time.Duration
	Once, Follow, JSON, Verbose, Smart, GitInit, JobsAuto    bool
	Wait, WaitTimeoutSet                                     bool
	Plain, Yes, Reset, Detached, NoAnimation, Full, Preview  bool
	Contract                                                 int
	Sources                                                  Sources
	ProfilePath                                              string
	Plans                                                    []string
}

// Source records where a setting's effective value came from. Resolution
// follows the explicit layer order; numeric values retain compatibility with
// saved controller records. An explicit default still overrides a profile.
type Source int

const (
	SourceDefault Source = iota
	SourceProfile
	SourceEnv
	SourceFlag
	// Keep the numeric values of persisted legacy sources unchanged.
	SourceMachine Source = -1
)

func (s Source) String() string {
	switch s {
	case SourceMachine:
		return "machine"
	case SourceProfile:
		return "profile"
	case SourceEnv:
		return "environment"
	case SourceFlag:
		return "flag"
	default:
		return "default"
	}
}

// Sources is the per-setting provenance of the effective configuration.
type Sources struct {
	Agent, ReviewAgent, Jobs, Model, ClaudeModel, Effort, Verify, Setup, ReviewEvery Source
}

func (s Sources) any(atLeast Source) bool {
	for _, v := range []Source{s.Agent, s.ReviewAgent, s.Jobs, s.Model, s.ClaudeModel, s.Effort, s.Verify, s.Setup, s.ReviewEvery} {
		if v >= atLeast {
			return true
		}
	}
	return false
}

// AnyFlag reports whether any profile-managed setting came from a flag.
func (s Sources) AnyFlag() bool { return s.any(SourceFlag) }

// AnyEnv reports whether any profile-managed setting came from the environment.
func (s Sources) AnyEnv() bool { return s.any(SourceEnv) }

type inputValue struct {
	value string
	from  Source
}
type intValue struct {
	value int
	from  Source
}

// settingSources layers one value per profile-managed setting: defaults, then
// the saved profile, then present environment variables, then explicit flags.
type settingSources struct {
	agent, reviewAgent, jobs, model, claudeModel, effort, verify, setup inputValue
	reviewEvery                                                         intValue
}

// flagOptions records setting flags as replay functions in command-line
// order. Replaying after the environment layer preserves the ordered
// semantics of overlapping flags: a later --agent, --all, or
// --review-agent wins over --interleave, exactly as the original parser
// behaved. An explicit empty value ("--model \"\"") is a real override, not
// an absent flag.
type flagOptions struct {
	apps []func(*settingSources)
}

func (f *flagOptions) apply(fn func(*settingSources)) {
	f.apps = append(f.apps, fn)
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
func integer(key string, def int) (int, error) {
	s := env(key, strconv.Itoa(def))
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return 0, environmentInvalid(key, s)
	}
	return v, nil
}

// environmentInvalid reports a present environment variable whose value the
// command cannot accept. The raw value is numeric configuration, never a
// secret, so naming it helps the caller fix it.
func environmentInvalid(key, value string) *PublicError {
	p := usageFailure("environment_invalid", key+" must be a nonnegative integer")
	p.Expected = map[string]any{key: "nonnegative integer"}
	p.Observed = map[string]any{key: clampString(value, 128)}
	return p
}

// optionValueInvalid reports a recognized option or setting whose value the
// command cannot accept.
func optionValueInvalid(name, value, want string) *PublicError {
	p := usageFailure("option_value_invalid", name+" must be "+want)
	p.Expected = map[string]any{name: want}
	p.Observed = map[string]any{name: clampString(value, 128)}
	return p
}

// profileUnreadableError reports a saved profile that exists but cannot be
// read. Reset stays available because it never reads the file.
func profileUnreadableError(err error, path string) *PublicError {
	p := unavailableFailure("profile_unreadable", err.Error())
	p.Paths = []string{path}
	p.Actions = []ErrorAction{{
		Description: "Repair the profile syntax or remove the file; looper config --reset removes it without reading it.",
		Command:     "looper config --reset",
	}}
	return p
}

// usesProfile reports whether a command reads saved settings. Read-only
// commands never fail on an invalid profile; preflight does, because it
// previews a start that would fail on the same profile the same way.
func usesProfile(command string) bool {
	switch command {
	case "run", "start", "tui", "config", "preflight":
		return true
	}
	return false
}

// eventQueryCommand reports whether a command reads the event journal with
// the bounded query flags.
func eventQueryCommand(command string) bool {
	return command == "events" || command == "tail"
}

// inspectQueryCommand reports whether a command reads task history with the
// inspection paging flags (AX-005).
func inspectQueryCommand(command string) bool {
	return command == "inspect"
}

// runsQueryCommand reports whether a command reads the saved-run listing
// with the bounded page flag (AX-025).
func runsQueryCommand(command string) bool {
	return command == "runs"
}

// requestsQueryCommand reports whether a command reads the durable request
// records with the bounded query flags (AX-010).
func requestsQueryCommand(command string) bool {
	return command == "requests"
}

// mutationWaitCommand reports whether a command submits controller requests
// and accepts the explicit bounded --wait (AX-011). Stop joined the family in
// AX-022: its wait is the durable, session-bound drain wait.
func mutationWaitCommand(command string) bool {
	return command == "add-task" || command == "add-plan" || command == "cancel" || command == "amend" || command == "config" || command == "recovery" || command == "stop"
}

// validateMutationWaitFlags checks the mutation wait options against the
// command that received them. Waiting is explicit and bounded: --wait opts
// in, --wait-timeout bounds it, and both belong only to the mutation
// commands that answer through the request protocol.
func validateMutationWaitFlags(c Config) error {
	if !c.Wait && !c.WaitTimeoutSet {
		return nil
	}
	if !mutationWaitCommand(c.Command) {
		supported := "--wait is supported by looper add-task, looper add-plan, looper cancel, looper amend, looper config, and looper stop"
		p := usageFailure("option_command_mismatch", supported)
		p.Expected = map[string]any{"option": "--wait", "command": "add-task, add-plan, cancel, amend, config, or stop"}
		p.Observed = map[string]any{"option": "--wait", "command": c.Command}
		return p
	}
	if c.WaitTimeoutSet && !c.Wait {
		p := usageFailure("option_conflict", "--wait-timeout bounds a --wait; pass --wait to wait for the outcome")
		p.Observed = map[string]any{"--wait": false, "--wait-timeout": true}
		return p
	}
	return nil
}

// waitCommand reports whether the command is the finite wait (AX-013).
func waitCommand(command string) bool {
	return command == "wait"
}

// helpTopicFailure reports a command line that named two help topics.
func helpTopicFailure() error {
	p := usageFailure("usage_invalid", "help takes one command name")
	p.Observed = map[string]any{"topics": 2}
	return p
}

// taskFileArgument reports whether a bare positional names a task file. A
// task-file reference carries a path separator, a .json suffix, or names a
// file that exists; anything else is a command spelling Looper does not know
// and fails as a usage error instead of starting a run against a phantom
// task file (AX-026).
func taskFileArgument(a string) bool {
	if strings.ContainsAny(a, `/\`) || strings.EqualFold(filepath.Ext(a), ".json") {
		return true
	}
	_, err := os.Stat(a)
	return err == nil
}

// validateHelpFlags checks the help topic and output selection. Help is text
// for people and agents at a terminal: the machine-readable command surface
// is the capabilities document, so --json is refused with that route instead
// of mixing human prose into a machine stream, and a topic that names no
// command is a usage error, not an empty answer (AX-026).
func validateHelpFlags(c Config) error {
	if c.Command != "help" {
		return nil
	}
	if c.JSON {
		p := usageFailure("option_command_mismatch", "--json is not supported by help; the machine-readable command surface is the capabilities document")
		p.Expected = map[string]any{"command": "version --contract v2 --json"}
		p.Observed = map[string]any{"option": "--json", "command": "help"}
		p.Actions = []ErrorAction{{
			Description: "Read the client and controller capabilities, including the command list.",
			Command:     "looper version --contract v2 --json",
		}}
		return p
	}
	if c.HelpTopic == "" {
		return nil
	}
	for _, name := range clientCommands {
		if name == c.HelpTopic {
			return nil
		}
	}
	p := usageFailure("unknown_command", fmt.Sprintf("no help for %q; looper help lists every command", c.HelpTopic))
	p.Observed = map[string]any{"command": clampString(c.HelpTopic, 64)}
	p.Actions = []ErrorAction{{
		Description: "Show the overview with every command.",
		Command:     "looper help",
	}}
	return p
}

// validateWaitFlags checks the wait options against the command that received
// them. A wait names exactly one predicate and carries one explicit finite
// bound: the timeout is what makes the wait finite, so it is required, and a
// second predicate is a conflict because the outcome could not name one
// satisfied condition.
func validateWaitFlags(c Config) error {
	options := []struct {
		name     string
		set      bool
		commands string
	}{
		{"--cursor", c.WaitCursorSet, "looper wait"},
		{"--request", c.WaitRequestID != "", "looper wait"},
		{"--run", c.WaitRunOutcome, "looper wait"},
		{"--status", c.WaitTaskStatus != "", "looper wait --task <task-id>"},
		{"--timeout", c.WaitForSet, "looper wait"},
	}
	for _, option := range options {
		if option.set && !waitCommand(c.Command) {
			p := usageFailure("option_command_mismatch", option.name+" is supported by "+option.commands)
			p.Expected = map[string]any{"option": option.name, "command": "wait"}
			p.Observed = map[string]any{"option": option.name, "command": c.Command}
			return p
		}
	}
	if !waitCommand(c.Command) {
		return nil
	}
	if c.Follow {
		p := usageFailure("option_conflict", "--follow streams events without a bound; looper wait returns one finite outcome. Remove --follow, or stream with looper events --follow")
		p.Observed = map[string]any{"--follow": true, "command": "wait"}
		p.Actions = []ErrorAction{{Description: "Stream the journal instead of waiting for one outcome.", Command: "looper events --contract v2 --follow --json"}}
		return p
	}
	if c.WaitTaskStatus != "" && c.EventTask == "" {
		p := usageFailure("option_conflict", "--status names the target of one task wait; pass --task with it")
		p.Observed = map[string]any{"--status": c.WaitTaskStatus, "--task": false}
		return p
	}
	predicates := 0
	if c.WaitCursorSet {
		predicates++
	}
	if c.EventTask != "" {
		predicates++
	}
	if c.WaitRequestID != "" {
		predicates++
	}
	if c.WaitRunOutcome {
		predicates++
	}
	switch {
	case predicates == 0:
		p := usageFailure("usage_invalid", "looper wait needs one predicate: --cursor <event-id>, --task <task-id> [--status <status>], --request <request-id>, or --run")
		p.Expected = map[string]any{"invocation": "looper wait (--cursor N | --task T [--status S] | --request ID | --run) --timeout <seconds>"}
		p.Observed = map[string]any{"invocation": "looper wait", "predicates": 0}
		p.Actions = []ErrorAction{{Description: "Wait for the next change from the position status reports.", Command: "looper wait --contract v2 --cursor 0 --timeout 30s"}}
		return p
	case predicates > 1:
		p := usageFailure("usage_invalid", "looper wait observes one predicate; pass only one of --cursor, --task, --request, and --run")
		p.Observed = map[string]any{"predicates": predicates}
		return p
	}
	if !c.WaitForSet {
		p := usageFailure("usage_invalid", "looper wait is finite: pass --timeout <seconds> to bound it")
		p.Expected = map[string]any{"--timeout": "at least one second, for example 30s"}
		p.Observed = map[string]any{"--timeout": "missing"}
		p.Actions = []ErrorAction{{Description: "Bound the wait explicitly; a timeout is an outcome of the wait and cancels nothing.", Command: "looper wait --contract v2 --cursor 0 --timeout 30s"}}
		return p
	}
	return nil
}

// parseWaitTimeout reads one wait bound. A bare number counts seconds and a
// duration string names its unit, so 30 and 30s bound the same wait; anything
// below one second cannot hold an observation and is refused.
func parseWaitTimeout(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		n, e := strconv.Atoi(v)
		if e != nil {
			return 0, optionValueInvalid("--timeout", v, "a duration of at least one second, for example 30s")
		}
		d = time.Duration(n) * time.Second
	}
	if d < time.Second {
		return 0, optionValueInvalid("--timeout", v, "a duration of at least one second, for example 30s")
	}
	return d, nil
}

// idempotencyKeyCommand reports whether a command accepts a caller
// idempotency key (AX-012). The key makes a mutation retry-safe, so it
// belongs to the submission commands; `looper stop` derives its request
// identity from the controller it targets and is retry-safe without one.
func idempotencyKeyCommand(command string) bool {
	return command == "add-task" || command == "add-plan" || command == "cancel" || command == "amend" || command == "config" || command == "recovery"
}

// validateIdempotencyKeyFlags checks the caller key option against the
// command that received it: nothing was submitted when the command cannot
// carry a key, so the refusal is a usage failure.
func validateIdempotencyKeyFlags(c Config) error {
	if c.IdempotencyKey == "" || idempotencyKeyCommand(c.Command) {
		return nil
	}
	supported := "--idempotency-key is supported by looper add-task, looper add-plan, looper cancel, looper amend, and looper config"
	if c.Command == "stop" {
		supported += "; looper stop derives its request identity from the controller it targets"
	}
	p := usageFailure("option_command_mismatch", supported)
	p.Expected = map[string]any{"option": "--idempotency-key", "command": "add-task, add-plan, cancel, amend, or config"}
	p.Observed = map[string]any{"option": "--idempotency-key", "command": c.Command}
	return p
}

// validateCancelFlags checks the cancellation options against the command
// that received them. --cascade belongs to looper cancel alone.
func validateCancelFlags(c Config) error {
	if !c.Cascade || c.Command == "cancel" {
		return nil
	}
	p := usageFailure("option_command_mismatch", "--cascade is supported by looper cancel")
	p.Expected = map[string]any{"option": "--cascade", "command": "cancel"}
	p.Observed = map[string]any{"option": "--cascade", "command": c.Command}
	return p
}

// validateInterruptFlags checks --interrupt. It belongs to looper stop, and
// an interrupt has no durable result to wait for.
func validateInterruptFlags(c Config) error {
	if !c.Interrupt {
		return nil
	}
	if c.Command != "stop" {
		p := usageFailure("option_command_mismatch", "--interrupt is supported by looper stop")
		p.Expected = map[string]any{"option": "--interrupt", "command": "stop"}
		p.Observed = map[string]any{"option": "--interrupt", "command": c.Command}
		return p
	}
	if c.Wait {
		p := usageFailure("option_conflict", "--interrupt does not combine with --wait; run looper status to observe the exit")
		p.Observed = map[string]any{"--interrupt": true, "--wait": true}
		return p
	}
	return nil
}

// validateForegroundFlags checks --foreground. Only looper tui chooses
// between a daemon controller and one inside the dashboard process.
func validateForegroundFlags(c Config) error {
	if !c.Foreground || c.Command == "tui" {
		return nil
	}
	p := usageFailure("option_command_mismatch", "--foreground is supported by looper tui")
	p.Expected = map[string]any{"option": "--foreground", "command": "tui"}
	p.Observed = map[string]any{"option": "--foreground", "command": c.Command}
	return p
}

// afterOptionCommand reports whether a command accepts --after: the journal
// commands continue after an event ID, inspect after an attempt number, and
// the request listing after a request identifier.
func afterOptionCommand(command string) bool {
	return eventQueryCommand(command) || inspectQueryCommand(command) || requestsQueryCommand(command)
}

// fullResponseCommand reports whether a command accepts --full: the events
// export, the complete status document, and the complete inspection history.
func fullResponseCommand(command string) bool {
	return command == "events" || command == "status" || command == "inspect"
}

// pageLimitMax names the largest page one command may return for --limit.
func pageLimitMax(command string) int {
	if inspectQueryCommand(command) {
		return maxInspectPageLimit
	}
	if runsQueryCommand(command) {
		return maxRunsPageLimit
	}
	if requestsQueryCommand(command) {
		return maxRequestsPageLimit
	}
	return maxEventPageLimit
}

// validateEventQueryFlags checks the bounded query options against the
// command that received them. The options bound one finite read; refusing a
// contradictory combination at parse time keeps every later query honest
// about what it returns.
func validateEventQueryFlags(c Config) error {
	options := []struct {
		name      string
		set       bool
		commands  string
		supported bool
	}{
		{"--task", c.EventTask != "", "looper events, looper tail, and looper wait", eventQueryCommand(c.Command) || waitCommand(c.Command)},
		{"--kind", c.EventKind != "", "looper events and looper tail", eventQueryCommand(c.Command)},
		{"--limit", c.EventLimit != 0, "looper events, looper tail, looper inspect, looper runs, and looper requests", eventQueryCommand(c.Command) || inspectQueryCommand(c.Command) || runsQueryCommand(c.Command) || requestsQueryCommand(c.Command)},
		{"--full", c.Full, "looper events, looper status, and looper inspect", fullResponseCommand(c.Command)},
		{"--after", c.AfterSet, "looper events, looper tail, looper inspect, and looper requests", afterOptionCommand(c.Command)},
		{"--checks-after", c.CheckAfterSet, "looper inspect", inspectQueryCommand(c.Command)},
	}
	for _, option := range options {
		if option.set && !option.supported {
			p := usageFailure("option_command_mismatch", option.name+" is supported by "+option.commands)
			p.Expected = map[string]any{"option": option.name, "command": option.commands}
			p.Observed = map[string]any{"option": option.name, "command": c.Command}
			return p
		}
	}
	if limit := c.EventLimit; limit > 0 && (eventQueryCommand(c.Command) || inspectQueryCommand(c.Command) || runsQueryCommand(c.Command) || requestsQueryCommand(c.Command)) {
		if max := pageLimitMax(c.Command); limit > max {
			if runsQueryCommand(c.Command) || requestsQueryCommand(c.Command) {
				// The run listing and the request listing have no --full
				// export: both are bounded by design and report what they
				// omit, so the action names the largest page instead.
				listing := "run listing"
				if requestsQueryCommand(c.Command) {
					listing = "request listing"
				}
				p := usageFailure("option_value_invalid", fmt.Sprintf("--limit must be at most %d; the %s is bounded and reports what it omits", max, listing))
				p.Expected = map[string]any{"--limit": fmt.Sprintf("1 to %d", max)}
				p.Observed = map[string]any{"--limit": limit}
				p.Actions = []ErrorAction{{Description: "Read one bounded page.", Command: fmt.Sprintf("looper %s --limit %d --contract v2 --json", c.Command, max)}}
				return p
			}
			p := usageFailure("option_value_invalid", fmt.Sprintf("--limit must be at most %d; ask for --full when the complete history is wanted", max))
			p.Expected = map[string]any{"--limit": fmt.Sprintf("1 to %d", max)}
			p.Observed = map[string]any{"--limit": limit}
			p.Actions = []ErrorAction{{Description: "Bound one finite query with a smaller page, or export the complete history.", Command: "looper " + c.Command + " --full"}}
			return p
		}
	}
	if requestsQueryCommand(c.Command) && c.RequestID != "" {
		// One record query takes no position and no page bound: the record
		// is the whole response.
		contradictions := []struct {
			option string
			set    bool
			reason string
		}{
			{"--after", c.AfterSet, "--after continues a list page; remove it to read one record"},
			{"--limit", c.EventLimit != 0, "--limit bounds a list page; one record needs no bound"},
		}
		for _, conflict := range contradictions {
			if conflict.set {
				p := usageFailure("option_conflict", conflict.reason)
				p.Observed = map[string]any{"request_id": clampString(c.RequestID, 128), conflict.option: true}
				return p
			}
		}
	}
	if !c.Full || !fullResponseCommand(c.Command) {
		return nil
	}
	// The complete-history export is explicit: it carries no position and no
	// bound. The complete status and inspection documents carry neither, so
	// the contradictions below cover every --full command.
	contradictions := []struct {
		option string
		set    bool
		reason string
	}{
		{"--after", c.AfterSet, "--full reads the complete history from the beginning; remove --after"},
		{"--follow", c.Follow, "--full returns one complete document; a followed stream is unbounded. Remove --follow"},
		{"--limit", c.EventLimit != 0, "--full returns every matching record; remove --limit"},
		{"--checks-after", c.CheckAfterSet, "--full returns every matching record; remove --checks-after"},
	}
	for _, conflict := range contradictions {
		if conflict.set {
			p := usageFailure("option_conflict", conflict.reason)
			p.Observed = map[string]any{"--full": true, conflict.option: true}
			return p
		}
	}
	return nil
}

// validateRecoveryFlags checks the recovery options against the command that
// received them. The preview and the checked application are the two
// operations looper recovery serves (AX-016, AX-017): exactly one must be
// selected, an application requires its preview token, and a token belongs to
// an application alone.
func validateRecoveryFlags(c Config) error {
	if c.Preview && c.Command != "recovery" {
		p := usageFailure("option_command_mismatch", "--preview is supported by looper recovery")
		p.Expected = map[string]any{"option": "--preview", "command": "recovery"}
		p.Observed = map[string]any{"option": "--preview", "command": c.Command}
		return p
	}
	if (c.RecoveryChoice != "" || c.RecoveryToken != "") && c.Command != "recovery" {
		p := usageFailure("option_command_mismatch", "--apply and --token are supported by looper recovery")
		p.Expected = map[string]any{"option": "--apply, --token", "command": "recovery"}
		p.Observed = map[string]any{"option": "--apply, --token", "command": c.Command}
		return p
	}
	if c.Command != "recovery" {
		return nil
	}
	if c.Preview && c.RecoveryChoice != "" {
		p := usageFailure("option_conflict", "--preview reads the recovery choices; --apply applies one. Select one operation.")
		p.Observed = map[string]any{"--preview": true, "--apply": c.RecoveryChoice}
		return p
	}
	if c.RecoveryChoice != "" && c.RecoveryToken == "" {
		p := usageFailure("usage_invalid", "an application requires the token from looper recovery --preview")
		p.Expected = map[string]any{"invocation": "looper recovery --apply <choice> --token <token>"}
		p.Observed = map[string]any{"invocation": "looper recovery --apply " + c.RecoveryChoice}
		p.Actions = []ErrorAction{{Description: "Preview the recovery choices and their tokens.", Command: "looper recovery --preview --contract v2 --json"}}
		return p
	}
	if c.RecoveryToken != "" && c.RecoveryChoice == "" {
		p := usageFailure("usage_invalid", "--token belongs to a recovery application; name the choice with --apply")
		p.Expected = map[string]any{"invocation": "looper recovery --apply <choice> --token <token>"}
		p.Observed = map[string]any{"option": "--token", "value": "present", "--apply": "missing"}
		return p
	}
	if !c.Preview && c.RecoveryChoice == "" {
		p := usageFailure("usage_invalid", "looper recovery serves two operations: --preview and --apply <choice> --token <token>")
		p.Expected = map[string]any{"invocation": "looper recovery --preview or looper recovery --apply <choice> --token <token>"}
		p.Observed = map[string]any{"invocation": "looper recovery"}
		p.Actions = []ErrorAction{{Description: "Preview the recovery choices for the saved run.", Command: "looper recovery --preview"}}
		return p
	}
	if c.RecoveryToken != "" && !recoveryTokenPattern.MatchString(c.RecoveryToken) {
		return optionValueInvalid("--token", c.RecoveryToken, "the 64-hex-digit token from looper recovery --preview")
	}
	// The mutation options belong to the application: the preview is one
	// read with nothing to wait for and nothing a key makes retry-safe.
	if c.Preview && (c.Wait || c.IdempotencyKey != "") {
		p := usageFailure("option_conflict", "--preview is one read; --wait and --idempotency-key belong to looper recovery --apply")
		p.Observed = map[string]any{"--preview": true, "--wait": c.Wait, "--idempotency-key": c.IdempotencyKey != ""}
		return p
	}
	return nil
}

// Parse splits parsing from resolution: flags are collected first, the saved
// profile loads as the lowest non-default layer, present environment
// variables follow, and explicit flags win. Final resolution (automatic jobs,
// the review-agent fallback, smart mode) happens once after the merge.
func Parse(args []string) (Config, error) {
	root, err := os.Getwd()
	if err != nil {
		return Config{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Config{}, err
	}
	home, _ := os.UserHomeDir()
	c := Config{Root: root, Todo: "to-do.json", BaseDir: env("LOOPER_BASE_DIR", filepath.Join(home, ".looper")), CodexBin: env("CODEX_BIN", "codex"), ClaudeBin: env("CLAUDE_BIN", "claude"), ZcodeBin: env("ZCODE_BIN", "zcode"), Assess: env("LOOPER_ASSESS_COMMAND", ""), Hook: env("LOOPER_HOOK", ""), Command: "run", GitInit: env("LOOPER_GIT_INIT", "1") != "0"}
	for k, p := range map[string]*int{"MAX_ITERATIONS": &c.Limit, "LOOPER_MAX_REPAIRS": &c.MaxRepairs, "LOOPER_RATE_LIMIT_MAX": &c.RetryLimit} {
		d := 0
		switch k {
		case "LOOPER_MAX_REPAIRS":
			d = 6
		case "LOOPER_RATE_LIMIT_MAX":
			d = 12
		}
		v, e := integer(k, d)
		if e != nil {
			return c, e
		}
		*p = v
	}
	for k, p := range map[string]*time.Duration{"LOOPER_POLL_SECONDS": &c.Poll, "LOOPER_RATE_LIMIT_DELAY": &c.RetryDelay, "LOOPER_TIMEOUT_SECONDS": &c.Timeout, "LOOPER_GIT_TIMEOUT_SECONDS": &c.GitTimeout} {
		d := 30
		switch k {
		case "LOOPER_RATE_LIMIT_DELAY":
			d = 60
		case "LOOPER_TIMEOUT_SECONDS":
			d = 0
		case "LOOPER_GIT_TIMEOUT_SECONDS":
			d = 600
		}
		v, e := integer(k, d)
		if e != nil {
			return c, e
		}
		*p = time.Duration(v) * time.Second
	}
	// Environment validation keeps its original precedence: an invalid
	// numeric environment value fails before any argument is examined.
	if v, ok := os.LookupEnv("LOOPER_REVIEW_EVERY"); ok {
		if n, e := strconv.Atoi(v); e != nil || n < 0 {
			return c, environmentInvalid("LOOPER_REVIEW_EVERY", v)
		}
	}
	// Parsing: collect every option before touching the setting layers.
	flags := flagOptions{}
	contractFlag := false
	// commandNamed records that an argument named a command, so `--help`
	// knows whether it follows a command worth describing.
	commandNamed := false
	checks := env("LOOPER_CHECK_JOBS", "1")
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, error) {
			i++
			if i >= len(args) {
				p := usageFailure("option_needs_value", a+" needs a value")
				p.Observed = map[string]any{"option": a, "value": "missing"}
				return "", p
			}
			return args[i], nil
		}
		var v string
		switch a {
		case "run", "status", "stop", "doctor", "tail", "version", "start", "tui", "attach", "config", "add-task", "add-plan", "cancel", "amend", "events", "inspect", "recovery", "preflight", "runs", "requests", "wait", "help":
			if c.Command == "help" && a != "help" {
				// `looper help <command>`: the word names the block to serve,
				// not a second command.
				if c.HelpTopic != "" {
					return c, helpTopicFailure()
				}
				c.HelpTopic = a
				continue
			}
			if a == "help" && commandNamed {
				// `looper status help` mirrors `looper status --help`.
				if c.HelpTopic != "" {
					return c, helpTopicFailure()
				}
				c.HelpTopic = c.Command
			}
			c.Command = a
			commandNamed = true
		case "--help", "-h":
			// Contextual help: a command the same line already named stays
			// the topic, so `looper status --help` describes status while a
			// bare `--help` describes the whole surface (AX-026).
			if commandNamed {
				if c.HelpTopic != "" {
					return c, helpTopicFailure()
				}
				c.HelpTopic = c.Command
			}
			c.Command = "help"
		case "--version":
			c.Command = "version"
			commandNamed = true
		case "--doctor":
			c.Command = "doctor"
		case "--tail":
			c.Command = "tail"
		case "--after":
			v, err = value()
			if err != nil {
				return c, err
			}
			if requestsQueryCommand(c.Command) {
				// The request listing continues after a request identifier,
				// not after a numeric position.
				if v == "" || strings.ContainsAny(v, " \t\n") {
					return c, optionValueInvalid("--after", v, "a request identifier")
				}
				c.AfterID, c.AfterSet = v, true
				continue
			}
			c.After, err = strconv.ParseInt(v, 10, 64)
			if err != nil || c.After < 0 {
				return c, optionValueInvalid("--after", v, "a nonnegative continuation position")
			}
			c.AfterSet = true
		case "--checks-after":
			v, err = value()
			if err != nil {
				return c, err
			}
			c.CheckAfter, err = strconv.ParseInt(v, 10, 64)
			if err != nil || c.CheckAfter < 0 {
				return c, optionValueInvalid("--checks-after", v, "a nonnegative check ID")
			}
			c.CheckAfterSet = true
		case "--test-agent":
			v, err = value()
			if err != nil {
				return c, err
			}
			if !agentProfileName.MatchString(v) {
				return c, optionValueInvalid("--test-agent", v, "an agent profile name")
			}
			c.TestAgent = v
		case "--follow":
			c.Follow = true
		case "--cursor":
			v, err = value()
			if err != nil {
				return c, err
			}
			n, e := strconv.ParseInt(v, 10, 64)
			if e != nil || n < 0 {
				return c, optionValueInvalid("--cursor", v, "a nonnegative event identifier")
			}
			c.WaitCursor, c.WaitCursorSet = n, true
		case "--request":
			v, err = value()
			if err != nil {
				return c, err
			}
			if v == "" || strings.ContainsAny(v, " \t\n") {
				return c, optionValueInvalid("--request", v, "a request identifier")
			}
			c.WaitRequestID = v
		case "--run":
			c.WaitRunOutcome = true
		case "--status":
			v, err = value()
			if err != nil {
				return c, err
			}
			if !waitTaskStatuses[v] {
				return c, optionValueInvalid("--status", v, "one of todo, doing, blocked, done, or cancelled")
			}
			c.WaitTaskStatus = v
		case "--timeout":
			v, err = value()
			if err != nil {
				return c, err
			}
			bound, e := parseWaitTimeout(v)
			if e != nil {
				return c, e
			}
			c.WaitFor, c.WaitForSet = bound, true
		case "--wait":
			c.Wait = true
		case "--idempotency-key":
			v, err = value()
			if err != nil {
				return c, err
			}
			if keyErr := validateIdempotencyKey(v); keyErr != nil {
				p := optionValueInvalid("--idempotency-key", v, "1 to 128 bytes without control characters")
				p.Message = keyErr.Error()
				return c, p
			}
			c.IdempotencyKey = v
		case "--wait-timeout":
			v, err = value()
			if err != nil {
				return c, err
			}
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 {
				return c, optionValueInvalid("--wait-timeout", v, "a positive number of seconds")
			}
			c.WaitTimeout, c.WaitTimeoutSet = time.Duration(n)*time.Second, true
		case "--full":
			c.Full = true
		case "--preview":
			c.Preview = true
		case "--apply":
			v, err = value()
			if err != nil {
				return c, err
			}
			if c.RecoveryChoice != "" {
				p := usageFailure("usage_invalid", "recovery accepts one application choice")
				p.Observed = map[string]any{"choices": 2}
				return c, p
			}
			c.RecoveryChoice = v
		case "--token":
			v, err = value()
			if err != nil {
				return c, err
			}
			if c.RecoveryToken != "" {
				p := usageFailure("usage_invalid", "recovery accepts one preview token")
				p.Observed = map[string]any{"tokens": 2}
				return c, p
			}
			c.RecoveryToken = v
		case "--task":
			v, err = value()
			if err != nil {
				return c, err
			}
			c.EventTask = v
		case "--kind":
			v, err = value()
			if err != nil {
				return c, err
			}
			c.EventKind = v
		case "--limit":
			v, err = value()
			if err != nil {
				return c, err
			}
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 {
				return c, optionValueInvalid("--limit", v, "a positive integer")
			}
			c.EventLimit = n
		case "--once":
			c.Once = true
		case "--json":
			c.JSON = true
		case "--plain":
			c.Plain = true
		case "--no-animation":
			c.NoAnimation = true
		case "--yes", "-y":
			c.Yes = true
		case "--reset":
			c.Reset = true
		case "--cascade":
			c.Cascade = true
		case "--interrupt":
			c.Interrupt = true
		case "--foreground":
			c.Foreground = true
		case "--verbose", "-v":
			c.Verbose = true
		case "--smart", "-s":
			c.Smart = true
		case "--interleave":
			flags.apply(func(s *settingSources) {
				// At its command-line position: a later --agent, --all, or
				// --review-agent still wins. The preset fills an empty review
				// agent from any layer, so a present-but-empty value keeps a
				// second-opinion reviewer.
				s.agent = inputValue{"claude", SourceFlag}
				if s.reviewAgent.value == "" {
					s.reviewAgent = inputValue{"codex", SourceFlag}
				}
			})
		case "--plan":
			v, err = value()
			if err != nil {
				return c, err
			}
			path, err := filepath.Abs(v)
			if err != nil {
				return c, err
			}
			c.Plans = append(c.Plans, filepath.Clean(path))
		case "--jobs", "--check-jobs", "--agent", "--all", "--review-agent", "--model", "--effort", "--max-iterations", "--review-every", "--ls", "--contract":
			v, err = value()
			if err != nil {
				return c, err
			}
			switch a {
			case "--jobs":
				flags.apply(func(s *settingSources) { s.jobs = inputValue{v, SourceFlag} })
			case "--check-jobs":
				checks = v
			case "--agent":
				flags.apply(func(s *settingSources) { s.agent = inputValue{v, SourceFlag} })
			case "--all":
				flags.apply(func(s *settingSources) {
					s.agent = inputValue{v, SourceFlag}
					s.reviewAgent = inputValue{v, SourceFlag}
				})
			case "--review-agent":
				flags.apply(func(s *settingSources) { s.reviewAgent = inputValue{v, SourceFlag} })
			case "--model":
				flags.apply(func(s *settingSources) {
					s.model = inputValue{v, SourceFlag}
					s.claudeModel = inputValue{v, SourceFlag}
				})
			case "--effort":
				flags.apply(func(s *settingSources) { s.effort = inputValue{v, SourceFlag} })
			case "--ls":
				c.Command = "status"
				c.ListStatus = v
			case "--contract":
				// An explicit flag wins over LOOPER_CONTRACT; an unset
				// variable selects the default version.
				contract, e := parseContractVersion(v)
				if e != nil {
					return c, e
				}
				c.Contract = contract
				contractFlag = true
			case "--max-iterations", "--review-every":
				n, e := strconv.Atoi(v)
				if e != nil || n < 0 {
					return c, fmt.Errorf("%s needs a nonnegative integer", a)
				}
				if a == "--max-iterations" {
					c.Limit = n
				} else {
					flags.apply(func(s *settingSources) { s.reviewEvery = intValue{n, SourceFlag} })
				}
			}
		default:
			if strings.HasPrefix(a, "-") {
				p := usageFailure("unknown_option", "unknown option "+a)
				p.Observed = map[string]any{"option": a}
				return c, p
			}
			if c.Command == "help" {
				// `looper help <command>` names the block to serve.
				if c.HelpTopic != "" {
					return c, helpTopicFailure()
				}
				c.HelpTopic = a
				continue
			}
			if c.Command == "add-plan" || c.Command == "add-task" {
				// The positional argument of add-plan is a plan path, never
				// a task file, so Markdown cannot be mistaken for a backlog.
				path, e := filepath.Abs(a)
				if e != nil {
					return c, e
				}
				c.Plans = append(c.Plans, filepath.Clean(path))
				continue
			}
			if c.Command == "inspect" {
				if c.InspectTask != "" {
					p := usageFailure("usage_invalid", "inspect accepts one task ID")
					p.Observed = map[string]any{"task_ids": 2}
					return c, p
				}
				c.InspectTask = a
				continue
			}
			if c.Command == "requests" {
				if c.RequestID != "" {
					p := usageFailure("usage_invalid", "requests accepts one request identifier")
					p.Observed = map[string]any{"request_ids": 2}
					return c, p
				}
				c.RequestID = a
				continue
			}
			if c.Command == "cancel" {
				if c.CancelTask != "" {
					p := usageFailure("usage_invalid", "cancel accepts one task ID")
					p.Observed = map[string]any{"task_ids": 2}
					return c, p
				}
				c.CancelTask = a
				continue
			}
			if c.Command == "wait" {
				// A positional cannot name a predicate: the wait options are
				// flags, so a bare word is a task-file path nobody asked for.
				p := usageFailure("usage_invalid", "looper wait takes no positional argument; name the predicate with --cursor, --task, --request, or --run")
				p.Observed = map[string]any{"positional_arg": clampString(a, 128)}
				return c, p
			}
			if c.Command == "amend" {
				if c.AmendTask == "" {
					c.AmendTask = a
					continue
				}
				if c.AmendFile == "" {
					c.AmendFile = a
					continue
				}
				p := usageFailure("usage_invalid", "amend accepts one task ID and one task file")
				p.Observed = map[string]any{"positional_args": 3}
				return c, p
			}
			// A bare word that names no file is a command spelling Looper
			// does not know, not a task file: it must fail as a usage error
			// before any controller, agent, or runtime record starts (AX-026).
			if !taskFileArgument(a) {
				p := usageFailure("unknown_command", fmt.Sprintf("unknown command %q", a))
				p.Observed = map[string]any{"command": clampString(a, 64)}
				p.Actions = []ErrorAction{{
					Description: "Show the overview, or one command's own help.",
					Command:     "looper help",
				}}
				return c, p
			}
			c.Todo = a
		}
	}
	if c.TestAgent != "" {
		if c.Command != "config" {
			return c, usageFailure("usage_invalid", "--test-agent is only available with looper config")
		}
		if c.Reset || c.Wait || c.WaitTimeoutSet || c.IdempotencyKey != "" {
			return c, usageFailure("usage_invalid", "--test-agent cannot be combined with settings reset or request options")
		}
	}
	// Event-query options are validated while the command is known: a
	// refusal here is a usage failure, so nothing is read or written.
	if err = validateEventQueryFlags(c); err != nil {
		return c, err
	}
	if err = validateHelpFlags(c); err != nil {
		return c, err
	}
	if err = validateWaitFlags(c); err != nil {
		return c, err
	}
	if err = validateRecoveryFlags(c); err != nil {
		return c, err
	}
	if err = validateMutationWaitFlags(c); err != nil {
		return c, err
	}
	if err = validateIdempotencyKeyFlags(c); err != nil {
		return c, err
	}
	if err = validateCancelFlags(c); err != nil {
		return c, err
	}
	if err = validateForegroundFlags(c); err != nil {
		return c, err
	}
	if err = validateInterruptFlags(c); err != nil {
		return c, err
	}
	// Resolution starts here. Layer 1 is the built-in defaults; automatic
	// jobs resolve at controller startup, not during parsing.
	settings := settingSources{jobs: inputValue{"auto", SourceDefault}, reviewEvery: intValue{5, SourceDefault}}
	c.BaseDir, err = filepath.Abs(c.BaseDir)
	if err != nil {
		return c, err
	}
	if err = resolveAgentProfiles(&c); err != nil {
		return c, usageFailure("agent_configuration_invalid", err.Error())
	}
	if c.DefaultWorker != "" {
		settings.agent = inputValue{c.DefaultWorker, SourceMachine}
	}
	if c.DefaultReviewer != "" {
		settings.reviewAgent = inputValue{c.DefaultReviewer, SourceMachine}
	}
	// Layer 2: the saved profile, below any environment variable or flag.
	c.ProfilePath = ProfilePath(c)
	var profile *Profile
	// A corrupt profile blocks every command that applies settings, but
	// `config --reset` must still be able to remove it.
	if usesProfile(c.Command) && !(c.Command == "config" && c.Reset) {
		if profile, err = LoadProfile(c.ProfilePath); err != nil {
			return c, profileUnreadableError(err, c.ProfilePath)
		}
	}
	profile.mergeInto(&settings)
	// Layer 3: present environment variables. An explicitly empty variable
	// counts as present and overrides the profile with an empty value.
	for k, into := range map[string]*inputValue{
		"LOOPER_ITER_SCHEDULE": &settings.agent, "LOOPER_REVIEW_AGENT": &settings.reviewAgent,
		"CODEX_MODEL": &settings.model, "CLAUDE_MODEL": &settings.claudeModel,
		"CODEX_REASONING_EFFORT": &settings.effort, "LOOPER_VERIFY_COMMAND": &settings.verify,
		"LOOPER_SETUP_COMMAND": &settings.setup,
	} {
		if v, ok := os.LookupEnv(k); ok {
			*into = inputValue{v, SourceEnv}
		}
	}
	if v, ok := os.LookupEnv("LOOPER_JOBS"); ok {
		settings.jobs = inputValue{v, SourceEnv}
	}
	if v, ok := os.LookupEnv("LOOPER_REVIEW_EVERY"); ok {
		n, _ := strconv.Atoi(v) // validated before parsing
		settings.reviewEvery = intValue{n, SourceEnv}
	}
	// Layer 4: explicit flags, replayed in command-line order so the last
	// flag for each field wins, exactly as the original parser did.
	for _, fn := range flags.apps {
		fn(&settings)
	}
	if settings.jobs.value == "auto" {
		c.JobsAuto = true
		c.Jobs = max(1, min(4, runtime.NumCPU()/2))
		if m := availableMemory(); m > 0 {
			c.Jobs = min(c.Jobs, max(1, int(m/(2<<30))))
		}
	} else {
		c.Jobs, err = strconv.Atoi(settings.jobs.value)
		if err != nil || c.Jobs < 1 {
			return c, optionValueInvalid("jobs", settings.jobs.value, "auto or a positive integer")
		}
	}
	c.Checks, err = strconv.Atoi(checks)
	if err != nil || c.Checks < 1 {
		return c, optionValueInvalid("--check-jobs", checks, "a positive integer")
	}
	if c.Smart {
		settings.model = inputValue{env("CODEX_SMART_MODEL", "gpt-6-astra"), SourceFlag}
		settings.effort = inputValue{env("CODEX_SMART_REASONING_EFFORT", "high"), SourceFlag}
	}
	c.Agent = settings.agent.value
	if c.Agent == "" && settings.agent.from == SourceDefault {
		c.Agent = defaultAgent
	}
	c.ReviewAgent = settings.reviewAgent.value
	if c.ReviewAgent == "" {
		c.ReviewAgent = c.Agent
	}
	c.Model, c.ClaudeModel, c.Effort = settings.model.value, settings.claudeModel.value, settings.effort.value
	c.Verify, c.Setup = settings.verify.value, settings.setup.value
	c.ReviewEvery = settings.reviewEvery.value
	c.Sources = Sources{
		Agent: settings.agent.from, ReviewAgent: settings.reviewAgent.from,
		Jobs: settings.jobs.from, Model: settings.model.from, ClaudeModel: settings.claudeModel.from,
		Effort: settings.effort.from, Verify: settings.verify.from, Setup: settings.setup.from,
		ReviewEvery: settings.reviewEvery.from,
	}
	for _, a := range []string{c.Agent, c.ReviewAgent} {
		if _, resolveErr := c.agentProfile(a); resolveErr != nil && c.Command != "config" {
			p := usageFailure("agent_unsupported", resolveErr.Error())
			p.Expected = map[string]any{"agent": "codex, claude, or zcode"}
			p.Observed = map[string]any{"agent": a}
			if len(c.AgentProfiles) > 0 {
				p.Expected["configured_profiles"] = agentProfileNames(c)
			}
			p.Actions = []ErrorAction{{Description: "Add or select an agent profile.", Command: "looper config"}}
			return c, p
		}
	}
	// Contract selection resolves after the command is known. Every command
	// serves the version 2 error contract (AX-014); the remaining version 2
	// documents ship per command with their owning tasks, and a command whose
	// success document has not shipped keeps its legacy success document, as
	// docs/AGENT-CONTRACT-MIGRATION.md records.
	if !contractFlag {
		if v, ok := os.LookupEnv("LOOPER_CONTRACT"); ok {
			contract, e := parseContractVersion(v)
			if e != nil {
				return c, e
			}
			c.Contract = contract
		}
	}
	if c.Contract == 0 {
		c.Contract = DefaultContractVersion
	}
	if c.Poll <= 0 {
		c.Poll = time.Second
	}
	if c.RetryLimit < 1 {
		c.RetryLimit = 1
	}
	if !filepath.IsAbs(c.Todo) {
		c.Todo = filepath.Join(c.Root, c.Todo)
	}
	c.Todo = filepath.Clean(c.Todo)
	gitTimeout = c.GitTimeout
	return c, nil
}

// The schema control file is named after the task file: a trailing .json is
// replaced by .schema.json, as in the earlier shell controller.
func (c Config) SchemaPath() string {
	base := filepath.Base(c.Todo)
	if strings.HasSuffix(base, ".json") {
		base = strings.TrimSuffix(base, ".json")
	}
	return filepath.Join(filepath.Dir(c.Todo), base+".schema.json")
}

// memoryFiles names the files the memory bound reads: procfs for the host and
// one entry per cgroup hierarchy (limit, usage, stat). Tests substitute fixture
// paths so container bounds stay checkable on any host; production points at
// the real files.
type memoryFiles struct {
	meminfo string
	cgroups [][3]string
}

var hostMemoryFiles = memoryFiles{
	meminfo: "/proc/meminfo",
	cgroups: [][3]string{
		{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory.current", "/sys/fs/cgroup/memory.stat"},
		{"/sys/fs/cgroup/memory/memory.limit_in_bytes", "/sys/fs/cgroup/memory/memory.usage_in_bytes", "/sys/fs/cgroup/memory/memory.stat"},
	},
}

// Use a conservative bound when Linux reports memory or container limits:
// inside a container, /proc/meminfo shows the host, not the cgroup limit.
func availableMemory() int64 {
	bounds := []int64{}
	if n := memAvailable(hostMemoryFiles.meminfo); n > 0 {
		bounds = append(bounds, n)
	}
	for _, paths := range hostMemoryFiles.cgroups {
		if n := cgroupHeadroom(paths[0], paths[1], paths[2]); n > 0 {
			bounds = append(bounds, n)
		}
	}
	if len(bounds) == 0 {
		return 0
	}
	m := bounds[0]
	for _, b := range bounds[1:] {
		m = min(m, b)
	}
	return m
}
func memAvailable(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		var k string
		var n int64
		if _, err := fmt.Sscan(l, &k, &n); err == nil && k == "MemAvailable:" {
			return n * 1024
		}
	}
	return 0
}
func cgroupHeadroom(limitPath, usagePath, statPath string) int64 {
	limit, err := readCount(limitPath)
	if err != nil || limit <= 0 || limit > 1<<40 { // "max" or a legacy unlimited value
		return 0
	}
	used, err := readCount(usagePath)
	if err != nil {
		return 0
	}
	// The usage counter includes reclaimable page cache, which the kernel
	// reclaims before declaring memory pressure, so bound by the working
	// set as container runtimes do.
	if cache := statCount(statPath, "inactive_file", "total_inactive_file"); cache > 0 {
		used -= min(used, cache)
	}
	return max(0, limit-used)
}

// statCount reads one memory-stat counter from a cgroup memory.stat file.
func statCount(path string, keys ...string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		for _, key := range keys {
			if fields[0] == key {
				if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil && n > 0 {
					return n
				}
			}
		}
	}
	return 0
}
func readCount(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}
