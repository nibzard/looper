package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RunRecovery is saved before consultation. An interrupted consultation consumes
// its opportunity, so restart cannot cause an unbounded series of agent calls.
type RunRecovery struct {
	Identity   string   `json:"identity"`
	Outcome    string   `json:"outcome"`
	Tasks      []string `json:"tasks,omitempty"`
	Integrated int      `json:"integrated"`
}

// RecoveryCommands preserves the effective correction without editing a profile.
// A later user override wins over the recorded correction.
type RecoveryCommands struct {
	PreviousSetup  string `json:"previous_setup"`
	PreviousVerify string `json:"previous_verify"`
	BeforeSetup    string `json:"before_setup"`
	BeforeVerify   string `json:"before_verify"`
	Setup          string `json:"setup"`
	Verify         string `json:"verify"`
}

type runRepair struct {
	RemoveSources  []string                `json:"remove_sources"`
	Setup          string                  `json:"setup"`
	Verify         string                  `json:"verify"`
	Reopen         []string                `json:"reopen"`
	DeferredTasks  []string                `json:"deferred_tasks"`
	Administrative []administrativeClosure `json:"administrative_closures,omitempty"`
}

type administrativeClosure struct {
	TaskID string `json:"task_id"`
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// disposableSource requires retained evidence, rather than trusting the agent's
// assertion that an unreadable requirement can be removed.
func (e *Engine) disposableSource(source string) bool {
	if e.Repo.validateBootstrapSources([]string{source}) == nil {
		return false
	}
	clean := filepath.Clean(source)
	prefix := e.Repo.contextPath() + string(filepath.Separator)
	if !strings.HasPrefix(clean, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(clean, prefix), string(filepath.Separator))
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "bootstrap-") || parts[1] != "backlog.json" {
		return false
	}
	var b Backlog
	if readJSON(filepath.Join(e.Dir, parts[0], "context", "backlog.json"), &b) != nil {
		return false
	}
	return b.Validate() == nil && b.Goal == "" && len(b.Project) == 0 && len(b.Sources) == 0 && len(b.Tasks) == 0
}

func (e *Engine) recoveryCommands(p *Plan) {
	r := e.State.RecoveredCommands
	if r == nil {
		return
	}
	if e.Config.Setup == r.BeforeSetup {
		p.Setup = r.Setup
	}
	if e.Config.Verify == r.BeforeVerify {
		p.Verify = r.Verify
	}
}

// recoveryOpportunity only selects proven private context defects or retained
// repeated check failures. External prerequisites alone never call an agent.
func (e *Engine) recoveryOpportunity() (string, string) {
	var evidence []string
	proven := false
	for _, s := range e.State.Backlog.Sources {
		if e.disposableSource(s) {
			proven = true
			evidence = append(evidence, "Disposable private snapshot: "+s)
		}
	}
	for _, a := range e.State.Attempts {
		t := e.State.Backlog.Task(a.Task.ID)
		if t == nil || t.Status != "blocked" || t.OperatorOnly() {
			continue
		}
		if a.Repeats >= 2 && (strings.Contains(a.Feedback, "verification failed") || strings.Contains(a.Feedback, "setup failed")) {
			proven = true
		}
		evidence = append(evidence, a.Task.ID+": "+failureEvidence(strings.Join(t.Blockers, "\n"))+"\nRetained work: "+a.Worktree)
	}
	if !proven {
		return "", ""
	}
	return e.recoveryIdentity(), strings.Join(evidence, "\n")
}

func (e *Engine) recoveryIdentity() string {
	// Contracts exclude status and transient blocker messages. Operation locations
	// are supplied as evidence but never form the durable identity.
	var contracts []string
	for _, t := range e.State.Backlog.Tasks {
		finding := false
		for _, tag := range t.Tags {
			if strings.HasPrefix(tag, findingKeyPrefix) {
				finding = true
			}
		}
		if !finding {
			contracts = append(contracts, t.Contract())
		}
	}
	p := e.effectiveRecoveryPlan()
	sources := append([]string(nil), e.State.Backlog.Sources...)
	for i, source := range sources {
		if e.disposableSource(source) {
			sources[i] = "looper:empty-bootstrap-context"
			p.Setup = strings.ReplaceAll(p.Setup, source, sources[i])
			p.Verify = strings.ReplaceAll(p.Verify, source, sources[i])
		}
	}
	identity := hashJSON(struct {
		Sources       []string
		Contracts     []string
		Setup, Verify string
	}{sources, contracts, p.Setup, p.Verify})
	return identity
}

var executableCheck = regexp.MustCompile(`(?m)(?:^|[\n;&| ]+)(?:cargo|go|npm|pnpm|yarn|make|pytest|python3|bash|sh|\./[^ ]+)\s+[^\n]*(?:check|test|verify|lint|build)`)

func (e *Engine) validateRunRepair(p runRepair) error {
	if len(p.Reopen) == 0 {
		return fmt.Errorf("no retained task selected")
	}
	if e.Config.Setup != "" && p.Setup != e.Config.Setup {
		supported := false
		for _, source := range p.RemoveSources {
			if strings.Contains(e.Config.Setup, source) && e.disposableSource(source) {
				supported = true
			}
		}
		if !supported {
			return fmt.Errorf("setup override has no proven private-source defect")
		}
	}
	if e.Config.Verify != "" && p.Verify != e.Config.Verify {
		preserved := false
		for _, id := range p.DeferredTasks {
			if t := e.State.Backlog.Task(id); t != nil && strings.Contains(t.Details+"\n"+strings.Join(t.Steps, "\n"), e.Config.Verify) {
				preserved = true
			}
		}
		if !preserved {
			return fmt.Errorf("verification override is not preserved in future task requirements")
		}
	}
	if !executableCheck.MatchString(p.Verify) || strings.Contains(p.Verify, "||") || strings.Contains(p.Verify, "exit 0") || strings.Contains(p.Verify, "set +e") {
		return fmt.Errorf("replacement must retain executable checks")
	}
	if len(p.RemoveSources) == 0 && len(p.DeferredTasks) == 0 {
		return fmt.Errorf("no proven source or future-task defect")
	}
	for _, s := range p.RemoveSources {
		found := false
		for _, old := range e.State.Backlog.Sources {
			if old == s {
				found = true
			}
		}
		if !found || !e.disposableSource(s) {
			return fmt.Errorf("source %q is not disposable context", s)
		}
	}
	for _, id := range p.DeferredTasks {
		t := e.State.Backlog.Task(id)
		if t == nil || t.Status == "done" || t.OperatorOnly() || strings.TrimSpace(t.Details) == "" {
			return fmt.Errorf("future check requires an unfinished implementation task: %s", id)
		}
	}
	for _, closure := range p.Administrative {
		t := e.State.Backlog.Task(closure.TaskID)
		removed := false
		for _, source := range p.RemoveSources {
			if source == closure.Source {
				removed = true
			}
		}
		if t == nil || t.Status != "blocked" || !hasTaskTag(*t, "looper:orchestration") || !removed || !strings.Contains(t.Details, closure.Source) || strings.TrimSpace(closure.Reason) == "" {
			return fmt.Errorf("task %s is not a proven obsolete administrative task", closure.TaskID)
		}
		for _, id := range p.Reopen {
			if id == closure.TaskID {
				return fmt.Errorf("task cannot be reopened and closed")
			}
		}
	}
	for _, id := range p.Reopen {
		t := e.State.Backlog.Task(id)
		if t == nil || t.Status != "blocked" || t.OperatorOnly() {
			return fmt.Errorf("task %s cannot be reopened", id)
		}
		retained := false
		for _, a := range e.State.Attempts {
			if a.Task.ID == id && a.Phase == "blocked" && a.Task.Contract() == t.Contract() {
				if _, err := os.Stat(a.Worktree); err == nil {
					retained = true
				}
			}
		}
		if !retained {
			return fmt.Errorf("task %s has no retained implementation", id)
		}
	}
	return nil
}

// recoverRun runs only at a drained controller boundary. All mutations use the
// existing state and export journal. A stale proposal never overwrites edits.
func (e *Engine) recoverRun(ctx context.Context) (bool, error) {
	identity, evidence := e.recoveryOpportunity()
	if e.State.PendingRunRepair != nil {
		identity = e.recoveryIdentity()
	}
	if identity == "" {
		return false, nil
	}
	for _, r := range e.State.Recoveries {
		if r.Identity == identity {
			if e.State.PendingRunRepair != nil {
				e.State.PendingRunRepair = nil
				return false, e.save()
			}
			return false, nil
		}
	}
	index := len(e.State.Recoveries)
	e.State.Recoveries = append(e.State.Recoveries, RunRecovery{Identity: identity, Outcome: "started", Integrated: e.State.Integrated})
	if err := e.save(); err != nil {
		return false, err
	}
	e.event("run-recovery-started", "Consulting the review agent about retained orchestration failures.")
	reject := func(reason string) (bool, error) {
		e.State.Recoveries[index].Outcome = "rejected"
		e.event("run-recovery-rejected", reason)
		return false, e.save()
	}
	baseline := hashJSON(e.State.Backlog)
	exportHash := e.State.ExportHash
	plan := e.effectiveRecoveryPlan()
	var d diagnosis
	var err error
	if pending := e.State.PendingRunRepair; pending != nil {
		e.State.PendingRunRepair = nil
		if pending.Identity != identity {
			return reject("The diagnosed contract changed before application.")
		}
		d = pending.Proposal
	} else {
		d, err = e.consultRunRepair(ctx, evidence)
		if err != nil {
			return reject(err.Error())
		}
	}
	if d.RunRepair == nil {
		return reject("No run repair in the saved proposal.")
	}
	if err = e.validateRunRepair(*d.RunRepair); err != nil {
		return reject(err.Error())
	}
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		return reject(err.Error())
	}
	if digest(data) != exportHash || hashJSON(e.State.Backlog) != baseline {
		return reject("Backlog changed during consultation; discarded stale proposal.")
	}
	current := e.recoveryIdentity()
	if current != identity {
		return reject("Effective contract changed during consultation.")
	}
	p := d.RunRepair
	sources := []string{}
	for _, s := range e.State.Backlog.Sources {
		remove := false
		for _, v := range p.RemoveSources {
			if v == s {
				remove = true
			}
		}
		if !remove {
			sources = append(sources, s)
		}
	}
	e.State.Backlog.Sources = sources
	e.State.RecoveredCommands = &RecoveryCommands{BeforeSetup: e.Config.Setup, BeforeVerify: e.Config.Verify, PreviousSetup: plan.Setup, PreviousVerify: plan.Verify, Setup: p.Setup, Verify: p.Verify}
	// Preserve scopes; invalidate the signature so the next plan refreshes them.
	if e.State.Plan != nil {
		e.State.Plan.Setup = p.Setup
		e.State.Plan.Verify = p.Verify
	}
	e.State.PlanSignature = ""
	for _, id := range p.Reopen {
		t := e.State.Backlog.Task(id)
		t.Status = "todo"
		t.Blockers = nil
		t.UnblockWhen = ""
	}
	for _, closure := range p.Administrative {
		t := e.State.Backlog.Task(closure.TaskID)
		t.Status = "done"
		t.Blockers = []string{"Administrative closure: " + closure.Reason}
		t.UnblockWhen = ""
		e.event("run-recovery-administrative", t.ID+"  Administrative closure: "+closure.Reason)
	}
	r := &e.State.Recoveries[index]
	r.Outcome = "applied"
	r.Tasks = append([]string(nil), p.Reopen...)
	// Consume the resulting contract too: a failed retry cannot authorize another
	// consultation merely because the repair itself changed the commands.
	next := e.recoveryIdentity()
	if next != "" && next != identity {
		e.State.Recoveries = append(e.State.Recoveries, RunRecovery{Identity: next, Outcome: "consumed", Integrated: e.State.Integrated})
	}
	if err = e.export(); err != nil {
		return false, err
	}
	e.event("run-recovery-applied", "Applied orchestration correction; retained implementation must pass integration checks.")
	return true, nil
}

func (e *Engine) recoveryProgress(taskID string) {
	for i := range e.State.Recoveries {
		r := &e.State.Recoveries[i]
		if r.Outcome != "applied" && r.Outcome != "retry" {
			continue
		}
		for _, id := range r.Tasks {
			if id == taskID {
				r.Outcome = "integrated"
				e.event("run-recovery-retry", "Recovered task "+taskID+" passed integration.")
			}
		}
	}
}

func (e *Engine) recoveryDispatch(t Task) error {
	changed := false
	for i := range e.State.Recoveries {
		r := &e.State.Recoveries[i]
		if r.Outcome != "integrated" {
			continue
		}
		for _, id := range r.Tasks {
			for _, dep := range t.DependsOn {
				if dep == id {
					changed = true
					r.Outcome = "progress"
					e.event("run-recovery-progress", "Recovered task "+id+" integrated; successor "+t.ID+" started.")
				}
			}
		}
	}
	if changed {
		return e.save()
	}
	return nil
}

func (e *Engine) consultRunRepair(ctx context.Context, evidence string) (diagnosis, error) {
	name := identifier("run-repair")
	work, err := e.Repo.Workspace(name, e.State.Tip, e.State.Backlog)
	if err != nil {
		return diagnosis{}, err
	}
	defer e.Repo.Remove(work)
	before, err := e.Repo.Snapshot(work)
	if err != nil {
		return diagnosis{}, err
	}
	baseline := hashJSON(e.State.Backlog)
	dir := filepath.Join(e.Dir, name)
	c, err := prepareAgentContext(e.Repo, work, dir, e.State.Backlog, nil, evidence, nil, nil)
	if err != nil {
		return diagnosis{}, err
	}
	plan := e.effectiveRecoveryPlan()
	prompt := fmt.Sprintf(`Diagnose a blocked run. Read the accepted backlog and failure feedback. Do not change any files.
Effective setup: %q
Effective verification: %q
Return one JSON object with root_cause, evidence, fix_approach, and optional run_repair:
{"root_cause":"cause","evidence":"retained records","fix_approach":"approach","run_repair":{"remove_sources":[],"setup":"dependency setup","verify":"meaningful current-stage check","reopen":["T001"],"deferred_tasks":[]}}.
Remove only proven empty Looper bootstrap snapshots. Preserve all product requirements and task acceptance criteria.
Use deferred_tasks to identify unfinished tasks that own later verification commands. Keep their requirements intact.
Reopen only retained implementation tasks blocked by the orchestration defect. Do not reopen external prerequisites.
Optional administrative_closures [{"task_id":"...","source":"removed private snapshot","reason":"obsolete orchestration task"}] may close only blocked tasks tagged looper:orchestration. Never close implementation tasks.
If external input is needed, omit run_repair. Do not disable checks, grant credentials, publish, or add tasks.
`, plan.Setup, plan.Verify) + c.references()
	op := e.capture()
	raw, err := op.agent.Run(ctx, op.config.ReviewAgent, work, dir, prompt, false)
	if err != nil {
		return diagnosis{}, err
	}
	if err = c.validate(); err != nil {
		return diagnosis{}, err
	}
	var privateBacklog Backlog
	if err := readJSON(filepath.Join(work, e.Repo.Excluded[0]), &privateBacklog); err != nil || hashJSON(privateBacklog) != baseline {
		return diagnosis{}, fmt.Errorf("consultation changed the private backlog")
	}
	after, err := e.Repo.Snapshot(work)
	if err != nil || after != before {
		return diagnosis{}, fmt.Errorf("consultation changed project files")
	}
	var d diagnosis
	if err = decodeDiagnosis(raw, &d); err != nil || d.RootCause == "" || d.Evidence == "" || d.FixApproach == "" || d.RunRepair == nil {
		return diagnosis{}, fmt.Errorf("no usable run repair; existing blockers remain")
	}
	return d, nil
}

// A task diagnosis can carry the proposal directly to the drained controller.
// This avoids a second agent call that only translates the same evidence.
type pendingRunRepair struct {
	Identity string    `json:"identity"`
	Proposal diagnosis `json:"proposal"`
}

func (e *Engine) effectiveRecoveryPlan() Plan {
	p := Plan{}
	if e.State.Plan != nil {
		p = *e.State.Plan
	}
	if e.Config.Setup != "" {
		p.Setup = e.Config.Setup
	}
	if e.Config.Verify != "" {
		p.Verify = e.Config.Verify
	}
	e.recoveryCommands(&p)
	return p
}

func decodeDiagnosis(raw []byte, d *diagnosis) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(d); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("diagnosis must contain exactly one JSON object")
	}
	return nil
}

func (e *Engine) recoveryBlocked(taskID string) {
	for i := range e.State.Recoveries {
		r := &e.State.Recoveries[i]
		if r.Outcome != "applied" {
			continue
		}
		for _, id := range r.Tasks {
			if id == taskID {
				r.Outcome = "retry_blocked"
				e.event("run-recovery-retry", "Recovered task "+taskID+" remains blocked after retry; its work and evidence are retained.")
			}
		}
	}
}
