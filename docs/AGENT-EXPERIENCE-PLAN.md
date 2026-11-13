# Plan for the Looper agent experience

Date: 2026-11-05. Scope: implement the accepted agent experience proposal.

This document contains the requirements and acceptance criteria for AX-001
through AX-028. Workers must use this repository document; access to `/notes/`
or the original audit transcripts is not required.

The audit of 626 CLI calls found repeated observation, submission, recovery,
and discovery costs. These counts describe the audited sample only.
Source report: `/notes/looper-cli-interaction-audit-2026-11-05.md`.
Source proposal: `/notes/looper-agent-experience-proposal-2026-11-05.md`.

## Execution rules

- Preserve existing tasks, statuses, blockers, metadata, and acceptance rules.
- This plan explicitly adds scope beyond the older UI plan. Resolve its
  byte-compatible JSON requirement through versioned opt-in and migration.
  Do not silently replace an existing machine contract.
- Reuse fixes already present in the tree. Reproduce reported bugs before
  changing behavior. Test new behavior in temporary projects with stub agents.
- Each implementation task includes contextual help, schemas, focused tests,
  and working notes where its behavior needs them. AX-026 checks completeness.
- Do not install, publish, restart the active self-run, or invoke paid agents
  merely to complete these tasks. Such actions need their own requested scope.
- Use actual repository paths when implementing. Suggested task paths are
  ownership hints; a new helper file is allowed when needed.
- Keep research records in Markdown under `/notes/`, with dated sources and
  verified findings. Keep repository requirements and reproducible validation
  artifacts in the repository so later worktrees can use them.

## Existing work and dependencies

T020 owns queue overflow recovery. T029 owns restart-safe request sweeping.
AX-009 depends on both instead of creating duplicate repair tasks.
T044 owns accepted-import dispatch pause; AX-027 verifies that integration.
T058 owns startup import of a retained backlog edit; AX-017 reuses it.
T046 and T047 retain their existing independent recovery scope.
These tasks remain blocked under their recorded conditions, including T042
where applicable. This batch does not mark operator actions complete or
remove their existing gates. Report a real unmet prerequisite when reached.
Current status lives in `docs/AGENT-EXPERIENCE-OWNERSHIP.md`, which follows
the accepted backlog; some conditions named here have since been met or
completed.

AX-001 adds three artifacts that the rest of this plan works from:
`docs/AGENT-CONTRACTS.md` defines the shared query and operation contracts
and the inventory of existing consumers and in-tree fixes;
`docs/AGENT-CONTRACT-MIGRATION.md` defines the version selection, the frozen
legacy shapes, and the default transition; `docs/AGENT-EXPERIENCE-OWNERSHIP.md`
maps every task in this plan to its owner and its overlaps, preserving the
blocked statuses and operator gates above. Executable schemas and fixtures
live in `docs/contracts/`.

P2-004 already owns scoped stop requests. P2-006 already owns shallow-build
stamps. Reuse them. Detached startup already has a readiness pipe.

## Delivery sequence

1. Establish contracts, persistence correctness, consistent cursors, bounded
   observation, structured events, capabilities, and actionable errors.
2. After the existing queue prerequisites, persist request receipts, expose
   request queries, return immediately, and make retries idempotent.
3. Add finite waits, guard differences, recovery preview/application, semantic
   backlog comparison, task cancellation/amendment, and failure evidence.
4. Finish lifecycle waits, preflight, identity/install notices, run discovery,
   documentation, deterministic evaluation, and live-evaluation preparation.

The task dependency graph is authoritative within these stages. Independent
tasks can proceed when their dependencies are complete.

## Design requirements

## Recommendation

Make the CLI a complete public interface for operating Looper. An agent must
be able to identify the run, observe changes, submit work, and resolve a
refusal without reading private database tables or source code.

Use one set of queries and operations for plain output, JSON output, and the
dashboard. Share the underlying contracts, without forcing identical layouts.
Keep summaries small and provide explicit routes to detailed evidence.

## Verified current behavior

- `statusReport` embeds `*State`, plus observation, timing, and statistics.
- Observation can embed a full dashboard view. Shrinking only `State` is
  insufficient to establish a bounded response.
- The event cursor comes from the saved view, rather than a database read
  coordinated with the reported state.
- Plain status lists counts and blockers, but omits routine controller and
  active-work summaries. It reports build differences when detected.
- Events start from `c.After`, whose default is zero. They drain successive
  500-row pages. JSON output is newline-delimited JSON (NDJSON).
- JSON plan submission waits up to 60 seconds before printing. Task
  submission waits up to 30 seconds for an acknowledgement.
- Controller metadata already carries build identity. `currentBuild` already
  reads Go build metadata, with explicit build stamps taking precedence.
- Detached startup has a readiness pipe. Its success response follows
  `NewEngine`; the reported start/status race still needs reproduction.
- Scoped stop handling exists in the working tree. Notes record regression
  and live fixture checks. This review does not establish release status.

## 1. Make observation small and sufficient

The default status response should answer:

- Which project, run, controller, and build does this command observe?
- Is the controller running, starting, draining, stopped, or unknown?
- Which tasks and controller phases are active?
- What prevents scheduling, submission processing, or recovery?
- What changed most recently, and where does observation continue?

Separate controller ownership, process evidence, output activity, and task
progress. A held lock proves ownership. Recent output does not prove useful
progress. Missing evidence stays unknown.

Include counts, a bounded active-work list, important findings, pending request
counts, the last durable transition, and a continuation cursor. Include limits,
omitted counts, and detail commands when collections exceed the response budget.
Cap strings as well as arrays. Never shorten output into invalid JSON.

Target at most 8 KiB for default status, including a large backlog. Treat this
as an initial engineering budget, then validate it against useful answers.
Avoid constructing historical statistics and full state just to discard them.

Keep complete state available through explicit `--full`. Give `inspect` a
bounded current-attempt response, with paged history and evidence references.
Add field selection only after the compact response proves insufficient.

Generate plain status from the same summary. Always show controller state,
active work, and the reason for waiting, including when output is piped.

## 2. Replace repeated dumps with resumable observation

Make bare events return the latest 50 events, oldest first within that tail.
Provide `--limit`, `--task`, `--kind`, and an explicit full-history option.
Apply filters and limits in the database query.

Document `--after` as exclusive. Return a continuation position even when no
events match a filter. Scope cursors to the retained event store, detect
invalidated cursors, and preserve continuity across controller restarts.
Capture a high-water mark so a stream of new events cannot prevent a page
from completing. Read state and its cursor consistently to prevent gaps.

Add a finite `wait` operation for a task, request, run outcome, or changed
cursor. Give it an explicit timeout and a structured outcome. Most tool
harnesses benefit from finite waits more than a permanent follow process.
Keep `--follow` for callers that support streaming.

Use one JSON document for finite queries. Use an `items` array and pagination
metadata for finite events. Reserve NDJSON for explicit streaming. Introduce
the new finite-event format through a documented compatibility transition.

New task events should include task, attempt, session, phase, and request
identifiers where applicable. Historical records can report missing fields.
Do not fabricate task identities by treating message parsing as authoritative.

## 3. Make every mutation a durable request

Use the same request protocol for add-task, add-plan, settings, requeue, and
stop. Return promptly by default. Optional waits must have explicit timeouts.

Distinguish these outcomes:

- `submitted`: the request envelope is durable, but not yet accepted.
- `accepted`: the controller durably records responsibility for the request.
- `waiting`: the request names a prerequisite and its current age.
- `applied`: the change and its result are durable.
- `rejected`, `failed`, or `cancelled`: a terminal result explains the cause.

If submission can establish durable acceptance before returning, omit the
intermediate submitted state. Never call an envelope write acceptance when
recovery can still discard it without an outcome.

Expose `looper requests <id> --json`, a bounded list, and finite waiting.
Include affected task IDs, plan coverage, controller identity, and the exact
continuation command. A wait timeout does not cancel or reject the request.
Document transport exit codes separately from operation state.

Accept a caller-supplied idempotency key. Repeating the same key and payload
returns the same request. Reusing a key with different content is a conflict.
Do not deduplicate all identical task content: intentional repetitions exist.

Advertise supported request types in live controller metadata. Validate
capabilities before queueing, then validate again when the controller claims
the request. Unknown capabilities must not silently create dead letters.
Restart must resume accepted work or record an explicit terminal result.

## 4. Turn refusals into supported recovery

Extend the useful doctor contract to all errors: stable code, message,
affected paths, expected and observed values, evidence references, and an
executable next step when one exists. State whether retry can help.

For a retained-work guard, show the task file, expected and actual digests,
retained tasks, and changed project paths. Provide a read-only recovery
preview that explains which work and files each recovery action preserves.
Apply recovery through a checked controller operation that rejects a stale
preview. A general force bypass would defeat the guard's purpose.

Separate byte identity from semantic identity. Validate formatting-only edits
before accepting them as equivalent. Preserve exact hashes for immutable
artifacts and task contracts that require byte identity.

Add supported task cancellation and amendment through this request protocol.
Define dependency handling and retained-result behavior before implementation.
Use a revision precondition to prevent overwriting intervening changes.

## 5. Make discovery and execution explicit

Provide per-command help, examples, input schemas, output schemas, exit codes,
and both version forms. Invalid commands must produce an error.
Expose client and controller capabilities through a machine-readable entry.
Show resolved project and base paths on lookup errors. Provide bounded run
discovery within the configured base directory.

Before foreground execution, identify the agent executable, model, launch
mode, and the meaning of `--once`. Add a read-only preflight or dry run that
resolves this configuration without invoking an agent. It cannot promise
success for credentials or services it has not checked.

Build identity must come from the executable's build metadata. A runtime
`git rev-parse HEAD` can identify the user's project instead of Looper.
Keep unavailable identity explicit. Display client/controller differences;
warn during installation when an executable belongs to an active controller.
Installing a binary does not update that controller.

Retain the readiness handshake. Define start success as a committed run that
status can query, even if the run finishes immediately. Add a regression test
before changing startup ordering. Bind `stop --wait` to the original session;
it must never stop or wait indefinitely for a replacement controller.

Failure summaries should include exit status, operation, bounded safe context,
and a public evidence reference. Keep raw logs local and omit credentials and
private reasoning. Publish success only after the associated state commits.

## Delivery order

| Stage | Deliverable | Acceptance condition |
| --- | --- | --- |
| 1 | Compact status, piped liveness, bounded events, cursor correctness | Routine observation needs no database reads and stays within its size budget. |
| 2 | Immediate receipts, request queries, capabilities, idempotent retry | A timeout or restart cannot silently lose or duplicate accepted work. |
| 3 | Structured guard errors, failure evidence, recovery preview | The agent finds the cause and a supported next step without source reading. |
| 4 | Lifecycle waits, preflight, help, schemas, run discovery, task edits | Agents can complete common operating procedures through documented commands. |

Fix confirmed lost-state and false-success defects before exposing the new
request contract. Ship command help with each feature, rather than postponing
all documentation until stage 4. Review existing working-tree fixes first.

Introduce additive compact responses during migration. Change defaults only
with an explicit contract version and release guidance. Keep an explicit path
to the old full response. Do not silently change existing JSON shapes.

## Verification and measurement

Build fixture scenarios from the verified incidents, with stub agents and
large retained histories. Cover idle and dead controllers, bootstrap, quiet
workers, backlog holds, incompatible controllers, retries, interrupted
acceptance, and stop/start races. Assert observable outcomes, not wording.

Measure task success, output bytes, command latency, redundant observation,
unsupported retries, and private-state detours. A single command with a huge
response is not an improvement. Replay fixtures to establish the baseline;
use the same tasks and output limits when comparing the new interface.

Proposed release gates:

- Default status stays within 8 KiB on large-backlog fixtures.
- A finite events call returns no more than its configured page limit.
- Snapshot-to-event continuation misses no committed transition.
- Repeated idempotency keys never apply a mutation twice.
- Every accepted request remains queryable after restart.
- Every fixture guard refusal includes actionable evidence or states exactly
  which missing evidence prevents a safe recovery recommendation.
- Common operating fixtures need no source or private database access.

Update the diagnosis skill to teach one short workflow using these commands.
Validate with fresh agent sessions after deterministic checks. A live
evaluation that invokes real agents needs a separately requested test.
Consider a connector only after these contracts settle; it should reuse the
same operations rather than create a second implementation.


## Task contracts

### AX-001: Define versioned agent contracts and migration

Priority: 1. Dependencies: none.

Inventory existing CLI consumers, schemas, and in-tree fixes. Define shared query and operation contracts for CLI, doctor, and dashboard; JSON documents for finite queries and explicit NDJSON streaming. Specify compact/full negotiation, cursor scope, request states, transport exit codes, and compatibility fixtures. Preserve legacy JSON until an explicit version selection or documented default transition. Resolve the older UI plan byte-compatibility constraint through an explicit migration, never a silent shape change. Record overlap ownership: T020 and T029 remain request prerequisites, T044 owns import dispatch pause, T058 owns retained backlog import. Preserve their blocked status and operator conditions. Acceptance: the repository has executable schema/compatibility fixtures, a migration guide, and an ownership map covering every task in this plan; existing implemented fixes are reused.

### AX-002: Commit findings before reporting success

Priority: 1. Dependencies: AX-001.

Reproduce the audited addFinding/export-guard false-success incident with a stub agent and injected persistence failures. Stage backlog and receipt changes until the saved state/export protocol commits them. Emit Added follow-up and applied outcomes only after their corresponding changes are durable. Acceptance: failures before commit leave no success event or lost in-memory-only task; recovery after commit returns exactly one durable task and outcome. Preserve successful existing paths and existing acceptance/publication invariants. If the tree already fixes this, pin the behavior with regression coverage and record the evidence.

### AX-003: Read state and event cursors consistently

Priority: 1. Dependencies: AX-001.

Read saved state and its event high-water mark from one consistent database snapshot, including stopped runs without a view. Scope continuation cursors to the retained event store, distinguish controller sessions, and detect reset/invalid cursors. Define exclusive after semantics and a fixed high-water mark per finite page. Acceptance: concurrent commits between status and events cause no missed transition; stopped runs report the current cursor; restarts preserve valid continuation and store resets return an actionable cursor error. Do not use view.json as the authoritative event position.

### AX-004: Add bounded status and piped liveness

Priority: 1. Dependencies: AX-001, AX-003.

Implement the negotiated compact status contract and shared summary model. Include project/run identity, client/controller builds, ownership and freshness, active workers and controller phases, scheduling/wait reasons, request counts, last durable transition, findings, and cursor. Distinguish unknown progress from output activity. Bound arrays and strings, expose omitted counts and detail routes, and avoid building full historical statistics for compact reads. Plain piped status always shows liveness, in-flight work, and waiting reason. Acceptance: valid compact JSON stays at or below 8 KiB with thousands of tasks, long strings, and many attempts; full state remains explicitly accessible; stale, missing, idle, bootstrapping, draining, stopped, and quiet cases are distinguishable.

### AX-005: Bound task inspection and page attempt history

Priority: 2. Dependencies: AX-001, AX-003.

Make negotiated compact inspect return the task contract, current/latest attempt, blockers, relevant checks, timing provenance, and evidence references. Page attempt/check/log history and bound excerpts and strings. Provide explicit full/history paths; preserve the legacy contract through migration. Acceptance: a task with many repairs and large feedback returns bounded valid JSON, all history remains reachable without duplication or gaps, and active versus retained work stays explicit. Document and test the chosen response budget.

### AX-006: Make finite event queries bounded and filterable

Priority: 1. Dependencies: AX-001, AX-003.

Add newest-tail queries with default 50 events, configurable bounded limits, task/kind filters, and explicit full-history export. Query/filter/page in SQLite rather than serializing all history. In the new finite JSON contract return items, high-water mark, continuation, and has_more; keep NDJSON explicit for streaming and the legacy format available during migration. Advance filtered cursors even on empty pages. Acceptance: large journals return at most the page limit, default tails are chronological, after is exclusive, filtered continuation has no gaps, and concurrent writers cannot keep a finite request open indefinitely.

### AX-007: Attach structured identities to lifecycle events

Priority: 2. Dependencies: AX-001.

Add task_id, attempt_id, controller session, phase, and request_id to new lifecycle events wherever applicable, including findings and import/task mutations. Keep message text for people. Historical records with absent fields remain readable and explicitly incomplete; never make message parsing authoritative. Acceptance: task-filtered queries retrieve task start/check/repair/finish/failure events, request outcomes correlate with receipts, and old journals remain readable.

### AX-008: Advertise controller capabilities before submission

Priority: 1. Dependencies: AX-001.

Publish supported request types and protocol versions with live controller identity. Add machine-readable client/controller capability discovery and version output. Validate capabilities before envelope publication and again when claiming a request; return a specific incompatibility result without queueing unsupported work. Keep absent legacy metadata explicit, with a documented compatibility policy. Acceptance: newer clients against older controllers create no dead letters, stale-session races return an actionable outcome, and both version command forms appear in help.

### AX-009: Persist common request receipts across restart

Priority: 1. Dependencies: AX-001, AX-002, AX-008, T020, T029.

Implement one durable request lifecycle for tasks, plans, settings, requeue, and stop. Distinguish submitted envelopes from durable acceptance, waiting with reason/age, applied, rejected, failed, and cancelled. Commit mutation/result consistently and preserve receipts before removing envelopes. Accepted requests survive restart or receive an explicit terminal result; session-bound operations must never replay silently into another controller. Preserve immutable plan artifacts and legacy acknowledgement recovery. Acceptance: crash injection at envelope, claim, acceptance, mutation, export, receipt, and cleanup boundaries yields queryable outcomes with no silent deletion or false success. Reuse T020/T029 fixes after their recorded gates are met.

### AX-010: Expose bounded request queries and waiting reasons

Priority: 1. Dependencies: AX-009.

Add requests <id> and a bounded paginated request list using public durable receipts. Include state, age, prerequisite/wait reason, session, affected tasks, plan coverage, safe error evidence, and continuation commands. Integrate bounded pending-request summaries into status and doctor. Acceptance: pending imports and settings can be diagnosed without reading acks directories or SQLite; missing/stale IDs and stopped controllers produce precise results; large receipt histories remain bounded and all records are reachable.

### AX-011: Return immediate receipts for CLI mutations

Priority: 1. Dependencies: AX-009, AX-010.

Route add-task, add-plan, settings, requeue, and stop through the common protocol. Default to returning a durable submission/acceptance receipt promptly; make waiting explicit and bounded. Preserve useful applied task IDs, coverage, adjustments, and gates in text and JSON. Timeout is a wait outcome, never cancellation or rejection. Update dashboard controls to consume the same operations without creating a second mutation path. Acceptance: submissions produce an immediate parseable response during long planning/check phases; every returned ID is queryable; offline, held-input, draining, and incompatible-controller cases explain their outcome. Preserve old command behavior only through the explicit migration policy.

### AX-012: Make retries idempotent with caller keys

Priority: 1. Dependencies: AX-009, AX-011.

Accept caller-supplied idempotency keys scoped to the project and operation, persist their canonical payload digests and result identity, and support restart lookup. Same key and payload returns the original request/result; changed payload is a conflict. Do not globally deduplicate identical task content. Acceptance: concurrent submissions and retries after lost client output or controller restart apply one mutation; changed content under the same key is rejected; intentional same-content submissions with distinct keys remain possible. Bound key input and document receipt retention guarantees.

### AX-013: Add finite waits for changes and outcomes

Priority: 2. Dependencies: AX-003, AX-006, AX-010.

Implement wait for cursor change, task state, request outcome, and run outcome with an explicit finite timeout. Return one structured result naming the satisfied condition or timeout plus the latest cursor/receipt. Waits observe only, release resources on cancellation, and never cancel the underlying operation. Bind targets to project/store/session identity as appropriate. Acceptance: each predicate handles completion before invocation, during the wait, timeout, controller exit/restart, and client cancellation without busy loops or missed transitions; follow remains available for streaming callers.

### AX-014: Use actionable structured errors across the CLI

Priority: 1. Dependencies: AX-001.

Extend the doctor finding pattern to public CLI failures: stable code, message, affected paths, expected/observed values, retry guidance, evidence references, and supported next actions. Emit negotiated JSON errors consistently without mixed human stdout; render plain errors from the same data. Cover missing run, malformed input, unsupported capability, conflict, retention, and persistence errors. Acceptance: representative failures validate against the error schema and let callers distinguish retry, changed input, and unavailable evidence without parsing English text; sensitive arguments and private reasoning are omitted.

### AX-015: Explain retained-work guards with concrete differences

Priority: 1. Dependencies: AX-014.

For backlog, project-tree, self-hosting collision, and branch guards, report resolved paths, expected/actual digests or revisions, retained task identities, and bounded changed-path evidence with a complete-detail route. Handle unborn HEAD explicitly rather than asserting branch movement. State which evidence is unknown and which supported recovery operation to inspect. Acceptance: formatting-only, actual contract, unrelated project edit, collision, and unborn-HEAD fixtures give distinct actionable evidence while preserving every existing guard and retained result.

### AX-016: Preview recovery without changing saved work

Priority: 2. Dependencies: AX-015.

Add a public read-only recovery preview that compares current files, saved contract, retained attempts, and publication state. Enumerate supported recovery choices and what each preserves or changes; attach a revision/digest token for later application. Do not propose force bypasses or fabricated safe actions when evidence is missing. Acceptance: preview changes no files, database records, processes, or Git refs; outputs are bounded with detail routes; unavailable or unsafe cases explain the missing evidence and retain work.

### AX-017: Apply checked recovery through controller operations

Priority: 2. Dependencies: AX-009, AX-016, T058.

Implement supported recovery actions from preview through the common request protocol, with expected revision/digest checks and repository ownership. Provide the same locking discipline for stopped-run recovery without launching real agents. Reuse T058 retained-backlog import rather than duplicating it; leave unrelated recovery permissions and blocked tasks unchanged. Acceptance: stale previews reject without mutation, concurrent edits are preserved, a crash leaves a recoverable outcome, and replay-safe retained results resume without implementation repeating. Unsupported situations retain work and report a concrete limitation.

### AX-018: Recognize validated formatting-only backlog changes

Priority: 2. Dependencies: AX-015.

Define semantic backlog comparison for parsed, fully validated JSON, distinguishing whitespace/key-order/escaping changes from task contract changes. Reject duplicate keys or ambiguous input rather than silently normalizing it. Keep exact byte digests for immutable artifacts and any contract that requires them. Acceptance: equivalent formatting does not trigger a false contract refusal on supported paths, changed task/dependency data still triggers the guard, invalid input remains rejected, and original bytes/evidence remain available for recovery.

### AX-019: Cancel tasks through checked controller requests

Priority: 2. Dependencies: AX-009, AX-012, AX-014.

Define and implement supported task cancellation with expected backlog revision, dependency effects, active-worker handling, and retained-result preservation. Define whether schema/state needs a cancellation representation; do not encode cancelled work as accepted done work. Document pending versus active cancellation and expose outcomes through inspect/status/events. Acceptance: cancellation cannot silently strand dependents, discard active results, bypass publication checks, or overwrite concurrent edits; retries return the same receipt; restart retains the decision. Validate any schema migration against old backlogs.

### AX-020: Amend task contracts through revision checks

Priority: 2. Dependencies: AX-009, AX-012, AX-014, AX-019.

Add supported task amendment using expected backlog/task revision and the common request protocol. Validate dependency cycles and changed write scopes before committing. Completed work remains historical; changed requirements create follow-up work rather than rewriting accepted results. Active/retained contracts require an explicit safe boundary and cannot silently change under a worker. Acceptance: valid pending edits apply once, concurrent/stale edits conflict without loss, unsafe active edits explain the boundary, and restart preserves both history and applied amendment receipts.

### AX-021: Summarize failures with bounded public evidence

Priority: 2. Dependencies: AX-007, AX-014.

Include operation, tool/agent identity, exit status when available, bounded safe failure context, and Looper-owned evidence references in worker, verification, launcher, and resource-cleanup failures. Provide a bounded evidence retrieval path so agents do not need private session files. Audit the reported cleanup wrapper failure with a stub and fix only reproduced behavior. Acceptance: exit failures and cleanup on already-absent paths are distinguishable, useful causes are retrievable in one focused query, long logs cannot overflow responses, and credentials/private reasoning do not enter summaries.

### AX-022: Make startup readiness queryable and drains waitable

Priority: 2. Dependencies: AX-009, AX-013, AX-014.

Reproduce the reported start/status race against the existing readiness pipe before altering ordering. Define success only after saved run state is publicly queryable, even for immediately completed once runs. Add stop --wait with a finite timeout bound to the original controller session and durable stop result. Reuse P2-004 scoped stop handling and existing signal distinctions. Acceptance: status immediately after successful start finds a saved run; failed startup returns structured evidence; drain wait handles active work, hold correction, startup, exit, and replacement controllers without stopping a new run. Test with deterministic stub barriers.

### AX-023: Show execution configuration before launching agents

Priority: 2. Dependencies: AX-001, AX-014.

Add a read-only preflight/dry-run command that resolves executable paths, coding/review agents, models/effort and their sources, launch mode, verification/setup, and once semantics without spawning agents or changing saved state. Before foreground execution, emit a concise safe launch description or structured equivalent. Acceptance: preflight under missing tools, profiles, environment overrides, and unknown provider defaults produces accurate provenance without creating a controller or billing usage; credentials and raw secret-bearing arguments are omitted; unverified services remain explicitly unchecked.

### AX-024: Keep executable identity accurate during installs

Priority: 2. Dependencies: AX-008, AX-014.

Reuse Go VCS metadata and explicit stamps, including the existing shallow-clone development stamp. Preserve unknown identity for archives or stripped metadata; never infer executable revision from the runtime project repository. Display client/controller differences and warn when an installer replaces an executable used by an active controller. Do not restart or install as part of this task. Acceptance: stamped, direct Go build, shallow, dirty, and metadata-free fixtures identify only the executable; installer fixtures warn for live destination use and state that running controllers keep their original build; fixtures never alter the user installation.

### AX-025: Discover saved runs and report searched paths

Priority: 2. Dependencies: AX-001, AX-014.

Add bounded run listing within the resolved configured base directory, with project roots, retained sessions, last known outcomes, ownership freshness, build evidence, and explicit selection hints. No-run errors name the resolved project root, task path, and searched base/runtime location. Do not scan private agent session directories. Acceptance: multiple roots/base overrides, missing state, unreadable records, stale metadata, and large run counts produce bounded actionable results without starting, stopping, or mutating runs.

### AX-026: Document command contracts and agent workflows

Priority: 2. Dependencies: AX-002, AX-003, AX-004, AX-005, AX-006, AX-007, AX-008, AX-009, AX-010, AX-011, AX-012, AX-013, AX-014, AX-015, AX-016, AX-017, AX-018, AX-019, AX-020, AX-021, AX-022, AX-023, AX-024, AX-025.

Provide contextual subcommand help, both version forms, valid minimal add-task examples, input/output schemas, pagination/streaming formats, exit and timeout semantics, capabilities, and JSON migration guidance. Unknown commands exit nonzero. Update looper-diagnose and backlog skills to teach compact status, bounded continuation/wait, focused inspection, receipt tracking, and supported recovery without private-state detours. Help accompanies earlier features; this task audits completeness and cross-command consistency. Acceptance: every new operation has accurate help/schema/examples, legacy invocation migration is explicit, and skill/package checks pass without wording-only tests.

### AX-027: Benchmark public workflows against audit fixtures

Priority: 2. Dependencies: AX-002, AX-003, AX-004, AX-005, AX-006, AX-007, AX-008, AX-009, AX-010, AX-011, AX-012, AX-013, AX-014, AX-015, AX-016, AX-017, AX-018, AX-019, AX-020, AX-021, AX-022, AX-023, AX-024, AX-025, AX-026, T044.

Build deterministic end-to-end fixtures from verified audit incidents using stub agents and the public CLI. Cover large histories, idle/dead/quiet/bootstrap controllers, held inputs, old capabilities, import dispatch pause, retries, interrupted acceptance/export, restart, and stop/start overlap. Measure successful procedures, output bytes, command latency, redundant observation, and private-state detours against the same baseline fixtures and output limits. Acceptance: compact status <=8 KiB, finite events <=page limit, continuation has no missed commits, idempotent mutations apply once, accepted receipts survive restart, and all common fixtures finish using documented public commands. Run appropriate focused checks, make test, make race, and record actual results/limits; do not claim agent-quality improvements from fixture success alone.

### AX-028: Prepare a fresh-agent evaluation and release checklist

Priority: 3. Dependencies: AX-026, AX-027.

Prepare a reproducible evaluation protocol using fresh sessions with matched tasks, tools, context/output limits, success rubrics, and cost/latency/output measurements. Include a baseline comparison and failure review. Prepare release checks for compatibility opt-in/default transition, schema documentation, platform gaps, and existing controllers retaining old builds. Acceptance: fixtures, prompts, commands, scoring, and result templates are ready; completed deterministic evidence and unmeasured claims are clearly separated. This task prepares the live evaluation only: no real-agent launch, installation, publication, or controller restart is authorized by backlog preparation. Record the concrete separately requested live test needed for empirical agent validation.
