package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// gatedAgent holds every invocation until a release file exists, so a test
// can observe controller state while the analysis agent is still running.
func gatedAgent(t *testing.T, release string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	script := `#!/bin/bash
out=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$a"; fi
  prev="$a"
done
while [ ! -f "$LOOPER_TEST_GATE" ]; do sleep 0.02; done
p="$(cat)"
if [ -n "$out" ]; then printf '%s' "$LOOPER_FAKE_RESULT" > "$out"; fi
printf '{"structured_output":%s}\n' "$LOOPER_FAKE_RESULT"
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_TEST_GATE", release)
	return bin
}

func importViewByRequest(v *ui.ViewSnapshot, id string) *ui.ImportView {
	for i := range v.Imports {
		if v.Imports[i].RequestID == id {
			return &v.Imports[i]
		}
	}
	return nil
}

func waitForViewImport(t *testing.T, dir, id string) *ui.ImportView {
	t.Helper()
	deadline := time.Now().Add(testBudget(5 * time.Second))
	for time.Now().Before(deadline) {
		if v := LoadView(dir); v != nil {
			if imp := importViewByRequest(v, id); imp != nil {
				return imp
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no published import record for %s", id)
	return nil
}

// The published snapshot carries each import's structured state, added task
// identifiers, coverage, and failure detail. A fresh observer reads the same
// records without parsing events, so a reconnecting dashboard retains access.
func TestPublishedViewCarriesImportRecords(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	applied, err := submitPlanImport(e.Dir, e.controller.Session, "", []string{planFile(t, "Add telemetry.")})
	if err != nil {
		t.Fatal(err)
	}
	e.processPlanImports(context.Background())
	if e.err != nil {
		t.Fatal(e.err)
	}
	// A second submission uses an agent that proposes a conflict.
	conflict := `{"additions":[],"covered":[],"conflicts":["unsupported requirement"]}`
	bin, _ := fakeAgent(t, conflict)
	e.Config.CodexBin, e.Config.ClaudeBin = bin, bin
	e.Agent = Agent{e.Config}
	failed, err := submitPlanImport(e.Dir, e.controller.Session, "", []string{planFile(t, "Replace storage.")})
	if err != nil {
		t.Fatal(err)
	}
	e.processPlanImports(context.Background())
	if e.err != nil {
		t.Fatal(e.err)
	}
	e.publishView(false)
	v := LoadView(e.Dir)
	if v == nil {
		t.Fatal("no published view")
	}
	got := importViewByRequest(v, applied)
	if got == nil || got.Status != ImportApplied {
		t.Fatalf("applied import missing from view: %+v", v.Imports)
	}
	if len(got.AddedTasks) != 1 || got.AddedTasks[0] != "T002" {
		t.Fatalf("added task ids missing: %+v", got)
	}
	if len(got.Covered) != 1 || got.Covered[0].TaskID != "T001" || got.Covered[0].Evidence == "" {
		t.Fatalf("coverage missing: %+v", got)
	}
	if len(got.Plans) != 1 || !strings.HasSuffix(got.Plans[0], "plan.md") {
		t.Fatalf("plan paths missing: %+v", got)
	}
	bad := importViewByRequest(v, failed)
	if bad == nil || bad.Status != ImportFailed || !strings.Contains(bad.Error, "conflicts") || len(bad.Conflicts) != 1 {
		t.Fatalf("failure detail missing: %+v", bad)
	}
	// A fresh observer, as a reconnected attached dashboard uses, sees the
	// same records through the published snapshot.
	observer, err := openObserver(e.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	attached := observer.view(e.Dir, e.controller.Session)
	if attached == nil {
		t.Fatal("observer lost the view")
	}
	if imp := importViewByRequest(attached, applied); imp == nil || imp.Status != ImportApplied {
		t.Fatalf("observer missing import record: %+v", attached.Imports)
	}
}

// A queued receipt that has not been processed yet publishes as queued, so
// the dashboard reports the accepted request before analysis starts.
func TestQueuedImportPublishesBeforeProcessing(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	id, err := submitPlanImport(e.Dir, e.controller.Session, "", []string{planFile(t, "Add telemetry.")})
	if err != nil {
		t.Fatal(err)
	}
	e.claimPlanImports()
	if e.err != nil {
		t.Fatal(e.err)
	}
	e.publishView(false)
	v := LoadView(e.Dir)
	if imp := importViewByRequest(v, id); imp == nil || imp.Status != ImportQueued {
		t.Fatalf("queued receipt not published: %+v", v.Imports)
	}
}

// The planning transition publishes while the analysis agent still runs, so
// dashboards show the planning state instead of a stale queued status.
func TestImportPlanningStatePublishesDuringAnalysis(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	bin := gatedAgent(t, release)
	t.Setenv("LOOPER_FAKE_RESULT", additionProposal)
	c := repoConfig(t, "run")
	c.CodexBin, c.ClaudeBin = bin, bin
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{
		{ID: "T001", Title: "existing", Priority: 1, Status: "done", Tags: []string{"project-done"}},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	if err = e.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	id, err := submitPlanImport(e.Dir, e.controller.Session, "", []string{planFile(t, "Add telemetry.")})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 1)
	go func() { e.processPlanImports(context.Background()); done <- struct{}{} }()
	imp := waitForViewImport(t, e.Dir, id)
	if imp.Status != ImportPlanning {
		t.Fatalf("published status %q during analysis, want planning", imp.Status)
	}
	select {
	case <-done:
		t.Fatal("analysis finished before the planning state was observed")
	default:
	}
	if err = os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(testBudget(10 * time.Second)):
		t.Fatal("analysis did not finish after release")
	}
	if e.err != nil {
		t.Fatal(e.err)
	}
	e.publishView(false)
	imp = waitForViewImport(t, e.Dir, id)
	if imp.Status != ImportApplied || len(imp.AddedTasks) != 1 {
		t.Fatalf("final result missing from view: %+v", imp)
	}
}

// The attached submission path must return the request identifier and queue
// a durable request for the controller the viewer currently observes.
func TestAttachmentAddPlanQueuesAndPublishesRequest(t *testing.T) {
	e := protocolEngine(t)
	a := &dashboardAttachment{observer: &observer{store: e.Store}, dir: e.Dir, session: "run-1"}
	publish := func() {
		t.Helper()
		if err := writeJSON(controllerPath(e.Dir), &ControllerInfo{Version: ControllerVersion, Session: "run-1", PID: os.Getpid()}); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(viewPath(e.Dir), &ui.ViewSnapshot{Version: ui.ViewVersion, Session: "run-1"}); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	if v := a.poll(); v == nil || v.Session != "run-1" {
		t.Fatal("attachment did not bind the controller")
	}
	id, err := a.addPlan(planFile(t, "Add an attached requirement."))
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("attached submission discarded the request identifier")
	}
	raw, err := os.ReadFile(filepath.Join(requestDir(e.Dir), id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var req planImportRequest
	if err = json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if req.ID != id || req.Type != RequestTypePlanImport || req.Session != "run-1" || len(req.Paths) != 1 || !filepath.IsAbs(req.Paths[0]) {
		t.Fatalf("queued request incomplete: %+v", req)
	}
	// The controller publishes the receipt; the attached viewer follows the
	// import through structured state after reconnecting.
	published := &ui.ViewSnapshot{Version: ui.ViewVersion, Session: "run-1",
		Imports: []ui.ImportView{{RequestID: id, Status: ui.ImportQueued, Plans: req.Paths}}}
	if err = writeJSON(viewPath(e.Dir), published); err != nil {
		t.Fatal(err)
	}
	v := a.poll()
	if v == nil {
		t.Fatal("attachment lost the republished view")
	}
	if imp := importViewByRequest(v, id); imp == nil || imp.Status != ui.ImportQueued {
		t.Fatalf("attached view missing queued import: %+v", v.Imports)
	}
	// A disconnected controller refuses new submissions.
	if err = writeJSON(controllerPath(e.Dir), &ControllerInfo{Version: ControllerVersion, Session: "gone", PID: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.addPlan(planFile(t, "Orphaned requirement.")); err == nil {
		t.Fatal("submission accepted a disconnected controller")
	}
}

// gatedActivityAgent behaves like gatedAgent and first writes one readable
// agent action, so a test can observe streamed activity evidence while the
// analysis agent is still running. It records its invocation arguments.
func gatedActivityAgent(t *testing.T, release, argsPath string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	script := `#!/bin/bash
out=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$a"; fi
  prev="$a"
done
printf '%s\n' "$@" > "$LOOPER_TEST_ARGS"
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"to-do.json"}}]}}'
while [ ! -f "$LOOPER_TEST_GATE" ]; do sleep 0.02; done
p="$(cat)"
if [ -n "$out" ]; then printf '%s' "$LOOPER_FAKE_RESULT" > "$out"; fi
printf '{"structured_output":%s}\n' "$LOOPER_FAKE_RESULT"
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_TEST_GATE", release)
	t.Setenv("LOOPER_TEST_ARGS", argsPath)
	return bin
}

// waitForActivityView blocks until the published view satisfies want, or fails
// the test. Tests use it to observe controller state while an agent still runs.
func waitForActivityView(t *testing.T, dir string, want func(*ui.ViewSnapshot) bool) *ui.ViewSnapshot {
	t.Helper()
	deadline := time.Now().Add(testBudget(5 * time.Second))
	for time.Now().Before(deadline) {
		if v := LoadView(dir); v != nil && want(v) {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("activity evidence was not published")
	return nil
}

// A long import analysis publishes the same activity evidence as ordinary
// planning: activity state, agent identity, stream path, last-output age,
// and readable agent actions, plus controller heartbeats, while the analysis
// agent is still running. The agent invocation keeps its normal review
// contract: no planning schema and no planning tool restriction.
func TestImportAnalysisPublishesActivityEvidence(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	argsPath := filepath.Join(t.TempDir(), "args.txt")
	bin := gatedActivityAgent(t, release, argsPath)
	t.Setenv("LOOPER_FAKE_RESULT", additionProposal)
	c := repoConfig(t, "run")
	c.CodexBin, c.ClaudeBin, c.ReviewAgent = bin, bin, "claude"
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{
		{ID: "T001", Title: "existing", Priority: 1, Status: "done", Tags: []string{"project-done"}},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	if err = e.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = submitPlanImport(e.Dir, e.controller.Session, "", []string{planFile(t, "Add telemetry.")}); err != nil {
		t.Fatal(err)
	}
	// One heartbeat per tick keeps the controller event observable in a
	// bounded test; production keeps the 30-second planner cadence.
	e.importHeartbeat = time.Second
	done := make(chan struct{}, 1)
	go func() { e.processPlanImports(context.Background()); done <- struct{}{} }()
	v := waitForActivityView(t, e.Dir, func(v *ui.ViewSnapshot) bool {
		return v.State == "planning" && v.ActivityAgent == "claude" && strings.Contains(v.ActivityLog, "import-analysis-")
	})
	v = waitForActivityView(t, e.Dir, func(v *ui.ViewSnapshot) bool {
		return v.ActivityDetail == "Read to-do.json" && v.ActivityUpdated != ""
	})
	select {
	case <-done:
		t.Fatal("analysis finished before activity evidence was observed")
	default:
	}
	log, err := plannerLog(e.Dir, v.ActivityLog)
	if err != nil || !strings.Contains(log, "Read to-do.json") {
		t.Fatalf("dashboard cannot open the analysis stream: %q %v", log, err)
	}
	if err = os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(testBudget(10 * time.Second)):
		t.Fatal("analysis did not finish after release")
	}
	if e.err != nil {
		t.Fatal(e.err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(args, []byte("--tools")) || bytes.Contains(args, []byte("--json-schema")) {
		t.Fatalf("import analysis inherited the planning contract: %s", args)
	}
	after := LoadView(e.Dir)
	if after == nil || after.State != "" || after.ActivityLog != "" || after.ActivityAgent != "" || after.ActivityDetail != "" || after.ActivityUpdated != "" {
		t.Fatalf("import analysis left stale activity: %+v", after)
	}
	events, err := e.Store.RecentEvents(-1, 100)
	if err != nil {
		t.Fatal(err)
	}
	heartbeats, actions := 0, 0
	for _, event := range events {
		if event.Kind == "import" && strings.Contains(event.Message, "Controller waiting for import analysis") {
			heartbeats++
		}
		if event.Kind == "import-activity" && strings.Contains(event.Message, "Read to-do.json") {
			actions++
		}
	}
	if heartbeats == 0 {
		t.Fatal("import analysis emitted no controller heartbeat")
	}
	if actions == 0 {
		t.Fatal("agent stream actions were not journaled")
	}
}

// The dashboard log reader accepts import-analysis streams and still rejects
// directories that hold captured plans rather than agent output.
func TestDashboardReadsImportAnalysisLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "import-analysis-0123456", "events-00.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"plan.md"}}]}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	text, err := plannerLog(dir, path)
	if err != nil || !strings.Contains(text, "Read plan.md") {
		t.Fatalf("import analysis log: %q %v", text, err)
	}
	if _, err = plannerLog(dir, filepath.Join(dir, "imports", "events-00.jsonl")); err == nil {
		t.Fatal("accepted a captured-plan directory as an agent stream")
	}
}
