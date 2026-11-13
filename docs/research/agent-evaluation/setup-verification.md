# Setup verification for the fresh-agent evaluation

Date: 2026-11-09. Owner: AX-028. Host: this Linux machine, load below 2.
Build: `make build` at revision `2661c56682c5` (dirty: this task's
documents), stamp `0.9.1-dev.23`.

This file records the deterministic verification run of every scenario
recipe in `docs/research/AGENT-EXPERIENCE-EVALUATION.md`, performed with
the stub agent and the built binary in scratch directories under the
session resource prefix. It is the completed deterministic evidence for
the preparation. No real agent session ran: that is the separately
requested live test.

## What was verified

| Check | Result |
| --- | --- |
| Measurement shim smoke test | `looper version --contract v2 --json` logged one JSON line: exit 0, 0.004 s, 719 stdout bytes; output passed through unchanged. |
| S1 recipe (mature run with a failure) | 30 tasks with long, escape-heavy titles completed (`--once`, exit 0); `T-FAIL` with a failing verify command blocked the second run (exit 2). 30 done, 1 blocked. |
| S1 evidence answers | Compact status: run id, controller `stopped`/not owned, counts, cursor 227, last transition `shutdown`; `events --kind failure` returned three `verify`/`exit_failure` summaries, exit status 4, task `T-FAIL`, evidence file path, marker in the bounded context. |
| S1 size contrast (measured once, this host) | Legacy `status --json` 66,723 bytes; `status --contract v2 --json` 1,690 bytes; `--full` 66,724 bytes. |
| S2 recipe (live controller, both fixtures) | Gated controller live; `add-task fixtures/s2-new-task.json` returned receipt `settings-a550f8fca7b9e79c` (submitted); `add-plan fixtures/s2-plan.md` returned `import-6dbe133339b99b24` (submitted); two bounded `wait --request ... --timeout 60s` calls returned `satisfied`; `stop --wait` drained. Final backlog: `S2-ADDED` done and imported task `T004` done — each applied exactly once. |
| S3 recipe (refusal beside retained work) | Controller hard-killed behind the worker gate leaves 1 retained running attempt; external task-file edit (retitled `T1`); resume refuses exit 1 with "the backlog changed while work is retained; restore its saved contract before resuming". Read-only preview: task file `changed` with both digests, guards `backlog_contract_changed` fail / `branch_moved` pass / `task_contract_changed` pass, four choices with preserves/changes (`restore-task-file` available, `resume` and `import-backlog` blocked, `discard-retained` unsupported). |
| S4 recipe (discovery with build drift) | Two runs under one base dir: `alpha product` complete, controller `stopped`, controller build `0.9.0-eval-drift` (deliberately differently stamped binary); `beta tooling` killed mid-work, ownership `stale`, 1 done of 2, retained work listed. `runs --contract v2` reported both with exact `cd` select commands. |
| AX-027 benchmark suite | `python3 -m unittest runtime-test.BenchmarkTests` from `scripts/`: 8 tests, 85.0 s, all pass (recorded to a `LOOPER_BENCHMARK_RECORD` file). These fixtures own the S2 mechanics (receipts across restart, idempotency, waits, import pause, stop/start overlap). |

## Session notes recorded during verification

- A controller must be killed by the PID of the `looper` process itself.
  Killing the wrapping shell leaves the controller alive and adopted by
  init, which silently breaks the S3/S4 recipes. The protocol's setup
  scripts launch controllers directly (no `cd ... &&` wrapper around the
  background command).
- The stub's gate helpers wait for a gate file to *exist* (worker and
  import gates). A gate that is never touched blocks its branch for
  120 s and then fails the stub; setup must release gates.
- The engine writes `to-do.schema.json` beside the task file when
  missing, and killed-mid-work runs leave task output files untracked.
  The no-mutation baseline is recorded after setup, so these setup-made
  entries never count against a session.
- Imported additions receive engine-assigned ids (the S2 plan's addition
  landed as `T004`, not the analysis proposal's temporary id) — already
  recorded in NOTES.md under AX-027.

## What this verification does not establish

- Nothing here measures a real agent session: command counts, latency,
  cost, and rubric outcomes for the arms remain unmeasured until the
  separately requested live test runs.
- The single-measurement byte contrast above describes this fixture and
  host; the comparison template carries the measured medians for the
  live run.
