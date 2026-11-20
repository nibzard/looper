package looper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Timed provider quota pauses (T125). A quota failure closes the provider's
// usage window for a bounded time. The controller pauses the affected agent
// profile, retains its work with the provider's reset text, and resumes
// automatically at the reset time the provider reported — or, when it
// reported none, after the operator's rate-limit delay. Untimed quota text,
// invalid credentials, and unsupported configuration keep their existing
// classes: they repeat identically until an operator acts, so they still end
// the run for an operator action.

// failureQuota is the AgentError kind of a timed quota window.
const failureQuota = "quota"

// quotaRetryClock renders a pause's retry time for a human line (T145): the
// clock alone while the retry lands today, and the date beside it once it
// does not, so an operator can tell which day a "retries at 11:34:00" line
// means.
func quotaRetryClock(retry, now time.Time) string {
	retry, now = retry.Local(), now.Local()
	ry, rm, rd := retry.Date()
	ny, nm, nd := now.Date()
	if ry == ny && rm == nm && rd == nd {
		return retry.Format("15:04:05")
	}
	return retry.Format("2006-01-02 15:04:05")
}

// maxQuotaPause bounds one window wait even when the provider reports a
// distant reset: the pause re-arms once per probe, so a weekly window costs
// at most one rejected invocation per day instead of parking the run.
const maxQuotaPause = 24 * time.Hour

// providerPause is one agent profile's closed quota window. The map lives in
// engine memory, but the held task itself persists everything a restart
// needs: block_reason code quota_window names the closed window, the blocked
// attempt names the agent profile, and unblock_when records the retry time.
// Startup re-arms the pause from that record (restoreQuotaPauses, T146), so
// the recorded promise survives a restart; work the pause does not hold
// still rediscovers a closed window from the provider's next failure.
type providerPause struct {
	profile     string
	retryAt     time.Time // zero between windows
	resetText   string    // the provider's reset wording, timestamps as reported
	consecutive int       // windows paused without an integration between them
}

// pauseOnQuota records one timed quota pause when err is a quota failure and
// reports whether it did. Call sites then skip their ordinary failure
// handling and let the loop gate on the pause, so a closed window never ends
// the run and never burns an execution retry.
func (e *Engine) pauseOnQuota(err error, profile string) bool {
	var agent *AgentError
	if !errors.As(err, &agent) || agent.Kind != failureQuota {
		return false
	}
	now := time.Now()
	retry := agent.Retry
	if retry.IsZero() || !retry.After(now) {
		// The provider supplied no reset, or it already passed — a reported
		// clock inside the past tolerance reaches this branch instead of
		// rolling to tomorrow (T145). The operator's rate-limit delay is
		// the existing spending policy for waiting out a provider.
		delay := e.Config.RetryDelay
		if delay <= 0 {
			delay = 30 * time.Second
		}
		retry = now.Add(delay)
	}
	if wait := retry.Sub(now); wait > maxQuotaPause {
		retry = now.Add(maxQuotaPause)
	}
	if e.quotaPauses == nil {
		e.quotaPauses = map[string]*providerPause{}
	}
	p := e.quotaPauses[profile]
	if p != nil && p.retryAt.After(now) {
		// The window is already closed. A later reported reset extends it,
		// and one window counts once toward the pause budget.
		if retry.After(p.retryAt) {
			p.retryAt = retry
		}
		return true
	}
	consecutive := 0
	if p != nil {
		consecutive = p.consecutive
	}
	if e.State != nil && e.State.Integrated != e.quotaProgress {
		// Work integrated since a pause was recorded: the window had
		// reopened, so this closure starts a fresh budget cycle.
		e.quotaProgress = e.State.Integrated
		for _, other := range e.quotaPauses {
			other.consecutive = 0
		}
		consecutive = 0
	}
	if p == nil {
		p = &providerPause{profile: profile}
		e.quotaPauses[profile] = p
	}
	p.retryAt = retry
	p.resetText = quotaResetText(agent)
	p.consecutive = consecutive + 1
	e.eventData("quota", fmt.Sprintf(
		"Provider %s quota window closed (%s). Work on it is retained and retries automatically at %s.",
		profile, firstLine(p.resetText), quotaRetryClock(retry, now)),
		e.identity().merge(map[string]any{
			"profile":    profile,
			"retry_at":   retry.UTC().Format(time.RFC3339),
			"reset_text": p.resetText,
			"windows":    p.consecutive,
		}))
	return true
}

// expireQuotaPauses resumes providers whose quota window reopened. It runs
// once at each loop boundary, before any gate reads the pauses: dispatch,
// planning, review, and queued imports all observe the same deadline. A held
// backlog imports its edit first; the resume mutates the exported task file
// and must not collide with a pending external edit.
func (e *Engine) expireQuotaPauses(held bool) error {
	if held {
		return nil
	}
	now := time.Now()
	for profile, p := range e.quotaPauses {
		if p.retryAt.IsZero() || now.Before(p.retryAt) {
			continue
		}
		p.retryAt = time.Time{}
		if p.consecutive > e.Config.RetryLimit {
			// The operator's pause budget is spent. The quota-held work
			// stays blocked with the recorded reset text; an operator
			// resumes it by rerunning Looper after the window or raising
			// LOOPER_RATE_LIMIT_MAX.
			e.eventData("quota", fmt.Sprintf(
				"Provider %s quota window reopened, but the pause budget of %d window(s) is spent; quota-held work stays blocked for an operator.",
				profile, e.Config.RetryLimit),
				e.identity().merge(map[string]any{"profile": profile, "budget_spent": true}))
			continue
		}
		if err := e.resumeQuotaTasks(profile); err != nil {
			return err
		}
		e.eventData("quota-resume", fmt.Sprintf(
			"Provider %s quota window reopened; paused dispatch, planning, and review resume.", profile),
			e.identity().merge(map[string]any{"profile": profile}))
	}
	return nil
}

// quotaPaused reports whether one agent profile's quota window is still
// closed. The predicate is pure: expiry and the resume transition happen in
// expireQuotaPauses at the loop boundary. A pause whose deadline just passed
// stays closed until that boundary consumes it — a gate reading the clock
// directly would open mid-iteration, before the resume journal and the
// requeue land, so a restarted review could race ahead of its own
// quota-resume event (T136).
func (e *Engine) quotaPaused(profile string) bool {
	p := e.quotaPauses[profile]
	return p != nil && !p.retryAt.IsZero()
}

// quotaHeldProfiles names every agent profile whose closed window holds
// blocked quota work at this boundary, including a profile the current
// configuration no longer selects: its recorded promise still governs the
// work it holds. Sorted, so the wait message names profiles in a stable
// order.
func (e *Engine) quotaHeldProfiles() []string {
	var profiles []string
	for profile, p := range e.quotaPauses {
		if p.retryAt.IsZero() || len(e.quotaHeldAttempts(profile)) == 0 {
			continue
		}
		profiles = append(profiles, profile)
	}
	sort.Strings(profiles)
	return profiles
}

// quotaHoldsWork reports whether a closed quota window is the barrier at
// this boundary: dispatch waits for the worker profile's window, and the
// milestone or final review and a missing plan wait for the reviewer's.
// Blocked quota work alone holds the boundary too (T146): a held task is
// never dispatched, so without this clause the controller would idle at the
// generic waiting-for-changes line while quota-held work remains.
func (e *Engine) quotaHoldsWork() bool {
	if e.quotaPaused(e.Config.Agent) && len(e.State.Backlog.Ready()) > 0 {
		return true
	}
	if len(e.quotaHeldProfiles()) > 0 {
		return true
	}
	return e.quotaPaused(e.Config.ReviewAgent) &&
		(e.reviewDue() || e.State.ReviewedTip != e.State.Tip || e.State.Plan == nil || e.State.PlanSignature == "")
}

// quotaWaitMessage names every closed window and its retry time, for the
// boundary wait that sleeps the poll interval instead of spinning.
func (e *Engine) quotaWaitMessage() string {
	profiles := []string{e.Config.Agent}
	if e.Config.ReviewAgent != e.Config.Agent {
		profiles = append(profiles, e.Config.ReviewAgent)
	}
	for _, profile := range e.quotaHeldProfiles() {
		if profile != e.Config.Agent && profile != e.Config.ReviewAgent {
			profiles = append(profiles, profile)
		}
	}
	var parts []string
	for _, profile := range profiles {
		p := e.quotaPauses[profile]
		if p == nil || p.retryAt.IsZero() {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s retries at %s", profile, quotaRetryClock(p.retryAt, time.Now())))
	}
	return "Provider quota window closed; " + strings.Join(parts, ", ") + ". Work is retained and resumes automatically."
}

// quotaHeldAttempts returns the blocked attempts one agent profile's closed
// quota window holds. Control tags stay authoritative gates: a closed window
// never reopens an operator or deferral hold.
func (e *Engine) quotaHeldAttempts(profile string) []*Attempt {
	var held []*Attempt
	for _, a := range e.State.Attempts {
		if a.Agent != profile || a.Phase != "blocked" {
			continue
		}
		t := e.State.Backlog.Task(a.Task.ID)
		if t == nil || t.Status != "blocked" || t.BlockReason == nil || t.BlockReason.Code != blockCodeQuotaWindow {
			continue
		}
		if t.Deferred() || t.OperatorOnly() {
			continue
		}
		held = append(held, a)
	}
	return held
}

// requeueQuotaHeld applies the requeue transition to every task one
// provider's closed window holds and returns their IDs. Only state fields
// outside Contract() change, mirroring an accepted requeue, so retained
// attempts keep their acceptance identity and dispatch resumes their partial
// implementation.
func (e *Engine) requeueQuotaHeld(profile string) ([]string, error) {
	var resumed []string
	for _, a := range e.quotaHeldAttempts(profile) {
		t := e.State.Backlog.Task(a.Task.ID)
		t.Status = "todo"
		t.Blockers = nil
		t.BlockReason = nil
		t.UnblockWhen = ""
		t.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		resumed = append(resumed, t.ID)
	}
	if len(resumed) == 0 {
		return nil, nil
	}
	e.State.Finished = false
	// Keep the live plan for running work; the cleared signature rebuilds it
	// before the next dispatch reads a stale scope.
	e.State.PlanSignature = ""
	if err := e.export(); err != nil {
		return nil, err
	}
	return resumed, nil
}

// resumeQuotaTasks requeues the tasks one provider's closed window held, at
// the boundary the window reopened.
func (e *Engine) resumeQuotaTasks(profile string) error {
	resumed, err := e.requeueQuotaHeld(profile)
	if err != nil {
		return err
	}
	for _, id := range resumed {
		e.taskIDEvent("quota-resume", id+" requeued; the provider quota window reopened and dispatch resumes its retained work.", id)
	}
	return nil
}

// quotaUnblockCue is the fixed prefix holdQuota writes before the retry
// time it records in one quota-held task's unblock_when promise.
const quotaUnblockCue = "Looper retries this task automatically at "

// quotaRetryFromUnblock parses the retry time one quota-held task's
// unblock_when promise records. The second return value reports whether the
// promise names a time at all; a hold whose provider reported no reset
// promises only "when the provider window reopens".
func quotaRetryFromUnblock(unblockWhen string) (time.Time, bool) {
	i := strings.Index(unblockWhen, quotaUnblockCue)
	if i < 0 {
		return time.Time{}, false
	}
	stamp := unblockWhen[i+len(quotaUnblockCue):]
	if j := strings.IndexAny(stamp, " \t\r\n"); j >= 0 {
		stamp = stamp[:j]
	}
	// The promise ends with a sentence period; an RFC3339 stamp never does.
	stamp = strings.TrimSuffix(stamp, ".")
	retry, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return time.Time{}, false
	}
	return retry, true
}

// restoreQuotaPauses re-arms the session's quota pauses from work the saved
// state already holds (T146). The pause map is engine memory, but the held
// task carries the whole promise on disk: the typed block reason names the
// closed window, the blocked attempt names the agent profile, and
// unblock_when records the retry time. Startup reads that record, so the
// promise "Looper retries this task automatically at <time>" survives a
// restart made mid-window and the first loop boundary after the recorded
// time resumes the work. Without it a blocked task is never dispatched, no
// provider failure can re-arm the pause, and the controller idles at the
// generic waiting line while quota-held work remains.
func (e *Engine) restoreQuotaPauses() error {
	if e.State == nil || e.quotaRestored {
		return nil
	}
	now := time.Now()
	// Candidate profiles come from the blocked attempts; quotaHeldAttempts
	// applies the same hold filter the resume transition uses.
	var order []string
	seen := map[string]bool{}
	for _, a := range e.State.Attempts {
		if a.Phase == "blocked" && !seen[a.Agent] {
			seen[a.Agent] = true
			order = append(order, a.Agent)
		}
	}
	restored := false
	for _, profile := range order {
		attempts := e.quotaHeldAttempts(profile)
		if len(attempts) == 0 {
			continue
		}
		restored = true
		var retry time.Time  // latest time any held task's promise records
		var resetText string // the provider's reset wording, first blocker
		for _, a := range attempts {
			t := e.State.Backlog.Task(a.Task.ID)
			if r, ok := quotaRetryFromUnblock(t.UnblockWhen); ok && r.After(retry) {
				retry = r
			}
			if resetText == "" && len(t.Blockers) > 0 {
				resetText = t.Blockers[0]
			}
		}
		if retry.IsZero() {
			// No held task records a time: the saved state cannot say whether
			// the window is still closed. Requeue now; a still-closed window
			// re-arms from the provider's next failure, the same discovery a
			// dispatched task has always made.
			if _, err := e.requeueQuotaHeld(profile); err != nil {
				return err
			}
			e.eventData("quota-restore", fmt.Sprintf(
				"Provider %s quota hold found after the restart with no recorded retry time; its held task(s) are requeued and a still-closed window re-arms from the provider's next failure.",
				profile),
				e.identity().merge(map[string]any{"profile": profile, "requeued": true}))
			continue
		}
		capped := retry
		if wait := capped.Sub(now); wait > maxQuotaPause {
			// The same daily cap a recorded pause applies: the restored pause
			// probes a distant window instead of parking the run until it.
			capped = now.Add(maxQuotaPause)
		}
		if e.quotaPauses == nil {
			e.quotaPauses = map[string]*providerPause{}
		}
		e.quotaPauses[profile] = &providerPause{
			profile:     profile,
			retryAt:     capped,
			resetText:   resetText,
			consecutive: 1,
		}
		e.eventData("quota-restore", fmt.Sprintf(
			"Provider %s quota window restored from the saved state after the restart; held work retries automatically at %s.",
			profile, quotaRetryClock(capped, now)),
			e.identity().merge(map[string]any{
				"profile":  profile,
				"retry_at": capped.UTC().Format(time.RFC3339),
				"windows":  1,
			}))
	}
	if restored {
		// A restart starts a fresh pause-budget cycle: the previous session's
		// window count lived only in its memory, and the integration counter
		// is the same baseline pauseOnQuota records for a new cycle.
		e.quotaProgress = e.State.Integrated
	}
	e.quotaRestored = true
	return nil
}

// holdQuota blocks one attempt whose provider window closed. The task keeps
// the provider's reset text and a typed quota reason; the controller requeues
// it when the window reopens or, past the pause budget, leaves it for an
// operator.
func (e *Engine) holdQuota(a *Attempt, err *AgentError) error {
	evidence := quotaResetText(err)
	a.Phase = "blocked"
	a.Feedback = evidence
	resume := "when the provider window reopens"
	if !err.Retry.IsZero() {
		resume = "at " + err.Retry.Local().Format(time.RFC3339)
	}
	a.Summary = &Summary{
		TaskID: a.Task.ID, Status: "blocked",
		Summary:     "Provider quota window closed; work is retained.",
		Files:       []string{},
		Blockers:    []string{evidence},
		UnblockWhen: "Looper retries this task automatically " + resume + ".",
		BlockReason: failureBlockReason(blockCodeQuotaWindow, blockRetryYes, a.ID),
	}
	return e.applyResult(a)
}

// quotaResetText bounds the provider's own wording a quota error carries,
// keeping the timestamp exactly as reported.
func quotaResetText(err *AgentError) string {
	text := ""
	if err.Summary != nil {
		text = err.Summary.Cause
	}
	if text == "" {
		text = err.Detail
	}
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	return clampString(scrubSecrets(sanitize(text)), maxFailureCause)
}

// errBootstrapDrained reports a drain request that ended the startup
// bootstrap wait. Run completes it as a clean drain exit: bootstrap created
// no task file, attempts, or baseline, so nothing is retained (T141).
var errBootstrapDrained = errors.New("startup bootstrap drained by request")

// errBootstrapSupplied reports a task file the operator supplied while a
// closed quota window held the startup bootstrap. Bootstrap exists to
// create that file, so the operator's bytes end the wait instead of
// waiting out the window to be overwritten by the retry (T141).
var errBootstrapSupplied = errors.New("task file supplied during startup bootstrap")

// adoptSuppliedTaskFile ends a startup bootstrap whose work the operator
// superseded by supplying the task file bootstrap exists to create: during
// the quota wait (T141) and during the bootstrap agent call itself (T144),
// whose minutes-long window kept the historical overwrite. The operator's
// bytes stay on disk and the startup parse below the bootstrap branch
// validates them like any startup input. importID names the staging record
// of captured startup plans, when any exist; removing it routes those plans
// through the ordinary startup import, which analyzes them against the
// operator's backlog instead of recording a bootstrap import for a file the
// bootstrap never produced. during names the window for the journal.
func (e *Engine) adoptSuppliedTaskFile(importID, during string) error {
	e.event("bootstrap", "The task file appeared "+during+"; startup adopts the operator's file and skips bootstrap.")
	if importID != "" {
		os.Remove(stagingPath(e.Dir, importID))
	}
	return nil
}

// bootstrapAndWait runs the startup bootstrap and waits out a closed
// reviewer quota window instead of ending the start (T141). Bootstrap runs
// before the controller loop exists, so no loop boundary can hold a pause
// for it: this retry loop owns the wait. Each closure records the same
// provider pause the loop records, and the operator's pause budget
// (LOOPER_RATE_LIMIT_MAX) bounds the windows probed. Past the budget the
// start still ends with the provider's reset text and recovery action in
// the error — the limit T125 recorded — because an operator rerunning
// after the window remains the manual path. importID names the staging
// record of captured startup plans, when any exist.
func (e *Engine) bootstrapAndWait(ctx context.Context, plans []capturedPlan, importID string) error {
	for {
		err := e.bootstrap(ctx, plans, importID)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !e.pauseOnQuota(err, e.Config.ReviewAgent) {
			return err
		}
		p := e.quotaPauses[e.Config.ReviewAgent]
		if p.consecutive > e.Config.RetryLimit {
			// The budget mirrors the loop boundary: past it the operator
			// reruns after the window instead of the controller probing it.
			e.eventData("quota", fmt.Sprintf(
				"Provider %s quota window closed, but the pause budget of %d window(s) is spent; the start ends for an operator. Rerun Looper after the window reopens.",
				e.Config.ReviewAgent, e.Config.RetryLimit),
				e.identity().merge(map[string]any{"profile": e.Config.ReviewAgent, "budget_spent": true}))
			return fmt.Errorf("startup bootstrap exhausted the quota pause budget of %d window(s); rerun Looper after the window reopens: %w", e.Config.RetryLimit, err)
		}
		switch err = e.waitBootstrapQuota(ctx, p); {
		case err == nil:
			// The window reopened; bootstrap retries.
		case errors.Is(err, errBootstrapSupplied):
			// The wait saw the operator's file appear; the shared adoption
			// ends the bootstrap the same way the agent-call guard does.
			return e.adoptSuppliedTaskFile(importID, "during the quota wait")
		default:
			return err
		}
	}
}

// waitBootstrapQuota sleeps out one closed quota window during startup
// bootstrap. The controller loop does not exist yet, so the wait itself
// watches the three ways an operator can still end it: cancellation
// (SIGINT) stops the start at once, a drain request (`looper stop`) ends it
// cleanly like any drain, and a hand-written task file supersedes the
// bootstrap the wait was holding. The pause is consumed here rather than at
// a loop boundary: no gate can read it before the loop starts, so the
// expiry, its journal note, and the bootstrap retry stay ordered on this
// goroutine alone.
func (e *Engine) waitBootstrapQuota(ctx context.Context, p *providerPause) error {
	e.event("waiting", fmt.Sprintf(
		"Provider quota window closed during startup bootstrap; the start waits and retries at %s. Nothing is retained; looper stop ends the start cleanly.",
		quotaRetryClock(p.retryAt, time.Now())))
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.drainingNow() {
			return errBootstrapDrained
		}
		if _, statErr := os.Stat(e.Config.Todo); statErr == nil {
			return errBootstrapSupplied
		}
		remaining := time.Until(p.retryAt)
		if remaining <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(min(remaining, e.bootstrapQuotaSlice())):
		}
	}
	p.retryAt = time.Time{}
	e.eventData("quota-resume", fmt.Sprintf(
		"Provider %s quota window reopened; startup bootstrap retries.", p.profile),
		e.identity().merge(map[string]any{"profile": p.profile}))
	return nil
}

// bootstrapQuotaSlice bounds one drain-check slice of the bootstrap quota
// wait. The poll interval keeps a stop request answered promptly without
// spinning the wait.
func (e *Engine) bootstrapQuotaSlice() time.Duration {
	if e.Config.Poll <= 0 {
		return time.Second
	}
	return e.Config.Poll
}
