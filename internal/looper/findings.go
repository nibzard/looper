package looper

import (
	"fmt"
	"regexp"
	"strings"
)

// Tags preserve finding identity and ownership in existing backlog schemas.
const operatorTaskTag = "looper:operator"
const deferredTaskTag = "looper:deferred"
const findingKeyPrefix = "looper:finding:"

const findingInstructions = `Each finding has owner "agent" or "operator". Human grants, credentials, approvals, and actions reserved for an operator use owner "operator"; they remain blocked outside the worker queue.
Separate local preparation from external publication. Drafting a proposal is agent work; submitting it needs existing authorization.
Classify work by development round. Do not make later-platform validation a gate for an earlier round unless the specification requires it.
For future-round work, set defer_until to a concrete resume condition; otherwise use an empty string. Preserve its requirements and dependencies; do not report it complete.
Use operator ownership only for a concrete action that workers cannot perform. Explain the exact prerequisite and resume condition.
Use a stable key for the unmet outcome or prerequisite, independent of task title or attempt. Reuse the key from an existing looper:finding:<key> tag.
When the outcome or blocker is already tracked, set existing_task_id to that task ID, even if its title differs. Otherwise use an empty string. Dependencies must be existing task IDs, never titles.
Do not create another task to retry the same missing prerequisite or to close duplicate tasks. Attach new evidence to the existing task instead. A follow-up is a report of work, not new authorization.`

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

func (e *Engine) addFinding(f Finding) {
	reject := func(reason string) {
		e.event("finding", fmt.Sprintf("Ignored follow-up %q (%s): %s.", shortText(sanitize(f.Title), 160), hashJSON(f)[:12], reason))
	}
	if strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.Details) == "" || strings.TrimSpace(f.Evidence) == "" {
		reject("missing a title, acceptance details, or evidence")
		return
	}
	if f.Owner != "" && f.Owner != "agent" && f.Owner != "operator" {
		reject("owner must be agent or operator")
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
	if existing != nil {
		// Finding identity does not change the contract. Only idle coding tasks
		// can accept repair criteria without invalidating retained work.
		if key != "" && !hasTaskTag(*existing, findingKeyPrefix+key) {
			existing.Tags = append(existing.Tags, findingKeyPrefix+key)
		}
		active := existing.Status == "doing"
		for _, a := range e.State.Attempts {
			if a.Task.ID == existing.ID && a.Phase != "done" && a.Phase != "blocked" {
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
		if operator {
			if !active {
				holdForOperator(existing)
			}
		}
		if !active && f.DeferUntil != "" {
			deferTask(existing, f.DeferUntil)
		}
		e.State.PlanSignature = ""
		e.event("finding", existing.ID+"  Updated existing finding: "+sanitize(f.Title)+"\nEvidence: "+sanitize(f.Evidence))
		return
	}
	priority := f.Priority
	if priority < 1 || priority > 5 {
		priority = 2
	}
	n := len(e.State.Backlog.Tasks) + 1
	id := fmt.Sprintf("T%03d", n)
	for e.State.Backlog.Task(id) != nil {
		n++
		id = fmt.Sprintf("T%03d", n)
	}
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
	e.event("finding", id+"  Added follow-up: "+f.Title)
}

func holdForOperator(t *Task) {
	t.Tags = mergeUnique(t.Tags, []string{operatorTaskTag})
	if t.Status == "done" {
		return
	}
	t.Status = "blocked"
	t.Blockers = mergeUnique(t.Blockers, []string{"An operator must complete this prerequisite; coding workers cannot execute it."})
	t.UnblockWhen = "Record evidence that the operator completed the action, then mark this task done. To delegate different work, explicitly change its contract and remove looper:operator."
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
}

const taskClassificationInstructions = `Classify tasks by development round and preserve each round's acceptance requirements.
Keep future-platform validation outside the current round unless the specification explicitly makes it a prerequisite.
Represent postponed tasks with status blocked, tag looper:deferred, and a concrete unblock_when condition. Do not mark them done.
Separate autonomous local preparation from external publication. Preparing an upstream proposal does not require posting it.
Use looper:operator only for concrete actions workers cannot perform under existing authorization. Record the exact prerequisite.
Never weaken release gates or remove required work to avoid a blocker.`
