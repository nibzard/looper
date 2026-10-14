# Unattended run recovery

## Goal and scope

Looper recovers from its own orchestration mistakes without operator intervention.
Reuse the existing agent runner, task repair, state store, and event journal.
Do not require a separate repair command, service, or operator approval for
ordinary repairs within the user's requested project scope.

This document proposes changes. Except where marked implemented, they are not
present in the installed binary. References describe the checkout reviewed on
15 October 2026. Source links use relative paths so they work in this repository.

## Incident and evidence

Project: `/home/agent/gitpickle`.
Original session: `run-0f555d74014a88ae`, revision `f48cbdd0f4d9`.
Recovery session: `run-8f8986e91f7b7e90`, revision `a09e08d67cdf`.
All times below use Coordinated Universal Time (UTC).

| Evidence | Observation | Consequence |
| --- | --- | --- |
| Bootstrap result `bootstrap-73c63b41735051bf/result.json` | `source_files` included `to-do.json.context/bootstrap-73c63b41735051bf/backlog.json`. | Later worktrees could not read this operation-specific path. |
| Retained bootstrap `context/backlog.json` | The snapshot contained an empty source list and task list. | Restoring it would add no product requirements. |
| T001, attempt `task-d47f8ab737b2ffbe` | The worker retained a Rust workspace but reported the missing snapshot as a blocker. | Useful implementation never reached integration. |
| Tasks T038 and T040 | Workers attempted context restoration but could not change backlog or setup contracts. | Task execution could not repair the orchestration error. |
| Event 730, 14 October at 21:25:54 | The controller waited for changed requirements or resolved blockers. | At 22:02, no workers ran; zero tasks had integrated. |
| Saved plan before recovery | Verification required `cargo xtask model`, `fault-test`, and `bench` before their implementation tasks completed. | Removing the source reference alone would leave premature verification gates. |
| Repeated external-prerequisite tasks | T039/T041/T042 repeated resource needs; T046–T050 repeated naming correspondence needs. | Follow-ups consumed attempts without resolving the prerequisite. |
| Event 784, 22:10:58 approximately | T001 entered repair after verification changed project files. | The feedback did not identify the changed path. |
| T001 check at 22:12:01, revision `d7a351969253fedba30eadee8375a78552a112fb` | Integration verification passed after adding `/.cache/` to ignore rules. | Existing task repair handled the implementation defect. |
| Events 814 and 815 | T001 completed; T002 started. | The repaired run resumed useful work. |

The initial idle snapshot recorded 50 tasks, 17 blocked, none ready, and no
integrations. After recovery, three tasks were marked done, but only one was an
integrated implementation task. The other two were administrative closures of
obsolete recovery tasks. Do not use the done count alone as proof of recovery.

### Retained local evidence

These files are local runtime artifacts, not portable repository dependencies:

- [Original status and plan](/home/agent/.looper/gitpickle-c39a4efa/runtime/recovery-20261015/status.before.json).
- [Original backlog](/home/agent/.looper/gitpickle-c39a4efa/runtime/recovery-20261015/to-do.before.json).
- [Recovery record](/home/agent/.looper/gitpickle-c39a4efa/runtime/recovery-20261015/recovery.txt).
- [T001 result and check records](/home/agent/.looper/gitpickle-c39a4efa/runtime/recovery-20261015/T001.after.json).
- [Bootstrap result](/home/agent/.looper/gitpickle-c39a4efa/runtime/bootstrap-73c63b41735051bf/result.json).
- [Empty bootstrap snapshot](/home/agent/.looper/gitpickle-c39a4efa/runtime/bootstrap-73c63b41735051bf/context/backlog.json).

Use `looper inspect T001 --json` and `looper events --json --after 720` to
inspect structured evidence while the retained run remains available. Keep raw
agent logs local. Build regression fixtures from minimal, synthetic evidence.

## Existing implementation and gaps

| Area | Reference | Current behavior and gap |
| --- | --- | --- |
| Bootstrap | [engine.go](internal/looper/engine.go), `Engine.bootstrap`; [context.go](internal/looper/context.go), `validateBootstrapSources` | Private context paths are rejected before backlog persistence. Existing accepted backlogs are not repaired. |
| Planning | [agent.go](internal/looper/agent.go), `planPrompt`; [engine.go](internal/looper/engine.go), `Engine.plan` | The planner generates setup, verification, and task scopes. Configuration overrides can replace its commands. |
| Verification | [integration.go](internal/looper/integration.go), `Engine.verify` | Commands run in a candidate worktree. A snapshot difference and a capture error share one generic failure message. |
| Task repair | [integration.go](internal/looper/integration.go), `Engine.repair` | Repairs retain implementation work and count repeated failures. They do not provide a structured run-contract repair. |
| Diagnosis | [integration.go](internal/looper/integration.go), `Engine.diagnose` | A repeated failure calls the review agent for a read-only cause, evidence, and fix approach. It cannot apply configuration changes. |
| Idle handling | [run.go](internal/looper/run.go), `Engine.Run` | With no runnable work, the loop reviews and waits. It does not first attempt recovery from a proven orchestration blocker. |
| State and export | [engine.go](internal/looper/engine.go), `save`, `export`, `contract`; [run.go](internal/looper/run.go), `refreshIdle` | Existing checks protect retained contracts and concurrent backlog edits. Recovery must preserve them. |
| Work reuse | [engine.go](internal/looper/engine.go), `Engine.start` | A reopened task can reuse a blocked attempt when its contract still matches. |
| Settings | [profile.go](internal/looper/profile.go); [config.go](internal/looper/config.go) | Saved settings and environment overrides affect effective commands. Repairing only the cached plan can leave the actual failing command unchanged. |
| Follow-ups | [findings.go](internal/looper/findings.go); [findings_test.go](internal/looper/findings_test.go) | Pending local changes add stable identity and operator ownership. They were excluded from commit `a09e08d`; review them before relying on them. |
| Status | [observe.go](internal/looper/observe.go); [controller.go](internal/looper/controller.go) | Structured observations expose controller build metadata. Make the installed/running distinction clear in normal status output. |

The bootstrap prevention fix is in
[commit a09e08d](https://github.com/nibzard/looper/commit/a09e08d67cdf93181b4e50dc107b7c43de6e817d).
Its regression coverage is in
[bootstrap_sources_test.go](internal/looper/bootstrap_sources_test.go).
It prevents the original invalid source from being accepted on a new bootstrap.
It does not migrate old run state.

## Proposed recovery behavior

### Trigger recovery where the loop already handles failures

Keep ordinary code and test failures on the existing task repair path. Use run
repair when evidence shows that the task cannot fix the failing contract.
Examples include a private source reference, an invalid setup command, or a
verification command that depends on a later unfinished task.

Before entering the no-runnable-work wait, inspect retained blockers for these
conditions. Do not call an agent on every idle poll. A missing credential alone
is not a run configuration defect.

For a newly rejected bootstrap result, permit one corrected agent response
before giving up. No accepted backlog exists at that point; do not manufacture
normal task state merely to reuse recovery code.

### One consultation, followed by a real retry

1. Gather the accepted backlog, effective commands, relevant failures, and retained-work locations.
2. Ask the existing review agent for a small structured repair proposal in an isolated worktree.
3. Validate the proposal against the state that produced the failure.
4. Apply permitted changes through the controller and invalidate affected cached plans.
5. Resume the retained task and run the real integration checks.
6. Record recovery only when execution makes observable progress.

Extend the existing diagnosis response with optional repair fields. Avoid a
second agent call solely to translate a diagnosis into a patch. A minimal
proposal needs the cause, evidence references, changed source entries or
commands, and task IDs to reopen. Use a structured object, not shell commands
that edit the database or state files.

The agent may conclude that the failure requires external input. In that case,
retain the blocker and explain what is missing. Do not create another coding
task for the same prerequisite.

### Apply only changes supported by evidence

For the GitPickle fixture, the allowed repair is specific:

- Remove the private, empty bootstrap snapshot from `source_files`.
- Replace setup's missing-snapshot requirement with workspace dependency setup.
- Replace premature model and benchmark gates with the current workspace check.
- Reopen T001 without changing its implementation acceptance criteria.
- Resolve obsolete context-recovery tasks with an explicit administrative reason.

Do not remove an unreadable source merely because it blocks execution. Its
contents or origin must establish that it is disposable orchestration context.
If it may contain unique product requirements, preserve the blocker.

Do not mark implementation tasks done based on a repair proposal. Do not grant
credentials, publish releases, send correspondence, or claim external checks
passed. Those actions retain their original authorization and evidence needs.

### Preserve state and avoid retry loops

Use the existing controller serialization and journal. Apply shared changes only
when workers and checks affected by them are drained. Unrelated work must not
lose its results. Do not mutate contracts beneath an active worker.

Compare the expected backlog and effective command values before application.
If the user changed them during consultation, discard the stale proposal and
read the new state. Preserve the existing export conflict checks.

Persist the repair attempt and its outcome with the run state. Use a normalized
failure identity based on the failure category, relevant contract or command,
and concrete evidence. Ignore timestamps and temporary operation paths. Review
`failureEvidence` in [integration.go](internal/looper/integration.go) before
adding another normalizer.

Allow one run-repair attempt for unchanged evidence. A restart, idle poll, or
renamed follow-up must not reset this limit. A material command, requirement, or
external prerequisite change can create a new recovery opportunity.

A crash after applying a repair must resume the recorded retry without applying
the patch again. Prefer extending existing save/export recovery over introducing
another transaction mechanism.

## Verification at the current stage

The current planner must distinguish executable checks from future acceptance
commands. Preserve the later checks in their task requirements. Run them when
the corresponding implementation exists.

The manual recovery used the following effective commands for T001:

```sh
# Setup: T001 creates the initial manifest and lockfile.
set -eu
export CARGO_HOME="$PWD/.cache/cargo"
export CARGO_TARGET_DIR="$PWD/target"
if test -f Cargo.toml; then cargo fetch --locked; fi
```

```sh
# Verification: the existing workspace check must pass.
set -eu
export CARGO_HOME="$PWD/.cache/cargo"
export CARGO_TARGET_DIR="$PWD/target"
cargo xtask check
```

These are incident examples, not permanent defaults for every Rust project.
The worker must ignore generated cache and build directories. A missing manifest
still fails verification; setup's initial allowance does not imply success.

The model, fault-test, and benchmark requirements remain in the backlog. Final
review must still assess the complete product. A successful T001 check proves
only that the workspace meets T001's acceptance criteria.

Make corrected commands part of the effective run configuration. Respect the
existing precedence rules and ensure a restart cannot silently restore the bad
command. Record the previous and replacement values locally. Do not silently
rewrite global preferences or weaken explicit user requirements.

## Better verification evidence

Update `Engine.verify` to produce distinct results for command failure, snapshot
capture failure, and project-file changes. Include the underlying capture error
when one exists.

When both snapshots exist, retain a path-level comparison before removing the
candidate worktree. Use the existing repository snapshot and Git utilities.
Report added, modified, and deleted paths with a bounded summary. Keep the full
comparison in the check artifact directory. Do not dump file contents into the
event stream.

For the cache failure, feedback should identify an added path under `.cache/`,
state that the command itself passed, and point to the retained comparison.
The existing task repair path should then add the ignore rule and retry. This
failure does not require a new run-repair subsystem.

## Follow-up handling and status

Review the pending stable-key and operator-ownership changes before adding more
logic. Repeated prerequisite reports should attach evidence to the existing task.
Preserve completed status and active task contract hashes when evidence changes.

Display installed and running revisions together when they differ. Installation
does not replace an existing controller. Restarting a controller also does not,
by itself, repair a previously accepted backlog.

Record concise recovery events: started, applied or rejected, and retry outcome.
A patch being applied is not a successful recovery. Use integration and successor
dispatch as the success evidence for this incident.

## Tests and observable acceptance

Use stub agents and temporary repositories for routine tests. No real credentials,
external test resources, or paid agent calls are needed for these fixtures.

| Fixture | Required result |
| --- | --- |
| New bootstrap lists a private snapshot | Reject the source before persistence; one corrected response can proceed. |
| Existing run has the GitPickle source and verification defects | One run-repair consultation corrects both contracts without operator edits. |
| T001 has retained implementation | Reuse that work; do not restart from an empty workspace or alter its acceptance criteria. |
| Integration generates an unignored Cargo cache | Report the added path; ordinary task repair fixes it and verification passes. |
| Recovery succeeds | T001 integrates and T002 starts. Administrative task closures alone do not satisfy the assertion. |
| Agent proposes removing a real requirement or disabling all checks | Reject the proposal; preserve the accepted requirements and prior commands. |
| Missing provider credentials or owner decision | Keep the task blocked; do not run a repeated repair or create duplicate prerequisite tasks. |
| Agent returns malformed output or changes protected files | Discard the proposal and retain the original state. |
| The same failure persists after repair and restart | Do not call the repair agent again for the unchanged failure. |
| User edits the backlog during consultation | Reject the stale patch without overwriting the edit. |
| Controller stops after applying the patch | Resume the recorded retry without applying the patch twice or losing retained work. |
| Installed and running revisions differ | Status clearly reports both and does not imply that the controller received the update. |

Extend these existing test areas rather than building a separate harness:

- [diagnosis_test.go](internal/looper/diagnosis_test.go): independent diagnosis,
  invalid output, changed-file rejection, and repeated-failure handling.
- [bootstrap_sources_test.go](internal/looper/bootstrap_sources_test.go): private
  paths, custom backlog names, and valid project sources.
- [inputwait_test.go](internal/looper/inputwait_test.go): concurrent edits, waiting,
  correction, and continuation without repeating completed work.
- [planning_test.go](internal/looper/planning_test.go): planning behavior and contracts.
- [findings_test.go](internal/looper/findings_test.go): pending duplicate and
  operator-task cases; preserve active contract identity.
- [observe_test.go](internal/looper/observe_test.go): structured run observations.
- [runtime-test.py](scripts/runtime-test.py): end-to-end controller behavior.

Run focused tests while implementing, then `make test` on the combined change.
Run relevant race tests for controller state changes. A real-agent smoke run is
optional and requires an explicitly requested live test.

## Delivery order

1. Add precise verification evidence and regression coverage.
2. Extend diagnosis to propose run repairs, then apply them through the controller.
3. Add bounded automatic triggers before terminal waiting or repeated failure.
4. Correct planner guidance and preserve effective command changes across restart.
5. Review the pending follow-up handling changes and run the combined recovery fixture.
6. Add revision mismatch reporting and document recovery outcomes.

Keep each change small enough to review. The first usable delivery must recover
the complete GitPickle fixture; a diagnostic message alone is not sufficient.
Do not publish or restart unrelated live runs as part of implementation.

## Completion criteria

The isolated fixture recovers without operator edits, integrates T001, and starts
T002. Product requirements and retained work survive. External prerequisites stay
blocked. Repeated equivalent failures do not cause unlimited agent calls or
follow-up tasks. A restart preserves both the repair and its retry limit.

The evidence distinguishes a proposed repair, an applied repair, a passing
integration check, and a completed product. The operator can understand the
remaining blocker without reconstructing private agent logs.

## Checkout implementation, 15 October 2026

The controller now applies structured proposals at drained idle boundaries.
Task diagnosis can supply the same proposal without another consultation.
Run state retains consultation limits, effective command corrections, and retry
outcomes. The existing export journal protects application across restart.

Source removal requires the original retained bootstrap snapshot to be empty.
Unknown or unreadable sources stay blocked. Explicit verification overrides can
change only when unfinished task requirements preserve the original command.
Administrative closure requires a `looper:orchestration` tag and a direct link
to the removed private source. Legacy tasks without this evidence stay blocked.

The combined shell-agent fixture in `internal/looper/recovery_test.go` checks
retained work, current-stage verification, generated-cache feedback, ordinary
repair, T001 integration, and T002 dispatch. It also checks interruption after
application. Separate tests cover stale edits, protected files, unsafe proposals,
external prerequisites, command precedence, and build mismatch reporting.
