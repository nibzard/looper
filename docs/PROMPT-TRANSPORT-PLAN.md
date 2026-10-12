# Prompt transport and context pointers plan

Status: implemented and validated on Linux, 2026-10-12.
Darwin runtime validation remains a release follow-up.
Revised after review and current agent guidance.

An oversized planning prompt made `looper tui` fail with
`fork/exec ...: argument list too long`. The prompt embedded the whole backlog
and traveled as one argument of 272,768 bytes. The failure was labeled
`execution` and pointed at an empty events log.

Implement these contracts:

- Codex and Claude receive prompts through standard input. `prompt.txt` records
  the exact bytes sent to the agent.
- Setup, verification, and assessment receive command bodies through private
  script files. Arguments carry paths and fixed options.
- Zcode retains its documented argument transport until a live test verifies
  another transport. This is an explicit compatibility exception. Its prompt
  remains visible through process inspection, and the size guard can reject it.
- Prompts contain focused instructions and file references. Missing accepted
  requirements stop the operation; they never disappear from its scope.
- Launch failures distinguish the helper from the target. A target that starts
  and exits with an error remains an execution failure.

Automatic task splitting, token budgets, and changes to the log-tail limit
remain outside this plan. Do not change unrelated work in the current tree.

## 1 Complete the recovery evidence

Primary file: `internal/looper/inputwait_test.go`.

Complete the outstanding recovery check in
[LOOP-CORRECTNESS-PLAN.md](LOOP-CORRECTNESS-PLAN.md). The existing recovery test
adds a task already marked `done`. Add a test that corrects invalid input by
adding a runnable `todo` task. Keep the real controller running throughout.

Acceptance checks:

- [x] The controller waits without launching agents while input is invalid.
- [x] Correction adds a runnable task, and that task completes and publishes.
- [x] The completed task from before the wait has no additional worker run.
- [x] The same controller session handles the wait and the resumed work.

## 2 Guard both execution boundaries

Primary files: `internal/looper/process_unix.go`, platform-specific limit
helpers, `internal/looper/agent.go`, and focused process tests.

Build the actual helper arguments and cleaned environment before opening the
log. Include the executable path, `__exec`, terminating zero bytes, and pointers
in the calculation. Check both the helper vector and the target vector. Use the
same environment for validation and execution. Account for environment entries
that Go adds, such as `PWD`.

Keep a per-string policy of fewer than 128 KiB, including the terminating byte.
Compute the total limit from the platform. Do not assume that 512 KiB always
fits. On Linux, derive the limit from the current stack limit, page size, and
kernel floor and ceiling. On Darwin, query `kern.argmax`. Reserve 16 KiB for
additional execution overhead and interpreter arguments. Use a testable limit
helper without changing global state.

Treat the calculation as a guard, not a guarantee against every kernel failure.
Keep typed reporting for execution errors after validation. If limit discovery
fails, return an explicit launch error before starting the helper.

Return `ExecLimitError` with the argument index or environment key, measured
bytes, and limit. Do not include argument or environment values in the error.

Add a separate pipe for launch reports from `RunChild`. Preserve the existing
coordinator lifetime pipe and process-group cancellation. The helper calls
`cmd.Start`, reports success or a structured failure, then waits for the target.
Launch reports never share the agent transcript channel.

Read the pipe while honoring controller cancellation. An unexpected helper exit
without a report is a helper protocol failure. Do not infer launch status from
exit code 127 or from text in the events log.

Handle typed launch failures before reading agent output. Map them to
`invalid_request`, skip retries, and record no agent usage. Agent error details
name `prompt.txt` and `invocation.json`. State that the target agent never
started only when the launch report establishes that fact. A missing report
states that target startup is unconfirmed. Early guard rejection creates
no events log. A helper failure can leave a log, but that log does not supply
the launch diagnostic.

Keep genuine agent exits on the existing classification path. Do not add an
error-text regular expression to `classifyFailure`.

Acceptance checks:

- [x] A 131,072-byte argument is rejected before the helper starts.
- [x] An oversized environment entry reports its key without its value.
- [x] Total-size tests include terminating bytes, pointers, and helper options.
- [x] A Linux subprocess with a reduced stack limit rejects an excessive total
  below 512 KiB. The test does not change the test runner's limit.
- [x] Platform helpers compile on Linux and Darwin.
- [ ] Test Darwin limit discovery on a Darwin runner before release.
- [x] A missing target binary reports a typed launch failure with no usage row.
- [x] A target that starts and exits 127 remains an execution failure.
- [x] Missing reports and cancellation release the helper and its descendants.
- [x] Launch classification reads structured reports, not transcript error text.

## 3 Move supported payloads out of arguments

Primary files: `internal/looper/agent.go`, `internal/looper/engine.go`, and
`internal/looper/integration.go`.

Drop the positional prompt from both Claude branches and retain `-p`.
Standard input already carries the prompt through `__exec`. Keep Codex on
standard input and keep the documented Zcode exception.

Record `prompt_bytes` and `prompt_transport` (`stdin` or `argv`) in
`invocation.json`. Write that record and `prompt.txt` before launch validation.
These records also exist when a guard rejects a Zcode prompt.

Write each setup, verification, and assessment command to a private script
beside its existing log. Run Bash with a fixed login-shell invocation that
loads that file. Do not interpolate the body into the invocation. Preserve the
working directory, resource environment, logs, cancellation, and standard input
behavior. Test command compatibility before selecting the final invocation,
including positional parameters and shell exit behavior.

Include `err.Error()` in setup and verification repair feedback. Read a log tail
only when the log exists. Command launch errors name their script artifact.
Do not claim that agent prompt artifacts exist for a shell command.

Acceptance checks:

- [x] A Claude stub reads a 272 KiB prompt from standard input in both branches.
- [x] Codex receives the exact recorded prompt through standard input.
- [x] An oversized Zcode prompt returns `invalid_request` with no target run.
- [x] Setup, verification, and assessment arguments contain no command body.
- [x] Multiline commands, quoting, login setup, exit status, and cancellation
  retain supported behavior. Document any unavoidable difference.
- [x] A command body larger than 128 KiB runs through a script file.
- [x] Setup and verification launch errors stop the operation with retained
  work. Ordinary command failures supply structured repair feedback.

## 4 Make planner context focused and complete

Primary files: `internal/looper/agent.go`, `internal/looper/engine.go`,
`internal/looper/imports.go`, and prompt tests.

Write a backlog snapshot into each plan attempt directory before launch.
Replace the embedded backlog with its path and the private backlog path in
the worktree. Preserve instruction meaning and phrases used by agent stubs.
Adjust sentences that currently assume an embedded backlog.

Use 4,096 bytes as the initial planner prompt target. This is a measured
engineering target, not a model limit or a reason to reject valid work.
Give the optional task index at most 1,024 bytes.
Truncate titles at valid character boundaries. Remove line breaks from index
entries. Prefer unfinished tasks and include an omitted-task count. The full
backlog remains the authority; the index is only a navigation aid.

Write applied requirement paths to an index file and reference it. Use a
simple list of paths without a versioned manifest or new schema. Record the
role, prompt bytes, context bytes, target bytes, and whether the prompt exceeds
its target in `invocation.json`. Record actual model usage separately.
Reading a context file still consumes model tokens.

Use `appliedContext` as an error-returning helper. Open every artifact of
every applied import. Missing or unreadable files fail planning, worker
dispatch, and review. Include the import identifier and artifact path in the
error. Preserve the applied receipt for recovery. Never skip an import or mark
it unapplied. A stat check alone does not establish readability.

Make every referenced file accessible from the agent's worktree. Use
private, excluded context copies in that worktree. External read access is
optional. Keep durable
copies in the attempt directory. This fallback covers imports and feedback,
as well as the backlog. Context copies must not enter the candidate tree.

Controller checks own missing-file failures. Agent instructions also require
an explicit failure if a referenced file becomes unreadable during execution.
Never treat a missing requirement as uncertain write scope.

Acceptance checks:

- [x] A backlog with 300 KiB of details produces a prompt of at most 4,096 bytes.
- [x] Large task and import collections use index files. Target overruns are
  recorded without rejecting work.
- [x] The snapshot round-trips to the accepted backlog without losing fields.
- [x] Missing and unreadable applied artifacts stop planning and worker dispatch.
- [x] Index truncation preserves valid text and references the complete context.
- [x] Agent stubs read all context through the worktree fallback.
- [x] Context copies leave the candidate tree unchanged.

## 5 Convert worker and review context

Primary files: `internal/looper/agent.go`, `internal/looper/integration.go`,
`internal/looper/requirements.go`, and `internal/looper/engine.go`.

Move repair feedback to `context/feedback.md` in each operation directory.
The operation name includes the repair count and a session suffix.
Reference it through the same context access policy. Write it before launch.
Never overwrite feedback from an earlier session.

Do not assume that one task is bounded. Permit inline task JSON only when its
encoded size is at most 8 KiB. This threshold selects the starting context;
it does not limit task size. Otherwise reference the complete task snapshot.
Move large goals
and source lists to a backlog snapshot. Apply the same context selection to the
Zcode prompt without requiring task splitting. Only execution limits reject
an oversized Zcode argument.

Replace whole-backlog, import-text, and supplied-document embeds in review,
import analysis, and bootstrap. Use 16 KiB as an initial prompt target and
record target overruns without rejecting work. Preserve the existing evidence
count cap. Include evidence path lengths in the measured prompt size.

Remove `importedScope` after converting every caller. Apply the missing-artifact
failure policy before review can update findings or completion. Do not alter
historical attempts, stored prompt files, or saved review results.

Acceptance checks:

- [x] A single task with 300 KiB of details uses a task file and remains runnable.
- [x] Long goals, source lists, feedback, and import lists use file references.
  Prompt size and target overruns remain observable.
- [x] No builder embeds a whole backlog, import text, or feedback body.
- [x] Feedback artifacts remain separate across repairs and controller sessions.
- [x] Missing applied requirements prevent final completion, including imports
  that added no tasks. Findings and completion state remain unchanged.
- [x] Review, import analysis, and bootstrap can read their complete context.
- [x] Historical `prompt.txt` files remain byte-exact and are not rewritten.

## Context selection and outcome comparison

Keep each role focused while retaining access to complete evidence:

- The planner sees an unfinished task index and reads dependencies, source
  specifications, and documented commands from the snapshot and repository.
- The worker sees its task and acceptance criteria inline when practical.
  It retrieves relevant requirements and reads current repair evidence.
- The reviewer checks the full requirement index, integrated behavior, and
  executable evidence. Worker selection never reduces final review scope.

Use one durable context directory per operation, with copies inside its private
worktree. Exclude only controller-owned copies from code snapshots. Check their
contents before accepting agent results. Missing or changed applied import
artifacts stop work; preserve the receipt so evidence can be restored.

Keep repair feedback organized by command, error, observed output, and full log
path. Launch failures stop the controller with retained work. They do not
consume implementation repair cycles. Keep optional assessment failures
advisory under the existing verified-tree check.

Compare inline and file context with the same agent configuration and transport.
Use planning with a large backlog, repair with a buried requirement, and final
review of an incorrect product. Run two repeats per arm and case. Alternate
arm order. Grade executable outcomes independently of agent claims. Record
prompt bytes, elapsed time, tokens, cache usage, failures, and raw evidence.
Report costs only when model and pricing evidence support them.

Run the opt-in comparison with:

```sh
LOOPER_CONTEXT_EVAL_AGENT=codex \
LOOPER_CONTEXT_EVAL_DIR=/absolute/path/to/artifacts \
go test ./internal/looper -run '^TestContextLiveComparison$' -count=1 -v -timeout=45m
```

The comparison starts real agents and consumes their usage allowance. A small
comparison establishes feasibility; it does not establish a general success
rate. Increase repeats if results differ. Adjust context targets using outcomes
and total usage rather than prompt bytes alone. Let agent clients manage their
own session compaction. Do not add mandatory resets or another agent framework.

This follows [Anthropic's context guidance](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents),
[Amp's updated compaction design](https://ampcode.com/news/neo), and
[Amp's executable feedback examples](https://ampcode.com/notes/feedback-loopable).

## Validation and delivery

1. Complete the recovery test and run the focused correctness tests.
2. Implement launch guards and reporting with focused process tests.
3. Move supported payloads out of arguments and run the agent stub tests.
4. Convert planner context, then worker and review context. Run the relevant
   planning, import, review, and runtime tests after each change.
5. Update `README.md` and `docs/ARCHITECTURE.md` with prompt targets, script
   transport, missing-artifact behavior, and the Zcode exception.
6. Run `make test` and `make race`. Record Linux and Darwin validation results.
7. Review the diff against every acceptance check. Leave unverified checks open.

Use focused changes for these stages. Commit only relevant work when the
implementation workflow calls for commits. The live smoke test and context
comparison provide additional outcome evidence. Worktree copies avoid a
dependency on repeated Claude `--add-dir` flags. Codex read-only access to
those copies passes the live planning comparison. A live Zcode test is required before changing
its transport contract.

Outcome evidence: [context transport results](research/CONTEXT-TRANSPORT-RESULTS-2026-10-12.md).

Platform references: [Linux execution limits](https://man7.org/linux/man-pages/man2/execve.2.html)
and [Darwin execution limits](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/syslimits.h).
