package looper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Checked publication of accepted work to the project branch. The preview is
// one read-only document: it composes the exact commit tree, runs every guard
// against the saved run and the repository, and binds a content-sensitive
// token to what it observed. The application re-derives that token under the
// repository lock and the Git index lock, writes one project commit from the
// accepted tree, advances the branch by compare-and-swap, installs the
// prepared index without touching working files, and reconciles the saved
// baseline so the next resume passes its guards. A prepared journal carries
// every crashed operation to a deterministic completion or a preserved
// conflict.

const (
	publishSchemaVersion  = 1
	publishPreviewKind    = "looper.publish.preview"
	publishAppliedKind    = "looper.publish.applied"
	publishJournalDirName = "publish"
	publishPrivateRef     = "refs/looper-publish/"
	publishChangesShown   = 40
	publishDefaultMessage = "Accept Looper work"
)

// Journal operation states. prepared names a commit whose branch has not
// moved; committed names a branch that moved before its index installed;
// installed names an index that moved before the baseline reconciled; done
// and conflict are terminal.
const (
	publishPrepared  = "prepared"
	publishCommitted = "committed"
	publishInstalled = "installed"
	publishDone      = "done"
	publishConflict  = "conflict"
)

func publishJournalDir(dir string) string { return filepath.Join(dir, publishJournalDirName) }

// publishJournal is the durable record of one publication application. It is
// written before the branch moves and updated at every boundary, so a crash
// leaves an operation a later run can complete or report as a preserved
// conflict instead of guessing.
type publishJournal struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	Branch         string `json:"branch"`
	OldCommit      string `json:"old_commit"`
	NewCommit      string `json:"new_commit,omitempty"`
	Tree           string `json:"tree,omitempty"`
	SourceTree     string `json:"source_tree"`
	ProposedDigest string `json:"proposed_digest"`
	OldIndexDigest string `json:"old_index_digest"`
	ExpectedTree   string `json:"expected_tree,omitempty"`
	PreparedIndex  string `json:"prepared_index,omitempty"`
	Token          string `json:"token"`
	Message        string `json:"message"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	PayloadDigest  string `json:"payload_digest,omitempty"`
	RequestedAt    string `json:"requested_at"`
	UpdatedAt      string `json:"updated_at"`
	Outcome        string `json:"outcome,omitempty"`
	Conflict       string `json:"conflict,omitempty"`
}

// publishGuard is one named verdict the preview reports and the application
// re-checks.
type publishGuard struct {
	Code    string
	Verdict string
	Detail  string
}

// publishEvidence is everything one preview observed and one application
// re-derives. Composing it is read-only.
type publishEvidence struct {
	s    *State
	snap *StoreSnapshot
	dir  string
	repo *Repository
	c    Config

	head, branch string
	unborn       bool

	entries          map[string]string // path -> "<mode> <type> <oid>"
	headEntries      map[string]string
	publishedEntries map[string]string
	proposedDigest   string

	acceptedBytes  []byte
	acceptedSource string
	taskFileDigest string
	taskFileEdit   backlogEdit
	schemaDigest   string

	indexDigest string
	indexLock   bool
	mergeInProg string
	staged      []string

	workingChanges []string
	unrelated      []string

	taskTracked    bool
	schemaTracked  bool
	runtimeTracked []string

	authorName, authorEmail string
	signing                 string

	retainedAttempts int
	retainedTasks    []string

	journal      *publishJournal
	journalCount int

	guards      []publishGuard
	refusals    []string
	publishable bool
}

// treeEntryPath splits one `ls-tree -r` line into its path. The line shape is
// "<mode> <type> <oid>\t<path>"; a path can contain any byte except NUL.
func treeEntryPath(line string) string {
	if i := strings.IndexByte(line, '\t'); i >= 0 {
		return line[i+1:]
	}
	return ""
}

// lsTreeLists the recursive entries of one tree as raw lines.
func lsTreeLines(root, tree string) ([]string, error) {
	out, err := gitCommand(root, nil, nil, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(string(out), "\x00") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func lsTreeEntries(root, tree string) (map[string]string, error) {
	lines, err := lsTreeLines(root, tree)
	if err != nil {
		return nil, err
	}
	entries := make(map[string]string, len(lines))
	for _, line := range lines {
		entries[treeEntryPath(line)] = line
	}
	return entries, nil
}

// blobOID computes one blob's object identifier without writing it. The
// preview composes tree identities from these names alone.
func blobOID(root string, content []byte) (string, error) {
	out, err := gitCommand(root, content, nil, "hash-object", "-t", "blob", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// indexListing digests the staged state: modes, object identifiers, and
// stages, without stat information that changes on every touch.
func indexListing(root string) (string, error) {
	out, err := gitCommand(root, nil, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// mergeMarkers names the Git state files whose presence means an operation is
// in progress that owns the index or the branch.
var mergeMarkers = []string{"MERGE_HEAD", "REVERT_HEAD", "CHERRY_PICK_HEAD", "BISECT_LOG", "rebase-merge", "rebase-apply", "rebase-merge.dir"}

func mergeState(common string) string {
	for _, name := range mergeMarkers {
		if _, err := os.Stat(filepath.Join(common, name)); err == nil {
			return name
		}
	}
	return ""
}

// publishCompose builds the exact commit tree entries from the accepted tree,
// the project HEAD, and the accepted control-file bytes. Excluded paths never
// enter from the accepted side; tracked control files keep their tracked
// place, and the tracked task file carries the accepted exported bytes.
func (ev *publishEvidence) compose() error {
	entries := map[string]string{}
	head := map[string]string{}
	var err error
	if entries, err = lsTreeEntries(ev.c.Root, ev.s.Published); err != nil {
		return fmt.Errorf("the accepted tree could not be listed: %w", err)
	}
	if head, err = lsTreeEntries(ev.c.Root, ev.head); err != nil {
		return fmt.Errorf("the project HEAD tree could not be listed: %w", err)
	}
	ev.headEntries = head
	ev.publishedEntries = make(map[string]string, len(entries))
	taskRel := ev.repo.Excluded[0]
	schemaRel := ev.repo.Excluded[1]
	// The accepted side supplies every non-excluded path.
	for path, line := range entries {
		ev.publishedEntries[path] = line
		if ev.repo.excluded(path) {
			delete(entries, path)
		}
	}
	ev.taskTracked, ev.schemaTracked = head[taskRel] != "", head[schemaRel] != ""
	// Excluded paths preserve their tracked project entries, except the task
	// file, which carries the accepted exported bytes.
	for path, line := range head {
		if !ev.repo.excluded(path) {
			continue
		}
		if path == taskRel && ev.taskTracked && ev.acceptedBytes != nil {
			oid, err := blobOID(ev.c.Root, ev.acceptedBytes)
			if err != nil {
				return fmt.Errorf("the accepted task-file bytes could not be identified: %w", err)
			}
			mode := strings.Fields(line)
			entries[path] = fmt.Sprintf("%s blob %s\t%s", mode[0], oid, path)
			continue
		}
		entries[path] = line
		if ev.repo.Runtime != "" && (path == ev.repo.Runtime || strings.HasPrefix(path, ev.repo.Runtime+"/")) {
			ev.runtimeTracked = append(ev.runtimeTracked, path)
		}
	}
	sort.Strings(ev.runtimeTracked)
	ev.entries = entries
	ev.proposedDigest = digest([]byte(ev.canonicalEntries()))
	return nil
}

// canonicalEntries renders the composed entries in one stable order, so the
// preview identity and the apply-time identity are the same value.
func (ev *publishEvidence) canonicalEntries() string {
	paths := make([]string, 0, len(ev.entries))
	for path := range ev.entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, path := range paths {
		b.WriteString(ev.entries[path])
		b.WriteByte(0)
	}
	return b.String()
}

// composedDiff lists the paths the project commit would change: entries whose
// mode or object differs from the project HEAD tree, added or removed.
func (ev *publishEvidence) composedDiff() []string {
	var changed []string
	seen := map[string]bool{}
	for path, line := range ev.entries {
		seen[path] = true
		if ev.headEntries[path] != line {
			changed = append(changed, path)
		}
	}
	for path := range ev.headEntries {
		if !seen[path] {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}

// guard appends one named verdict and, on a failure, one refusal.
func (ev *publishEvidence) guard(code, verdict, detail string) {
	ev.guards = append(ev.guards, publishGuard{Code: code, Verdict: verdict, Detail: detail})
	if verdict != "pass" {
		ev.refusals = append(ev.refusals, code+": "+detail)
	}
}

// collectPublishEvidence composes everything a preview reports. It writes no
// object, no ref, no index, and no database record.
func collectPublishEvidence(c Config, dir string, repo *Repository, snap *StoreSnapshot, s *State) (*publishEvidence, error) {
	ev := &publishEvidence{s: s, snap: snap, dir: dir, repo: repo, c: c}
	// Branch state.
	ev.head, ev.branch, ev.unborn = repo.HeadDetail()
	// Control-file bytes and task-file classification.
	data, err := os.ReadFile(c.Todo)
	if err != nil {
		ev.taskFileEdit = backlogEdit{Changed: true, Reason: "the task file: " + err.Error()}
	} else {
		ev.taskFileDigest = digest(data)
		bare := &Engine{Config: c, State: s}
		ev.taskFileEdit = bare.classifyBacklog(data)
	}
	ev.acceptedBytes, ev.acceptedSource, _ = restoreSourceFor(dir, s)
	if schemaData, err := os.ReadFile(c.SchemaPath()); err == nil {
		ev.schemaDigest = digest(schemaData)
	} else {
		ev.schemaDigest = "missing"
	}
	// Index state.
	if listing, err := indexListing(c.Root); err == nil {
		ev.indexDigest = digest([]byte(listing))
	} else {
		ev.indexDigest = "unreadable"
	}
	_, err = os.Stat(filepath.Join(repo.Common, "index.lock"))
	ev.indexLock = err == nil
	ev.mergeInProg = mergeState(repo.Common)
	if staged, err := gitCommand(c.Root, nil, nil, "diff", "--cached", "--name-only", "-z", "HEAD"); err == nil {
		ev.staged = splitZPaths(string(staged))
	}
	// Retained work evidence.
	for _, a := range s.Attempts {
		switch a.Phase {
		case "done", "blocked", "cancelled":
			continue
		}
		ev.retainedAttempts++
		if !containsString(ev.retainedTasks, a.Task.ID) {
			ev.retainedTasks = append(ev.retainedTasks, a.Task.ID)
		}
	}
	sort.Strings(ev.retainedTasks)
	// Journals.
	ev.journal, ev.journalCount = loadNewestPublishJournal(dir)
	// Git identity.
	ev.authorName, _ = gitText(c.Root, "config", "--get", "user.name")
	ev.authorEmail, _ = gitText(c.Root, "config", "--get", "user.email")
	sign, _ := gitText(c.Root, "config", "--get", "commit.gpgsign")
	ev.signing = "disabled"
	if sign == "true" {
		ev.signing = "configured"
	}
	ev.evaluate()
	if err := ev.compose(); err != nil {
		return ev, err
	}
	ev.evaluateComposition()
	return ev, nil
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// evaluate runs the guards that do not need the composed tree.
func (ev *publishEvidence) evaluate() {
	if ev.unborn {
		ev.guard("head_baseline", "fail", fmt.Sprintf("the project HEAD is unborn on branch %s; make a commit before publishing", branchLabel(ev.branch)))
	} else if ev.branch == "" {
		ev.guard("head_baseline", "fail", "HEAD is detached, so no project branch can receive the commit")
	} else if ev.head != ev.s.Head || ev.branch != ev.s.Branch {
		ev.guard("head_baseline", "fail", fmt.Sprintf("the project branch is %s at %s; the saved run baseline is %s on %s. Resume the controller to fold the change, or restore the baseline, before publishing", branchLabel(ev.branch), shortRevision(ev.head), shortRevision(ev.s.Head), branchLabel(ev.s.Branch)))
	} else {
		ev.guard("head_baseline", "pass", fmt.Sprintf("the project HEAD matches the saved baseline (%s on %s)", shortRevision(ev.head), branchLabel(ev.branch)))
	}
	if ev.s.Publication != nil {
		ev.guard("publication_pending", "fail", "a verified result waits on its publication; resume the controller so it publishes, then preview again")
	} else {
		ev.guard("publication_pending", "pass", "no publication waits on the run")
	}
	if ev.s.ExportPending {
		ev.guard("task_file", "fail", "an export is pending; resume the controller so it commits the export, then preview again")
	} else if ev.taskFileEdit.Changed && !ev.taskFileEdit.Equivalent {
		detail := "the task file holds an unimported edit"
		if ev.taskFileEdit.Reason != "" {
			detail = "the task file cannot be compared: " + ev.taskFileEdit.Reason
		}
		ev.guard("task_file", "fail", detail+"; import or restore it before publishing")
	} else {
		ev.guard("task_file", "pass", "the task file matches the accepted export")
	}
	if ev.s.Version != 2 || ev.s.Root != ev.c.Root || ev.s.Todo != ev.c.Todo {
		ev.guard("saved_run", "fail", "the saved run belongs to another project or task file")
	} else if err := ev.s.Backlog.Validate(); err != nil {
		ev.guard("saved_run", "fail", "the saved backlog is invalid: "+err.Error())
	} else {
		ev.guard("saved_run", "pass", fmt.Sprintf("the saved run holds %d task(s)", len(ev.s.Backlog.Tasks)))
	}
	if ev.acceptedBytes == nil {
		ev.guard("task_file_bytes", "fail", fmt.Sprintf("no restore source holds the accepted task-file bytes (digest %s); the next accepted export refreshes the retained copy", ev.s.ExportHash))
	} else {
		ev.guard("task_file_bytes", "pass", "the accepted task-file bytes are available from "+ev.acceptedSource)
	}
	if ev.indexLock {
		ev.guard("index_clean", "fail", "a Git index lock exists; another Git command holds the index")
	} else if ev.mergeInProg != "" {
		ev.guard("index_clean", "fail", "a merge or rebase operation is in progress ("+ev.mergeInProg+")")
	} else if len(ev.staged) > 0 {
		listed, total := boundPaths(ev.staged)
		ev.guard("index_clean", "fail", fmt.Sprintf("the index holds %d staged path(s): %s", total, strings.Join(listed, ", ")))
	} else {
		ev.guard("index_clean", "pass", "the index holds no staged changes")
	}
	if ev.authorName == "" || ev.authorEmail == "" {
		ev.guard("commit_identity", "fail", "the configured Git identity is incomplete; set user.name and user.email")
	} else {
		ev.guard("commit_identity", "pass", fmt.Sprintf("author %s <%s>, signing %s, hooks not invoked (the commit uses an immutable tree)", ev.authorName, ev.authorEmail, ev.signing))
	}
	if j := ev.journal; j != nil && j.State != publishDone && j.State != publishConflict {
		ev.guard("journal_resolved", "fail", fmt.Sprintf("a publication from %s is unresolved (state %s); run looper publish --apply --token %s to complete or report it", j.RequestedAt, j.State, j.Token))
	} else {
		ev.guard("journal_resolved", "pass", fmt.Sprintf("no unresolved publication journal (%d completed)", ev.journalCount))
	}
}

// evaluateComposition runs the guards that need the composed tree. It runs
// after compose so a composition failure still reports the guards above.
func (ev *publishEvidence) evaluateComposition() {
	// Working source content against the accepted tree, ignored accepted
	// files included: a temporary index seeded from the accepted tree makes
	// the comparison exact without writing any object.
	if changed, err := diffIndexAgainst(ev.c.Root, ev.s.Published); err != nil {
		ev.guard("source_matches_accepted", "unknown", "the working tree could not be compared with the accepted tree: "+err.Error())
	} else if len(changed) > 0 {
		listed, total := boundPaths(changed)
		ev.guard("source_matches_accepted", "fail", fmt.Sprintf("%d working path(s) differ from the accepted tree: %s; preserve them, restore the accepted tree, and publish again", total, strings.Join(listed, ", ")))
	} else {
		ev.guard("source_matches_accepted", "pass", "the working tree matches the accepted tree, ignored accepted files included")
	}
	if untracked, err := gitCommand(ev.c.Root, nil, nil, "ls-files", "--others", "--exclude-standard", "-z"); err != nil {
		ev.guard("no_unrelated_changes", "unknown", "the untracked files could not be listed: "+err.Error())
	} else {
		var unrelated []string
		for _, path := range splitZPaths(string(untracked)) {
			if ev.repo.excluded(path) {
				continue
			}
			// Untracked work the accepted tree holds is the publication
			// itself; only paths outside the accepted tree are unrelated.
			if _, ok := ev.entries[path]; ok {
				continue
			}
			unrelated = append(unrelated, path)
		}
		ev.unrelated = unrelated
		if len(unrelated) > 0 {
			listed, total := boundPaths(unrelated)
			ev.guard("no_unrelated_changes", "fail", fmt.Sprintf("%d untracked path(s) are unrelated to the accepted work: %s; commit or remove them first", total, strings.Join(listed, ", ")))
		} else {
			ev.guard("no_unrelated_changes", "pass", "no unrelated untracked files")
		}
	}
	ev.publishable = true
	for _, g := range ev.guards {
		if g.Verdict != "pass" {
			ev.publishable = false
		}
	}
}

// diffIndexAgainst lists the paths whose working content differs from one
// tree, using a temporary index seeded from that tree so ignored-but-accepted
// files compare exactly. The index refresh first re-stats every seeded entry:
// a freshly read tree carries no stat data, which diff-index would otherwise
// report as changes. It writes no object.
func diffIndexAgainst(root, tree string) ([]string, error) {
	f, err := os.CreateTemp(os.TempDir(), "looper-publish-index-")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	defer os.Remove(name)
	env := map[string]string{"GIT_INDEX_FILE": name}
	if _, err = gitCommand(root, nil, env, "read-tree", tree); err != nil {
		return nil, err
	}
	// A non-zero exit here names files that need update, which the diff
	// below reports; the refresh itself must only succeed in running.
	_, _ = gitCommand(root, nil, env, "update-index", "--refresh", "-q")
	out, err := gitCommand(root, nil, env, "diff-index", "--name-only", "-z", tree)
	if err != nil {
		return nil, err
	}
	return splitZPaths(string(out)), nil
}

// publishToken binds the preview to everything it observed: the store, the
// saved run, the composed tree, the control-file bytes, the index, the
// branch, and the journal revision. Any later change invalidates it.
func publishToken(ev *publishEvidence) string {
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
			Published  string `json:"published"`
		} `json:"run"`
		PublicationPending bool   `json:"publication_pending"`
		ProposedDigest     string `json:"proposed_digest"`
		ControlFiles       struct {
			TaskFileObserved string `json:"task_file_observed"`
			TaskFileAccepted string `json:"task_file_accepted"`
			Schema           string `json:"schema"`
		} `json:"control_files"`
		IndexDigest    string   `json:"index_digest"`
		TrackedRuntime []string `json:"tracked_runtime"`
		JournalCount   int      `json:"journal_count"`
	}{}
	input.Store.ID, input.Store.Epoch = ev.snap.StoreID, ev.snap.Epoch
	input.Run.Ref, input.Run.ExportHash = ev.s.Ref, ev.s.ExportHash
	input.Run.Head, input.Run.Branch, input.Run.Published = ev.s.Head, ev.s.Branch, ev.s.Published
	input.PublicationPending = ev.s.Publication != nil
	input.ProposedDigest = ev.proposedDigest
	input.ControlFiles.TaskFileObserved = ev.taskFileDigest
	input.ControlFiles.TaskFileAccepted = digest(ev.acceptedBytes)
	input.ControlFiles.Schema = ev.schemaDigest
	input.IndexDigest = ev.indexDigest
	input.TrackedRuntime = ev.runtimeTracked
	input.JournalCount = ev.journalCount
	return digest(mustMarshal(input))
}

// publishPreviewDocument is the version 2 preview contract,
// docs/contracts/schemas/v2/publish.schema.json.
type publishPreviewDocument struct {
	ContractVersion int                      `json:"contract_version"`
	SchemaVersion   int                      `json:"schema_version"`
	Kind            string                   `json:"kind"`
	Run             publishRunView           `json:"run"`
	Branch          publishBranchView        `json:"branch"`
	Changes         publishChangesView       `json:"changes"`
	ControlFiles    []publishControlFileView `json:"control_files"`
	TrackedRuntime  publishRuntimeView       `json:"tracked_runtime"`
	Index           publishIndexView         `json:"index"`
	Identity        publishIdentityView      `json:"identity"`
	Guards          []publishGuardView       `json:"guards"`
	Refusals        []string                 `json:"refusals,omitempty"`
	Publishable     bool                     `json:"publishable"`
	Journal         publishJournalView       `json:"journal"`
	Retained        publishRetainedView      `json:"retained"`
	Token           string                   `json:"token,omitempty"`
	Apply           string                   `json:"apply,omitempty"`
	Detail          publishDetailView        `json:"detail"`
}

// publishRuntimeView bounds the tracked runtime paths the commit preserves.
type publishRuntimeView struct {
	Count   int      `json:"count"`
	Paths   []string `json:"paths"`
	Omitted int      `json:"omitted"`
}

type publishRunView struct {
	Root         string `json:"root"`
	StoreID      string `json:"store_id"`
	StoreEpoch   int64  `json:"store_epoch"`
	Ref          string `json:"ref"`
	BacklogTasks int    `json:"backlog_tasks"`
}

type publishBranchView struct {
	Name         string `json:"name"`
	OldCommit    string `json:"old_commit"`
	AcceptedTree string `json:"accepted_tree"`
	ProposedTree string `json:"proposed_tree"`
}

type publishChangesView struct {
	Count   int      `json:"count"`
	Paths   []string `json:"paths"`
	Omitted int      `json:"omitted"`
}

type publishControlFileView struct {
	Path     string `json:"path"`
	Role     string `json:"role"`
	Included bool   `json:"included"`
	Digest   string `json:"digest"`
	Note     string `json:"note"`
}

type publishIndexView struct {
	StagedCount int    `json:"staged_count"`
	LockPresent bool   `json:"lock_present"`
	Digest      string `json:"digest"`
}

type publishIdentityView struct {
	AuthorName  string `json:"author_name"`
	AuthorEmail string `json:"author_email"`
	Signing     string `json:"signing"`
	Hooks       string `json:"hooks"`
}

type publishGuardView struct {
	Code    string `json:"code"`
	Verdict string `json:"verdict"`
	Detail  string `json:"detail"`
}

type publishJournalView struct {
	State    string `json:"state"`
	Revision int    `json:"revision"`
}

type publishRetainedView struct {
	Attempts int      `json:"attempts"`
	Tasks    []string `json:"tasks"`
}

type publishDetailView struct {
	ChangesCommand string `json:"changes_command"`
	StatusCommand  string `json:"status_command"`
}

// publishAppliedDocument is the version 2 application receipt.
type publishAppliedDocument struct {
	ContractVersion int                      `json:"contract_version"`
	SchemaVersion   int                      `json:"schema_version"`
	Kind            string                   `json:"kind"`
	RequestID       string                   `json:"request_id"`
	IdempotencyKey  string                   `json:"idempotency_key,omitempty"`
	Duplicate       bool                     `json:"duplicate"`
	Branch          publishBranchResultView  `json:"branch"`
	AcceptedTree    string                   `json:"accepted_tree"`
	Changes         int                      `json:"changes"`
	ControlFiles    []publishControlFileView `json:"control_files"`
	Journal         publishJournalDoneView   `json:"journal"`
	Retained        publishRetainedView      `json:"retained"`
	Resume          publishResumeView        `json:"resume"`
}

type publishBranchResultView struct {
	Name      string `json:"name"`
	OldCommit string `json:"old_commit"`
	NewCommit string `json:"new_commit"`
}

type publishJournalDoneView struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

type publishResumeView struct {
	Command string `json:"command"`
	Note    string `json:"note"`
}

func publishControlFileViews(ev *publishEvidence) []publishControlFileView {
	views := []publishControlFileView{}
	taskRel, schemaRel := ev.repo.Excluded[0], ev.repo.Excluded[1]
	if ev.taskTracked {
		views = append(views, publishControlFileView{Path: taskRel, Role: "task-file", Included: true,
			Digest: digest(ev.acceptedBytes), Note: "tracked; the commit carries the accepted exported bytes"})
	} else {
		views = append(views, publishControlFileView{Path: taskRel, Role: "task-file", Included: false,
			Digest: ev.taskFileDigest, Note: "untracked; the commit leaves it untracked"})
	}
	if ev.schemaTracked {
		views = append(views, publishControlFileView{Path: schemaRel, Role: "task-schema", Included: true,
			Digest: ev.schemaDigest, Note: "tracked; the commit preserves the tracked schema bytes"})
	} else {
		views = append(views, publishControlFileView{Path: schemaRel, Role: "task-schema", Included: false,
			Digest: ev.schemaDigest, Note: "untracked; the commit leaves it untracked"})
	}
	return views
}

func publishChangesViewOf(paths []string) publishChangesView {
	view := publishChangesView{Count: len(paths), Paths: []string{}}
	for i, path := range paths {
		if i >= publishChangesShown {
			view.Omitted = len(paths) - publishChangesShown
			break
		}
		view.Paths = append(view.Paths, path)
	}
	return view
}

// publishRuntimeViewOf bounds the tracked runtime listing.
func publishRuntimeViewOf(paths []string) publishRuntimeView {
	view := publishRuntimeView{Count: len(paths), Paths: []string{}}
	for i, path := range paths {
		if i >= publishChangesShown/2 {
			view.Omitted = len(paths) - publishChangesShown/2
			break
		}
		view.Paths = append(view.Paths, path)
	}
	return view
}

// buildPublishPreview composes the preview document from fresh evidence.
func buildPublishPreview(c Config, dir string, repo *Repository, snap *StoreSnapshot, s *State) (*publishPreviewDocument, error) {
	ev, err := collectPublishEvidence(c, dir, repo, snap, s)
	if err != nil {
		return nil, err
	}
	changes := ev.composedDiff()
	doc := &publishPreviewDocument{
		ContractVersion: ContractVersionV2,
		SchemaVersion:   publishSchemaVersion,
		Kind:            publishPreviewKind,
		Run: publishRunView{Root: s.Root, StoreID: snap.StoreID, StoreEpoch: snap.Epoch,
			Ref: s.Ref, BacklogTasks: len(s.Backlog.Tasks)},
		Branch: publishBranchView{Name: branchLabel(ev.branch), OldCommit: ev.head,
			AcceptedTree: s.Published, ProposedTree: ev.proposedDigest},
		Changes:        publishChangesViewOf(changes),
		ControlFiles:   publishControlFileViews(ev),
		TrackedRuntime: publishRuntimeViewOf(ev.runtimeTracked),
		Index:          publishIndexView{StagedCount: len(ev.staged), LockPresent: ev.indexLock, Digest: ev.indexDigest},
		Identity: publishIdentityView{AuthorName: ev.authorName, AuthorEmail: ev.authorEmail,
			Signing: ev.signing, Hooks: "not-invoked"},
		Guards:      []publishGuardView{},
		Refusals:    clampList(ev.refusals, 16),
		Publishable: ev.publishable,
		Journal:     publishJournalView{State: "none", Revision: ev.journalCount},
		Retained:    publishRetainedView{Attempts: ev.retainedAttempts, Tasks: ev.retainedTasks},
		Detail: publishDetailView{
			ChangesCommand: fmt.Sprintf("git diff --name-only %s %s -- . ':(exclude)%s' ':(exclude)%s'", ev.head, s.Published, ev.repo.Excluded[0], ev.repo.Excluded[1]),
			StatusCommand:  "looper status --contract v2 --json",
		},
	}
	if ev.journal != nil {
		doc.Journal.State = ev.journal.State
	}
	for _, g := range ev.guards {
		doc.Guards = append(doc.Guards, publishGuardView{Code: g.Code, Verdict: g.Verdict, Detail: clampJSONString(g.Detail, maxErrorString)})
	}
	if ev.publishable {
		token := publishToken(ev)
		doc.Token = token
		doc.Apply = fmt.Sprintf("looper publish --apply --token %s --message %q --contract v2 --json", token, publishDefaultMessage)
	}
	return doc, nil
}

// renderPublishPreview prints the same facts as text.
func renderPublishPreview(doc *publishPreviewDocument, out io.Writer) {
	line := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
	line("Publish preview (read-only; nothing was changed)")
	line("Run: %s | ref %s | %d task(s)", doc.Run.Root, doc.Run.Ref, doc.Run.BacklogTasks)
	line("Branch: %s | old commit %s | accepted tree %s", doc.Branch.Name, doc.Branch.OldCommit, doc.Branch.AcceptedTree)
	line("Proposed commit: %d changed path(s) against the project HEAD (tree identity %s)", doc.Changes.Count, doc.Branch.ProposedTree)
	for _, path := range doc.Changes.Paths {
		line("  %s", path)
	}
	if doc.Changes.Omitted > 0 {
		line("  ... and %d more; full detail: %s", doc.Changes.Omitted, doc.Detail.ChangesCommand)
	}
	for _, cf := range doc.ControlFiles {
		state := "included"
		if !cf.Included {
			state = "omitted (left untracked)"
		}
		line("Control file %s (%s): %s | digest %s | %s", cf.Path, cf.Role, state, cf.Digest, cf.Note)
	}
	for _, path := range doc.TrackedRuntime.Paths {
		line("Tracked runtime path preserved: %s", path)
	}
	if doc.TrackedRuntime.Omitted > 0 {
		line("  ... and %d more tracked runtime path(s)", doc.TrackedRuntime.Omitted)
	}
	line("Retained work: %d attempt(s) over %d task(s); publishing does not finish or erase it", doc.Retained.Attempts, len(doc.Retained.Tasks))
	line("Identity: %s <%s> | signing %s | hooks %s", doc.Identity.AuthorName, doc.Identity.AuthorEmail, doc.Identity.Signing, doc.Identity.Hooks)
	line("Guards:")
	for _, g := range doc.Guards {
		line("  %s: %s | %s", g.Code, g.Verdict, g.Detail)
	}
	if len(doc.Refusals) > 0 {
		line("Refusals:")
		for _, r := range doc.Refusals {
			line("  %s", r)
		}
	}
	if doc.Publishable {
		line("Publishable: yes")
		line("Token: %s", doc.Token)
		line("Apply: %s", doc.Apply)
	} else {
		line("Publishable: no; resolve the failed guards above, then preview again")
	}
}

// loadNewestPublishJournal returns the newest unresolved journal and the count
// of completed ones. Terminal journals stay for idempotent replay.
func loadNewestPublishJournal(dir string) (*publishJournal, int) {
	entries, err := os.ReadDir(publishJournalDir(dir))
	if err != nil {
		return nil, 0
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	done := 0
	var pending *publishJournal
	for _, name := range names {
		var j publishJournal
		if readJSON(filepath.Join(publishJournalDir(dir), name), &j) != nil {
			continue
		}
		if j.State == publishDone || j.State == publishConflict {
			done++
			continue
		}
		pending = &j
	}
	return pending, done
}

// listPublishJournals lists every journal in identity order.
func listPublishJournals(dir string) []*publishJournal {
	entries, err := os.ReadDir(publishJournalDir(dir))
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	var out []*publishJournal
	for _, name := range names {
		var j publishJournal
		if readJSON(filepath.Join(publishJournalDir(dir), name), &j) == nil {
			out = append(out, &j)
		}
	}
	return out
}

// writePublishJournal persists one journal atomically.
func writePublishJournal(dir string, j *publishJournal) error {
	j.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := os.MkdirAll(publishJournalDir(dir), 0700); err != nil {
		return err
	}
	return writeJSON(filepath.Join(publishJournalDir(dir), j.ID+".json"), j)
}

// publishJournalByKey finds the terminal journal one caller key recorded.
func publishJournalByKey(dir, key, payloadDigest string) *publishJournal {
	if key == "" {
		return nil
	}
	for _, j := range listPublishJournals(dir) {
		if j.IdempotencyKey == key {
			if j.PayloadDigest != "" && payloadDigest != "" && j.PayloadDigest == payloadDigest && j.State == publishDone {
				return j
			}
			return nil
		}
	}
	return nil
}

// publishPayloadDigest digests the application payload the caller controls:
// the token and the message. Transport fields stay out.
func publishPayloadDigest(token, message string) string {
	return digest(mustMarshal(struct {
		Token   string `json:"token"`
		Message string `json:"message"`
	}{token, message}))
}

// runPublish serves the publish command.
func runPublish(c Config, out, stderr io.Writer) int {
	_, dir := projectPaths(c)
	path := filepath.Join(dir, "state.db")
	if _, err := os.Stat(path); err != nil {
		p := runNotFoundForRun(c, path, "looper")
		p.Message = "no saved run; publication commits the accepted work of a saved run"
		return emitError(c, p, out, stderr)
	}
	if c.Preview {
		return runPublishPreview(c, dir, out, stderr)
	}
	return runPublishApply(c, dir, out, stderr)
}

// publishOpenEvidence opens the stopped run read-only and composes evidence.
func publishOpenEvidence(c Config, dir string) (*publishEvidence, *Store, error) {
	repo := c
	repo.GitInit = false
	r, err := OpenRepository(repo, dir)
	if err != nil {
		return nil, nil, err
	}
	store, err := openObservedStore(dir)
	if err != nil {
		return nil, nil, err
	}
	snap, err := store.Snapshot()
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	if snap.State == nil {
		store.Close()
		return nil, nil, &recoveryRefusal{message: "the store holds no saved state yet"}
	}
	ev, err := collectPublishEvidence(c, dir, r, snap, snap.State)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	return ev, store, nil
}

func runPublishPreview(c Config, dir string, out, stderr io.Writer) int {
	if repositoryOwned(c) {
		p := operationFailure("controller_owns_repository", "A controller owns this repository. Stop it before publishing; the handoff needs the stopped-run lock.")
		p.Actions = []ErrorAction{{Description: "Request a drain and wait for the controller to stop.", Command: "looper stop --wait"}}
		p.withLegacy("A controller owns this repository. Stop it before publishing.", 1)
		return emitError(c, p, out, stderr)
	}
	ev, store, err := publishOpenEvidence(c, dir)
	if err != nil {
		return emitError(c, publishEvidenceError(err), out, stderr)
	}
	defer store.Close()
	doc, err := buildPublishPreview(c, dir, ev.repo, ev.snap, ev.s)
	if err != nil {
		return emitError(c, publishEvidenceError(err), out, stderr)
	}
	if c.JSON && c.Contract != ContractVersionV2 {
		p := incompatibleFailure("contract_version_required", "The publication document is a contract version 2 document.")
		p.Expected = map[string]any{"contract": "v2"}
		p.Observed = map[string]any{"contract": "legacy", "option": "--json"}
		p.Actions = []ErrorAction{{Description: "Request the preview with the version 2 contract.", Command: "looper publish --preview --contract v2 --json"}}
		return emitError(c, p, out, stderr)
	}
	if c.JSON {
		json.NewEncoder(out).Encode(doc)
		return 0
	}
	renderPublishPreview(doc, out)
	return 0
}

func publishEvidenceError(err error) *PublicError {
	var refusal *recoveryRefusal
	if errors.As(err, &refusal) {
		p := operationFailure("publish_unavailable", refusal.message)
		p.Retry = retryNo
		return p
	}
	return stateUnreadableError("", err)
}

// publishAcquireIndexLock creates the real Git index lock exclusively. An
// existing lock always belongs to someone else and is never removed.
func publishAcquireIndexLock(common string) (*os.File, error) {
	return os.OpenFile(filepath.Join(common, "index.lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
}

// publishBuildTree writes the composed tree through one prepared index and
// returns its object identifier. The accepted task-file blob enters the
// object database first, because the composed tree references it; every
// other object already exists in the accepted tree.
func publishBuildTree(c Config, repo *Repository, ev *publishEvidence, prepared string) (string, error) {
	if ev.taskTracked && ev.acceptedBytes != nil {
		if _, err := gitCommand(c.Root, ev.acceptedBytes, nil, "hash-object", "-w", "-t", "blob", "--stdin"); err != nil {
			return "", fmt.Errorf("the accepted task-file bytes could not be stored: %w", err)
		}
	}
	env := map[string]string{"GIT_INDEX_FILE": prepared}
	if _, err := gitCommand(c.Root, nil, env, "read-tree", ev.s.Published); err != nil {
		return "", err
	}
	// --index-info consumes ls-tree lines verbatim; a zero-mode entry
	// removes the path. One call installs the whole composition.
	var input strings.Builder
	input.WriteString(ev.canonicalEntries())
	for path := range ev.publishedEntries {
		if _, ok := ev.entries[path]; !ok {
			input.WriteString("0 0000000000000000000000000000000000000000\t")
			input.WriteString(path)
			input.WriteByte(0)
		}
	}
	if _, err := gitCommand(c.Root, []byte(input.String()), env, "update-index", "-z", "--add", "--replace", "--index-info"); err != nil {
		return "", fmt.Errorf("the composed index could not be prepared: %w", err)
	}
	out, err := gitCommand(c.Root, nil, env, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// publishCommit creates the project commit with the configured Git identity.
// It never reuses Repository.Commit, which forces the Looper identity.
func publishCommit(c Config, repo *Repository, tree, parent, message string, ev *publishEvidence) (string, error) {
	args := []string{"commit-tree", tree, "-p", parent}
	if ev.signing == "configured" {
		args = append(args, "-S")
	}
	env := map[string]string{
		"GIT_AUTHOR_NAME": ev.authorName, "GIT_AUTHOR_EMAIL": ev.authorEmail,
		"GIT_COMMITTER_NAME": ev.authorName, "GIT_COMMITTER_EMAIL": ev.authorEmail,
	}
	out, err := gitCommand(c.Root, []byte(message+"\n"), env, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// publishInstallIndex installs the prepared index under the held index lock:
// the prepared bytes replace the lock file and rename over the index, which
// releases the lock atomically with the installation.
func publishInstallIndex(common, prepared string) error {
	data, err := os.ReadFile(prepared)
	if err != nil {
		return err
	}
	lockPath := filepath.Join(common, "index.lock")
	if err := os.WriteFile(lockPath, data, 0600); err != nil {
		return err
	}
	return os.Rename(lockPath, filepath.Join(common, "index"))
}

// runPublishApply applies one previewed publication. The guards and the token
// are re-derived under the repository lock; a prepared journal from an
// interrupted application resolves first.
func runPublishApply(c Config, dir string, out, stderr io.Writer) int {
	if c.JSON && c.Contract != ContractVersionV2 {
		p := incompatibleFailure("contract_version_required", "The publication receipt is a contract version 2 document.")
		p.Expected = map[string]any{"contract": "v2"}
		p.Observed = map[string]any{"contract": "legacy", "option": "--json"}
		p.Actions = []ErrorAction{{Description: "Request the application with the version 2 contract.", Command: "looper publish --apply --token <token> --contract v2 --json"}}
		return emitError(c, p, out, stderr)
	}
	repo := c
	repo.GitInit = false
	r, err := OpenRepository(repo, dir)
	if err != nil {
		return emitError(c, publishEvidenceError(err), out, stderr)
	}
	if repositoryOwned(c) {
		p := operationFailure("controller_owns_repository", "A controller owns this repository. Stop it before publishing; the handoff needs the stopped-run lock.")
		p.Actions = []ErrorAction{{Description: "Request a drain and wait for the controller to stop.", Command: "looper stop --wait"}}
		p.withLegacy("A controller owns this repository. Stop it before publishing.", 1)
		return emitError(c, p, out, stderr)
	}
	lock, err := lockRepository(r.Common)
	if err != nil {
		p := operationFailure("controller_owns_repository", "A controller owns this repository; the publication runs inside it.")
		p.withLegacy("A controller already owns this repository.", 1)
		return emitError(c, p, out, stderr)
	}
	defer lock.Close()
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		return emitError(c, stateUnreadableError(filepath.Join(dir, "state.db"), err), out, stderr)
	}
	defer store.Close()
	s, err := store.Load()
	if err != nil {
		return emitError(c, stateUnreadableError(filepath.Join(dir, "state.db"), err), out, stderr)
	}
	if s == nil {
		p := runNotFoundForRun(c, filepath.Join(dir, "state.db"), "looper")
		p.Message = "no saved run; the store holds no state yet"
		return emitError(c, p, out, stderr)
	}
	payloadDigest := publishPayloadDigest(c.RecoveryToken, orDefault(c.PublishMessage, publishDefaultMessage))
	// Resolve an interrupted application first: its journal owns the pending
	// state, whatever token the caller brought.
	if pending, _ := loadNewestPublishJournal(dir); pending != nil {
		resolved, resErr := resolvePublishJournal(c, dir, r, store, s, pending)
		if resErr != nil {
			return emitError(c, publishOperationalError(resErr), out, stderr)
		}
		if pending.Token != c.RecoveryToken {
			p := operationFailure("publish_pending_resolved", fmt.Sprintf("An interrupted publication (%s, state %s) was resolved first; its outcome is recorded. Preview again to publish newer work.", pending.ID, resolved))
			p.Retry = retryAfterChange
			p.Actions = []ErrorAction{{Description: "Preview the publication again.", Command: "looper publish --preview --contract v2 --json"}}
			p.withLegacy("An interrupted publication was resolved first; preview again.", 1)
			return emitError(c, p, out, stderr)
		}
		if pending.State == publishConflict {
			p := operationFailure("publish_conflict", pending.Conflict)
			p.Retry = retryAfterChange
			p.Observed = map[string]any{"journal": pending.ID, "state": pending.State}
			p.Actions = []ErrorAction{{Description: "Inspect both states, reconcile them by hand, and publish again.", Command: "looper publish --preview --contract v2 --json"}}
			p.withLegacy(pending.Conflict, 1)
			return emitError(c, p, out, stderr)
		}
		return reportPublishApplied(c, dir, r, s, pending, payloadDigest == pending.PayloadDigest, out)
	}
	// A caller key answers from the terminal journal that recorded it.
	if c.IdempotencyKey != "" {
		if j := publishJournalByKey(dir, c.IdempotencyKey, payloadDigest); j != nil {
			return reportPublishApplied(c, dir, r, s, j, true, out)
		}
		for _, j := range listPublishJournals(dir) {
			if j.IdempotencyKey == c.IdempotencyKey {
				p := operationFailure("idempotency_key_conflict", keyConflictMessage(c.IdempotencyKey, &RequestRecord{ID: j.ID}, payloadDigest))
				p.Retry = retryAfterChange
				p.withLegacy(keyConflictMessage(c.IdempotencyKey, &RequestRecord{ID: j.ID}, payloadDigest), 1)
				return emitError(c, p, out, stderr)
			}
		}
	}
	// Re-derive the evidence under the lock and check the token.
	snap, err := store.Snapshot()
	if err != nil {
		return emitError(c, stateUnreadableError(filepath.Join(dir, "state.db"), err), out, stderr)
	}
	ev, err := collectPublishEvidence(c, dir, r, snap, s)
	if err != nil {
		return emitError(c, publishEvidenceError(err), out, stderr)
	}
	if !ev.publishable {
		p := operationFailure("publish_refused", "The preview is stale or the run is not publishable; nothing was applied.")
		p.Retry = retryAfterChange
		p.Observed = map[string]any{"refusals": clampList(ev.refusals, 8)}
		p.Actions = []ErrorAction{{Description: "Read the failed guards and preview again.", Command: "looper publish --preview --contract v2 --json"}}
		p.withLegacy("The run is not publishable: "+strings.Join(ev.refusals, "; "), 1)
		return emitError(c, p, out, stderr)
	}
	if observed := publishToken(ev); observed != c.RecoveryToken {
		p := operationFailure("publish_token_stale", "The token is stale: the index, the source trees, the control files, the branch, or the saved run changed since the preview. Nothing was applied.")
		p.Retry = retryAfterChange
		p.Observed = map[string]any{"token": clampString(c.RecoveryToken, 128)}
		p.Actions = []ErrorAction{{Description: "Preview again and apply the new token.", Command: "looper publish --preview --contract v2 --json"}}
		p.withLegacy("The preview token is stale. Preview again.", 1)
		return emitError(c, p, out, stderr)
	}
	// The application proper.
	j := &publishJournal{
		ID: identifier("publish"), State: publishPrepared,
		Branch: ev.branch, OldCommit: ev.head, SourceTree: ev.s.Published,
		ProposedDigest: ev.proposedDigest, OldIndexDigest: ev.indexDigest,
		Token: c.RecoveryToken, Message: orDefault(c.PublishMessage, publishDefaultMessage),
		IdempotencyKey: c.IdempotencyKey, PayloadDigest: payloadDigest,
		RequestedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err = writePublishJournal(dir, j); err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	if err = completePublishJournal(c, dir, r, store, s, j, ev); err != nil {
		return emitError(c, publishOperationalError(err), out, stderr)
	}
	if j.State == publishConflict {
		p := operationFailure("publish_conflict", j.Conflict)
		p.Retry = retryAfterChange
		p.Observed = map[string]any{"journal": j.ID}
		p.Actions = []ErrorAction{{Description: "Inspect both states, reconcile them by hand, and publish again.", Command: "looper publish --preview --contract v2 --json"}}
		p.withLegacy(j.Conflict, 1)
		return emitError(c, p, out, stderr)
	}
	return reportPublishApplied(c, dir, r, s, j, false, out)
}

// completePublishJournal drives one journal from its current state to done or
// conflict. Each boundary is idempotent: re-running from any recorded state
// reaches the same outcome.
func completePublishJournal(c Config, dir string, r *Repository, store *Store, s *State, j *publishJournal, ev *publishEvidence) error {
	var lockFile *os.File
	defer func() {
		if lockFile != nil {
			lockFile.Close()
			os.Remove(filepath.Join(r.Common, "index.lock"))
		}
	}()
	ensureLock := func() error {
		if lockFile != nil {
			return nil
		}
		f, err := publishAcquireIndexLock(r.Common)
		if err != nil {
			return fmt.Errorf("the Git index lock could not be acquired; another Git command holds it")
		}
		lockFile = f
		return nil
	}
	for {
		switch j.State {
		case publishPrepared:
			if listing, err := indexListing(c.Root); err != nil {
				return err
			} else if digest([]byte(listing)) != j.OldIndexDigest {
				j.State = publishConflict
				j.Conflict = "the index changed after the preview; the prepared publication is preserved and nothing moved. Reconcile the index by hand and publish again."
				return writePublishJournal(dir, j)
			}
			head, branch, _ := r.HeadDetail()
			switch {
			case head == j.OldCommit:
				if branch != j.Branch {
					j.State = publishConflict
					j.Conflict = fmt.Sprintf("the checked-out branch changed to %s; the prepared publication is preserved and nothing moved", branchLabel(branch))
					return writePublishJournal(dir, j)
				}
				if j.NewCommit == "" {
					if ev == nil {
						j.State = publishConflict
						j.Conflict = "the prepared publication stopped before its commit was built; nothing moved and the preview can run again"
						return writePublishJournal(dir, j)
					}
					if ev.proposedDigest != j.ProposedDigest {
						j.State = publishConflict
						j.Conflict = "the composed tree changed since the publication prepared; nothing moved. Preview again."
						return writePublishJournal(dir, j)
					}
					prepared := filepath.Join(publishJournalDir(dir), j.ID+".index")
					tree, err := publishBuildTree(c, r, ev, prepared)
					if err != nil {
						return err
					}
					commit, err := publishCommit(c, r, tree, j.OldCommit, j.Message, ev)
					if err != nil {
						return err
					}
					j.Tree, j.NewCommit, j.PreparedIndex = tree, commit, prepared
					// The journal records the commit before the branch can
					// move; the private ref keeps it reachable whatever
					// happens next.
					if err = writePublishJournal(dir, j); err != nil {
						return err
					}
					if _, err = gitText(c.Root, "update-ref", publishPrivateRef+j.ID, j.NewCommit); err != nil {
						return err
					}
				}
				if err := ensureLock(); err != nil {
					return err
				}
				// The branch records the full ref, so the compare-and-swap
				// needs no prefix.
				if _, err := gitText(c.Root, "update-ref", j.Branch, j.NewCommit, j.OldCommit); err != nil {
					// A racing writer moved the branch; report the conflict
					// with both revisions preserved.
					if now, _, _ := r.HeadDetail(); now != j.OldCommit && now != j.NewCommit {
						j.State = publishConflict
						j.Conflict = fmt.Sprintf("the branch moved to %s while the publication applied; the prepared commit %s is preserved on %s and nothing else changed", shortRevision(now), shortRevision(j.NewCommit), publishPrivateRef+j.ID)
						return writePublishJournal(dir, j)
					}
					return err
				}
				j.State = publishCommitted
				if err := writePublishJournal(dir, j); err != nil {
					return err
				}
			case j.NewCommit != "" && head == j.NewCommit:
				// The branch update survived a crash before the journal.
				j.State = publishCommitted
				if err := writePublishJournal(dir, j); err != nil {
					return err
				}
			default:
				j.State = publishConflict
				j.Conflict = fmt.Sprintf("the branch holds %s, neither the previewed %s nor the prepared %s; both states are preserved", shortRevision(head), shortRevision(j.OldCommit), shortRevision(j.NewCommit))
				return writePublishJournal(dir, j)
			}
		case publishCommitted:
			if err := ensureLock(); err != nil {
				return err
			}
			// Install only onto the recorded old index state, or the already
			// installed one; any other index state is a preserved conflict.
			current, err := indexDigest(c.Root)
			if err != nil {
				return err
			}
			if current != j.OldIndexDigest && current != publishInstalledIndexDigest(c, j.NewCommit) {
				j.State = publishConflict
				j.Conflict = "the index changed outside the publication while it applied; the moved branch is preserved and the index was not overwritten. Reconcile the index by hand."
				return writePublishJournal(dir, j)
			}
			prepared, err := publishRebuildIndex(c, r, j)
			if err != nil {
				return err
			}
			if err = publishInstallIndex(r.Common, prepared); err != nil {
				return err
			}
			os.Remove(prepared)
			lockFile = nil // the rename released the lock
			j.State = publishInstalled
			if err = writePublishJournal(dir, j); err != nil {
				return err
			}
		case publishInstalled:
			head, branch, _ := r.HeadDetail()
			if head != j.NewCommit || branch != j.Branch {
				j.State = publishConflict
				j.Conflict = fmt.Sprintf("the branch moved after the publication installed its index (now %s on %s); the publication commit is preserved and nothing was reset", shortRevision(head), branchLabel(branch))
				return writePublishJournal(dir, j)
			}
			// Reconcile the saved baseline so a later resume accepts the new
			// head instead of comparing it against the old baseline.
			old := s.Head
			s.Head = j.NewCommit
			if err := store.Save(s); err != nil {
				s.Head = old
				return err
			}
			if _, err := store.Event("publish", fmt.Sprintf("Published accepted work to %s as %s; the run baseline moved from %s.", branchLabel(j.Branch), shortRevision(j.NewCommit), shortRevision(j.OldCommit)), map[string]any{
				"journal": j.ID, "branch": j.Branch, "old_commit": j.OldCommit, "new_commit": j.NewCommit,
				"accepted_tree": j.SourceTree, "request_id": j.ID,
			}); err != nil {
				return err
			}
			j.State = publishDone
			j.Outcome = fmt.Sprintf("committed the accepted work to %s as %s", branchLabel(j.Branch), shortRevision(j.NewCommit))
			return writePublishJournal(dir, j)
		case publishDone:
			return nil
		case publishConflict:
			return nil
		default:
			return fmt.Errorf("unknown publication journal state %q", j.State)
		}
	}
}

// publishInstalledIndexDigest derives the index state a completed
// installation holds: the staged entries of the published commit. It is
// deterministic, so recovery can recognize an installation that raced its
// journal write.
func publishInstalledIndexDigest(c Config, commit string) string {
	f, err := os.CreateTemp(os.TempDir(), "looper-publish-index-")
	if err != nil {
		return ""
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	defer os.Remove(name)
	env := map[string]string{"GIT_INDEX_FILE": name}
	if _, err = gitCommand(c.Root, nil, env, "read-tree", commit); err != nil {
		return ""
	}
	listing, err := gitCommand(c.Root, nil, env, "ls-files", "--stage", "-z")
	if err != nil {
		return ""
	}
	return digest(listing)
}

// indexDigest digests the live index listing.
func indexDigest(root string) (string, error) {
	listing, err := indexListing(root)
	if err != nil {
		return "", err
	}
	return digest([]byte(listing)), nil
}

// publishRebuildIndex rebuilds the prepared index from the published commit:
// its staged entries are exactly the installation. A prepared file that still
// exists is verified against the same tree instead.
func publishRebuildIndex(c Config, r *Repository, j *publishJournal) (string, error) {
	if j.PreparedIndex != "" {
		if _, err := os.Stat(j.PreparedIndex); err == nil {
			env := map[string]string{"GIT_INDEX_FILE": j.PreparedIndex}
			out, err := gitCommand(c.Root, nil, env, "write-tree")
			if err == nil && j.Tree != "" && strings.TrimSpace(string(out)) == j.Tree {
				return j.PreparedIndex, nil
			}
		}
	}
	f, err := os.CreateTemp(r.Dir, "publish-index-")
	if err != nil {
		return "", err
	}
	prepared := f.Name()
	f.Close()
	os.Remove(prepared)
	env := map[string]string{"GIT_INDEX_FILE": prepared}
	if _, err = gitCommand(c.Root, nil, env, "read-tree", j.NewCommit); err != nil {
		os.Remove(prepared)
		return "", err
	}
	return prepared, nil
}

// resolvePublishJournal drives one interrupted journal to a terminal state
// and returns the state it reached.
func resolvePublishJournal(c Config, dir string, r *Repository, store *Store, s *State, j *publishJournal) (string, error) {
	if j.State == publishDone || j.State == publishConflict {
		return j.State, nil
	}
	ev, err := collectPublishEvidence(c, dir, r, mustSnapshot(store), s)
	if err != nil {
		return j.State, err
	}
	if err := completePublishJournal(c, dir, r, store, s, j, ev); err != nil {
		return j.State, err
	}
	return j.State, nil
}

func mustSnapshot(store *Store) *StoreSnapshot {
	snap, err := store.Snapshot()
	if err != nil || snap == nil {
		return &StoreSnapshot{}
	}
	return snap
}

func publishOperationalError(err error) *PublicError {
	p := operationFailure("publish_failed", err.Error())
	p.Actions = []ErrorAction{{Description: "The journal preserves the operation; re-run the same application to complete it.", Command: "looper publish --apply --token <token> --contract v2 --json"}}
	p.withLegacy(err.Error(), 1)
	return p
}

// reportPublishApplied renders the terminal outcome of one journal.
func reportPublishApplied(c Config, dir string, r *Repository, s *State, j *publishJournal, duplicate bool, out io.Writer) int {
	changes := 0
	if lines, err := lsTreeLines(c.Root, j.NewCommit); err == nil {
		headEntries, _ := lsTreeEntries(c.Root, j.OldCommit)
		entries := map[string]string{}
		for _, line := range lines {
			entries[treeEntryPath(line)] = line
		}
		for path, line := range entries {
			if headEntries[path] != line {
				changes++
			}
		}
		for path := range headEntries {
			if _, ok := entries[path]; !ok {
				changes++
			}
		}
	}
	var retainedTasks []string
	for _, a := range s.Attempts {
		switch a.Phase {
		case "done", "blocked", "cancelled":
			continue
		}
		if !containsString(retainedTasks, a.Task.ID) {
			retainedTasks = append(retainedTasks, a.Task.ID)
		}
	}
	sort.Strings(retainedTasks)
	doc := &publishAppliedDocument{
		ContractVersion: ContractVersionV2,
		SchemaVersion:   publishSchemaVersion,
		Kind:            publishAppliedKind,
		RequestID:       j.ID,
		IdempotencyKey:  j.IdempotencyKey,
		Duplicate:       duplicate,
		Branch:          publishBranchResultView{Name: branchLabel(j.Branch), OldCommit: j.OldCommit, NewCommit: j.NewCommit},
		AcceptedTree:    j.SourceTree,
		Changes:         changes,
		ControlFiles:    []publishControlFileView{},
		Journal:         publishJournalDoneView{ID: j.ID, State: j.State},
		Retained:        publishRetainedView{Attempts: len(retainedTasks), Tasks: retainedTasks},
		Resume: publishResumeView{Command: "looper",
			Note: "Resume the controller when more work should run; it accepts the published head and repeats no completed work."},
	}
	if c.JSON {
		json.NewEncoder(out).Encode(doc)
		return 0
	}
	fmt.Fprintf(out, "Published %s: %s -> %s on %s (%d changed path(s))\n", j.ID, shortRevision(j.OldCommit), shortRevision(j.NewCommit), branchLabel(j.Branch), changes)
	fmt.Fprintf(out, "Retained work: %d task(s) keep their state and artifacts.\n", len(retainedTasks))
	fmt.Fprintln(out, "Resume with: looper")
	return 0
}
