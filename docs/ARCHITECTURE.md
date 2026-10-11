# Controller design

The Go controller owns scheduling and durable state. Coding agents own proposed
changes in detached worktrees. Native agent commands handle model interaction.
The controller has no provider SDK dependency. Measuretwice is an optional process.

## Ownership

One goroutine owns the state machine and SQLite writes. Worker goroutines return
immutable results. Check goroutines return candidates. Git worktrees and private
resource names isolate concurrent execution. A Git common-directory lock prevents
another controller from operating on linked worktrees.

SQLite contains a versioned state document plus append-only event and check tables.
The state document keeps the goal, backlog, attempts, plan, and publication journal
together. The database uses write-ahead logging and full synchronous commits.
Artifacts contain prompts, invocation settings, raw logs, summaries, and reports.

## Acceptance invariant

A task becomes done only after these conditions hold:

1. Its summary identifies the assigned task and has a valid result shape.
2. Its actual changes are captured in a private Git commit.
3. A candidate combines that commit with an integration base.
4. Setup and executable checks pass without changing the candidate tree.
5. The candidate base still equals the current integration tip.
6. The user's branch, task contract, and working tree still match the journal.
7. The exact candidate tree is published, then its task status is exported.

The controller leaves the user's index and branch untouched. A private temporary
index includes staged, unstaged, untracked, and ignored-but-staged baseline files.
Control files and runtime artifacts are excluded from code snapshots.

## Recovery boundaries

Task export saves its expected previous hash before replacing the task file. A
restart accepts the complete old or new file, then finishes the export. An outside
edit matches neither and stops recovery with its contents preserved.

Publication saves the complete before tree, candidate tree, and candidate commit
before applying changes. A restart sees the before tree and applies it, or sees
the after tree and completes status application. A third tree requires inspection.
Git and SQLite do not form one transaction; the journal resolves these boundaries.

Workers save completion receipts before delivering results. A matching receipt
lets restart skip implementation and continue with checks. Interrupted workers
without a receipt resume in their retained worktree. Invalid reports get one
report-only recovery session, with snapshots before and after that session.

Agent processes run beneath a small helper in a separate process group. A pipe
connects the helper to the controller lifetime. Closing that pipe kills the group,
including after a controller hard kill. Normal cancellation also kills the group.
Processes that deliberately detach from that group are outside this guarantee.

## Scheduling and convergence

Ready tasks are ordered by existing `doing` status, priority, downstream dependency
length, and natural task ID. Predicted write scopes and resources exclude known
conflicts. Missing scopes are exclusive. Completed integrations unblock dependent
tasks without waiting for unrelated workers.

Checks can overlap on a speculative integration chain. Failed or obsolete bases
cannot publish. Their successors are recombined and checked against the accepted
tip. Repeated repairs reduce implementation concurrency; successful integrations
restore it gradually.

Identical failures trigger a fresh diagnosis, then hold the task if the failure
persists. Blocked attempts retain their worktree. Explicit reopening reuses partial
work, combining it with the current baseline when necessary. Other tasks continue.

Milestone reviews drain existing work before examining complete product behavior.
Final reviews can replenish the backlog. Findings require a title, acceptance
details, evidence, and known dependencies. Duplicate titles are suppressed. Semantic
deduplication and scope judgment remain reviewer responsibilities.

Measuretwice reports are advisory evidence. A fail or review requests independent
reproduction during product review. Neither provider availability nor an
unvalidated semantic score substitutes for executable acceptance checks.

## Limits

The controller protects its own publication operations. It does not sandbox an
agent against arbitrary filesystem access or shared Git metadata changes. It does
not prove that a test suite covers every requirement. Submodules, Windows, automatic
log retention, and remote worker execution are outside this implementation.

A finite run reports completion only when final review reports the goal satisfied
and every required task is done. The default continuous run stays idle afterward
and reads new requirements. Blocked tasks require explicit reopening after their
recorded condition is resolved; they are not retried on a timer.
