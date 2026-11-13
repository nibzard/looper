package looper

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The preflight contract, docs/contracts/schemas/v2/preflight.schema.json.
const (
	preflightSchemaVersion = 2
	preflightKind          = "looper.preflight"
	// preflightBytesBudget bounds one preflight document. The document is a
	// decision basis, not a dump: every collection stays small and the bounds
	// that applied are stated.
	preflightBytesBudget = 8192
)

// preflightFit names the bounds one rendering pass applies. A document over
// the byte budget renders again with the next fit: strings shrink first and
// findings drop entries only when that is not enough. Every bound counts the
// bytes the JSON encoder emits, so text that escapes heavily — an ampersand
// becomes six bytes — shrinks with the same ladder instead of overflowing it.
// The ladder ends in preflightTerminalFit, whose string bound leaves every
// content-derived string a single rune at worst, so a document always fits
// whatever the configuration holds.
type preflightFit struct {
	stringMax   int
	findingsMax int
}

var preflightFits = []preflightFit{{256, 16}, {96, 8}, preflightTerminalFit}

// preflightTerminalFit is the ladder's last resort. Its string bound leaves
// every content-derived string a single rune at worst, so the document it
// renders is structure alone — a size fixed by the contract's fixed fields,
// not by how large a configured value grew. The strings the schema pins
// (agent names, sandbox modes, sources, states) are literals that never pass
// through the clamp, so the terminal document stays schema-valid.
// TestPreflightTerminalFitBoundsAnyContent pins that worst case under the
// byte budget.
var preflightTerminalFit = preflightFit{stringMax: clampJSONStringMin, findingsMax: 8}

// preflightClampOf returns the clamp one fit applies to a content-derived
// string: scrubbed of credential shapes, then bounded by the bytes the JSON
// encoder emits, so escape-heavy text cannot outgrow the fit that stated it.
// Fixed literals the schema constrains never pass through it.
func preflightClampOf(fit preflightFit) func(string) string {
	return func(s string) string { return clampJSONString(scrubSecrets(s), fit.stringMax) }
}

// preflightDocument is the one JSON document `looper preflight --contract v2
// --json` returns. It resolves the execution configuration a controller start
// would use — executable paths, coding and review agents, models and effort
// with their sources, launch mode, verification and setup, and once semantics
// — without starting a controller, spawning an agent, or changing saved
// state. Credentials, environment values, and raw agent arguments never enter
// the document: only resolved values, their provenance, and the states
// Looper did not verify.
type preflightDocument struct {
	ContractVersion int                  `json:"contract_version"`
	SchemaVersion   int                  `json:"schema_version"`
	Kind            string               `json:"kind"`
	GeneratedAt     string               `json:"generated_at"`
	Project         preflightProject     `json:"project"`
	Agents          preflightAgents      `json:"agents"`
	Settings        preflightSettings    `json:"settings"`
	Execution       preflightExecution   `json:"execution"`
	Commands        preflightCommands    `json:"commands"`
	Environment     []preflightEnvVar    `json:"environment"`
	Findings        []preflightFinding   `json:"findings"`
	Unchecked       []preflightUnchecked `json:"unchecked"`
	Limits          preflightLimits      `json:"limits"`
	Detail          preflightDetail      `json:"detail"`
}

type preflightProject struct {
	Root       string              `json:"root"`
	TodoPath   string              `json:"todo_path"`
	RuntimeDir string              `json:"runtime_dir"`
	Profile    preflightProfileRef `json:"profile"`
}

type preflightProfileRef struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
}

// preflightExecutable is one resolved executable. The state names what the
// resolution proved: found carries the resolved path, missing names a tool a
// controller cannot launch, and builtin and unchecked state that PATH
// resolution does not apply.
type preflightExecutable struct {
	Tool      string `json:"tool"`
	Requested string `json:"requested"`
	Source    string `json:"source,omitempty"` // environment or default; a command word has neither
	State     string `json:"state"`            // found, missing, builtin, or unchecked
	Resolved  string `json:"resolved,omitempty"`
	Note      string `json:"note,omitempty"`
}

type preflightAgent struct {
	Adapter       string              `json:"adapter,omitempty"`
	Model         string              `json:"model,omitempty"`
	Effort        string              `json:"effort,omitempty"`
	Name          string              `json:"name"`
	Source        string              `json:"source"` // flag, environment, profile, default, or derived
	FollowsCoding bool                `json:"follows_coding,omitempty"`
	Binary        preflightExecutable `json:"binary"`
	Sandbox       string              `json:"sandbox"`
	SandboxNote   string              `json:"sandbox_note"`
}

type preflightAgents struct {
	Coding preflightAgent `json:"coding"`
	Review preflightAgent `json:"review"`
}

// preflightSetting is one managed setting with its value, its source, and how
// the value resolves. An empty value with resolution provider_default means
// the agent binary decides at launch; Looper does not resolve the provider
// default, and the document says so instead of inventing one.
type preflightSetting struct {
	Value      string `json:"value"`
	Source     string `json:"source"`
	Resolution string `json:"resolution"` // configured, provider_default, or not_applicable
	Note       string `json:"note,omitempty"`
}

type preflightJobsSetting struct {
	Value  int    `json:"value"`
	Auto   bool   `json:"auto"`
	Source string `json:"source"`
	Note   string `json:"note,omitempty"`
}

type preflightChecksSetting struct {
	Value int    `json:"value"`
	Note  string `json:"note"`
}

type preflightSettings struct {
	Jobs        preflightJobsSetting   `json:"jobs"`
	Checks      preflightChecksSetting `json:"checks"`
	CodexModel  preflightSetting       `json:"codex_model"`
	ClaudeModel preflightSetting       `json:"claude_model"`
	Effort      preflightSetting       `json:"effort"`
	ReviewEvery preflightCountSetting  `json:"review_every"`
}

type preflightCountSetting struct {
	Value  int    `json:"value"`
	Source string `json:"source"`
}

type preflightExecution struct {
	Mode                 string         `json:"mode"`
	ModeNote             string         `json:"mode_note"`
	Once                 preflightOnce  `json:"once"`
	DispatchLimit        int            `json:"dispatch_limit"`
	PollSeconds          int            `json:"poll_seconds"`
	WorkerTimeoutSeconds int            `json:"worker_timeout_seconds"`
	GitTimeoutSeconds    int            `json:"git_timeout_seconds"`
	Retry                preflightRetry `json:"retry"`
	Plans                []string       `json:"plans"`
	PlanTotal            int            `json:"plan_total"`
}

type preflightOnce struct {
	Enabled bool   `json:"enabled"`
	Note    string `json:"note"`
}

type preflightRetry struct {
	Limit        int `json:"limit"`
	DelaySeconds int `json:"delay_seconds"`
}

// preflightCommand is the effective setup or verification command with its
// source and, when one is resolvable, its executable. A command chosen by the
// planner, recovered from a saved run, or taken from the saved plan states
// which, because that is the provenance a caller needs to predict what the
// next run executes.
type preflightCommand struct {
	Value      string               `json:"value"`
	Source     string               `json:"source"` // flag, environment, profile, plan, recovered, planner, or none
	Note       string               `json:"note,omitempty"`
	Executable *preflightExecutable `json:"executable,omitempty"`
}

type preflightCommands struct {
	Verify preflightCommand `json:"verify"`
	Setup  preflightCommand `json:"setup"`
}

// preflightEnvVar names one recognized environment variable and whether it is
// present. The value never enters the document: an environment value can
// carry a credential, so the report states only that the variable applies.
type preflightEnvVar struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
	Affects string `json:"affects"`
}

type preflightFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"` // error, warning, or info
	Message  string `json:"message"`
	Action   string `json:"action,omitempty"`
}

type preflightUnchecked struct {
	Code string `json:"code"`
	Note string `json:"note"`
}

type preflightLimits struct {
	BytesBudget int `json:"bytes_budget"`
	StringMax   int `json:"string_max"`
	FindingsMax int `json:"findings_max"`
}

type preflightDetail struct {
	Settings     string `json:"settings,omitempty"`
	Health       string `json:"health,omitempty"`
	Capabilities string `json:"capabilities,omitempty"`
}

// preflightEnvironmentNames lists the recognized environment variables that
// shape execution, with the setting each one affects. Values are never read
// into the document.
var preflightEnvironmentNames = []struct {
	name, affects string
}{
	{"LOOPER_AGENTS_FILE", "machine agent configuration"},
	{"XDG_CONFIG_HOME", "default machine configuration directory"},
	{"LOOPER_ITER_SCHEDULE", "coding agent"},
	{"LOOPER_REVIEW_AGENT", "review agent"},
	{"LOOPER_JOBS", "worker jobs"},
	{"LOOPER_REVIEW_EVERY", "review frequency"},
	{"LOOPER_CHECK_JOBS", "concurrent checks"},
	{"CODEX_MODEL", "codex model"},
	{"CLAUDE_MODEL", "claude model"},
	{"CODEX_REASONING_EFFORT", "codex reasoning effort"},
	{"CODEX_SMART_MODEL", "smart mode codex model default"},
	{"CODEX_SMART_REASONING_EFFORT", "smart mode effort default"},
	{"LOOPER_VERIFY_COMMAND", "verify command"},
	{"LOOPER_SETUP_COMMAND", "setup command"},
	{"CODEX_BIN", "codex executable"},
	{"CLAUDE_BIN", "claude executable"},
	{"ZCODE_BIN", "zcode executable"},
	{"CODEX_YOLO", "codex sandbox posture"},
	{"LOOPER_CONTRACT", "machine contract version"},
	{"LOOPER_BASE_DIR", "runtime base directory"},
	{"LOOPER_GIT_INIT", "git initialization"},
	{"LOOPER_POLL_SECONDS", "poll interval"},
	{"LOOPER_TIMEOUT_SECONDS", "worker timeout"},
	{"LOOPER_GIT_TIMEOUT_SECONDS", "git timeout"},
	{"MAX_ITERATIONS", "dispatch limit"},
	{"LOOPER_MAX_REPAIRS", "repair limit"},
	{"LOOPER_RATE_LIMIT_MAX", "agent retry limit"},
	{"LOOPER_RATE_LIMIT_DELAY", "agent retry delay"},
	{"LOOPER_HOOK", "result hook"},
	{"LOOPER_ASSESS_COMMAND", "assessment command"},
	{"LOOPER_NO_PROMPT", "first-run form"},
}

// shellBuiltins are words PATH resolution cannot decide; a command led by one
// runs inside the shell.
var shellBuiltins = map[string]bool{
	":": true, ".": true, "[": true, "cd": true, "eval": true, "exec": true,
	"exit": true, "export": true, "local": true, "return": true, "set": true,
	"shift": true, "source": true, "test": true, "true": true, "false": true,
	"unset": true, "wait": true,
}

// commandWord matches one plain command word: no shell syntax, no quoting, no
// separators. A first word outside it belongs to a compound command, which
// stays unchecked rather than guessed at.
var commandWord = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`)

func agentBinaryOf(c Config, agent string) string {
	p, err := c.agentProfile(agent)
	if err != nil {
		return ""
	}
	return p.Command
}

func agentBinaryEnv(agent string) string {
	switch agent {
	case "codex":
		return "CODEX_BIN"
	case "zcode":
		return "ZCODE_BIN"
	default:
		return "CLAUDE_BIN"
	}
}

// agentSandbox reports the launch posture a worker of this agent kind uses.
// The posture is a named mode, never the raw argument list.
func agentSandbox(agent string) (string, string) {
	if adapter, ok := agentAdapters[agent]; ok {
		return adapter.protocol.permissions()
	}
	return "unsupported", "no adapter is registered"
}

// probeBinary resolves one executable through PATH. LookPath starts nothing;
// it only resolves, so a preflight never launches the tool it names. The
// requested value and the resolved path are content a caller configured, so
// the report clamps both; the resolution itself still sees the true value.
func probeBinary(tool, requested, envName string, clamp func(string) string) preflightExecutable {
	source := "default"
	if envName != "" {
		if _, ok := os.LookupEnv(envName); ok {
			source = "environment"
		}
	}
	out := preflightExecutable{Tool: clamp(tool), Requested: clamp(requested), Source: source}
	if path, err := exec.LookPath(requested); err == nil {
		out.State, out.Resolved = "found", clamp(path)
		return out
	}
	out.State = "missing"
	return out
}

// probeCommand resolves the first word of one shell command. Compound
// commands and shell builtins stay unchecked: PATH cannot decide them, and
// the report never claims a resolution it did not perform. The word is
// content from the command text, so the report clamps it.
func probeCommand(command string, clamp func(string) string) *preflightExecutable {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil
	}
	first := fields[0]
	if !commandWord.MatchString(first) {
		return &preflightExecutable{Tool: clamp(first), Requested: clamp(first), State: "unchecked",
			Note: "compound command; the first word is not resolved through PATH"}
	}
	if shellBuiltins[first] {
		return &preflightExecutable{Tool: first, Requested: first, State: "builtin",
			Note: "shell builtin; it runs inside the shell"}
	}
	probe := probeBinary(first, first, "", clamp)
	// A command word was not requested from any configuration layer, so it
	// carries no binary provenance.
	probe.Source = ""
	return &probe
}

// preflightSavedRun is the read-only view of the saved run that affects
// configuration resolution: recovered command overrides and the plan's setup
// and verify commands.
type preflightSavedRun struct {
	state      *State
	unreadable error
}

// readSavedRun opens the saved state read-only, creating no file beside a
// stopped run's database. A missing store is not an
// error: the next run plans from scratch. Any other reason the state path
// could not be read — an unreadable runtime directory among them — keeps its
// error, because the overrides the store would carry stay unknown and the
// report must say so instead of guessing.
func readSavedRun(dir string) preflightSavedRun {
	path := filepath.Join(dir, "state.db")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return preflightSavedRun{}
		}
		return preflightSavedRun{unreadable: err}
	}
	store, err := openObservedStore(dir)
	if err != nil {
		return preflightSavedRun{unreadable: err}
	}
	defer store.Close()
	snap, err := store.Snapshot()
	if err != nil {
		return preflightSavedRun{unreadable: err}
	}
	return preflightSavedRun{state: snap.State}
}

// modelSetting renders one model or effort setting with its resolution. An
// empty value on an agent kind that never reads the field is not a provider
// default: the setting does not apply and the report says so. The value is
// content a caller configured, so the render clamp bounds it.
func modelSetting(value string, from Source, applies bool, unsetNote string, clamp func(string) string) preflightSetting {
	out := preflightSetting{Value: clamp(value), Source: from.String()}
	switch {
	case value != "":
		out.Resolution = "configured"
	case applies:
		out.Resolution = "provider_default"
		out.Note = unsetNote
	default:
		out.Resolution = "not_applicable"
		out.Note = "no agent of this kind is configured, so the setting is unused"
	}
	return out
}

// buildPreflight resolves the launch configuration into one bounded document.
// Every read is read-only: the profile and the saved run open for reading,
// binaries resolve through PATH, and no controller, agent, or runtime
// directory is created. Because every bound counts emitted bytes and the
// ladder ends in a fit whose strings are one rune at worst, the returned
// document is always valid JSON at or below preflightBytesBudget, whatever
// the configuration holds.
func buildPreflight(c Config) *preflightDocument {
	_, dir := projectPaths(c)
	saved := readSavedRun(dir)
	var doc *preflightDocument
	for _, fit := range preflightFits {
		doc = renderPreflight(c, dir, saved, fit)
		if preflightEncoded(doc) <= preflightBytesBudget {
			return doc
		}
	}
	return doc
}

func preflightEncoded(doc *preflightDocument) int {
	var b strings.Builder
	_ = json.NewEncoder(&b).Encode(doc)
	return b.Len()
}

func renderPreflight(c Config, dir string, saved preflightSavedRun, fit preflightFit) *preflightDocument {
	clamp := preflightClampOf(fit)
	doc := &preflightDocument{
		ContractVersion: ContractVersionV2,
		SchemaVersion:   preflightSchemaVersion,
		Kind:            preflightKind,
		GeneratedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	_, profileErr := os.Stat(c.ProfilePath)
	doc.Project = preflightProject{
		Root: clamp(c.Root), TodoPath: clamp(c.Todo), RuntimeDir: clamp(dir),
		Profile: preflightProfileRef{Path: clamp(c.ProfilePath), Present: profileErr == nil},
	}
	// Agents and their binaries. An empty review agent follows the coding
	// agent; that derivation is a source of its own.
	follows := c.ReviewAgent == c.Agent && c.Sources.ReviewAgent == SourceDefault
	newAgent := func(name string, from Source, derived bool) preflightAgent {
		sandbox, note := agentSandbox(c.adapterOf(name))
		result := preflightAgent{
			Name: name, Source: agentSourceLabel(from, derived), FollowsCoding: derived,
			Binary: probeBinary(name, agentBinaryOf(c, name), agentBinaryEnv(name), clamp),
			// The sandbox is a schema enum and a fixed literal, so no fit
			// bound applies to it; only the note passes through the clamp.
			Sandbox: sandbox, SandboxNote: clamp(note),
		}
		if _, named := c.AgentProfiles[name]; named {
			profile, _ := c.effectiveAgent(name)
			result.Adapter = profile.Adapter
			result.Model = clamp(orDefault(profile.Model, "agent settings"))
			result.Effort = clamp(orDefault(profile.Effort, "agent settings"))
			result.Binary.Source = "machine"
		}
		return result
	}
	doc.Agents = preflightAgents{
		Coding: newAgent(c.Agent, c.Sources.Agent, false),
		Review: newAgent(c.ReviewAgent, c.Sources.ReviewAgent, follows),
	}
	// Settings with their sources.
	jobs := preflightJobsSetting{Value: c.Jobs, Auto: c.JobsAuto, Source: c.Sources.Jobs.String()}
	if c.JobsAuto {
		jobs.Note = "auto mode sizes the worker count from CPU and memory"
	}
	uses := map[string]bool{c.adapterOf(c.Agent): true, c.adapterOf(c.ReviewAgent): true}
	doc.Settings = preflightSettings{
		Jobs:        jobs,
		Checks:      preflightChecksSetting{Value: c.Checks, Note: "--check-jobs or LOOPER_CHECK_JOBS; default 1"},
		CodexModel:  modelSetting(c.Model, c.Sources.Model, uses["codex"], "the codex binary chooses its own default model at launch", clamp),
		ClaudeModel: modelSetting(c.ClaudeModel, c.Sources.ClaudeModel, uses["claude"], "the claude binary chooses its own default model at launch", clamp),
		Effort:      modelSetting(c.Effort, c.Sources.Effort, uses["codex"], "the codex binary chooses its own default reasoning effort at launch", clamp),
		ReviewEvery: preflightCountSetting{Value: c.ReviewEvery, Source: c.Sources.ReviewEvery.String()},
	}
	if c.Smart {
		note := clamp("smart mode resolved this value at parse time")
		for _, s := range []*preflightSetting{&doc.Settings.CodexModel, &doc.Settings.Effort} {
			if s.Source == SourceFlag.String() {
				s.Note = note
			}
		}
	}
	// Execution semantics.
	once := preflightOnce{Enabled: c.Once,
		Note: "the run keeps waiting for new work when idle; use looper stop to finish"}
	if c.Once {
		once.Note = "the run exits after the current goal is checked or all remaining work is blocked"
	}
	timeout := 0
	if c.Timeout > 0 {
		timeout = int(c.Timeout.Seconds())
	}
	plans := []string{}
	for _, p := range c.Plans {
		if len(plans) < 8 {
			plans = append(plans, clamp(p))
		}
	}
	doc.Execution = preflightExecution{
		Mode:                 "foreground",
		ModeNote:             clamp("looper start runs the same configuration detached through the daemon; looper tui runs it under the dashboard"),
		Once:                 once,
		DispatchLimit:        c.Limit,
		PollSeconds:          int(c.Poll.Seconds()),
		WorkerTimeoutSeconds: timeout,
		GitTimeoutSeconds:    int(c.GitTimeout.Seconds()),
		Retry:                preflightRetry{Limit: c.RetryLimit, DelaySeconds: int(c.RetryDelay.Seconds())},
		Plans:                plans, PlanTotal: len(c.Plans),
	}
	// Verification and setup, with the saved run's contributions.
	doc.Commands = preflightCommands{
		Verify: preflightCommandOf(c, c.Verify, c.Sources.Verify, saved.state, true, clamp),
		Setup:  preflightCommandOf(c, c.Setup, c.Sources.Setup, saved.state, false, clamp),
	}
	// Environment overrides, by name only.
	doc.Environment = []preflightEnvVar{}
	for _, e := range preflightEnvironmentNames {
		_, present := os.LookupEnv(e.name)
		doc.Environment = append(doc.Environment, preflightEnvVar{Name: e.name, Present: present, Affects: e.affects})
	}
	doc.Findings = preflightFindings(c, doc, saved, fit)
	doc.Unchecked = preflightUncheckedServices(doc)
	doc.Limits = preflightLimits{BytesBudget: preflightBytesBudget, StringMax: fit.stringMax, FindingsMax: fit.findingsMax}
	doc.Detail = preflightDetail{
		Settings:     "looper config",
		Health:       "looper doctor",
		Capabilities: "looper version --contract v2 --json",
	}
	return doc
}

// agentSourceLabel names where the effective agent came from.
func agentSourceLabel(from Source, derived bool) string {
	if derived {
		return "derived"
	}
	return from.String()
}

// savedCommandLayers reads the saved-run layers of one project command: the
// value a recovery overrode from, the recovered command, and the saved plan's
// value. A nil state — no saved run, or one that cannot be read — supplies
// none of them.
func savedCommandLayers(s *State, verify bool) (before, recovered, planValue string) {
	if s == nil {
		return "", "", ""
	}
	// A saved run without a plan rebuilt it at its next planning pass; until
	// then only the other layers can supply the command.
	if s.Plan != nil {
		if verify {
			planValue = s.Plan.Verify
		} else {
			planValue = s.Plan.Setup
		}
	}
	if r := s.RecoveredCommands; r != nil {
		if verify {
			before, recovered = r.BeforeVerify, r.Verify
		} else {
			before, recovered = r.BeforeSetup, r.Setup
		}
	}
	return before, recovered, planValue
}

// preflightCommandOf resolves one project command through the layers that can
// supply it, mirroring Engine.capture: the explicit configuration wins, a
// recovered command replaces a matching previous value, the saved plan keeps
// a planner's choice, and otherwise the planner decides at first run.
func preflightCommandOf(c Config, configured string, from Source, s *State, verify bool, clamp func(string) string) preflightCommand {
	before, saved, planValue := savedCommandLayers(s, verify)
	effective, source, note := effectiveCommand(configured, from.String(), before, saved, planValue, verify)
	out := preflightCommand{Value: clamp(effective), Source: source}
	if note != "" {
		out.Note = clamp(note)
	}
	if effective != "" {
		out.Executable = probeCommand(effective, clamp)
	} else if verify && source == "planner" {
		// A detected candidate is a hint from project files, not a decision:
		// the planner still chooses at first run.
		if detection := DetectVerifyCommand(c.Root); detection.Command != "" {
			out.Note = clamp(fmt.Sprintf("the planning agent chooses this command at first run; detected candidate %q from %s", detection.Command, detection.Source))
		}
	}
	return out
}

// effectiveCommand applies the command precedence one run applies, mirroring
// Engine.capture and Engine.verify: a recovered command replaces a configured
// value that still equals the value it recovered from (the recovery overrode
// exactly that value, and recovery can clear it, which hands the command back
// to the plan), an untouched explicit value wins, the saved plan supplies the
// planner's choice, and otherwise the planner decides at first run. A
// recovered command whose previous value differs from the current
// configuration no longer applies. The planner note differs by command: an
// empty verify needs a decision, an empty setup is a valid choice.
func effectiveCommand(configured, source, before, recovered, planValue string, verify bool) (string, string, string) {
	const recoveredNote = "the saved run recovered this command after a failure; it applies until the configuration changes"
	if configured == before && (configured != "" || recovered != "") {
		if recovered != "" {
			return recovered, "recovered", recoveredNote
		}
		// Recovery cleared a value the configuration still names; the plan or
		// the planner supplies the command, exactly as capture hands the
		// emptied value through.
		configured = ""
	}
	if configured != "" {
		return configured, source, ""
	}
	if planValue != "" {
		return planValue, "plan", "the saved execution plan runs this command until the plan is rebuilt"
	}
	if verify {
		return "", "planner", "the planning agent chooses this command at first run"
	}
	return "", "planner", "no setup command is configured; the planning agent may propose one at first run, and an empty setup is valid"
}

// preflightFindings reports the conditions that change what a start can do.
// Missing agent binaries and missing core tools are errors, like doctor;
// missing command executables and an unreadable saved run are warnings,
// because the run still starts and the failure surfaces in checks.
func preflightFindings(c Config, doc *preflightDocument, saved preflightSavedRun, fit preflightFit) []preflightFinding {
	clamp := preflightClampOf(fit)
	findings := []preflightFinding{}
	add := func(f preflightFinding) {
		if len(findings) < fit.findingsMax {
			findings = append(findings, f)
		}
	}
	hint := map[string]string{
		"git":    "install Git and make sure git is on PATH",
		"bash":   "install Bash or point LOOPER_* commands at another shell",
		"codex":  "install codex or set CODEX_BIN",
		"claude": "install claude or set CLAUDE_BIN",
		"zcode":  "install zcode or set ZCODE_BIN",
	}
	for _, tool := range []struct {
		probe *preflightExecutable
		role  string
	}{
		{&doc.Agents.Coding.Binary, "coding agent"},
		{&doc.Agents.Review.Binary, "review agent"},
	} {
		if tool.probe.State == "missing" {
			// The message embeds the requested value the document already
			// clamped, so the finding stays bounded with it.
			add(preflightFinding{Code: "executable_missing", Severity: "error",
				Message: clamp(fmt.Sprintf("the %s binary %q was not found on PATH", tool.role, tool.probe.Requested)),
				Action:  clamp(orDefault(hint[tool.probe.Tool], "run looper config to repair the agent command"))})
		}
	}
	for _, name := range []string{"git", "bash"} {
		probe := probeBinary(name, name, "", clamp)
		if probe.State == "missing" {
			add(preflightFinding{Code: "executable_missing", Severity: "error",
				Message: clamp(fmt.Sprintf("the %s executable was not found on PATH", name)),
				Action:  clamp(hint[name])})
		}
	}
	for _, cmd := range []struct {
		label   string
		command preflightCommand
	}{
		{"verify", doc.Commands.Verify},
		{"setup", doc.Commands.Setup},
	} {
		if e := cmd.command.Executable; e != nil && e.State == "missing" {
			add(preflightFinding{Code: "command_executable_missing", Severity: "warning",
				Message: clamp(fmt.Sprintf("the %s command's executable %q was not found on PATH", cmd.label, e.Requested)),
				Action:  clamp("install the executable or change the " + cmd.label + " command")})
		}
	}
	for _, plan := range c.Plans {
		if _, err := os.Stat(plan); err != nil {
			add(preflightFinding{Code: "plan_unreadable", Severity: "warning",
				Message: clamp(fmt.Sprintf("the startup plan %s cannot be read", plan)),
				Action:  "repair the path; a start rejects unreadable plans before any work begins"})
		}
	}
	if saved.unreadable != nil {
		add(preflightFinding{Code: "state_unreadable", Severity: "warning",
			Message: clamp(fmt.Sprintf("the saved run cannot be read: %v", saved.unreadable)),
			Action:  "the recovered-command overrides and the saved plan stay unknown; read the run with looper doctor"})
	}
	if repositoryOwnedNoCreate(c) {
		add(preflightFinding{Code: "controller_owns_repository", Severity: "warning",
			Message: "a controller owns this repository; a new start cannot acquire it",
			Action:  "looper stop"})
	}
	return findings
}

// preflightUncheckedServices names what this report deliberately did not
// verify. A preflight never starts an agent, so provider-side state stays
// unknown; the report says so instead of implying a check that never ran.
func preflightUncheckedServices(doc *preflightDocument) []preflightUnchecked {
	unchecked := []preflightUnchecked{
		{Code: "agent_authentication",
			Note: "no credential, login, quota, or billing state was probed; the first real launch decides"},
	}
	providerDefault := false
	for _, s := range []preflightSetting{doc.Settings.CodexModel, doc.Settings.ClaudeModel, doc.Settings.Effort} {
		if s.Resolution == "provider_default" {
			providerDefault = true
		}
	}
	if providerDefault {
		unchecked = append(unchecked, preflightUnchecked{Code: "provider_default_models",
			Note: "unset model and effort fields are chosen by the agent binary at launch; Looper does not resolve the provider default"})
	}
	if doc.Commands.Verify.Source == "planner" {
		unchecked = append(unchecked, preflightUnchecked{Code: "verify_command",
			Note: "the verify command is chosen by the planning agent at first run; a detected candidate is a hint, not a decision"})
	}
	return unchecked
}

// preflightBlocking reports whether any finding blocks a start.
func preflightBlocking(findings []preflightFinding) bool {
	for _, f := range findings {
		if f.Severity == "error" {
			return true
		}
	}
	return false
}

// runPreflight serves the read-only execution preview (AX-023). It resolves
// the configuration a controller start would use, creates no controller,
// starts no agent, and changes no saved state; the exit code reports whether
// a finding blocks a start, like doctor.
func runPreflight(c Config, out, stderr io.Writer) int {
	if c.JSON && c.Contract != ContractVersionV2 {
		p := incompatibleFailure("contract_version_required", "The preflight document is a contract version 2 document.")
		p.Expected = map[string]any{"contract": "v2"}
		p.Observed = map[string]any{"contract": "legacy", "option": "--json"}
		p.Actions = []ErrorAction{{
			Description: "Request the preflight document with the version 2 contract.",
			Command:     "looper preflight --contract v2 --json",
		}}
		return emitError(c, p, out, stderr)
	}
	doc := buildPreflight(c)
	code := 0
	if preflightBlocking(doc.Findings) {
		code = 1
	}
	if c.JSON {
		if err := json.NewEncoder(out).Encode(doc); err != nil {
			return 1
		}
		return code
	}
	renderPreflightPlain(doc, out)
	return code
}

// renderPreflightPlain renders the same document as text: the resolved
// configuration, the findings, and the services that stayed unchecked.
func renderPreflightPlain(doc *preflightDocument, out io.Writer) {
	line := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
	line("Preflight (read-only; no controller was started, no agent was launched, nothing was changed)")
	line("Project: %s | task file %s | runtime %s", doc.Project.Root, doc.Project.TodoPath, doc.Project.RuntimeDir)
	line("Profile: %s (%s)", doc.Project.Profile.Path, map[bool]string{true: "present", false: "absent"}[doc.Project.Profile.Present])
	for _, a := range []struct {
		label string
		agent preflightAgent
	}{
		{"Coding agent", doc.Agents.Coding}, {"Review agent", doc.Agents.Review},
	} {
		derived := ""
		if a.agent.FollowsCoding {
			derived = " (follows the coding agent)"
		}
		line("%s: %s [%s]%s | %s", a.label, a.agent.Name, a.agent.Source, derived, executableLine(a.agent.Binary))
		line("  sandbox: %s; %s", a.agent.Sandbox, a.agent.SandboxNote)
	}
	setting := func(name string, s preflightSetting) string {
		value := s.Value
		switch s.Resolution {
		case "provider_default":
			value = "unset -> provider default"
		case "not_applicable":
			value = "unset (unused)"
		}
		return fmt.Sprintf("%s %s [%s]", name, value, s.Source)
	}
	line("Models: %s | %s | %s", setting("codex", doc.Settings.CodexModel), setting("claude", doc.Settings.ClaudeModel), setting("effort", doc.Settings.Effort))
	jobs := fmt.Sprintf("%d", doc.Settings.Jobs.Value)
	if doc.Settings.Jobs.Auto {
		jobs = fmt.Sprintf("%d (auto)", doc.Settings.Jobs.Value)
	}
	line("Sessions: %s [%s] | concurrent checks %d | review every %d [%s]", jobs, doc.Settings.Jobs.Source,
		doc.Settings.Checks.Value, doc.Settings.ReviewEvery.Value, doc.Settings.ReviewEvery.Source)
	line("Launch: %s | once %s | %s", doc.Execution.Mode, yesNo(doc.Execution.Once.Enabled), doc.Execution.Once.Note)
	line("  %s", doc.Execution.ModeNote)
	timeout := "none"
	if doc.Execution.WorkerTimeoutSeconds > 0 {
		timeout = fmt.Sprintf("%ds", doc.Execution.WorkerTimeoutSeconds)
	}
	limit := "unlimited"
	if doc.Execution.DispatchLimit > 0 {
		limit = fmt.Sprintf("%d", doc.Execution.DispatchLimit)
	}
	line("Timing: poll %ds | worker timeout %s | retry %d at %ds | git timeout %ds | dispatch limit %s",
		doc.Execution.PollSeconds, timeout, doc.Execution.Retry.Limit, doc.Execution.Retry.DelaySeconds,
		doc.Execution.GitTimeoutSeconds, limit)
	if doc.Execution.PlanTotal > 0 {
		line("Startup plans: %s", strings.Join(doc.Execution.Plans, ", "))
	}
	for _, cmd := range []struct {
		label   string
		command preflightCommand
	}{
		{"Verify", doc.Commands.Verify}, {"Setup", doc.Commands.Setup},
	} {
		value := cmd.command.Value
		if value == "" {
			value = "(unset)"
		}
		line("%s: %s [%s]", cmd.label, value, cmd.command.Source)
		if cmd.command.Note != "" {
			line("  %s", cmd.command.Note)
		}
		if e := cmd.command.Executable; e != nil {
			line("  executable: %s", executableLine(*e))
		}
	}
	var present []string
	for _, e := range doc.Environment {
		if e.Present {
			present = append(present, e.Name)
		}
	}
	if len(present) > 0 {
		line("Environment overrides present (values are never shown): %s", strings.Join(present, ", "))
	}
	if len(doc.Findings) > 0 {
		line("Findings:")
		for _, f := range doc.Findings {
			if f.Action != "" {
				line("  %s %s: %s | fix: %s", f.Severity, f.Code, f.Message, f.Action)
			} else {
				line("  %s %s: %s", f.Severity, f.Code, f.Message)
			}
		}
	} else {
		line("Findings: none")
	}
	line("Unchecked:")
	for _, u := range doc.Unchecked {
		line("  %s: %s", u.Code, u.Note)
	}
	line("Detail: %s | %s | %s", doc.Detail.Settings, doc.Detail.Health, doc.Detail.Capabilities)
}

// executableLine renders one executable probe for plain output.
func executableLine(e preflightExecutable) string {
	switch e.State {
	case "found":
		return fmt.Sprintf("%s -> %s [%s]", e.Requested, e.Resolved, e.Source)
	case "missing":
		return fmt.Sprintf("%s -> MISSING [%s]", e.Requested, e.Source)
	default:
		return fmt.Sprintf("%s -> %s [%s]", e.Requested, e.State, e.Source)
	}
}

// launchStringMax bounds one configured value the launch line embeds, in the
// bytes the JSON encoder emits. The line is emitted before any budgeted
// document exists, so its own bound is what keeps an oversized or
// escape-heavy configured value — a PATH entry, a command, a model — from
// growing the plain line and the launch event without limit.
const launchStringMax = 256

// launchDescription is the concise, safe description of the launch a
// foreground run is about to perform, emitted before any controller or agent
// starts. It carries resolved identities and sources only: credentials,
// environment values, and raw agent arguments stay out. Every configured
// value it names is scrubbed and clamped by its emitted bytes, so the plain
// line and the launch event that carries it stay bounded whatever the
// configuration holds.
func launchDescription(c Config) (string, map[string]any) {
	clamp := func(s string) string { return clampJSONString(scrubSecrets(s), launchStringMax) }
	bin := agentBinaryOf(c, c.Agent)
	resolved := ""
	if path, err := exec.LookPath(bin); err == nil {
		resolved = clamp(path)
	} else {
		resolved = clamp("(" + bin + " not found on PATH)")
	}
	modelOf := func() string {
		if p, err := c.effectiveAgent(c.Agent); err == nil && p.Model != "" {
			return p.Model
		}
		switch c.adapterOf(c.Agent) {
		case "codex":
			return orDefault(c.Model, "provider default")
		case "claude":
			return orDefault(c.ClaudeModel, "provider default")
		default:
			return "agent-configured"
		}
	}
	// The line prints before NewEngine, so it reads the saved run itself —
	// read-only, like preflight — and resolves the project commands through
	// the same precedence the engine applies on a resume: the value a
	// recovery overrode from and the recovered command, then the saved plan.
	// A saved run that cannot be read supplies no layer, exactly as the
	// preflight document reports for the same state.
	_, dir := projectPaths(c)
	saved := readSavedRun(dir)
	beforeVerify, recoveredVerify, planVerify := savedCommandLayers(saved.state, true)
	verify, verifySource, _ := effectiveCommand(c.Verify, c.Sources.Verify.String(), beforeVerify, recoveredVerify, planVerify, true)
	if verify == "" {
		verify = "planner-chosen"
	}
	beforeSetup, recoveredSetup, planSetup := savedCommandLayers(saved.state, false)
	setup, setupSource, _ := effectiveCommand(c.Setup, c.Sources.Setup.String(), beforeSetup, recoveredSetup, planSetup, false)
	if setup == "" {
		setup = "none"
		setupSource = "none"
	}
	verify, setup = clamp(verify), clamp(setup)
	verifySource, setupSource = clamp(verifySource), clamp(setupSource)
	agent, review, model, effort := clamp(c.Agent), clamp(c.ReviewAgent), clamp(modelOf()), clamp(orDefault(c.Effort, "provider default"))
	line := scrubSecrets(fmt.Sprintf("Launch: agent %s (%s) | review %s | model %s | effort %s | jobs %d | checks %d | verify %s [%s] | setup %s [%s] | once %s | %s",
		agent, resolved, review, model, effort,
		c.Jobs, c.Checks, verify, verifySource, setup, setupSource, yesNo(c.Once), "foreground"))
	data := map[string]any{
		"mode":          "foreground",
		"once":          c.Once,
		"coding_agent":  agent,
		"review_agent":  review,
		"binary":        resolved,
		"model":         model,
		"effort":        effort,
		"jobs":          c.Jobs,
		"checks":        c.Checks,
		"verify":        verify,
		"verify_source": verifySource,
		"setup":         setup,
		"setup_source":  setupSource,
	}
	return line, data
}

// emitLaunchDescription reports the launch before foreground execution
// starts. Plain output prints the concise line; version 2 JSON output gets
// the same facts as one NDJSON launch event, and the legacy JSON stream keeps
// its byte-compatible shape, as docs/AGENT-CONTRACT-MIGRATION.md records.
func emitLaunchDescription(c Config, out io.Writer) {
	line, data := launchDescription(c)
	if c.JSON {
		if c.Contract != ContractVersionV2 {
			return
		}
		payload, err := json.Marshal(data)
		if err != nil {
			return
		}
		b, err := json.Marshal(Event{Time: time.Now().UTC().Format(time.RFC3339), Kind: "launch", Message: line, Data: payload})
		if err != nil {
			return
		}
		fmt.Fprintln(out, string(b))
		return
	}
	fmt.Fprintln(out, line)
}
