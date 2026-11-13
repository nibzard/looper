package looper

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// docs/AGENT-EXPERIENCE-OWNERSHIP.md makes the accepted backlog authoritative
// for status and promises to be corrected on disagreement. Between 2026-11-05
// and 2026-11-09 five completions broke that promise with no check noticing
// (T115, looper:finding:ownership-map-status-sync). This guard makes the
// promise executable: every status cell in the map is a projection of the
// backlog, so a row that trails the backlog fails the suite with the line to
// fix.

// ownershipMapTaskID matches the backlog's task identifiers (T020, AX-001,
// P2-006): a letter-led name that ends in digits, with an optional dash
// before them. Only task cells are scanned, so identifiers inside prose do
// not matter.
var ownershipMapTaskID = regexp.MustCompile(`\b[A-Z][A-Z0-9]*-?[0-9]+\b`)

// ownershipDashCell matches one cell of the separator row under a header.
var ownershipDashCell = regexp.MustCompile(`^:?-+:?$`)

// pendingIntegrationNote is the map's agreed wording for a row that names a
// landed mechanism while the run's integration has not recorded the
// completion in the backlog yet. The note must go once the backlog catches
// up, so a surviving note is as stale as a trailing status word.
const pendingIntegrationNote = "pending integration"

// ownershipDocPath anchors a repository path at this file's location: other
// tests in this package chdir into temporary projects, so a relative path
// would resolve against whichever directory ran last.
func ownershipDocPath(t *testing.T, parts ...string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test file location is unavailable")
	}
	return filepath.Join(append([]string{filepath.Dir(thisFile), "..", ".."}, parts...)...)
}

// ownershipRow is one data row of a Markdown table in the map.
type ownershipRow struct {
	line  int
	cells []string
}

// ownershipTable is one parsed Markdown table from the map: a header row and
// the data rows below it.
type ownershipTable struct {
	header ownershipRow
	rows   []ownershipRow
}

// ownershipTables splits the map's Markdown tables. A table is a run of
// lines that start with '|'; its first line is the header and the separator
// row of dashes under it is dropped.
func ownershipTables(text string) []ownershipTable {
	var tables []ownershipTable
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		if _, ok := ownershipCells(lines[i]); !ok {
			continue
		}
		var table ownershipTable
		for ; i < len(lines); i++ {
			cells, ok := ownershipCells(lines[i])
			if !ok {
				break
			}
			if len(table.header.cells) == 0 {
				table.header = ownershipRow{line: i + 1, cells: cells}
				continue
			}
			if ownershipSeparator(cells) {
				continue
			}
			table.rows = append(table.rows, ownershipRow{line: i + 1, cells: cells})
		}
		if len(table.header.cells) > 0 {
			tables = append(tables, table)
		}
	}
	return tables
}

// ownershipCells splits one Markdown table line into trimmed cells. It
// reports false for a line that is not a table row.
func ownershipCells(line string) ([]string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "|") {
		return nil, false
	}
	cells := strings.Split(strings.Trim(trimmed, "|"), "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells, true
}

// ownershipSeparator reports whether a row is the dash row under a header.
func ownershipSeparator(cells []string) bool {
	for _, cell := range cells {
		if !ownershipDashCell.MatchString(cell) {
			return false
		}
	}
	return true
}

// ownershipTableLookup returns the table whose header holds the named cell.
func ownershipTableLookup(tables []ownershipTable, headerCell string) *ownershipTable {
	for i := range tables {
		for _, cell := range tables[i].header.cells {
			if cell == headerCell {
				return &tables[i]
			}
		}
	}
	return nil
}

// ownershipCellIndex finds a header cell by name.
func ownershipCellIndex(cells []string, name string) int {
	for i, cell := range cells {
		if cell == name {
			return i
		}
	}
	return -1
}

// ownershipStatusWord is the status a row claims: the first word of its
// status cell, without trailing punctuation.
func ownershipStatusWord(cell string) string {
	fields := strings.Fields(cell)
	if len(fields) == 0 {
		return ""
	}
	return strings.Trim(fields[0], ".,;:")
}

// ownershipMapStatusWord reports whether a lower-cased word is one of the
// five status values docs/to-do.schema.json defines.
func ownershipMapStatusWord(word string) bool {
	switch word {
	case "todo", "doing", "blocked", "done", "cancelled":
		return true
	}
	return false
}

// ownershipLeads reports whether a row's status word is ahead of the
// backlog: a terminal claim the accepted backlog has not recorded yet. That
// window is legitimate — a completion edits the map before the run's
// integration records the backlog — and the pending-integration note is how
// a row says so.
func ownershipLeads(word, status string) bool {
	if word != "done" && word != "cancelled" {
		return false
	}
	return status != word
}

// ownershipRowErrors checks one row against the backlog statuses. The row
// covers every identifier in its task cell that the backlog holds; each one
// must agree with the row's status cell.
func ownershipRowErrors(doc string, row ownershipRow, taskCell, statusCell string, statuses map[string]string) []error {
	known := map[string]bool{}
	for _, id := range ownershipMapTaskID.FindAllString(taskCell, -1) {
		if _, ok := statuses[id]; ok {
			known[id] = true
		}
	}
	if len(known) == 0 {
		// The row names no task the backlog holds — a plan label such as
		// "Readiness pipe", or a task from another plan. There is nothing
		// authoritative to sync against.
		return nil
	}
	var errs []error
	word := strings.ToLower(ownershipStatusWord(statusCell))
	for _, id := range ownershipSortedIDs(map[string]bool(known)) {
		where := fmt.Sprintf("%s:%d (%s)", doc, row.line, id)
		status := strings.ToLower(statuses[id])
		switch {
		case !ownershipMapStatusWord(status):
			errs = append(errs, fmt.Errorf("%s: the backlog holds a status the schema does not define (%q)", where, statuses[id]))
		case word == "":
			errs = append(errs, fmt.Errorf("%s: the status cell is empty; start it with the backlog's status word", where))
		case !ownershipMapStatusWord(word):
			errs = append(errs, fmt.Errorf("%s: the status cell starts with %q, which is not a backlog status word; start it with todo, doing, blocked, done, or cancelled", where, ownershipStatusWord(statusCell)))
		case ownershipLeads(word, status) && !strings.Contains(statusCell, pendingIntegrationNote):
			errs = append(errs, fmt.Errorf("%s: says %s while the backlog still holds the task %s; add the %q note or restore the backlog's word", where, word, status, pendingIntegrationNote))
		case !ownershipLeads(word, status) && strings.Contains(statusCell, pendingIntegrationNote):
			errs = append(errs, fmt.Errorf("%s: the %q note is stale, the backlog already holds the task %s; drop the note", where, pendingIntegrationNote, status))
		case word != status && (status == "done" || status == "cancelled" || status == "blocked"):
			errs = append(errs, fmt.Errorf("%s: trails the accepted backlog, which holds the task %s while the map says %s; the backlog wins, so correct the row", where, status, word))
		case word == "blocked" && (status == "todo" || status == "doing"):
			errs = append(errs, fmt.Errorf("%s: claims a hold the backlog no longer records (it holds the task %s); correct the row", where, status))
		}
	}
	return errs
}

// ownershipSortedIDs lists identifiers in order, so the errors of one run
// read in document order.
func ownershipSortedIDs(ids map[string]bool) []string {
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ownershipSyncErrors returns every way the map disagrees with the accepted
// statuses, in document order.
func ownershipSyncErrors(text string, statuses map[string]string) []error {
	const doc = "docs/AGENT-EXPERIENCE-OWNERSHIP.md"
	var errs []error
	tables := ownershipTables(text)
	overlap := ownershipTableLookup(tables, "Status and gate")
	if overlap == nil {
		return append(errs, errors.New(doc+": the overlap table (header cell \"Status and gate\") is missing"))
	}
	taskMap := ownershipTableLookup(tables, "Reuse and boundaries")
	if taskMap == nil {
		return append(errs, errors.New(doc+": the task map (header cell \"Reuse and boundaries\") is missing"))
	}
	// Overlap table: the first cell names the tasks, the status cell is the
	// last one.
	for _, row := range overlap.rows {
		if len(row.cells) < 2 {
			errs = append(errs, fmt.Errorf("%s:%d: the row has fewer than two cells", doc, row.line))
			continue
		}
		errs = append(errs, ownershipRowErrors(doc, row, row.cells[0], row.cells[len(row.cells)-1], statuses)...)
	}
	// Task map: one task per row, and a dedicated status column whose index
	// the header names.
	statusColumn := ownershipCellIndex(taskMap.header.cells, "Status")
	if statusColumn < 0 {
		return append(errs, fmt.Errorf("%s:%d: the task map has no Status column; add one so its rows can sync with the backlog", doc, taskMap.header.line))
	}
	covered := map[string]bool{}
	for _, row := range taskMap.rows {
		if len(row.cells) <= statusColumn {
			errs = append(errs, fmt.Errorf("%s:%d: the row has no Status cell", doc, row.line))
			continue
		}
		ids := ownershipMapTaskID.FindAllString(row.cells[0], -1)
		if len(ids) != 1 {
			errs = append(errs, fmt.Errorf("%s:%d: the task cell must name exactly one task, found %v", doc, row.line, ids))
			continue
		}
		id := ids[0]
		if _, ok := statuses[id]; !ok {
			errs = append(errs, fmt.Errorf("%s:%d (%s): the accepted backlog holds no such task; remove the row or correct the identifier", doc, row.line, id))
			continue
		}
		covered[id] = true
		errs = append(errs, ownershipRowErrors(doc, row, id, row.cells[statusColumn], statuses)...)
	}
	// The map's rule 1: every task of this plan appears in the task map. The
	// plan's tasks carry the AX- prefix, and a backlog task without a map row
	// is exactly the drift this guard exists for.
	planTasks := map[string]bool{}
	for id := range statuses {
		planTasks[id] = strings.HasPrefix(id, "AX-")
	}
	for _, id := range ownershipSortedIDs(planTasks) {
		if planTasks[id] && !covered[id] {
			errs = append(errs, fmt.Errorf("%s: the backlog holds %s, but the task map has no row for it; rule 1 requires one", doc, id))
		}
	}
	return errs
}

// TestOwnershipMapStatusesMatchBacklog keeps the map's status cells in sync
// with the accepted backlog. The map is tracked content, so a missing map
// fails. The backlog is a control file the assembly seeds per tree and Git
// ignores, so a fresh clone holds none; the check skips there and runs
// wherever a backlog exists — the self-run trees where the map drifted.
func TestOwnershipMapStatusesMatchBacklog(t *testing.T) {
	mapBytes, err := os.ReadFile(ownershipDocPath(t, "docs", "AGENT-EXPERIENCE-OWNERSHIP.md"))
	if err != nil {
		t.Fatal(err)
	}
	backlogPath := ownershipDocPath(t, "to-do.json")
	backlogBytes, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Skipf("no accepted backlog beside the map to sync against: %v", err)
	}
	var backlog Backlog
	if err = json.Unmarshal(backlogBytes, &backlog); err != nil {
		t.Fatalf("%s does not parse: %v", backlogPath, err)
	}
	statuses := map[string]string{}
	for _, task := range backlog.Tasks {
		statuses[task.ID] = task.Status
	}
	for _, err := range ownershipSyncErrors(string(mapBytes), statuses) {
		t.Error(err)
	}
}

// TestOwnershipMapSyncCheckHasTeeth proves the guard fails on the drift it
// exists for: a synthetic map that carries one instance of every defect the
// 2026-11-05 to 2026-11-09 drift produced, and the same map corrected.
func TestOwnershipMapSyncCheckHasTeeth(t *testing.T) {
	drifted := `# Map

| Task | What it owns | Consumers | Status and gate |
| --- | --- | --- | --- |
| T1 | Request queues | AX-9 | Blocked. The backlog still holds T1 with the deferred tag. |
| T2 | Settings sweep | AX-9 | Implemented 2026-11-08: landed. |
| T3 | Import pause | AX-9 | Done 2026-11-08. |
| T4 | Held import | AX-9 | Done 2026-11-08. |
| T5 | Owner commit | AX-9 | Done 2026-11-08, pending integration. |
| T6 | Disputed findings | AX-9 | Todo. |
| Readiness pipe | Detached readiness | AX-9 | Done. |

| Task | Title | Owner | Depends on | Status | Reuse and boundaries |
| --- | --- | --- | --- | --- | --- |
| AX-1 | Receipts | agent | none | Done 2026-11-08 | Reuses T1. |
| AX-2 | Waits | agent | AX-1 | Todo | Finite waits. |
`
	statuses := map[string]string{
		"T1": "done", "T2": "done", "T3": "done", "T4": "doing", "T5": "done",
		"T6":   "blocked",
		"AX-1": "done", "AX-2": "todo", "AX-9": "done",
	}
	var joined []string
	for _, err := range ownershipSyncErrors(drifted, statuses) {
		joined = append(joined, err.Error())
	}
	text := strings.Join(joined, "\n")
	for _, want := range []string{
		// T1: a completed task the map still holds blocked.
		"(T1): trails the accepted backlog",
		// T2: "Implemented" is not a status word.
		"(T2): the status cell starts with",
		// T4: a row ahead of the backlog without the agreed note.
		"(T4): says done while the backlog still holds the task doing",
		// T5: a note that outlived the lag it described.
		"(T5): the \"pending integration\" note is stale",
		// T6: a hold the row never recorded.
		"(T6): trails the accepted backlog, which holds the task blocked while the map says todo",
		// AX-9: a plan task the backlog holds but the map does not cover.
		"the backlog holds AX-9, but the task map has no row for it",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the drifted map produced no error containing %q; the guard lost its teeth:\n%s", want, text)
		}
	}
	// Agreeing rows stay quiet: T3 matches its backlog status, the plan-label
	// row names no backlog task, and both task-map rows are covered.
	for _, quiet := range []string{"(T3)", "(Readiness", "(AX-1)", "(AX-2)"} {
		if strings.Contains(text, quiet) {
			t.Errorf("an agreeing row produced an error:\n%s", text)
		}
	}
	// Corrected, the same map produces no errors: T1 and T2 carry their
	// status, T4 returns to the backlog's word, T5 drops the stale note, T6
	// records the hold, and the task map gains the AX-9 row rule 1 demands.
	agreeing := strings.NewReplacer(
		"| T1 | Request queues | AX-9 | Blocked. The backlog still holds T1 with the deferred tag. |",
		"| T1 | Request queues | AX-9 | Done 2026-11-08. |",
		"| T2 | Settings sweep | AX-9 | Implemented 2026-11-08: landed. |",
		"| T2 | Settings sweep | AX-9 | Done 2026-11-08. |",
		"| T4 | Held import | AX-9 | Done 2026-11-08. |",
		"| T4 | Held import | AX-9 | Doing. |",
		"| T5 | Owner commit | AX-9 | Done 2026-11-08, pending integration. |",
		"| T5 | Owner commit | AX-9 | Done 2026-11-08. |",
		"| T6 | Disputed findings | AX-9 | Todo. |",
		"| T6 | Disputed findings | AX-9 | Blocked. |",
		"| AX-2 | Waits | agent | AX-1 | Todo | Finite waits. |",
		"| AX-2 | Waits | agent | AX-1 | Todo | Finite waits. |\n| AX-9 | Evaluation | agent | AX-1 | Done 2026-11-09 | Fresh agents. |",
	).Replace(drifted)
	if errs := ownershipSyncErrors(agreeing, statuses); len(errs) != 0 {
		t.Errorf("the corrected map produced errors:\n%v", errs)
	}
}
