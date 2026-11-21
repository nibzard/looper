package looper

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nibzard/looper/internal/ui"
)

const observationVersion = 1

type WorkerObservation struct {
	AttemptID   string              `json:"attempt_id"`
	TaskID      string              `json:"task_id"`
	Phase       string              `json:"phase"`
	Retained    bool                `json:"retained"`
	Log         string              `json:"log,omitempty"`
	LastOutput  string              `json:"last_output_at,omitempty"`
	OutputAge   *int64              `json:"output_age_seconds,omitempty"`
	OutputBytes int64               `json:"output_bytes"`
	Process     *ProcessObservation `json:"process,omitempty"`
	Progress    string              `json:"progress"`
}

type ProcessObservation struct {
	PID      int    `json:"pid"`
	Identity string `json:"identity,omitempty"`
	Started  string `json:"started"`
	Ended    string `json:"ended,omitempty"`
	Running  bool   `json:"running"`
}

type Observation struct {
	Executable            BuildInfo           `json:"executable"`
	BuildMismatch         bool                `json:"build_mismatch"`
	Version               int                 `json:"schema_version"`
	ObservedAt            string              `json:"observed_at"`
	ControllerOwned       bool                `json:"controller_owned"`
	Controller            *ControllerInfo     `json:"controller,omitempty"`
	View                  *ui.ViewSnapshot    `json:"view,omitempty"`
	ViewMatchesController bool                `json:"view_matches_controller"`
	Workers               []WorkerObservation `json:"workers"`
	EventCursor           int64               `json:"event_cursor"`
}

func activeAttempt(a *Attempt) bool {
	return a.Phase != "done" && a.Phase != "blocked" && a.Phase != "cancelled"
}

// Restrict discovery to Looper-owned artifacts. Never inspect agent home files.
func attemptLogs(a *Attempt) []string {
	var paths []string
	if a.Directory == "" {
		return paths
	}
	for _, pattern := range []string{"work-*/events-*.jsonl", "work-*/report-recovery/events-*.jsonl", "work-*/setup.log", "checks/*/setup.log", "checks/*/verify.log"} {
		matches, _ := filepath.Glob(filepath.Join(a.Directory, pattern))
		for _, p := range matches {
			if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() {
				paths = append(paths, p)
			}
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		a, ea := os.Stat(paths[i])
		b, eb := os.Stat(paths[j])
		if ea != nil || eb != nil {
			return paths[i] < paths[j]
		}
		if a.ModTime().Equal(b.ModTime()) {
			return paths[i] < paths[j]
		}
		return a.ModTime().Before(b.ModTime())
	})
	return paths
}

func observeWorker(a *Attempt) WorkerObservation {
	w := WorkerObservation{AttemptID: a.ID, TaskID: a.Task.ID, Phase: a.Phase, Progress: "unknown"}
	paths := attemptLogs(a)
	// A repair's old check logs must not masquerade as its worker output.
	// A queued check similarly must not show an implementation process as live.
	if a.Phase == "running" || a.Phase == "verifying" {
		filtered := paths[:0]
		for _, path := range paths {
			rel, err := filepath.Rel(a.Directory, path)
			isCheck := strings.HasPrefix(rel, "checks"+string(filepath.Separator))
			if err == nil && ((a.Phase == "verifying" && isCheck) || (a.Phase == "running" && !isCheck)) {
				filtered = append(filtered, path)
			}
		}
		paths = filtered
	}
	if len(paths) == 0 {
		return w
	}
	w.Log = paths[len(paths)-1]
	if st, err := os.Stat(w.Log); err == nil {
		w.OutputBytes = st.Size()
		if st.Size() > 0 {
			w.LastOutput = st.ModTime().UTC().Format(time.RFC3339Nano)
			age := int64(max(0, wallClock().Sub(st.ModTime()).Seconds()))
			w.OutputAge = &age
		}
	}
	var p ProcessObservation
	if readJSON(w.Log+".process.json", &p) == nil {
		identity, err := runIdentityCommand(p.PID)
		p.Running = p.Ended == "" && err == nil && identity == p.Identity && p.Identity != ""
		w.Process = &p
	}
	return w
}

func observe(c Config, s *State, snap *StoreSnapshot) Observation {
	_, dir := projectPaths(c)
	o := Observation{Version: observationVersion, ObservedAt: wallClock().UTC().Format(time.RFC3339Nano),
		ControllerOwned: repositoryOwned(c), View: LoadView(dir), Workers: []WorkerObservation{}}
	o.Executable = currentBuild()
	o.Controller, _ = readController(dir)
	o.BuildMismatch = o.ControllerOwned && o.Controller != nil && o.Controller.Build != nil && *o.Controller.Build != o.Executable
	o.ViewMatchesController = o.ControllerOwned && o.Controller != nil && o.View != nil && o.View.Session == o.Controller.Session
	// The cursor is the store's committed high-water mark from the same
	// snapshot as the state (AX-003). The published view is presentation
	// only: a stopped run has none, and a live view can lag or lead the
	// state being reported. Without a snapshot there is no store, so zero
	// honestly reports that no position exists.
	if snap != nil {
		o.EventCursor = snap.HighWater
	}
	if s != nil {
		for _, a := range s.Attempts {
			if activeAttempt(a) {
				w := observeWorker(a)
				w.Retained = !o.ControllerOwned
				o.Workers = append(o.Workers, w)
			}
		}
	}
	return o
}

// Preserve existing status fields; the observation is an additive contract.
func statusReport(c Config, snap *StoreSnapshot, timing TimingReport, statistics ui.StatisticsView) any {
	// The legacy status document is shape-frozen (AX-001) and its schema
	// admits no export_semantics, requests, cancellations, or startup_seed
	// key. Those are saved-state machinery — the AX-018 fingerprint, the
	// AX-009 request receipts, the AX-019 pending cancellations, the AX-022
	// startup seed that marks the bootstrap window, and the T128 review
	// coverage record — so the projection reports a copy of the state
	// without them; omitempty then leaves each key out. The copy is shallow
	// and read-only, so the engine's state keeps every hidden field.
	state := *snap.State
	state.ExportSemantics = ""
	state.Requests = nil
	state.Cancellations = nil
	state.StartupSeed = false
	state.ReviewedTree = ""
	state.ReviewedComplete = false
	state.ReviewedImports = 0
	state.ReviewedBacklog = ""
	// The structured block reason is version 2 output (OW-10): the legacy
	// and --full documents share this frozen shape and admit no
	// block_reason key on nested tasks. The compact documents carry it.
	state.Backlog.Tasks = append([]Task{}, state.Backlog.Tasks...)
	for i := range state.Backlog.Tasks {
		state.Backlog.Tasks[i].BlockReason = nil
	}
	// A reconciliation proposal's transition and revision (T124) are input
	// the finding application consumed; the frozen document admits no such
	// key inside a retained summary's follow-ups. The saved state keeps
	// them, because recovery replays the retained result and re-applies its
	// findings. Readers see the outcome on the task and in the journal.
	state.Attempts = append([]*Attempt{}, state.Attempts...)
	for i, a := range state.Attempts {
		if a == nil || a.Summary == nil || len(a.Summary.Followups) == 0 {
			continue
		}
		attempt, summary := *a, *a.Summary
		summary.Followups = append([]Finding{}, summary.Followups...)
		for j := range summary.Followups {
			summary.Followups[j].Transition = ""
			summary.Followups[j].Revision = ""
		}
		attempt.Summary = &summary
		state.Attempts[i] = &attempt
	}
	return struct {
		*State
		Observation Observation       `json:"observation"`
		Timing      TimingReport      `json:"timing"`
		Statistics  ui.StatisticsView `json:"statistics"`
	}{&state, observe(c, snap.State, snap), timing, statistics}
}

func inspectTask(c Config, store *Store, out io.Writer) error {
	if c.InspectTask == "" {
		p := usageFailure("usage_invalid", "usage: looper inspect <task-id> [--json]")
		// The legacy contract reported this from the read path, not parsing.
		p.legacyExit = exitOperationFailed
		return p
	}
	s, err := store.Load()
	if err != nil {
		return err
	}
	if s == nil {
		p := runNotFoundForRun(c, "", "looper")
		p.Message = "no saved run"
		return p
	}
	for _, t := range s.Backlog.Tasks {
		if t.ID != c.InspectTask {
			continue
		}
		attempts := []*Attempt{}
		workers := []WorkerObservation{}
		logs := []string{}
		for _, a := range s.Attempts {
			if a.Task.ID == t.ID {
				attempts = append(attempts, a)
				w := observeWorker(a)
				w.Retained = activeAttempt(a) && !repositoryOwned(c)
				workers = append(workers, w)
				logs = append(logs, attemptLogs(a)...)
			}
		}
		checks, err := taskChecks(store, attempts)
		if err != nil {
			return err
		}
		timing, err := store.Timing(wallClock().UTC())
		if err != nil {
			return err
		}
		taskTiming := timing.task(t.ID)
		// The legacy inspection document keeps the frozen task shape: the
		// structured reason lives in the compact version 2 projection.
		reported := t
		reported.BlockReason = nil
		report := struct {
			Version  int                 `json:"schema_version"`
			Task     Task                `json:"task"`
			Attempts []*Attempt          `json:"attempts"`
			Workers  []WorkerObservation `json:"workers"`
			Checks   []CheckObservation  `json:"checks"`
			Logs     []string            `json:"logs"`
			Timing   TaskTimingReport    `json:"timing"`
		}{observationVersion, reported, attempts, workers, checks, logs, taskTiming}
		if c.JSON {
			return json.NewEncoder(out).Encode(report)
		}
		fmt.Fprintf(out, "%s  %s  %s\n", t.ID, t.Status, sanitize(t.Title))
		printTaskTiming(out, taskTiming)
		for _, b := range t.Blockers {
			fmt.Fprintln(out, "Blocked:", sanitize(b))
		}
		for _, w := range workers {
			phase := w.Phase
			if w.Retained {
				phase = "retained (last phase: " + phase + ")"
			}
			fmt.Fprintf(out, "%s  %s  %s\n", w.AttemptID, phase, w.Log)
		}
		return nil
	}
	return taskNotFoundError(c.InspectTask, len(s.Backlog.Tasks))
}

type HealthFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	TaskID   string `json:"task_id,omitempty"`
	Message  string `json:"message"`
	Action   string `json:"suggested_action"`
}

// findingTaskCircular names a blocked task whose resume condition requires
// the task itself (QUICK-WINS-PLAN section 1, T135). It keeps warning
// severity under the operator and deferral reason classes: expected waiting
// ends where only the blocked task's own progress could satisfy the
// condition.
const findingTaskCircular = "task_circular_condition"

// selfRequirementWords put a blocked task's own identifier in requirement
// position: the condition waits on, needs, or is gated at that task.
// "before" is absent on purpose — "T090 must land before this task starts"
// names T090 as the prerequisite and this task as the consequence.
var selfRequirementWords = map[string]bool{
	"for": true, "until": true, "on": true, "by": true, "after": true, "when": true,
	"need": true, "needs": true, "require": true, "requires": true,
	"await": true, "awaits": true, "awaiting": true,
}

// selfProgressWords name the events a blocked task's own clause can require:
// the task starting, beginning, landing, becoming unblocked, or resuming its
// work ("T040 auto-save work resumes", the retained Charlie wording).
// Completion words stay out — "T077 outcome verified complete" reports an
// outcome, and a resume consequence that follows an external cue ("after the
// operator grants access, T027 resumes") is exempt before this set is read.
var selfProgressWords = map[string]bool{
	"start": true, "starts": true, "started": true, "starting": true,
	"begin": true, "begins": true, "began": true, "beginning": true,
	"unblock": true, "unblocks": true, "unblocked": true, "unblocking": true,
	"finish": true, "finishes": true, "finished": true,
	"land": true, "lands": true, "landed": true, "landing": true,
	"ship": true, "ships": true, "shipped": true,
	"resume": true, "resumes": true, "resumed": true, "resuming": true,
}

// consequenceConnectors mark the identifier as the later, explained event:
// "T085 lands before T027 starts" and "the prerequisites land, then T027
// begins" mention the task to say what follows, not what is required.
var consequenceConnectors = map[string]bool{"then": true, "before": true}

// externalCues introduce an event the task waits for instead of itself.
var externalCues = map[string]bool{"when": true, "once": true, "after": true, "until": true}

// The subject-position check reads a bounded window: the recorded shapes
// keep the progress word within four words of the identifier ("T045 art
// direction pass starts"), and the bound keeps an unrelated verb deep in a
// long clause from turning a mention into a warning.
const selfProgressWindow = 4

// wordRune reports whether r can appear inside an identifier.
func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// wholeIdentifier reports whether s[at:end] stands as one word: neither
// neighboring rune belongs to an identifier, so T040 does not match the
// T0401 it sits inside.
func wholeIdentifier(s string, at, end int) bool {
	if at > 0 {
		if r, _ := utf8.DecodeLastRuneInString(s[:at]); wordRune(r) {
			return false
		}
	}
	if end < len(s) {
		if r, _ := utf8.DecodeRuneInString(s[end:]); wordRune(r) {
			return false
		}
	}
	return true
}

// lastWord returns the last identifier run in s: the word that would touch
// an identifier written at the end of s.
func lastWord(s string) string {
	runes := []rune(s)
	end := len(runes)
	for end > 0 && !wordRune(runes[end-1]) {
		end--
	}
	start := end
	for start > 0 && wordRune(runes[start-1]) {
		start--
	}
	return string(runes[start:end])
}

// firstWords returns the first n identifier runs in s, in order; n of zero
// or less returns every word.
func firstWords(s string, n int) []string {
	var words []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return !wordRune(r) }) {
		words = append(words, part)
		if n > 0 && len(words) == n {
			break
		}
	}
	return words
}

// clauseBefore splits the text before one event or mention into its own
// clause so far and the text before that clause: the last comma, semicolon,
// or colon ends a clause, so the segment is what the same clause said
// before the event and before holds every earlier clause.
func clauseBefore(prefix string) (segment, before string) {
	if at := strings.LastIndexAny(prefix, ",;:"); at >= 0 {
		return prefix[at+1:], prefix[:at]
	}
	return prefix, ""
}

// connectorInWords reports whether s carries a consequence connector: a
// clause that said "then" or "before" explains the event that follows the
// connector instead of requiring it.
func connectorInWords(s string) bool {
	for _, w := range firstWords(s, 0) {
		if consequenceConnectors[w] {
			return true
		}
	}
	return false
}

// mentionRequiresTask reports whether one occurrence of a blocked task's
// identifier, at lowered[at:], requires the task itself. Two shapes count:
// the identifier in requirement position ("waits for T027 to start"), and
// the identifier in subject position with its own progress as the event
// ("T027 starts implementation", "T066 is unblocked"). A mention whose own
// clause carries a consequence connector, that follows a clause with an
// external cue, or that an external cue governs ("T027 resumes when the
// migration lands") explains what happens after a prerequisite — with or
// without subject words in between ("before the T027 work starts") — and
// never counts.
func mentionRequiresTask(lowered string, at int, id string) bool {
	prefix, rest := lowered[:at], lowered[at+len(id):]
	if selfRequirementWords[lastWord(prefix)] {
		return true
	}
	// A possessive mention modifies another noun — "the environment that
	// T065's unblock condition requires" gates on the environment.
	if strings.HasPrefix(rest, "'") || strings.HasPrefix(rest, "’") {
		return false
	}
	segment, before := clauseBefore(prefix)
	if connectorInWords(segment) || hasExternalCue(before) || cueInFirstWords(rest) {
		return false
	}
	return progressWordInWindow(rest)
}

// progressWordInWindow reports whether one of the first words after the
// identifier names the task's own progress as the required event. A word
// followed by a colon labels the clause that follows, and a passive "by"
// names the external agent that delivers the event — "T052 unblocks: an
// Android device is attached" and "T130, itself unblocked by T118's push"
// gate on that agent, not on the task — so neither counts.
func progressWordInWindow(rest string) bool {
	words := 0
	for at := 0; at < len(rest) && words < selfProgressWindow; {
		r, size := utf8.DecodeRuneInString(rest[at:])
		if !wordRune(r) {
			at += size
			continue
		}
		end := at
		for end < len(rest) {
			r, size = utf8.DecodeRuneInString(rest[end:])
			if !wordRune(r) {
				break
			}
			end += size
		}
		if selfProgressWords[rest[at:end]] && !progressWordIsLabel(rest, end) {
			return true
		}
		words++
		at = end
	}
	return false
}

// progressWordIsLabel reports whether the word ending at rest[end] names
// something other than the task's own required event: a colon that labels
// the following clause, or a "by" that names the delivering agent.
func progressWordIsLabel(rest string, end int) bool {
	after := strings.TrimLeft(rest[end:], " \t")
	if strings.HasPrefix(after, ":") {
		return true
	}
	next := firstWords(after, 1)
	return len(next) == 1 && next[0] == "by"
}

// cueInFirstWords reports whether an external cue governs the identifier's
// own clause, as in "T027 starts verification when the migration lands".
func cueInFirstWords(rest string) bool {
	for _, w := range firstWords(rest, selfProgressWindow+2) {
		if externalCues[w] {
			return true
		}
	}
	return false
}

// circularResumeCondition returns the blocker or resume condition that
// requires the blocked task itself to start, finish, resume, or become
// unblocked. No independent prerequisite can satisfy such a condition, so
// the task cannot leave the queue — the recorded Charlie shapes (delivery
// audit 2026-11-17) are "Waits for T027 to start implementation", "T027
// starts implementation", "T066 is unblocked", and the identifier-free
// "Waits for save work to resume" (T040, whose own scope is the save work).
// A condition that names another task, or one that mentions this task only
// as the work that follows an external prerequisite ("after the operator
// grants access, T027 resumes"), reports nothing. The reconcile guard
// (circularConditionReason in findings.go) stays broader on purpose: an
// automatic transition escalates on any mention of the task itself, while
// this warning must stay quiet when the recorded condition is honest.
func circularResumeCondition(t Task) string {
	if t.Status != "blocked" || t.ID == "" {
		return ""
	}
	id := strings.ToLower(t.ID)
	for _, condition := range append(append([]string{}, t.Blockers...), t.UnblockWhen) {
		lowered := strings.ToLower(condition)
		for search := 0; ; {
			at := strings.Index(lowered[search:], id)
			if at < 0 {
				break
			}
			at += search
			search = at + len(id)
			if wholeIdentifier(lowered, at, at+len(id)) && mentionRequiresTask(lowered, at, id) {
				return condition
			}
		}
		if idFreeCircularWording(condition, t) {
			return condition
		}
	}
	return ""
}

// idFreeEventWords name the events an identifier-free condition can require
// of the blocked task itself: its own progress (the selfProgressWords
// vocabulary) plus the resumption and deferral-round endings the recorded
// identifier-free shapes carry. A condition that names any task never reads
// this set; the positional identifier check governs it.
var idFreeEventWords = func() map[string]bool {
	words := map[string]bool{
		"lost": true, "lifted": true, "cleared": true,
	}
	for w := range selfProgressWords {
		words[w] = true
	}
	return words
}()

// deferralResolutionWords name the ending of the task's own deferral round.
// They pair with a deferral reference inside one clause, because only the
// task's own resumption (or the operator edit that resumes it) ends the
// deferral — the recorded "T027 starts implementation and lose its deferred
// tag" without the identifier.
var deferralResolutionWords = map[string]bool{
	"lose": true, "loses": true, "lost": true, "lifted": true,
	"cleared": true, "gone": true,
}

// idFreeSelfWords name the blocked task's own subject matter without its
// identifier: its implementation, or the deferral round only its own
// resumption ends.
var idFreeSelfWords = map[string]bool{"implementation": true, "deferral": true, "deferred": true}

// subjectFillerWords carry no subject matter inside a noun phrase, so the
// subject scan steps over them without spending its word budget.
var subjectFillerWords = map[string]bool{
	"to": true, "the": true, "a": true, "an": true, "its": true,
	"their": true, "of": true, "and": true, "or": true,
}

// scopeStopWords leave a title's significant words: function words, the
// event vocabulary (a condition echoing the event is not a scope match),
// and project vocabulary nearly every title carries, which would flag
// honest conditions that echo it.
var scopeStopWords = map[string]bool{
	// Function words.
	"a": true, "an": true, "the": true, "and": true, "or": true, "of": true,
	"for": true, "to": true, "in": true, "on": true, "at": true, "by": true,
	"with": true, "from": true, "into": true, "per": true, "as": true,
	"is": true, "are": true, "be": true, "been": true, "was": true, "were": true,
	"it": true, "its": true, "this": true, "that": true, "these": true,
	"those": true, "when": true, "until": true, "after": true, "before": true,
	"while": true, "then": true, "than": true, "so": true, "not": true,
	"no": true, "any": true, "all": true, "every": true, "each": true,
	"must": true, "should": true, "can": true, "cannot": true, "will": true,
	"would": true, "may": true, "might": true, "own": true, "new": true,
	// Event vocabulary.
	"start": true, "starts": true, "started": true, "starting": true,
	"begin": true, "begins": true, "began": true, "beginning": true,
	"unblock": true, "unblocks": true, "unblocked": true, "unblocking": true,
	"finish": true, "finishes": true, "finished": true,
	"land": true, "lands": true, "landed": true, "landing": true,
	"ship": true, "ships": true, "shipped": true,
	"resume": true, "resumes": true, "resumed": true, "resuming": true,
	// Project vocabulary nearly every title carries.
	"work": true, "works": true, "working": true, "worked": true,
	"task": true, "tasks": true, "implementation": true, "implement": true,
	"implements": true, "implemented": true, "section": true, "system": true,
	"systems": true, "acceptance": true, "test": true, "tests": true,
	"testing": true, "check": true, "checks": true, "make": true,
	"green": true, "complete": true, "completed": true, "done": true,
}

// taskIdentifierWord reports whether w spells one task identifier, t
// followed by digits, as every backlog ID is written (T027, T085).
func taskIdentifierWord(w string) bool {
	if len(w) < 2 || (w[0] != 't' && w[0] != 'T') {
		return false
	}
	for i := 1; i < len(w); i++ {
		if w[i] < '0' || w[i] > '9' {
			return false
		}
	}
	return true
}

// mentionsTaskIdentifier reports whether s names any task explicitly. A
// condition that does reads through the positional identifier check, so the
// identifier-free wording below never rewrites its meaning.
func mentionsTaskIdentifier(s string) bool {
	for _, w := range firstWords(s, 0) {
		if taskIdentifierWord(w) {
			return true
		}
	}
	return false
}

// titleScopeWords collects the significant words of one task's title — its
// own subject matter, which an identifier-free condition can reference:
// "save work" is the scope of "Auto-save system working per section 18".
func titleScopeWords(title string) map[string]bool {
	scope := map[string]bool{}
	for _, w := range firstWords(strings.ToLower(title), 0) {
		if !scopeStopWords[w] {
			scope[w] = true
		}
	}
	return scope
}

// subjectNamesTask reports whether the words directly before one event word
// name the blocked task's own subject matter: an explicit self word or a
// significant word of the task's title. The backward scan stops at a clause
// boundary or a structural word (a requirement, an external cue, a
// consequence connector), steps over articles and conjunctions, and reads
// at most selfProgressWindow subject words — "waits for save work to
// resume" reads the subject "save work" of the event "resume".
func subjectNamesTask(prefix string, scope map[string]bool) bool {
	words := 0
	for at := len(prefix); at > 0 && words < selfProgressWindow; {
		r, size := utf8.DecodeLastRuneInString(prefix[:at])
		if !wordRune(r) {
			if r == ',' || r == ';' || r == ':' {
				return false
			}
			at -= size
			continue
		}
		end := at
		for end > 0 {
			r, size = utf8.DecodeLastRuneInString(prefix[:end])
			if !wordRune(r) {
				break
			}
			end -= size
		}
		w := prefix[end:at]
		if selfRequirementWords[w] || externalCues[w] || consequenceConnectors[w] {
			return false
		}
		if !subjectFillerWords[w] {
			if idFreeSelfWords[w] || scope[w] {
				return true
			}
			words++
		}
		at = end
	}
	return false
}

// hasExternalCue reports whether s carries a word that introduces an event
// the task waits for instead of itself.
func hasExternalCue(s string) bool {
	for _, w := range firstWords(s, 0) {
		if externalCues[w] {
			return true
		}
	}
	return false
}

// idFreeCircularWording reports whether one condition that names no task at
// all still requires the blocked task's own progress. The recorded shape
// (delivery audit 2026-11-17, Charlie T040) is "Waits for save work to
// resume": no identifier appears, and the condition's subject is the task's
// own scope with its own progress as the required event. A deferral
// reference paired with its resolution word inside one clause counts the
// same way, because only the task's own resumption ends its deferral.
//
// Quiet by construction: a condition naming any task is left to the
// identifier check; an event word governed by an external cue after it or
// in a preceding clause, or by a consequence connector in its own clause,
// explains what follows a prerequisite — subject words between the
// connector or cue and the event change nothing; a clause carrying an
// external cue never pairs a deferral with its resolution; and a subject
// that matches neither a self word nor the task's title scope names
// something else.
func idFreeCircularWording(condition string, t Task) bool {
	lowered := strings.ToLower(strings.TrimSpace(condition))
	if lowered == "" || mentionsTaskIdentifier(lowered) {
		return false
	}
	// The "Deferred: " label a deferred blocker carries names the recorded
	// state, not the deferral round as a subject; drop it before reading.
	if head, rest, found := strings.Cut(lowered, ":"); found && strings.TrimSpace(head) == "deferred" {
		lowered = strings.TrimSpace(rest)
		if lowered == "" {
			return false
		}
	}
	scope := titleScopeWords(t.Title)
	clauseStart := 0
	clauseDeferral, clauseResolution := false, false
	closeClause := func(to int) bool {
		if clauseDeferral && clauseResolution && !hasExternalCue(lowered[clauseStart:to]) {
			return true
		}
		clauseDeferral, clauseResolution = false, false
		clauseStart = to
		return false
	}
	for at := 0; at < len(lowered); {
		r, size := utf8.DecodeRuneInString(lowered[at:])
		if !wordRune(r) {
			if r == ',' || r == ';' || r == ':' {
				at += size
				if closeClause(at) {
					return true
				}
				continue
			}
			at += size
			continue
		}
		end := at
		for end < len(lowered) {
			r, size = utf8.DecodeRuneInString(lowered[end:])
			if !wordRune(r) {
				break
			}
			end += size
		}
		w := lowered[at:end]
		if w == "deferred" || w == "deferral" {
			clauseDeferral = true
		}
		if deferralResolutionWords[w] {
			clauseResolution = true
		}
		if idFreeEventWords[w] {
			prefix, rest := lowered[:at], lowered[end:]
			// The same exemptions the identifier check reads: a connector in
			// the event's own clause, a cue in an earlier clause, or a cue
			// after the event explains a consequence — with or without
			// subject words in between ("the migration lands, then the save
			// work resumes"). A cue in the same clause does not exempt:
			// "waits until the save work resumes" stays a requirement.
			segment, before := clauseBefore(prefix)
			if connectorInWords(segment) || hasExternalCue(before) || cueInFirstWords(rest) {
				at = end
				continue
			}
			if !progressWordIsLabel(rest, 0) && subjectNamesTask(prefix, scope) {
				return true
			}
		}
		at = end
	}
	return clauseDeferral && clauseResolution && !hasExternalCue(lowered[clauseStart:])
}

func diagnose(o Observation, s *State) []HealthFinding {
	f := []HealthFinding{}
	if o.ControllerOwned && !o.ViewMatchesController {
		f = append(f, HealthFinding{Code: "view_unavailable", Severity: "warning", Message: "The repository is owned but no matching view is available.", Action: "Check controller metadata and logs; startup may still be in progress."})
	}
	if !o.ControllerOwned && len(o.Workers) > 0 {
		f = append(f, HealthFinding{Code: "retained_work", Severity: "info", Message: "Attempts are retained without an active controller.", Action: "Run looper to resume retained work."})
	}
	for _, w := range o.Workers {
		if o.ControllerOwned && w.Process != nil && w.Process.Ended == "" && !w.Process.Running {
			f = append(f, HealthFinding{Code: "worker_process_missing", Severity: "warning", TaskID: w.TaskID, Message: "The recorded worker process is absent.", Action: "Read task events; the controller may be processing its exit."})
		}
		if o.ControllerOwned && w.OutputAge != nil && *w.OutputAge > 300 {
			f = append(f, HealthFinding{Code: "output_quiet", Severity: "info", TaskID: w.TaskID, Message: "No worker log output for over five minutes. Progress is unknown.", Action: "Inspect the task and check logs. Quiet output alone does not justify a restart."})
		}
	}
	if s != nil {
		for _, t := range s.Backlog.Tasks {
			if t.Deferred() && t.Status != "done" {
				f = append(f, HealthFinding{Code: "task_deferred", Severity: "info", TaskID: t.ID, Message: "The task belongs to later work.", Action: t.UnblockWhen})
			} else if t.Status == "blocked" {
				f = append(f, HealthFinding{Code: "task_blocked", Severity: "warning", TaskID: t.ID, Message: "The task reports an unmet prerequisite.", Action: "Read blockers with looper inspect " + t.ID + " --json."})
			}
			// The circular warning rides beside the class finding, deferral
			// included: a deferred task can still record a condition only
			// its own implementation can satisfy. Doctor reports it and
			// changes nothing — the operator edits the task file.
			if condition := circularResumeCondition(t); condition != "" {
				f = append(f, HealthFinding{
					Code: findingTaskCircular, Severity: "warning", TaskID: t.ID,
					Message: fmt.Sprintf("The resume condition requires task %s itself, so no independent prerequisite can satisfy it: %s", t.ID, sanitize(condition)),
					Action:  "Record an independent prerequisite in the task file, or correct the task state; Looper imports the edit at a safe boundary. Cancel the task with looper cancel " + t.ID + " when the work is no longer needed.",
				})
			}
		}
		for _, a := range s.Attempts {
			if activeAttempt(a) && a.Repeats > 0 {
				f = append(f, HealthFinding{Code: "repeated_failure", Severity: "warning", TaskID: a.Task.ID, Message: "An attempt repeated a failure.", Action: "Inspect its feedback and checks before another repair."})
			}
		}
	}
	return f
}

func doctorJSON(c Config, out io.Writer) int {
	_, dir := projectPaths(c)
	findings := []HealthFinding{}
	var s *State
	var snap *StoreSnapshot
	store, err := openObservedStore(dir)
	if os.IsNotExist(err) {
		findings = append(findings, HealthFinding{Code: "no_saved_run", Severity: "info", Message: "No saved run exists.", Action: "Start looper in this project."})
	} else if err != nil {
		findings = append(findings, HealthFinding{Code: "state_unreadable", Severity: "error", Message: err.Error(), Action: "Check the state database path and permissions."})
	} else {
		defer store.Close()
		// One snapshot pairs the diagnosed state with its cursor (AX-003).
		snap, err = store.Snapshot()
		if err != nil {
			findings = append(findings, HealthFinding{Code: "state_unreadable", Severity: "error", Message: err.Error(), Action: "Retain the database and inspect the failure before restarting."})
		}
		if snap != nil {
			s = snap.State
		}
	}
	o := observe(c, s, snap)
	findings = append(findings, diagnose(o, s)...)
	if !o.ControllerOwned && s != nil && retained(s) {
		head, branch, unborn := headDetail(c.Root)
		if head != s.Head || branch != s.Branch {
			// The finding names the concrete difference (AX-015): the saved
			// and observed revisions, or an unborn HEAD stated as unborn
			// rather than as a branch that moved.
			if unborn {
				findings = append(findings, HealthFinding{Code: "branch_moved", Severity: "warning",
					Message: fmt.Sprintf("The project HEAD is unborn: branch %s has no commits, so the retained baseline %s on %s cannot be compared.", branchLabel(branch), s.Head, branchLabel(s.Branch)),
					Action:  "Make a commit on the saved branch or restore it, then resume; the retained work and its results stay unchanged. Read the saved baseline with: looper status --json"})
			} else {
				findings = append(findings, HealthFinding{Code: "branch_moved", Severity: "warning",
					Message: fmt.Sprintf("The project branch differs from the retained run baseline: expected %s on %s, observed %s on %s.", s.Head, branchLabel(s.Branch), head, branchLabel(branch)),
					Action: "Resume accepts a descendant commit on the same branch when committed and working files match the accepted or verified tree, or when the committed changes touch no file inside a retained or published attempt's write scope. " +
						"List the committed changes with: git diff --name-only " + s.Head + " " + head + ". Preserve other edits and reconcile them before resuming."})
			}
		}
		if store != nil {
			var id int64
			var raw string
			if store.db.QueryRow("SELECT id,data FROM events WHERE kind='shutdown' ORDER BY id DESC LIMIT 1").Scan(&id, &raw) == nil {
				var exit sessionExit
				if json.Unmarshal([]byte(raw), &exit) == nil {
					findings = append(findings, HealthFinding{Code: "last_shutdown", Severity: "info", Message: fmt.Sprintf("Session %s ended: %s (exit %d, event %d).", exit.Session, exit.Reason, exit.ExitCode, id), Action: "Read the shutdown event with looper events --json for the recorded cause."})
				}
			}
		}
	}
	if c.Contract == ContractVersionV2 {
		// The version 2 document adds the bounded request summaries beside
		// the same findings, and its exit code follows the transport
		// registry: findings are observation content, so a command that
		// completed exits 0 however unhealthy the run looks.
		requests := collectDoctorRequestsSummary(dir, s, wallClock())
		doc := &doctorCompactDocument{
			ContractVersion:   ContractVersionV2,
			SchemaVersion:     doctorCompactSchemaVersion,
			Kind:              doctorCompactKind,
			Observation:       o,
			Findings:          doctorFindingViews(findings, s),
			Requests:          requests,
			AutomaticRecovery: false,
			Detail: doctorDetailRoutes{
				Requests: "looper requests --contract v2 --json",
				Status:   "looper status --contract v2 --json",
			},
		}
		if json.NewEncoder(out).Encode(doc) != nil {
			return 1
		}
		return exitOK
	}
	code := 0
	for _, f := range findings {
		if f.Severity == "error" {
			code = 1
		}
	}
	result := struct {
		Version           int             `json:"schema_version"`
		Observation       Observation     `json:"observation"`
		Findings          []HealthFinding `json:"findings"`
		AutomaticRecovery bool            `json:"automatic_recovery"`
	}{observationVersion, o, findings, false}
	if json.NewEncoder(out).Encode(result) != nil {
		return 1
	}
	return code
}

// The version 2 doctor document contract,
// docs/contracts/schemas/v2/doctor.schema.json (AX-010).
const (
	doctorCompactSchemaVersion = 2
	doctorCompactKind          = "looper.doctor"
	// doctorWaitingOnMax bounds the waiting-request entries the summary
	// carries; the count of what was omitted rides beside them.
	doctorWaitingOnMax = 8
)

// doctorCompactDocument is the doctor document under contract version 2:
// the same observation and findings shape the legacy document carries, plus
// the bounded pending-request summaries (AX-010), the structured block
// reasons (OW-10), and their detail routes.
type doctorCompactDocument struct {
	ContractVersion   int                   `json:"contract_version"`
	SchemaVersion     int                   `json:"schema_version"`
	Kind              string                `json:"kind"`
	Observation       Observation           `json:"observation"`
	Findings          []doctorFindingView   `json:"findings"`
	Requests          doctorRequestsSummary `json:"requests"`
	AutomaticRecovery bool                  `json:"automatic_recovery"`
	Detail            doctorDetailRoutes    `json:"detail"`
}

// doctorFindingView is the version 2 finding projection: the shared finding
// plus the structured reason of a blocked task (OW-10). The legacy document
// keeps the shared HealthFinding shape unchanged.
type doctorFindingView struct {
	Code        string `json:"code"`
	Severity    string `json:"severity"`
	TaskID      string `json:"task_id,omitempty"`
	Message     string `json:"message"`
	Action      string `json:"suggested_action"`
	ReasonClass string `json:"reason_class,omitempty"`
	ReasonCode  string `json:"reason_code,omitempty"`
	Retry       string `json:"retry,omitempty"`
	AttemptID   string `json:"attempt_id,omitempty"`
}

// doctorFindingViews projects findings for the version 2 document, joining
// each blocked-task finding to the structured reason the saved state holds.
// Operator and deferral classes are expected waiting: their default version
// 2 severity is informational, so a monitor does not page on them. A
// circular condition is never expected waiting, so its warning survives the
// downgrade.
func doctorFindingViews(findings []HealthFinding, s *State) []doctorFindingView {
	views := make([]doctorFindingView, 0, len(findings))
	for _, f := range findings {
		v := doctorFindingView{Code: f.Code, Severity: f.Severity, TaskID: f.TaskID, Message: f.Message, Action: f.Action}
		if f.TaskID != "" && s != nil {
			if t := s.Backlog.Task(f.TaskID); t != nil {
				if r := t.effectiveBlockReason(); r != nil {
					v.ReasonClass = r.Class
					v.ReasonCode = clampJSONString(r.Code, 64)
					v.Retry = r.Retry
					v.AttemptID = clampJSONString(r.AttemptID, 64)
					if v.Severity == "warning" && f.Code != findingTaskCircular {
						switch r.Class {
						case blockClassOperator, blockClassDeferred:
							v.Severity = "info"
						}
					}
				}
			}
		}
		views = append(views, v)
	}
	return views
}

// doctorRequestsSummary names the pending work the run owes an outcome to:
// queued envelopes, waiting durable requests, the oldest of them, and one
// bounded list of what each waits for.
type doctorRequestsSummary struct {
	Pending          int                    `json:"pending"`
	Waiting          int                    `json:"waiting,omitempty"`
	OldestAgeSeconds *int                   `json:"oldest_age_seconds,omitempty"`
	WaitingOn        []doctorWaitingRequest `json:"waiting_on"`
	WaitingOmitted   int                    `json:"waiting_omitted,omitempty"`
}

// doctorWaitingRequest is one non-terminal request's diagnosis line.
type doctorWaitingRequest struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	State      string `json:"state"`
	Reason     string `json:"reason"`
	AgeSeconds *int   `json:"age_seconds,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

type doctorDetailRoutes struct {
	Requests string `json:"requests"`
	Status   string `json:"status"`
}

// collectDoctorRequestsSummary collects the bounded pending-request summary
// from the same durable evidence the request queries read. The oldest
// entries come first: a bounded summary keeps exactly the requests a caller
// must act on.
func collectDoctorRequestsSummary(dir string, s *State, now time.Time) doctorRequestsSummary {
	pending, waiting, oldest := pendingRequestSummary(dir, s, now)
	summary := doctorRequestsSummary{Pending: pending, Waiting: waiting, OldestAgeSeconds: oldest, WaitingOn: []doctorWaitingRequest{}}
	ev := collectRequestEvidence(dir, s, now)
	var outstanding []*requestQueryRecord
	for _, rec := range ev.records {
		if !requestStateTerminal(rec.state) {
			outstanding = append(outstanding, rec)
		}
	}
	sort.SliceStable(outstanding, func(i, j int) bool {
		return outstanding[i].ageBase().Before(outstanding[j].ageBase())
	})
	shown := 0
	for _, rec := range outstanding {
		if shown >= doctorWaitingOnMax {
			break
		}
		reason, detail := rec.waitReason, rec.waitDetail
		if reason == "" {
			switch rec.state {
			case RequestSubmitted:
				reason = "submitted"
			case RequestAccepted:
				reason = "accepted"
			default:
				reason = "unknown"
			}
		}
		entry := doctorWaitingRequest{
			ID: clampJSONString(rec.id, maxErrorString), Type: clampJSONString(rec.typ, maxErrorString),
			State: rec.state, Reason: clampJSONString(reason, maxErrorString), Detail: clampJSONString(detail, maxErrorString),
		}
		if age := rec.ageSeconds(); age > 0 || !rec.ageBase().IsZero() {
			entry.AgeSeconds = &age
		}
		summary.WaitingOn = append(summary.WaitingOn, entry)
		shown++
	}
	summary.WaitingOmitted = max(0, len(outstanding)-shown)
	return summary
}

// Summaries omit arguments, tool inputs, and reasoning. Raw evidence stays
// local. A failing tool's own error line rides beside the action (AX-021), so
// a reader can judge severity from the journal without opening the raw
// session log.
func workerActivity(line []byte) []string {
	var v struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
		Message struct {
			Content []struct {
				Type    string          `json:"type"`
				Name    string          `json:"name"`
				IsError bool            `json:"is_error"`
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"message"`
		Item struct {
			Type     string `json:"type"`
			ExitCode *int   `json:"exit_code"`
		} `json:"item"`
	}
	if json.Unmarshal(line, &v) != nil {
		return nil
	}
	var actions []string
	for _, b := range v.Message.Content {
		if b.Type == "tool_use" {
			actions = append(actions, "Tool started: "+sanitize(b.Name))
		}
		if b.Type == "tool_result" && b.IsError {
			action := "Tool reported an error"
			if excerpt := toolErrorExcerpt(b.Content); excerpt != "" {
				action += ": " + excerpt
			}
			actions = append(actions, action)
		}
	}
	if v.Item.Type == "command_execution" {
		if v.Type == "item.completed" && v.Item.ExitCode != nil {
			actions = append(actions, fmt.Sprintf("Command completed (exit %d)", *v.Item.ExitCode))
		} else {
			actions = append(actions, "Command started")
		}
	}
	if v.Type == "result" || v.Type == "turn.completed" {
		if v.IsError {
			actions = append(actions, "Agent reported an error")
		} else {
			actions = append(actions, "Agent returned a result")
		}
	}
	return actions
}

// Only the owner writes the journal. Work remains independent of observers.
func (e *Engine) pollWorkerActivity() {
	if time.Since(e.lastObservation) < time.Second {
		return
	}
	e.lastObservation = time.Now()
	if e.workerReaders == nil {
		e.workerReaders = map[string]*plannerActivityReader{}
	}
	live := map[string]bool{}
	for _, a := range e.State.Attempts {
		if !activeAttempt(a) {
			continue
		}
		for _, p := range attemptLogs(a) {
			if filepath.Ext(p) != ".jsonl" {
				continue
			}
			if e.controller != nil {
				start, _ := time.Parse(time.RFC3339, e.controller.Started)
				// Filesystem mtime granularity is coarser than the started
				// stamp's nanoseconds, so a log created in the first
				// milliseconds of the session reads as older than the session
				// itself. Truncating to the second keeps those logs visible.
				start = start.Truncate(time.Second)
				if st, err := os.Stat(p); err != nil || st.ModTime().Before(start) {
					continue
				}
			}
			live[p] = true
			r := e.workerReaders[p]
			if r == nil {
				r = &plannerActivityReader{decode: workerActivity}
				e.workerReaders[p] = r
			}
			r.poll(p, func(action string) {
				session := ""
				if e.controller != nil {
					session = e.controller.Session
				}
				id, err := e.Store.Event("worker_activity", a.Task.ID+" "+action, map[string]string{
					"session": session, "task_id": a.Task.ID, "attempt_id": a.ID, "phase": a.Phase, "log": p, "action": action,
				})
				if err != nil {
					e.err = err
					return
				}
				e.eventCursor = id
			})
		}
	}
	for p := range e.workerReaders {
		if !live[p] {
			delete(e.workerReaders, p)
		}
	}
	e.viewDirty = true
}

type CheckObservation struct {
	Time      string `json:"time"`
	AttemptID string `json:"attempt_id"`
	Revision  string `json:"revision"`
	Kind      string `json:"kind"`
	Result    string `json:"result"`
	Artifact  string `json:"artifact"`
}

func taskChecks(store *Store, attempts []*Attempt) ([]CheckObservation, error) {
	out := []CheckObservation{}
	for _, a := range attempts {
		rows, err := store.db.Query("SELECT time,attempt_id,revision,kind,result,artifact FROM checks WHERE attempt_id=? ORDER BY id", a.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c CheckObservation
			if err = rows.Scan(&c.Time, &c.AttemptID, &c.Revision, &c.Kind, &c.Result, &c.Artifact); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
