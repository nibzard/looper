package looper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// maxPlanBytes bounds one import, shared across all submitted files.
const maxPlanBytes = 1 << 20

// capturedPlan is one validated requirements document copied into immutable
// runtime storage. Later edits to the source file cannot change a queued
// import; workers and review read the captured text, never the source path.
type capturedPlan struct {
	Path     string
	Digest   string
	Artifact string
	Text     string
}

func importsDir(dir string) string { return filepath.Join(dir, "imports") }

// validatePlanFiles rejects unreadable files, directories, empty files,
// invalid text, and oversized input before any work starts.
func validatePlanFiles(paths []string) error {
	total := 0
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("cannot read plan %s: %w", path, err)
		}
		if info.IsDir() {
			return fmt.Errorf("plan %s is a directory", path)
		}
		if info.Size() == 0 {
			return fmt.Errorf("plan %s is empty", path)
		}
		total += int(info.Size())
	}
	if total > maxPlanBytes {
		return fmt.Errorf("plans total %d bytes; the combined limit is %d bytes", total, maxPlanBytes)
	}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !utf8.Valid(b) {
			return fmt.Errorf("plan %s is not valid UTF-8 text", path)
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			return fmt.Errorf("plan %s has no content", path)
		}
	}
	return nil
}

// capturePlans copies submitted bytes into runtime artifacts and records the
// original paths and digests. Capture happens before any acknowledgement of
// acceptance.
func capturePlans(dir, requestID string, paths []string) ([]capturedPlan, error) {
	if err := validatePlanFiles(paths); err != nil {
		return nil, err
	}
	base := filepath.Join(importsDir(dir), requestID)
	plans := make([]capturedPlan, 0, len(paths))
	for i, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		artifact := filepath.Join(base, fmt.Sprintf("plan-%02d.md", i+1))
		if err = atomicWrite(artifact, b, 0600); err != nil {
			return nil, err
		}
		plans = append(plans, capturedPlan{Path: path, Digest: digest(b), Artifact: artifact, Text: string(b)})
	}
	return plans, nil
}

// loadCapturedPlans reads artifacts back for queued receipts after restart.
func loadCapturedPlans(record *ImportRecord) ([]capturedPlan, error) {
	plans := make([]capturedPlan, 0, len(record.Artifacts))
	for i, artifact := range record.Artifacts {
		b, err := os.ReadFile(artifact)
		if err != nil {
			return nil, fmt.Errorf("import %s captured plan unavailable: %s: %w", record.RequestID, artifact, err)
		}
		if i < len(record.Digests) && digest(b) != record.Digests[i] {
			return nil, fmt.Errorf("import %s captured plan changed: %s", record.RequestID, artifact)
		}
		path := ""
		if i < len(record.SourcePaths) {
			path = record.SourcePaths[i]
		}
		plans = append(plans, capturedPlan{Path: path, Digest: digest(b), Artifact: artifact, Text: string(b)})
	}
	return plans, nil
}

// planImportRequest is the durable transport for additional requirements.
// It reuses the settings envelope with typed plan-import payloads.
type planImportRequest struct {
	Version   int      `json:"version"`
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Session   string   `json:"session"`
	Paths     []string `json:"paths"`
	CreatedAt string   `json:"created_at,omitempty"`
}

// submitPlanImport writes a request for the active controller and returns the
// request identifier. The request survives controller restarts with its
// captured artifacts.
func submitPlanImport(dir, session string, paths []string) (string, error) {
	if err := validatePlanFiles(paths); err != nil {
		return "", err
	}
	req := planImportRequest{
		Version: SettingsRequestVersion, ID: identifier("import"),
		Type: RequestTypePlanImport, Session: session, Paths: paths,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeJSON(filepath.Join(requestDir(dir), req.ID+".json"), req); err != nil {
		return "", err
	}
	return req.ID, nil
}

// claimPlanImports claims submitted plan-import requests, validates and
// captures them, and persists queued receipts before acknowledging
// acceptance. A queued artifact without that receipt is unaccepted.
func (e *Engine) claimPlanImports() {
	for {
		raw, err := claimNextRequest(e.Dir, RequestTypePlanImport)
		if err != nil {
			e.event("import-error", err.Error())
			return
		}
		if raw == nil {
			return
		}
		e.acceptPlanImport(raw.ID, raw.Paths, raw.Session != e.controller.Session)
		os.Remove(filepath.Join(processingDir(e.Dir), raw.ID+".json"))
	}
}

// acceptPlanImport validates, captures, and receipts one import. Imports for
// an earlier session are rejected with a reload hint, mirroring settings.
func (e *Engine) acceptPlanImport(requestID string, paths []string, staleSession bool) {
	if record := e.importReceipt(requestID); record != nil {
		// Duplicate identifiers replay the recorded result.
		e.writePlanAck(record)
		return
	}
	if staleSession {
		e.ackPlanImport(requestID, AckStale, "the request targets an earlier controller session; resubmit it after reloading state", nil)
		return
	}
	plans, err := capturePlans(e.Dir, requestID, paths)
	if err != nil {
		e.ackPlanImport(requestID, AckError, err.Error(), nil)
		return
	}
	if applied := e.appliedByDigest(plans); applied != nil {
		// Reply under the new request identifier while reusing the applied result.
		e.ackPlanImport(requestID, ImportApplied, "", applied)
		return
	}
	record := &ImportRecord{
		RequestID: requestID, Status: ImportQueued,
		Enqueued: time.Now().UTC().Format(time.RFC3339Nano),
	}
	for _, p := range plans {
		record.Digests = append(record.Digests, p.Digest)
		record.Artifacts = append(record.Artifacts, p.Artifact)
		record.SourcePaths = append(record.SourcePaths, p.Path)
	}
	e.State.Imports = append(e.State.Imports, record)
	if err = e.save(); err != nil {
		e.err = err
		return
	}
	e.event("import", fmt.Sprintf("Queued plan import %s (%d file(s)).", requestID, len(plans)))
	e.writePlanAck(record)
}

// importReceipt finds a receipt by request identifier.
func (e *Engine) importReceipt(requestID string) *ImportRecord {
	for _, r := range e.State.Imports {
		if r.RequestID == requestID {
			return r
		}
	}
	return nil
}

// appliedByDigest matches already-applied content digests so repeated
// imports never add tasks twice.
func (e *Engine) appliedByDigest(plans []capturedPlan) *ImportRecord {
	return e.matchingImport(plans, ImportApplied)
}

func (e *Engine) matchingImport(plans []capturedPlan, statuses ...string) *ImportRecord {
	digests := map[string]bool{}
	for _, p := range plans {
		digests[p.Digest] = true
	}
	for _, r := range e.State.Imports {
		accepted := false
		for _, status := range statuses {
			accepted = accepted || r.Status == status
		}
		if !accepted || len(r.Digests) != len(digests) {
			continue
		}
		match := true
		for _, d := range r.Digests {
			if !digests[d] {
				match = false
			}
		}
		if match {
			return r
		}
	}
	return nil
}

// writePlanAck persists the acknowledgement file for a receipt.
func (e *Engine) writePlanAck(record *ImportRecord) {
	ack := planImportAck{
		Version: SettingsRequestVersion, ID: record.RequestID, Session: e.controller.Session,
		Status: record.Status, AddedTasks: record.AddedTasks, Covered: record.Covered,
		Conflicts: record.Conflicts, Error: record.Error,
	}
	if err := writeJSON(filepath.Join(ackDir(e.Dir), record.RequestID+".json"), ack); err != nil {
		e.err = err
	}
}

func (e *Engine) ackPlanImport(id, status, message string, record *ImportRecord) {
	ack := planImportAck{Version: SettingsRequestVersion, ID: id, Session: e.controller.Session, Status: status}
	if message != "" {
		ack.Error = message
	}
	if record != nil {
		ack.AddedTasks, ack.Covered, ack.Conflicts = record.AddedTasks, record.Covered, record.Conflicts
		if record.Status == ImportApplied {
			ack.Status = ImportApplied
		}
	}
	if err := writeJSON(filepath.Join(ackDir(e.Dir), id+".json"), ack); err != nil {
		e.err = err
	}
	e.event("import", fmt.Sprintf("Plan import %s %s: %s", id, status, message))
}

// planImportAck is the machine-readable request result.
type planImportAck struct {
	Version    int          `json:"version"`
	ID         string       `json:"id"`
	Status     string       `json:"status"`
	Session    string       `json:"controller_session"`
	AddedTasks []string     `json:"added_task_ids,omitempty"`
	Covered    []CoveredRef `json:"coverage,omitempty"`
	Conflicts  []string     `json:"conflicts,omitempty"`
	Error      string       `json:"error,omitempty"`
}

// processPlanImports drives queued receipts at loop boundaries: wait for
// safe conditions, analyze, validate, and apply additively. A stop request
// takes precedence; queued imports remain for the next run. While the task
// file needs correction, new requests are still claimed and queued, but no
// import applies until the input is valid again.
func (e *Engine) processPlanImports(ctx context.Context) {
	e.claimPlanImports()
	if e.err != nil {
		return
	}
	if e.inputPending {
		return
	}
	for _, record := range e.State.Imports {
		if record.Status != ImportQueued && record.Status != ImportWaiting && record.Status != ImportPlanning {
			continue
		}
		if e.drainingNow() {
			e.event("import", "Plan import "+record.RequestID+" stays queued; the controller is draining.")
			continue
		}
		if e.workRetained() {
			if record.Status != ImportWaiting {
				record.Status = ImportWaiting
				if err := e.save(); err != nil {
					e.err = err
					return
				}
			}
			e.event("import", "Plan import "+record.RequestID+" waits for active work to reach a boundary.")
			continue
		}
		if err := e.applyPlanImport(ctx, record); err != nil {
			var bad *invalidBacklogError
			if errors.As(err, &bad) {
				// A corrected edit, not a new submission, unblocks this import.
				e.event("import", "Plan import "+record.RequestID+" waits for a corrected task file.")
				continue
			}
			e.event("import-error", fmt.Sprintf("Plan import %s failed: %v", record.RequestID, err))
			record.Status = ImportFailed
			record.Error = err.Error()
			if err = e.save(); err != nil {
				e.err = err
				return
			}
			e.writePlanAck(record)
			continue
		}
	}
}

func (e *Engine) drainingNow() bool {
	_, err := os.Stat(filepath.Join(e.Dir, "stop"))
	return err == nil
}

// workRetained reports whether unfinished attempts or pending publications
// still own the current contract.
func (e *Engine) workRetained() bool {
	if e.State.Publication != nil {
		return true
	}
	for _, a := range e.State.Attempts {
		switch a.Phase {
		case "done", "blocked":
		default:
			return true
		}
	}
	return false
}

// applyPlanImport refreshes the baseline, analyzes the captured plans, and
// applies additive scope in one journalled transition.
func (e *Engine) applyPlanImport(ctx context.Context, record *ImportRecord) error {
	plans, err := loadCapturedPlans(record)
	if err != nil {
		return err
	}
	if record.Status != ImportPlanning {
		record.Status = ImportPlanning
		if err = e.save(); err != nil {
			return err
		}
		e.writePlanAck(record)
	}
	// Refresh the baseline through the existing checks before analysis.
	if err = e.refreshIdle(); err != nil {
		return err
	}
	// Compare analysis against the refreshed baseline.
	backlogBefore := digestFile(e.Config.Todo)
	tipBefore := e.State.Tip
	proposal, err := e.analyzeImport(ctx, plans)
	if err != nil {
		return err
	}
	if len(proposal.Conflicts) > 0 {
		record.Conflicts = proposal.Conflicts
		return fmt.Errorf("the plan conflicts with existing scope: %s", strings.Join(proposal.Conflicts, "; "))
	}
	if digestFile(e.Config.Todo) != backlogBefore || e.State.Tip != tipBefore {
		return fmt.Errorf("the project changed during analysis; resubmit the import")
	}
	added, covered, err := e.applyProposal(proposal)
	if err != nil {
		return err
	}
	record.Status = ImportApplied
	record.AddedTasks = added
	record.Covered = covered
	record.Applied = time.Now().UTC().Format(time.RFC3339Nano)
	// Scope expansion invalidates execution planning and satisfaction.
	e.State.Plan = nil
	e.State.PlanSignature = ""
	e.State.Finished = false
	e.State.ReviewedTip = ""
	if err = e.save(); err != nil {
		return err
	}
	if err = e.export(); err != nil {
		return err
	}
	e.event("import", fmt.Sprintf("Applied plan import %s: %d task(s) added, %d covered.", record.RequestID, len(added), len(covered)))
	e.writePlanAck(record)
	return nil
}

func digestFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return digest(b)
}

// applyProposal allocates unique task identifiers, resolves proposal-local
// dependencies, and validates the combined backlog before any mutation.
// Already covered requirements reference existing tasks instead of creating
// duplicates.
func (e *Engine) applyProposal(proposal *importProposal) ([]string, []CoveredRef, error) {
	if len(proposal.Additions) == 0 && len(proposal.Covered) == 0 {
		return nil, nil, fmt.Errorf("the analysis proposed neither additions nor coverage")
	}
	next := len(e.State.Backlog.Tasks) + 1
	alloc := func() string {
		for {
			id := fmt.Sprintf("T%03d", next)
			next++
			if e.State.Backlog.Task(id) == nil {
				return id
			}
		}
	}
	local := map[string]string{}
	tasks := make([]Task, 0, len(proposal.Additions))
	for i := range proposal.Additions {
		t := proposal.Additions[i]
		if strings.TrimSpace(t.Title) == "" || strings.TrimSpace(t.Details) == "" {
			return nil, nil, fmt.Errorf("addition %d lacks a title or acceptance criteria", i+1)
		}
		if strings.TrimSpace(t.ID) == "" || local[t.ID] != "" {
			return nil, nil, fmt.Errorf("addition %d has an empty or duplicate identifier", i+1)
		}
		local[t.ID] = alloc()
	}
	for i := range proposal.Additions {
		t := proposal.Additions[i]
		t.ID = local[t.ID]
		t.Status = "todo"
		t.Priority = max(1, min(5, t.Priority))
		deps := make([]string, 0, len(t.DependsOn))
		for _, d := range t.DependsOn {
			if mapped, ok := local[d]; ok {
				deps = append(deps, mapped)
				continue
			}
			if e.State.Backlog.Task(d) == nil {
				return nil, nil, fmt.Errorf("addition %s depends on unknown task %s", t.ID, d)
			}
			deps = append(deps, d)
		}
		t.DependsOn = deps
		tasks = append(tasks, t)
	}
	for _, ref := range proposal.Covered {
		if ref.TaskID != "" && e.State.Backlog.Task(ref.TaskID) == nil {
			return nil, nil, fmt.Errorf("coverage references unknown task %s", ref.TaskID)
		}
	}
	// Validate the combined backlog before mutating anything.
	combined := clone(e.State.Backlog)
	combined.Tasks = append(combined.Tasks, tasks...)
	if err := combined.Validate(); err != nil {
		return nil, nil, fmt.Errorf("combined backlog is invalid: %w", err)
	}
	e.State.Backlog.Tasks = append(e.State.Backlog.Tasks, tasks...)
	e.State.Backlog.Validate()
	added := make([]string, 0, len(tasks))
	for _, t := range tasks {
		added = append(added, t.ID)
	}
	return added, proposal.Covered, nil
}

// appliedContext never drops accepted requirements when evidence is missing.
func (e *Engine) appliedContext() ([]contextDocument, error) {
	var documents []contextDocument
	for _, r := range e.State.Imports {
		if r.Status != ImportApplied {
			continue
		}
		plans, err := loadCapturedPlans(r)
		if err != nil {
			return nil, err
		}
		for _, p := range plans {
			documents = append(documents, contextDocument{Import: r.RequestID, Source: p.Path, Text: []byte(p.Text)})
		}
	}
	return documents, nil
}

// waitForPlanImport polls a request's acknowledgement until it reaches a
// terminal status or the timeout. A timeout reports a pending import.
func waitForPlanImport(dir, id string, timeout time.Duration) (*planImportAck, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var ack planImportAck
		if err := readJSON(filepath.Join(ackDir(dir), id+".json"), &ack); err == nil {
			switch ack.Status {
			case ImportApplied, ImportFailed, AckError, AckStale:
				return &ack, nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return nil, fmt.Errorf("import %s is still pending; watch it with looper tui", id)
}

// sortStrings orders deterministically for output.
func sortStrings(s []string) { sort.Strings(s) }
