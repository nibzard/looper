# Change the backlog during a run

## Add tasks during a run

Use `looper add-task tasks.json` to add tasks through the active controller.
The file contains one task object or an array of task objects:

```json
[
  {
    "id": "T044",
    "title": "Share storage loading checks",
    "priority": 2,
    "details": "Share bounded file reads. Preserve the independent parsers.",
    "steps": ["Run the storage tests."],
    "depends_on": []
  }
]
```

The status defaults to `todo`. You can also supply `blocked`. Use unique IDs
and priorities from 1 through 5. Dependencies can refer to existing tasks or
other tasks in the batch. The controller validates the complete batch before
adding it. Existing tasks and active attempts stay intact.

Add `--json` for a structured acknowledgement. The command returns a receipt
at once and the controller owns the outcome; query it with `looper requests
<id>`. Add `--wait` to wait for the applied acknowledgement, bounded by
`--wait-timeout` (default 30 seconds). A timeout reports the request ID; the
request can still apply later. Check `looper status` before submitting it
again. Requests belong to the current controller session. If that session
ends before application, inspect the backlog before resubmitting. An invalid
batch adds no tasks.

Pass `--idempotency-key <key>` to make a retry safe. The key is scoped to
this project and this operation: repeating the same key with the same task
batch returns the original request instead of adding the tasks twice, and a
changed batch under the same key is rejected as a conflict. Distinct keys
with identical content both apply, so intentional repeats stay possible. A
key is 1 to 128 bytes without control characters. Keys answer from the
retained request receipts: a request that is still running keeps its receipt,
and at most the newest 200 decided receipts stay per run, so a key whose
receipt was pruned behaves as fresh.

This command needs an active controller built with support for `add-task`.
Installing a new binary does not update an existing controller. Use
`looper add-plan requirements.md` when the planner should create tasks from
requirements. `add-plan` and `looper config` accept `--idempotency-key` with
the same semantics; for `add-plan` the key addresses the plan content, not
its path.

## Cancel tasks during a run

Use `looper cancel <task-id>` to withdraw open work through the active
controller. The command reads the task from your task file and sends its
revision with the request: if the task changed, completed, or left the
backlog since you read it, the controller refuses with a conflict and
changes nothing. Done work is accepted history and cannot be cancelled.

A task that other live tasks depend on is refused by name, because a
cancelled dependency never satisfies a dependent. Pass `--cascade` to cancel
the transitive live dependent set in one decision; dependents that are
already done keep their history.

When the target has work in flight, the cancellation is recorded and waits:
the receipt reports `waiting` with the reason `active_work`, the running
worker finishes first — its verified result still passes the checks and is
never discarded — and the cancellation applies at the first boundary where
the work drained. If the work completed first, the task stays done and the
request records that outcome. A restart between the decision and the drain
keeps the decision.

The command returns a receipt at once; query it with `looper requests <id>`.
Add `--wait` for one bounded wait that reports the record, and
`--idempotency-key` with the same semantics as `add-task`. Cancelled tasks
carry the `cancelled` status in the task file, never `done`: they stay in
the backlog for inspection, never re-enter the ready set, and `looper
status --contract v2` counts them under `work.counts.cancelled`. This
command needs an active controller built with support for `cancel-task`.

## Amend tasks during a run

Use `looper amend <task-id> <task.json>` to change one task contract through
the active controller. The file carries the complete amended task: title,
priority, details, steps, `depends_on`, files, blockers, tags, and
`unblock_when`. Identity, status, and `created_at` are not amendable, and the
control tags Looper owns (`project-done`, `looper:operator`, `looper:deferred`,
`looper:finding:*`) cannot be added or removed through a request.

The request carries the task revision your task file held. If the task
changed, left the backlog, or a held task-file edit is waiting for its
boundary import, the controller refuses with a conflict and changes nothing.
Dependency cycles, unknown dependencies, duplicate identifiers, and invalid
write paths are refused before anything commits.

Work that holds the task contract — running, verifying, ready, integrated, or
resume attempts — is refused with the boundary named, because a contract
cannot change under a worker. Wait for the work to finish or cancel it, then
amend the drained task. A done task is never rewritten: the changed
requirements create a follow-up task named `<task-id>-amend-<n>` while the
accepted task, its attempts, and its results stay as they were. A cancelled
task refuses and points at `looper add-task`.

The command returns a receipt at once; query it with `looper requests <id>`,
add `--wait` for one bounded wait, and pass `--idempotency-key` with the same
semantics as `add-task`. This command needs an active controller built with
support for `amend-task`. Installing a new binary does not update a running
controller.
