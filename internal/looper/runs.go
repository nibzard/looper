package looper

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The run discovery contract, docs/contracts/schemas/v2/runs.schema.json.
const (
	runsSchemaVersion = 2
	runsKind          = "looper.runs"
	// runsBytesBudget bounds one listing document. The listing is a decision
	// basis — which saved run to observe, and how — not a dump: collections
	// stay small, the bounds that applied are stated, and what was omitted is
	// counted.
	runsBytesBudget = 8192
	// runsDefaultLimit is the default page size of the listing.
	runsDefaultLimit = 20
	// maxRunsPageLimit is the largest run page --limit may request.
	maxRunsPageLimit = 100
	// runsScanMax bounds the base directory scan. A base directory with more
	// entries reports the truncation instead of reading without bound.
	runsScanMax = 4096
	// runsRetainedMax bounds the retained task identifiers one entry shows
	// before the fit ladder shrinks them further.
	runsRetainedMax = 8
	// runsSelectMax bounds the selection hints one entry carries.
	runsSelectMax = 2
)

// runsDirName matches the Looper-owned runtime directory names projectPaths
// derives from a project root: a slug of the root's base name, a dash, and
// the first eight hex digits of the root's digest. Nothing else under the
// base directory is a run, so nothing else is read.
var runsDirName = regexp.MustCompile(`^[A-Za-z0-9._-]*-[0-9a-f]{8}$`)

// runsFit names the bounds one rendering pass applies. A document over the
// byte budget renders again with the next fit: strings shrink first and the
// run page drops entries only when that is not enough. Every string bound
// counts the bytes the JSON encoder emits, so escape-heavy identities — an
// ampersand becomes six bytes — shrink with the ladder instead of expanding
// past the fit that stated them. The ladder ends in runsTerminalFit, which
// keeps the newest run entry alone with every string a single rune at worst,
// so a valid document always fits whatever the saved records hold.
type runsFit struct {
	stringMax   int
	runsMax     int
	retainedMax int
}

// runsTerminalFit is the ladder's last resort. Its string bound leaves every
// emitted string a single rune at worst and its page keeps only the newest
// run with one retained identifier, so the document it renders is structure
// alone — a size fixed by the contract's fields, not by how escape-heavy the
// recorded identities grew. The runs terminal-fit test pins that worst case
// under the byte budget and against the published schema, whose
// retained_ids_max minimum of one the fit respects.
var runsTerminalFit = runsFit{stringMax: clampJSONStringMin, runsMax: 1, retainedMax: 1}

// runsFitsFor builds the fit ladder for one requested page. The first fits
// honor the requested page; the later ones trade entry count and string size
// for the byte budget, so a larger request still renders within it.
func runsFitsFor(limit int) []runsFit {
	return []runsFit{
		{256, limit, runsRetainedMax},
		{160, limit, 4},
		{96, min(limit, 12), 2},
		{64, min(limit, 6), 1},
		{48, min(limit, 3), 1},
		runsTerminalFit,
	}
}

// runsDocument is the one JSON document `looper runs --contract v2 --json`
// returns: the saved runs found within the resolved configured base
// directory, with each run's project root, retained work, session count and
// last known outcome, controller ownership freshness, recorded build
// evidence, and the exact command that selects it. Discovery is read-only —
// no run is started, stopped, or mutated — and reads only Looper-owned
// runtime directories; private agent session directories are never scanned.
type runsDocument struct {
	ContractVersion int          `json:"contract_version"`
	SchemaVersion   int          `json:"schema_version"`
	Kind            string       `json:"kind"`
	GeneratedAt     string       `json:"generated_at"`
	Project         runsProject  `json:"project"`
	Searched        runsSearched `json:"searched"`
	Runs            []runsEntry  `json:"runs"`
	RunTotal        int          `json:"run_total"`
	Limits          runsLimits   `json:"limits"`
	Detail          runsDetail   `json:"detail"`
}

// runsProject names the invocation's own resolved locations: the project
// root and task file the caller operates on, the resolved base directory,
// and the runtime directory this project maps to, so a caller can tell a
// foreign run from its own even before reading entries.
type runsProject struct {
	Root            string `json:"root"`
	TodoPath        string `json:"todo_path"`
	BaseDir         string `json:"base_dir"`
	RuntimeDir      string `json:"runtime_dir"`
	CurrentRunFound bool   `json:"current_run_found"`
	CurrentRunState string `json:"current_run_state"` // saved, missing, unreadable, or none
}

type runsSearched struct {
	BaseDir        string `json:"base_dir"`
	EntriesScanned int    `json:"entries_scanned"`
	EntriesSkipped int    `json:"entries_skipped"`
	ScanTruncated  bool   `json:"scan_truncated,omitempty"`
	ScanNote       string `json:"scan_note"`
}

type runsEntry struct {
	Name              string            `json:"name"`
	Directory         string            `json:"directory"`
	RuntimeDir        string            `json:"runtime_dir"`
	State             string            `json:"state"` // saved, missing, or unreadable
	StateError        string            `json:"state_error,omitempty"`
	CurrentProject    bool              `json:"current_project,omitempty"`
	RunID             string            `json:"run_id,omitempty"`
	Root              string            `json:"root,omitempty"`
	RootExists        *bool             `json:"root_exists,omitempty"`
	TodoPath          string            `json:"todo_path,omitempty"`
	Head              string            `json:"head,omitempty"`
	Branch            string            `json:"branch,omitempty"`
	Retained          bool              `json:"retained"`
	RetainedTasks     []string          `json:"retained_tasks"`
	RetainedTaskTotal int               `json:"retained_task_total"`
	TasksTotal        int               `json:"tasks_total"`
	TasksDone         int               `json:"tasks_done"`
	SessionsTotal     int               `json:"sessions_total"`
	LastSession       runsSessionRef    `json:"last_session"`
	LastActivity      string            `json:"last_activity"`
	Controller        runsControllerRef `json:"controller"`
	Select            []string          `json:"select"`
}

// runsSessionRef is the last known outcome of the newest recorded controller
// session. Outcome stays the value the store recorded — running, stopped,
// failed, cancelled, panicked, or interrupted — and unknown when the store
// predates session records.
type runsSessionRef struct {
	ID       string `json:"id,omitempty"`
	Started  string `json:"started,omitempty"`
	Ended    string `json:"ended,omitempty"`
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// runsControllerRef is the ownership freshness evidence of one run:
// controller metadata plus process liveness. owning means the recorded
// controller process is alive; stale means metadata names a process that is
// gone; stopped means no metadata remains (a clean shutdown removes its own
// record); unknown means the metadata could not be read.
type runsControllerRef struct {
	State      string     `json:"state"`
	Session    string     `json:"session,omitempty"`
	Mode       string     `json:"mode,omitempty"`
	Started    string     `json:"started,omitempty"`
	AgeSeconds *int64     `json:"age_seconds,omitempty"`
	Build      *BuildInfo `json:"build,omitempty"`
	Error      string     `json:"error,omitempty"`
}

type runsLimits struct {
	BytesBudget    int            `json:"bytes_budget"`
	PageLimit      int            `json:"page_limit"`
	RunsMax        int            `json:"runs_max"`
	StringMax      int            `json:"string_max"`
	RetainedIDsMax int            `json:"retained_ids_max"`
	Omitted        map[string]int `json:"omitted"`
}

type runsDetail struct {
	Run  string `json:"run"`
	Page string `json:"page"`
}

// runsRecord is one run's collected evidence, before the fit bounds clamp it
// into a document entry.
type runsRecord struct {
	name       string
	directory  string
	runtimeDir string
	activity   time.Time

	state      string // saved, missing, or unreadable
	stateError error

	runID       string
	root        string
	todo        string
	head        string
	branch      string
	retained    bool
	retainedIDs []string
	retainedTtl int
	tasksTtl    int
	tasksDone   int

	sessionsTtl int
	newest      *runSessionRow
	shutdown    *sessionExit
	sessionInfo *ControllerInfo // newest session event's identity: build evidence

	controllerState string
	controllerErr   error
	info            *ControllerInfo
	infoAge         time.Duration

	rootExists bool
	current    bool
}

// runSessionRow is one row of the store's sessions table.
type runSessionRow struct {
	id, started, ended, outcome string
}

// runsEvidence is everything the listing read: every Looper-owned runtime
// directory under the resolved base directory and what the scan skipped.
// Every read is read-only; nothing is created, written, or removed.
type runsEvidence struct {
	c Config

	base          string
	expectedName  string
	expectedRunAt string

	records []*runsRecord
	scanned int
	skipped int
	trunc   bool

	currentFound bool
	currentState string

	now time.Time
}

// collectRunsEvidence scans the resolved base directory for saved runs. The
// scan reads only directories whose names projectPaths could have derived
// from a project root and that hold a runtime subdirectory; everything else
// is counted as skipped and never opened. Private agent session directories
// outside the base directory are never touched.
func collectRunsEvidence(c Config, now time.Time) (*runsEvidence, error) {
	logs, runtime := projectPaths(c)
	ev := &runsEvidence{
		c: c, base: c.BaseDir, expectedName: filepath.Base(logs),
		expectedRunAt: runtime, now: now, currentState: "none",
	}
	entries, err := os.ReadDir(c.BaseDir)
	if err != nil {
		return nil, err
	}
	read := entries
	if len(read) > runsScanMax {
		read = read[:runsScanMax]
		ev.trunc = true
	}
	ev.scanned = len(read)
	for _, entry := range read {
		name := entry.Name()
		if !entry.IsDir() || !runsDirName.MatchString(name) {
			ev.skipped++
			continue
		}
		runtime := filepath.Join(c.BaseDir, name, "runtime")
		if info, err := os.Stat(runtime); err != nil || !info.IsDir() {
			ev.skipped++
			continue
		}
		rec := collectRunRecord(name, runtime, now)
		rec.current = name == ev.expectedName
		ev.records = append(ev.records, rec)
	}
	// The newest run first, so the bounded page carries the runs a caller is
	// most likely to act on.
	sort.SliceStable(ev.records, func(i, j int) bool {
		if ev.records[i].activity.Equal(ev.records[j].activity) {
			return ev.records[i].name < ev.records[j].name
		}
		return ev.records[i].activity.After(ev.records[j].activity)
	})
	ev.currentFound, ev.currentState = probeCurrentRun(ev.expectedRunAt)
	return ev, nil
}

// probeCurrentRun reports whether this project's own runtime location holds
// a saved run, and how far it got. The listing answers this independently of
// the bounded page, so a caller always learns whether its own project has a
// run even when the page omitted it.
func probeCurrentRun(runtimeDir string) (bool, string) {
	statePath := filepath.Join(runtimeDir, "state.db")
	if _, err := os.Stat(statePath); err != nil {
		if _, err := os.Stat(runtimeDir); err == nil {
			return false, "missing"
		}
		return false, "none"
	}
	store, err := openObservedStore(runtimeDir)
	if err != nil {
		return true, "unreadable"
	}
	defer store.Close()
	if _, err = store.Snapshot(); err != nil {
		return true, "unreadable"
	}
	return true, "saved"
}

// collectRunRecord reads one run's Looper-owned records. Every failure stays
// evidence: a missing or unreadable state database is reported with its
// reason, never turned into an invented result, and never fails the listing.
func collectRunRecord(name, runtime string, now time.Time) *runsRecord {
	rec := &runsRecord{name: name, directory: filepath.Dir(runtime), runtimeDir: runtime}
	// Controller metadata first: its liveness decides how the store may be
	// attached, beside the freshness it reports.
	readRunController(rec, runtime, now)
	statePath := filepath.Join(runtime, "state.db")
	switch _, err := os.Stat(statePath); {
	case err != nil:
		rec.state = "missing"
	default:
		rec.state = "unreadable"
		if store, err := openRunStore(statePath, rec.controllerState == "owning"); err != nil {
			rec.stateError = err
		} else {
			snap, err := store.Snapshot()
			if err != nil {
				rec.stateError = err
			} else {
				rec.state = "saved"
				readRunState(rec, snap.State)
			}
			readRunSessions(rec, store)
			store.Close()
		}
	}
	// The activity stamp orders the listing: the newest write the run's own
	// records show. The state file is the last store write; a start that
	// never saved state falls back to its controller metadata, then to the
	// runtime directory itself.
	rec.activity = time.Now()
	for _, probe := range []string{statePath, controllerPath(runtime), runtime} {
		if st, err := os.Stat(probe); err == nil {
			rec.activity = st.ModTime()
			break
		}
	}
	if rec.root != "" {
		_, err := os.Stat(rec.root)
		rec.rootExists = err == nil
	}
	return rec
}

// readRunState extracts the discovery facts from the saved state: the
// project root and task file the run belongs to, its baseline, and its
// retained work.
func readRunState(rec *runsRecord, s *State) {
	if s == nil {
		return
	}
	rec.root, rec.todo = s.Root, s.Todo
	rec.head, rec.branch = s.Head, s.Branch
	rec.runID = runIdentifier(s, nil)
	rec.retained = retained(s)
	rec.retainedIDs, rec.retainedTtl = retainedTaskIDs(s)
	rec.tasksTtl = len(s.Backlog.Tasks)
	for _, t := range s.Backlog.Tasks {
		if t.Status == "done" {
			rec.tasksDone++
		}
	}
}

// readRunSessions reads the retained controller sessions: how many the store
// holds, the newest row, and the shutdown event that names its exit. A store
// from before the sessions table existed records none, which stays an
// explicit unknown rather than an error.
func readRunSessions(rec *runsRecord, store *Store) {
	var total int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&total); err != nil {
		if !strings.Contains(err.Error(), "no such table") {
			rec.stateError = errors.Join(rec.stateError, fmt.Errorf("session records: %w", err))
		}
		return
	}
	rec.sessionsTtl = total
	var pid int
	var ended sql.NullString
	row := &runSessionRow{}
	if err := store.db.QueryRow("SELECT id,pid,started,ended,outcome FROM sessions ORDER BY rowid DESC LIMIT 1").
		Scan(&row.id, &pid, &row.started, &ended, &row.outcome); err != nil {
		return
	}
	if ended.Valid {
		row.ended = ended.String
	}
	rec.newest = row
	rec.shutdown = newestShutdownFor(store, row.id)
	if rec.runID == "" || rec.runID == "unknown" {
		rec.runID = row.id
	}
	// The newest session event carries the controller identity recorded at
	// its start: build evidence for a run whose controller.json is gone.
	rec.sessionInfo = newestSessionEvent(store)
}

// newestShutdownFor finds the shutdown event of one session, newest first.
func newestShutdownFor(store *Store, session string) *sessionExit {
	rows, err := store.db.Query("SELECT id,data FROM events WHERE kind='shutdown' ORDER BY id DESC")
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil
		}
		var exit sessionExit
		if json.Unmarshal([]byte(raw), &exit) == nil && exit.Session == session {
			return &exit
		}
	}
	return nil
}

// newestSessionEvent reads the newest controller identity event. It is the
// build evidence for a stopped run: beginSession records it with the session.
func newestSessionEvent(store *Store) *ControllerInfo {
	var raw string
	if err := store.db.QueryRow("SELECT data FROM events WHERE kind='session' ORDER BY id DESC LIMIT 1").Scan(&raw); err != nil {
		return nil
	}
	var info ControllerInfo
	if json.Unmarshal([]byte(raw), &info) != nil {
		return nil
	}
	return &info
}

// readRunController reads the ownership freshness evidence of one run:
// controller metadata, its age, and whether the recorded process is alive.
func readRunController(rec *runsRecord, runtime string, now time.Time) {
	st, err := os.Stat(controllerPath(runtime))
	switch {
	case os.IsNotExist(err):
		// A clean shutdown removes its own record; no metadata remains.
		rec.controllerState = "stopped"
	case err != nil:
		rec.controllerState = "unknown"
		rec.controllerErr = err
	default:
		rec.infoAge = now.Sub(st.ModTime())
		info, err := readController(runtime)
		if err != nil {
			rec.controllerState = "unknown"
			rec.controllerErr = err
			return
		}
		rec.info = info
		if controllerAlive(info) {
			rec.controllerState = "owning"
		} else {
			// Metadata names a controller whose process is gone: the record
			// is stale, and the state's own session outcome says how the run
			// actually ended.
			rec.controllerState = "stale"
		}
	}
}

// buildRunsDocument renders the listing within its byte budget.
func buildRunsDocument(ev *runsEvidence, limit int) *runsDocument {
	var doc *runsDocument
	for _, fit := range runsFitsFor(limit) {
		doc = renderRunsDocument(ev, limit, fit)
		if runsEncoded(doc) <= runsBytesBudget {
			return doc
		}
	}
	return doc
}

// runsEncoded reports the size of the document as the CLI emits it.
func runsEncoded(doc *runsDocument) int {
	var b strings.Builder
	_ = json.NewEncoder(&b).Encode(doc)
	return b.Len()
}

// renderRunsDocument renders the evidence into one bounded document. Every
// string bound counts the bytes the JSON encoder emits, so escape-heavy
// identities shrink with the ladder. Plain text renders from the same
// document, so the two outputs cannot disagree.
func renderRunsDocument(ev *runsEvidence, limit int, fit runsFit) *runsDocument {
	clamp := func(s string) string { return clampJSONString(s, fit.stringMax) }
	doc := &runsDocument{
		ContractVersion: ContractVersionV2,
		SchemaVersion:   runsSchemaVersion,
		Kind:            runsKind,
		GeneratedAt:     ev.now.UTC().Format(time.RFC3339),
	}
	doc.Project = runsProject{
		Root: clamp(ev.c.Root), TodoPath: clamp(ev.c.Todo), BaseDir: clamp(ev.base),
		RuntimeDir:      clamp(ev.expectedRunAt),
		CurrentRunFound: ev.currentFound, CurrentRunState: ev.currentState,
	}
	note := "Only Looper-owned runtime directories named <project>-<hash> under the base directory are read; private agent session directories are never scanned."
	if ev.trunc {
		note += fmt.Sprintf(" The scan stopped after %d entries; more entries exist than were counted.", runsScanMax)
	}
	doc.Searched = runsSearched{
		BaseDir: clamp(ev.base), EntriesScanned: ev.scanned, EntriesSkipped: ev.skipped,
		ScanTruncated: ev.trunc, ScanNote: clamp(note),
	}
	doc.RunTotal = len(ev.records)
	page := ev.records
	if len(page) > limit {
		page = page[:limit]
	}
	if len(page) > fit.runsMax {
		page = page[:fit.runsMax]
	}
	doc.Runs = []runsEntry{}
	for _, rec := range page {
		doc.Runs = append(doc.Runs, renderRunEntry(rec, fit, clamp))
	}
	doc.Limits = runsLimits{
		BytesBudget: runsBytesBudget, PageLimit: limit, RunsMax: min(fit.runsMax, limit),
		StringMax: fit.stringMax, RetainedIDsMax: fit.retainedMax,
		Omitted: map[string]int{"runs": max(0, len(ev.records)-len(doc.Runs))},
	}
	doc.Detail = runsDetail{
		Run:  "looper status --contract v2 --json (from a listed project root; each entry names its select command)",
		Page: fmt.Sprintf("looper runs --limit %d --contract v2 --json", maxRunsPageLimit),
	}
	return doc
}

// renderRunEntry renders one collected record under the fit bounds.
func renderRunEntry(rec *runsRecord, fit runsFit, clamp func(string) string) runsEntry {
	e := runsEntry{
		Name: clamp(rec.name), Directory: clamp(rec.directory), RuntimeDir: clamp(rec.runtimeDir),
		State: rec.state, CurrentProject: rec.current, RunID: clamp(rec.runID),
		Root: clamp(rec.root), TodoPath: clamp(rec.todo),
		Head: clamp(rec.head), Branch: clamp(branchLabel(rec.branch)),
		Retained: rec.retained, RetainedTasks: []string{},
		RetainedTaskTotal: rec.retainedTtl, TasksTotal: rec.tasksTtl, TasksDone: rec.tasksDone,
		SessionsTotal: rec.sessionsTtl, Select: runSelectionHints(rec, clamp),
	}
	if rec.root != "" {
		exists := rec.rootExists
		e.RootExists = &exists
	}
	if rec.stateError != nil {
		e.StateError = clamp(rec.stateError.Error())
	}
	for i, id := range rec.retainedIDs {
		if i >= fit.retainedMax {
			break
		}
		e.RetainedTasks = append(e.RetainedTasks, clamp(id))
	}
	e.LastSession = runsSessionRef{Outcome: "unknown"}
	if rec.newest != nil {
		e.LastSession = runsSessionRef{
			ID: clamp(rec.newest.id), Started: clamp(rec.newest.started), Ended: clamp(rec.newest.ended),
			Outcome: clamp(orUnknown(rec.newest.outcome)),
		}
	}
	if rec.shutdown != nil {
		e.LastSession.Reason = clamp(rec.shutdown.Reason)
		exit := rec.shutdown.ExitCode
		e.LastSession.ExitCode = &exit
	}
	e.LastActivity = rec.activity.UTC().Format(time.RFC3339)
	c := runsControllerRef{State: rec.controllerState}
	clampBuild := func(b *BuildInfo) *BuildInfo {
		if b == nil {
			return nil
		}
		return &BuildInfo{Release: clamp(b.Release), Revision: clamp(b.Revision), Modified: b.Modified}
	}
	switch {
	case rec.info != nil:
		c.Session, c.Mode, c.Started = clamp(rec.info.Session), clamp(rec.info.Mode), clamp(rec.info.Started)
		c.Build = clampBuild(rec.info.Build)
	case rec.sessionInfo != nil:
		// The controller's own record is gone; the session event recorded at
		// its start keeps the build evidence and the session it belonged to.
		c.Session, c.Mode = clamp(rec.sessionInfo.Session), clamp(rec.sessionInfo.Mode)
		c.Build = clampBuild(rec.sessionInfo.Build)
	}
	if rec.controllerErr != nil {
		c.Error = clamp(rec.controllerErr.Error())
	}
	if rec.infoAge != 0 {
		age := int64(rec.infoAge.Seconds())
		if age < 0 {
			age = 0
		}
		c.AgeSeconds = &age
	}
	e.Controller = c
	return e
}

// runSelectionHints names the exact commands that select one run. Looper
// resolves a project's runtime directory from the working directory, so
// selecting a run means running from its recorded project root.
func runSelectionHints(rec *runsRecord, clamp func(string) string) []string {
	hints := []string{}
	switch {
	case rec.state == "missing":
		hints = append(hints, clamp(fmt.Sprintf("no state records exist under %s; a start that reaches its first save creates them, and the directory name maps to one project root", rec.runtimeDir)))
	case rec.root != "" && rec.rootExists:
		hints = append(hints, clamp(fmt.Sprintf("cd %s && looper status --contract v2 --json", rec.root)))
	case rec.root != "":
		hints = append(hints, clamp(fmt.Sprintf("the recorded project root %s no longer exists; its runtime records stay in %s", rec.root, rec.runtimeDir)))
	default:
		hints = append(hints, clamp(fmt.Sprintf("the saved run cannot be read; its runtime records stay in %s", rec.runtimeDir)))
	}
	if rec.retained && rec.root != "" && rec.rootExists && len(hints) < runsSelectMax {
		hints = append(hints, clamp(fmt.Sprintf("cd %s && looper recovery --preview --contract v2 --json", rec.root)))
	}
	return hints
}

// runRunsListing serves the read-only run discovery (AX-025). It lists the
// saved runs within the resolved configured base directory, changes nothing,
// and never starts, stops, or mutates a run. An empty base directory is a
// bounded result, not an error; a base directory that cannot be read is.
func runRunsListing(c Config, out, stderr io.Writer) int {
	if c.JSON && c.Contract != ContractVersionV2 {
		p := incompatibleFailure("contract_version_required", "The run listing document is a contract version 2 document.")
		p.Expected = map[string]any{"contract": "v2"}
		p.Observed = map[string]any{"contract": "legacy", "option": "--json"}
		p.Actions = []ErrorAction{{
			Description: "Request the run listing with the version 2 contract.",
			Command:     "looper runs --contract v2 --json",
		}}
		return emitError(c, p, out, stderr)
	}
	ev, err := collectRunsEvidence(c, time.Now())
	if err != nil {
		p := unavailableFailure("base_unreadable", fmt.Sprintf("The configured base directory cannot be read: %v", err))
		p.Paths = []string{c.BaseDir}
		p.Observed = map[string]any{"base_dir": clampString(c.BaseDir, maxErrorString)}
		p.Actions = []ErrorAction{{
			Description: "Check the LOOPER_BASE_DIR path and its permissions, then list the saved runs again.",
			Command:     "looper runs --contract v2 --json",
		}}
		return emitError(c, p, out, stderr)
	}
	limit := c.EventLimit
	if limit == 0 {
		limit = runsDefaultLimit
	}
	doc := buildRunsDocument(ev, limit)
	if c.JSON {
		if err := json.NewEncoder(out).Encode(doc); err != nil {
			return 1
		}
		return 0
	}
	renderRunsPlain(doc, out)
	return 0
}

// renderRunsPlain renders the same document as text: the searched locations,
// this project's own state, and one bounded block per run with its outcome,
// freshness, build, and selection command.
func renderRunsPlain(doc *runsDocument, out io.Writer) {
	line := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
	line("Saved runs (read-only; no run was started, stopped, or changed)")
	line("Project: %s | task file %s | base %s", doc.Project.Root, doc.Project.TodoPath, doc.Project.BaseDir)
	state := doc.Project.CurrentRunState
	if doc.Project.CurrentRunFound {
		state += " (state records exist)"
	}
	line("This project's runtime: %s | own saved run: %s", doc.Project.RuntimeDir, state)
	line("Searched: %d entr%s under %s, %d skipped; %s", doc.Searched.EntriesScanned,
		map[bool]string{true: "y", false: "ies"}[doc.Searched.EntriesScanned == 1],
		doc.Searched.BaseDir, doc.Searched.EntriesSkipped, doc.Searched.ScanNote)
	if len(doc.Runs) == 0 {
		line("No saved runs under %s. This project would save its runtime records in %s.", doc.Project.BaseDir, doc.Project.RuntimeDir)
		line("Start one with: cd %s && looper", doc.Project.Root)
		line("Detail: %s | %s", doc.Detail.Run, doc.Detail.Page)
		return
	}
	shown := len(doc.Runs)
	omitted := doc.Limits.Omitted["runs"]
	if omitted > 0 {
		line("Runs: %d shown of %d total (%d omitted by the page or the byte budget; more: %s)", shown, doc.RunTotal, omitted, doc.Detail.Page)
	} else {
		line("Runs: %d shown of %d total", shown, doc.RunTotal)
	}
	for _, r := range doc.Runs {
		root := r.Root
		switch {
		case root == "" && r.State == "missing":
			root = "unknown (no state records exist)"
		case root == "":
			root = "unknown (the saved run cannot be read)"
		case r.RootExists != nil && !*r.RootExists:
			root += " (root no longer exists)"
		}
		current := ""
		if r.CurrentProject {
			current = " | this project"
		}
		line("%s  [%s]  root: %s%s", r.Name, r.State, root, current)
		retainedNote := "none"
		if r.Retained {
			retainedNote = fmt.Sprintf("%d task(s) retained: %s", r.RetainedTaskTotal, strings.Join(r.RetainedTasks, ", "))
			if r.RetainedTaskTotal > len(r.RetainedTasks) {
				retainedNote += fmt.Sprintf(" (+%d more)", r.RetainedTaskTotal-len(r.RetainedTasks))
			}
		}
		line("  work: %s | tasks %d/%d done | sessions %d | last activity %s",
			retainedNote, r.TasksDone, r.TasksTotal, r.SessionsTotal, r.LastActivity)
		outcome := r.LastSession.Outcome
		if r.LastSession.Reason != "" {
			outcome += fmt.Sprintf(" (%s", r.LastSession.Reason)
			if r.LastSession.ExitCode != nil {
				outcome += fmt.Sprintf(", exit %d", *r.LastSession.ExitCode)
			}
			outcome += ")"
		}
		if r.LastSession.ID != "" {
			outcome += " | session " + r.LastSession.ID
		}
		line("  last outcome: %s", outcome)
		controller := r.Controller.State
		if r.Controller.Session != "" {
			controller += " (session " + r.Controller.Session
			if r.Controller.Mode != "" {
				controller += ", " + r.Controller.Mode
			}
			if r.Controller.AgeSeconds != nil {
				controller += fmt.Sprintf(", metadata age %ds", *r.Controller.AgeSeconds)
			}
			controller += ")"
		}
		if r.Controller.Build != nil {
			controller += " | build " + r.Controller.Build.String()
		}
		line("  controller: %s", controller)
		if r.Controller.Error != "" {
			line("    metadata: %s", r.Controller.Error)
		}
		if r.StateError != "" {
			line("    state: %s", r.StateError)
		}
		for _, hint := range r.Select {
			line("  select: %s", hint)
		}
	}
	line("Detail: %s | %s", doc.Detail.Run, doc.Detail.Page)
}
