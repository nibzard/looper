# Controller design

The Go controller owns scheduling and durable state. Coding agents own proposed
changes in detached worktrees. Native agent commands handle model interaction.
The controller has no provider SDK dependency. Measuretwice is an optional process.

## Ownership

One goroutine owns the state machine and its SQLite writes. Worker goroutines return
immutable results. Check goroutines return candidates. Git worktrees and private
resource names isolate concurrent execution. A Git common-directory lock prevents
another controller from operating on linked worktrees.

SQLite contains a versioned state document plus append-only event and check tables.
The state document keeps the goal, backlog, attempts, plan, and publication journal
together. The database uses write-ahead logging and full synchronous commits.
Artifacts contain prompts, invocation settings, raw logs, summaries, and reports.

The `sessions` and `processes` tables store runtime intervals separately from
task state. The process supervisor records starts and ends on the same serialized
database connection. A context carries the recorder to agents and commands;
there is no global recorder. The controller closes its session after workers stop.
Restart marks unfinished prior intervals as interrupted without inventing an end.

Timing reports sum process intervals for combined worker time and merge overlapping
intervals for active time. Controller time includes idle waits. Read-only reports
check process identities before extending open intervals to the observation time.
They report missing history explicitly. Startup imports existing Looper process
metadata by artifact path and start time, so retries remain separate and repeated
imports do not duplicate durations.

Repository build commands stamp the Git revision and modified-source state
explicitly. Direct Go builds use automatic VCS metadata as a fallback.
Controller metadata exposes this identity to observers. The session start and a
structured `session` event commit together, retaining build identity without
changing the timing table schema. Source archives report an unknown revision.

Review supervision uses the controller goroutine for log metadata and heartbeat
writes. The agent retains its normal tools and result contract. Cancellation
waits for the agent process before workspace cleanup. Heartbeats expose paths,
byte counts, and timestamps; they do not copy transcript contents into events.

Task timing joins process records to saved attempts. Artifact paths distinguish
worker operations, integration checks, and other task operations. Repairs retain
their task identity. Category totals sum durations; active time merges overlaps.
Elapsed time includes gaps between recorded task processes. Missing attempts and
unknown process ends mark coverage as partial. Read-only stores retain their
artifact directory so status and task inspection classify the same records.

Controller shutdown commits a session end and a structured `shutdown` event in
one transaction. It records normal completion, drain, cancellation, errors,
dispatch limits, and controller panic. Recording failure leaves the session open
for recovery. Restart reports the previous exit reason and event ID. An open
previous session produces a `recovery` event with its last event ID and an
unknown cause. A controller cannot infer why an external process killed it.

Agent completion extracts public usage counters into `usage_reports`. Each
artifact and process start identifies one report, including retries. Claude's
model totals replace its aggregate totals. Codex turn totals accumulate within
the report. Reasoning tokens remain a subset of output. Cache reads and writes
have separate counters and prices. Raw text is never used to estimate tokens.

The pricing calculator uses a pinned Pydantic GenAI Prices v2 snapshot. Usage
reports retain their estimate and pricing revision. Historical date conditions
apply at the process start. A report without enough context or model information
remains unpriced. Agent-reported estimates stay separate from calculated costs.
The recorder also captures failed attempts and canceled runs with usage evidence.
An optional usage-recording error does not fail the task.

The dashboard requests statistics through an asynchronous read-only callback
every five seconds. It does not query the controller's mutable state or scan
agent transcripts. Queries use durable records for project totals, model
breakdowns, and daily buckets. Worker intervals split at UTC midnight; token
reports belong to their completion day. Refresh errors retain the last view.

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

A second pipe reports target launch success or failure. The controller does
not infer launch failure from an exit code or transcript text. A target that
starts and exits 127 remains an execution failure. Launch failures stop the
controller with retained work, without recording agent usage or consuming
implementation repairs.

Execution guards check the actual helper and target argument vectors and
environment. They count terminating bytes and pointers, query platform limits,
and reserve space for additional execution overhead. Linux uses the current
stack limit; Darwin queries `kern.argmax`. Kernel failures after this check
still use structured launch reporting.

## Agent context

Codex and Claude receive prompt bytes through standard input. Zcode retains
its argument transport as a documented compatibility exception. Shell commands
load private script files through a fixed Bash login-shell invocation. Their
bodies stay out of process arguments.

Each operation writes a durable `context/` directory and `context.json` beside
its prompt. Readable copies live under `<task-file>.context/<operation>` in the
private worktree. An ownership marker distinguishes these copies from project
files. The temporary Git index removes controller-owned context before creating
the candidate tree, including when an agent adds ignore rules for that directory.
Context content is checked before accepting the agent result.

The planner starts with an unfinished task index and references the accepted
backlog and complete applied requirements. Workers receive small task contracts
inline and retrieve relevant requirements. Large tasks use a task snapshot.
Repair evidence identifies the command, error, observed output, and full log.
Final review receives the complete requirement index, including applied imports
that added no tasks. Missing, unreadable, or changed import evidence fails the
operation; the applied receipt remains available for recovery.

`invocation.json` records the context role, prompt bytes, context bytes,
transport, prompt target, and target overruns. Initial targets are 4 KiB for
planning and 16 KiB for other operations. They are observations, not rejection
limits. Model usage remains separate because file reads also consume tokens.
The agent client retains control of session compaction.

`TestContextLiveComparison` is an opt-in comparison with real agents. It uses
the same transport for inline and file context and grades product outcomes
independently. Planning, repair, and final review each run with two repeats per
arm. Raw logs, context, results, elapsed time, and usage remain available for
inspection. Stub tests cover controller boundaries; they do not establish model
behavior.

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
Final reviews report gaps as findings and set completion explicitly; the
review response is the only path that adds work or finishes a goal. Findings
require a title, acceptance
details, evidence, and known dependencies. Duplicate titles are suppressed. Semantic
deduplication and scope judgment remain reviewer responsibilities.
The private task file belongs to the controller: a review that edits it,
including a legacy project-done marker, is rejected before any finding or
completion applies. Historical markers still load and stay excluded from
scheduling, but no marker authorizes completion.

Measuretwice reports are advisory evidence. A fail or review requests independent
reproduction during product review. Neither provider availability nor an
unvalidated semantic score substitutes for executable acceptance checks.
Each assessment command runs under a fixed 60-second controller deadline
derived from the run context. A deadline or nonzero exit records an advisory
error with the command and log location; the verified candidate still
publishes. An assessment that changes project files cannot publish, and
controller cancellation during an assessment discards the check rather than
publishing from it.

## Limits

The controller protects its own publication operations. It does not sandbox an
agent against arbitrary filesystem access or shared Git metadata changes. It does
not prove that a test suite covers every requirement. Submodules, Windows, automatic
log retention, and remote worker execution are outside this implementation.

A finite run reports completion only when final review reports the goal satisfied,
every required task is done, and no accepted finding remains open. The default
continuous run stays idle afterward
and reads new requirements. Blocked tasks require explicit reopening after their
recorded condition is resolved; they are not retried on a timer.

An idle continuous run treats an invalid task file as recoverable input rather
than a failure. The last accepted state and the user's file are preserved while
planning, review, dispatch, and backlog writes pause. The wait reports the file,
the problem, and the correction action when the problem appears or changes, and
any valid edit resumes the loop within one poll interval. Startup, finite runs,
and every non-input failure keep their fatal behavior.
The controller publishes the waiting snapshot before it sleeps and clears it
as soon as the input is accepted. The dashboard shows the correction action
and a scrollable diagnostic even when the accepted backlog has ready tasks.
