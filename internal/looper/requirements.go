package looper

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// importProposal is the review agent's structured analysis result. Only the
// controller writes the backlog or persistent state; the agent proposes.
type importProposal struct {
	Additions []Task       `json:"additions"`
	Covered   []CoveredRef `json:"covered"`
	Conflicts []string     `json:"conflicts"`
}

// bootstrapStaging is the recoverable record for fresh-project imports: a
// crash between artifact capture and state initialization must not cause a
// second import.
type bootstrapStaging struct {
	RequestID string   `json:"request_id"`
	Digests   []string `json:"digests"`
	Artifacts []string `json:"artifacts"`
	Paths     []string `json:"paths"`
	Enqueued  string   `json:"enqueued"`
}

func stagingPath(dir, requestID string) string {
	return filepath.Join(importsDir(dir), requestID, "staging.json")
}

// bootstrapStartupPlans captures explicitly supplied plans for a fresh
// project and writes the staging record the state will adopt.
func (e *Engine) bootstrapStartupPlans(requestID string) ([]capturedPlan, error) {
	plans, err := capturePlans(e.Dir, requestID, e.Config.Plans)
	if err != nil {
		return nil, err
	}
	staging := bootstrapStaging{RequestID: requestID, Enqueued: time.Now().UTC().Format(time.RFC3339)}
	for _, p := range plans {
		staging.Digests = append(staging.Digests, p.Digest)
		staging.Artifacts = append(staging.Artifacts, p.Artifact)
		staging.Paths = append(staging.Paths, p.Path)
	}
	if err = writeJSON(stagingPath(e.Dir, requestID), staging); err != nil {
		return nil, err
	}
	return plans, nil
}

// adoptStagedImports marks staged bootstrap imports as applied when the
// initial state is created, so a restart never imports them twice.
func (e *Engine) adoptStagedImports() {
	entries, err := os.ReadDir(importsDir(e.Dir))
	if err != nil {
		return
	}
	for _, entry := range entries {
		var staging bootstrapStaging
		if readJSON(stagingPath(e.Dir, entry.Name()), &staging) != nil {
			continue
		}
		if e.importReceipt(staging.RequestID) != nil {
			continue
		}
		e.State.Imports = append(e.State.Imports, &ImportRecord{
			RequestID: staging.RequestID, Digests: staging.Digests,
			Artifacts: staging.Artifacts, SourcePaths: staging.Paths,
			Status:   ImportApplied,
			Enqueued: staging.Enqueued,
			Applied:  time.Now().UTC().Format(time.RFC3339Nano),
			Covered:  []CoveredRef{{Requirement: "initial backlog created from these plans"}},
		})
		os.Remove(stagingPath(e.Dir, entry.Name()))
	}
}

// startupImportPlans captures invocation plans for an existing project and
// queues them through the durable receipt path.
func (e *Engine) startupImportPlans() {
	if len(e.Config.Plans) == 0 {
		return
	}
	if e.State == nil {
		return
	}
	requestID := identifier("import")
	plans, err := capturePlans(e.Dir, requestID, e.Config.Plans)
	if err != nil {
		e.event("import-error", err.Error())
		return
	}
	if prior := e.matchingImport(plans, ImportApplied, ImportQueued, ImportWaiting, ImportPlanning); prior != nil {
		e.event("import", fmt.Sprintf("Plans already recorded by import %s (%s).", prior.RequestID, prior.Status))
		return
	}
	record := &ImportRecord{RequestID: requestID, Status: ImportQueued, Enqueued: time.Now().UTC().Format(time.RFC3339Nano)}
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
	e.event("import", fmt.Sprintf("Queued startup plan import %s.", requestID))
}

// analyzeImport runs the configured review agent in a private inspection
// worktree over the current backlog, prior imported requirements, and the
// new captured text. Analysis must not change code.
func (e *Engine) analyzeImport(ctx context.Context, plans []capturedPlan) (*importProposal, string, error) {
	name := identifier("import-analysis")
	work, err := e.Repo.Workspace(name, e.State.Tip, e.State.Backlog)
	if err != nil {
		return nil, "", err
	}
	defer e.Repo.Remove(work)
	op := e.capture()
	backlog, _ := json.MarshalIndent(e.State.Backlog, "", "  ")
	var newTexts []string
	for _, p := range plans {
		newTexts = append(newTexts, p.Text)
	}
	prompt := fmt.Sprintf(`Analyze additional requirements for Looper.
Project goal: %s

Current accepted backlog (revision %s):
%s

Previously imported requirements:%s

New requirements to import:
%s

Compare each new requirement with existing tasks and implemented behavior.
Already covered requirements must reference the covering task identifier and evidence.
Changed requirements become follow-up work; they never reopen or rewrite completed tasks.
Reject conflicting requirements that would silently replace existing scope.
Do not regenerate the backlog and do not modify any files.
Return one JSON object: {"additions":[{"id":"P1","title":"outcome","priority":1,"status":"todo","details":"acceptance criteria","depends_on":["P1 or existing task ids"]}],"covered":[{"requirement":"...","task_id":"T003","evidence":"..."}],"conflicts":["explanation"]}.
New tasks need concrete acceptance criteria; execution planning adds executable verification.
Use temporary identifiers P1..Pn for additions; depends_on may reference them or existing task ids.`,
		e.State.Backlog.Goal, e.State.Tip, backlog, orNone(e.importedScope()), strings.Join(newTexts, "\n\n---\n\n"))
	e.event("import", "Analyzing imported requirements with the review agent...")
	dir := filepath.Join(e.Dir, name)
	raw, err := op.agent.Run(ctx, op.config.ReviewAgent, work, dir, prompt, false)
	if err != nil {
		return nil, "", err
	}
	var proposal importProposal
	if err = json.Unmarshal(raw, &proposal); err != nil {
		return nil, "", fmt.Errorf("the analysis returned invalid JSON: %w", err)
	}
	for i := range proposal.Additions {
		t := proposal.Additions[i]
		if t.Status != "todo" {
			t.Status = "todo"
		}
		proposal.Additions[i] = t
	}
	after, err := e.Repo.Snapshot(work)
	if err != nil {
		return nil, "", err
	}
	if after != e.State.Published {
		return nil, "", fmt.Errorf("the analysis changed code; its result was not applied")
	}
	return &proposal, dir, nil
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return " none"
	}
	return "\n" + s
}
