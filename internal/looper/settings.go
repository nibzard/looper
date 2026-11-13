package looper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// controllerStart reports whether a command starts a new controller. The
// first-run form runs only for these commands.
func controllerStart(command string) bool {
	switch command {
	case "run", "start", "tui":
		return true
	}
	return false
}

// FormDecision explains whether the first-run form may run.
type FormDecision struct {
	Ask    bool
	Reason string
}

// firstRunDecision applies the gating conditions in plan order: a controller
// start, no quiet mode, no profile and no owner, no setting flags or
// environment variables, and finally interactive terminal streams.
func firstRunDecision(c Config, in *os.File, out io.Writer) FormDecision {
	reject := func(reason string) FormDecision { return FormDecision{false, reason} }
	if !controllerStart(c.Command) {
		return reject("command does not start a controller")
	}
	if c.JSON {
		return reject("--json suppresses forms")
	}
	if c.Plain {
		return reject("--plain suppresses forms")
	}
	if c.Yes {
		return reject("--yes skips the first-run form")
	}
	if env("LOOPER_NO_PROMPT", "") == "1" {
		return reject("LOOPER_NO_PROMPT=1")
	}
	if _, err := os.Stat(c.ProfilePath); err == nil {
		return reject("a saved profile already exists")
	}
	if repositoryOwned(c) {
		return reject("a controller already owns this repository")
	}
	if !isTTY(in) || !writerTTY(out) {
		return reject("input and output must be terminals")
	}
	if !terminalInteractive() {
		return reject("the terminal type does not support interaction")
	}
	return FormDecision{true, ""}
}

// defaultAgentNotice names the agent on the one path where the operator never
// chose one: a non-interactive controller start with no saved profile.
func defaultAgentNotice(c Config, out io.Writer) {
	if c.JSON || c.Sources.Agent != SourceDefault {
		return
	}
	if _, err := os.Stat(c.ProfilePath); err == nil {
		return
	}
	fmt.Fprintf(out, "No saved profile: the coding agent defaults to %s. Run 'looper config' to change it.\n", c.Agent)
}

// repositoryOwned probes the repository lock without holding it.
func repositoryOwned(c Config) bool {
	common, err := gitText(c.Root, "rev-parse", "--git-common-dir")
	if err != nil {
		return false
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(c.Root, common)
	}
	return ownsRepository(common)
}

// maybeFirstRun asks the startup form when eligible, saves the answers, and
// re-resolves the configuration. Cancellation exits without starting work or
// saving answers. A profile created while the form was open is never replaced;
// answers are not saved when a controller took ownership in the meantime.
func maybeFirstRun(args []string, c Config, in *os.File, out, stderr io.Writer) (Config, bool, error) {
	decision := firstRunDecision(c, in, out)
	if !decision.Ask {
		if err := requireAgentConfiguration(c); err != nil {
			return c, false, err
		}
		return c, true, nil
	}
	s := ui.Settings{Agent: c.Agent, ReviewAgent: c.ReviewAgent, Jobs: c.Jobs, ReviewEvery: c.ReviewEvery, Model: c.Model, ClaudeModel: c.ClaudeModel, Effort: c.Effort}
	if c.Sources.ReviewAgent == SourceDefault {
		s.ReviewAgent = ""
	}
	if c.JobsAuto {
		s.Jobs = 0
	}
	detection := DetectVerifyCommand(c.Root)
	s.VerifyCommand, s.SetupCommand = c.Verify, c.Setup
	if s.VerifyCommand == "" && c.Sources.Verify < SourceEnv {
		s.VerifyCommand = detection.Command
	}
	catalog, err := configureAgentCommands(c, &s)
	if err == nil {
		err = ui.RunFirstRunForm(&s)
	}
	if err == nil {
		err = confirmAgentConfiguration(c, s, catalog)
	}
	if err != nil {
		if errors.Is(err, ui.ErrAborted) {
			return c, false, nil
		}
		return c, false, err
	}
	// Recheck ownership after the form: a controller that started meanwhile
	// keeps its own effective settings, so the answers are not saved.
	if repositoryOwned(c) {
		fmt.Fprintln(out, "A controller started while the form was open; the answers were not saved.")
		fmt.Fprintln(out, "Another Looper controller owns this repository.")
		return c, false, nil
	}
	if err := saveAgentCatalog(c.AgentCatalogPath, catalog); err != nil {
		return c, false, err
	}
	profile := ProfileFromSettings(s)
	created, err := saveProfileExclusive(c.ProfilePath, profile)
	if err != nil {
		return c, false, err
	}
	if !created {
		fmt.Fprintln(out, "A profile was created while the form was open; using it.")
	} else if !c.JSON {
		fmt.Fprintln(out, "Saved agent commands to", c.AgentCatalogPath)
		fmt.Fprintln(out, "Saved settings to", c.ProfilePath)
		noticeVerifyCommand(out, profile.VerifyCommand, detection, false)
	}
	// Re-resolve: the saved profile now supplies the settings for this run.
	merged, err := Parse(args)
	if err != nil {
		return c, false, err
	}
	return merged, true, nil
}

// noticeVerifyCommand reports the selected verify command and its source.
// An empty command keeps planner selection, which still requires executable
// verification before work proceeds.
func noticeVerifyCommand(out io.Writer, command string, detection Detection, manual bool) {
	switch {
	case command == "":
		fmt.Fprintln(out, "Verify command: chosen by the planner at first run")
	case manual:
		fmt.Fprintf(out, "Verify command: %s (edited)\n", command)
	default:
		fmt.Fprintf(out, "Verify command: %s (detected from %s)\n", command, detection.Source)
	}
}

// ProfileFromSettings converts form output into a saved profile.
func ProfileFromSettings(s ui.Settings) *Profile {
	return &Profile{
		Version: ProfileVersion, Agent: s.Agent, ReviewAgent: s.ReviewAgent,
		Jobs: s.Jobs, Model: s.Model, ClaudeModel: s.ClaudeModel, Effort: s.Effort,
		VerifyCommand: s.VerifyCommand, SetupCommand: s.SetupCommand,
		ReviewEvery: &s.ReviewEvery,
	}
}

// settingsFromProfile fills a form from a saved profile, applying defaults
// for absent fields. The absent agent field uses defaultAgent, so a fresh
// form never pins a different agent than a defaulted run.
func settingsFromProfile(p *Profile) ui.Settings {
	s := ui.Settings{Agent: defaultAgent, ReviewEvery: 5}
	if p == nil {
		return s
	}
	s.Agent = orDefault(p.Agent, defaultAgent)
	s.ReviewAgent, s.Jobs = p.ReviewAgent, p.Jobs
	s.Model, s.ClaudeModel, s.Effort = p.Model, p.ClaudeModel, p.Effort
	s.VerifyCommand, s.SetupCommand = p.VerifyCommand, p.SetupCommand
	if p.ReviewEvery != nil {
		s.ReviewEvery = *p.ReviewEvery
	}
	return s
}

// runConfig edits or resets the saved profile. Without a controller it saves
// changes under the repository lock and releases the lock without starting
// the engine. A controller-owned repository answers through the controller
// request protocol; no editor writes the profile directly while a controller
// owns the repository.
func runConfig(c Config, in *os.File, out, stderr io.Writer) int {
	if c.TestAgent != "" {
		return testAgentProfile(c, out, stderr)
	}
	if repositoryOwned(c) {
		return runConfigOnline(c, in, out, stderr)
	}
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	repo, err := OpenRepository(c, dir)
	if err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	lock, err := lockRepository(repo.Common)
	if err != nil {
		p := operationFailure("repository_busy", err.Error())
		p.Retry = retryYes
		p.Actions = []ErrorAction{{Description: "Wait for the holder to release the repository, then retry."}}
		return emitError(c, p, out, stderr)
	}
	defer lock.Close()
	if c.Reset {
		if _, err := os.Stat(c.ProfilePath); err != nil {
			p := operationFailure("profile_absent", "No saved profile exists.")
			p.Retry = retryNo
			p.Observed = map[string]any{"saved_profile": false}
			p.Paths = []string{c.ProfilePath}
			p.withLegacy("No saved profile exists.", 1)
			return emitError(c, p, out, stderr)
		}
		if err := os.Remove(c.ProfilePath); err != nil {
			return emitError(c, persistenceError(err.Error()), out, stderr)
		}
		fmt.Fprintln(out, "Removed", c.ProfilePath)
		fmt.Fprintln(out, "Effective settings stay active until the controller restarts.")
		return 0
	}
	profile, err := LoadProfile(c.ProfilePath)
	if err != nil {
		return emitError(c, profileUnreadableError(err, c.ProfilePath), out, stderr)
	}
	if !isTTY(in) || !writerTTY(out) || !terminalInteractive() {
		p := operationFailure("terminal_unavailable", "looper config needs an interactive terminal; --reset works without one.")
		p.Actions = []ErrorAction{{Description: "Run the editor from an interactive terminal, or reset non-interactively.", Command: "looper config --reset"}}
		p.withLegacy("looper config needs an interactive terminal; --reset works without one.", 1)
		return emitError(c, p, out, stderr)
	}
	if !c.JSON {
		fmt.Fprintln(out, "Effective settings:", EffectiveSettings(c))
	}
	s := settingsFromProfile(profile)
	if profile == nil || profile.Agent == "" {
		s.Agent = c.Agent
	}
	if profile == nil && c.Sources.ReviewAgent == SourceMachine {
		s.ReviewAgent = c.ReviewAgent
	}
	detection := DetectVerifyCommand(c.Root)
	// Detection pre-fills the candidate when nothing explicit exists, so
	// clearing the field keeps planner selection instead of re-baking it.
	edited := s.VerifyCommand != ""
	if !edited {
		s.VerifyCommand = detection.Command
	}
	catalog, err := configureAgentCommands(c, &s)
	if err == nil {
		err = ui.RunEditorForm(&s)
	}
	if err == nil {
		err = confirmAgentConfiguration(c, s, catalog)
	}
	if err != nil {
		if errors.Is(err, ui.ErrAborted) {
			fmt.Fprintln(out, "Cancelled. Nothing was saved.")
			return 0
		}
		return emitError(c, err, out, stderr)
	}
	if err = saveAgentCatalog(c.AgentCatalogPath, catalog); err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	saved := ProfileFromSettings(s)
	if profile != nil {
		saved.Unknown = profile.Unknown
	}
	if err = SaveProfile(c.ProfilePath, saved); err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	if !c.JSON {
		fmt.Fprintln(out, "Saved agent commands to", c.AgentCatalogPath)
		fmt.Fprintln(out, "Saved settings to", c.ProfilePath)
		noticeVerifyCommand(out, saved.VerifyCommand, detection, edited || s.VerifyCommand != detection.Command)
	}
	return 0
}

// EffectiveSettings lists each managed setting with its effective value and
// where the value came from, so users can see what a run will use.
func EffectiveSettings(c Config) string {
	value := func(v string, from Source) string {
		if v == "" {
			switch from {
			case SourceFlag, SourceEnv, SourceProfile:
				return "(empty) [" + from.String() + "]"
			}
			return "(default)"
		}
		return v + " [" + from.String() + "]"
	}
	jobs := fmt.Sprintf("%d", c.Jobs)
	if c.JobsAuto {
		jobs = "auto [" + c.Sources.Jobs.String() + "]"
	} else {
		jobs += " [" + c.Sources.Jobs.String() + "]"
	}
	return fmt.Sprintf("agent %s | review %s | sessions %s | codex model %s | claude model %s | effort %s | verify %s | setup %s | review frequency %d [%s]",
		value(c.Agent, c.Sources.Agent), value(c.ReviewAgent, c.Sources.ReviewAgent), jobs,
		value(c.Model, c.Sources.Model), value(c.ClaudeModel, c.Sources.ClaudeModel), value(c.Effort, c.Sources.Effort),
		value(c.Verify, c.Sources.Verify), value(c.Setup, c.Sources.Setup), c.ReviewEvery, c.Sources.ReviewEvery.String())
}

// SettingsRequestVersion is the only supported request-envelope version.
const SettingsRequestVersion = 1

// Request types share one transport. Settings requests live for one
// controller session; plan-import requests carry durable receipts instead.
const (
	RequestTypeSettings   = "settings"
	RequestTypePlanImport = "plan-import"
	RequestTypeRequeue    = "requeue"
	RequestTypeCancel     = "cancel"
	RequestTypeAmend      = "amend"
	RequestTypeRecovery   = "recovery"
)

// SettingsRequest is a typed controller request written atomically to
// requests/<id>.json. Fields are assignment patches, never increments.
type SettingsRequest struct {
	Tasks         []Task                     `json:"tasks,omitempty"`
	Version       int                        `json:"version"`
	ID            string                     `json:"id"`
	Type          string                     `json:"type"`
	Session       string                     `json:"session"`
	ProfileDigest string                     `json:"profile_digest,omitempty"`
	Fields        map[string]json.RawMessage `json:"fields,omitempty"`
	Reset         bool                       `json:"reset,omitempty"`
	// Paths carries plan-import payloads on the same typed transport.
	Paths        []string `json:"paths,omitempty"`
	CreatedAt    string   `json:"created_at,omitempty"`
	TaskID       string   `json:"task_id,omitempty"`
	TaskRevision string   `json:"task_revision,omitempty"`
	// Cascade marks a cancellation that takes its live dependents along
	// (AX-019); without it the controller refuses a task with live
	// dependents, naming them.
	Cascade bool `json:"cascade,omitempty"`
	// RecoveryChoice names the recovery choice one checked application
	// applies, and RecoveryToken binds it to the preview that enumerated the
	// choice (AX-017). The controller re-derives the token and refuses a
	// stale preview without mutation.
	RecoveryChoice string `json:"recovery_choice,omitempty"`
	RecoveryToken  string `json:"recovery_token,omitempty"`
	// SubmitterPID and SubmitterIdentity bind a session-scoped request to the
	// process waiting for its acknowledgement. The startup sweep uses the
	// same identity check controller liveness uses to sweep only requests
	// whose submitter is gone; a live submitter's request survives a
	// controller restart so the new controller answers it instead of
	// silently deleting it.
	SubmitterPID      int    `json:"submitter_pid,omitempty"`
	SubmitterIdentity string `json:"submitter_identity,omitempty"`
	// IdempotencyKey is the caller key one submission carries, when it carries
	// one (AX-012). Scoped to the project and the request type: the same key
	// and payload returns the original request, and a changed payload is a
	// conflict.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// SettingsAck reports a processed request. Clients read their own
// acknowledgement instead of treating any settings event as success.
type SettingsAck struct {
	Version       int               `json:"version"`
	ID            string            `json:"id"`
	Status        string            `json:"status"`
	Session       string            `json:"controller_session"`
	ProfileDigest string            `json:"profile_digest,omitempty"`
	Outcomes      map[string]string `json:"outcomes,omitempty"`
	Message       string            `json:"message,omitempty"`
}

// Ack statuses. Waiting is the one non-terminal acknowledgement: a decided
// request that waits for a named prerequisite (AX-019 cancellations waiting
// for active work) answers with it while the durable receipt carries the
// waiting state and its reason.
const (
	AckApplied  = "applied"
	AckReset    = "reset"
	AckConflict = "conflict"
	AckError    = "error"
	AckStale    = "stale-session"
	AckWaiting  = "waiting"
)

var settingFieldKeys = map[string]bool{
	"agent": true, "review_agent": true, "jobs": true, "model": true,
	"claude_model": true, "effort": true, "verify_command": true,
	"setup_command": true, "review_every": true,
}

// maxPendingRequests bounds the request queues. sweepRequestQueue enforces
// it at every loop boundary by answering excess envelopes with an error
// acknowledgement, so a flooded queue recovers instead of wedging claims.
const maxPendingRequests = 100

// staleRequestEntryAge is how long a non-request entry may sit in the
// requests directory before the sweep removes it. Atomic writes stage a
// .looper-write-* temporary in the target directory, and a writer killed
// mid-write leaves it behind; a younger entry may still belong to a live
// writer, so the sweep leaves it for a later pass.
const staleRequestEntryAge = time.Minute

// staleAckAge is how long an acknowledgement from an earlier session stays
// after a restart before the startup sweep removes it. A submitter polls for
// its answer every 100 ms, but a controller restart can land between the
// acknowledgement write and that read; retention far beyond the bounded
// submit wait keeps a written answer readable while the sweep still collects
// old files. A variable so tests can age acknowledgements quickly.
var staleAckAge = 10 * time.Minute

func requestDir(dir string) string    { return filepath.Join(dir, "requests") }
func processingDir(dir string) string { return filepath.Join(dir, "processing") }
func ackDir(dir string) string        { return filepath.Join(dir, "acks") }

// loadSettingsRequest reads and validates one request envelope.
func loadSettingsRequest(path string) (*SettingsRequest, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > 64<<10 {
		return nil, fmt.Errorf("request %s exceeds 64 KiB", filepath.Base(path))
	}
	var req SettingsRequest
	if err = readJSON(path, &req); err != nil {
		return nil, fmt.Errorf("request %s is unreadable: %w", filepath.Base(path), err)
	}
	if req.Version != SettingsRequestVersion {
		return nil, fmt.Errorf("request %s has unsupported version %d", req.ID, req.Version)
	}
	if req.ID == "" || strings.Contains(req.ID, "/") || strings.Contains(req.ID, "..") {
		return nil, fmt.Errorf("request has an invalid identifier")
	}
	if req.IdempotencyKey != "" {
		if err := validateIdempotencyKey(req.IdempotencyKey); err != nil {
			return nil, fmt.Errorf("request %s carries an unusable idempotency key: %w", req.ID, err)
		}
	}
	if req.Type == "" {
		req.Type = RequestTypeSettings
	}
	return &req, nil
}

// requestRefusal names one envelope the queue refused without claiming it,
// with the parsed envelope when it could be read. The engine records a
// rejected receipt for each, so a refused request keeps a queryable outcome
// after its envelope and its acknowledgement are gone (AX-009).
type requestRefusal struct {
	id      string
	req     *SettingsRequest
	message string
}

// claimNextRequest renames the oldest request of the given type into
// processing/, claiming it. One request is claimed at a time. Entries that
// are not envelopes never block a claim here: only .json names are
// claimable, and sweepRequestQueue holds the envelope count at or below
// the pending bound before the claim processors run. Envelopes refused at
// claim time are returned beside the claim so their receipts outlive the
// removed envelope.
func claimNextRequest(dir, typ string) (*SettingsRequest, []requestRefusal, error) {
	var refusals []requestRefusal
	entries, err := os.ReadDir(requestDir(dir))
	if os.IsNotExist(err) {
		return nil, refusals, nil
	}
	if err != nil {
		return nil, refusals, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		req, err := loadSettingsRequest(filepath.Join(requestDir(dir), name))
		if err != nil {
			// Invalid envelopes receive an error ack without stopping. The
			// envelope stays when its acknowledgement cannot be written, so
			// the next boundary retries it instead of silently deleting the
			// request.
			id := strings.TrimSuffix(name, ".json")
			if ackErr := writeJSON(filepath.Join(ackDir(dir), id+".json"), SettingsAck{
				Version: SettingsRequestVersion, ID: id, Status: AckError, Message: err.Error(),
			}); ackErr != nil {
				return nil, refusals, fmt.Errorf("request %s stays queued: its error acknowledgement could not be written: %w", id, ackErr)
			}
			os.Remove(filepath.Join(requestDir(dir), name))
			refusals = append(refusals, requestRefusal{id: id, message: err.Error()})
			continue
		}
		// Claim-time capability validation: an envelope of a type this
		// controller never claims would otherwise wait forever. It receives
		// the specific incompatibility acknowledgement instead of becoming a
		// dead letter, whatever wrote it.
		if !supportedEnvelopeTypes[req.Type] {
			ack := SettingsAck{
				Version: SettingsRequestVersion, ID: req.ID, Status: AckError,
				Message: fmt.Sprintf("unsupported request type %q; this controller accepts %s requests. No work was queued. Resubmit to a controller that supports it, per %s.",
					req.Type, strings.Join(controllerRequestTypes(), ", "), capabilitiesPolicy),
			}
			if ackErr := writeJSON(filepath.Join(ackDir(dir), req.ID+".json"), ack); ackErr != nil {
				return nil, refusals, fmt.Errorf("request %s stays queued: its error acknowledgement could not be written: %w", req.ID, ackErr)
			}
			os.Remove(filepath.Join(requestDir(dir), name))
			refusals = append(refusals, requestRefusal{id: req.ID, req: req, message: ack.Message})
			continue
		}
		if req.Type != typ {
			continue
		}
		if err = os.MkdirAll(processingDir(dir), 0700); err != nil {
			return nil, refusals, err
		}
		// Claim under the envelope identifier so the acknowledgement and the
		// removal always target the same file, whatever the source name was.
		if err = os.Rename(filepath.Join(requestDir(dir), name), filepath.Join(processingDir(dir), req.ID+".json")); err != nil {
			return nil, refusals, err
		}
		return req, refusals, nil
	}
	return nil, refusals, nil
}

// sweepRequestQueue recovers a request queue that would wedge the
// controller. Only claimable envelopes count toward maxPendingRequests:
// leftover write temporaries and other non-request entries never block a
// claim, and stale ones are removed. Envelopes beyond the bound are
// answered with an error acknowledgement and removed, newest in claim
// order first, so a flooded queue drops to the bound while the envelopes
// it keeps still claim. At most maxPendingRequests envelopes are shed per
// pass so one boundary stays responsive; over reports how many excess
// envelopes remain for the next pass. An envelope whose acknowledgement
// cannot be written stays put and is retried, while claims continue. Each
// shed envelope is returned as a refusal so its receipt outlives it; the
// acknowledgement speaks the shed envelope's own transport, so a plan
// import submitter reads its reason instead of an empty error.
func sweepRequestQueue(dir, session string) (shed []requestRefusal, cleaned, over int) {
	entries, err := os.ReadDir(requestDir(dir))
	if err != nil {
		return nil, 0, 0
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, ".json") {
			names = append(names, name)
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil || time.Since(info.ModTime()) < staleRequestEntryAge {
			// A young non-request entry may belong to a live writer.
			continue
		}
		if os.Remove(filepath.Join(requestDir(dir), name)) == nil {
			cleaned++
		}
	}
	sort.Strings(names)
	if len(names) <= maxPendingRequests {
		return nil, cleaned, 0
	}
	excess := names[maxPendingRequests:]
	over = len(excess)
	if len(excess) > maxPendingRequests {
		excess = excess[len(excess)-maxPendingRequests:]
	}
	shedMessage := fmt.Sprintf("The request queue held more than %d pending requests, so this request was removed without being read. Resubmit it.", maxPendingRequests)
	for _, name := range excess {
		id := strings.TrimSuffix(name, ".json")
		req, ackErr := writeShedAcknowledgement(dir, session, id, shedMessage)
		if ackErr != nil {
			// Keep the envelope; the next pass retries the acknowledgement.
			continue
		}
		if os.Remove(filepath.Join(requestDir(dir), name)) == nil {
			shed = append(shed, requestRefusal{id: id, req: req, message: shedMessage})
			over--
		}
	}
	return shed, cleaned, over
}

// writeShedAcknowledgement answers one shed envelope in its own transport.
// A plan-import envelope receives the plan-import acknowledgement shape
// whose error field carries the reason; every readable envelope keeps the
// settings acknowledgement shape. An unreadable envelope cannot name its
// transport, so it keeps the settings shape and its reason rides in the
// message field alone.
func writeShedAcknowledgement(dir, session, id, message string) (*SettingsRequest, error) {
	req, err := loadSettingsRequest(filepath.Join(requestDir(dir), id+".json"))
	if err == nil && req != nil && req.Type == RequestTypePlanImport {
		return req, writeJSON(filepath.Join(ackDir(dir), id+".json"), planImportAck{
			Version: SettingsRequestVersion, Type: RequestTypePlanImport, ID: id,
			Session: session, Status: ImportFailed, Error: message,
		})
	}
	return req, writeJSON(filepath.Join(ackDir(dir), id+".json"), SettingsAck{
		Version: SettingsRequestVersion, ID: id, Status: AckError, Message: message,
	})
}

// submitterAlive reports whether the process recorded as a request's
// submitter is still running. The check is the controller identity check:
// the recorded process start identity, when both sides have one, rules out a
// reused process ID, and an envelope that records only a process ID trusts
// that ID, exactly like a scoped stop request. An envelope that records no
// submitter has nothing to check and reports false.
func submitterAlive(req *SettingsRequest) bool {
	if req == nil || req.SubmitterPID <= 1 {
		return false
	}
	current, err := runIdentityCommand(req.SubmitterPID)
	if err != nil {
		return false
	}
	return req.SubmitterIdentity == "" || current == req.SubmitterIdentity
}

// submitterGone reports the only condition under which the startup sweep may
// drop a request envelope: the envelope records a submitter process and that
// process no longer runs with its recorded start identity. An envelope
// without a recorded submitter cannot be verified and stays, so the
// controller that starts now answers it instead of silently deleting it.
func submitterGone(req *SettingsRequest) bool {
	return req.SubmitterPID > 1 && !submitterAlive(req)
}

// clearSessionFiles removes settings requests and acknowledgements left by
// earlier controller sessions. Plan-import requests survive restarts with
// their durable receipts and are never removed here. A request is removed
// only when its recorded submitter is verifiably gone: a live submitter may
// still wait for an answer, so its envelope stays and this controller
// answers it with the stale-session acknowledgement at its first boundary. A
// claim the previous controller left in processing/ returns to requests/
// unless its acknowledgement already exists. Acknowledgements younger than
// staleAckAge stay, because a submitter may not have read its answer yet.
// After state recovery, recoverPlanImports completes claims left in
// processing/ and restores acks. Every removed envelope is returned as a
// refusal, so the starting controller can record a rejected receipt and the
// removal stays a queryable outcome instead of a silent deletion (AX-009).
func clearSessionFiles(dir string) []requestRefusal {
	var removed []requestRefusal
	for _, base := range []string{requestDir(dir), processingDir(dir)} {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(base, entry.Name())
			req, err := loadSettingsRequest(path)
			if err == nil && req.Type == RequestTypePlanImport {
				continue
			}
			if err == nil && !submitterGone(req) {
				// A live submitter, or one that cannot be verified, still
				// owns this envelope. A claim stranded in processing/ returns
				// to the queue so a boundary answers it here.
				if base == processingDir(dir) {
					if _, statErr := os.Stat(filepath.Join(ackDir(dir), req.ID+".json")); statErr == nil {
						// The previous controller answered this claim; the
						// retained acknowledgement is the answer.
						os.Remove(path)
						continue
					}
					if err = os.MkdirAll(requestDir(dir), 0700); err == nil {
						os.Rename(path, filepath.Join(requestDir(dir), req.ID+".json"))
					}
				}
				continue
			}
			if os.Remove(path) == nil {
				id := strings.TrimSuffix(entry.Name(), ".json")
				message := "the submitting process ended before a controller accepted this request; it was never applied"
				if err != nil {
					message = err.Error()
				}
				removed = append(removed, requestRefusal{id: id, req: req, message: message})
			}
		}
	}
	entries, err := os.ReadDir(ackDir(dir))
	if err != nil {
		return removed
	}
	for _, entry := range entries {
		// Digest replays and rejected imports may have no state receipt.
		// Keep their terminal answers so a pending submitter can finish.
		var ack planImportAck
		if readJSON(filepath.Join(ackDir(dir), entry.Name()), &ack) == nil && ack.Type == RequestTypePlanImport {
			continue
		}
		// An acknowledgement written moments ago may still be unread by a
		// live submitter; keep it until staleAckAge passes.
		if info, statErr := entry.Info(); statErr == nil && time.Since(info.ModTime()) < staleAckAge {
			continue
		}
		os.Remove(filepath.Join(ackDir(dir), entry.Name()))
	}
	return removed
}

// patchProfile applies assignment patches onto the current saved profile.
func patchProfile(current *Profile, fields map[string]json.RawMessage) (*Profile, error) {
	p := current
	if p == nil {
		p = &Profile{Version: ProfileVersion}
	}
	p = &Profile{
		Version: ProfileVersion, Agent: p.Agent, ReviewAgent: p.ReviewAgent, Jobs: p.Jobs,
		Model: p.Model, ClaudeModel: p.ClaudeModel, Effort: p.Effort,
		VerifyCommand: p.VerifyCommand, SetupCommand: p.SetupCommand,
		ReviewEvery: p.ReviewEvery, Unknown: p.Unknown,
	}
	stringField := func(key string, into *string) error {
		if v, ok := fields[key]; ok {
			if err := json.Unmarshal(v, into); err != nil {
				return fmt.Errorf("%s must be a string", key)
			}
		}
		return nil
	}
	for _, f := range []struct {
		key  string
		into *string
	}{
		{"agent", &p.Agent}, {"review_agent", &p.ReviewAgent}, {"model", &p.Model},
		{"claude_model", &p.ClaudeModel}, {"effort", &p.Effort},
		{"verify_command", &p.VerifyCommand}, {"setup_command", &p.SetupCommand},
	} {
		if err := stringField(f.key, f.into); err != nil {
			return nil, err
		}
	}
	if v, ok := fields["jobs"]; ok {
		if err := json.Unmarshal(v, &p.Jobs); err != nil {
			return nil, fmt.Errorf("jobs must be a nonnegative integer; use 0 for auto mode")
		}
	}
	if v, ok := fields["review_every"]; ok {
		var n int
		if err := json.Unmarshal(v, &n); err != nil {
			return nil, fmt.Errorf("review_every must be a nonnegative integer")
		}
		p.ReviewEvery = &n
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// autoJobs resolves auto mode for this machine.
func autoJobs() int {
	n := max(1, min(4, runtime.NumCPU()/2))
	if m := availableMemory(); m > 0 {
		n = min(n, max(1, int(m/(2<<30))))
	}
	return n
}

// applySettingsRequest validates and applies one claimed request. It runs on
// the owner goroutine at a loop boundary; synchronous planning and review are
// never interrupted to apply settings. Every decided request records a
// durable receipt beside its acknowledgement (AX-009); a controller that
// restarts between the profile write and the acknowledgement write replays
// the recorded answer instead of applying the patch twice.
func (e *Engine) applySettingsRequest(req *SettingsRequest) SettingsAck {
	base := SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controllerSessionID()}
	if ack, ok := e.acks[req.ID]; ok {
		// Duplicate identifiers return the recorded acknowledgement.
		return ack
	}
	if replayed, ok := e.replayedAck(req); ok {
		// A durable receipt already decided this identifier.
		e.rememberAck(req.ID, replayed)
		return replayed
	}
	defer func() {
		e.rememberAck(req.ID, base)
		e.recordSettingsRequest(req, base)
		detail := base.Status
		if base.Message != "" {
			detail += ": " + base.Message
		}
		e.requestEvent("settings", fmt.Sprintf("Settings request %s %s", req.ID, detail), req.ID)
	}()
	// A caller key answers from the receipt that recorded it, before any
	// session check: a retry after a controller restart targets a new session
	// and must still return the original outcome, never a second mutation
	// (AX-012). The replay records its own terminal mirror, which the deferred
	// receipt writer then leaves untouched.
	if req.IdempotencyKey != "" {
		if _, original, conflict := e.resolveIdempotencyKey(req, nil); original != nil {
			base = e.replayIdempotentAck(req, original)
			return base
		} else if conflict != nil {
			base.Status = AckConflict
			base.Message = keyConflictMessage(req.IdempotencyKey, conflict, requestPayloadDigest(req, nil))
			return base
		}
	}
	if req.Session != e.controllerSessionID() {
		base.Status = AckStale
		base.Message = "the request targets an earlier controller session; the profile may have been saved before a crash - reload it and resubmit"
		return base
	}
	digestNow := profileDigest(e.Config.ProfilePath)
	if digestNow != req.ProfileDigest {
		base.Status = AckConflict
		base.Message = "the saved profile changed since this edit started; reload the profile and resubmit"
		return base
	}
	if req.Reset {
		if digestNow == "" {
			base.Status = AckError
			base.Message = "no saved profile exists"
			return base
		}
		if err := os.Remove(e.Config.ProfilePath); err != nil {
			base.Status = AckError
			base.Message = err.Error()
			return base
		}
		// Effective settings stay unchanged until the controller restarts.
		base.Status = AckReset
		base.Message = "profile removed; effective settings apply until restart and the next eligible run asks again"
		return base
	}
	if len(req.Fields) == 0 {
		base.Status = AckError
		base.Message = "the request changes no fields"
		return base
	}
	for key, value := range req.Fields {
		if !settingFieldKeys[key] {
			base.Status = AckError
			base.Message = "unknown settings field " + key
			return base
		}
		if isNull(value) {
			base.Status = AckError
			base.Message = key + " must not be null"
			return base
		}
	}
	current, err := LoadProfile(e.Config.ProfilePath)
	if err != nil {
		base.Status = AckError
		base.Message = err.Error()
		return base
	}
	patched, err := patchProfile(current, req.Fields)
	if err != nil {
		base.Status = AckError
		base.Message = err.Error()
		return base
	}
	// Role names must exist in this controller's snapshot or in the newly
	// saved machine catalog. New definitions can be saved for restart without
	// allowing arbitrary unknown names into the project profile.
	for key, name := range map[string]string{"agent": patched.Agent, "review_agent": patched.ReviewAgent} {
		if _, touched := req.Fields[key]; !touched || name == "" {
			continue
		}
		if _, err := e.Config.agentProfile(name); err == nil {
			continue
		}
		path := orDefault(e.Config.AgentCatalogPath, agentCatalogPath())
		catalog, err := loadAgentCatalog(path)
		if err != nil {
			base.Status = AckError
			base.Message = err.Error()
			return base
		}
		if _, ok := catalog.Agents[name]; !ok {
			base.Status = AckError
			base.Message = fmt.Sprintf("unknown agent profile %q; add it with looper config", name)
			return base
		}
	}
	if err = SaveProfile(e.Config.ProfilePath, patched); err != nil {
		base.Status = AckError
		base.Message = err.Error()
		return base
	}
	base.ProfileDigest = profileDigest(e.Config.ProfilePath)
	base.Outcomes = e.applyLiveSettings(patched, req.Fields)
	base.Status = AckApplied
	return base
}

// recordSettingsRequest writes the durable receipt for one decided settings
// request. Applied and reset are applied outcomes; the refusal statuses are
// rejections that changed no saved work. A protocol engine without state
// records nothing, exactly as before.
func (e *Engine) recordSettingsRequest(req *SettingsRequest, ack SettingsAck) {
	if e.State == nil {
		return
	}
	if rec := e.requestReceipt(req.ID); rec != nil && requestStateTerminal(rec.State) {
		return
	}
	rec := &RequestRecord{
		ID: req.ID, Type: PublicRequestSettings, Session: e.controllerSessionID(),
		TargetSession: req.Session,
		SubmittedAt:   orDefault(req.CreatedAt, time.Now().UTC().Format(time.RFC3339Nano)),
		DecidedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		Ack:           &ack,
	}
	if req.IdempotencyKey != "" {
		rec.IdempotencyKey = req.IdempotencyKey
		rec.PayloadDigest = requestPayloadDigest(req, nil)
	}
	switch ack.Status {
	case AckApplied, AckReset:
		rec.State = RequestApplied
		rec.Summary = ack.Status + ": " + ack.Message
		for key := range ack.Outcomes {
			rec.Fields = append(rec.Fields, key)
		}
		sort.Strings(rec.Fields)
	default:
		rec.State = RequestRejected
		rec.Error = ack.Message
	}
	e.upsertRequest(rec)
	if err := e.Store.Save(e.State); err != nil && e.err == nil {
		e.err = err
	}
}

// applyLiveSettings moves saved values into the effective configuration for
// fields that support mid-run application. Flags and environment variables
// stay authoritative for this run; a conflicting profile edit is saved for a
// later run and reported as overridden. Verify and setup commands and the
// review frequency apply on the next controller run.
func (e *Engine) applyLiveSettings(p *Profile, fields map[string]json.RawMessage) map[string]string {
	outcomes := map[string]string{}
	touched := func(key string) bool { _, ok := fields[key]; return ok }
	overridden := func(key string, value string) {
		outcomes[key] = "saved; overridden by flag or environment for this run (" + value + ")"
	}
	applyString := func(key, value string, from Source, into *string) {
		if !touched(key) {
			return
		}
		if from >= SourceEnv {
			overridden(key, value)
			return
		}
		if key == "agent" || key == "review_agent" {
			if _, err := e.Config.agentProfile(value); err != nil {
				outcomes[key] = "saved; applies after controller restart"
				return
			}
		}
		*into = value
		outcomes[key] = "saved; applied"
	}
	applyString("agent", orDefault(p.Agent, orDefault(e.Config.DefaultWorker, defaultAgent)), e.Config.Sources.Agent, &e.Config.Agent)
	review := p.ReviewAgent
	if review == "" {
		review = e.Config.Agent
	}
	applyString("review_agent", review, e.Config.Sources.ReviewAgent, &e.Config.ReviewAgent)
	applyString("model", p.Model, e.Config.Sources.Model, &e.Config.Model)
	applyString("claude_model", p.ClaudeModel, e.Config.Sources.ClaudeModel, &e.Config.ClaudeModel)
	applyString("effort", p.Effort, e.Config.Sources.Effort, &e.Config.Effort)
	// Parallel sessions apply at the next dispatch boundary.
	if touched("jobs") {
		if e.Config.Sources.Jobs >= SourceEnv {
			overridden("jobs", "current limit kept")
		} else {
			jobs := p.Jobs
			if jobs == 0 {
				jobs = autoJobs()
				e.Config.JobsAuto = true
			} else {
				e.Config.JobsAuto = false
			}
			e.Config.Jobs = jobs
			// Decreasing jobs lets active workers finish and blocks new
			// dispatch; increasing jobs preserves backoff and memory limits.
			e.capacity = min(e.capacity, jobs)
			outcomes["jobs"] = "saved; applied at the next dispatch boundary"
		}
	}
	// Commands and review frequency never rewire verification mid-run.
	for _, key := range []string{"verify_command", "setup_command", "review_every"} {
		if touched(key) {
			outcomes[key] = "saved; applies on the next controller run"
		}
	}
	return outcomes
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// claimRequest claims the next queued request of typ on the owner goroutine
// and suppresses the idle wait's request wake for this pass when claiming
// fails. Envelopes the boundary just failed to claim would otherwise wake
// the idle wait at once, spinning the loop without sleep and growing the
// journal without bound. The next loop boundary clears the mark and
// retries the claims. Envelopes refused at claim time record their
// receipts here, so the refusal stays queryable after the envelope is
// removed.
func (e *Engine) claimRequest(typ string) (*SettingsRequest, error) {
	req, refusals, err := claimNextRequest(e.Dir, typ)
	e.recordRefusedRequests(refusals)
	if err != nil {
		e.suppressRequestWake = true
	}
	return req, err
}

// recordRefusedRequests records one rejected receipt per refused envelope
// and commits them together. An envelope that already holds a receipt keeps
// it: the recorded outcome stays authoritative, whatever removed the
// envelope since.
func (e *Engine) recordRefusedRequests(refusals []requestRefusal) {
	if len(refusals) == 0 || e.State == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, refusal := range refusals {
		if e.requestReceipt(refusal.id) != nil {
			continue
		}
		req := refusal.req
		if req == nil {
			req = &SettingsRequest{ID: refusal.id}
		}
		rec := &RequestRecord{
			ID: refusal.id, Type: publicRequestType(req.Type), State: RequestRejected,
			Session: e.controllerSessionID(), TargetSession: req.Session,
			SubmittedAt: orDefault(req.CreatedAt, now), DecidedAt: now,
			Error: refusal.message,
		}
		if req.IdempotencyKey != "" {
			// The refused envelope keeps its key with the digest its payload
			// allows: content digests need the plan files a refusal never
			// read, so an import refusal records the key without one and a
			// later reuse of that key conflicts instead of replaying.
			rec.IdempotencyKey = req.IdempotencyKey
			rec.PayloadDigest = requestPayloadDigest(req, nil)
		}
		e.upsertRequest(rec)
	}
	if err := e.Store.Save(e.State); err != nil && e.err == nil {
		e.err = err
	}
}

// processSettingsRequests claims and answers pending settings requests at a
// loop boundary. Invalid requests receive an error acknowledgement without
// stopping the controller.
func (e *Engine) processSettingsRequests() {
	if e.controller == nil {
		return
	}
	for {
		req, err := e.claimRequest(RequestTypeSettings)
		if err != nil {
			e.event("settings-error", err.Error())
			return
		}
		if req == nil {
			return
		}
		ack := e.applySettingsRequest(req)
		if err = writeJSON(filepath.Join(ackDir(e.Dir), req.ID+".json"), ack); err != nil {
			e.err = err
			return
		}
		os.Remove(filepath.Join(processingDir(e.Dir), req.ID+".json"))
		// A poisoned engine exits at the next loop check; this request is
		// answered and consumed, so the pass stops here.
		if e.err != nil {
			return
		}
	}
}

// requestSubmitWait bounds the wait for a controller acknowledgement. A
// variable so tests can exercise the timeout quickly.
var requestSubmitWait = 30 * time.Second

// publishSettingsRequest validates and writes one request envelope for the
// active controller, then returns without waiting (AX-011). Capabilities are
// validated before the envelope is published, so an unsupported request is
// refused without queueing work. The envelope is bound to this process, so a
// controller that starts later answers it instead of sweeping it. The durable
// envelope is the evidence the immediate receipt names.
func publishSettingsRequest(dir string, req *SettingsRequest) error {
	if req.ID == "" {
		req.ID = identifier("settings")
	}
	if req.Type == "" {
		req.Type = RequestTypeSettings
	}
	if req.IdempotencyKey != "" {
		if err := validateIdempotencyKey(req.IdempotencyKey); err != nil {
			return &inputError{code: "option_value_invalid", message: err.Error()}
		}
	}
	// An envelope type this executable cannot claim itself is refused here:
	// the capabilities check below compares public names, so a hand-written
	// envelope naming a public type would otherwise publish speculatively and
	// wait for a claim that never comes.
	if !supportedEnvelopeTypes[req.Type] {
		return &inputError{code: "option_value_invalid", message: fmt.Sprintf("unsupported request type %q; this executable answers %s requests", req.Type, strings.Join(controllerRequestTypes(), ", "))}
	}
	if err := validateRequestPublication(dir, req.Session, req.Type); err != nil {
		return err
	}
	// Bind the envelope to this process before publication: the startup
	// sweep of the next controller sweeps only requests whose submitter is
	// verifiably gone, so an in-flight request is answered, not deleted.
	req.SubmitterPID = os.Getpid()
	if identity, idErr := runIdentityCommand(req.SubmitterPID); idErr == nil {
		req.SubmitterIdentity = identity
	}
	req.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return writeJSON(filepath.Join(requestDir(dir), req.ID+".json"), req)
}

// submitSettingsRequest writes a request for the active controller and waits
// for its own acknowledgement: the explicit bounded wait behind --wait and
// the dashboard's confirmations. A timeout reports a pending request, never a
// cancellation. A controller session change during the wait is terminal for
// session-bound requests: metadata disappears only after a controller's
// final boundary, so a different recorded session means no controller will
// claim this envelope.
func submitSettingsRequest(dir string, req SettingsRequest, timeout time.Duration) (*SettingsAck, error) {
	if err := publishSettingsRequest(dir, &req); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ack, ok := readRequestAck(dir, req.ID); ok {
			return ack, nil
		}
		if current, changed := pendingSessionChange(dir, req.Session); changed {
			// The previous controller may have answered in the instant
			// between the two reads; one final check keeps the race report
			// from contradicting an acknowledgement that exists.
			if ack, ok := readRequestAck(dir, req.ID); ok {
				return ack, nil
			}
			return nil, &staleSessionError{requestType: publicRequestType(req.Type), targeted: req.Session, current: current, raced: true}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, &pendingError{
		message: fmt.Sprintf("request %s is still pending; the controller has not answered yet", req.ID),
		request: req.ID, waitKind: "controller acknowledgement",
	}
}

// readRequestAck loads this request's acknowledgement when it exists and
// parses. Any other state reports false so the caller keeps waiting.
func readRequestAck(dir, id string) (*SettingsAck, bool) {
	raw, err := os.ReadFile(filepath.Join(ackDir(dir), id+".json"))
	if err != nil {
		return nil, false
	}
	var ack SettingsAck
	if err = json.Unmarshal(raw, &ack); err != nil {
		return nil, false
	}
	return &ack, true
}

// runConfigOnline submits settings changes to the active controller through
// the request protocol and reports the acknowledgement.
func runConfigOnline(c Config, in *os.File, out, stderr io.Writer) int {
	_, dir := projectPaths(c)
	info, err := readController(dir)
	if err != nil {
		p := controllerMetadataError(err)
		p.Message = "A controller owns this repository, but its metadata cannot be verified: " + err.Error() + " Refusing to submit requests without verified ownership."
		p.withLegacy("A controller owns this repository, but its metadata cannot be verified: "+err.Error()+"\nRefusing to submit requests without verified ownership.", 1)
		return emitError(c, p, out, stderr)
	}
	if !controllerAlive(info) {
		p := unavailableFailure("controller_lock_stale", "The recorded controller session is not running; remove the stale lock or restart Looper.")
		p.Observed = map[string]any{"controller_session": info.Session, "controller_process": "not running"}
		p.Actions = []ErrorAction{{Description: "Remove the stale repository lock or restart Looper.", Command: "looper doctor"}}
		p.withLegacy("The recorded controller session is not running; remove the stale lock or restart Looper.", 1)
		return emitError(c, p, out, stderr)
	}
	if c.Reset {
		if code, handled := answerFromRetainedKey(c, dir, PublicRequestSettings, c.IdempotencyKey,
			requestPayloadDigest(&SettingsRequest{Type: RequestTypeSettings, Reset: true}, nil), out, stderr); handled {
			return code
		}
		if c.Wait {
			ack, err := submitSettingsRequest(dir, SettingsRequest{
				Version: SettingsRequestVersion, Session: info.Session,
				ProfileDigest: profileDigest(c.ProfilePath), Reset: true,
				IdempotencyKey: c.IdempotencyKey,
			}, c.waitBound())
			return reportAck(c, ack, err, out, stderr)
		}
		req := SettingsRequest{
			Version: SettingsRequestVersion, Session: info.Session,
			ProfileDigest: profileDigest(c.ProfilePath), Reset: true,
			IdempotencyKey: c.IdempotencyKey,
		}
		if err := publishSettingsRequest(dir, &req); err != nil {
			return emitError(c, requestSubmitError(err), out, stderr)
		}
		return emitMutationReceipt(c, keyedMutationReceipt(newMutationReceipt(req.ID, PublicRequestSettings, RequestSubmitted, req.CreatedAt, info.Session),
			c.IdempotencyKey, requestPayloadDigest(&req, nil), false),
			"Submitted a profile reset.", "", out)
	}
	if !isTTY(in) || !writerTTY(out) || !terminalInteractive() {
		p := operationFailure("terminal_unavailable", "looper config needs an interactive terminal; --reset works without one.")
		p.Actions = []ErrorAction{{Description: "Run the editor from an interactive terminal, or reset non-interactively.", Command: "looper config --reset"}}
		p.withLegacy("looper config needs an interactive terminal; --reset works without one.", 1)
		return emitError(c, p, out, stderr)
	}
	profile, err := LoadProfile(c.ProfilePath)
	if err != nil {
		return emitError(c, profileUnreadableError(err, c.ProfilePath), out, stderr)
	}
	before := settingsFromProfile(profile)
	s := before
	catalog, err := configureAgentCommands(c, &s)
	if err == nil {
		err = ui.RunEditorForm(&s)
	}
	if err == nil {
		err = confirmAgentConfiguration(c, s, catalog)
	}
	if err != nil {
		if errors.Is(err, ui.ErrAborted) {
			fmt.Fprintln(out, "Cancelled. Nothing was submitted.")
			return 0
		}
		return emitError(c, err, out, stderr)
	}
	if err = saveAgentCatalog(c.AgentCatalogPath, catalog); err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	fields := changedFields(before, s)
	if len(fields) == 0 {
		fmt.Fprintln(out, "Saved machine agent commands. Restart the controller to load command changes.")
		fmt.Fprintln(out, "No project settings changed.")
		return 0
	}
	if code, handled := answerFromRetainedKey(c, dir, PublicRequestSettings, c.IdempotencyKey,
		requestPayloadDigest(&SettingsRequest{Type: RequestTypeSettings, Fields: fields}, nil), out, stderr); handled {
		return code
	}
	if c.Wait {
		ack, err := submitSettingsRequest(dir, SettingsRequest{
			Version: SettingsRequestVersion, Session: info.Session,
			ProfileDigest: profileDigest(c.ProfilePath), Fields: fields,
			IdempotencyKey: c.IdempotencyKey,
		}, c.waitBound())
		return reportAck(c, ack, err, out, stderr)
	}
	req := SettingsRequest{
		Version: SettingsRequestVersion, Session: info.Session,
		ProfileDigest: profileDigest(c.ProfilePath), Fields: fields,
		IdempotencyKey: c.IdempotencyKey,
	}
	if err := publishSettingsRequest(dir, &req); err != nil {
		return emitError(c, requestSubmitError(err), out, stderr)
	}
	return emitMutationReceipt(c, keyedMutationReceipt(newMutationReceipt(req.ID, PublicRequestSettings, RequestSubmitted, req.CreatedAt, info.Session),
		c.IdempotencyKey, requestPayloadDigest(&req, nil), false),
		"Submitted settings changes.", "", out)
}

func reportAck(c Config, ack *SettingsAck, err error, out, stderr io.Writer) int {
	if err != nil {
		return emitError(c, requestSubmitError(err), out, stderr)
	}
	switch ack.Status {
	case AckApplied, AckReset:
		if c.Contract == ContractVersionV2 {
			return emitWaitRecord(c, settingsAckRecordView(ack, PublicRequestSettings), ack.ID, out, stderr)
		}
		fmt.Fprintf(out, "Settings request %s: %s\n", ack.ID, ack.Status)
		keys := make([]string, 0, len(ack.Outcomes))
		for key := range ack.Outcomes {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(out, "  %s: %s\n", key, ack.Outcomes[key])
		}
		return 0
	default:
		if c.Contract == ContractVersionV2 {
			return emitError(c, requestAckError(ack), out, stderr)
		}
		fmt.Fprintf(stderr, "Settings request %s: %s\n", ack.ID, ack.Status)
		if ack.Message != "" {
			fmt.Fprintln(stderr, " ", ack.Message)
		}
		return 1
	}
}

// changedFields diffs two form snapshots into assignment patches.
func changedFields(before, after ui.Settings) map[string]json.RawMessage {
	fields := map[string]json.RawMessage{}
	set := func(key, value string) {
		if b, err := json.Marshal(value); err == nil {
			fields[key] = b
		}
	}
	if before.Agent != after.Agent {
		set("agent", after.Agent)
	}
	if before.ReviewAgent != after.ReviewAgent {
		set("review_agent", after.ReviewAgent)
	}
	if before.Jobs != after.Jobs {
		if b, err := json.Marshal(after.Jobs); err == nil {
			fields["jobs"] = b
		}
	}
	if before.Model != after.Model {
		set("model", after.Model)
	}
	if before.ClaudeModel != after.ClaudeModel {
		set("claude_model", after.ClaudeModel)
	}
	if before.Effort != after.Effort {
		set("effort", after.Effort)
	}
	if before.VerifyCommand != after.VerifyCommand {
		set("verify_command", after.VerifyCommand)
	}
	if before.SetupCommand != after.SetupCommand {
		set("setup_command", after.SetupCommand)
	}
	if before.ReviewEvery != after.ReviewEvery {
		if b, err := json.Marshal(after.ReviewEvery); err == nil {
			fields["review_every"] = b
		}
	}
	return fields
}
