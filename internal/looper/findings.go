package looper

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Tags preserve finding identity and ownership in existing backlog schemas.
const operatorTaskTag = "looper:operator"
const deferredTaskTag = "looper:deferred"
const findingKeyPrefix = "looper:finding:"

// Reconciliation transitions. A finding that confirms a blocked task's
// condition, an already delivered outcome, or a wrong owner asks for a
// guarded transition of recorded work instead of another task.
const (
	transitionResume   = "resume"
	transitionComplete = "complete"
	transitionReassign = "reassign"
)

var reconciliationTransitions = map[string]bool{
	transitionResume: true, transitionComplete: true, transitionReassign: true,
}

// reconciliationKeyPrefix names the finding keys the escalated decisions
// carry: one decision per target task, however the report is titled.
const reconciliationKeyPrefix = "reconcile-"

const findingInstructions = `Each finding has owner "agent" or "operator". Human grants, credentials, approvals, and actions reserved for an operator use owner "operator"; they remain blocked outside the worker queue.
Separate local preparation from external publication. Drafting a proposal is agent work; submitting it needs existing authorization.
Classify work by development round. Do not make later-platform validation a gate for an earlier round unless the specification requires it.
For future-round work, set defer_until to a concrete resume condition; otherwise use an empty string. Preserve its requirements and dependencies; do not report it complete.
Use operator ownership only for a concrete action that workers cannot perform. Explain the exact prerequisite and resume condition.
Use a stable key for the unmet outcome or prerequisite, independent of task title or attempt. Reuse the key from an existing looper:finding:<key> tag.
When the outcome or blocker is already tracked, set existing_task_id to that task ID, even if its title differs. Otherwise use an empty string. Dependencies must be existing task IDs, never titles.
Do not create another task to retry the same missing prerequisite or to close duplicate tasks. Attach new evidence to the existing task instead. A follow-up is a report of work, not new authorization.
An existing task keeps its ownership. Report a distinct operator prerequisite with its own key; never relabel the software task it blocks.`

func normalizedFindingKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func hasTaskTag(t Task, tag string) bool {
	for _, existing := range t.Tags {
		if existing == tag {
			return true
		}
	}
	return false
}

// Older reports did not carry ownership. Preserve explicit operator wording
// without classifying ordinary code tasks that merely mention operators.
var operatorTitle = regexp.MustCompile(`(?i)^(?:the |an? )?(?:operator|human|administrator)\s+(?:must\s+|needs? to\s+|issues?\b|grants?\b|appends?\b|authorizes?\b|approves?\b|provides?\b|supplies\b)`)
var operatorInstruction = regexp.MustCompile(`(?im)(?:^|[.!?]\s+)(?:the |an? )?(?:operator|human|administrator)\s+(?:must|needs? to)\s+(?:git\s+push|push|run|issue|grant|append|authorize|approve|provide|supply|install|create|configure|rotate|revoke)\b`)

func operatorFinding(f Finding) bool {
	return f.Owner == "operator" || operatorTitle.MatchString(strings.TrimSpace(f.Title)) || operatorInstruction.MatchString(f.Details)
}

func (t Task) OperatorOnly() bool {
	return hasTaskTag(t, operatorTaskTag) || operatorFinding(Finding{Title: t.Title, Details: t.Details})
}

// A findingStage applies one transaction's findings together with the state
// the surrounding result or review transition mutates. The mutations and
// their journal notes stage in memory: the notes are emitted only after the
// saved state/export protocol commits the changes, and a failed commit rolls
// the mutations back so no in-memory-only follow-up can survive it. Recovery
// replays the retained result or review and re-applies the findings.
type findingStage struct {
	e           *Engine
	backlog     Backlog
	planSig     string
	finished    bool
	reviewedAt  int
	reviewedTip string
	reviewPend  bool
	// The review coverage record (T128) stages with the same transaction, so
	// a failed commit leaves the recorded coverage exactly as it was.
	reviewedTree     string
	reviewedComplete bool
	reviewedImports  int
	reviewedBacklog  string
	// revision is the accepted revision this transaction applies against. A
	// reconciliation must have verified exactly it, so evidence gathered on an
	// older tree cannot release or close a task the controller has moved on.
	revision string
	// attempt names the worker whose result this transaction applies, or nil
	// on the review paths. A worker cannot read the accepted revision
	// mid-flight, so its reconciliation names the base its worktree assembled
	// from and the guard accepts that base only while the accepted revision
	// still is what that attempt itself verified (T139).
	attempt *Attempt
	notes   []findingNote
}

// findingNote is one staged finding outcome. The task identity names the
// mutated task; a rejected report that never reached a task carries none.
type findingNote struct {
	taskID  string
	message string
}

func (e *Engine) beginFindingStage() *findingStage {
	return &findingStage{
		e:                e,
		backlog:          clone(e.State.Backlog),
		planSig:          e.State.PlanSignature,
		finished:         e.State.Finished,
		reviewedAt:       e.State.ReviewedAt,
		reviewedTip:      e.State.ReviewedTip,
		reviewPend:       e.State.ReviewPending,
		reviewedTree:     e.State.ReviewedTree,
		reviewedComplete: e.State.ReviewedComplete,
		reviewedImports:  e.State.ReviewedImports,
		reviewedBacklog:  e.State.ReviewedBacklog,
		revision:         e.State.Tip,
	}
}

// beginWorkerStage opens the transaction one worker result applies. The
// attempt records the base the worker's worktree assembled from, so a
// reconciliation it reports can name a revision the worker could actually
// read instead of the accepted revision it cannot know mid-flight.
func (e *Engine) beginWorkerStage(a *Attempt) *findingStage {
	s := e.beginFindingStage()
	s.attempt = a
	return s
}

// verifiedRevision reports whether one reconciliation reported a revision
// this transaction can vouch for. A review names the accepted revision its
// prompt taught. A worker names the base its worktree assembled from, and
// that base counts only while the accepted revision still is a tree the
// attempt's own evidence covers: the unchanged base, or the verification
// commit this attempt's work became on top of that base. Anything else
// gathered its evidence on a tree the controller has moved past, exactly the
// report a review at an older revision produces.
func (s *findingStage) verifiedRevision(reported string) bool {
	if reported == "" {
		return false
	}
	if s.attempt == nil {
		return reported == s.revision
	}
	a := s.attempt
	if reported != a.Base {
		return false
	}
	if s.revision == a.Base {
		return true
	}
	return a.Verification != nil && s.revision == a.Verification.Commit && a.Verification.Base == a.Base
}

// emit reports the staged finding outcomes. Call it only after the changes
// are durable, so a journal success is never unbacked by saved state.
func (s *findingStage) emit() {
	for _, note := range s.notes {
		s.e.taskIDEvent("finding", note.message, note.taskID)
	}
	s.notes = nil
}

// rollback restores every field the transaction staged. A commit that fails
// before the export protocol finishes leaves neither a task nor a marker of
// this application behind.
func (s *findingStage) rollback() {
	e := s.e
	e.State.Backlog = s.backlog
	e.State.PlanSignature = s.planSig
	e.State.Finished = s.finished
	e.State.ReviewedAt = s.reviewedAt
	e.State.ReviewedTip = s.reviewedTip
	e.State.ReviewPending = s.reviewPend
	e.State.ReviewedTree = s.reviewedTree
	e.State.ReviewedComplete = s.reviewedComplete
	e.State.ReviewedImports = s.reviewedImports
	e.State.ReviewedBacklog = s.reviewedBacklog
	s.notes = nil
}

func (s *findingStage) add(f Finding) {
	e := s.e
	note := func(taskID, message string) { s.notes = append(s.notes, findingNote{taskID: taskID, message: message}) }
	reject := func(reason string) {
		note("", fmt.Sprintf("Ignored follow-up %q (%s): %s.", shortText(sanitize(f.Title), 160), hashJSON(f)[:12], reason))
	}
	if strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.Details) == "" || strings.TrimSpace(f.Evidence) == "" {
		reject("missing a title, acceptance details, or evidence")
		return
	}
	if f.Owner != "" && f.Owner != "agent" && f.Owner != "operator" {
		reject("owner must be agent or operator")
		return
	}
	if f.Transition != "" && !reconciliationTransitions[f.Transition] {
		reject("transition must be resume, complete, or reassign")
		return
	}
	if f.Transition != "" && f.DeferUntil != "" {
		reject("a reconciliation does not defer work")
		return
	}
	key := normalizedFindingKey(f.Key)
	if f.Key != "" && key == "" {
		reject("finding key must not be whitespace")
		return
	}
	var existing *Task
	if f.ExistingTaskID != "" {
		existing = e.State.Backlog.Task(f.ExistingTaskID)
		if existing == nil {
			reject("unknown existing task " + sanitize(f.ExistingTaskID))
			return
		}
	}
	for i := range e.State.Backlog.Tasks {
		t := &e.State.Backlog.Tasks[i]
		if normalizedFindingKey(t.Title) == normalizedFindingKey(f.Title) ||
			(key != "" && hasTaskTag(*t, findingKeyPrefix+key)) {
			if existing != nil && existing.ID != t.ID {
				reject("finding identity conflicts with existing task")
				return
			}
			existing = t
		}
	}
	for _, d := range f.DependsOn {
		if e.State.Backlog.Task(d) == nil {
			reject("unknown dependency " + sanitize(d))
			return
		}
	}
	operator := operatorFinding(f)
	if f.Transition != "" {
		s.reconcile(f, key, existing)
		return
	}
	if existing != nil {
		// Finding identity does not change the contract. Only idle coding tasks
		// can accept repair criteria without invalidating retained work.
		repeat := key != "" && hasTaskTag(*existing, findingKeyPrefix+key)
		if key != "" && !repeat {
			existing.Tags = append(existing.Tags, findingKeyPrefix+key)
		}
		active := existing.Status == "doing"
		for _, a := range e.State.Attempts {
			if a.Task.ID == existing.ID && a.Phase != "done" && a.Phase != "blocked" && a.Phase != "cancelled" {
				active = true
			}
		}
		if !active && f.DeferUntil == "" && !existing.Deferred() && !operator && !existing.OperatorOnly() && (existing.Status == "done" || existing.Status == "todo") {
			// A completed implementation can still have an unmet requirement.
			// Retain its attempt history and give the next worker the repair
			// contract, rather than leaving evidence only in the journal.
			existing.Status = "todo"
			repair := "\n\nReview repair: " + f.Title + "\n" + f.Details + "\nEvidence: " + f.Evidence
			if !strings.Contains(existing.Details, repair) {
				existing.Details += repair
			}
			existing.Blockers = nil
			existing.UnblockWhen = ""
			e.State.Finished = false
		}
		if operator && existing.OperatorOnly() && !active {
			holdForOperator(existing)
		}
		if !active && f.DeferUntil != "" {
			deferTask(existing, f.DeferUntil)
		}
		if reason, action := repeatedBlockedFinding(*existing, f, repeat, operator, active); reason != "" {
			// A plain report cannot change a blocked task's state, and this one
			// has now arrived twice. One stable decision replaces the repeated
			// evidence-only reports the next review would rediscover.
			s.escalate(*existing, f, reason, action)
		}
		e.State.PlanSignature = ""
		note(existing.ID, existing.ID+"  Updated existing finding: "+sanitize(f.Title)+"\nEvidence: "+sanitize(f.Evidence))
		return
	}
	priority := f.Priority
	if priority < 1 || priority > 5 {
		priority = 2
	}
	id := nextTaskID(e.State.Backlog)
	task := Task{ID: id, Title: f.Title, Priority: priority, Status: "todo", Details: f.Details + "\nEvidence: " + f.Evidence, DependsOn: f.DependsOn}
	if key != "" {
		task.Tags = []string{findingKeyPrefix + key}
	}
	if operator {
		holdForOperator(&task)
	}
	if f.DeferUntil != "" {
		deferTask(&task, f.DeferUntil)
	}
	e.State.Backlog.Tasks = append(e.State.Backlog.Tasks, task)
	e.State.PlanSignature = ""
	e.State.Finished = false
	note(id, id+"  Added follow-up: "+f.Title)
}

// nextTaskID derives the identifier one new finding task takes: the first
// free T<n>, so added follow-ups and escalated decisions cannot collide.
func nextTaskID(b Backlog) string {
	n := len(b.Tasks) + 1
	id := fmt.Sprintf("T%03d", n)
	for b.Task(id) != nil {
		n++
		id = fmt.Sprintf("T%03d", n)
	}
	return id
}

// circularConditionReason names why one blocked task's own condition can be
// satisfied only by the task itself, or "" when independent work can satisfy
// it. Dependency cycles are rejected at validation; this catches the two
// recorded forms (delivery audit 2026-11-17): a blocker or resume condition
// that names the task — any mention counts, because an automatic transition
// must err toward the operator — and one that names no task at all but
// requires the task's own scope or deferral round (observe.go's
// idFreeCircularWording, the Charlie T040 "waits for save work to resume"
// wording).
func circularConditionReason(t Task) string {
	if t.Status != "blocked" || t.ID == "" {
		return ""
	}
	id := strings.ToLower(t.ID)
	for _, condition := range append(append([]string{}, t.Blockers...), t.UnblockWhen) {
		lowered := strings.ToLower(condition)
		if strings.Contains(lowered, id) {
			return "the unblock condition names task " + t.ID + " itself"
		}
		if idFreeCircularWording(condition, t) {
			return "the unblock condition requires task " + t.ID + "'s own work, so no independent prerequisite can satisfy it"
		}
	}
	return ""
}

// taskHoldsWork names the phase that owns one task's contract, mirroring the
// amendment boundary: a live worker, a check, a retained result, or
// restartable resume work must drain before a finding can reconcile the task.
func taskHoldsWork(e *Engine, id string) string {
	if t := e.State.Backlog.Task(id); t != nil && t.Status == "doing" {
		return "doing"
	}
	for _, a := range e.State.Attempts {
		if a != nil && a.Task.ID == id && amendHoldsContract(a) {
			return a.Phase
		}
	}
	return ""
}

// reconcile applies one reconciliation proposal: a finding that confirms a
// blocked task's condition is satisfied, that its outcome already exists, or
// that its owner is wrong. Every transition is guarded: it names existing
// work, carries the revision the report verified, targets an idle task, and
// changes only fields outside Contract(), so retained work keeps its
// acceptance identity. Operator gates, deferrals, ownership changes, and
// circular conditions stay with the operator: the engine escalates one
// stable decision instead of applying them itself.
func (s *findingStage) reconcile(f Finding, key string, existing *Task) {
	e := s.e
	note := func(taskID, message string) { s.notes = append(s.notes, findingNote{taskID: taskID, message: message}) }
	reject := func(reason string) {
		note("", fmt.Sprintf("Ignored reconciliation %q (%s): %s.", shortText(sanitize(f.Title), 160), hashJSON(f)[:12], reason))
	}
	if existing == nil {
		reject("it names no existing task")
		return
	}
	if f.Revision == "" {
		reject("it carries no revision")
		return
	}
	if !s.verifiedRevision(f.Revision) {
		reason := fmt.Sprintf("it verified revision %s while the controller accepted %s", shortText(sanitize(f.Revision), 64), shortText(sanitize(s.revision), 64))
		if s.attempt != nil {
			if f.Revision != s.attempt.Base {
				reason += fmt.Sprintf("; a worker reconciliation names the revision its worktree started from (%s)", shortText(sanitize(s.attempt.Base), 64))
			} else {
				reason += "; the accepted revision holds work this attempt never read"
			}
		}
		reject(reason)
		return
	}
	if held := taskHoldsWork(e, existing.ID); held != "" {
		reject("task " + existing.ID + " holds " + held + " work that owns its contract")
		return
	}
	if key != "" && !hasTaskTag(*existing, findingKeyPrefix+key) {
		existing.Tags = append(existing.Tags, findingKeyPrefix+key)
	}
	if isReconciliationDecision(*existing) {
		// The target is itself an escalated decision: the operator's action is
		// that task. A report against it records evidence; it never escalates
		// a second decision about the decision.
		note(existing.ID, existing.ID+"  Reconciliation report against an operator decision; the decision records its own outcome\nEvidence: "+sanitize(f.Evidence))
		return
	}
	switch {
	case f.Transition == transitionReassign:
		s.escalate(*existing, f, "an ownership change needs the operator",
			"Correct the owner by editing the task file; Looper imports the edit at a safe boundary. Cancel the task with looper cancel "+existing.ID+" if the work is no longer needed.")
		return
	case circularConditionReason(*existing) != "":
		s.escalate(*existing, f, circularConditionReason(*existing),
			"Rewrite the condition in the task file so independent work can satisfy it, or cancel the task with looper cancel "+existing.ID+"; Looper imports a task-file edit at a safe boundary.")
		return
	case existing.Deferred():
		s.escalate(*existing, f, "the deferral gate stays with the operator",
			"When the condition holds, edit the task file to remove looper:deferred and set status todo; Looper imports the edit at a safe boundary. Keep the deferral and record why otherwise.")
		return
	case existing.OperatorOnly():
		s.escalate(*existing, f, "the operator gate stays with the operator",
			"Record the completed operator action as evidence and mark the task done, edit the task file to remove looper:operator and clear the blocker, or cancel the task with looper cancel "+existing.ID+".")
		return
	}
	switch f.Transition {
	case transitionResume:
		switch existing.Status {
		case "done":
			reject("task " + existing.ID + " is done; a satisfied condition never reopens completed work")
			return
		case "cancelled":
			reject("task " + existing.ID + " is cancelled; decided work is not reconciled")
			return
		case "todo":
			note(existing.ID, existing.ID+"  Reconciliation confirmed the task is already schedulable\nEvidence: "+sanitize(f.Evidence))
			return
		}
		for _, d := range existing.DependsOn {
			if dep := e.State.Backlog.Task(d); dep == nil || dep.Status != "done" {
				reject("dependency " + d + " is not done")
				return
			}
		}
		existing.Status = "todo"
		existing.Blockers = nil
		existing.UnblockWhen = ""
		existing.BlockReason = nil
		existing.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		e.State.PlanSignature = ""
		e.State.Finished = false
		note(existing.ID, existing.ID+"  Reconciled resume at "+shortText(sanitize(f.Revision), 64)+": "+sanitize(f.Title)+"\nEvidence: "+sanitize(f.Evidence))
	case transitionComplete:
		switch existing.Status {
		case "done":
			note(existing.ID, existing.ID+"  Reconciliation confirmed the recorded outcome; no worker implements it again\nEvidence: "+sanitize(f.Evidence))
			return
		case "cancelled":
			reject("task " + existing.ID + " is cancelled; decided work is not reconciled")
			return
		}
		existing.Status = "done"
		existing.Blockers = nil
		existing.UnblockWhen = ""
		existing.BlockReason = nil
		existing.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		e.State.PlanSignature = ""
		note(existing.ID, existing.ID+"  Reconciled complete at "+shortText(sanitize(f.Revision), 64)+": "+sanitize(f.Title)+"\nEvidence: "+sanitize(f.Evidence))
	}
}

// repeatedBlockedFinding reports why a plain, non-deferring finding that
// reaches a blocked task it cannot move should escalate: the report arrived
// before (its key tag is already recorded) and nothing changed the state
// since. Operator-owned findings and gated tasks keep their existing stable
// action — the blocked task itself names the operator's prerequisite.
func repeatedBlockedFinding(t Task, f Finding, repeat, operator, active bool) (reason, action string) {
	if !repeat || active || f.DeferUntil != "" || operator || t.Status != "blocked" || t.Deferred() || t.OperatorOnly() {
		return "", ""
	}
	if reason := circularConditionReason(t); reason != "" {
		return reason,
			"Rewrite the condition in the task file so independent work can satisfy it, or cancel the task with looper cancel " + t.ID + "; Looper imports a task-file edit at a safe boundary."
	}
	return "repeated findings change no state while task " + t.ID + " stays blocked",
		"Approve one action: edit the task file to set status todo (Looper imports the edit at a safe boundary), or cancel the task with looper cancel " + t.ID + "."
}

// escalate records the one operator decision a reconciliation the engine
// refuses to apply itself needs. The decision task is keyed by the target
// task, never by the finding's title, so the first refusal creates it and
// however many later reports repeat the condition only add evidence.
func (s *findingStage) escalate(target Task, f Finding, reason, action string) {
	e := s.e
	if decision := decisionTaskFor(&e.State.Backlog, target.ID); decision != nil {
		e.State.PlanSignature = ""
		s.notes = append(s.notes, findingNote{taskID: decision.ID, message: decision.ID + "  Reconciliation of " + target.ID + " still needs the recorded decision: " + reason + "\nEvidence: " + sanitize(f.Evidence)})
		return
	}
	priority := f.Priority
	if priority < 1 || priority > 5 {
		priority = 2
	}
	decision := Task{
		ID:       nextTaskID(e.State.Backlog),
		Title:    "Decide the blocked state of task " + target.ID,
		Priority: priority,
		Status:   "todo",
		Details: "A review reconciling task " + target.ID + " was refused: " + reason + ".\n" +
			"Finding: " + f.Title + "\n" +
			"Evidence: " + f.Evidence + "\n" +
			"Operator action: " + action,
		Tags: []string{findingKeyPrefix + reconciliationKey(target.ID)},
	}
	holdForOperator(&decision)
	e.State.Backlog.Tasks = append(e.State.Backlog.Tasks, decision)
	e.State.PlanSignature = ""
	e.State.Finished = false
	s.notes = append(s.notes, findingNote{taskID: decision.ID, message: decision.ID + "  Escalated reconciliation of " + target.ID + ": " + reason + "\nEvidence: " + sanitize(f.Evidence)})
}

// decisionTaskFor returns the escalated operator decision for one target
// task, or nil when none exists. The key names the target, so renamed
// findings and repeated cycles find the same decision.
func decisionTaskFor(b *Backlog, targetID string) *Task {
	key := findingKeyPrefix + reconciliationKey(targetID)
	for i := range b.Tasks {
		if hasTaskTag(b.Tasks[i], key) {
			return &b.Tasks[i]
		}
	}
	return nil
}

// recordedFindingKey returns the stable finding key one task records, when it
// carries one. Reconciliation decision keys name the decision's target, not a
// reported finding, so they do not count.
func recordedFindingKey(t Task) string {
	for _, tag := range t.Tags {
		if !strings.HasPrefix(tag, findingKeyPrefix) {
			continue
		}
		key := strings.TrimPrefix(tag, findingKeyPrefix)
		if key != "" && !strings.HasPrefix(key, reconciliationKeyPrefix) {
			return key
		}
	}
	return ""
}

// sweepUnchanged reconciles the findings a due review would re-report at an
// unchanged revision (T128). Each blocked task that carries a recorded
// finding gets the one stable operator decision the same repeated report
// would escalate; the cycle itself is the repeat, because the finding stayed
// unresolved through it. A decision that already exists is left exactly as
// it is, so repeated cycles change nothing: no new evidence exists, and the
// recorded decision already presents the action. Tasks that present their
// own operator action — operator gates, deferrals, and the decisions
// themselves — keep it.
func (s *findingStage) sweepUnchanged() {
	e := s.e
	for i := range e.State.Backlog.Tasks {
		t := &e.State.Backlog.Tasks[i]
		if t.Status != "blocked" || t.Marker() || isReconciliationDecision(*t) {
			continue
		}
		key := recordedFindingKey(*t)
		if key == "" || t.Deferred() || t.OperatorOnly() {
			continue
		}
		active := false
		for _, a := range e.State.Attempts {
			if a != nil && a.Task.ID == t.ID && a.Phase != "done" && a.Phase != "blocked" && a.Phase != "cancelled" {
				active = true
			}
		}
		if active {
			continue
		}
		finding := Finding{
			Title:          t.Title,
			Details:        "The recorded finding is unresolved and the published revision is unchanged since the review that reported it.",
			Evidence:       "Review cycle skipped at " + e.State.Tip + "; the finding's task stays blocked.",
			Owner:          "agent",
			Key:            key,
			ExistingTaskID: t.ID,
			Priority:       t.Priority,
		}
		reason, action := repeatedBlockedFinding(*t, finding, true, false, active)
		if reason == "" || decisionTaskFor(&e.State.Backlog, t.ID) != nil {
			continue
		}
		s.escalate(*t, finding, reason, action)
	}
}

// reconciliationKey derives the stable finding key of the one operator
// decision a refused reconciliation escalates to. The key names the target
// task, so renamed reports reuse the same decision instead of adding one.
func reconciliationKey(taskID string) string {
	return reconciliationKeyPrefix + strings.ToLower(taskID)
}

// isReconciliationDecision reports whether one task is itself an escalated
// operator decision — it carries the reconciliation key of the task it
// decides. Such a task is the operator's action; reports against it record
// evidence, never another decision.
func isReconciliationDecision(t Task) bool {
	for _, tag := range t.Tags {
		if strings.HasPrefix(tag, findingKeyPrefix+reconciliationKeyPrefix) {
			return true
		}
	}
	return false
}

func holdForOperator(t *Task) {
	t.Tags = mergeUnique(t.Tags, []string{operatorTaskTag})
	if t.Status == "done" {
		return
	}
	t.Status = "blocked"
	t.Blockers = mergeUnique(t.Blockers, []string{"An operator must complete this prerequisite; coding workers cannot execute it."})
	t.UnblockWhen = "Record evidence that the operator completed the action, then mark this task done. To delegate different work, explicitly change its contract and remove looper:operator."
	t.BlockReason = operatorBlockReason()
}

func (t Task) Deferred() bool { return hasTaskTag(t, deferredTaskTag) }

func deferTask(t *Task, condition string) {
	if t.Status == "done" {
		return
	}
	t.Tags = mergeUnique(t.Tags, []string{deferredTaskTag})
	t.Status = "blocked"
	t.Blockers = mergeUnique(t.Blockers, []string{"Deferred: " + condition})
	t.UnblockWhen = condition + "; remove looper:deferred and set todo when this condition is met."
	t.BlockReason = deferredBlockReason()
}

const taskClassificationInstructions = `Classify tasks by development round and preserve each round's acceptance requirements.
Keep future-platform validation outside the current round unless the specification explicitly makes it a prerequisite.
Represent postponed tasks with status blocked, tag looper:deferred, and a concrete unblock_when condition. Do not mark them done.
Separate autonomous local preparation from external publication. Preparing an upstream proposal does not require posting it.
Use looper:operator only for concrete actions workers cannot perform under existing authorization. Record the exact prerequisite.
Never weaken release gates or remove required work to avoid a blocker.`
