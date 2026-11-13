# Ownership map for the agent experience plan

Date: 2026-11-05. Updated: 2026-11-09. Owner: AX-001 in
`docs/AGENT-EXPERIENCE-PLAN.md`.

Update note 2026-11-09 (T115): the status columns are now a checked
projection of the accepted backlog. `TestOwnershipMapStatusesMatchBacklog`
in `internal/looper/ownership_map_test.go` fails the suite on
disagreement, so a completion that touches the backlog without this file no
longer passes its own checks. The same pass corrected the rows the check
found trailing: T029 (done 2026-11-08, the row still described the lifted
deferred hold), T058 (done 2026-11-08, the row still said the backlog read
todo), the T042 row's list of holds that have since lifted, and the task
map, which gained a Status column and the AX-027 and AX-028 implementation
notes.

Update note 2026-11-08: corrected the status column after the Stage 2
completions. T042 recorded its owner decision on 2026-11-07 (authorized;
supervision through the live TUI; validation on this Linux host), and T020,
T018, T021, T044, and T046 then completed. T047 and T058 carry the recorded
unblock instruction that lifted their deferred hold. The map agreed with the
backlog when written on 2026-11-05; five completions since then touched
`NOTES.md` without refreshing this file. Later on 2026-11-08, T058's startup
import landed in the working tree; its row names the landed mechanism while
the backlog row still holds todo until the run integrates it.

This map records who owns each piece of the agent experience work and where
it overlaps work that already exists in the backlog. The accepted backlog
(`to-do.json`) stays authoritative for status, blockers, and unblock
conditions; nothing here marks blocked work complete, removes a gate, or
reassigns ownership. When this map and the backlog disagree, the backlog wins
and this map must be corrected.

## Rules

1. Every task in `docs/AGENT-EXPERIENCE-PLAN.md` appears below, with its
   owner, its dependencies, and the existing work it reuses.
2. Overlapping work keeps its original owner. This plan consumes it; it does
   not reimplement it.
3. Blocked tasks stay blocked under their recorded conditions. A worker that
   reaches one reports the unmet prerequisite instead of duplicating the work.
4. Operator-owned actions stay operator-owned. Workers prepare proposals and
   evidence; they do not grant, approve, or perform them.
5. Every status cell starts with the backlog's status word for that task
   (`Todo`, `Doing`, `Blocked`, `Done`, or `Cancelled`). A row that names a
   landed mechanism before the run's integration records the completion in
   the backlog says `pending integration`, and drops that note once the
   backlog catches up. Do not restate another task's status in prose; name
   the task and let its own row or the backlog carry the status.
   `TestOwnershipMapStatusesMatchBacklog` in
   `internal/looper/ownership_map_test.go` enforces the first two sentences
   wherever an accepted backlog sits beside this file.

## Overlap ownership

| Task | What it owns | Consumers in this plan | Status and gate |
| --- | --- | --- | --- |
| T020 | Recovering request queues from overflow instead of wedging | AX-009 depends on it. Request receipts cannot be honest while the claim path refuses everything above a threshold and hot-spins. | Done 2026-11-07 (`sweepRequestQueue`). AX-009, which waited on it and on T029, completed 2026-11-08. |
| T029 | Making the startup settings sweep safe for live submitters | AX-009 depends on it, after T020. Durable acceptance must not delete a request a live submitter still waits on. | Done 2026-11-08, integrated at 1da356f: submitter binding, the verified-gone sweep, and the ack-write keep rule. The deferred hold lifted the same day, after the owner recorded the unblock instruction. |
| T044 | Pausing ordinary dispatch after accepting a plan import | AX-027 verifies that integration as one of its fixtures. No other task implements the pause. | Done 2026-11-07 (`importDispatchHold`). AX-027 exercised that integration as its `import-pause` benchmark fixture on 2026-11-09. |
| T058 | Startup import of a retained backlog edit left by a cancelled hold | AX-017 reuses it instead of writing a second import path. | Done 2026-11-08, committed at f913437: `initializeState` routes a non-equivalent external edit through `refreshHeld` when every retained attempt holds a replayable result (a result commit, or a receipt the recovery adopts), keeps the restore demand otherwise, and requeues a publication whose base the import moved. The recovery preview mirrors the same gate read-only. The guarded crash-recovery fixture is `TestStartupRecoversCancelledHoldRun`. |
| T046 | Keeping the controller running across an owner commit mid-run | Independent recovery scope. AX-015 and AX-016 explain the guard; they do not change its acceptance rule. | Done 2026-11-08 (`recoverCommittedHead` folds a descendant head with no retained-scope changes). |
| T047 | Quarantining one invalid task instead of failing startup | Independent recovery scope. AX-025 and AX-014 report such refusals; they do not change the startup rule. | Done 2026-11-08 (`quarantineInvalidFiles` at startup and refresh, `quarantineInvalidScopes` in the planning pass; the legacy plan import drops malformed scopes; `applyResult` rejects a reported entry no scope can use at the merge, so the files list stays scannable at the moment of the report). |
| T042 | The operator schedules supervised Stage 2 | Gate for T020, T029, T044, T046, T047, T058 and the Stage 2 tasks T011, T018, T019, T021, T022. | Done 2026-11-07, operator action recorded: authorized, supervision through the live TUI, validation on this Linux host. The deferred holds it gated were lifted one by one as the owner recorded each unblock instruction (T047 and T058 on 2026-11-08, T011, T022, and T029 later the same day). |
| T043 | The operator records decisions for disputed findings | Gate for triage of disputed review findings. | Blocked, operator-owned. |
| T053 | Deploying the integrated build to the live self-run controller | Out of scope for this plan. AX tasks must not install, publish, or restart the active self-run. | Blocked, operator-owned. |
| P2-004 | Scoped stop requests (done) | Reused by AX-009, AX-011, AX-022: the stop request type binds to the owning session. | Done. |
| P2-006 | Shallow-history build stamps (done) | Reused by AX-008 and AX-024 for executable identity. | Done. |
| Readiness pipe | Detached startup readiness (done, part of T015) | Reused by AX-022, which defines start success as a queryable run. | Done. |
| T026, T028, T050, T052, T057, T066, T068, T069, T041, T051, T055 | In-tree fixes already landed | Reused as documented in `docs/AGENT-CONTRACTS.md`. Version 2 contracts describe their behavior; they do not reimplement it. | Done. |

## Task map

Owner is `agent` unless a row says otherwise. Dependencies are the backlog's
`depends_on` values. The Status column follows rule 5: it starts with the
backlog's status word, and the sync check holds it there.

| Task | Title | Owner | Depends on | Status | Reuse and boundaries |
| --- | --- | --- | --- | --- | --- |
| AX-001 | Define versioned agent contracts and migration | agent | none | Done 2026-11-05 | Delivers `docs/AGENT-CONTRACTS.md`, `docs/AGENT-CONTRACT-MIGRATION.md`, `docs/contracts/`, this map, and the executable guards in `internal/contractschema` and `internal/looper/compatibility_test.go`. |
| AX-002 | Commit findings before reporting success | agent | AX-001 | Done 2026-11-05 | Reuses the existing `addFinding` and export journal; pins behavior if the tree already fixes it. |
| AX-003 | Read state and event cursors consistently | agent | AX-001 | Done 2026-11-05 | Stops using `view.json` as the event position; cursor scope is defined in the contracts document. |
| AX-004 | Add bounded status and piped liveness | agent | AX-001, AX-003 | Done 2026-11-05 | Implements `schemas/v2/status-compact.schema.json`. Legacy status stays frozen. |
| AX-005 | Bound task inspection and page attempt history | agent | AX-001, AX-003 | Done 2026-11-06 | Implements the compact inspect contract; keeps the legacy unpaged document. |
| AX-006 | Make finite event queries bounded and filterable | agent | AX-001, AX-003 | Done 2026-11-05 | Implements `schemas/v2/events-page.schema.json`; NDJSON stays for `--follow`. |
| AX-007 | Attach structured identities to lifecycle events | agent | AX-001 | Done 2026-11-06 | Extends `Store.Event` payloads; message text stays for people. |
| AX-008 | Advertise controller capabilities before submission | agent | AX-001 | Done 2026-11-05 | Implements `schemas/v2/capabilities.schema.json`; reuses P2-006 build identity. |
| AX-009 | Persist common request receipts across restart | agent | AX-001, AX-002, AX-008, T020, T029 | Done 2026-11-08 | Generalizes the T026 import receipt and the settings acknowledgement into the `State.Requests` store, with staged rollback and the frozen `accepted.json` byte copy. |
| AX-010 | Expose bounded request queries and waiting reasons | agent | AX-009 | Done 2026-11-08 | Implements `schemas/v2/request-record.schema.json`; `looper requests` and the doctor v2 exit policy read it. |
| AX-011 | Return immediate receipts for CLI mutations | agent | AX-009, AX-010 | Done 2026-11-08 | Implements `schemas/v2/request-receipt.schema.json`; dashboard controls consume the same operations. |
| AX-012 | Make retries idempotent with caller keys | agent | AX-009, AX-011 | Done 2026-11-08 | Idempotency keys are caller-supplied; mirrors never own the key. |
| AX-013 | Add finite waits for changes and outcomes | agent | AX-003, AX-006, AX-010 | Done 2026-11-09 | One structured outcome; a timeout never cancels. |
| AX-014 | Use actionable structured errors across the CLI | agent | AX-001 | Done 2026-11-05 | Implements `schemas/v2/error.schema.json`, extending the doctor finding pattern; legacy bytes stay frozen. |
| AX-015 | Explain retained-work guards with concrete differences | agent | AX-014 | Done 2026-11-05 | Explains the guards; T046 and T047 keep their own recovery scope. |
| AX-016 | Preview recovery without changing saved work | agent | AX-015 | Done 2026-11-06 | Read-only `looper recovery --preview`; no force bypass. |
| AX-017 | Apply checked recovery through controller operations | agent | AX-009, AX-016, T058 | Done 2026-11-09 | `looper recovery --apply <choice> --token` applies `restore-task-file` (byte-pinned restore source) and `import-backlog` (equivalent adoption, or the T058 import only when every retained result replays); every application re-derives the preview token and refuses a stale preview without mutation. A live controller claims a `recovery` envelope at its boundary (an in-flight import is refused because the drained boundary owns it); a stopped run applies under the repository lock with no agent launched. |
| AX-018 | Recognize validated formatting-only backlog changes | agent | AX-015 | Done 2026-11-06 | Semantic comparison only; byte digests stay for immutable artifacts. |
| AX-019 | Cancel tasks through checked controller requests | agent | AX-009, AX-012, AX-014 | Done 2026-11-09 | `looper cancel` through request envelopes; validates against old backlogs. |
| AX-020 | Amend task contracts through revision checks | agent | AX-009, AX-012, AX-014, AX-019 | Done 2026-11-09 | `looper amend` and `amend-task` requests; completed work stays historical, and done targets create follow-ups. |
| AX-021 | Summarize failures with bounded public evidence | agent | AX-007, AX-014 | Done 2026-11-06 | One `failure` event kind with a scrubbed 2 KiB context; the cleanup wrapper audit ran against a stub first. |
| AX-022 | Make startup readiness queryable and drains waitable | agent | AX-009, AX-013, AX-014 | Done 2026-11-09 | Reuses the readiness pipe and P2-004 scoped stop; the startup seed survives the bootstrap window and `stop --wait` drains durably. |
| AX-023 | Show execution configuration before launching agents | agent | AX-001, AX-014 | Done 2026-11-06 | Read-only preflight; no agent invocation, no usage. |
| AX-024 | Keep executable identity accurate during installs | agent | AX-008, AX-014 | Done 2026-11-06 | Reuses P2-006. Does not install or restart anything. |
| AX-025 | Discover saved runs and report searched paths | agent | AX-001, AX-014 | Done 2026-11-06 | `looper runs` opens stores immutably so no sidecar appears; does not scan private agent session directories. |
| AX-026 | Document command contracts and agent workflows | agent | AX-002 through AX-025 | Done 2026-11-09 | Audits help, schemas, and examples against the implemented contracts. Every command serves its own help block (`looper help <command>`, `<command> --help`, `<command> help`), `help` is a command word so bare `looper help` prints the overview, and a bare word that names no command and no file is the `unknown_command` usage error instead of a run against a phantom task file — closing the evidence rounds recorded since d929777. The skills teach the version 2 workflows, and `TestCommandHelp*` and `TestSkill*` keep the help blocks, the registry, and the taught invocations consistent. |
| AX-027 | Benchmark public workflows against audit fixtures | agent | AX-002 through AX-026, T044 | Done 2026-11-09 | Eight benchmark fixtures live in `scripts/runtime-test.py` (`BenchmarkTests`) and run as part of `make test`: large-history, submission-restart, bootstrap-foreground, bootstrap-detached, idle-held-input, old-capabilities, import-pause (T044's integration), and stop-start-overlap. Each drives the built executable and the stub agent through public commands only; `LOOPER_BENCHMARK_RECORD` appends one JSON line per command. `docs/research/AGENT-EXPERIENCE-RESULTS.md` records the measured run. |
| AX-028 | Prepare a fresh-agent evaluation and release checklist | agent | AX-026, AX-027 | Done 2026-11-09 | Prepares the evaluation only: `docs/research/AGENT-EXPERIENCE-EVALUATION.md` defines the protocol, authorization boundary, scenarios, and scoring, with fixtures, prompts, and result templates under `docs/research/agent-evaluation/`; the release checklist work landed in `docs/release.md` and `docs/VALIDATION.md`. Every recipe is stub-verified; the live agent test needs a separately requested scope (see the operator section) and `docs/research/AGENT-EXPERIENCE-EVALUATION-RESULTS.md` does not exist until that run fills it. |

## Operator-owned prerequisites

These actions belong to the operator. A worker reports them as blocked with
their resume condition instead of performing them.

- **T042** (done 2026-11-07): the operator recorded the supervised Stage 2
  schedule — authorized, supervision through the live TUI, validation on this
  Linux host. T018, T020, T021, T044, and T046 completed under it, and the
  deferred holds on T047 and T058 were lifted to todo. The remaining holds
  under this gate (T011, T022, T029) received their recorded unblock
  instructions on 2026-11-08 and completed the same day.
- **T043**: record decisions for the disputed review findings.
- **T053**: deploy an integrated build to the live self-run controller. No AX
  task installs, publishes, or restarts a live controller.
- **Live evaluation authorization**: AX-028 prepares a fresh-agent evaluation
  protocol. Running it with real agents needs its own requested scope; the
  plan's execution rules forbid invoking paid agents to complete these tasks.
