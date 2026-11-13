---
name: looper-backlog
description: Maintain Looper backlogs or interpret Looper task summaries. Use for Looper task workflows, not arbitrary JSON task lists.
---

# Maintain a Looper backlog

Use the project's schema and the active phase prompt. Looper selects tasks,
owns execution status, and validates agent results. During task execution,
complete the selected task without editing the backlog. Return the summary
fields requested by the active prompt; report missing prerequisites with a
concrete unblock condition.

## Change work through requests, not file edits, while a run is live

A controller that owns the repository answers the request commands, and every
one of them returns a durable receipt at once:

- New requirements become tasks through `looper add-plan`, new task objects
  through `looper add-task`, and both take `--idempotency-key`.
- Withdrawing open work is `looper cancel`, which checks the revision you
  read and refuses live dependents until `--cascade` is explicit.
- Changing one contract is `looper amend <task-id> <task.json>`. Work that
  holds the task is refused with its boundary named, and a done task is never
  rewritten: the changed requirements create a follow-up task.
- Read an outcome from the receipt, not the file: `looper requests <id>
  --contract v2 --json`, or one bounded wait in the same call with `--wait`.

Cancelled work carries the `cancelled` status, never `done`. A cancelled
dependency never satisfies a dependent, so Looper refuses a cancellation that
would strand one.

A minimal add-task file holds one task object or an array of them:

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

`status` defaults to `todo`. Use unique IDs and priorities from 1 through 5.
An invalid batch adds nothing.

## Edit the file only when asked, or when no run is live

For requested backlog maintenance, preserve unrelated tasks and metadata.
Keep dependencies valid and acceptance criteria observable. Keep blocked
tasks blocked until their cause is resolved. Validate edits before replacing
the task file. Do not change a live run's backlog merely to diagnose it.

A task-file edit left beside retained work is not silently lost: the run
holds it, the guard names the expected and observed digests and the retained
tasks, and `looper recovery --preview --contract v2 --json` lists the
supported choices with what each preserves or changes. Prefer the preview's
checked application over manual restore.

Read relevant project requirements before changing scope. In `source_files`,
list project requirement files that remain available to later worktrees.
Looper's private context, including `to-do.json.context/` snapshots, is
operation evidence, not a project source. Supplied plan documents are retained
by Looper separately. If an existing source points to private context, report
the invalid reference; do not invent requirements or silently discard it.

During final review, return the requested findings. Looper applies task
changes; do not edit the backlog as part of a review.
