package looper

import (
	"fmt"
	"sort"
	"strings"
)

// Branch states the read-only comparison reports. They mirror the decisions
// recoverCommittedHead makes: a matching head needs no recovery, a
// same-branch descendant whose committed and working content still match the
// saved trees is accepted at startup, a descendant whose committed delta
// touches no retained or published write scope folds into the baseline
// (T046), and every other observed state refuses with the check that failed.
const (
	branchMatch       = "match"
	branchRecoverable = "recoverable"
	branchMoved       = "moved"
	branchUnborn      = "unborn"
	branchDetached    = "detached"
	branchUnreadable  = "unreadable"
	branchUnknown     = "unknown"
)

// branchRead is the read-only form of the startup branch check (AX-016). It
// performs the same comparisons recoverCommittedHead performs, without
// writing: no snapshot is hashed, no state saved, no ref read twice. A check
// that cannot run leaves the state unknown with its reason, so the recovery
// preview never presents an undecided comparison as a pass.
type branchRead struct {
	state   string
	reason  string // why the state is not acceptable, or what an acceptance accepted
	unborn  bool
	unknown string // why a check could not run
}

func (r *Repository) compareBranchRecovery(s *State, head, branch string, unborn bool) branchRead {
	if head == s.Head && branch == s.Branch {
		return branchRead{state: branchMatch}
	}
	moved := func(reason string, unknown string) branchRead {
		state := branchMoved
		if unknown != "" {
			state = branchUnknown
		}
		return branchRead{state: state, reason: reason, unknown: unknown}
	}
	if unborn {
		return branchRead{
			state:  branchUnborn,
			reason: fmt.Sprintf("the project HEAD is unborn: branch %s has no commits, so the retained baseline %s cannot be compared; make a commit on the saved branch %s or restore it before resuming", branchLabel(branch), s.Head, branchLabel(s.Branch)),
		}
	}
	if branch == "" {
		return moved("HEAD is detached, so no branch can be compared with the retained branch "+branchLabel(s.Branch), "")
	}
	if head == "" {
		return moved("HEAD could not be resolved to a commit", "")
	}
	if branch != s.Branch {
		return moved(
			fmt.Sprintf("the checked-out branch %s differs from the retained branch %s", branchLabel(branch), branchLabel(s.Branch)), "")
	}
	if _, err := gitRead(r.Root, "merge-base", "--is-ancestor", s.Head, head); err != nil {
		return moved("the retained head is not an ancestor of the observed head", "")
	}
	// The content checks reuse the listings the recovery preview collected;
	// a failed query keeps its reason instead of an empty result.
	paths := []string{"--", "."}
	for _, path := range r.Excluded {
		paths = append(paths, ":(exclude)"+path)
	}
	if staged, err := readPaths(r.Root, append([]string{"diff", "--cached", "--name-only", "-z", head}, paths...)...); err != nil {
		return moved("the staged files could not be read: "+err.Error(), "")
	} else if len(staged) > 0 {
		return moved("the index holds staged changes the resume would overwrite", "")
	}
	trees := []string{s.Published}
	if p := s.Publication; p != nil {
		trees = append(trees, p.Tree)
	}
	for _, tree := range trees {
		committed, err := readPaths(r.Root, append([]string{"diff", "--name-only", "-z", head, tree}, paths...)...)
		if err != nil {
			return moved(fmt.Sprintf("the committed changes against tree %s could not be read: %v", tree, err), "")
		}
		if len(committed) > 0 {
			// A descendant head whose committed delta touches no retained or
			// published write scope folds into the baseline at resume (T046).
			// The mirror stays read-only: it reuses the listings a preview can
			// collect instead of hashing the working tree.
			if accepted, note := ownerCommitAccepted(r, s, head, paths); accepted {
				return branchRead{state: branchRecoverable, reason: note}
			}
			return moved(fmt.Sprintf("committed files differ from the retained tree %s", tree), "")
		}
	}
	return branchRead{state: branchRecoverable}
}

// pathInside reports whether one repository path falls inside a write-scope
// prefix, using the same containment rule overlaps applies to live dispatch.
// A trailing slash on the prefix changes nothing.
func pathInside(path, prefix string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	return prefix == "." || path == prefix || strings.HasPrefix(path, prefix+"/")
}

// planWritePaths returns one task's planned write paths and whether the plan
// lists the task at all. An entry without paths covers the repository root,
// exactly as Plan.Validate normalizes it.
func planWritePaths(p *Plan, id string) (paths []string, listed bool) {
	for _, s := range p.Tasks {
		if s.TaskID != id {
			continue
		}
		if len(s.Paths) == 0 {
			return []string{"."}, true
		}
		return s.Paths, true
	}
	return nil, false
}

// protectedScopePaths lists the repository paths an accepted owner commit
// must not touch (T046): the write scope of every attempt the run retains or
// published, including the observed paths the engine records on the plan. An
// absent plan, a task the plan does not list, or a "." entry protects the
// whole repository, because an uncertain scope never accepts a competing
// commit.
func protectedScopePaths(s *State) (paths []string, wholeRepository bool) {
	seen := map[string]bool{}
	for _, a := range s.Attempts {
		if s.Plan == nil {
			return nil, true
		}
		scope, listed := planWritePaths(s.Plan, a.Task.ID)
		if !listed {
			return nil, true
		}
		for _, p := range scope {
			if p == "." {
				return nil, true
			}
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	sort.Strings(paths)
	return paths, false
}

// scopeConflicts reports the changed paths that fall inside the protected
// write scopes. With the whole repository protected every changed path is a
// conflict.
func scopeConflicts(s *State, changed []string) []string {
	protected, whole := protectedScopePaths(s)
	if whole {
		return changed
	}
	var conflicts []string
	for _, p := range changed {
		for _, prefix := range protected {
			if pathInside(p, prefix) {
				conflicts = append(conflicts, p)
				break
			}
		}
	}
	return conflicts
}

// shortRevision shortens one commit digest for messages; guard evidence keeps
// the complete value.
func shortRevision(revision string) string {
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

// splitZPaths splits one NUL-separated git name-only listing into its
// non-empty paths. A path can contain a newline, so the -z form never splits
// on lines.
func splitZPaths(out string) []string {
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// pathSetDifference returns the paths of a that b does not hold.
func pathSetDifference(a, b []string) []string {
	known := make(map[string]bool, len(b))
	for _, p := range b {
		known[p] = true
	}
	var out []string
	for _, p := range a {
		if !known[p] {
			out = append(out, p)
		}
	}
	return out
}

// ownerCommitAccepted is the read-only mirror of the T046 fold decision: the
// committed delta since the saved head avoids every protected write scope,
// the working tree carries no change beyond that delta, and the committed
// content stands in the working tree. The engine's fold hashes the working
// tree through SnapshotFrom; this mirror approximates that hash with the
// tracked, staged, and untracked listings a read-only preview can collect.
func ownerCommitAccepted(r *Repository, s *State, head string, paths []string) (bool, string) {
	deltaOut, err := gitRead(r.Root, append([]string{"diff", "--name-only", "-z", s.Head, head}, paths...)...)
	if err != nil {
		return false, ""
	}
	delta := splitZPaths(deltaOut)
	if len(scopeConflicts(s, delta)) > 0 {
		return false, ""
	}
	workingOut, err := gitRead(r.Root, append([]string{"diff", "--name-only", "-z", s.Published}, paths...)...)
	if err != nil {
		return false, ""
	}
	working := splitZPaths(workingOut)
	untracked, err := readPaths(r.Root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return false, ""
	}
	// The untracked listing names control files and the runtime tree the
	// snapshot never carries; only project content counts as a change.
	for _, p := range untracked {
		if !r.excluded(p) {
			working = append(working, p)
		}
	}
	if extra := pathSetDifference(working, delta); len(extra) > 0 {
		return false, ""
	}
	// The committed paths must hold the committed content: a later
	// uncommitted edit rides no acceptance in. Beside the run's own
	// integrated changes, the working tree may not drift from the head.
	knownOut, err := gitRead(r.Root, append([]string{"diff", "--name-only", "-z", s.Head, s.Published}, paths...)...)
	if err != nil {
		return false, ""
	}
	driftOut, err := gitRead(r.Root, append([]string{"diff", "--name-only", "-z", head}, paths...)...)
	if err != nil {
		return false, ""
	}
	if drift := pathSetDifference(splitZPaths(driftOut), splitZPaths(knownOut)); len(drift) > 0 {
		return false, ""
	}
	return true, fmt.Sprintf("startup accepts the descendant head because its committed changes touch no retained write scope; the %d changed path(s) fold into the run baseline at resume", len(delta))
}

// recoverCommittedHead accepts a new project commit at a boundary the engine
// holds the repository lock: startup recovery, publication, and the baseline
// refresh all route through here (T046). Two forms qualify: a head whose tree
// still matches the saved trees, and a same-branch descendant whose committed
// delta touches no file inside any retained or published attempt's write
// scope, which foldOwnerCommit folds into the baseline. It reports whether
// the baseline moved, so a caller holding base-sensitive work requeues it.
// Publication still performs its normal contract and tree checks. Every
// refusal is a structured branch guard (AX-015): the saved and observed
// revisions, the retained task identities, and the bounded changed-path
// evidence for the check that failed. An unborn HEAD is reported as unborn,
// never as a branch that moved.
func (e *Engine) recoverCommittedHead() (bool, error) {
	head, branch, unborn := e.Repo.HeadDetail()
	if head == e.State.Head && branch == e.State.Branch {
		return false, nil
	}
	expected := map[string]any{"head": e.State.Head, "branch": e.State.Branch}
	if unborn {
		observed := map[string]any{"head": headUnborn, "branch": branch, "unborn_head": true}
		return false, e.retain(&retentionError{
			code: "branch_moved",
			message: fmt.Sprintf("the project HEAD is unborn: branch %s has no commits, so the retained baseline %s cannot be compared; work is retained in %s. Make a commit on the saved branch %s or restore it before resuming",
				branchLabel(branch), e.State.Head, e.Dir, branchLabel(e.State.Branch)),
			expected: expected, observed: observed,
		}, unbornDifference("committed_changes", e.State.Head))
	}
	moved := func(reason string, differences ...pathDifference) error {
		return e.retain(&retentionError{
			code: "branch_moved",
			message: fmt.Sprintf("the project branch moved; retained work is in %s. Resume requires the same branch and project content no retained work competes with: %s",
				e.Dir, reason),
			expected: expected,
			observed: map[string]any{"head": head, "branch": branch},
		}, differences...)
	}
	if branch == "" {
		return false, moved("HEAD is detached, so no branch can be compared with the retained branch " + branchLabel(e.State.Branch))
	}
	if head == "" {
		return false, moved("HEAD could not be resolved to a commit")
	}
	if branch != e.State.Branch {
		return false, moved(
			"the checked-out branch "+branchLabel(branch)+" differs from the retained branch "+branchLabel(e.State.Branch),
			e.changedPaths("committed_changes", e.State.Head, head))
	}
	if _, err := gitText(e.Config.Root, "merge-base", "--is-ancestor", e.State.Head, head); err != nil {
		return false, moved(
			"the retained head is not an ancestor of the observed head",
			e.changedPaths("committed_changes", e.State.Head, head))
	}
	trees := []string{e.State.Published}
	if p := e.State.Publication; p != nil {
		trees = append(trees, p.Tree)
	}
	paths := []string{"--", "."}
	for _, path := range e.Repo.Excluded {
		paths = append(paths, ":(exclude)"+path)
	}
	staged, err := gitText(e.Config.Root, append([]string{"diff", "--cached", "--name-only", "-z", head}, paths...)...)
	if err != nil {
		return false, moved("the staged files could not be read: " + err.Error())
	}
	if staged != "" {
		listed, total := boundPaths(splitZPaths(staged))
		return false, moved("the index holds staged changes the resume would overwrite", pathDifference{
			Label: "staged_files", From: head, To: "the index",
			Paths: listed, Total: total,
			Command: "git diff --name-only --cached " + head,
		})
	}
	candidates := []struct{ label, tree string }{
		{"accepted_tree_changes", e.State.Published},
	}
	if p := e.State.Publication; p != nil {
		candidates = append(candidates, struct{ label, tree string }{"verified_tree_changes", p.Tree})
	}
	matched := false
	var differences []pathDifference
	for i, tree := range trees {
		changed, err := gitText(e.Config.Root, append([]string{"diff", "--name-only", "-z", head, tree}, paths...)...)
		if err != nil {
			differences = append(differences, pathDifference{
				Label: candidates[i].label, From: head, To: tree,
				Unknown: clampString(fmt.Sprintf("the changed paths between %s and %s: %v", head, tree, err), maxErrorString),
			})
			continue
		}
		if lines := splitZPaths(changed); len(lines) > 0 {
			listed, total := boundPaths(lines)
			differences = append(differences, pathDifference{
				Label: candidates[i].label, From: head, To: tree,
				Paths: listed, Total: total,
				Command: fmt.Sprintf("git diff --name-only %s %s", head, tree),
			})
			continue
		}
		observed, err := e.Repo.SnapshotFrom(e.Config.Root, tree, trees...)
		if err == nil && observed == tree {
			matched = true
			break
		}
		differences = append(differences, pathDifference{
			Label: candidates[i].label, From: head, To: tree,
			Unknown: "the working tree differs from " + tree + " in content the committed diff does not show, for example ignored or untracked files",
		})
	}
	if !matched {
		folded, refusal, evidence, ferr := e.foldOwnerCommit(head, paths)
		if ferr != nil {
			return false, ferr
		}
		if folded {
			return true, e.err
		}
		if refusal != "" {
			return false, moved(refusal, evidence...)
		}
		return false, moved("committed or working files differ from the accepted and verified trees", differences...)
	}
	// Do not accept a ref that moved during the comparison.
	current, currentBranch := e.Repo.Head()
	if current != head || currentBranch != branch {
		return false, moved("the branch moved again while the comparison ran")
	}
	old := e.State.Head
	e.State.Head = head
	if err := e.save(); err != nil {
		e.State.Head = old
		return false, err
	}
	e.eventData("recovery", "Accepted a new commit with unchanged project content; resuming retained work.", map[string]string{"previous_head": old, "head": head})
	return true, e.err
}

// original guards publication against a moved project head. An owner commit
// that touches no retained write scope folds into the baseline (T046), and
// the caller requeues its verified result against the folded base; a move
// that competes with retained work still retains everything.
func (e *Engine) original() (bool, error) {
	return e.recoverCommittedHead()
}

// foldOwnerCommit evaluates one same-branch descendant head for the T046
// acceptance and folds it into the run baseline when it qualifies. The
// committed delta must avoid every retained or published attempt's write
// scope, and the working tree must carry exactly that committed content
// against the published tree, so the fold never discards, overwrites, or
// quietly amends retained work. On acceptance the chain gains a merge commit
// of the moved head and the current tip, the published tree becomes the
// folded content, and a pending publication requeues for verification against
// the new base. A refusal returns its reason with guard evidence; an
// operational failure returns err unchanged.
func (e *Engine) foldOwnerCommit(head string, paths []string) (folded bool, refusal string, evidence []pathDifference, err error) {
	deltaOut, derr := gitText(e.Config.Root, append([]string{"diff", "--name-only", "-z", e.State.Head, head}, paths...)...)
	if derr != nil {
		return false, "", []pathDifference{{
			Label: "owner_commit_changes", From: e.State.Head, To: head,
			Unknown: clampString("the committed changes since the retained head: "+derr.Error(), maxErrorString),
		}}, nil
	}
	delta := splitZPaths(deltaOut)
	if conflicts := scopeConflicts(e.State, delta); len(conflicts) > 0 {
		listed, total := boundPaths(conflicts)
		return false, fmt.Sprintf("the commit changed %d path(s) inside the write scopes of retained or published work", total), []pathDifference{{
			Label: "write_scope_conflicts", From: e.State.Head, To: head,
			Paths: listed, Total: total,
			Command: fmt.Sprintf("git diff --name-only %s %s", e.State.Head, head),
		}}, nil
	}
	// The folded content is the working tree as it stands: it holds the
	// published content beside the owner's commit. An uncommitted edit beyond
	// the commit keeps its existing conflict handling instead of riding the
	// acceptance in.
	tree, terr := e.Repo.SnapshotFrom(e.Config.Root, e.State.Published)
	if terr != nil {
		return false, "", []pathDifference{{
			Label: "folded_content", From: e.State.Published, To: "the working tree",
			Unknown: clampString("the working tree could not be captured: "+terr.Error(), maxErrorString),
		}}, nil
	}
	workingOut, werr := gitText(e.Config.Root, append([]string{"diff", "--name-only", "-z", e.State.Published, tree}, paths...)...)
	if werr != nil {
		return false, "", nil, werr
	}
	if extra := pathSetDifference(splitZPaths(workingOut), delta); len(extra) > 0 {
		listed, total := boundPaths(extra)
		return false, "working files differ from the published tree beyond the committed changes", []pathDifference{{
			Label: "uncommitted_changes", From: e.State.Published, To: "the working tree",
			Paths: listed, Total: total,
			Command: fmt.Sprintf("git diff --name-only %s", e.State.Published),
		}}, nil
	}
	// The committed paths must hold the committed content, not a later
	// uncommitted edit. Beside the run's own integrated changes, the working
	// tree may not drift from the observed head.
	knownOut, kerr := gitText(e.Config.Root, append([]string{"diff", "--name-only", "-z", e.State.Head, e.State.Published}, paths...)...)
	if kerr != nil {
		return false, "", nil, kerr
	}
	driftOut, derr2 := gitText(e.Config.Root, append([]string{"diff", "--name-only", "-z", head, tree}, paths...)...)
	if derr2 != nil {
		return false, "", nil, derr2
	}
	if drift := pathSetDifference(splitZPaths(driftOut), splitZPaths(knownOut)); len(drift) > 0 {
		listed, total := boundPaths(drift)
		return false, "working files differ from the committed content", []pathDifference{{
			Label: "uncommitted_edits", From: head, To: "the working tree",
			Paths: listed, Total: total,
			Command: fmt.Sprintf("git diff --name-only %s", head),
		}}, nil
	}
	// Do not fold a ref that moved while the comparison ran.
	current, currentBranch := e.Repo.Head()
	if current != head || currentBranch != e.State.Branch {
		return false, "the branch moved again while the acceptance ran", nil, nil
	}
	if p := e.State.Publication; p != nil {
		a := e.attempt(p.AttemptID)
		if a == nil {
			return false, "", nil, fmt.Errorf("publication references an unknown attempt")
		}
		// The verified result re-prepares against the folded base and
		// verifies again; its saved verification stays recorded beside the
		// attempt for inspection.
		a.Phase = "ready"
		e.State.Publication = nil
	}
	seed, cerr := e.Repo.Commit(tree, []string{e.State.Tip, head}, "Accept project commit for Looper")
	if cerr != nil {
		return false, "", nil, cerr
	}
	if err = e.Repo.Ref(e.State.Ref, seed); err != nil {
		return false, "", nil, err
	}
	oldHead, oldTip, oldPublished := e.State.Head, e.State.Tip, e.State.Published
	e.State.Head = head
	e.State.Tip = seed
	e.State.Published = tree
	e.State.Finished = false
	e.State.ReviewedTip = ""
	if err = e.save(); err != nil {
		e.State.Head, e.State.Tip, e.State.Published = oldHead, oldTip, oldPublished
		return false, "", nil, err
	}
	listed, total := boundPaths(delta)
	e.eventData("recovery", fmt.Sprintf("Accepted the project commit %s, which changes %d path(s) outside every retained write scope; continuing the run.", shortRevision(head), total), e.identity().merge(map[string]any{
		"previous_head": oldHead, "head": head,
		"previous_tip": oldTip, "tip": seed,
		"previous_published": oldPublished, "published": tree,
		"accepted_paths": listed, "accepted_path_count": total,
	}))
	return true, "", nil, e.err
}
