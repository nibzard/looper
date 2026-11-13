# Tasks and checks

```json
{
  "schema_version": 1,
  "goal": "Implement the workflows in PROJECT.md",
  "source_files": ["PROJECT.md"],
  "tasks": [
    {
      "id": "T001",
      "title": "Implement the first complete workflow",
      "priority": 1,
      "status": "todo",
      "details": "Describe observable acceptance criteria here.",
      "depends_on": []
    }
  ]
}
```

Statuses are `todo`, `doing`, `blocked`, and `done`. Priorities range from 1
to 5. Looper rejects dependency cycles and unknown fields instead of
dropping them. Task fields also support `steps`, `files`, `blockers`, `tags`,
`created_at`, `updated_at`, and `unblock_when`.

The planning agent finds project commands. You can supply known commands:

```sh
export LOOPER_SETUP_COMMAND='npm ci'
export LOOPER_VERIFY_COMMAND='npm test && npm run build'
looper
```

Setup runs in each implementation and verification worktree. Setup and
checks must leave tracked and unignored project files unchanged. Workers
must include ignore rules and remove tracked build artifacts in their
implementation. Looper checks product behavior in a fresh integrated
worktree. Checks must not depend on Looper-private refs or worker commit
history.

Setup, verification, and assessment command bodies run from private script
files beside their logs. Bash loads each script in a login shell with no
positional arguments. Command bodies do not appear in process arguments.
The script filename appears in `BASH_SOURCE`; a top-level `return` ends the
script. Use `exit` when you need to end the shell.

Looper checks arguments and environment sizes before launch. A launch failure
identifies the recorded inputs and stops with retained work. It does not use
implementation repair cycles. An agent that starts and exits unsuccessfully
uses the existing failure handling.

Reviews run after five integrations by default. They compare the product
against requirements, exercise workflows, and report missing capabilities.
Worker reports can also propose follow-up tasks with acceptance criteria and
observed evidence. These findings must stay within the project goal. Agents
can miss defects or report problems that do not exist. Use executable
acceptance checks where you can.

Follow-ups identify an `owner` (`agent` or `operator`), a stable outcome
`key`, and an `existing_task_id` (empty for new work). Reuse the existing
task ID or key when the same prerequisite appears under a different title.
Looper records new evidence in the task-linked event journal without reopening
completed work or changing an active worker's acceptance criteria.
Keys persist as `looper:finding:<key>` tags in the existing backlog format.

Operator follow-ups enter the backlog as blocked with a `looper:operator`
tag. They stay outside the worker queue even if a generic retry sets them
to `todo`. Legacy tasks that explicitly assign an action to an operator
also stay outside the queue. After the operator completes the action,
record the evidence and mark it done. To delegate different work, change
the task contract and remove the operator tag. Follow-ups do not grant
permission to act outside the original task's authority.

When an external prerequisite is missing, a blocked task records the reason
and an unblock condition. Resolve it, then change the task to `todo` while
Looper is idle. Looper reuses its partial work. It does not repeatedly
dispatch blocked tasks without that change. Editing a retained task's
requirements starts a fresh attempt.


## Resources for setup and checks

Each operation receives a unique `LOOPER_RESOURCE_PREFIX`. Its child
processes receive a private `TMPDIR` and `LOOPER_ARTIFACT_DIR`. Use these
for databases and other test resources.
`TMPDIR` is a short directory under `/tmp`, with mode 0700, private to
each child process. Looper removes it after the process exits or is
cancelled. Store retained evidence in `LOOPER_ARTIFACT_DIR`; do not use
`TMPDIR` to pass files between setup, workers, and checks. An abrupt
controller termination can leave temporary directories behind.

Setup receives `LOOPER_RESOURCE_CLEANUP_FILE`, a private path for a Bash cleanup
script. Write the script before you create external resources. Looper runs it
after the worker or check finishes, including setup and verification failures.
Make cleanup idempotent and check ownership before you remove a resource. Use
labels and container IDs for Docker resources; do not remove resources by a
shared name alone. Remove private anonymous volumes with their containers.
Use `io.looper.resource-prefix` and `io.looper.artifact` labels with the values
of `LOOPER_RESOURCE_PREFIX` and `LOOPER_ARTIFACT_DIR`. Preserve cleanup that
setup already registers when you add more resources.

Looper records each operation's owner, prefix, and cleanup status in
`LOOPER_ARTIFACT_DIR/resources.json`. Cancellation retains resources for recovery.
After retained work finishes, Looper retries its cleanup. Restart also retries
failed cleanup and cleans abandoned resources for completed work. Cleanup has
a 30-second limit and a separate `cleanup.log`. A cleanup failure preserves the
task result, emits a `resource-cleanup-error` event, and journals a bounded
`failure` summary with the script's exit status, its own log excerpt, and the
retained evidence; a cleanup that failed only on already-absent paths reports
the outcome `cleanup_already_absent`, so it never reads as a failure of the
cleanup logic itself. The wrapper runs the script under `set -e`; the
documented check-before-remove idiom `[ -d path ] && rm -rf path` survives a
clean path when commands follow it, but a script whose final command is that
idiom reports its own status 1 with nothing left to remove, and plain Bash
does the same without the wrapper. Projects without a registered cleanup
script keep their existing resource behavior.

Let the operating system choose available server ports. Worktrees share Git
metadata; they are workspace isolation, not a security sandbox.
