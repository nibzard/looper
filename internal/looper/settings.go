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
	if c.Sources.AnyFlag() || c.Sources.AnyEnv() {
		return reject("setting flags or environment variables were supplied")
	}
	if !isTTY(in) || !writerTTY(out) {
		return reject("input and output must be terminals")
	}
	if !terminalInteractive() {
		return reject("the terminal type does not support interaction")
	}
	return FormDecision{true, ""}
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
		return c, true, nil
	}
	s := ui.Settings{Agent: c.Agent, Jobs: c.Jobs, ReviewEvery: c.ReviewEvery}
	if c.JobsAuto {
		s.Jobs = 0
	}
	detection := DetectVerifyCommand(c.Root)
	s.VerifyCommand = detection.Command
	if err := ui.RunFirstRunForm(&s); err != nil {
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
	profile := ProfileFromSettings(s)
	created, err := saveProfileExclusive(c.ProfilePath, profile)
	if err != nil {
		return c, false, err
	}
	if !created {
		fmt.Fprintln(out, "A profile was created while the form was open; using it.")
	} else if !c.JSON {
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
// for absent fields.
func settingsFromProfile(p *Profile) ui.Settings {
	s := ui.Settings{Agent: "codex", ReviewEvery: 5}
	if p == nil {
		return s
	}
	s.Agent = p.Agent
	if s.Agent == "" {
		s.Agent = "codex"
	}
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
	if repositoryOwned(c) {
		return runConfigOnline(c, in, out, stderr)
	}
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	repo, err := OpenRepository(c, dir)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	lock, err := lockRepository(repo.Common)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	defer lock.Close()
	if c.Reset {
		if _, err := os.Stat(c.ProfilePath); err != nil {
			fmt.Fprintln(stderr, "No saved profile exists.")
			return 1
		}
		if err := os.Remove(c.ProfilePath); err != nil {
			fmt.Fprintln(stderr, "Error:", err)
			return 1
		}
		fmt.Fprintln(out, "Removed", c.ProfilePath)
		fmt.Fprintln(out, "Effective settings stay active until the controller restarts.")
		return 0
	}
	profile, err := LoadProfile(c.ProfilePath)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	if !isTTY(in) || !writerTTY(out) || !terminalInteractive() {
		fmt.Fprintln(stderr, "looper config needs an interactive terminal; --reset works without one.")
		return 1
	}
	if !c.JSON {
		fmt.Fprintln(out, "Effective settings:", EffectiveSettings(c))
	}
	s := settingsFromProfile(profile)
	detection := DetectVerifyCommand(c.Root)
	// Detection pre-fills the candidate when nothing explicit exists, so
	// clearing the field keeps planner selection instead of re-baking it.
	edited := s.VerifyCommand != ""
	if !edited {
		s.VerifyCommand = detection.Command
	}
	if err = ui.RunEditorForm(&s); err != nil {
		if errors.Is(err, ui.ErrAborted) {
			fmt.Fprintln(out, "Cancelled. Nothing was saved.")
			return 0
		}
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	saved := ProfileFromSettings(s)
	if profile != nil {
		saved.Unknown = profile.Unknown
	}
	if err = SaveProfile(c.ProfilePath, saved); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	if !c.JSON {
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
)

// SettingsRequest is a typed controller request written atomically to
// requests/<id>.json. Fields are assignment patches, never increments.
type SettingsRequest struct {
	Version       int                        `json:"version"`
	ID            string                     `json:"id"`
	Type          string                     `json:"type"`
	Session       string                     `json:"session"`
	ProfileDigest string                     `json:"profile_digest,omitempty"`
	Fields        map[string]json.RawMessage `json:"fields,omitempty"`
	Reset         bool                       `json:"reset,omitempty"`
	// Paths carries plan-import payloads on the same typed transport.
	Paths     []string `json:"paths,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
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

// Ack statuses.
const (
	AckApplied  = "applied"
	AckReset    = "reset"
	AckConflict = "conflict"
	AckError    = "error"
	AckStale    = "stale-session"
)

var settingFieldKeys = map[string]bool{
	"agent": true, "review_agent": true, "jobs": true, "model": true,
	"claude_model": true, "effort": true, "verify_command": true,
	"setup_command": true, "review_every": true,
}

// maxPendingRequests bounds the request queues; invalid or flooded queues
// produce errors without stopping the controller.
const maxPendingRequests = 100

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
	if req.Type == "" {
		req.Type = RequestTypeSettings
	}
	return &req, nil
}

// claimNextRequest renames the oldest request of the given type into
// processing/, claiming it. One request is claimed at a time.
func claimNextRequest(dir, typ string) (*SettingsRequest, error) {
	entries, err := os.ReadDir(requestDir(dir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > maxPendingRequests {
		return nil, fmt.Errorf("more than %d pending requests", maxPendingRequests)
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
			// Invalid envelopes receive an error ack without stopping.
			id := strings.TrimSuffix(name, ".json")
			writeJSON(filepath.Join(ackDir(dir), id+".json"), SettingsAck{
				Version: SettingsRequestVersion, ID: id, Status: AckError, Message: err.Error(),
			})
			os.Remove(filepath.Join(requestDir(dir), name))
			continue
		}
		if req.Type != typ {
			continue
		}
		if err = os.MkdirAll(processingDir(dir), 0700); err != nil {
			return nil, err
		}
		// Claim under the envelope identifier so the acknowledgement and the
		// removal always target the same file, whatever the source name was.
		if err = os.Rename(filepath.Join(requestDir(dir), name), filepath.Join(processingDir(dir), req.ID+".json")); err != nil {
			return nil, err
		}
		return req, nil
	}
	return nil, nil
}

// clearSessionFiles removes settings requests and acknowledgements left by
// earlier controller sessions. Plan-import requests survive restarts with
// their durable receipts and are never removed here.
func clearSessionFiles(dir string) {
	for _, base := range []string{requestDir(dir), processingDir(dir)} {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			req, err := loadSettingsRequest(filepath.Join(base, entry.Name()))
			if err == nil && req.Type == RequestTypePlanImport {
				continue
			}
			os.Remove(filepath.Join(base, entry.Name()))
		}
	}
	entries, err := os.ReadDir(ackDir(dir))
	if err != nil {
		return
	}
	for _, entry := range entries {
		os.Remove(filepath.Join(ackDir(dir), entry.Name()))
	}
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
// never interrupted to apply settings.
func (e *Engine) applySettingsRequest(req *SettingsRequest) SettingsAck {
	base := SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controller.Session}
	if ack, ok := e.acks[req.ID]; ok {
		// Duplicate identifiers return the recorded acknowledgement.
		return ack
	}
	defer func() {
		e.acks[req.ID] = base
		detail := base.Status
		if base.Message != "" {
			detail += ": " + base.Message
		}
		e.event("settings", fmt.Sprintf("Settings request %s %s", req.ID, detail))
	}()
	if req.Session != e.controller.Session {
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
		*into = value
		outcomes[key] = "saved; applied"
	}
	applyString("agent", orDefault(p.Agent, "codex"), e.Config.Sources.Agent, &e.Config.Agent)
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

// processSettingsRequests claims and answers pending settings requests at a
// loop boundary. Invalid requests receive an error acknowledgement without
// stopping the controller.
func (e *Engine) processSettingsRequests() {
	if e.controller == nil {
		return
	}
	for {
		req, err := claimNextRequest(e.Dir, RequestTypeSettings)
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
	}
}

// submitSettingsRequest writes a request for the active controller and waits
// for its own acknowledgement. A timeout reports a pending request, never a
// cancellation.
func submitSettingsRequest(dir string, req SettingsRequest, timeout time.Duration) (*SettingsAck, error) {
	if req.ID == "" {
		req.ID = identifier("settings")
	}
	if req.Type == "" {
		req.Type = RequestTypeSettings
	}
	req.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeJSON(filepath.Join(requestDir(dir), req.ID+".json"), req); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join(ackDir(dir), req.ID+".json"))
		if err == nil {
			var ack SettingsAck
			if err = json.Unmarshal(raw, &ack); err == nil {
				return &ack, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("request %s is still pending; the controller has not answered yet", req.ID)
}

// runConfigOnline submits settings changes to the active controller through
// the request protocol and reports the acknowledgement.
func runConfigOnline(c Config, in *os.File, out, stderr io.Writer) int {
	_, dir := projectPaths(c)
	info, err := readController(dir)
	if err != nil {
		fmt.Fprintln(stderr, "A controller owns this repository, but its metadata cannot be verified:", err)
		fmt.Fprintln(stderr, "Refusing to submit requests without verified ownership.")
		return 1
	}
	if !controllerAlive(info) {
		fmt.Fprintln(stderr, "The recorded controller session is not running; remove the stale lock or restart Looper.")
		return 1
	}
	if c.Reset {
		ack, err := submitSettingsRequest(dir, SettingsRequest{
			Version: SettingsRequestVersion, Session: info.Session,
			ProfileDigest: profileDigest(c.ProfilePath), Reset: true,
		}, 30*time.Second)
		return reportAck(ack, err, out, stderr)
	}
	if !isTTY(in) || !writerTTY(out) || !terminalInteractive() {
		fmt.Fprintln(stderr, "looper config needs an interactive terminal; --reset works without one.")
		return 1
	}
	profile, err := LoadProfile(c.ProfilePath)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	before := settingsFromProfile(profile)
	s := before
	if err = ui.RunEditorForm(&s); err != nil {
		if errors.Is(err, ui.ErrAborted) {
			fmt.Fprintln(out, "Cancelled. Nothing was submitted.")
			return 0
		}
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	fields := changedFields(before, s)
	if len(fields) == 0 {
		fmt.Fprintln(out, "No changes.")
		return 0
	}
	ack, err := submitSettingsRequest(dir, SettingsRequest{
		Version: SettingsRequestVersion, Session: info.Session,
		ProfileDigest: profileDigest(c.ProfilePath), Fields: fields,
	}, 30*time.Second)
	return reportAck(ack, err, out, stderr)
}

func reportAck(ack *SettingsAck, err error, out, stderr io.Writer) int {
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	switch ack.Status {
	case AckApplied, AckReset:
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
