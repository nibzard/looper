package looper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The non-interactive configuration surface (OW-05 and OW-06). Explicit
// modes replace the interactive editor: --show reads, --preview validates,
// and --set/--unset change the saved project profile or the machine agent
// catalog. No mode here needs a terminal.

const configSchemaVersion = 1

const (
	configScopeProject = "project"
	configScopeMachine = "machine"
)

// One outcome kind per assigned field. The saved document and the preview
// share them.
const (
	configOutcomeSaved      = "saved"
	configOutcomeSavedLive  = "saved-live"
	configOutcomeOverridden = "saved-overridden"
	configOutcomeRestart    = "saved-restart-required"
	configOutcomeUnchanged  = "unchanged"
)

var configOutcomeNotes = map[string]string{
	configOutcomeSaved:      "saved; it becomes effective when the next controller starts",
	configOutcomeSavedLive:  "saved; the controller applies it at the next dispatch boundary",
	configOutcomeOverridden: "saved, but an environment variable or flag overrides the effective value",
	configOutcomeRestart:    "saved, but a running controller keeps its snapshot until it restarts",
	configOutcomeUnchanged:  "unchanged; the saved value already matches",
}

// configProjectFields maps each public project field onto its canonical
// profile key. The role spellings share one canonical key, so an assignment
// through either name produces one payload digest.
var configProjectFields = map[string]string{
	"worker": "agent", "agent": "agent",
	"reviewer": "review_agent", "review_agent": "review_agent",
	"jobs": "jobs", "model": "model", "claude_model": "claude_model",
	"effort": "effort", "verify_command": "verify_command",
	"setup_command": "setup_command", "review_every": "review_every",
}

// configProjectPublic maps canonical keys back to the public field name.
var configProjectPublic = map[string]string{
	"agent": "worker", "review_agent": "reviewer", "jobs": "jobs",
	"model": "model", "claude_model": "claude_model", "effort": "effort",
	"verify_command": "verify_command", "setup_command": "setup_command",
	"review_every": "review_every",
}

var configMachineFields = map[string]string{
	"worker": "worker", "agent": "worker",
	"reviewer": "reviewer", "review_agent": "reviewer",
}

var configMachineAgentSuffixes = []string{"adapter", "command", "model", "effort"}

// configPatch is one parsed change request. The fields keys are canonical
// paths: profile keys for project scope and catalog paths for machine scope.
// An empty value with unsets set means clear the field.
type configPatch struct {
	scope  string
	fields map[string]string
	unsets map[string]bool
	order  []string
}

// configHeadlessRequested reports whether an explicit non-interactive mode
// was asked for. These modes never open the interactive editor.
func configHeadlessRequested(c Config) bool {
	return c.ConfigShow || c.Preview || len(c.ConfigSets) > 0 || len(c.ConfigUnsets) > 0
}

// parseConfigPatch turns --set and --unset arguments into a validated patch.
// It parses each assignment at the first "=" and preserves the remaining
// characters exactly. Unknown fields, duplicate assignments, and set/unset
// conflicts fail before any saved state is read.
func parseConfigPatch(scope string, sets, unsets []string) (*configPatch, error) {
	patch := &configPatch{scope: scope, fields: map[string]string{}, unsets: map[string]bool{}}
	assign := func(raw string, unset bool) error {
		field, value := raw, ""
		if !unset {
			key, val, found := strings.Cut(raw, "=")
			if !found {
				return fmt.Errorf("assignment %q has no '='; write --set field=value", raw)
			}
			field, value = key, val
		}
		if field == "" {
			return fmt.Errorf("an empty field name is not a setting")
		}
		canonical, err := canonicalConfigField(scope, field)
		if err != nil {
			return err
		}
		if _, exists := patch.fields[canonical]; exists {
			return fmt.Errorf("field %s is named by more than one --set or --unset; keep one assignment", canonical)
		}
		patch.fields[canonical] = value
		patch.unsets[canonical] = unset
		patch.order = append(patch.order, canonical)
		return nil
	}
	for _, raw := range sets {
		if err := assign(raw, false); err != nil {
			return nil, err
		}
	}
	for _, raw := range unsets {
		if err := assign(raw, true); err != nil {
			return nil, err
		}
	}
	return patch, nil
}

// canonicalConfigField maps one public field spelling onto its canonical
// path for the scope. Machine agent names may contain dots, so the parser
// matches the final known suffix instead of splitting the full path.
func canonicalConfigField(scope, field string) (string, error) {
	if scope == configScopeMachine {
		if canonical, ok := configMachineFields[field]; ok {
			return canonical, nil
		}
		if _, ok := configProjectFields[field]; ok {
			return "", fmt.Errorf("field %q applies to the saved project profile; machine scope supports worker, reviewer, and agents.<name>.<field>", field)
		}
		if rest, ok := strings.CutPrefix(field, "agents."); ok {
			for _, suffix := range configMachineAgentSuffixes {
				if name, found := strings.CutSuffix(rest, "."+suffix); found {
					if !agentProfileName.MatchString(name) {
						return "", fmt.Errorf("agent profile name %q is invalid", name)
					}
					return "agents." + name + "." + suffix, nil
				}
			}
			return "", fmt.Errorf("field %q is not a machine agent setting; the suffix must be adapter, command, model, or effort", field)
		}
		return "", fmt.Errorf("field %q is not a machine setting", field)
	}
	if canonical, ok := configProjectFields[field]; ok {
		return canonical, nil
	}
	if strings.HasPrefix(field, "agents.") {
		return "", fmt.Errorf("field %q defines a machine profile; add --scope machine", field)
	}
	return "", fmt.Errorf("field %q is not a project setting", field)
}

// splitMachineAgentPath splits one canonical machine agent path into its
// profile name and field suffix.
func splitMachineAgentPath(path string) (name, suffix string, ok bool) {
	rest, isAgent := strings.CutPrefix(path, "agents.")
	if !isAgent {
		return "", "", false
	}
	for _, s := range configMachineAgentSuffixes {
		if n, found := strings.CutSuffix(rest, "."+s); found {
			return n, s, true
		}
	}
	return "", "", false
}

// validateProfileRole enforces the shared role rule from the live settings
// handler: a selected role name must resolve in this process's catalog
// snapshot or in the saved machine catalog. The stopped path applies the
// same rules as the live path (OW-05).
func validateProfileRole(c Config, name string) error {
	if name == "" {
		return nil
	}
	if _, err := c.agentProfile(name); err == nil {
		return nil
	}
	catalog, err := loadAgentCatalog(orDefault(c.AgentCatalogPath, agentCatalogPath()))
	if err != nil {
		return err
	}
	if _, ok := catalog.Agents[name]; !ok {
		return fmt.Errorf("unknown agent profile %q; define it with looper config --scope machine", name)
	}
	return nil
}

// configPatchOutcome reports one field of a configuration change.
type configPatchOutcome struct {
	Field   string `json:"field"`
	From    any    `json:"from"`
	To      any    `json:"to"`
	Outcome string `json:"outcome"`
	Note    string `json:"note,omitempty"`
}

// profileFieldValue renders one canonical profile field for reports.
func profileFieldValue(p *Profile, field string) any {
	if p == nil {
		if field == "review_every" {
			return nil
		}
		if field == "jobs" {
			return 0
		}
		return ""
	}
	switch field {
	case "agent":
		return clampJSONString(p.Agent, 1025)
	case "review_agent":
		return clampJSONString(p.ReviewAgent, 1025)
	case "jobs":
		return p.Jobs
	case "model":
		return clampJSONString(p.Model, 1025)
	case "claude_model":
		return clampJSONString(p.ClaudeModel, 1025)
	case "effort":
		return clampJSONString(p.Effort, 1025)
	case "verify_command":
		return clampJSONString(p.VerifyCommand, 1025)
	case "setup_command":
		return clampJSONString(p.SetupCommand, 1025)
	case "review_every":
		if p.ReviewEvery == nil {
			return nil
		}
		return *p.ReviewEvery
	}
	return nil
}

var configProfileSources = map[string]func(Sources) Source{
	"agent":          func(s Sources) Source { return s.Agent },
	"review_agent":   func(s Sources) Source { return s.ReviewAgent },
	"jobs":           func(s Sources) Source { return s.Jobs },
	"model":          func(s Sources) Source { return s.Model },
	"claude_model":   func(s Sources) Source { return s.ClaudeModel },
	"effort":         func(s Sources) Source { return s.Effort },
	"verify_command": func(s Sources) Source { return s.Verify },
	"setup_command":  func(s Sources) Source { return s.Setup },
	"review_every":   func(s Sources) Source { return s.ReviewEvery },
}

// projectPatchFields renders the canonical assignment map the settings
// request protocol carries. Aliases are canonical before this map exists,
// so equivalent assignments produce one payload digest.
func projectPatchFields(patch *configPatch) (map[string]json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	for _, canonical := range patch.order {
		value := patch.fields[canonical]
		switch canonical {
		case "jobs", "review_every":
			// An unset carries an empty string, which is not a number. jobs
			// stores 0 (auto mode). review_every is the one optional numeric
			// key, so its unset rides the protocol as null and patchProfile
			// turns null into a removal: the default cadence applies again.
			if patch.unsets[canonical] {
				if canonical == removableSettingKey {
					fields[canonical] = json.RawMessage("null")
					continue
				}
				value = "0"
			}
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%s must be a nonnegative integer; %q is not", configProjectPublic[canonical], value)
			}
			b, _ := json.Marshal(n)
			fields[canonical] = b
		default:
			b, _ := json.Marshal(value)
			fields[canonical] = b
		}
	}
	return fields, nil
}

// applyProjectPatch composes the next saved profile from one parsed patch.
// It validates every field, the complete resulting profile, and the shared
// role rules, and reports per-field outcomes. It writes nothing (OW-05).
func applyProjectPatch(c Config, current *Profile, patch *configPatch) (*Profile, map[string]json.RawMessage, []configPatchOutcome, error) {
	fields, err := projectPatchFields(patch)
	if err != nil {
		return nil, nil, nil, err
	}
	patched, err := patchProfile(current, fields)
	if err != nil {
		return nil, nil, nil, err
	}
	// An unset review_every arrives as null in fields, and patchProfile
	// applies the removal itself, so both the stopped and the live paths
	// save a profile without the key.
	for _, role := range []string{"agent", "review_agent"} {
		if _, touched := patch.fields[role]; !touched {
			continue
		}
		name := patched.Agent
		if role == "review_agent" {
			name = patched.ReviewAgent
		}
		if err := validateProfileRole(c, name); err != nil {
			return nil, nil, nil, err
		}
	}
	outcomes := make([]configPatchOutcome, 0, len(patch.order))
	for _, canonical := range patch.order {
		from := profileFieldValue(current, canonical)
		to := profileFieldValue(patched, canonical)
		outcome := configOutcomeSaved
		switch {
		case from == to:
			outcome = configOutcomeUnchanged
		case configProfileSources[canonical](c.Sources) >= SourceEnv:
			outcome = configOutcomeOverridden
		}
		outcomes = append(outcomes, configPatchOutcome{
			Field: configProjectPublic[canonical], From: from, To: to,
			Outcome: outcome, Note: configOutcomeNotes[outcome],
		})
	}
	return patched, fields, outcomes, nil
}

// machineFieldValue renders one canonical machine path for reports.
func machineFieldValue(catalog *AgentCatalog, path string) any {
	if catalog == nil {
		return nil
	}
	if name, suffix, ok := splitMachineAgentPath(path); ok {
		p, exists := catalog.Agents[name]
		if !exists {
			return nil
		}
		switch suffix {
		case "adapter":
			return clampJSONString(p.Adapter, 1025)
		case "command":
			return clampJSONString(p.Command, 1025)
		case "model":
			return clampJSONString(p.Model, 1025)
		case "effort":
			return clampJSONString(p.Effort, 1025)
		}
		return nil
	}
	switch path {
	case "worker":
		return catalog.Worker
	case "reviewer":
		return catalog.Reviewer
	}
	return nil
}

// applyMachinePatch composes the next machine catalog from one parsed patch.
// Defining a new agent profile needs its adapter and command together; an
// existing definition accepts partial updates. The complete catalog is
// validated before anything is saved. It writes nothing (OW-05).
func applyMachinePatch(current *AgentCatalog, patch *configPatch) (*AgentCatalog, []configPatchOutcome, error) {
	agents := make(map[string]AgentProfile, len(current.Agents))
	for name, p := range current.Agents {
		agents[name] = p
	}
	version := current.Version
	if version == 0 {
		version = 1
	}
	next := &AgentCatalog{revision: current.revision, Version: version, Agents: agents, Worker: current.Worker, Reviewer: current.Reviewer}
	type agentOp struct {
		value string
		unset bool
	}
	ops := map[string]map[string]agentOp{}
	fresh := map[string]bool{}
	for _, path := range patch.order {
		if name, suffix, ok := splitMachineAgentPath(path); ok {
			if ops[name] == nil {
				ops[name] = map[string]agentOp{}
				if _, defined := next.Agents[name]; !defined {
					fresh[name] = true
				}
			}
			ops[name][suffix] = agentOp{value: patch.fields[path], unset: patch.unsets[path]}
			continue
		}
		switch path {
		case "worker":
			next.Worker = patch.fields[path]
		case "reviewer":
			next.Reviewer = patch.fields[path]
		}
	}
	names := make([]string, 0, len(fresh))
	for name := range fresh {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		set := ops[name]
		if set["adapter"].value == "" || set["command"].value == "" {
			return nil, nil, fmt.Errorf("defining agent profile %s needs its adapter and command together", name)
		}
		next.Agents[name] = AgentProfile{
			Adapter: set["adapter"].value, Command: set["command"].value,
			Model: set["model"].value, Effort: set["effort"].value,
		}
	}
	for name, set := range ops {
		if fresh[name] {
			continue
		}
		p := next.Agents[name]
		for suffix, op := range set {
			switch suffix {
			case "adapter":
				p.Adapter = op.value
			case "command":
				p.Command = op.value
			case "model":
				p.Model = op.value
			case "effort":
				p.Effort = op.value
			}
		}
		next.Agents[name] = p
	}
	if err := next.validate(); err != nil {
		return nil, nil, err
	}
	outcomes := make([]configPatchOutcome, 0, len(patch.order))
	for _, path := range patch.order {
		from := machineFieldValue(current, path)
		to := machineFieldValue(next, path)
		outcome := configOutcomeRestart
		if from == to {
			outcome = configOutcomeUnchanged
		}
		outcomes = append(outcomes, configPatchOutcome{
			Field: path, From: from, To: to,
			Outcome: outcome, Note: configOutcomeNotes[outcome],
		})
	}
	return next, outcomes, nil
}

// Document shapes. The three kinds share one schema with a oneOf.

type configDocumentHead struct {
	ContractVersion int    `json:"contract_version"`
	SchemaVersion   int    `json:"schema_version"`
	Kind            string `json:"kind"`
	GeneratedAt     string `json:"generated_at"`
	Scope           string `json:"scope"`
}

type configSavedProfileView struct {
	Worker        string `json:"worker"`
	Reviewer      string `json:"reviewer"`
	Jobs          int    `json:"jobs"`
	Model         string `json:"model"`
	ClaudeModel   string `json:"claude_model"`
	Effort        string `json:"effort"`
	VerifyCommand string `json:"verify_command"`
	SetupCommand  string `json:"setup_command"`
	ReviewEvery   *int   `json:"review_every"`
}

type configValueSource struct {
	Value  any    `json:"value"`
	Source string `json:"source"`
}

type configEffectiveView struct {
	Worker        configValueSource `json:"worker"`
	Reviewer      configValueSource `json:"reviewer"`
	Jobs          configValueSource `json:"jobs"`
	Model         configValueSource `json:"model"`
	ClaudeModel   configValueSource `json:"claude_model"`
	Effort        configValueSource `json:"effort"`
	VerifyCommand configValueSource `json:"verify_command"`
	SetupCommand  configValueSource `json:"setup_command"`
	ReviewEvery   configValueSource `json:"review_every"`
}

type configAgentView struct {
	Name    string `json:"name"`
	Adapter string `json:"adapter"`
	Command string `json:"command"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
}

type configMachineView struct {
	Path        string            `json:"path"`
	Exists      bool              `json:"exists"`
	Revision    string            `json:"revision"`
	Worker      string            `json:"worker"`
	Reviewer    string            `json:"reviewer"`
	Agents      []configAgentView `json:"agents"`
	AgentTotal  int               `json:"agent_total"`
	OmittedHint string            `json:"omitted_hint,omitempty"`
}

type configControllerView struct {
	State   string `json:"state"`
	Session string `json:"session,omitempty"`
}

type configDetail struct {
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

type configShowDocument struct {
	configDocumentHead
	Saved      *configSavedProfileView `json:"saved"`
	Effective  *configEffectiveView    `json:"effective"`
	Machine    configMachineView       `json:"machine"`
	Controller *configControllerView   `json:"controller"`
	Detail     configDetail            `json:"detail"`
}

type configApplicationView struct {
	Mode        string `json:"mode"`
	Description string `json:"description"`
	Session     string `json:"session,omitempty"`
	Command     string `json:"command,omitempty"`
}

type configPreviewDocument struct {
	configDocumentHead
	Revision     string                `json:"revision"`
	Changes      []configPatchOutcome  `json:"changes"`
	Application  configApplicationView `json:"application"`
	IfRevisionOK bool                  `json:"if_revision_ok"`
	Detail       configDetail          `json:"detail"`
}

type configJournalView struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

type configSavedDocument struct {
	configDocumentHead
	Revision string               `json:"revision"`
	Outcomes []configPatchOutcome `json:"outcomes"`
	Journal  *configJournalView   `json:"journal,omitempty"`
	Detail   configDetail         `json:"detail"`
}

const configShowKind = "looper.config.show"
const configPreviewKind = "looper.config.preview"
const configSavedKind = "looper.config.saved"

func configNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// savedProfileView renders the saved profile with public field names. A nil
// profile means no saved profile exists.
func savedProfileView(p *Profile) *configSavedProfileView {
	if p == nil {
		return nil
	}
	return &configSavedProfileView{
		Worker: clampJSONString(p.Agent, 1025), Reviewer: clampJSONString(p.ReviewAgent, 1025), Jobs: p.Jobs,
		Model: clampJSONString(p.Model, 1025), ClaudeModel: clampJSONString(p.ClaudeModel, 1025), Effort: clampJSONString(p.Effort, 1025),
		VerifyCommand: clampJSONString(p.VerifyCommand, 1025), SetupCommand: clampJSONString(p.SetupCommand, 1025),
		ReviewEvery: p.ReviewEvery,
	}
}

func effectiveConfigView(c Config) *configEffectiveView {
	value := func(v string) any { return clampJSONString(v, 1025) }
	return &configEffectiveView{
		Worker:        configValueSource{Value: value(c.Agent), Source: c.Sources.Agent.String()},
		Reviewer:      configValueSource{Value: value(c.ReviewAgent), Source: c.Sources.ReviewAgent.String()},
		Jobs:          configValueSource{Value: c.Jobs, Source: c.Sources.Jobs.String()},
		Model:         configValueSource{Value: value(c.Model), Source: c.Sources.Model.String()},
		ClaudeModel:   configValueSource{Value: value(c.ClaudeModel), Source: c.Sources.ClaudeModel.String()},
		Effort:        configValueSource{Value: value(c.Effort), Source: c.Sources.Effort.String()},
		VerifyCommand: configValueSource{Value: value(c.Verify), Source: c.Sources.Verify.String()},
		SetupCommand:  configValueSource{Value: value(c.Setup), Source: c.Sources.Setup.String()},
		ReviewEvery:   configValueSource{Value: c.ReviewEvery, Source: c.Sources.ReviewEvery.String()},
	}
}

// machineConfigView reads the catalog read-only. It never writes a catalog,
// lock, or directory, so show and preview can call it freely.
func machineConfigView(c Config) (configMachineView, error) {
	path := orDefault(c.AgentCatalogPath, agentCatalogPath())
	catalog, err := loadAgentCatalog(path)
	if err != nil {
		return configMachineView{}, err
	}
	view := configMachineView{
		Path: path, Exists: catalog.revision != "", Revision: catalog.revision,
		Worker: catalog.Worker, Reviewer: catalog.Reviewer,
	}
	names := make([]string, 0, len(catalog.Agents))
	for name := range catalog.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	view.Agents = []configAgentView{}
	view.AgentTotal = len(names)
	const listed = 32
	for _, name := range names {
		if len(view.Agents) == listed {
			view.OmittedHint = fmt.Sprintf("the catalog lists %d agents; the document shows the first %d", len(names), listed)
			break
		}
		p := catalog.Agents[name]
		view.Agents = append(view.Agents, configAgentView{
			Name: name, Adapter: p.Adapter, Command: clampJSONString(p.Command, 257),
			Model: clampJSONString(p.Model, 257), Effort: clampJSONString(p.Effort, 257),
		})
	}
	return view, nil
}

// controllerConfigView reads controller metadata without probing the lock
// file, because show must not create anything. A missing record means no
// controller owns the repository; an unreadable one stays unknown.
func controllerConfigView(c Config) *configControllerView {
	_, dir := projectPaths(c)
	if _, err := os.Stat(controllerPath(dir)); err != nil {
		if os.IsNotExist(err) {
			return &configControllerView{State: "stopped"}
		}
		return &configControllerView{State: "unknown"}
	}
	info, err := readController(dir)
	if err != nil {
		return &configControllerView{State: "unknown"}
	}
	if !controllerAlive(info) {
		return &configControllerView{State: "stopped", Session: clampJSONString(info.Session, 257)}
	}
	return &configControllerView{State: "owning", Session: clampJSONString(info.Session, 257)}
}

// The settings journal gives stopped project edits a durable result without
// inventing a controller run (OW-06). It lives beside profile.json.
const configJournalName = "settings-journal.json"
const configJournalMaxTerminal = 200

type configJournalEntry struct {
	ID        string            `json:"id"`
	Scope     string            `json:"scope"`
	OldDigest string            `json:"old_digest"`
	NewDigest string            `json:"new_digest"`
	Patch     map[string]string `json:"patch"`
	// Unsets names the paths the patch removed rather than assigned. An
	// assignment to the empty string clears the same field an unset removes,
	// so the recorded spelling is what keeps the two edits distinct. A
	// current entry always writes the list, even empty; only a legacy entry
	// lacks it.
	Unsets    []string `json:"unsets"`
	Key       string   `json:"key,omitempty"`
	State     string   `json:"state"`
	Result    string   `json:"result,omitempty"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

type configJournalFile struct {
	Entries []configJournalEntry `json:"entries"`
}

func loadConfigJournal(path string) (*configJournalFile, error) {
	var j configJournalFile
	if err := readJSON(path, &j); err != nil {
		if os.IsNotExist(err) {
			return &configJournalFile{}, nil
		}
		return nil, fmt.Errorf("settings journal %s is unreadable: %w", path, err)
	}
	return &j, nil
}

func saveConfigJournal(path string, j *configJournalFile) error {
	if err := writeJSON(path, j); err != nil {
		return fmt.Errorf("settings journal %s: %w", path, err)
	}
	return nil
}

func configJournalTerminal(state string) bool {
	return state == "done" || state == "failed"
}

// trimConfigJournal keeps every unresolved entry and the newest
// configJournalMaxTerminal terminal entries.
func trimConfigJournal(j *configJournalFile) {
	kept := make([]configJournalEntry, 0, len(j.Entries))
	terminal := 0
	for i := len(j.Entries) - 1; i >= 0; i-- {
		e := j.Entries[i]
		if !configJournalTerminal(e.State) {
			kept = append(kept, e)
			continue
		}
		if terminal < configJournalMaxTerminal {
			kept = append(kept, e)
			terminal++
		}
	}
	for i, j2 := 0, len(kept)-1; i < j2; i, j2 = i+1, j2-1 {
		kept[i], kept[j2] = kept[j2], kept[i]
	}
	j.Entries = kept
}

// resolveConfigJournal finishes entries a crash left unresolved. The digest
// on disk decides: the new digest completed the save, the old digest means
// the write never landed, and a third digest is foreign work this command
// must not replace.
func resolveConfigJournal(path, profilePath string) error {
	j, err := loadConfigJournal(path)
	if err != nil {
		return stateUnreadableError(path, err)
	}
	actual := profileDigest(profilePath)
	now := configNow()
	changed := false
	for i := range j.Entries {
		e := &j.Entries[i]
		if e.State != "prepared" {
			continue
		}
		switch actual {
		case e.NewDigest:
			e.State, e.Result, e.UpdatedAt = "done", "the save completed before the interruption", now
			changed = true
		case e.OldDigest:
			e.State, e.Result, e.UpdatedAt = "failed", "the profile write never landed; nothing changed", now
			changed = true
		default:
			p := operationFailure("unexpected_profile_digest", "The saved profile changed outside the last recorded settings edit. Resolve the profile manually before changing settings again.")
			p.Observed = map[string]any{"journal_entry": e.ID, "profile_digest": actual}
			p.Paths = []string{profilePath, path}
			p.withLegacy("The saved profile changed outside the last recorded settings edit; resolve it manually.", 1)
			return p
		}
	}
	if !changed {
		return nil
	}
	if err := saveConfigJournal(path, j); err != nil {
		return persistenceError(err.Error())
	}
	return nil
}

// configJournalMatches reports whether one recorded entry carries the same
// edit as a patch. A legacy entry records no unset spelling (the field is
// absent and decodes to nil), so its patch map alone decides, exactly as when
// it was written. A current entry always records the list, empty or not, so
// an assignment to the empty string and an unset stay distinct.
func configJournalMatches(e *configJournalEntry, patch *configPatch) bool {
	if fmt.Sprint(e.Patch) != fmt.Sprint(configPatchMap(patch)) {
		return false
	}
	return e.Unsets == nil || fmt.Sprint(e.Unsets) == fmt.Sprint(configPatchUnsets(patch))
}

// findConfigJournalReplay returns the recorded terminal entry for one caller
// key when the same patch is submitted again.
func findConfigJournalReplay(j *configJournalFile, key string, patch *configPatch) *configJournalEntry {
	return findConfigJournalEntry(j, key, func(e *configJournalEntry) bool { return configJournalMatches(e, patch) })
}

// findConfigJournalConflict returns the terminal entry one caller key already
// recorded under different content. A reused key replays its recorded outcome
// or refuses; it never silently applies changed content (AX-012).
func findConfigJournalConflict(j *configJournalFile, key string, patch *configPatch) *configJournalEntry {
	return findConfigJournalEntry(j, key, func(e *configJournalEntry) bool { return !configJournalMatches(e, patch) })
}

// findConfigJournalEntry returns the newest terminal entry under one caller
// key that satisfies want.
func findConfigJournalEntry(j *configJournalFile, key string, want func(*configJournalEntry) bool) *configJournalEntry {
	if key == "" {
		return nil
	}
	for i := len(j.Entries) - 1; i >= 0; i-- {
		e := &j.Entries[i]
		if e.Key == key && configJournalTerminal(e.State) && want(e) {
			return e
		}
	}
	return nil
}

func configPatchMap(patch *configPatch) map[string]string {
	m := make(map[string]string, len(patch.order))
	for _, path := range patch.order {
		m[path] = patch.fields[path]
	}
	return m
}

// configPatchUnsets names the paths one patch removes rather than assigns.
// The list is never nil, so a recorded entry is distinguishable from a legacy
// one that predates the field.
func configPatchUnsets(patch *configPatch) []string {
	unsets := []string{}
	for _, path := range patch.order {
		if patch.unsets[path] {
			unsets = append(unsets, path)
		}
	}
	return unsets
}

// runConfigHeadless dispatches the explicit configuration modes. None of
// them reads stdin, and none opens the interactive editor.
func runConfigHeadless(c Config, out, stderr io.Writer) int {
	scope := orDefault(c.ConfigScope, configScopeProject)
	if c.ConfigShow {
		return runConfigShow(c, scope, out, stderr)
	}
	patch, err := parseConfigPatch(scope, c.ConfigSets, c.ConfigUnsets)
	if err != nil {
		p := usageFailure("config_assignment_invalid", err.Error())
		p.withLegacy(err.Error(), 2)
		return emitError(c, p, out, stderr)
	}
	if c.Preview {
		return runConfigPreview(c, scope, patch, out, stderr)
	}
	if scope == configScopeMachine {
		return runConfigMachineApply(c, patch, out, stderr)
	}
	return runConfigProjectApply(c, patch, out, stderr)
}

func configCatalogError(err error) *PublicError {
	return stateUnreadableError("", err)
}

func configPatchInvalid(err error) *PublicError {
	p := usageFailure("config_patch_invalid", err.Error())
	p.withLegacy(err.Error(), 2)
	return p
}

// requireConfigContract enforces that the configuration documents are
// requested as contract version 2 when --json is set.
func requireConfigContract(c Config, example string, out, stderr io.Writer) int {
	if c.JSON && c.Contract != ContractVersionV2 {
		p := incompatibleFailure("contract_version_required", "The configuration document is a contract version 2 document.")
		p.Expected = map[string]any{"contract": "v2"}
		p.Observed = map[string]any{"contract": "legacy", "option": "--json"}
		p.Actions = []ErrorAction{{Description: "Request the document with the version 2 contract.", Command: example}}
		return emitError(c, p, out, stderr)
	}
	return -1
}

func emitConfigJSON(c Config, doc any, text string, out io.Writer) int {
	if c.JSON {
		json.NewEncoder(out).Encode(doc)
		return 0
	}
	fmt.Fprint(out, text)
	return 0
}

// runConfigShow reports saved values, effective values, sources, the machine
// catalog, and the controller state. It creates nothing.
func runConfigShow(c Config, scope string, out, stderr io.Writer) int {
	if code := requireConfigContract(c, "looper config --show --contract v2 --json", out, stderr); code >= 0 {
		return code
	}
	machine, err := machineConfigView(c)
	if err != nil {
		return emitError(c, configCatalogError(err), out, stderr)
	}
	doc := configShowDocument{
		configDocumentHead: configDocumentHead{
			ContractVersion: ContractVersionV2, SchemaVersion: configSchemaVersion,
			Kind: configShowKind, GeneratedAt: configNow(), Scope: scope,
		},
		Machine: machine,
	}
	if scope == configScopeMachine {
		doc.Detail = configDetail{Status: "machine settings", Note: "machine scope reads no project state"}
		return emitConfigJSON(c, doc, renderConfigShow(&doc, c), out)
	}
	profile, err := LoadProfile(c.ProfilePath)
	if err != nil {
		return emitError(c, profileUnreadableError(err, c.ProfilePath), out, stderr)
	}
	doc.Saved = savedProfileView(profile)
	doc.Effective = effectiveConfigView(c)
	doc.Controller = controllerConfigView(c)
	doc.Detail = configDetail{Status: "project settings", Note: "saved values apply when the next controller starts"}
	return emitConfigJSON(c, doc, renderConfigShow(&doc, c), out)
}

func renderConfigShow(doc *configShowDocument, c Config) string {
	var b strings.Builder
	if doc.Scope == configScopeMachine {
		fmt.Fprintf(&b, "Machine agent catalog: %s\n", doc.Machine.Path)
		fmt.Fprintf(&b, "Worker default: %s\n", orDefault(doc.Machine.Worker, "(unset)"))
		fmt.Fprintf(&b, "Reviewer default: %s\n", orDefault(doc.Machine.Reviewer, "(unset)"))
		for _, a := range doc.Machine.Agents {
			fmt.Fprintf(&b, "  %s: adapter %s, command %s\n", a.Name, a.Adapter, a.Command)
		}
		if doc.Machine.OmittedHint != "" {
			fmt.Fprintln(&b, doc.Machine.OmittedHint)
		}
		return b.String()
	}
	fmt.Fprintln(&b, "Effective settings:")
	// EffectiveSettings returns no trailing newline; terminate the line so
	// the sentence below starts on its own line.
	fmt.Fprintln(&b, EffectiveSettings(c))
	if doc.Saved == nil {
		fmt.Fprintln(&b, "No saved profile exists.")
	}
	if doc.Controller != nil {
		switch doc.Controller.State {
		case "owning":
			fmt.Fprintf(&b, "Controller session %s owns this repository; submit changes through the request protocol.\n", doc.Controller.Session)
		case "stopped":
			fmt.Fprintln(&b, "No controller owns this repository; saved changes apply at the next start.")
		default:
			fmt.Fprintln(&b, "The controller state is unknown; the metadata could not be read.")
		}
	}
	return b.String()
}

// runConfigPreview validates one patch read-only and reports the exact
// changes, the revision it read, and where the change applies. It submits
// nothing and writes nothing.
func runConfigPreview(c Config, scope string, patch *configPatch, out, stderr io.Writer) int {
	if code := requireConfigContract(c, "looper config --set worker=claude --preview --contract v2 --json", out, stderr); code >= 0 {
		return code
	}
	if scope == configScopeMachine {
		return runConfigMachinePreview(c, patch, out, stderr)
	}
	profile, err := LoadProfile(c.ProfilePath)
	if err != nil {
		return emitError(c, profileUnreadableError(err, c.ProfilePath), out, stderr)
	}
	_, _, outcomes, err := applyProjectPatch(c, profile, patch)
	if err != nil {
		return emitError(c, configPatchInvalid(err), out, stderr)
	}
	revision := profileDigest(c.ProfilePath)
	owned := repositoryOwnedNoCreate(c)
	application := configApplicationView{
		Mode:        "stopped-file",
		Description: "The stopped edit saves the profile under the repository lock and journals the result; it becomes effective when the next controller starts.",
		Command:     configApplyCommand(scope, patch),
	}
	detail := configDetail{Status: "read-only validation; nothing changed"}
	if owned {
		session := ""
		_, dir := projectPaths(c)
		if info, err := readController(dir); err == nil {
			session = info.Session
		}
		application = configApplicationView{
			Mode:        "controller-request",
			Description: "A controller owns this repository; the edit is submitted as a settings request and applies at the next dispatch boundary. --wait returns the decision.",
			Session:     session,
			Command:     configApplyCommand(scope, patch),
		}
		for i := range outcomes {
			if outcomes[i].Outcome == configOutcomeSaved {
				outcomes[i].Outcome = configOutcomeSavedLive
				outcomes[i].Note = configOutcomeNotes[configOutcomeSavedLive]
			}
		}
		detail = configDetail{Status: "read-only validation; nothing submitted"}
	}
	doc := configPreviewDocument{
		configDocumentHead: configDocumentHead{
			ContractVersion: ContractVersionV2, SchemaVersion: configSchemaVersion,
			Kind: configPreviewKind, GeneratedAt: configNow(), Scope: scope,
		},
		Revision: revision, Changes: outcomes, Application: application,
		IfRevisionOK: c.ConfigIfRevision == "" || c.ConfigIfRevision == revision,
		Detail:       detail,
	}
	return emitConfigJSON(c, doc, renderConfigPreview(&doc), out)
}

func runConfigMachinePreview(c Config, patch *configPatch, out, stderr io.Writer) int {
	catalog, err := loadAgentCatalog(orDefault(c.AgentCatalogPath, agentCatalogPath()))
	if err != nil {
		return emitError(c, configCatalogError(err), out, stderr)
	}
	if _, p := checkCatalogRevision(c, catalog.revision); p != nil {
		return emitError(c, p, out, stderr)
	}
	_, outcomes, err := applyMachinePatch(catalog, patch)
	if err != nil {
		return emitError(c, configPatchInvalid(err), out, stderr)
	}
	doc := configPreviewDocument{
		configDocumentHead: configDocumentHead{
			ContractVersion: ContractVersionV2, SchemaVersion: configSchemaVersion,
			Kind: configPreviewKind, GeneratedAt: configNow(), Scope: configScopeMachine,
		},
		Revision: catalog.revision, Changes: outcomes,
		Application: configApplicationView{
			Mode:        "catalog-file",
			Description: "The machine edit is a file transaction under the catalog lock with a revision result; running controllers keep their snapshot until they restart.",
			Command:     configApplyCommand(configScopeMachine, patch),
		},
		IfRevisionOK: c.ConfigIfRevision == "" || c.ConfigIfRevision == catalog.revision,
		Detail:       configDetail{Status: "read-only validation; nothing changed"},
	}
	return emitConfigJSON(c, doc, renderConfigPreview(&doc), out)
}

// checkCatalogRevision enforces the optional --if-revision compare for
// machine edits.
func checkCatalogRevision(c Config, revision string) (bool, *PublicError) {
	if c.ConfigIfRevision == "" || c.ConfigIfRevision == revision {
		return false, nil
	}
	p := operationFailure("revision_mismatch", "The machine agent catalog changed since the revision was read. Reload the catalog and retry.")
	p.Expected = map[string]any{"catalog_revision": c.ConfigIfRevision}
	p.Observed = map[string]any{"catalog_revision": revision}
	p.Retry = retryYes
	p.Actions = []ErrorAction{{Description: "Read the current revision with looper config --scope machine --show --contract v2 --json.", Command: "looper config --scope machine --show --contract v2 --json"}}
	p.withLegacy("The machine agent catalog changed since the revision was read; reload it and retry.", 1)
	return true, p
}

func renderConfigPreview(doc *configPreviewDocument) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Preview (%s scope, revision %s):\n", doc.Scope, orDefault(doc.Revision, "(none)"))
	for _, change := range doc.Changes {
		fmt.Fprintf(&b, "  %s: %v -> %v (%s)\n", change.Field, change.From, change.To, change.Outcome)
	}
	fmt.Fprintf(&b, "%s\n", doc.Application.Description)
	if doc.Application.Command != "" {
		fmt.Fprintf(&b, "Apply with: %s\n", doc.Application.Command)
	}
	return b.String()
}

func configApplyCommand(scope string, patch *configPatch) string {
	parts := []string{"looper", "config"}
	if scope == configScopeMachine {
		parts = append(parts, "--scope", "machine")
	}
	for _, path := range patch.order {
		public := path
		if scope != configScopeMachine {
			public = configProjectPublic[path]
		}
		if patch.unsets[path] {
			parts = append(parts, "--unset", public)
			continue
		}
		value := patch.fields[path]
		if strings.ContainsAny(value, " \t") {
			value = strconv.Quote(value)
		}
		parts = append(parts, "--set", public+"="+value)
	}
	parts = append(parts, "--contract", "v2", "--json")
	return clampJSONString(strings.Join(parts, " "), 1025)
}

// runConfigProjectApply routes one project patch to the live request
// protocol or the stopped journal path.
func runConfigProjectApply(c Config, patch *configPatch, out, stderr io.Writer) int {
	if repositoryOwned(c) {
		return runConfigProjectLive(c, patch, out, stderr)
	}
	return runConfigProjectStopped(c, patch, out, stderr)
}

func runConfigProjectLive(c Config, patch *configPatch, out, stderr io.Writer) int {
	_, dir := projectPaths(c)
	info, err := readController(dir)
	if err != nil {
		p := controllerMetadataError(err)
		p.Message = "A controller owns this repository, but its metadata cannot be verified: " + err.Error() + " Refusing to submit requests without verified ownership."
		p.withLegacy("A controller owns this repository, but its metadata cannot be verified.", 1)
		return emitError(c, p, out, stderr)
	}
	if !controllerAlive(info) {
		p := unavailableFailure("controller_lock_stale", "The recorded controller session is not running; remove the stale lock or restart Looper.")
		p.Observed = map[string]any{"controller_session": info.Session, "controller_process": "not running"}
		p.Actions = []ErrorAction{{Description: "Remove the stale repository lock or restart Looper.", Command: "looper doctor"}}
		p.withLegacy("The recorded controller session is not running; remove the stale lock or restart Looper.", 1)
		return emitError(c, p, out, stderr)
	}
	profile, err := LoadProfile(c.ProfilePath)
	if err != nil {
		return emitError(c, profileUnreadableError(err, c.ProfilePath), out, stderr)
	}
	_, fields, _, err := applyProjectPatch(c, profile, patch)
	if err != nil {
		return emitError(c, configPatchInvalid(err), out, stderr)
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

func runConfigProjectStopped(c Config, patch *configPatch, out, stderr io.Writer) int {
	if c.Wait {
		p := operationFailure("controller_absent", "--wait waits for a controller request, but no controller owns this repository. The stopped edit saves immediately.")
		p.withLegacy("--wait waits for a controller request, but no controller owns this repository.", 2)
		return emitError(c, p, out, stderr)
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

	journalPath := filepath.Join(dir, configJournalName)
	if err := resolveConfigJournal(journalPath, c.ProfilePath); err != nil {
		return emitError(c, err, out, stderr)
	}
	journal, err := loadConfigJournal(journalPath)
	if err != nil {
		return emitError(c, stateUnreadableError(journalPath, err), out, stderr)
	}
	profile, err := LoadProfile(c.ProfilePath)
	if err != nil {
		return emitError(c, profileUnreadableError(err, c.ProfilePath), out, stderr)
	}
	patched, _, outcomes, err := applyProjectPatch(c, profile, patch)
	if err != nil {
		return emitError(c, configPatchInvalid(err), out, stderr)
	}
	if entry := findConfigJournalReplay(journal, c.IdempotencyKey, patch); entry != nil {
		doc := configSavedDocument{
			configDocumentHead: configDocumentHead{
				ContractVersion: ContractVersionV2, SchemaVersion: configSchemaVersion,
				Kind: configSavedKind, GeneratedAt: configNow(), Scope: configScopeProject,
			},
			Revision: profileDigest(c.ProfilePath), Outcomes: outcomes,
			Journal: &configJournalView{ID: entry.ID, State: entry.State},
			Detail: configDetail{
				Status: "idempotent replay; the recorded edit already applied",
				Note:   entry.Result,
			},
		}
		return emitConfigJSON(c, doc, renderConfigSaved(&doc), out)
	}
	if conflict := findConfigJournalConflict(journal, c.IdempotencyKey, patch); conflict != nil {
		p := operationFailure("idempotency_key_conflict", fmt.Sprintf(
			"Idempotency key %s already recorded settings edit %s with different content; submit the changed content under a new key.",
			c.IdempotencyKey, conflict.ID))
		p.Retry = retryAfterChange
		p.Expected = map[string]any{"idempotency_key": clampString(c.IdempotencyKey, maxIdempotencyKeyBytes), "recorded_edit": conflict.ID}
		p.Observed = map[string]any{"patch": configPatchMap(patch), "recorded_patch": conflict.Patch}
		p.Paths = []string{journalPath}
		p.withLegacy("The idempotency key already recorded a different settings edit; use a new key.", 1)
		return emitError(c, p, out, stderr)
	}

	// Validate everything before the first write: the intended bytes are
	// fixed here, so the journal records a digest that a save can match.
	newBytes, err := json.MarshalIndent(patched, "", "  ")
	if err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	newDigest := digest(append(newBytes, '\n'))
	entry := configJournalEntry{
		ID: identifier("cfgset"), Scope: configScopeProject,
		OldDigest: profileDigest(c.ProfilePath), NewDigest: newDigest,
		Patch: configPatchMap(patch), Unsets: configPatchUnsets(patch), Key: c.IdempotencyKey,
		State: "prepared", CreatedAt: configNow(), UpdatedAt: configNow(),
	}
	journal.Entries = append(journal.Entries, entry)
	if err := saveConfigJournal(journalPath, journal); err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	if err := SaveProfile(c.ProfilePath, patched); err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	if actual := profileDigest(c.ProfilePath); actual != newDigest {
		p := operationFailure("profile_save_mismatch", "The saved profile digest does not match the journaled intention; the journal keeps the record.")
		p.Observed = map[string]any{"expected_digest": newDigest, "observed_digest": actual}
		p.Paths = []string{c.ProfilePath, journalPath}
		p.withLegacy("The saved profile digest does not match the journaled intention.", 1)
		return emitError(c, p, out, stderr)
	}
	entry.State, entry.Result, entry.UpdatedAt = "done", "saved", configNow()
	journal.Entries[len(journal.Entries)-1] = entry
	trimConfigJournal(journal)
	if err := saveConfigJournal(journalPath, journal); err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	doc := configSavedDocument{
		configDocumentHead: configDocumentHead{
			ContractVersion: ContractVersionV2, SchemaVersion: configSchemaVersion,
			Kind: configSavedKind, GeneratedAt: configNow(), Scope: configScopeProject,
		},
		Revision: newDigest, Outcomes: outcomes,
		Journal: &configJournalView{ID: entry.ID, State: "done"},
		Detail:  configDetail{Status: "saved", Note: "the changes become effective when the next controller starts"},
	}
	return emitConfigJSON(c, doc, renderConfigSaved(&doc), out)
}

func runConfigMachineApply(c Config, patch *configPatch, out, stderr io.Writer) int {
	path := orDefault(c.AgentCatalogPath, agentCatalogPath())
	catalog, err := loadAgentCatalog(path)
	if err != nil {
		return emitError(c, configCatalogError(err), out, stderr)
	}
	if _, p := checkCatalogRevision(c, catalog.revision); p != nil {
		return emitError(c, p, out, stderr)
	}
	next, outcomes, err := applyMachinePatch(catalog, patch)
	if err != nil {
		return emitError(c, configPatchInvalid(err), out, stderr)
	}
	if err := saveAgentCatalog(path, next); err != nil {
		if errors.Is(err, errCatalogConcurrent) {
			p := operationFailure("catalog_changed", "The machine agent catalog changed while this edit ran. Reload the catalog and retry.")
			p.Retry = retryYes
			p.Observed = map[string]any{"catalog_revision": catalog.revision}
			p.Actions = []ErrorAction{{Description: "Read the current revision and retry the edit.", Command: "looper config --scope machine --show --contract v2 --json"}}
			p.withLegacy("The machine agent catalog changed while this edit ran; reload it and retry.", 1)
			return emitError(c, p, out, stderr)
		}
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	doc := configSavedDocument{
		configDocumentHead: configDocumentHead{
			ContractVersion: ContractVersionV2, SchemaVersion: configSchemaVersion,
			Kind: configSavedKind, GeneratedAt: configNow(), Scope: configScopeMachine,
		},
		Revision: next.revision, Outcomes: outcomes,
		Detail: configDetail{
			Status: "saved",
			Note:   "running controllers keep their snapshot until they restart",
		},
	}
	return emitConfigJSON(c, doc, renderConfigSaved(&doc), out)
}

func renderConfigSaved(doc *configSavedDocument) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Saved %s settings (revision %s).\n", doc.Scope, doc.Revision)
	for _, o := range doc.Outcomes {
		fmt.Fprintf(&b, "  %s: %v -> %v (%s)\n", o.Field, o.From, o.To, o.Outcome)
	}
	if doc.Detail.Note != "" {
		fmt.Fprintf(&b, "%s\n", doc.Detail.Note)
	}
	return b.String()
}
