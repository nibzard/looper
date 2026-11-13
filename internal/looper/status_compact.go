package looper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// The compact status contract, docs/contracts/schemas/v2/status-compact.schema.json.
const (
	statusCompactSchemaVersion = 2
	statusCompactKind          = "looper.status.compact"
	// statusBytesBudget is the release gate for routine observation: one
	// compact document stays within 8 KiB even on a large backlog.
	statusBytesBudget = 8192
	// statusQuietAfter matches the doctor's output_quiet threshold. Output
	// older than this reports quiet progress; a worker with no output at all
	// reports unknown progress, because output activity never proves task
	// progress by itself.
	statusQuietAfter = 300 * time.Second
	// statusRouteID bounds an identifier that appears inside a detail command,
	// counted in the bytes the JSON encoder emits. A longer identifier would
	// make the route itself unbounded, so the route is dropped instead.
	statusRouteID = 64
)

// statusFit names the bounds one rendering pass applies. A document that
// exceeds the byte budget renders again with the next fit: strings shrink
// first, and bounded collections drop entries only when shrinking strings is
// not enough. Every string bound counts the bytes the JSON encoder emits, so
// text that escapes heavily — an ampersand becomes six bytes — shrinks with
// the ladder instead of overflowing it. The ladder ends in
// statusTerminalFit, which keeps the identity, counts, and cursor with no
// collections at all and every string a single rune at worst, so a valid
// document always fits whatever the saved records hold.
type statusFit struct {
	stringMax   int
	activeMax   int
	findingsMax int
}

var statusFits = []statusFit{
	{200, 16, 16},
	{120, 12, 8},
	{80, 8, 4},
	{48, 4, 2},
	{32, 2, 1},
	{24, 0, 0},
	statusTerminalFit,
}

// statusTerminalFit is the ladder's last resort. Its string bound leaves
// every emitted string a single rune at worst, so the document it renders is
// identity, counts, and cursor alone — a size fixed by the contract's fields,
// not by how large the saved records grew. The terminal-fit test pins that
// worst case under the byte budget.
var statusTerminalFit = statusFit{stringMax: clampJSONStringMin, activeMax: 0, findingsMax: 0}

// statusCompactDocument answers the five questions a routine observation
// asks: which project, run, controller, and build; whether the controller is
// starting, controlling, draining, stopped, or unknown; which tasks and phases
// are active; what prevents scheduling; and what changed last with the
// position observation continues from. It validates against the published
// schema and stays within statusBytesBudget.
type statusCompactDocument struct {
	ContractVersion int               `json:"contract_version"`
	SchemaVersion   int               `json:"schema_version"`
	Kind            string            `json:"kind"`
	GeneratedAt     string            `json:"generated_at"`
	Project         statusProject     `json:"project"`
	Run             statusRun         `json:"run"`
	Builds          statusBuilds      `json:"builds"`
	Controller      statusController  `json:"controller"`
	Work            statusWork        `json:"work"`
	Waiting         statusWaiting     `json:"waiting"`
	Requests        statusRequests    `json:"requests"`
	Findings        []statusFinding   `json:"findings"`
	LastTransition  *statusTransition `json:"last_transition"`
	Cursor          statusCursor      `json:"cursor"`
	Limits          statusLimits      `json:"limits"`
	Detail          statusDetail      `json:"detail"`
}

type statusProject struct {
	Root     string `json:"root"`
	Name     string `json:"name,omitempty"`
	TodoPath string `json:"todo_path,omitempty"`
}

type statusRun struct {
	ID                string `json:"id"`
	ControllerSession string `json:"controller_session,omitempty"`
	Mode              string `json:"mode"`
	StartedAt         string `json:"started_at,omitempty"`
}

type statusBuild struct {
	Release  string `json:"release"`
	Revision string `json:"revision"`
	Modified bool   `json:"modified"`
}

type statusBuilds struct {
	Client     statusBuild  `json:"client"`
	Controller *statusBuild `json:"controller,omitempty"`
	Match      *bool        `json:"match,omitempty"`
	Note       string       `json:"note,omitempty"`
}

type statusController struct {
	State               string `json:"state"`
	Owned               bool   `json:"owned"`
	OwnershipEvidence   string `json:"ownership_evidence,omitempty"`
	OwnershipAgeSeconds *int   `json:"ownership_age_seconds,omitempty"`
	ViewMatches         *bool  `json:"view_matches,omitempty"`
	ViewAgeSeconds      *int   `json:"view_age_seconds,omitempty"`
}

type statusCounts struct {
	Done       int `json:"done"`
	Todo       int `json:"todo"`
	Blocked    int `json:"blocked"`
	Ready      int `json:"ready"`
	InFlight   int `json:"in_flight,omitempty"`
	Integrated int `json:"integrated,omitempty"`
	// Cancelled counts work cancelled through a checked request (AX-019).
	// Optional so documents without cancellations keep their shape.
	Cancelled int `json:"cancelled,omitempty"`
}

type statusActive struct {
	AttemptID        string `json:"attempt_id"`
	TaskID           string `json:"task_id"`
	Phase            string `json:"phase"`
	Agent            string `json:"agent,omitempty"`
	Progress         string `json:"progress"`
	OutputAgeSeconds *int   `json:"output_age_seconds,omitempty"`
	OutputBytes      int64  `json:"output_bytes,omitempty"`
	Retained         bool   `json:"retained,omitempty"`
}

type statusPhase struct {
	Phase      string `json:"phase"`
	AgeSeconds *int   `json:"age_seconds,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

type statusWork struct {
	Counts statusCounts   `json:"counts"`
	Active []statusActive `json:"active"`
	Phases []statusPhase  `json:"phases,omitempty"`
}

type statusWaiting struct {
	Reason     string `json:"reason"`
	Detail     string `json:"detail,omitempty"`
	AgeSeconds *int   `json:"age_seconds,omitempty"`
}

type statusRequests struct {
	Pending          int  `json:"pending"`
	Waiting          int  `json:"waiting,omitempty"`
	OldestAgeSeconds *int `json:"oldest_age_seconds,omitempty"`
}

type statusFinding struct {
	Code     string             `json:"code"`
	Severity string             `json:"severity"`
	TaskID   string             `json:"task_id,omitempty"`
	Message  string             `json:"message"`
	Action   string             `json:"action,omitempty"`
	Evidence *statusEvidenceRef `json:"evidence,omitempty"`
}

type statusEvidenceRef struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Command   string `json:"command,omitempty"`
}

type statusTransition struct {
	EventID int64  `json:"event_id"`
	Kind    string `json:"kind"`
	Time    string `json:"time"`
	TaskID  string `json:"task_id,omitempty"`
	Message string `json:"message,omitempty"`
}

type statusCursor struct {
	EventID           int64  `json:"event_id"`
	Scope             string `json:"scope"`
	StoreID           string `json:"store_id,omitempty"`
	StoreEpoch        int64  `json:"store_epoch,omitempty"`
	ControllerSession string `json:"controller_session,omitempty"`
}

type statusLimits struct {
	BytesBudget int            `json:"bytes_budget"`
	ActiveMax   int            `json:"active_max,omitempty"`
	FindingsMax int            `json:"findings_max,omitempty"`
	StringMax   int            `json:"string_max,omitempty"`
	Omitted     map[string]int `json:"omitted"`
}

type statusDetail struct {
	Full     string `json:"full,omitempty"`
	Events   string `json:"events,omitempty"`
	Wait     string `json:"wait,omitempty"`
	Inspect  string `json:"inspect,omitempty"`
	Requests string `json:"requests,omitempty"`
}

// statusEvidence is the bounded evidence one compact status renders. Every
// read is a single-row query, a directory listing, or one file probe; building
// it never constructs the timing or usage statistics a full report carries,
// so a compact read stays cheap on a run with a long history.
type statusEvidence struct {
	config   Config
	dir      string
	store    *Store
	snap     *StoreSnapshot
	now      time.Time
	obs      Observation
	info     *ControllerInfo
	view     *ui.ViewSnapshot
	owned    bool
	findings []HealthFinding

	// draining is set by a stop request that targets the live session or by
	// the published view reporting a drain in progress.
	draining bool
	// stopAge is the age of the published stop request, when one exists.
	stopAge *int
	// todoChanged marks a task file whose bytes differ from the saved export
	// digest: an external edit waiting for a boundary import.
	todoChanged bool
	// bootstrap carries the bootstrap agent stream while the controller owns
	// the repository but has saved no backlog yet.
	bootstrap *bootstrapStream
	// lastEvent is the newest durable transition in the journal.
	lastEvent *Event
	// agents names the agent of each active attempt. The observation carries
	// the output evidence; the attempt owns its agent identity.
	agents map[string]string
	// viewAge is the age of the published view file, when one exists.
	viewAge *int

	pendingRequests  int
	waitingImports   int
	oldestRequestAge *int
}

// collectStatusEvidence performs the bounded reads a compact status needs.
func collectStatusEvidence(c Config, store *Store, snap *StoreSnapshot, now time.Time) (*statusEvidence, error) {
	_, dir := projectPaths(c)
	ev := &statusEvidence{config: c, dir: dir, store: store, snap: snap, now: now}
	ev.obs = observe(c, snap.State, snap)
	ev.info, ev.view, ev.owned = ev.obs.Controller, ev.obs.View, ev.obs.ControllerOwned
	if ev.info != nil {
		if req, info, err := readStopRequest(dir); err == nil && req != nil {
			ev.draining = req.targets(ev.info.Session, info)
			if info != nil && !info.ModTime().After(now) {
				age := int(now.Sub(info.ModTime()).Seconds())
				ev.stopAge = &age
			}
		}
	}
	if ev.view != nil && ev.view.Draining {
		ev.draining = true
	}
	if st, err := os.Stat(viewPath(dir)); err == nil && !st.ModTime().After(now) {
		age := int(now.Sub(st.ModTime()).Seconds())
		ev.viewAge = &age
	}
	if bootstrapping := snap.State == nil || snap.State.StartupSeed; bootstrapping {
		if ev.owned {
			// The controller owns the repository but has saved no backlog yet
			// (a detached start seeds a placeholder state before readiness):
			// the bootstrap agent stream is the only liveness evidence.
			ev.bootstrap = latestBootstrap(dir)
		}
	} else if observed := digestFile(c.Todo); observed != "" && observed != snap.State.ExportHash {
		ev.todoChanged = true
	}
	if store != nil {
		last, err := store.LastTransition()
		if err != nil {
			return nil, err
		}
		ev.lastEvent = last
	}
	ev.pendingRequests, ev.waitingImports, ev.oldestRequestAge = pendingRequestSummary(dir, snap.State, now)
	findings := diagnose(ev.obs, snap.State)
	if ev.preBacklog() {
		// A run without a published view is expected while it bootstraps.
		kept := findings[:0]
		for _, f := range findings {
			if f.Code != "view_unavailable" {
				kept = append(kept, f)
			}
		}
		findings = kept
	}
	if snap.State != nil {
		ev.agents = map[string]string{}
		for _, a := range snap.State.Attempts {
			if activeAttempt(a) {
				ev.agents[a.ID] = a.Agent
			}
		}
	}
	ev.findings = orderStatusFindings(findings)
	return ev, nil
}

// agentOf names the agent running one active attempt, empty when unknown.
func (ev *statusEvidence) agentOf(attemptID string) string {
	if ev.agents == nil {
		return ""
	}
	return ev.agents[attemptID]
}

// preBacklog reports whether the run has saved its first backlog yet. A
// detached start seeds a placeholder state so the run is queryable before
// readiness (AX-022); that seed marks the same bootstrap window as no state
// at all, so observation reports bootstrap instead of an empty backlog that
// looks finished.
func (ev *statusEvidence) preBacklog() bool {
	return ev.snap.State == nil || ev.snap.State.StartupSeed
}

// orderStatusFindings puts errors and warnings before informational notes so
// a bounded document keeps the findings a caller must act on. Each severity
// group keeps diagnosis order.
func orderStatusFindings(findings []HealthFinding) []HealthFinding {
	rank := map[string]int{"error": 0, "warning": 1}
	ordered := append([]HealthFinding(nil), findings...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return rank[ordered[i].Severity] < rank[ordered[j].Severity]
	})
	return ordered
}

// importRecords reads the saved import receipts of a state, nil when no
// state exists.
func importRecords(s *State) []*ImportRecord {
	if s == nil {
		return nil
	}
	return s.Imports
}

// pendingRequestSummary counts queued and claimed request envelopes and the
// durable requests still owed an outcome, from the public directory layout
// and the saved receipts. Receipts cover every request type — the plan
// imports among them — and import records saved before the receipt store
// existed keep counting through their own status, so an older run reports
// its waiting imports too. A stopped run reports its envelopes as well:
// they are durable.
func pendingRequestSummary(dir string, s *State, now time.Time) (pending, waiting int, oldest *int) {
	var oldestTime time.Time
	note := func(t time.Time) {
		if !t.IsZero() && (oldestTime.IsZero() || t.Before(oldestTime)) {
			oldestTime = t
		}
	}
	for _, d := range []string{requestDir(dir), processingDir(dir)} {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			pending++
			if info, err := entry.Info(); err == nil {
				note(info.ModTime())
			}
		}
	}
	seen := map[string]bool{}
	for _, r := range requestReceipts(s) {
		if requestStateTerminal(r.State) || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		waiting++
		if submitted, err := time.Parse(time.RFC3339Nano, r.SubmittedAt); err == nil {
			note(submitted)
		}
	}
	for _, r := range importRecords(s) {
		switch r.Status {
		case ImportQueued, ImportWaiting, ImportPlanning:
			if seen[r.RequestID] {
				continue
			}
			seen[r.RequestID] = true
			waiting++
			if enqueued, err := time.Parse(time.RFC3339Nano, r.Enqueued); err == nil {
				note(enqueued)
			}
		}
	}
	if !oldestTime.IsZero() && !oldestTime.After(now) {
		age := int(now.Sub(oldestTime).Seconds())
		oldest = &age
	}
	return pending, waiting, oldest
}

// requestReceipts lists a state's durable request receipts, nil when no
// state exists.
func requestReceipts(s *State) []*RequestRecord {
	if s == nil {
		return nil
	}
	return s.Requests
}

// controllerState resolves the controller state and the evidence that proves
// it. The repository lock is authoritative ownership; metadata locates the
// controller; the published view proves the controller reached its loop.
func controllerState(ev *statusEvidence) (state, evidence string) {
	switch {
	case ev.owned:
		if ev.info == nil {
			// A lock is held but no readable controller metadata exists.
			return "unknown", "lock"
		}
		if !controllerAlive(ev.info) {
			// The lock is held and the recorded process is gone: the owner
			// cannot be identified from this evidence.
			return "unknown", "lock"
		}
		if ev.draining {
			return "draining", "lock"
		}
		if ev.view == nil {
			// No published view yet: the controller is starting or
			// bootstrapping its first backlog.
			return "starting", "lock"
		}
		return "controlling", "lock"
	case ev.info != nil:
		if controllerAlive(ev.info) {
			// Live metadata without the lock contradicts itself; the lock
			// decides, and the disagreement stays visible.
			return "unknown", "metadata"
		}
		// Metadata a dead controller left behind: the run is stopped and the
		// record is stale.
		return "stopped", "metadata"
	default:
		return "stopped", "none"
	}
}

// runIdentifier names the run. The saved ref is the durable identity; before
// the first backlog exists the controller session is the only identity, and
// unknown stays explicit when neither is available.
func runIdentifier(s *State, info *ControllerInfo) string {
	if s != nil && s.Ref != "" {
		if id := strings.TrimPrefix(s.Ref, "refs/looper/"); id != "" {
			return id
		}
		return strings.TrimPrefix(s.Ref, "refs/")
	}
	if info != nil && info.Session != "" {
		return info.Session
	}
	return "unknown"
}

// compactStatus builds the compact status document and renders it within the
// byte budget. The returned document is always valid JSON at or below
// statusBytesBudget.
func compactStatus(c Config, store *Store, snap *StoreSnapshot) (*statusCompactDocument, error) {
	ev, err := collectStatusEvidence(c, store, snap, time.Now())
	if err != nil {
		return nil, err
	}
	var doc *statusCompactDocument
	for _, fit := range statusFits {
		doc = renderStatusDocument(ev, fit)
		if statusEncoded(doc) <= statusBytesBudget {
			return doc, nil
		}
	}
	return doc, nil
}

// statusEncoded reports the size of the document as the CLI emits it,
// including the trailing newline the encoder writes.
func statusEncoded(doc *statusCompactDocument) int {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(doc)
	return buf.Len()
}

// renderStatusDocument renders the evidence into one compact document under
// the given bounds. Every string bound counts the bytes the JSON encoder
// emits, so escape-heavy saved content shrinks with the ladder. Plain text
// renders from the same document, so the two outputs cannot disagree.
func renderStatusDocument(ev *statusEvidence, fit statusFit) *statusCompactDocument {
	clamp := func(s string) string { return clampJSONString(s, fit.stringMax) }
	doc := &statusCompactDocument{
		ContractVersion: ContractVersionV2,
		SchemaVersion:   statusCompactSchemaVersion,
		Kind:            statusCompactKind,
		GeneratedAt:     ev.now.UTC().Format(time.RFC3339),
	}
	// Identity: project and run.
	root, todo := ev.config.Root, ev.config.Todo
	if ev.snap.State != nil {
		if ev.snap.State.Root != "" {
			root = ev.snap.State.Root
		}
		if ev.snap.State.Todo != "" {
			todo = ev.snap.State.Todo
		}
	}
	doc.Project = statusProject{Root: clamp(root), Name: clamp(filepath.Base(root)), TodoPath: clamp(todo)}
	doc.Run = statusRun{ID: clamp(runIdentifier(ev.snap.State, ev.info)), Mode: "unknown"}
	if ev.info != nil {
		doc.Run.ControllerSession = clamp(ev.info.Session)
		doc.Run.Mode = ev.info.Mode
		doc.Run.StartedAt = clamp(ev.info.Started)
	}
	switch doc.Run.Mode {
	case ControllerModeForeground, ControllerModeDetached:
	default:
		doc.Run.Mode = "unknown"
	}
	// Builds: the client executable and, when one claims the project, the
	// controller's own build. A difference is expected, not an error.
	doc.Builds = statusBuilds{Client: statusBuildOf(ev.obs.Executable, clamp)}
	if ev.info != nil && ev.info.Build != nil && ev.info.Build.Release != "" {
		controller := statusBuildOf(*ev.info.Build, clamp)
		doc.Builds.Controller = &controller
		match := *ev.info.Build == ev.obs.Executable
		doc.Builds.Match = &match
		if !match {
			doc.Builds.Note = "The running controller keeps its original build until it restarts."
		}
	}
	// Ownership and freshness.
	state, evidence := controllerState(ev)
	doc.Controller = statusController{State: state, Owned: ev.owned, OwnershipEvidence: evidence}
	if ev.info != nil {
		if started, err := time.Parse(time.RFC3339Nano, ev.info.Started); err == nil && !started.After(ev.now) {
			age := int(ev.now.Sub(started).Seconds())
			doc.Controller.OwnershipAgeSeconds = &age
		}
	}
	if ev.view != nil && ev.info != nil {
		viewMatches := ev.owned && ev.view.Session == ev.info.Session
		doc.Controller.ViewMatches = &viewMatches
		doc.Controller.ViewAgeSeconds = ev.viewAge
	}
	// Work: counts, a bounded active list, and the controller's own phases.
	doc.Work = statusWork{Counts: statusWorkCounts(ev), Active: []statusActive{}}
	doc.Work.Active = append(doc.Work.Active, statusActiveWorkers(ev, fit)...)
	doc.Work.Phases = statusPhases(ev, clamp)
	// Obstruction and motion.
	doc.Waiting = statusWaitingSummary(ev, doc.Work.Counts, clamp)
	doc.Requests = statusRequestsSummary(ev)
	doc.Findings = statusFindingsFor(ev, fit)
	doc.LastTransition = statusTransitionOf(ev, clamp)
	doc.Cursor = statusCursor{
		EventID: ev.snap.HighWater, Scope: "store",
		StoreID: clamp(ev.snap.StoreID), StoreEpoch: ev.snap.Epoch,
	}
	if doc.Cursor.StoreEpoch < 1 {
		doc.Cursor.StoreEpoch = 0 // omitted: an unknown epoch stays unstated
	}
	doc.Limits = statusLimits{
		BytesBudget: statusBytesBudget, ActiveMax: fit.activeMax,
		FindingsMax: fit.findingsMax, StringMax: fit.stringMax,
		Omitted: map[string]int{
			"active_work": max(0, len(ev.obs.Workers)-fit.activeMax),
			"findings":    max(0, len(ev.findings)-fit.findingsMax),
		},
	}
	// Detail routes name commands that exist today.
	doc.Detail = statusDetail{Full: "looper status --contract v2 --full"}
	doc.Detail.Events = fmt.Sprintf("looper events --contract v2 --after %d", ev.snap.HighWater)
	// The finite wait replaces repeated status polling: the same cursor this
	// document reports bounds one bounded call that returns when something
	// changes (AX-013).
	doc.Detail.Wait = fmt.Sprintf("looper wait --contract v2 --cursor %d --timeout 30s", ev.snap.HighWater)
	if task := statusInspectTarget(ev); task != "" {
		doc.Detail.Inspect = "looper inspect " + task + " --json"
	}
	// Pending work names its own route: the bounded request query reads each
	// record's state, wait reason, and outcome evidence (AX-010).
	if ev.pendingRequests > 0 || ev.waitingImports > 0 {
		doc.Detail.Requests = "looper requests --contract v2 --json"
	}
	return doc
}

func statusBuildOf(b BuildInfo, clamp func(string) string) statusBuild {
	return statusBuild{Release: clamp(b.Release), Revision: clamp(b.Revision), Modified: b.Modified}
}

// statusWorkCounts summarizes the backlog. Markers never count. A task that is
// neither done, blocked, nor cancelled counts as todo, including work
// currently in progress, so done+todo+blocked+cancelled is the whole backlog.
func statusWorkCounts(ev *statusEvidence) statusCounts {
	counts := statusCounts{}
	if ev.snap.State == nil {
		return counts
	}
	s := ev.snap.State
	for _, t := range s.Backlog.Tasks {
		if t.Marker() {
			continue
		}
		switch t.Status {
		case "done":
			counts.Done++
		case "blocked":
			counts.Blocked++
		case "cancelled":
			counts.Cancelled++
		default:
			counts.Todo++
		}
	}
	counts.Integrated = s.Integrated
	claimed := map[string]bool{}
	for _, a := range s.Attempts {
		switch a.Phase {
		case "verifying", "ready":
			// Checking work and queued results are visible as active entries.
			claimed[a.Task.ID] = true
		case "done", "blocked", "cancelled":
		default:
			claimed[a.Task.ID] = true
			counts.InFlight++
		}
	}
	// Ready counts work a controller could dispatch now: the ready set minus
	// the tasks an active attempt already claimed.
	for _, t := range s.Backlog.Ready() {
		if !claimed[t.ID] {
			counts.Ready++
		}
	}
	return counts
}

// statusActiveWorkers renders the newest active work first, because a bounded
// list that kept the oldest entries would hide exactly the work an operator
// waits on.
func statusActiveWorkers(ev *statusEvidence, fit statusFit) []statusActive {
	out := []statusActive{}
	workers := ev.obs.Workers
	for i := len(workers) - 1; i >= 0 && len(out) < fit.activeMax; i-- {
		w := workers[i]
		entry := statusActive{
			AttemptID: clampJSONString(w.AttemptID, fit.stringMax),
			TaskID:    clampJSONString(w.TaskID, fit.stringMax),
			Phase:     clampJSONString(w.Phase, fit.stringMax),
			Agent:     clampJSONString(ev.agentOf(w.AttemptID), fit.stringMax),
			Progress:  "unknown", Retained: w.Retained,
		}
		// Output activity is evidence about output, not about task progress:
		// unknown stays unknown, recent output is advancing output, and stale
		// output is quiet output.
		if w.OutputAge != nil {
			age := int(*w.OutputAge)
			entry.OutputAgeSeconds = &age
			entry.Progress = "advancing"
			if *w.OutputAge > int64(statusQuietAfter.Seconds()) {
				entry.Progress = "quiet"
			}
		}
		entry.OutputBytes = w.OutputBytes
		out = append(out, entry)
	}
	return out
}

// statusPhases reports the controller's own synchronous phases, which block
// the run loop: planning, review, diagnosis, recovery, an input correction
// wait, or the bootstrap pass before the first backlog exists.
func statusPhases(ev *statusEvidence, clamp func(string) string) []statusPhase {
	if ev.preBacklog() {
		phase := statusPhase{Phase: "bootstrap", Detail: clamp("reading project requirements to create the first backlog")}
		if ev.info != nil && ev.info.BootstrapAgent != "" {
			phase.Detail = clamp("reading project requirements with the " + ev.info.BootstrapAgent + " agent")
		}
		if ev.bootstrap != nil {
			age := int(ev.bootstrap.idle.Seconds())
			phase.AgeSeconds = &age
		}
		return []statusPhase{phase}
	}
	if ev.view == nil || ev.view.State == "" {
		return nil
	}
	phase := statusPhase{Phase: clamp(ev.view.State), Detail: clamp(ev.view.ActivityDetail)}
	if ev.view.ActivityUpdated != "" {
		if updated, err := time.Parse(time.RFC3339, ev.view.ActivityUpdated); err == nil && !updated.After(ev.now) {
			age := int(ev.now.Sub(updated).Seconds())
			phase.AgeSeconds = &age
		}
	}
	return []statusPhase{phase}
}

// statusWaitingSummary names the machine-stable reason nothing is being
// scheduled right now, in the order the run loop itself applies them.
func statusWaitingSummary(ev *statusEvidence, counts statusCounts, clamp func(string) string) statusWaiting {
	switch {
	case !ev.owned:
		detail := "No controller owns the repository."
		if counts.InFlight > 0 {
			detail = fmt.Sprintf("No controller owns the repository; %d attempt(s) are retained for the next run.", counts.InFlight)
		}
		return statusWaiting{Reason: "controller_stopped", Detail: clamp(detail)}
	case ev.preBacklog():
		return statusWaiting{Reason: "bootstrap", Detail: clamp("The controller is reading project requirements; the first backlog is not saved yet.")}
	case ev.draining:
		return statusWaiting{Reason: "draining", Detail: clamp("A stop request is draining active work before the controller stops."), AgeSeconds: ev.stopAge}
	case ev.view != nil && ev.view.State == "waiting":
		waiting := statusWaiting{Reason: "input_correction", Detail: clamp(ev.view.ActivityDetail)}
		if updated, err := time.Parse(time.RFC3339, ev.view.ActivityUpdated); err == nil && !updated.After(ev.now) {
			age := int(ev.now.Sub(updated).Seconds())
			waiting.AgeSeconds = &age
		}
		return waiting
	case ev.todoChanged:
		return statusWaiting{Reason: "backlog_hold", Detail: clamp("The task file changed outside Looper; the edit imports when active work reaches a boundary.")}
	case ev.snap.State.Plan == nil && (counts.InFlight > 0 || counts.Ready > 0):
		return statusWaiting{Reason: "plan_missing", Detail: clamp("Work waits for a planning pass to rebuild the execution plan.")}
	case ev.snap.State.ReviewPending:
		return statusWaiting{Reason: "review_pending", Detail: clamp("A milestone review runs before more work dispatches.")}
	case counts.InFlight > 0:
		return statusWaiting{Reason: "active_work", Detail: clamp(fmt.Sprintf("%d attempt(s) in flight; scheduling waits for a free worker slot and the integration chain.", counts.InFlight))}
	case counts.Ready == 0:
		return statusWaiting{Reason: "no_ready_task", Detail: clamp("No runnable tasks remain; waiting for changed requirements or resolved blockers.")}
	default:
		return statusWaiting{Reason: "scheduling", Detail: clamp(fmt.Sprintf("%d task(s) are ready; the controller is scheduling them.", counts.Ready))}
	}
}

func statusRequestsSummary(ev *statusEvidence) statusRequests {
	return statusRequests{Pending: ev.pendingRequests, Waiting: ev.waitingImports, OldestAgeSeconds: ev.oldestRequestAge}
}

func statusFindingsFor(ev *statusEvidence, fit statusFit) []statusFinding {
	out := []statusFinding{}
	for _, f := range ev.findings {
		if len(out) >= fit.findingsMax {
			break
		}
		entry := statusFinding{
			Code: clampJSONString(f.Code, fit.stringMax), Severity: f.Severity,
			TaskID: clampJSONString(f.TaskID, fit.stringMax), Message: clampJSONString(f.Message, fit.stringMax),
			Action: clampJSONString(f.Action, fit.stringMax),
		}
		if entry.Severity != "info" && entry.Severity != "warning" && entry.Severity != "error" {
			entry.Severity = "info"
		}
		// A task-scoped finding names the read-only route into its record.
		// An identifier too long for a route leaves the finding readable
		// without one.
		if statusRouteIDOk(f.TaskID) {
			entry.Evidence = &statusEvidenceRef{
				Kind: "file", Reference: clampJSONString("task record "+f.TaskID, fit.stringMax),
				Command: "looper inspect " + f.TaskID + " --json",
			}
		}
		out = append(out, entry)
	}
	return out
}

// statusTransitionOf renders the newest durable transition from the journal,
// with the structured task identity the recorded payload carries. Every
// recorded field is clamped: the journal holds content a caller cannot vouch
// for, and the byte budget must hold for any of it.
func statusTransitionOf(ev *statusEvidence, clamp func(string) string) *statusTransition {
	if ev.lastEvent == nil || ev.lastEvent.ID < 1 {
		return nil
	}
	return &statusTransition{
		EventID: ev.lastEvent.ID, Kind: clamp(ev.lastEvent.Kind), Time: clamp(ev.lastEvent.Time),
		TaskID: clamp(eventTaskIdentity(ev.lastEvent.Data)), Message: clamp(ev.lastEvent.Message),
	}
}

// eventTaskIdentity reads the structured task identity from a recorded event
// payload. Message text never decides an identity.
func eventTaskIdentity(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var payload struct {
		TaskID string `json:"task_id"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return ""
	}
	return payload.TaskID
}

// statusRouteIDOk reports whether an identifier is short enough to embed in
// an executable route command, counting the bytes the JSON encoder emits so
// an escape-heavy identifier cannot turn the route itself unbounded.
func statusRouteIDOk(id string) bool { return id != "" && jsonEscapedLen(id) <= statusRouteID }

// statusInspectTarget names one task a caller can inspect next: the newest
// active work, else the first blocked task.
func statusInspectTarget(ev *statusEvidence) string {
	for i := len(ev.obs.Workers) - 1; i >= 0; i-- {
		if statusRouteIDOk(ev.obs.Workers[i].TaskID) {
			return ev.obs.Workers[i].TaskID
		}
	}
	if ev.snap.State != nil {
		for _, t := range ev.snap.State.Backlog.Tasks {
			if t.Status == "blocked" && statusRouteIDOk(t.ID) {
				return t.ID
			}
		}
	}
	return ""
}

// renderCompactStatus renders the same summary as text. A piped plain status
// always shows the controller state, the active work, and the reason for
// waiting, because a caller piping output has no dashboard to fall back on.
func renderCompactStatus(doc *statusCompactDocument, statePath string, out io.Writer) {
	line := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
	line("Controller: %s", doc.Controller.State)
	switch doc.Controller.OwnershipEvidence {
	case "lock":
		detail := "owns the repository"
		if doc.Controller.OwnershipAgeSeconds != nil {
			detail += fmt.Sprintf(" for %s", statusDuration(*doc.Controller.OwnershipAgeSeconds))
		}
		if doc.Run.ControllerSession != "" {
			detail += " | session " + doc.Run.ControllerSession
		}
		line("  %s", detail)
	case "metadata":
		detail := "no owner; metadata left by an earlier controller is stale"
		if doc.Run.ControllerSession != "" {
			detail += " | session " + doc.Run.ControllerSession
		}
		line("  %s", detail)
	default:
		line("  no owner and no controller metadata")
	}
	if doc.Controller.ViewMatches != nil {
		if *doc.Controller.ViewMatches {
			line("  view matches | published %s ago", statusDuration(*doc.Controller.ViewAgeSeconds))
		} else {
			line("  view does not match the controller | published %s ago", statusDuration(orZero(doc.Controller.ViewAgeSeconds)))
		}
	}
	if doc.Builds.Controller != nil && doc.Builds.Match != nil && !*doc.Builds.Match {
		line("Builds: client %s (%s), controller %s (%s)", doc.Builds.Client.Release, doc.Builds.Client.Revision, doc.Builds.Controller.Release, doc.Builds.Controller.Revision)
		line("  %s", doc.Builds.Note)
	}
	total := doc.Work.Counts.Done + doc.Work.Counts.Todo + doc.Work.Counts.Blocked + doc.Work.Counts.Cancelled
	line("Backlog: %d/%d complete | %d ready | %d blocked | %d in flight", doc.Work.Counts.Done, total, doc.Work.Counts.Ready, doc.Work.Counts.Blocked, doc.Work.Counts.InFlight)
	if doc.Work.Counts.Cancelled > 0 {
		line("  %d cancelled (open work withdrawn through checked requests)", doc.Work.Counts.Cancelled)
	}
	if len(doc.Work.Active) == 0 {
		line("Active: none")
	} else {
		line("Active: %d shown", len(doc.Work.Active))
		if omitted := doc.Limits.Omitted["active_work"]; omitted > 0 {
			line("  %d more active attempt(s) not shown; read them with: looper status --contract v2 --full", omitted)
		}
		for _, a := range doc.Work.Active {
			output := "no output yet"
			if a.OutputAgeSeconds != nil {
				output = fmt.Sprintf("output %s ago, %d bytes", statusDuration(*a.OutputAgeSeconds), a.OutputBytes)
			}
			agent := a.Agent
			if agent != "" {
				agent = " (" + agent + ")"
			}
			retained := ""
			if a.Retained {
				retained = " | retained"
			}
			line("  %s %s%s | %s | progress %s%s", a.TaskID, a.Phase, agent, output, a.Progress, retained)
		}
	}
	for _, p := range doc.Work.Phases {
		age := ""
		if p.AgeSeconds != nil {
			// The age is the time since the agent last wrote output, not the
			// phase duration: a review can run for minutes with fresh output.
			age = fmt.Sprintf(" | last output %s ago", statusDuration(*p.AgeSeconds))
		}
		line("Controller phase: %s%s | %s", p.Phase, age, p.Detail)
	}
	if doc.Waiting.Detail != "" {
		line("Waiting: %s | %s", doc.Waiting.Reason, doc.Waiting.Detail)
	} else {
		line("Waiting: %s", doc.Waiting.Reason)
	}
	if doc.Requests.Pending > 0 || doc.Requests.Waiting > 0 {
		line("Requests: %d queued | %d waiting", doc.Requests.Pending, doc.Requests.Waiting)
		if doc.Detail.Requests != "" {
			line("  Detail: %s", doc.Detail.Requests)
		}
	}
	if len(doc.Findings) > 0 {
		line("Findings: %d shown of %d", len(doc.Findings), len(doc.Findings)+doc.Limits.Omitted["findings"])
		for _, f := range doc.Findings {
			task := ""
			if f.TaskID != "" {
				task = " " + f.TaskID
			}
			line("  %s%s: %s %s", f.Severity, task, f.Code, f.Message)
		}
	} else if doc.Limits.Omitted["findings"] > 0 {
		line("Findings: none shown of %d", doc.Limits.Omitted["findings"])
	}
	if doc.LastTransition != nil {
		line("Last transition: event %d | %s | %s", doc.LastTransition.EventID, doc.LastTransition.Kind, doc.LastTransition.Time)
	}
	line("Cursor: %d | store %s | continue with: %s", doc.Cursor.EventID, orUnknown(doc.Cursor.StoreID), doc.Detail.Events)
	line("Wait for the next change with: %s", doc.Detail.Wait)
	line("State: %s", statePath)
	line("Detail: %s", doc.Detail.Full)
}

func orZero(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func orUnknown(s string) string {
	if s == "" {
		return "identity not recorded"
	}
	return s
}

// statusDuration renders an age briefly for plain output.
func statusDuration(seconds int) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh", seconds/3600)
	default:
		return fmt.Sprintf("%dd", seconds/86400)
	}
}
