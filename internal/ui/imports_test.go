package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Every receipt state must render with its plan-required label, and results
// must show task identifiers, coverage, and failure detail.
func TestBoardShowsImportStatesAndResults(t *testing.T) {
	snap := &ViewSnapshot{Imports: []ImportView{
		{RequestID: "import-queued1", Status: ImportQueued, Plans: []string{"docs/A.md"}},
		{RequestID: "import-waiting1", Status: ImportWaiting, Plans: []string{"docs/B.md"}},
		{RequestID: "import-planning1", Status: ImportPlanning, Plans: []string{"docs/C.md"}},
		{RequestID: "import-applied1", Status: ImportApplied, Plans: []string{"docs/UI-PLAN.md", "docs/X.md"},
			AddedTasks:  []string{"T101", "T102"},
			Covered:     []CoveredView{{Requirement: "telemetry", TaskID: "T014", Evidence: "already implemented"}},
			Adjustments: []string{"schedule follow-up after T001"},
			Gates:       []ImportGateView{{TaskID: "T015", DependsOn: []string{"T101"}}}},
		{RequestID: "import-failed1", Status: ImportFailed, Plans: []string{"docs/BAD.md"},
			Error: "the plan conflicts with existing scope: unsupported requirement", Conflicts: []string{"unsupported requirement"}},
	}}
	m := model{width: 90, height: 60, snap: snap}
	content := m.boardContent()
	for _, want := range []string{
		"Plan imports · 5 results",
		"import-queued1", "Queued",
		"import-waiting1", "Waiting for active work",
		"import-planning1", "Planning",
		"import-applied1", "Applied", "docs/UI-PLAN.md, docs/X.md",
		"Added T101, T102",
		"Covered: telemetry -> T014 · already implemented",
		"Adjustment: schedule follow-up after T001",
		"Gate: T015 depends on T101",
		"import-failed1", "Failed",
		"the plan conflicts with existing scope: unsupported requirement",
		"Conflict: unsupported requirement",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("board missing %q:\n%s", want, content)
		}
	}
	// An applied import with no additions still reports its coverage.
	coverageOnly := &ViewSnapshot{Imports: []ImportView{
		{RequestID: "import-covered1", Status: ImportApplied, Plans: []string{"docs/SAME.md"},
			Covered: []CoveredView{{Requirement: "known requirement", TaskID: "T002", Evidence: "already scheduled"}}},
	}}
	m.snap = coverageOnly
	content = m.boardContent()
	for _, want := range []string{"import-covered1", "No new tasks", "Covered: known requirement -> T002 · already scheduled"} {
		if !strings.Contains(content, want) {
			t.Fatalf("coverage-only result missing %q:\n%s", want, content)
		}
	}
	// State colors distinguish outcomes even without the text label.
	_, queued := importStatusLabel(ImportQueued)
	_, planning := importStatusLabel(ImportPlanning)
	_, applied := importStatusLabel(ImportApplied)
	_, failed := importStatusLabel(ImportFailed)
	if applied.GetForeground() != lipgloss.Color("10") || failed.GetForeground() != lipgloss.Color("9") ||
		planning.GetForeground() != lipgloss.Color("12") || queued.GetForeground() != lipgloss.Color("7") {
		t.Fatal("import status colors do not distinguish states")
	}
}

// Pending imports always show; terminal history is bounded like the event
// tail so a long project does not flood the board.
func TestImportBoardBoundsTerminalHistory(t *testing.T) {
	all := make([]ImportView, 0, 8)
	for i := 0; i < 6; i++ {
		all = append(all, ImportView{RequestID: strings.Repeat("a", i) + "import-old" + strings.Repeat("a", 6-i), Status: ImportApplied})
	}
	all = append(all,
		ImportView{RequestID: "import-pending1", Status: ImportQueued},
		ImportView{RequestID: "import-newest", Status: ImportFailed, Error: "boom"})
	kept := visibleImports(all)
	if len(kept) != 4 {
		t.Fatalf("kept %d imports, want 4 (1 pending + 3 newest terminal): %+v", len(kept), kept)
	}
	seen := map[string]bool{}
	for _, imp := range kept {
		seen[imp.RequestID] = true
	}
	if !seen["import-pending1"] || !seen["import-newest"] {
		t.Fatalf("pending or newest result dropped: %+v", kept)
	}
	if seen["import-oldaaaaaa"] {
		t.Fatalf("oldest terminal result kept: %+v", kept)
	}
	for i := 1; i < len(kept); i++ {
		before, after := -1, -1
		for pos, imp := range all {
			if imp.RequestID == kept[i-1].RequestID {
				before = pos
			}
			if imp.RequestID == kept[i].RequestID {
				after = pos
			}
		}
		if before > after {
			t.Fatalf("submission order not preserved: %+v", kept)
		}
	}
}

// The submission notice must name the request identifier so the board entry
// is identifiable, and failures must reach the message area.
func TestAddPlanNoticeNamesRequest(t *testing.T) {
	m := model{width: 80, height: 24, ctrl: Controls{AddPlan: func(path string) (string, error) {
		if path != "docs/plan.md" {
			t.Fatalf("unexpected path %q", path)
		}
		return "import-abc123", nil
	}}}
	m.dialog = "add-plan"
	for _, key := range "docs/plan.md" {
		next, _ := m.keys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		m = next.(model)
	}
	next, _ := m.keys(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if m.dialog != "" || m.err != "" {
		t.Fatalf("dialog=%q err=%q", m.dialog, m.err)
	}
	if !strings.Contains(m.message, "import-abc123") || !strings.Contains(m.message, "Plan imports") {
		t.Fatalf("notice does not name the request: %q", m.message)
	}
	m2 := model{width: 80, height: 24, ctrl: Controls{AddPlan: func(string) (string, error) {
		return "", errStub{}
	}}}
	m2.dialog = "add-plan"
	next, _ = m2.keys(tea.KeyMsg{Type: tea.KeyEnter})
	m2 = next.(model)
	if m2.err == "" || m2.message != "" {
		t.Fatalf("submission error hidden: err=%q message=%q", m2.err, m2.message)
	}
}

type errStub struct{}

func (errStub) Error() string { return "plan docs/missing.md is empty" }

// A pending import explains held dispatch; active work keeps priority.
func TestPendingImportExplainsHeldDispatch(t *testing.T) {
	pending := &ViewSnapshot{Imports: []ImportView{{RequestID: "import-q", Status: ImportQueued}}}
	if got := activityLabel(pending, false); got != "IMPORTING" {
		t.Fatalf("queued import activity %q, want IMPORTING", got)
	}
	pending.Imports[0].Status = ImportPlanning
	if got := activityLabel(pending, false); got != "IMPORTING" {
		t.Fatalf("planning import activity %q, want IMPORTING", got)
	}
	pending.Imports[0].Status = ImportWaiting
	if got := activityLabel(pending, false); got != "IMPORTING" {
		t.Fatalf("waiting import activity %q, want IMPORTING", got)
	}
	pending.Imports = []ImportView{{RequestID: "import-done", Status: ImportApplied}}
	if got := activityLabel(pending, false); got == "IMPORTING" {
		t.Fatal("applied import still reports importing")
	}
	busy := &ViewSnapshot{Imports: []ImportView{{RequestID: "import-q", Status: ImportWaiting}},
		Attempts: []AttemptView{{ID: "task-1", TaskID: "T001", Phase: "running"}}}
	if got := activityLabel(busy, false); got != "IMPLEMENTING" {
		t.Fatalf("active work displaced by import state: %q", got)
	}
}

// Small terminals keep the imports section readable without overflow.
func TestPhoneBoardFitsImports(t *testing.T) {
	for _, size := range [][2]int{{58, 22}, {40, 18}, {30, 14}} {
		m := model{width: size[0], height: size[1], snap: &ViewSnapshot{Imports: []ImportView{
			{RequestID: "import-failed1", Status: ImportFailed,
				Plans:     []string{"docs/very-long-plan-directory-name/plan.md"},
				Error:     strings.Repeat("the plan conflicts with existing scope and cannot be applied ", 4),
				Conflicts: []string{"unsupported requirement replacement"}},
			{RequestID: "import-applied1", Status: ImportApplied, AddedTasks: []string{"T101", "T102", "T103"}},
		}}}
		view := m.View()
		if lipgloss.Width(view) > size[0] || lipgloss.Height(view) > size[1] {
			t.Fatalf("%v: imports overflow: %s", size, view)
		}
		if !strings.Contains(m.boardContent(), "Plan imports · 2 results") {
			t.Fatalf("%v: imports section missing: %s", size, m.boardContent())
		}
		m.showDetails = true
		view = m.View()
		if lipgloss.Width(view) > size[0] || lipgloss.Height(view) > size[1] {
			t.Fatalf("%v: detailed imports overflow: %s", size, view)
		}
	}
}
