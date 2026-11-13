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
)

// The recovery preview contract, docs/contracts/schemas/v2/recovery-preview.schema.json.
const (
	recoveryPreviewSchemaVersion = 2
	recoveryPreviewKind          = "looper.recovery.preview"
	// recoveryPreviewBytesBudget bounds one preview document. The document is
	// evidence for a decision, not the decision itself: every collection it
	// carries stays small and names the route that returns the rest.
	recoveryPreviewBytesBudget = 8192
	// maxRecoveryChoices bounds the enumerated recovery choices.
	maxRecoveryChoices = 8
)

// Guard codes the preview reports. The retained-work codes match the guards
// AX-015 documents so a caller correlates a preview with the refusal it
// explains; the task-file codes name the startup failures that precede them.
const (
	guardTaskFileMissing    = "task_file_missing"
	guardTaskFileInvalid    = "task_file_invalid"
	guardBacklogChanged     = "backlog_contract_changed"
	guardBranchMoved        = "branch_moved"
	guardProjectTreeChanged = "project_tree_changed"
	guardTaskContract       = "task_contract_changed"
	guardPublicationBase    = "publication_base_mismatch"
)

// Guard verdicts.
const (
	verdictPass    = "pass"
	verdictFail    = "fail"
	verdictUnknown = "unknown"
)

// recoveryRevisionMax bounds the digest- and revision-shaped fields the
// schema constrains to hex. Their pattern already caps any valid value at 64
// characters, and hex never escapes, so this clamp touches only recorded
// values the pattern would reject anyway — it keeps them from reaching the
// document unbounded without ever cutting a legitimate digest.
const recoveryRevisionMax = 64

// clampRevision bounds one digest- or revision-shaped value by its pattern
// maximum in emitted bytes.
func clampRevision(s string) string { return clampJSONString(s, recoveryRevisionMax) }

// recoveryFit names the bounds one rendering pass applies. A document over
// the byte budget renders again with the next fit: strings shrink first and
// bounded collections drop entries only when that is not enough. Every
// string bound counts the bytes the JSON encoder emits, so escape-heavy
// saved content — an ampersand becomes six bytes — shrinks with the ladder
// instead of expanding past the fit that stated it. The ladder ends in
// recoveryTerminalFit, whose strings are one rune at worst, so a valid
// document always fits whatever the saved records hold.
type recoveryFit struct {
	stringMax int
	pathsMax  int
	tasksMax  int
}

// recoveryTerminalFit is the ladder's last resort. Its string bound leaves
// every emitted string a single rune at worst, so the document it renders is
// structure alone — a size fixed by the contract's fields, not by how large
// or how escape-heavy the saved records grew. The recovery terminal-fit test
// pins that worst case under the byte budget.
var recoveryTerminalFit = recoveryFit{stringMax: clampJSONStringMin, pathsMax: 2, tasksMax: 2}

var recoveryFits = []recoveryFit{
	{256, maxGuardChangedPaths, maxGuardTasks},
	{160, 12, 12},
	{96, 8, 8},
	{64, 4, 4},
	{48, 2, 2},
	recoveryTerminalFit,
}

// recoveryPreviewDocument is the one JSON document `looper recovery
// --preview --contract v2 --json` returns. It compares the current task
// file, branch, working tree, and retained publication against the saved
// contract, reports which startup guard each comparison feeds, and enumerates
// the supported recovery choices with what each preserves or changes. The
// token binds the preview to what it observed, so the checked application
// operation rejects a stale preview instead of replaying it over changed
// work.
type recoveryPreviewDocument struct {
	ContractVersion int                   `json:"contract_version"`
	SchemaVersion   int                   `json:"schema_version"`
	Kind            string                `json:"kind"`
	GeneratedAt     string                `json:"generated_at"`
	Project         recoveryProject       `json:"project"`
	Run             recoveryRunState      `json:"run"`
	Controller      recoveryControllerRef `json:"controller"`
	Token           recoveryTokenRef      `json:"token"`
	Comparisons     recoveryComparisons   `json:"comparisons"`
	Guards          []recoveryGuard       `json:"guards"`
	Choices         []recoveryChoice      `json:"choices"`
	Limits          recoveryLimitsRef     `json:"limits"`
	Detail          recoveryDetailRef     `json:"detail"`
}

type recoveryProject struct {
	Root       string `json:"root"`
	TodoPath   string `json:"todo_path"`
	RuntimeDir string `json:"runtime_dir"`
}

type recoveryRunState struct {
	ID                string               `json:"id"`
	Retained          bool                 `json:"retained"`
	RetainedTasks     []string             `json:"retained_tasks"`
	RetainedTaskTotal int                  `json:"retained_task_total"`
	Attempts          []recoveryAttemptRef `json:"attempts"`
	AttemptTotal      int                  `json:"attempt_total"`
	PublicationHeld   bool                 `json:"publication_held"`
}

// recoveryAttemptRef names one retained attempt with the evidence the
// recovery decision needs: whether the worktree and the durable receipt still
// exist, and whether the saved task contract still matches the backlog.
type recoveryAttemptRef struct {
	AttemptID       string `json:"attempt_id"`
	TaskID          string `json:"task_id"`
	Phase           string `json:"phase"`
	Agent           string `json:"agent,omitempty"`
	ResultCommit    string `json:"result_commit,omitempty"`
	WorktreePresent bool   `json:"worktree_present"`
	ReceiptPresent  bool   `json:"receipt_present"`
	ContractHeld    bool   `json:"contract_held"`
}

type recoveryControllerRef struct {
	State   string `json:"state"` // owning, stopped, or unknown
	Owned   bool   `json:"owned"`
	Session string `json:"session,omitempty"`
}

type recoveryTokenRef struct {
	Value string `json:"value"`
	Note  string `json:"note"`
}

// boundedListing is one bounded path listing with its complete count and the
// read-only command that returns every path. An empty listing always means
// "no difference"; a comparison that could not run leaves the listing absent
// and states its reason instead.
type boundedListing struct {
	Paths   []string `json:"paths"`
	Count   int      `json:"count"`
	Command string   `json:"command,omitempty"`
}

type recoveryComparisons struct {
	TaskFile    recoveryTaskFileComparison    `json:"task_file"`
	Branch      recoveryBranchComparison      `json:"branch"`
	WorkingTree recoveryWorkingTreeComparison `json:"working_tree"`
	Publication *recoveryPublicationPreview   `json:"publication,omitempty"`
}

type recoveryTaskFileComparison struct {
	State            string   `json:"state"` // match, changed, missing, unreadable, or invalid
	ExpectedDigest   string   `json:"expected_digest,omitempty"`
	ObservedDigest   string   `json:"observed_digest,omitempty"`
	Parsed           string   `json:"parsed,omitempty"` // identical, differs, or unknown
	ChangedContracts []string `json:"changed_contracts"`
	AddedContracts   []string `json:"added_contracts"`
	RemovedContracts []string `json:"removed_contracts"`
	DataChanges      []string `json:"task_data_changes,omitempty"`
	NonTaskChange    bool     `json:"non_task_change,omitempty"`
	Unknown          string   `json:"unknown,omitempty"`
}

type recoveryBranchComparison struct {
	State            string          `json:"state"` // match, recoverable, moved, unborn, detached, unreadable, or unknown
	ExpectedHead     string          `json:"expected_head,omitempty"`
	ExpectedBranch   string          `json:"expected_branch,omitempty"`
	ObservedHead     string          `json:"observed_head,omitempty"`
	ObservedBranch   string          `json:"observed_branch,omitempty"`
	Unborn           bool            `json:"unborn,omitempty"`
	Reason           string          `json:"reason,omitempty"`
	CommittedChanges *boundedListing `json:"committed_changes,omitempty"`
	StagedFiles      *boundedListing `json:"staged_files,omitempty"`
	Unknown          string          `json:"unknown,omitempty"`
}

type recoveryWorkingTreeComparison struct {
	State          string          `json:"state"` // matches_published, changed, or unknown
	PublishedTree  string          `json:"published_tree,omitempty"`
	ChangedPaths   *boundedListing `json:"changed_paths,omitempty"`
	UntrackedPaths *boundedListing `json:"untracked_paths,omitempty"`
	ChangedTotal   int             `json:"changed_total,omitempty"`
	Basis          string          `json:"basis,omitempty"`
	Unknown        string          `json:"unknown,omitempty"`
	// OwnerCommitAccepted reports the T046 fold: the changed paths belong to a
	// descendant owner commit that touches no retained write scope, so the
	// startup tree check compares the working tree against the folded baseline
	// and passes.
	OwnerCommitAccepted bool `json:"owner_commit_accepted,omitempty"`
}

type recoveryPublicationPreview struct {
	VerifiedCommit string          `json:"verified_commit"`
	AttemptID      string          `json:"attempt_id,omitempty"`
	TaskID         string          `json:"task_id,omitempty"`
	AcceptedTree   string          `json:"accepted_tree"`
	VerifiedTree   string          `json:"verified_tree"`
	Outcome        string          `json:"outcome"` // applies_at_resume, already_applied, collision, base_mismatch, or unknown
	Changes        *boundedListing `json:"changes,omitempty"`
	LocalEdits     *boundedListing `json:"local_edits,omitempty"`
	Unknown        string          `json:"unknown,omitempty"`
}

type recoveryGuard struct {
	Code    string `json:"code"`
	Verdict string `json:"verdict"`
	Detail  string `json:"detail,omitempty"`
	Command string `json:"command,omitempty"`
}

type recoveryChoice struct {
	Name            string         `json:"name"`
	Status          string         `json:"status"` // available, blocked, unsafe, or unsupported
	Preserves       []string       `json:"preserves,omitempty"`
	Changes         []string       `json:"changes,omitempty"`
	BlockedBy       []string       `json:"blocked_by,omitempty"`
	MissingEvidence []string       `json:"missing_evidence,omitempty"`
	Apply           *recoveryApply `json:"apply,omitempty"`
}

type recoveryApply struct {
	Mode        string `json:"mode"` // command, operator, or none
	Description string `json:"description"`
	Command     string `json:"command,omitempty"`
}

type recoveryLimitsRef struct {
	BytesBudget int            `json:"bytes_budget"`
	StringMax   int            `json:"string_max"`
	PathsMax    int            `json:"paths_max"`
	TasksMax    int            `json:"tasks_max"`
	Omitted     map[string]int `json:"omitted"`
}

type recoveryDetailRef struct {
	Status string `json:"status,omitempty"`
	Events string `json:"events,omitempty"`
}

// gitRead runs one git query with optional locks disabled, so a read-only
// command never refreshes the index or creates a lock file. Every git call
// the preview makes goes through here or through calls that only resolve
// references.
func gitRead(cwd string, args ...string) (string, error) {
	b, err := gitCommand(cwd, nil, map[string]string{"GIT_OPTIONAL_LOCKS": "0"}, args...)
	return strings.TrimSpace(string(b)), err
}

// repositoryOwnedNoCreate reports controller ownership without creating the
// repository lock file. The shared repositoryOwned probe opens the lock with
// O_CREATE, which would be a write; the preview resolves the common
// directory read-only and probes an existing lock with a flock that never
// writes.
func repositoryOwnedNoCreate(c Config) bool {
	common, err := gitRead(c.Root, "rev-parse", "--git-common-dir")
	if err != nil {
		return false
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(c.Root, common)
	}
	return ownsRepositoryNoCreate(filepath.Join(common, "looper.lock"))
}

// readPaths lists the paths of one read-only git query. The query must use
// the -z separator so a path can never split on a newline.
func readPaths(cwd string, args ...string) ([]string, error) {
	out, err := gitRead(cwd, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// recoveryEvidence is every read the preview performed. Reads never write:
// the store is opened read-only, the git queries take no optional locks, and
// no controller, agent, or lock is created. What the reads observed can still
// change before a caller acts, which is exactly what the token records.
type recoveryEvidence struct {
	c    Config
	dir  string
	repo *Repository
	snap *StoreSnapshot
	s    *State
	now  time.Time

	// repoErr explains why repository comparisons cannot run.
	repoErr error

	owned bool
	info  *ControllerInfo

	// observed file and reference state
	todoData   []byte
	todoErr    error
	todoDigest string
	head       string
	branch     string
	unborn     bool

	// foldAccepted records that the branch comparison accepted the moved head
	// through the T046 fold, with its note; later comparisons report against
	// the folded baseline it implies.
	foldAccepted bool
	foldNote     string

	// importsHeldEdit records that the task-file guard expects the T058
	// startup import: the file changed and every retained attempt holds a
	// replayable result. Later comparisons then report against the observed
	// file's backlog, exactly as the resume does after it imports the edit.
	importsHeldEdit bool

	// replaySafe records whether every retained attempt holds a result that
	// replays after an import (T058), the gate the import-backlog choice
	// reports. It is computed beside the guard that decides it.
	replaySafe bool

	// importBreaksContracts lists the retained tasks whose contract the
	// observed edit changes, so the import choice refuses exactly what the
	// contract guard refuses: importing such an edit strands the retained
	// work at the next startup's contract check.
	importBreaksContracts []string

	// Full listings; bounded when the document renders.
	committedChanges []string
	staged           []string
	workingChanges   []string
	untracked        []string
	publicationDiff  []string
	localEdits       []string
}

// collectRecoveryEvidence performs the bounded reads a preview needs. Every
// failure stays evidence: an unreadable file or a failed comparison is
// recorded with its reason, never turned into an invented result.
func collectRecoveryEvidence(c Config, dir string, snap *StoreSnapshot) (*recoveryEvidence, error) {
	return collectRecoveryEvidenceFor(c, dir, snap, snap.State)
}

// collectRecoveryEvidenceFor is the same read against an explicit state. The
// preview passes the committed snapshot state; the checked application
// (AX-017) passes the live engine state, so the token it re-derives covers
// the exact state the application is about to change.
func collectRecoveryEvidenceFor(c Config, dir string, snap *StoreSnapshot, s *State) (*recoveryEvidence, error) {
	ev := &recoveryEvidence{c: c, dir: dir, snap: snap, s: s, now: time.Now()}
	// The repository handle resolves the same exclusions the startup guards
	// use, without initializing anything: GitInit stays off so a preview on a
	// broken or missing repository reports evidence instead of creating one.
	preview := c
	preview.GitInit = false
	if ev.repo, ev.repoErr = OpenRepository(preview, dir); ev.repo != nil {
		ev.head, ev.branch, ev.unborn = ev.repo.HeadDetail()
	}
	ev.todoData, ev.todoErr = os.ReadFile(c.Todo)
	ev.todoDigest = digest(ev.todoData)
	ev.owned = repositoryOwnedNoCreate(c)
	if info, err := readController(dir); err == nil {
		ev.info = info
	}
	if ev.s != nil && ev.repo != nil {
		ev.collectListings()
	}
	return ev, nil
}

// collectListings gathers the path listings the comparisons report. Each is
// one read-only git query filtered by the same exclusions the engine's
// snapshot applies, so the preview compares what the guard would compare.
func (ev *recoveryEvidence) collectListings() {
	s, r := ev.s, ev.repo
	excludeArgs := []string{"--", "."}
	for _, path := range r.Excluded {
		excludeArgs = append(excludeArgs, ":(exclude)"+path)
	}
	keep := func(paths []string) []string {
		var kept []string
		for _, p := range paths {
			if !r.excluded(p) {
				kept = append(kept, p)
			}
		}
		return kept
	}
	if s.Head != "" && ev.head != "" && ev.head != s.Head {
		if paths, err := readPaths(r.Root, append([]string{"diff", "--name-only", "-z", s.Head, ev.head}, excludeArgs...)...); err == nil {
			ev.committedChanges = keep(paths)
		}
	}
	if ev.head != "" {
		if paths, err := readPaths(r.Root, append([]string{"diff", "--cached", "--name-only", "-z", ev.head}, excludeArgs...)...); err == nil {
			ev.staged = keep(paths)
		}
	}
	if s.Published != "" {
		if paths, err := readPaths(r.Root, append([]string{"diff", "--name-only", "-z", s.Published}, excludeArgs...)...); err == nil {
			ev.workingChanges = keep(paths)
		}
		if paths, err := readPaths(r.Root, "ls-files", "--others", "--exclude-standard", "-z"); err == nil {
			ev.untracked = keep(paths)
		}
	}
	if p := s.Publication; p != nil {
		if paths, err := readPaths(r.Root, "diff", "--name-only", "-z", p.Before, p.Tree); err == nil {
			ev.publicationDiff = paths
		}
		if paths, err := readPaths(r.Root, append([]string{"diff", "--name-only", "-z", p.Before}, excludeArgs...)...); err == nil {
			ev.localEdits = keep(paths)
		}
	}
}

// buildRecoveryPreview renders the preview within its byte budget.
func buildRecoveryPreview(c Config, dir string, snap *StoreSnapshot) (*recoveryPreviewDocument, error) {
	ev, err := collectRecoveryEvidence(c, dir, snap)
	if err != nil {
		return nil, err
	}
	var doc *recoveryPreviewDocument
	for _, fit := range recoveryFits {
		doc = renderRecoveryDocument(ev, fit)
		if recoveryEncoded(doc) <= recoveryPreviewBytesBudget {
			return doc, nil
		}
	}
	return doc, nil
}

// recoveryEncoded reports the size of the document as the CLI emits it.
func recoveryEncoded(doc *recoveryPreviewDocument) int {
	var b strings.Builder
	_ = json.NewEncoder(&b).Encode(doc)
	return b.Len()
}

// listing renders one full listing under the fit bounds, keeping the
// complete count and the read-only command that returns every path. The
// command embeds recorded digests, so it is clamped like the paths: a
// truncated hint stays a hint, never an unbounded one.
func (ev *recoveryEvidence) listing(paths []string, fit recoveryFit, command string) *boundedListing {
	shown, total := paths, len(paths)
	if len(shown) > fit.pathsMax {
		shown = shown[:fit.pathsMax]
	}
	out := &boundedListing{Count: total, Command: clampJSONString(command, fit.stringMax), Paths: []string{}}
	for _, p := range shown {
		out.Paths = append(out.Paths, clampJSONString(p, fit.stringMax))
	}
	return out
}

// renderRecoveryDocument renders the evidence into one bounded document.
// Every string bound counts the bytes the JSON encoder emits, so escape-heavy
// saved content shrinks with the ladder. Plain text renders from the same
// document, so the two outputs cannot disagree.
func renderRecoveryDocument(ev *recoveryEvidence, fit recoveryFit) *recoveryPreviewDocument {
	clamp := func(s string) string { return clampJSONString(s, fit.stringMax) }
	doc := &recoveryPreviewDocument{
		ContractVersion: ContractVersionV2,
		SchemaVersion:   recoveryPreviewSchemaVersion,
		Kind:            recoveryPreviewKind,
		GeneratedAt:     ev.now.UTC().Format(time.RFC3339),
	}
	doc.Project = recoveryProject{Root: clamp(ev.c.Root), TodoPath: clamp(ev.c.Todo), RuntimeDir: clamp(ev.dir)}
	s := ev.s
	doc.Run = recoveryRunState{
		ID: clamp(runIdentifier(s, ev.info)), Retained: retained(s),
		RetainedTasks: []string{}, Attempts: []recoveryAttemptRef{},
	}
	ids, total := retainedTaskIDs(s)
	for _, id := range ids {
		doc.Run.RetainedTasks = append(doc.Run.RetainedTasks, clamp(id))
	}
	doc.Run.RetainedTaskTotal = total
	doc.Run.PublicationHeld = s.Publication != nil
	for _, a := range s.Attempts {
		if a.Phase == "done" || a.Phase == "blocked" || a.Phase == "cancelled" {
			continue
		}
		doc.Run.AttemptTotal++
		if len(doc.Run.Attempts) >= fit.tasksMax {
			continue
		}
		t := s.Backlog.Task(a.Task.ID)
		ref := recoveryAttemptRef{
			AttemptID: clamp(a.ID), TaskID: clamp(a.Task.ID), Phase: clamp(a.Phase), Agent: clamp(a.Agent),
			ResultCommit:    clampRevision(a.ResultCommit),
			WorktreePresent: func() bool { _, err := os.Stat(a.Worktree); return err == nil }(),
			ReceiptPresent: func() bool {
				var r resultReceipt
				return readJSON(filepath.Join(a.Directory, "receipt.json"), &r) == nil
			}(),
			ContractHeld: t != nil && t.Contract() == a.Task.Contract(),
		}
		doc.Run.Attempts = append(doc.Run.Attempts, ref)
	}
	// Controller ownership decides whether any recovery can apply here.
	doc.Controller = recoveryControllerRef{State: "unknown", Owned: ev.owned}
	if ev.info != nil {
		doc.Controller.Session = clamp(ev.info.Session)
	}
	switch {
	case ev.owned && ev.info != nil && controllerAlive(ev.info):
		doc.Controller.State = "owning"
	case ev.owned:
		doc.Controller.State = "unknown" // a lock is held but no live owner is identifiable
	default:
		doc.Controller.State = "stopped"
	}
	doc.Token = recoveryTokenRef{Value: recoveryToken(ev), Note: clamp("Present this token to the recovery application operation. Applying re-derives it and rejects a changed task file, project, or saved run instead of replaying a stale preview.")}
	doc.Comparisons = recoveryComparisons{
		TaskFile:    ev.taskFileComparison(clamp),
		Branch:      ev.branchComparison(fit, clamp),
		WorkingTree: ev.workingTreeComparison(fit, clamp),
		Publication: ev.publicationPreview(fit, clamp),
	}
	// The replay gate the import-backlog choice reports is the same one the
	// guard applies (T058), computed once so the two cannot drift.
	ev.replaySafe = retained(s) && replaySafeRetainedWork(s)
	doc.Guards = recoveryGuards(ev, doc.Comparisons, clamp)
	doc.Choices = recoveryChoices(ev, doc, clamp)
	doc.Limits = recoveryLimitsRef{
		BytesBudget: recoveryPreviewBytesBudget, StringMax: fit.stringMax,
		PathsMax: fit.pathsMax, TasksMax: fit.tasksMax,
		Omitted: map[string]int{
			"retained_tasks": max(0, total-fit.tasksMax),
			"attempts":       max(0, doc.Run.AttemptTotal-len(doc.Run.Attempts)),
		},
	}
	doc.Detail = recoveryDetailRef{
		Status: "looper status --contract v2 --full",
		Events: fmt.Sprintf("looper events --contract v2 --after %d", ev.snap.HighWater),
	}
	return doc
}

// taskFileComparison compares the task-file bytes and the parsed backlog
// against the saved contract, reusing the AX-015 comparison so the preview
// and the guard describe the same edit the same way.
func (ev *recoveryEvidence) taskFileComparison(clamp func(string) string) recoveryTaskFileComparison {
	s := ev.s
	out := recoveryTaskFileComparison{
		ExpectedDigest:   clampRevision(s.ExportHash),
		ChangedContracts: []string{}, AddedContracts: []string{}, RemovedContracts: []string{},
	}
	switch {
	case ev.todoErr != nil && os.IsNotExist(ev.todoErr):
		out.State = "missing"
		return out
	case ev.todoErr != nil:
		out.State = "unreadable"
		out.Unknown = clamp("the task file: " + ev.todoErr.Error())
		return out
	}
	out.ObservedDigest = clampRevision(ev.todoDigest)
	if ev.todoDigest == s.ExportHash {
		out.State = "match"
		return out
	}
	out.State = "changed"
	// The preview parses exactly as startup does: duplicate keys, ambiguous
	// documents, and invalid content are rejected, never normalized (AX-018).
	if _, err := decodeBacklogStrict(ev.todoData); err != nil {
		// An edit that no longer parses or validates is a distinct state:
		// startup fails on the invalid backlog before any guard compares
		// contracts.
		out.State = "invalid"
		out.Parsed = "unknown"
		out.Unknown = clamp("the parsed task-file comparison: " + err.Error())
		return out
	}
	d := compareBacklog(s.Backlog, ev.todoData)
	out.ChangedContracts = boundIDs(d.Changed, clamp)
	out.AddedContracts = boundIDs(d.Added, clamp)
	out.RemovedContracts = boundIDs(d.Removed, clamp)
	out.DataChanges = boundIDs(d.Data, clamp)
	out.NonTaskChange = d.Other
	if d.Equal {
		out.Parsed = "identical"
	} else {
		out.Parsed = "differs"
	}
	return out
}

// boundIDs clamps an identifier listing to the guard task bound.
func boundIDs(ids []string, clamp func(string) string) []string {
	out := []string{}
	for _, id := range ids {
		if len(out) >= maxGuardTasks {
			break
		}
		out = append(out, clamp(id))
	}
	return out
}

// branchComparison reports the read-only branch check branch_recovery.go
// performs, or why it cannot run. Every recorded reference and reason is
// clamped by the fit's emitted-byte bound: saved digests and branch names
// are content, not literals the contract fixes.
func (ev *recoveryEvidence) branchComparison(fit recoveryFit, clamp func(string) string) recoveryBranchComparison {
	s := ev.s
	out := recoveryBranchComparison{
		ExpectedHead:   clampRevision(s.Head),
		ExpectedBranch: clamp(branchLabel(s.Branch)),
	}
	if ev.repoErr != nil {
		out.State = branchUnreadable
		out.Unknown = clamp(fmt.Sprintf("the repository comparisons: %v", ev.repoErr))
		return out
	}
	out.ObservedHead, out.ObservedBranch = clampRevision(ev.head), clamp(branchLabel(ev.branch))
	cmp := ev.repo.compareBranchRecovery(s, ev.head, ev.branch, ev.unborn)
	out.State, out.Reason = cmp.state, clamp(cmp.reason)
	// A recoverable branch with a reason names the T046 fold: the working-tree
	// and publication comparisons below report against the folded baseline it
	// implies instead of counting the accepted paths as conflicts.
	ev.foldAccepted = cmp.state == branchRecoverable && cmp.reason != ""
	ev.foldNote = clamp(cmp.reason)
	// The observed unborn state stays visible even when both sides are
	// unborn and therefore match: the engine accepts an unborn baseline
	// against an unborn HEAD, and the preview must not hide it.
	out.Unborn = ev.unborn
	if cmp.unknown != "" {
		out.Unknown = clamp(cmp.unknown)
	}
	// The committed-changes listing is the evidence for a head that moved;
	// the staged listing matters whenever the index holds entries.
	if cmp.state != branchMatch && len(ev.committedChanges) > 0 {
		out.CommittedChanges = ev.listing(ev.committedChanges, fit, fmt.Sprintf("git diff --name-only %s %s", s.Head, ev.head))
	}
	if len(ev.staged) > 0 {
		out.StagedFiles = ev.listing(ev.staged, fit, fmt.Sprintf("git diff --name-only --cached %s", ev.head))
	}
	return out
}

// workingTreeComparison compares the working tree against the tree the
// startup guard hashes. The read-only comparison lists every tracked,
// staged, and untracked non-ignored path that differs; the basis names what
// it covers so an empty listing is never read as more than it proves.
func (ev *recoveryEvidence) workingTreeComparison(fit recoveryFit, clamp func(string) string) recoveryWorkingTreeComparison {
	s := ev.s
	out := recoveryWorkingTreeComparison{PublishedTree: clampRevision(s.Published)}
	if ev.repoErr != nil {
		out.State = "unknown"
		out.Unknown = clamp(fmt.Sprintf("the working-tree comparison: %v", ev.repoErr))
		return out
	}
	if s.Published == "" {
		out.State = "unknown"
		out.Unknown = clamp("the working-tree comparison: the saved run records no published tree")
		return out
	}
	// A publication that startup can apply changes the tree the final check
	// compares against; the collision case leaves the check unreached. A
	// publication the fold requeues never applies as saved (T046), so the
	// check keeps the published tree as its basis.
	tree := s.Published
	if p := s.Publication; p != nil {
		outcome, unknown := ev.publicationOutcome()
		switch outcome {
		case publicationApplies, publicationApplied:
			tree = p.Tree
		case publicationCollision, publicationUnknown:
			out.State = "unknown"
			if outcome == publicationCollision {
				out.Unknown = clamp("the working-tree check: startup stops at the publication collision first")
			} else {
				out.Unknown = clamp("the working-tree check: " + unknown)
			}
			return out
		}
	}
	out.PublishedTree = clampRevision(tree)
	changes := ev.workingChanges
	basis := tree
	if tree != s.Published {
		// A publication that applies at resume changes the tree by design, so
		// its own paths are expected differences, never conflicts. The local
		// edits against the accepted tree are exactly the paths outside the
		// publication's changes: the accepted tree cannot hold the verified
		// paths the publication introduces.
		p := s.Publication
		paths, err := readPaths(ev.repo.Root, "diff", "--name-only", "-z", p.Before)
		if err != nil {
			out.State = "unknown"
			out.Unknown = clamp(fmt.Sprintf("the working-tree comparison against the accepted tree %s: %v", p.Before, err))
			return out
		}
		changes = filterExcluded(ev.repo, paths)
		basis = p.Before
	}
	if ev.foldAccepted {
		// The accepted owner commit is an expected difference, never a
		// conflict: the fold makes the folded working content the published
		// baseline at resume, and the startup tree check passes against it
		// (T046).
		out.State = "matches_published"
		out.OwnerCommitAccepted = true
		out.Basis = clamp("the accepted owner commit folds into the published tree at resume; the startup tree check then matches the folded baseline")
		return out
	}
	listing := ev.listing(changes, fit, fmt.Sprintf("git diff --name-only %s", basis))
	untracked := ev.listing(ev.untracked, fit, "git ls-files --others --exclude-standard")
	if listing.Count == 0 && untracked.Count == 0 {
		out.State = "matches_published"
		return out
	}
	out.State = "changed"
	out.ChangedPaths = listing
	out.UntrackedPaths = untracked
	// On a repository with an empty index, git diff against a tree already
	// lists untracked paths; the union keeps the count honest.
	seen := map[string]bool{}
	for _, p := range append(append([]string{}, changes...), ev.untracked...) {
		seen[p] = true
	}
	out.ChangedTotal = len(seen)
	out.Basis = clamp("tracked, staged, and untracked non-ignored paths; the startup guard additionally hashes ignored content that is tracked or recorded in the published tree")
	if tree != s.Published {
		out.Basis = clamp("edits outside the retained publication's own paths; the publication applies its verified paths at resume, so those are expected differences, not conflicts")
	}
	return out
}

// Publication outcomes.
const (
	publicationApplies      = "applies_at_resume"
	publicationApplied      = "already_applied"
	publicationCollision    = "collision"
	publicationBaseMismatch = "base_mismatch"
	publicationRequeued     = "requeued_at_resume"
	publicationUnknown      = "unknown"
)

// publicationOutcome derives the publication outcome from the saved candidate
// and the working-tree listings, mirroring the decision publish() makes. A
// query that cannot run keeps its reason: the outcome stays unknown instead
// of guessing a collision. An accepted owner commit requeues the publication
// against the folded baseline (T046), so the saved candidate never applies
// as-is at resume.
func (ev *recoveryEvidence) publicationOutcome() (outcome, unknown string) {
	p := ev.s.Publication
	if p == nil {
		return "", ""
	}
	if ev.foldAccepted {
		return publicationRequeued, ""
	}
	if p.Base != ev.s.Tip {
		return publicationBaseMismatch, ""
	}
	// A working tree that already matches the verified candidate publishes by
	// reference alone; one that matches the accepted tree receives the
	// verified content at resume.
	verified, err := readPaths(ev.repo.Root, "diff", "--name-only", "-z", p.Tree)
	if err != nil {
		return publicationUnknown, fmt.Sprintf("the working tree against the verified tree %s: %v", p.Tree, err)
	}
	if len(filterExcluded(ev.repo, verified)) == 0 {
		return publicationApplied, ""
	}
	if len(filterExcluded(ev.repo, ev.localEdits)) == 0 {
		return publicationApplies, ""
	}
	return publicationCollision, ""
}

func filterExcluded(r *Repository, paths []string) []string {
	var kept []string
	for _, p := range paths {
		if !r.excluded(p) {
			kept = append(kept, p)
		}
	}
	return kept
}

// publicationPreview reports the retained publication: what publishing
// changes, and whether startup can apply it.
func (ev *recoveryEvidence) publicationPreview(fit recoveryFit, clamp func(string) string) *recoveryPublicationPreview {
	s := ev.s
	p := s.Publication
	if p == nil {
		return nil
	}
	out := &recoveryPublicationPreview{
		VerifiedCommit: clampRevision(p.Commit), AttemptID: clamp(p.AttemptID),
		AcceptedTree: clampRevision(p.Before), VerifiedTree: clampRevision(p.Tree),
	}
	if a := attemptByID(s, p.AttemptID); a != nil {
		out.TaskID = clamp(a.Task.ID)
	}
	if ev.repoErr != nil {
		out.Outcome = publicationUnknown
		out.Unknown = clamp(fmt.Sprintf("the publication comparison: %v", ev.repoErr))
		return out
	}
	outcome, unknown := ev.publicationOutcome()
	out.Outcome = outcome
	switch outcome {
	case publicationBaseMismatch:
		out.Unknown = clamp("the publication base no longer matches the verified integration base; startup cannot apply this candidate")
	case publicationCollision:
		out.LocalEdits = ev.listing(ev.localEdits, fit, fmt.Sprintf("git diff --name-only %s", p.Before))
	case publicationUnknown:
		out.Unknown = clamp(unknown)
	}
	out.Changes = ev.listing(ev.publicationDiff, fit, fmt.Sprintf("git diff --name-only %s %s", p.Before, p.Tree))
	return out
}

// attemptByID finds one attempt by identifier in the saved state.
func attemptByID(s *State, id string) *Attempt {
	for _, a := range s.Attempts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// recoveryGuards evaluates the startup guard set in the order
// initializeState applies it. A guard that could not decide reports unknown:
// the preview never presents an undecided comparison as a pass.
func recoveryGuards(ev *recoveryEvidence, cmp recoveryComparisons, clamp func(string) string) []recoveryGuard {
	s := ev.s
	guards := []recoveryGuard{}
	fail := func(code, detail string) recoveryGuard {
		return recoveryGuard{Code: code, Verdict: verdictFail, Detail: clamp(detail)}
	}
	// The task file must exist and parse before any guard compares it.
	switch cmp.TaskFile.State {
	case "missing":
		guards = append(guards, fail(guardTaskFileMissing, fmt.Sprintf("the saved task file %s is missing; restore it before resuming", ev.c.Todo)))
	case "unreadable":
		// Startup cannot read the file at all, so it certainly refuses; the
		// reason it stayed unreadable rides beside the verdict.
		guards = append(guards, fail(guardTaskFileMissing, cmp.TaskFile.Unknown))
	case "invalid":
		guards = append(guards, fail(guardTaskFileInvalid, cmp.TaskFile.Unknown))
	default:
		switch {
		case cmp.TaskFile.State == "changed" && cmp.TaskFile.Parsed == "identical":
			// Equivalent formatting (AX-018): startup adopts the observed
			// bytes as the accepted export and resumes, so this guard passes
			// with or without retained work.
			guards = append(guards, recoveryGuard{Code: guardBacklogChanged, Verdict: verdictPass, Detail: clamp(fmt.Sprintf("the task file changed only in formatting (expected digest %s, observed %s); the next run adopts the equivalent bytes and resumes", s.ExportHash, cmp.TaskFile.ObservedDigest))})
		case cmp.TaskFile.State == "changed" && retained(s) && replaySafeRetainedWork(s):
			// The held import (T058): every retained attempt holds a result
			// that replays, so startup imports the edit instead of demanding a
			// restore — unless the edit changes a retained task's contract,
			// which every import path refuses with the restore demand the
			// mirror reports here (T119). The mirror stays read-only; it reuses
			// the same gates the startup guard applies.
			ev.importBreaksContracts = retainedContractBreakers(s, ev.todoData)
			if len(ev.importBreaksContracts) == 0 {
				ev.importsHeldEdit = true
				guards = append(guards, recoveryGuard{Code: guardBacklogChanged, Verdict: verdictPass, Detail: clamp("the task file changed while work was retained; every retained result replays, so the next run imports the edit at startup (T058)")})
			} else {
				guard := fail(guardBacklogChanged, fmt.Sprintf("the task file changed while work was retained and the edit changes the contract of retained task(s) %s, so the next run refuses the import and keeps the restore demand; expected digest %s, observed %s", boundedIDList(ev.importBreaksContracts, 8), s.ExportHash, cmp.TaskFile.ObservedDigest))
				guard.Command = "looper status --contract v2 --full"
				guards = append(guards, guard)
			}
		case cmp.TaskFile.State == "changed" && retained(s):
			detail := fmt.Sprintf("the task file changed while work was retained and no result can replay; expected digest %s, observed %s", s.ExportHash, cmp.TaskFile.ObservedDigest)
			guard := fail(guardBacklogChanged, detail)
			guard.Command = "looper status --contract v2 --full"
			guards = append(guards, guard)
		case cmp.TaskFile.State == "changed":
			guards = append(guards, recoveryGuard{Code: guardBacklogChanged, Verdict: verdictPass, Detail: clamp("no retained work; the changed task file starts a new baseline at the next run")})
		default:
			guards = append(guards, recoveryGuard{Code: guardBacklogChanged, Verdict: verdictPass, Detail: clamp("the task file matches the saved contract")})
		}
	}
	// The branch and tree guards exist to protect retained work. With none
	// retained, startup archives the saved run and starts a new baseline from
	// the current task file and tree, so neither comparison can refuse.
	if !retained(s) {
		detail := clamp("no retained work; the next run archives the saved state and starts a new baseline from the current task file and tree")
		guards = append(guards,
			recoveryGuard{Code: guardBranchMoved, Verdict: verdictPass, Detail: detail},
			recoveryGuard{Code: guardProjectTreeChanged, Verdict: verdictPass, Detail: detail})
		return guards
	}
	switch cmp.Branch.State {
	case branchMatch:
		guards = append(guards, recoveryGuard{Code: guardBranchMoved, Verdict: verdictPass, Detail: clamp("the branch and head match the saved baseline")})
	case branchRecoverable:
		detail := "startup accepts the descendant head because the committed and working content match the saved trees"
		if cmp.Branch.Reason != "" {
			// The T046 fold names what the acceptance covers.
			detail = cmp.Branch.Reason
		}
		guards = append(guards, recoveryGuard{Code: guardBranchMoved, Verdict: verdictPass, Detail: clamp(detail)})
	default:
		reason := cmp.Branch.Reason
		if reason == "" {
			// A comparison that could not run reports its reason in place of
			// a refusal cause, so the guard never carries an empty detail.
			reason = cmp.Branch.Unknown
		}
		guard := fail(guardBranchMoved, reason)
		if cmp.Branch.Unknown != "" {
			guard.Verdict = verdictUnknown
			guard.Command = "git rev-parse --verify HEAD"
		}
		if cmp.Branch.State == branchUnborn {
			guard.Detail = clamp(fmt.Sprintf("the project HEAD is unborn: branch %s has no commits, so the retained baseline %s cannot be compared", cmp.Branch.ObservedBranch, s.Head))
		}
		guards = append(guards, guard)
	}
	// The publication applies before the tree check: when its guard refuses
	// or stays unknown, startup stops there and the tree check never runs.
	publicationDecided := true
	if cmp.Publication != nil {
		switch cmp.Publication.Outcome {
		case publicationBaseMismatch:
			guards = append(guards, fail(guardPublicationBase, cmp.Publication.Unknown))
			publicationDecided = false
		case publicationCollision:
			guards = append(guards, fail(guardProjectTreeChanged, "project files match neither the accepted tree nor the verified candidate; the verified work stays retained"))
			publicationDecided = false
		case publicationUnknown:
			guards = append(guards, recoveryGuard{Code: guardProjectTreeChanged, Verdict: verdictUnknown, Detail: clamp(cmp.Publication.Unknown)})
			publicationDecided = false
		case publicationRequeued:
			guards = append(guards, recoveryGuard{Code: guardProjectTreeChanged, Verdict: verdictPass, Detail: clamp("the accepted owner commit moves the baseline; the verified result re-prepares and verifies against the folded base")})
		default:
			guards = append(guards, recoveryGuard{Code: guardProjectTreeChanged, Verdict: verdictPass, Detail: clamp("the retained publication applies at resume before the tree check")})
		}
	}
	switch {
	case !publicationDecided:
		// Startup stops at the publication step; the tree comparison stays
		// unknown rather than silently passing.
		guards = append(guards, recoveryGuard{Code: guardProjectTreeChanged, Verdict: verdictUnknown, Detail: clamp("the working-tree check: startup stops at the publication step first")})
	case cmp.WorkingTree.State == "matches_published":
		guards = append(guards, recoveryGuard{Code: guardProjectTreeChanged, Verdict: verdictPass, Detail: clamp("the working tree matches the tree the saved run published")})
	case cmp.WorkingTree.State == "changed":
		guard := fail(guardProjectTreeChanged, fmt.Sprintf("working files differ from the tree the saved run published (%d path(s))", cmp.WorkingTree.ChangedTotal))
		guard.Command = cmp.WorkingTree.ChangedPaths.Command
		guards = append(guards, guard)
	default:
		guards = append(guards, recoveryGuard{Code: guardProjectTreeChanged, Verdict: verdictUnknown, Detail: clamp(cmp.WorkingTree.Unknown)})
	}
	// Retained attempt contracts. When the held edit imports at startup (T058),
	// the contract check runs against the imported backlog, so the mirror
	// compares the observed file's tasks rather than the saved ones. An edit
	// the startup import refuses (T119) never reaches that comparison, so the
	// mirror keeps comparing the saved contracts; the refusal is reported
	// above, at the task-file guard.
	if retained(s) {
		contracts := s.Backlog
		if ev.importsHeldEdit {
			if imported, err := decodeBacklogStrict(ev.todoData); err == nil {
				contracts = imported
			}
		}
		var mismatched []string
		for _, a := range s.Attempts {
			if a.Phase == "done" || a.Phase == "blocked" || a.Phase == "cancelled" {
				continue
			}
			if t := contracts.Task(a.Task.ID); t == nil || t.Contract() != a.Task.Contract() {
				mismatched = append(mismatched, a.Task.ID)
			}
		}
		switch {
		case len(mismatched) == 0:
			detail := "every retained attempt holds its saved task contract"
			if ev.importsHeldEdit {
				detail = "every retained attempt holds its task contract in the task file the next run imports"
			}
			guards = append(guards, recoveryGuard{Code: guardTaskContract, Verdict: verdictPass, Detail: clamp(detail)})
		default:
			guard := fail(guardTaskContract, fmt.Sprintf("task %s changed while its work was retained", mismatched[0]))
			if len(mismatched) > 1 {
				guard.Detail = clamp(fmt.Sprintf("tasks %s changed while their work was retained", strings.Join(mismatched, ", ")))
			}
			// An identifier too long for a route is dropped, not truncated: a
			// cut command would name a task that does not exist.
			if statusRouteIDOk(mismatched[0]) {
				guard.Command = "looper inspect " + mismatched[0] + " --json"
			}
			guards = append(guards, guard)
		}
	}
	return guards
}

// recoveryChoices enumerates the supported recovery choices with what each
// preserves and changes. A choice is available only when every guard it
// depends on decided and passed; missing evidence and unsafe cases stay
// explicit, and no force bypass is offered.
func recoveryChoices(ev *recoveryEvidence, doc *recoveryPreviewDocument, clamp func(string) string) []recoveryChoice {
	failed, unknown := map[string]string{}, map[string]string{}
	for _, g := range doc.Guards {
		switch g.Verdict {
		case verdictFail:
			failed[g.Code] = g.Detail
		case verdictUnknown:
			unknown[g.Code] = g.Detail
		}
	}
	clamped := func(values ...string) []string {
		out := make([]string, 0, len(values))
		for _, v := range values {
			out = append(out, clamp(v))
		}
		return out
	}
	// Resume: the only supported application of retained work today. The
	// startup recovery path runs it under the repository lock.
	resume := recoveryChoice{
		Name: "resume",
		Preserves: clamped(
			"every retained attempt, its worktree, and its result commit",
			"verified work is never repeated: startup replays receipts and coerces retained phases"),
		Changes: clamped("the run continues dispatching and publishing after it resumes"),
		Apply: &recoveryApply{
			Mode: "command", Description: clamp("Start the controller; the startup recovery path applies under the repository lock."), Command: "looper",
		},
	}
	if !doc.Run.Retained {
		// Without retained work the next start archives the saved run and
		// begins a new baseline; nothing can be lost and nothing repeats.
		resume.Preserves = clamped("the archived run stays inspectable under the runtime history directory")
		resume.Changes = clamped("the saved run is archived and a new baseline is planned from the current task file and working tree")
	}
	if ev.foldAccepted {
		// The T046 fold the branch comparison accepted changes what resume
		// does before any dispatch continues.
		resume.Changes = append(clamped("the accepted owner commit folds into the run baseline; retained results re-prepare against the folded base"), resume.Changes...)
	}
	if ev.importsHeldEdit {
		// The T058 startup import changes what resume does before any dispatch
		// continues.
		resume.Changes = append(clamped("the task-file edit the cancelled hold left behind imports at startup; retained results re-prepare and re-verify against the imported baseline"), resume.Changes...)
	}
	if pub := doc.Comparisons.Publication; pub != nil && (pub.Outcome == publicationApplies || pub.Outcome == publicationApplied) && pub.Changes != nil && pub.Changes.Count > 0 {
		resume.Changes = append(clamped(fmt.Sprintf("the retained publication for %s applies the verified content to %d path(s) at resume", orUnknown(pub.TaskID), pub.Changes.Count)), resume.Changes...)
	}
	blockedBy, missing := guardReasons(failed, clamp), guardReasons(unknown, clamp)
	if ev.owned {
		blockedBy = append(blockedBy, clamp("a controller owns the repository; stop it before resuming (looper stop)"))
		if ev.info != nil && controllerAlive(ev.info) {
			// A live owner keeps its own state; a preview cannot promise what it will do next.
			missing = append(missing, clamp("what the owning controller does next; its run is live and the preview reads no live boundary"))
		}
	}
	resume.MissingEvidence = missing
	switch {
	case len(blockedBy) > 0 || len(missing) > 0:
		resume.Status = "blocked"
		resume.BlockedBy = blockedBy
	default:
		resume.Status = "available"
	}
	choices := []recoveryChoice{resume}
	// The retained publication, when one exists. Publishing runs inside the
	// resume startup path, so every guard that blocks resume blocks it too.
	if pub := doc.Comparisons.Publication; pub != nil {
		publish := recoveryChoice{
			Name: "publish-retained",
			Preserves: clamped(
				"the verified result commit and every retained attempt",
				"the task stays unaccepted until integration checks pass"),
			Changes: clamped(fmt.Sprintf("the project tree takes the verified content on %d path(s)", pub.Changes.count())),
			Apply: &recoveryApply{
				Mode: "command", Description: clamp("Applied automatically at resume; no separate operation exists."), Command: "looper",
			},
		}
		switch pub.Outcome {
		case publicationApplies, publicationApplied:
			switch {
			case len(blockedBy) > 0 || len(missing) > 0:
				publish.Status = "blocked"
				publish.BlockedBy = blockedBy
				publish.MissingEvidence = missing
			default:
				publish.Status = "available"
			}
		case publicationRequeued:
			publish.Changes = append(clamped("the accepted owner commit moves the baseline; this verified result re-prepares against the folded base and verifies again before it publishes"), publish.Changes...)
			switch {
			case len(blockedBy) > 0 || len(missing) > 0:
				publish.Status = "blocked"
				publish.BlockedBy = blockedBy
				publish.MissingEvidence = missing
			default:
				publish.Status = "available"
			}
		case publicationCollision:
			publish.Status = "blocked"
			publish.BlockedBy = append(guardReasons(map[string]string{guardProjectTreeChanged: failed[guardProjectTreeChanged]}, clamp),
				clamp("project files match neither the accepted tree nor the verified candidate"))
			publish.MissingEvidence = clamped("which process changed the project files after verification")
		case publicationBaseMismatch:
			publish.Status = "blocked"
			publish.BlockedBy = append(guardReasons(map[string]string{guardPublicationBase: failed[guardPublicationBase]}, clamp),
				clamp("the publication base no longer matches the verified integration base"))
		default:
			publish.Status = "blocked"
			publish.BlockedBy = blockedBy
			publish.MissingEvidence = append(missing, clamp(orUnknown(pub.Unknown)))
		}
		choices = append(choices, publish)
	}
	// Restoring the task file, when its guard refused. The checked
	// application (AX-017) restores the accepted bytes itself, after it
	// re-derives the preview token; without a byte-exact restore source the
	// choice stays operator-owned and names the limitation.
	if doc.Comparisons.TaskFile.State != "match" {
		restore := recoveryChoice{
			Name:      "restore-task-file",
			Preserves: clamped("every retained attempt and its result commit", "the saved backlog contract in the run state"),
			Changes:   clamped(fmt.Sprintf("the external task-file edit is discarded; the file returns to the saved contract (digest %s)", ev.s.ExportHash)),
		}
		if restoreBytes, source, restoreErr := restoreSourceFor(ev.dir, ev.s); restoreErr == nil && restoreBytes != nil {
			restore.Apply = &recoveryApply{
				Mode:        "command",
				Description: clamp("Checked application: restores " + source + " after re-deriving the preview token; a stale preview is refused without mutation."),
				Command:     clamp(fmt.Sprintf("looper recovery --apply restore-task-file --token %s", doc.Token.Value)),
			}
		} else {
			restore.Apply = &recoveryApply{
				Mode:        "operator",
				Description: clamp("No byte-exact restore source holds the accepted bytes, so the checked application refuses; restore the accepted bytes by hand."),
			}
		}
		switch doc.Comparisons.TaskFile.Parsed {
		case "identical", "differs":
			restore.Status = "available"
			if restore.Apply.Mode == "operator" {
				restore.Status = "blocked"
				restore.MissingEvidence = clamped(fmt.Sprintf("a restore source holding the accepted bytes (digest %s): the retained accepted copy is absent or stale", ev.s.ExportHash))
			}
			if doc.Comparisons.TaskFile.Parsed == "identical" {
				restore.Changes = clamped(fmt.Sprintf("the external formatting edit is discarded and the file returns to the saved bytes (digest %s); no task content differs, and the next run accepts the edit as-is, so restoring is optional (AX-018)", ev.s.ExportHash))
			}
			if ev.importsHeldEdit {
				// The next run imports the edit at startup (T058), so the
				// restore only chooses which backlog the resume starts from.
				restore.Changes = clamped(fmt.Sprintf("the external edit is discarded and the file returns to the saved bytes (digest %s); the next run imports the edit as-is because every retained result replays, so restoring is optional (T058)", ev.s.ExportHash))
			}
		default:
			restore.Status = "unsafe"
			restore.MissingEvidence = clamped("what a restore would discard: " + orUnknown(doc.Comparisons.TaskFile.Unknown))
		}
		choices = append(choices, restore)
	}
	// Importing the edited backlog now, when the file changed. Equivalent
	// formatting adopts through the checked application (AX-018); a real edit
	// imports through the T058 retained-backlog import, which the next run
	// would otherwise run at startup.
	if doc.Comparisons.TaskFile.State == "changed" {
		imp := recoveryChoice{
			Name: "import-backlog",
			Apply: &recoveryApply{
				Mode:        "command",
				Description: clamp("Checked application: applies the edit after re-deriving the preview token; a stale preview is refused without mutation."),
				Command:     clamp(fmt.Sprintf("looper recovery --apply import-backlog --token %s", doc.Token.Value)),
			},
		}
		switch {
		case doc.Comparisons.TaskFile.Parsed == "identical":
			imp.Status = "available"
			imp.Preserves = clamped("every retained attempt, its result commit, and the saved backlog content")
			imp.Changes = clamped(fmt.Sprintf("the equivalent bytes (digest %s) become the accepted export; the content and the baseline stay unchanged (AX-018)", orUnknown(doc.Comparisons.TaskFile.ObservedDigest)))
		case doc.Comparisons.TaskFile.Parsed == "differs" && doc.Run.Retained && ev.replaySafe && len(ev.importBreaksContracts) == 0:
			imp.Status = "available"
			imp.Preserves = clamped(
				"every retained attempt and its result commit",
				"verified work is never repeated: the retained results replay against the imported baseline (T058)")
			imp.Changes = clamped(fmt.Sprintf("the edited backlog (digest %s) imports as the new baseline; retained results re-prepare and re-verify against it", orUnknown(doc.Comparisons.TaskFile.ObservedDigest)))
			if ev.importsHeldEdit {
				imp.Changes = append(imp.Changes, clamp("the next run would import this edit at startup anyway; applying it now moves that import ahead of the resume"))
			}
		case doc.Comparisons.TaskFile.Parsed == "differs" && doc.Run.Retained && ev.replaySafe:
			// Every result replays, but the edit changes a retained task's
			// contract: the import would strand that work at the next
			// startup's contract check, so it stays refused.
			imp.Status = "blocked"
			imp.Preserves = clamped("every retained attempt, its result commit, and the external edit itself")
			imp.Changes = clamped("nothing: the import is refused and the edit stays on the file")
			imp.BlockedBy = clamped(fmt.Sprintf("%s: the edit changes the contract of retained task(s) %s while their work is retained", guardTaskContract, boundedIDList(ev.importBreaksContracts, 8)))
			imp.MissingEvidence = clamped("an edit that leaves every retained task's contract unchanged; restore the saved contract instead, or wait for the retained work to finish and edit then")
		case doc.Comparisons.TaskFile.Parsed == "differs" && doc.Run.Retained:
			imp.Status = "blocked"
			imp.Preserves = clamped("every retained attempt, its result commit, and the external edit itself")
			imp.Changes = clamped("nothing: the import is refused and the edit stays on the file")
			imp.BlockedBy = clamped(fmt.Sprintf("%s: the edit changed content while work was retained and no result can replay", guardBacklogChanged))
			imp.MissingEvidence = clamped(fmt.Sprintf("%d retained attempt(s) hold no replayable result, so the import would rerun implementation; restore the saved contract instead", recoveryNonReplayable(ev.s)))
		case doc.Comparisons.TaskFile.Parsed == "differs":
			imp.Status = "unsupported"
			imp.Preserves = clamped("the external edit; it becomes the baseline of the next run")
			imp.Changes = clamped("nothing: with no retained work the next run starts a new baseline from the edited file, so no import is needed")
			imp.Apply = &recoveryApply{
				Mode:        "command",
				Description: clamp("No import applies: the next run archives the saved state and plans from the edited file. Run looper."),
				Command:     "looper",
			}
		default:
			imp.Status = "unsafe"
			imp.MissingEvidence = clamped("what an import would apply: " + orUnknown(doc.Comparisons.TaskFile.Unknown))
		}
		choices = append(choices, imp)
	}
	// Discarding retained work is enumerated to refuse it: Looper has no
	// supported discard, and deleting runtime state by hand is a force
	// action the preview does not propose.
	choices = append(choices, recoveryChoice{
		Name:    "discard-retained",
		Status:  "unsupported",
		Changes: clamped("retained attempts, result commits, and the saved run would be lost"),
		Apply: &recoveryApply{
			Mode:        "none",
			Description: clamp("No supported discard exists; Looper never deletes retained work through recovery. Nothing is proposed here."),
		},
	})
	if len(choices) > maxRecoveryChoices {
		choices = choices[:maxRecoveryChoices]
	}
	return choices
}

// guardReasons renders the blocking guards as bounded reason strings.
func guardReasons(reasons map[string]string, clamp func(string) string) []string {
	if len(reasons) == 0 {
		return nil
	}
	codes := make([]string, 0, len(reasons))
	for code := range reasons {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	out := make([]string, 0, len(codes))
	for _, code := range codes {
		reason := reasons[code]
		if reason == "" {
			reason = "the guard refused"
		}
		out = append(out, clamp(code+": "+reason))
	}
	return out
}

func (l *boundedListing) count() int {
	if l == nil {
		return 0
	}
	return l.Count
}

// recoveryToken binds the preview to what it observed: the saved run, its
// retained attempts and publication, and the observed file and reference
// state. The checked application operation (AX-017) re-derives this value
// and rejects a stale preview, so a preview can never replay over changed
// work.
func recoveryToken(ev *recoveryEvidence) string {
	s := ev.s
	input := struct {
		Store struct {
			ID    string `json:"id"`
			Epoch int64  `json:"epoch"`
		} `json:"store"`
		Run struct {
			Ref        string `json:"ref"`
			ExportHash string `json:"export_hash"`
			Head       string `json:"head"`
			Branch     string `json:"branch"`
			Tip        string `json:"tip"`
			Published  string `json:"published"`
		} `json:"run"`
		Publication *struct {
			Base   string `json:"base"`
			Commit string `json:"commit"`
			Tree   string `json:"tree"`
		} `json:"publication,omitempty"`
		Attempts []struct {
			ID       string `json:"id"`
			TaskID   string `json:"task_id"`
			Contract string `json:"contract"`
			Phase    string `json:"phase"`
		} `json:"attempts"`
		Observed struct {
			TaskFileDigest string `json:"task_file_digest"`
			Head           string `json:"head"`
			Branch         string `json:"branch"`
			WorkingDigest  string `json:"working_digest"`
		} `json:"observed"`
		HighWater int64 `json:"high_water"`
	}{}
	input.Store.ID, input.Store.Epoch = ev.snap.StoreID, ev.snap.Epoch
	input.Run.Ref, input.Run.ExportHash = s.Ref, s.ExportHash
	input.Run.Head, input.Run.Branch, input.Run.Tip, input.Run.Published = s.Head, s.Branch, s.Tip, s.Published
	input.HighWater = ev.snap.HighWater
	if p := s.Publication; p != nil {
		input.Publication = &struct {
			Base   string `json:"base"`
			Commit string `json:"commit"`
			Tree   string `json:"tree"`
		}{p.Base, p.Commit, p.Tree}
	}
	for _, a := range s.Attempts {
		if a.Phase == "done" || a.Phase == "blocked" || a.Phase == "cancelled" {
			continue
		}
		input.Attempts = append(input.Attempts, struct {
			ID       string `json:"id"`
			TaskID   string `json:"task_id"`
			Contract string `json:"contract"`
			Phase    string `json:"phase"`
		}{a.ID, a.Task.ID, a.Task.Contract(), a.Phase})
	}
	input.Observed.TaskFileDigest = ev.todoDigest
	input.Observed.Head, input.Observed.Branch = ev.head, ev.branch
	input.Observed.WorkingDigest = digest([]byte(strings.Join(append(append([]string{}, ev.workingChanges...), ev.untracked...), "\n")))
	return digest(mustMarshal(input))
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// runRecoveryPreview serves the read-only recovery preview (AX-016). The
// preview reads the store through a read-only connection and the repository
// through lock-free git queries: no file, database record, process, or Git
// ref changes while it runs.
func runRecoveryPreview(c Config, out io.Writer) error {
	if !c.Preview {
		p := usageFailure("usage_invalid", "looper recovery currently serves one operation: --preview")
		p.Expected = map[string]any{"invocation": "looper recovery --preview"}
		p.Observed = map[string]any{"invocation": "looper recovery"}
		p.Actions = []ErrorAction{{Description: "Preview the recovery choices for the saved run.", Command: "looper recovery --preview"}}
		return p
	}
	if c.JSON && c.Contract != ContractVersionV2 {
		p := incompatibleFailure("contract_version_required", "The recovery preview document is a contract version 2 document.")
		p.Expected = map[string]any{"contract": "v2"}
		p.Observed = map[string]any{"contract": "legacy", "option": "--json"}
		p.Actions = []ErrorAction{{
			Description: "Request the preview document with the version 2 contract.",
			Command:     "looper recovery --preview --contract v2 --json",
		}}
		return p
	}
	_, dir := projectPaths(c)
	path := filepath.Join(dir, "state.db")
	if _, err := os.Stat(path); err != nil {
		p := runNotFoundForRun(c, path, "looper")
		p.Message = "no saved run; a recovery preview compares a saved run"
		return p
	}
	store, err := openObservedStore(dir)
	if err != nil {
		return stateUnreadableError(path, err)
	}
	defer store.Close()
	snap, err := store.Snapshot()
	if err != nil {
		return stateUnreadableError(path, err)
	}
	if snap.State == nil {
		p := runNotFoundForRun(c, path, "looper")
		p.Message = "no saved run; the store holds no state yet"
		return p
	}
	doc, err := buildRecoveryPreview(c, dir, snap)
	if err != nil {
		return err
	}
	if c.JSON {
		return json.NewEncoder(out).Encode(doc)
	}
	renderRecoveryPreview(doc, path, out)
	return nil
}

// renderRecoveryPreview renders the same document as text: the comparisons,
// the guard verdicts, the choices with what each preserves or changes, the
// token, and the detail routes.
func renderRecoveryPreview(doc *recoveryPreviewDocument, statePath string, out io.Writer) {
	line := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
	line("Recovery preview (read-only; nothing was changed)")
	retainedNote := "none"
	if doc.Run.RetainedTaskTotal > 0 {
		retainedNote = fmt.Sprintf("%d task(s): %s", doc.Run.RetainedTaskTotal, strings.Join(doc.Run.RetainedTasks, ", "))
		if doc.Run.RetainedTaskTotal > len(doc.Run.RetainedTasks) {
			retainedNote = fmt.Sprintf("%d task(s): %s (+%d more)", doc.Run.RetainedTaskTotal, strings.Join(doc.Run.RetainedTasks, ", "), doc.Run.RetainedTaskTotal-len(doc.Run.RetainedTasks))
		}
	}
	line("Run: %s | retained work: %s | publication held: %s", doc.Run.ID, retainedNote, yesNo(doc.Run.PublicationHeld))
	line("Controller: %s%s", doc.Controller.State, sessionSuffix(doc.Controller.Session))
	tf := doc.Comparisons.TaskFile
	switch tf.State {
	case "match":
		line("Task file: matches the saved contract (digest %s)", tf.ExpectedDigest)
	case "missing":
		line("Task file: missing; restore the saved task file before resuming")
	default:
		line("Task file: %s | expected digest %s, observed %s", tf.State, orUnknown(tf.ExpectedDigest), orUnknown(tf.ObservedDigest))
		if tf.Parsed != "" {
			line("  parsed backlog: %s (changed: %d, added: %d, removed: %d, data: %d)", tf.Parsed, len(tf.ChangedContracts), len(tf.AddedContracts), len(tf.RemovedContracts), len(tf.DataChanges))
		}
		if tf.Unknown != "" {
			line("  unknown: %s", tf.Unknown)
		}
	}
	br := doc.Comparisons.Branch
	switch br.State {
	case branchMatch:
		if br.ObservedHead == "" {
			line("Branch: matches the saved baseline (HEAD is unborn on %s, as the saved run recorded)", orUnknown(br.ObservedBranch))
		} else {
			line("Branch: matches the saved baseline (%s on %s)", br.ObservedHead, br.ObservedBranch)
		}
	default:
		line("Branch: %s | expected %s on %s, observed %s on %s", br.State, br.ExpectedHead, br.ExpectedBranch, orUnknown(br.ObservedHead), orUnknown(br.ObservedBranch))
		if br.Reason != "" {
			line("  %s", br.Reason)
		}
		if l := br.CommittedChanges; l != nil {
			line("  committed changes: %d path(s)%s", l.Count, listingSuffix(l))
		}
		if l := br.StagedFiles; l != nil && l.Count > 0 {
			line("  staged files: %d path(s)%s", l.Count, listingSuffix(l))
		}
	}
	wt := doc.Comparisons.WorkingTree
	switch wt.State {
	case "matches_published":
		if wt.OwnerCommitAccepted {
			line("Working tree: matches the published tree %s after the accepted owner commit folds into it at resume", wt.PublishedTree)
			if wt.Basis != "" {
				line("  %s", wt.Basis)
			}
		} else {
			line("Working tree: matches the published tree %s", wt.PublishedTree)
		}
	case "unknown":
		line("Working tree: unknown")
		if wt.Unknown != "" {
			line("  %s", wt.Unknown)
		}
	default:
		line("Working tree: changed against the tree %s", wt.PublishedTree)
		if l := wt.ChangedPaths; l != nil {
			line("  changed: %d path(s)%s", l.Count, listingSuffix(l))
		}
		if l := wt.UntrackedPaths; l != nil && l.Count > 0 {
			line("  untracked: %d path(s)%s", l.Count, listingSuffix(l))
		}
	}
	if pub := doc.Comparisons.Publication; pub != nil {
		line("Publication: %s | verified commit %s | changes %d path(s)", pub.Outcome, pub.VerifiedCommit, pub.Changes.count())
		if pub.LocalEdits != nil && pub.LocalEdits.Count > 0 {
			line("  local edits that block it: %d path(s)%s", pub.LocalEdits.Count, listingSuffix(pub.LocalEdits))
		}
		if pub.Unknown != "" {
			line("  unknown: %s", pub.Unknown)
		}
	}
	line("Guards:")
	for _, g := range doc.Guards {
		if g.Command != "" {
			line("  %s: %s | %s | detail: %s", g.Code, g.Verdict, g.Detail, g.Command)
		} else {
			line("  %s: %s | %s", g.Code, g.Verdict, g.Detail)
		}
	}
	line("Choices:")
	for _, ch := range doc.Choices {
		line("  %s: %s", ch.Name, ch.Status)
		for _, p := range ch.Preserves {
			line("    preserves: %s", p)
		}
		for _, c := range ch.Changes {
			line("    changes: %s", c)
		}
		for _, b := range ch.BlockedBy {
			line("    blocked by: %s", b)
		}
		for _, m := range ch.MissingEvidence {
			line("    missing evidence: %s", m)
		}
		if ch.Apply != nil && ch.Apply.Mode != "none" {
			if ch.Apply.Command != "" {
				line("    apply (%s): %s (%s)", ch.Apply.Mode, ch.Apply.Description, ch.Apply.Command)
			} else {
				line("    apply (%s): %s", ch.Apply.Mode, ch.Apply.Description)
			}
		}
	}
	line("Token: %s", doc.Token.Value)
	line("  %s", doc.Token.Note)
	if doc.Detail.Status != "" {
		line("Detail: %s", doc.Detail.Status)
	}
	if doc.Detail.Events != "" {
		line("       %s", doc.Detail.Events)
	}
	line("State: %s", statePath)
}

func listingSuffix(l *boundedListing) string {
	if l == nil || len(l.Paths) == 0 {
		return ""
	}
	shown := fmt.Sprintf(": %s", strings.Join(l.Paths, ", "))
	if l.Count > len(l.Paths) {
		shown += fmt.Sprintf(" (+%d more)", l.Count-len(l.Paths))
	}
	if l.Command != "" {
		shown += "; every path: " + l.Command
	}
	return shown
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func sessionSuffix(session string) string {
	if session == "" {
		return ""
	}
	return " | session " + session
}
