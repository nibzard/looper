package looper

import (
	"fmt"
)

// The structured block reason (OW-09). Free-text blockers stay human
// evidence; this object names the current primary dispatch barrier so a
// monitor can tell expected waiting from execution failure without parsing
// blocker text.

// Reason classes name who must act before the task can run again.
const (
	blockClassOperator = "operator"
	blockClassDeferred = "deferred"
	blockClassFailure  = "failure"
	blockClassUnknown  = "unknown"
)

// Retry guidance restricts what may happen next. The class alone never
// authorizes an automatic retry.
const (
	blockRetryYes         = "yes"
	blockRetryAfterChange = "after-change"
	blockRetryNo          = "no"
	blockRetryUnknown     = "unknown"
)

// Reason sources name who recorded the reason.
const (
	blockSourceController = "controller"
	blockSourceWorker     = "worker"
	blockSourceImport     = "import"
	blockSourceAmend      = "amend"
)

// Stable codes built from typed failure paths. The list stays closed so
// monitors can filter on it.
const (
	blockCodeOperatorPrerequisite = "operator_prerequisite"
	blockCodeDeferredCondition    = "deferred_condition"
	blockCodeRepairLimit          = "repair_limit_reached"
	blockCodeRepeatedFailure      = "repeated_failure"
	blockCodeRunRepairProposed    = "run_repair_proposed"
	blockCodeMergeFailed          = "merge_failed"
	blockCodeInvalidWritePath     = "invalid_write_path"
	blockCodeUnknown              = "unknown"
)

// Bounds keep one reason inside response budgets. Code, source, and
// references stay short; evidence text lives in blockers, not here.
const (
	maxBlockReasonCode      = 64
	maxBlockReasonSource    = 32
	maxBlockReasonAttemptID = 64
)

// BlockReason is the current primary dispatch barrier of one blocked task.
// It never carries raw error output, executable arguments, secrets, or
// private agent reasoning; the free-text blockers keep that evidence.
type BlockReason struct {
	Class     string `json:"class"`
	Code      string `json:"code,omitempty"`
	Retry     string `json:"retry,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
	Source    string `json:"source,omitempty"`
}

var blockReasonClasses = map[string]bool{
	blockClassOperator: true, blockClassDeferred: true,
	blockClassFailure: true, blockClassUnknown: true,
}

var blockReasonRetries = map[string]bool{
	blockRetryYes: true, blockRetryAfterChange: true,
	blockRetryNo: true, blockRetryUnknown: true,
}

// blockReasonValid rejects a reason a consumer cannot act on: an unknown
// class, unknown retry guidance, or overlong bounded fields.
func blockReasonValid(r *BlockReason) error {
	if r == nil {
		return nil
	}
	if !blockReasonClasses[r.Class] {
		return fmt.Errorf("block_reason class %q is not one of operator, deferred, failure, unknown", r.Class)
	}
	if r.Retry != "" && !blockReasonRetries[r.Retry] {
		return fmt.Errorf("block_reason retry %q is not one of yes, after-change, no, unknown", r.Retry)
	}
	if len(r.Code) > maxBlockReasonCode {
		return fmt.Errorf("block_reason code exceeds %d characters", maxBlockReasonCode)
	}
	if len(r.Source) > maxBlockReasonSource {
		return fmt.Errorf("block_reason source exceeds %d characters", maxBlockReasonSource)
	}
	if len(r.AttemptID) > maxBlockReasonAttemptID {
		return fmt.Errorf("block_reason attempt_id exceeds %d characters", maxBlockReasonAttemptID)
	}
	return nil
}

// bound clamps one reason to its field bounds without changing its meaning.
func (r *BlockReason) bound() *BlockReason {
	if r == nil {
		return nil
	}
	r.Code = clampString(r.Code, maxBlockReasonCode)
	r.Source = clampString(r.Source, maxBlockReasonSource)
	r.AttemptID = clampString(r.AttemptID, maxBlockReasonAttemptID)
	return r
}

// validateRejectingUnknown reports why a caller-supplied reason is unusable.
// An amendment goes through this: it must carry a known class and known
// retry guidance, not silently fall back to unknown.
func validateBlockReasonStrict(r *BlockReason) error {
	if r == nil {
		return nil
	}
	if r.Class == "" {
		return fmt.Errorf("block_reason needs a class")
	}
	if err := blockReasonValid(r); err != nil {
		return err
	}
	if r.Retry == "" {
		return fmt.Errorf("block_reason needs retry guidance")
	}
	return nil
}

func operatorBlockReason() *BlockReason {
	return &BlockReason{
		Class: blockClassOperator, Code: blockCodeOperatorPrerequisite,
		Retry: blockRetryAfterChange, Source: blockSourceController,
	}
}

func deferredBlockReason() *BlockReason {
	return &BlockReason{
		Class: blockClassDeferred, Code: blockCodeDeferredCondition,
		Retry: blockRetryAfterChange, Source: blockSourceController,
	}
}

func failureBlockReason(code, retry, attemptID string) *BlockReason {
	return &BlockReason{
		Class: blockClassFailure, Code: code, Retry: retry,
		AttemptID: attemptID, Source: blockSourceController,
	}
}

// workerFailureBlockReason derives one reason from the bounded summary a
// failed operation already carries (AX-021). A timeout names a retry
// candidate; other outcomes keep unknown guidance until evidence is read.
func workerFailureBlockReason(a *Attempt, s *FailureSummary) *BlockReason {
	r := failureBlockReason(orDefault(s.Outcome, blockCodeUnknown), blockRetryUnknown, a.ID)
	if s.Operation != opWorker {
		r.Code = clampString(s.Operation+"_"+orDefault(s.Outcome, blockCodeUnknown), maxBlockReasonCode)
	}
	if s.Outcome == outcomeTimeout {
		r.Retry = blockRetryYes
	}
	return r
}

// classifyBlockReason picks the reason one blocked task carries. The
// operator and deferral control tags are authoritative gates: they win over
// any reported reason, and both present report deferred as the primary
// class because the resume condition is the outermost barrier. A validated
// typed reason from the new execution replaces prior current metadata;
// everything else stays unknown.
func classifyBlockReason(t *Task, reported *BlockReason, source string) *BlockReason {
	switch {
	case t.Deferred():
		r := deferredBlockReason()
		r.Source = source
		return r
	case hasTaskTag(*t, operatorTaskTag):
		r := operatorBlockReason()
		r.Source = source
		return r
	case reported != nil && blockReasonValid(reported) == nil:
		r := reported.bound()
		if r.Source == "" {
			r.Source = source
		}
		return r
	}
	return &BlockReason{
		Class: blockClassUnknown, Code: blockCodeUnknown,
		Retry: blockRetryUnknown, Source: source,
	}
}

// effectiveBlockReason derives the reason one blocked task reports: the
// stored reason when it is valid, the class its control tags fix when only
// tags exist, and unknown otherwise. Observation and projections use this
// without writing normalized metadata back into the saved backlog.
func (t Task) effectiveBlockReason() *BlockReason {
	if t.Status != "blocked" {
		return nil
	}
	if t.BlockReason != nil {
		if err := blockReasonValid(t.BlockReason); err == nil {
			return t.BlockReason
		}
		// Invalid stored metadata degrades to unknown for readers instead of
		// failing the observation; the backlog keeps the bytes it kept.
		return &BlockReason{Class: blockClassUnknown, Code: blockCodeUnknown, Retry: blockRetryUnknown, Source: t.BlockReason.Source}
	}
	return classifyBlockReason(&t, nil, "")
}

// normalizeImportedBlockReasons derives the structured reason for blocked
// tasks an import delivered without one. Control tags decide the class; the
// rest stay unknown until typed evidence exists. It runs inside an
// authorized import mutation, never during observation.
func normalizeImportedBlockReasons(b *Backlog) {
	for i := range b.Tasks {
		t := &b.Tasks[i]
		if t.Status != "blocked" || t.BlockReason != nil {
			continue
		}
		t.BlockReason = classifyBlockReason(t, nil, blockSourceImport)
	}
}
