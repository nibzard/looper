package looper

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Contract versions name the machine-readable document sets defined in
// docs/AGENT-CONTRACTS.md. Version 1 is the frozen legacy shape; version 2
// is the bounded contract the agent-experience plan builds.
const (
	ContractVersionLegacy = 1
	ContractVersionV2     = 2
)

// DefaultContractVersion is what commands emit without an explicit
// selection. It changes only through the default transition recorded in
// docs/AGENT-CONTRACT-MIGRATION.md.
const DefaultContractVersion = ContractVersionLegacy

// capabilitiesSchemaVersion is the schema revision of the capabilities
// document itself.
const capabilitiesSchemaVersion = 2

// capabilitiesKind identifies the capabilities document.
const capabilitiesKind = "looper.capabilities"

// legacyDefaultUntil names the gate after which the default contract
// version changes. It must match the compatibility section of
// docs/AGENT-CONTRACT-MIGRATION.md.
const legacyDefaultUntil = "The AX-027 release gates pass and the release notes name the new default."

// capabilitiesPolicy points callers at the compatibility policy.
const capabilitiesPolicy = "docs/AGENT-CONTRACT-MIGRATION.md"

// clientContractVersions lists the contract versions this executable emits.
var clientContractVersions = []int{ContractVersionLegacy, ContractVersionV2}

// clientCommands is the public command surface this executable parses, in
// help order. docs/contracts/commands.json lists the same names; tests
// cross-check this list, the parser, and the registry.
var clientCommands = []string{
	"run", "start", "tui", "attach", "add-task", "add-plan", "cancel", "amend", "status", "runs",
	"requests", "wait", "stop", "tail", "events", "inspect", "recovery", "doctor", "preflight", "version", "config", "help",
}

// Public request type names, the vocabulary of the capabilities contract.
// They name operations; the internal envelope types in settings.go name the
// transports those operations use. The stop transport is the scoped stop
// file rather than a request envelope, so it has no internal counterpart.
const (
	PublicRequestAddTask   = "add-task"
	PublicRequestAddPlan   = "add-plan"
	PublicRequestSettings  = "settings"
	PublicRequestRequeue   = "requeue"
	PublicRequestStop      = "stop"
	PublicRequestCancel    = "cancel-task"
	PublicRequestAmendTask = "amend-task"
	PublicRequestRecovery  = "recovery"
)

// publicRequestNames maps internal envelope types to their public request
// type names. The plan-import transport keeps its frozen version 1 name.
var publicRequestNames = map[string]string{
	RequestTypeAddTask:    PublicRequestAddTask,
	RequestTypePlanImport: PublicRequestAddPlan,
	RequestTypeSettings:   PublicRequestSettings,
	RequestTypeRequeue:    PublicRequestRequeue,
	RequestTypeCancel:     PublicRequestCancel,
	RequestTypeAmend:      PublicRequestAmendTask,
	RequestTypeRecovery:   PublicRequestRecovery,
}

// controllerRequestTypes returns the request types a controller built from
// this source accepts, in the public vocabulary and stable order.
func controllerRequestTypes() []string {
	return []string{
		PublicRequestAddTask, PublicRequestAddPlan, PublicRequestSettings,
		PublicRequestRequeue, PublicRequestStop, PublicRequestCancel,
		PublicRequestAmendTask, PublicRequestRecovery,
	}
}

// supportedEnvelopeTypes lists the internal request-envelope types a
// controller built from this source claims and answers.
var supportedEnvelopeTypes = map[string]bool{
	RequestTypeAddTask:    true,
	RequestTypePlanImport: true,
	RequestTypeSettings:   true,
	RequestTypeRequeue:    true,
	RequestTypeCancel:     true,
	RequestTypeAmend:      true,
	RequestTypeRecovery:   true,
}

// legacyBaseRequestTypes is the compatibility policy set: the request types
// every controller accepts, including ones whose metadata predates
// capability advertisement. A newer client may submit these to an older
// controller; any other type is refused before publication because the older
// controller could only leave it as a dead letter. The set is frozen: types
// added after the capabilities contract (cancel-task, later amend-task) are
// not part of it, because a pre-contract controller cannot answer them.
var legacyBaseRequestTypes = []string{
	PublicRequestAddTask, PublicRequestAddPlan, PublicRequestSettings,
	PublicRequestRequeue, PublicRequestStop,
}

// newControllerCapabilities returns the capability record this build
// publishes with its controller identity.
func newControllerCapabilities() *ControllerCapabilities {
	versions := make([]int, len(clientContractVersions))
	copy(versions, clientContractVersions)
	return &ControllerCapabilities{
		ContractVersions: versions,
		RequestTypes:     controllerRequestTypes(),
		Limits: map[string]int{
			// Real enforced bounds today: the request envelope size check in
			// loadSettingsRequest and the pending-request bound in
			// sweepRequestQueue. Later tasks add the response budgets they own.
			"request_bytes":    64 << 10,
			"pending_requests": maxPendingRequests,
		},
	}
}

// advertisedRequestTypes reports the request types a live controller accepts
// and whether its metadata carried no capability record. A record from a
// controller older than the capabilities contract is explicit about being
// absent: the documented policy derives the base set rather than guessing.
func advertisedRequestTypes(info *ControllerInfo) ([]string, bool) {
	if info.Capabilities != nil && len(info.Capabilities.RequestTypes) > 0 {
		return info.Capabilities.RequestTypes, false
	}
	return legacyBaseRequestTypes, true
}

// unsupportedRequestError is the specific incompatibility result a client
// returns instead of publishing a request envelope the live controller
// cannot accept. No work is queued.
type unsupportedRequestError struct {
	requestType    string
	session        string
	supported      []string
	absentMetadata bool
}

func (e *unsupportedRequestError) Error() string {
	supported := strings.Join(e.supported, ", ")
	if e.absentMetadata {
		return fmt.Sprintf("the controller session %s predates capability advertisement; it accepts %s requests, and %q is not among them. No request was queued. Restart the controller from a build that supports %q requests.", e.session, supported, e.requestType, e.requestType)
	}
	return fmt.Sprintf("the controller session %s does not accept %q requests; it supports %s. No request was queued.", e.session, e.requestType, supported)
}

// staleSessionError is the actionable outcome for a stale-session race: the
// controller the request targeted no longer owns the repository, so no live
// controller will claim the request.
type staleSessionError struct {
	requestType string
	targeted    string
	current     string
	// raced is true when the session changed after publication, false when
	// publication itself noticed the change.
	raced bool
}

func (e *staleSessionError) Error() string {
	if e.raced {
		return fmt.Sprintf("controller session %s stopped while request %s was pending; the repository is now owned by %s. The request was not applied. Check the current controller with looper status and resubmit against it.", e.targeted, e.requestType, e.current)
	}
	return fmt.Sprintf("controller session %s no longer owns this repository; %s does now. The %s request was not submitted. Resubmit against the current controller.", e.targeted, e.current, e.requestType)
}

// validateControllerRequest checks a public request type against the live
// controller's advertised capabilities before any envelope is published.
// A nil controller record cannot be validated and is allowed through; the
// callers that read metadata decide how to handle an unreadable record.
func validateControllerRequest(info *ControllerInfo, requestType string) error {
	if info == nil {
		return nil
	}
	supported, absent := advertisedRequestTypes(info)
	for _, t := range supported {
		if t == requestType {
			return nil
		}
	}
	return &unsupportedRequestError{
		requestType: requestType, session: info.Session,
		supported: supported, absentMetadata: absent,
	}
}

// validateRequestPublication is the gate every request-envelope writer
// passes just before publishing. It reads live controller metadata and
// refuses, without writing anything, when the controller that would claim
// the envelope cannot: an advertised request-type set without this type, or
// a session that no longer owns the repository. Plan imports are exempt
// from the session check because they survive controller restarts by
// design; the next controller replays them. Metadata that cannot be read
// offers nothing to validate against, so the caller's own ownership checks
// stay authoritative there.
func validateRequestPublication(dir, session, envelopeType string) error {
	public, known := publicRequestNames[envelopeType]
	if !known {
		public = envelopeType
	}
	info, err := readController(dir)
	if err != nil {
		return nil
	}
	if err = validateControllerRequest(info, public); err != nil {
		return err
	}
	if envelopeType != RequestTypePlanImport && info.Session != session {
		return &staleSessionError{requestType: public, targeted: session, current: info.Session}
	}
	return nil
}

// pendingSessionChange reports the session that owns the repository while a
// submitted request waits, when that session is not the one the request
// targets. An unreadable record decides nothing: the wait continues.
func pendingSessionChange(dir, targeted string) (string, bool) {
	info, err := readController(dir)
	if err != nil || info.Session == targeted {
		return "", false
	}
	return info.Session, true
}

// capabilitiesDocument is the machine-readable capability discovery document
// `looper version --contract v2 --json` emits. It validates against
// docs/contracts/schemas/v2/capabilities.schema.json.
type capabilitiesDocument struct {
	ContractVersion int                         `json:"contract_version"`
	SchemaVersion   int                         `json:"schema_version"`
	Kind            string                      `json:"kind"`
	Client          clientCapabilities          `json:"client"`
	Controller      *controllerCapabilitiesView `json:"controller,omitempty"`
	Compatibility   compatibilityNotice         `json:"compatibility"`
}

type clientCapabilities struct {
	Build            BuildInfo `json:"build"`
	ContractVersions []int     `json:"contract_versions"`
	Commands         []string  `json:"commands,omitempty"`
	Waits            []string  `json:"waits,omitempty"`
}

type controllerCapabilitiesView struct {
	Session          string         `json:"session"`
	Build            BuildInfo      `json:"build"`
	ContractVersions []int          `json:"contract_versions"`
	RequestTypes     []string       `json:"request_types"`
	AbsentMetadata   bool           `json:"absent_metadata,omitempty"`
	Limits           map[string]int `json:"limits,omitempty"`
}

type compatibilityNotice struct {
	DefaultContractVersion         int    `json:"default_contract_version"`
	OldestSupportedContractVersion int    `json:"oldest_supported_contract_version"`
	LegacyDefaultUntil             string `json:"legacy_default_until,omitempty"`
	Policy                         string `json:"policy,omitempty"`
}

// capabilitiesDocumentFor builds the capabilities document for this
// executable and, when one claims the project, the live controller.
func capabilitiesDocumentFor(c Config) *capabilitiesDocument {
	doc := &capabilitiesDocument{
		ContractVersion: ContractVersionV2,
		SchemaVersion:   capabilitiesSchemaVersion,
		Kind:            capabilitiesKind,
		Client: clientCapabilities{
			Build: currentBuild(), ContractVersions: clientContractVersions,
			Commands: clientCommands,
			// The wait predicates this executable serves (AX-013), in the
			// vocabulary the schema describes, so a caller learns what it can
			// wait for before it tries.
			Waits: clientWaits,
		},
		Compatibility: compatibilityNotice{
			DefaultContractVersion:         DefaultContractVersion,
			OldestSupportedContractVersion: ContractVersionLegacy,
			LegacyDefaultUntil:             legacyDefaultUntil,
			Policy:                         capabilitiesPolicy,
		},
	}
	_, dir := projectPaths(c)
	doc.Controller = liveControllerCapabilities(dir)
	return doc
}

// liveControllerCapabilities reads the capability record from live
// controller metadata. Metadata that is missing or cannot be verified
// reports no controller section rather than guessing at one; the repository
// lock stays authoritative for ownership.
func liveControllerCapabilities(dir string) *controllerCapabilitiesView {
	info, err := readController(dir)
	if err != nil {
		return nil
	}
	view := &controllerCapabilitiesView{
		Session: info.Session,
		// Unavailable identity stays explicit rather than borrowed from the
		// runtime project repository.
		Build:            unknownControllerBuild,
		ContractVersions: []int{ContractVersionLegacy},
	}
	if info.Build != nil {
		view.Build = *info.Build
	}
	types, absent := advertisedRequestTypes(info)
	view.RequestTypes, view.AbsentMetadata = types, absent
	if !absent {
		view.ContractVersions = info.Capabilities.ContractVersions
		if len(view.ContractVersions) == 0 {
			view.ContractVersions = []int{ContractVersionLegacy}
		}
		view.Limits = info.Capabilities.Limits
	}
	return view
}

// unknownControllerBuild is the placeholder a controller view carries when
// its metadata recorded no build. It never counts as a difference, because
// the controller's real build cannot be read, only missed.
var unknownControllerBuild = BuildInfo{Release: "unknown", Revision: "unknown"}

// printCapabilitiesSummary renders the same facts as the capabilities
// document for plain output.
func printCapabilitiesSummary(c Config, out io.Writer) {
	client := currentBuild()
	fmt.Fprintln(out, client)
	fmt.Fprintf(out, "Contract versions: %s (default %d); policy %s\n",
		joinInts(clientContractVersions), DefaultContractVersion, capabilitiesPolicy)
	_, dir := projectPaths(c)
	view := liveControllerCapabilities(dir)
	if view == nil {
		fmt.Fprintln(out, "Controller: no readable controller metadata; the repository lock decides ownership")
		return
	}
	derived := ""
	if view.AbsentMetadata {
		derived = " (derived from pre-contract metadata)"
	}
	fmt.Fprintf(out, "Controller: session %s, %s, contract %s, requests %s%s\n",
		view.Session, view.Build.String(), joinInts(view.ContractVersions),
		strings.Join(view.RequestTypes, ", "), derived)
	if view.Build != client && view.Build != unknownControllerBuild {
		fmt.Fprintln(out, "Builds differ: the running controller keeps its original build until it restarts.")
	}
}

func joinInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, ", ")
}

// parseContractVersion resolves one contract selection. Accepted spellings
// are the names and numbers of the defined versions; anything else is a
// usage error with the supported range in the message, never a fallback.
func parseContractVersion(v string) (int, error) {
	switch v {
	case "1", "v1", "legacy":
		return ContractVersionLegacy, nil
	case "2", "v2":
		return ContractVersionV2, nil
	}
	p := usageFailure("contract_version_unsupported", fmt.Sprintf("unsupported contract version %q; supported versions: legacy (v1) and v2", v))
	p.Expected = map[string]any{"contract": "legacy (v1) or v2"}
	p.Observed = map[string]any{"contract": clampString(v, 128)}
	return 0, p
}

// Since AX-014 every command negotiates the version 2 error contract, so
// version selection parses everywhere. `version` and a finite `events` query
// additionally serve their version 2 success documents today; other commands
// keep their legacy success document until their owning task lands one, as
// the migration guide records.
