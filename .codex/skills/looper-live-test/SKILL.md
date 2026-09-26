---
name: looper-live-test
description: Run one live Looper iteration in a temporary project to verify task selection, file creation, and status application. Use for a requested live smoke test; use the local test suite for routine changes.
---

# Test one live iteration

Use a temporary project so the test does not change the user's backlog.
The test starts a real agent and can consume its usage allowance.
A local stub checks the runner, but does not establish live agent behavior.

## Workflow

1. Resolve `scripts/run-live-test.sh` relative to this skill's directory.
   To test a particular checkout, set `LOOPER_BIN` to its absolute
   `bin/looper.sh` path. The runner otherwise prefers its checkout over PATH.
2. Run the script. It creates `PROJECT.md` and a two-task backlog, then
   executes Looper with `MAX_ITERATIONS=1` and no iteration pause.
3. Inspect the task entry, result, and final report.
   The expected selection is `T2`, ahead of `T10` at equal priority.
4. Verify the actual outcome: `T2` is `done`, `T10` remains `todo`, and
   a nonempty `README.md` exists. The runner checks these conditions.
   Exit code 2 is expected because one task remains.
5. Report the executable, agent, selected task, checks, outcome, and
   temporary paths. Keep the artifacts for inspection. Do not report a
   blocked task or an exit code alone as a successful test.

## If the runner is unavailable

Create the same fixture in a temporary directory and run the intended
Looper executable with `CODEX_JSON_LOG=0`, `LOOP_DELAY_SECONDS=0`, and
`MAX_ITERATIONS=1`. Check the same outcomes before reporting success.
