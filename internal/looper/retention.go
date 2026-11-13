package looper

import (
	"fmt"
	"strings"
)

// Bounds for retained-work guard evidence (AX-015). The changed-path listing
// stays small; the complete list stays behind the read-only detail command the
// same evidence names, so no guard reply grows with the project.
const (
	maxGuardChangedPaths = 20
	maxGuardTasks        = 16
)

// headUnborn is the observed HEAD value a branch guard reports when the
// checked-out branch has no commits. An unborn HEAD is a distinct state from a
// moved branch; guards never report it as an empty revision that moved.
const headUnborn = "unborn"

// pathDifference is one bounded changed-path comparison a guard reports. The
// label keys the values in the error document. Unknown keeps the reason no
// comparison was possible, so a guard never presents an uncomputed difference
// as an empty one.
type pathDifference struct {
	Label   string // document key, for example "accepted_tree_changes"
	From    string // expected revision or tree digest
	To      string // observed revision or tree digest, or a named state
	Paths   []string
	Total   int
	Command string // read-only command that lists every changed path
	Unknown string
}

// apply writes one difference into the error document fields: the expected
// revision, the bounded observed listing with its complete count, the detail
// route, and the unknown reason when no comparison was possible.
func (d pathDifference) apply(p *PublicError) {
	p.Expected[d.Label] = d.From
	if d.Unknown != "" {
		p.Observed[d.Label] = "unknown"
		p.UnknownEvidence = append(p.UnknownEvidence, clampString(d.Unknown, maxErrorString))
		return
	}
	paths := make([]string, 0, len(d.Paths))
	for _, path := range d.Paths {
		paths = append(paths, clampString(path, maxErrorString))
	}
	p.Observed[d.Label] = paths
	p.Observed[d.Label+"_count"] = d.Total
	if d.From == "" || d.To == "" || d.From == d.To {
		return
	}
	command := d.Command
	if command == "" {
		command = fmt.Sprintf("git diff --name-only %s %s", d.From, d.To)
	}
	p.Evidence = append(p.Evidence, ErrorEvidence{
		Kind:      "file",
		Reference: clampString(fmt.Sprintf("%d changed path(s) between %s and %s; the first %d are listed", d.Total, d.From, d.To, len(paths)), maxErrorString),
		Command:   command,
	})
}

// changedPaths computes one bounded difference between two revisions or trees.
// A failed comparison is reported as unknown evidence, never as an empty
// difference.
func (e *Engine) changedPaths(label, from, to string) pathDifference {
	d := pathDifference{Label: label, From: from, To: to}
	if from == "" || to == "" {
		d.Unknown = "the changed paths: one side of the comparison has no revision to list"
		return d
	}
	if from == to {
		d.Paths = []string{}
		return d
	}
	paths, err := e.Repo.Changes(from, to)
	if err != nil {
		d.Unknown = clampString(fmt.Sprintf("the changed paths between %s and %s: %v", from, to, err), maxErrorString)
		return d
	}
	d.Paths, d.Total = boundPaths(paths)
	d.Command = fmt.Sprintf("git diff --name-only %s %s", from, to)
	return d
}

// unbornDifference reports changed-path evidence that cannot exist because the
// observed HEAD has no commits to compare against the retained baseline.
func unbornDifference(label, from string) pathDifference {
	return pathDifference{
		Label:   label,
		From:    from,
		To:      "unborn HEAD",
		Unknown: "the changed paths since the retained baseline: HEAD is unborn and no commit exists to compare against",
	}
}

// diffLines splits one git name-only listing into its non-empty paths.
func diffLines(out string) []string {
	var paths []string
	for _, p := range strings.Split(out, "\n") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// boundPaths clamps a changed-path listing to the guard bound and keeps the
// complete count, so the document states what it omitted.
func boundPaths(paths []string) ([]string, int) {
	total := len(paths)
	if total > maxGuardChangedPaths {
		paths = paths[:maxGuardChangedPaths]
	}
	return paths, total
}

// retainedTaskIDs lists the task identities whose attempts the runtime
// retains, bounded for the error document, with the complete count.
func retainedTaskIDs(s *State) ([]string, int) {
	if s == nil {
		return nil, 0
	}
	var ids []string
	seen := map[string]bool{}
	for _, a := range s.Attempts {
		if a.Phase == "done" || a.Phase == "blocked" || a.Phase == "cancelled" {
			continue
		}
		if a.Task.ID == "" || seen[a.Task.ID] {
			continue
		}
		seen[a.Task.ID] = true
		ids = append(ids, a.Task.ID)
	}
	total := len(ids)
	if total > maxGuardTasks {
		ids = ids[:maxGuardTasks]
	}
	return ids, total
}

// retain finishes one guard error with the concrete differences AX-015
// requires: the resolved project, task-file, and runtime paths, the retained
// task identities, and the bounded changed-path evidence the caller collected.
// The guard still refuses; nothing is accepted, discarded, or overwritten
// here.
func (e *Engine) retain(err *retentionError, differences ...pathDifference) *retentionError {
	err.release = e.Dir
	err.todo = e.Config.Todo
	err.root = e.Config.Root
	err.changed = append(err.changed, differences...)
	if err.expected == nil {
		err.expected = map[string]any{}
	}
	if err.observed == nil {
		err.observed = map[string]any{}
	}
	ids, total := retainedTaskIDs(e.State)
	list := []string{}
	for _, id := range ids {
		list = append(list, id)
	}
	err.observed["retained_tasks"] = list
	if total > len(list) {
		err.observed["retained_task_count"] = total
	}
	return err
}

// backlogDifference reports what a backlog guard actually compared between the
// saved backlog and the bytes on disk: which task contracts differ, which
// tasks appeared or disappeared, which tasks changed data their contract hash
// does not cover, whether non-task content differs, and whether the parsed
// content is identical so only the bytes changed. A parse failure keeps its
// reason instead of an invented comparison.
type backlogDifference struct {
	Changed []string // task contracts that differ
	Added   []string
	Removed []string
	Data    []string // tasks whose non-contract data differs (status, files, blockers, timestamps)
	Other   bool
	Equal   bool
	Unknown string
}

// compareBacklog parses the observed task-file bytes and diffs them against
// the saved backlog by task contract and by task data. The parse is strict:
// duplicate keys and ambiguous input are reported, never normalized. The
// comparison is evidence only: it never decides whether the edit is accepted
// (AX-018 owns that rule).
func compareBacklog(saved Backlog, observed []byte) backlogDifference {
	parsed, err := parseBacklogStrict(observed)
	if err != nil {
		return backlogDifference{Unknown: "the parsed task-file comparison: " + err.Error()}
	}
	savedContracts := map[string]string{}
	for _, t := range saved.Tasks {
		savedContracts[t.ID] = t.Contract()
	}
	observedContracts := map[string]string{}
	observedTasks := map[string]Task{}
	for _, t := range parsed.Tasks {
		observedContracts[t.ID] = t.Contract()
		observedTasks[t.ID] = t
	}
	savedTasks := map[string]Task{}
	for _, t := range saved.Tasks {
		savedTasks[t.ID] = t
	}
	var d backlogDifference
	for _, t := range saved.Tasks {
		contract, ok := observedContracts[t.ID]
		switch {
		case !ok:
			d.Removed = append(d.Removed, t.ID)
		case contract != savedContracts[t.ID]:
			d.Changed = append(d.Changed, t.ID)
		case hashJSON(observedTasks[t.ID]) != hashJSON(savedTasks[t.ID]):
			// The contract hash covers acceptance criteria; status, files,
			// blockers, timestamps, and finding tags sit outside it. An edit
			// there changes task data without changing the contract.
			d.Data = append(d.Data, t.ID)
		}
	}
	for _, t := range parsed.Tasks {
		if _, ok := savedContracts[t.ID]; !ok {
			d.Added = append(d.Added, t.ID)
		}
	}
	withoutTasks := func(b Backlog) Backlog {
		c := clone(b)
		c.Tasks = nil
		c.Project = canonicalRawJSON(b.Project)
		return c
	}
	d.Other = hashJSON(withoutTasks(saved)) != hashJSON(withoutTasks(parsed))
	// Task order is data, not formatting: the fingerprint that decides
	// equivalence (AX-018) hashes the task slice in order, so a reordered
	// list differs even when every per-task record matches.
	ordered := len(saved.Tasks) == len(parsed.Tasks)
	for i := range saved.Tasks {
		if !ordered {
			break
		}
		ordered = saved.Tasks[i].ID == parsed.Tasks[i].ID
	}
	d.Equal = ordered && len(d.Changed) == 0 && len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Data) == 0 && !d.Other
	return d
}

// apply writes the backlog comparison into the error document fields. The
// empty lists stay explicit so a caller distinguishes "no task contract
// changed" from "the comparison did not run".
func (d *backlogDifference) apply(p *PublicError) {
	if d.Unknown != "" {
		p.Observed["task_contract_changes"] = "unknown"
		p.UnknownEvidence = append(p.UnknownEvidence, clampString(d.Unknown, maxErrorString))
		return
	}
	if d.Equal {
		p.Observed["parsed_backlog"] = "identical to the saved backlog; only the task-file bytes changed"
	} else {
		p.Observed["parsed_backlog"] = "differs from the saved backlog"
	}
	bound := func(ids []string) []string {
		if len(ids) > maxGuardTasks {
			return ids[:maxGuardTasks]
		}
		if ids == nil {
			return []string{}
		}
		return ids
	}
	p.Observed["task_contract_changes"] = bound(d.Changed)
	p.Observed["task_contract_additions"] = bound(d.Added)
	p.Observed["task_contract_removals"] = bound(d.Removed)
	if len(d.Data) > 0 {
		p.Observed["task_data_changes"] = bound(d.Data)
	}
	if d.Other {
		p.Observed["non_task_backlog_change"] = true
	}
	if len(d.Changed) > 0 {
		first := d.Changed[0]
		p.Evidence = append(p.Evidence, ErrorEvidence{
			Kind:      "file",
			Reference: "the saved task contract for " + first,
			Command:   "looper inspect " + first + " --json",
		})
	}
}

// branchLabel shortens one symbolic branch reference for guard messages.
func branchLabel(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}
