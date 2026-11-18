# Looper quick wins

Date: 2026-11-17. Status: proposed.

Make Looper's diagnostics easier to act on and reduce repetitive output.
Each change can ship independently. Start with the blocker warning and compact
status examples. Keep scheduling, automatic retries, and publication outside
this plan. [Delivery audit](/notes/looper-delivery-audit-2026-11-17.md)

## 1. Flag obvious circular blockers

Status: implemented by T135 — `looper doctor` reports the finding
`task_circular_condition` in text, legacy JSON, and version 2 JSON.

Add a warning to `looper doctor` for a blocked task whose resume condition
explicitly requires the same task to start, finish, or become unblocked.
Use the existing health finding format. Show the task ID, the condition,
and guidance to record an independent prerequisite or correct the task state.
Leave the existing blocker and task state intact.

Acceptance criteria:

- A task whose condition requires its own implementation to start produces a
  warning in text and version 2 JSON.
- A condition that refers to another task, or mentions the current task only
  to explain what happens after an external prerequisite, produces no warning.
- Running doctor leaves the backlog unchanged and follows the existing exit
  code policy for warnings.

[Health findings](../internal/looper/observe.go)

## 2. Report timed quota failures

Status: implemented by T125 with a wider scope than proposed here — the
controller pauses the affected provider and resumes automatically at the
recorded reset within the operator's pause budget, instead of keeping the
stop-and-retain behavior this section proposed. The bounded reporting patch
landed as T136: the classification itself returns a quota diagnostic whose
failure summary carries outcome `quota_window` and a recovery action beside
the provider's reset text, and the version 2 error document leads with that
action.

Recognize the observed provider message, which combines
`Request rejected (429)` with a usage limit and a reset time.
Report the quota failure, retain the provider's reset text, and give the
operator the relevant resume or requeue action. Keep the existing stop and
retention behavior in this patch. Preserve the timestamp as reported when the
provider does not supply a time zone.

Acceptance criteria:

- A stub agent that emits the observed message produces a quota diagnostic
  with the reset text and a recovery action.
- Invalid credentials and ordinary rate limits keep their existing behavior.
- A quota failure retains work and schedules no automatic retry.

[Agent failure classification](../internal/looper/agent.go)

## 3. Show compact status in agent examples

Status: implemented by T137 — the agent-facing surfaces teach the compact
call. `looper help status` already led with it; the routine monitoring
examples in the agent contracts, observability, and recovery pages, and the
repository agent instructions, now use `looper status --contract v2 --json`
and link to `--full` for the complete state. Legacy examples stay where
they explain compatibility.

Use `looper status --contract v2 --json` in agent-facing help, skills, and
routine monitoring examples. Keep legacy examples where they explain
compatibility. Link to `--full` for callers that need the detailed state.

Acceptance criteria:

- The common monitoring examples use compact status and show where to get
  full detail.
- An example run returns the compact status document, including the controller
  state and event cursor. Plain `looper status --json` keeps its current format.

[Agent contracts](AGENT-CONTRACTS.md)

## 4. Group repeated cleanup warnings

Status: implemented by T138 — the first failure of an artifact reports the
unchanged notice and failure summary once per controller session, identical
repeats count silently, every tenth repeat and the closing sweep journal one
`resource-cleanup-summary` event with the repeat count, and a changed error
reports immediately. Cleanup attempts and lease handling are unchanged.

On the first cleanup failure, record the artifact and error. Count matching
failures per artifact during the controller session. Emit a summary at a
bounded interval or on shutdown. Report a changed error immediately.
Keep the existing cleanup attempts and lease handling.

Acceptance criteria:

- Repeated identical failures produce one initial warning and a summary
  with the correct repeat count. Check both plain and structured failure
  events so they do not duplicate every retry.
- A different artifact or error produces its own warning.
- Cleanup still retries and a later success clears the lease as before.

[Resource cleanup](../internal/looper/resources.go)
