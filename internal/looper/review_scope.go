package looper

import (
	"fmt"
	"strings"
)

// Review scope modes (T128). One mode names what a due review cycle runs
// against the coverage the last applied review recorded.
const (
	// reviewScopeFull runs the ordinary agent pass. Nothing recorded permits
	// less: no coverage exists yet, a covered path changed, an import the
	// recorded review never saw applied, or the changed paths cannot be
	// listed.
	reviewScopeFull = "full"
	// reviewScopeFocused runs a scoped agent pass over a change entirely
	// outside the recorded source coverage while work stays open: the pass
	// verifies only whether the change affects the goal or recorded tasks.
	reviewScopeFocused = "focused"
	// reviewScopeSkip runs no agent pass. The published content is unchanged,
	// or the change is outside the recorded coverage and the recorded verdict
	// already closed the goal, so the cycle reconciles recorded findings
	// instead of re-reporting them.
	reviewScopeSkip = "skip"
)

// reviewScope is the decision one due review cycle carries: the mode, the
// paths changed since the recorded review, and the reason the mode holds.
type reviewScope struct {
	Mode    string
	Changed []string
	Reason  string
}

// reviewScope decides what one due review runs. The recorded coverage — the
// published tree the last applied review read, its completion verdict, and
// the imports it saw — decides:
//
//   - An unchanged published tree skips the agent pass. The same review would
//     re-read the same content and re-report the same unresolved findings at
//     full cost, so the cycle reconciles recorded findings instead: one
//     operator decision per repeated finding, nothing else.
//   - A change entirely outside the accepted backlog's source_files cannot
//     move requirements the recorded review covered. A final cycle whose
//     recorded verdict already closed the goal with no open work re-affirms
//     that completion; any other cycle verifies only the changed paths.
//   - Everything else keeps the full pass: no recorded coverage, a changed
//     path inside the coverage, an import that applied since the recorded
//     review (the final review must cover every applied import), or a diff
//     that cannot be listed.
func (e *Engine) reviewScope(final bool) reviewScope {
	if e.State.ReviewedTree == "" {
		return reviewScope{Mode: reviewScopeFull, Reason: "no review coverage is recorded"}
	}
	if applied := appliedImportCount(e.State); applied != e.State.ReviewedImports {
		return reviewScope{Mode: reviewScopeFull, Reason: fmt.Sprintf("%d import(s) applied since the review at %s", applied-e.State.ReviewedImports, e.State.ReviewedTree)}
	}
	if e.State.ReviewedTree == e.State.Published {
		return reviewScope{Mode: reviewScopeSkip, Reason: fmt.Sprintf("the published tree is unchanged since the review at %s", e.State.ReviewedTree)}
	}
	changed, err := e.Repo.Changes(e.State.ReviewedTree, e.State.Published)
	if err != nil {
		return reviewScope{Mode: reviewScopeFull, Reason: "the changed paths since the recorded review cannot be listed: " + err.Error()}
	}
	if len(changed) == 0 {
		// The tip moved without moving content, for example a refresh that
		// imported a task-file edit: the revision the review verified and the
		// published content are the same pair again.
		return reviewScope{Mode: reviewScopeSkip, Changed: changed, Reason: fmt.Sprintf("no project file changed since the review at %s", e.State.ReviewedTree)}
	}
	if e.State.Backlog.coversSources(changed) {
		return reviewScope{Mode: reviewScopeFull, Changed: changed, Reason: "a reviewed source file changed"}
	}
	if final && e.State.ReviewedComplete && !e.openWork() {
		return reviewScope{Mode: reviewScopeSkip, Changed: changed, Reason: fmt.Sprintf("every changed path is outside the reviewed source files and the review at %s closed the goal", e.State.ReviewedTree)}
	}
	return reviewScope{Mode: reviewScopeFocused, Changed: changed, Reason: "every changed path is outside the reviewed source files"}
}

// coversSources reports whether any changed path lies inside the accepted
// backlog's source coverage. Coverage follows the source_files entries the
// accepted snapshot records: a path is covered when it equals an entry or
// lies below it. An entry that cannot scope repository work — absolute,
// escaping, or patterned — covers everything, and a snapshot without entries
// records no coverage at all, so every change counts as covered and reviews
// keep their full pass.
func (b Backlog) coversSources(paths []string) bool {
	if len(b.Sources) == 0 {
		return true
	}
	for _, p := range paths {
		path, ok := validWritePath(p)
		if !ok {
			return true
		}
		for _, s := range b.Sources {
			source, ok := validWritePath(s)
			if !ok || source == "." {
				return true
			}
			if path == source || strings.HasPrefix(path, source+"/") {
				return true
			}
		}
	}
	return false
}

// appliedImportCount counts the import receipts a state records as applied.
func appliedImportCount(s *State) int {
	n := 0
	for _, r := range s.Imports {
		if r != nil && r.Status == ImportApplied {
			n++
		}
	}
	return n
}

// openWork reports whether an ordinary task is still unfinished, exactly as
// the applied review computes its completion marker: markers and done tasks
// do not count.
func (e *Engine) openWork() bool {
	for _, t := range e.State.Backlog.Tasks {
		if !t.Marker() && t.Status != "done" {
			return true
		}
	}
	return false
}
