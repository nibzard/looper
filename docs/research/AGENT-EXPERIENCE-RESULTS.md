# Agent experience benchmark results

Date: 2026-11-09. Scope: AX-027, benchmark public workflows against
deterministic audit fixtures.

This file records what the AX-027 benchmark fixtures measured on this Linux
host. The fixtures live in `scripts/runtime-test.py`
(`BenchmarkTests`) and run as part of `make test`. Every fixture drives the
built `looper` executable and the stub agent through documented public
commands only.

## Method

- Fixtures reproduce verified incidents from the 2026-11-05 CLI interaction
  audit (626 calls, 40 sessions; `/notes/looper-cli-interaction-audit-2026-11-05.md`).
  Each maps to an audit pattern by number.
- The stub agent is deterministic: gates hold windows open (bootstrap,
  import analysis, workers), so assertions observe states, not timing luck.
- `LOOPER_BENCHMARK_RECORD=<path>` appends one JSON line per public command
  (fixture, procedure, command, exit, seconds, stdout bytes, stderr bytes).
  The numbers below come from one recorded run of the eight fixtures at host
  load below 1 on 2026-11-09. Latency varies by host; the byte bounds and
  outcome assertions do not.
- Private state — the runtime SQLite database, acknowledgement directories,
  and agent session files — is never read by a fixture procedure. Fixture
  setup may shape the project (for example, age the controller metadata to
  pre-contract shape) the way an incident left it.

## Fixtures and procedures

| Fixture | Audit pattern | Procedure through public commands |
| --- | --- | --- |
| `large-history` | 1, 3 | Complete a 30-task backlog with long titles, then observe with compact status, bounded event tails, a 17-item page, a full continuation walk, a restart, and one finite wait. |
| `submission-restart` | 6, 12 | Submit `add-task` behind a 6-second verification hold; replay the same idempotency key; conflict on changed content; kill the controller; restart; query both receipts; resubmit the never-accepted task under a new key. |
| `bootstrap-foreground` | AX-004 live gap | Hold the first-backlog bootstrap of a foreground run open; observe with compact status; release. |
| `bootstrap-detached` | AX-004 live gap | The same window on `looper start`, whose startup seed previously hid the bootstrap phase. |
| `idle-held-input` | 2, input holds | Observe the idle wait, hold the run on a malformed task file, and release it — each transition woken by one bounded `wait --cursor`, not repeated polls. |
| `old-capabilities` | 6 | Age live controller metadata to pre-contract shape; submit `cancel`; read the refusal; confirm no dead letter; discover capabilities; stop. |
| `import-pause` | T044 | Submit `add-plan` during active work; observe the waiting import; hold the analysis with every worker slot free and T3 ready; release; verify T3 starts only after the applied import. |
| `stop-start-overlap` | 11 | `stop --wait` against a gated worker; observe the drain; release; start a replacement controller; verify it completes rather than inheriting the stop. |

## Measured results (recorded run, 2026-11-09)

| Measure | Result | Limit or baseline |
| --- | --- | --- |
| Compact status `--contract v2 --json` | 18 calls, max 2,085 bytes | 8,192 bytes (gate) |
| Plain piped status | bounded, liveness and cursor always present | 8,192 bytes (gate) |
| Finite events page | 17 calls, max 50 items per page (default and `--limit 17` respected) | page limit (gate) |
| Events page bytes | max 17,301 bytes for a 50-item page with payloads | count-bounded; byte gate applies to status |
| Continuation walk | 30-task journal walked through 17-item pages: ids 1..high-water, no gap, no duplicate; restart continuation resumed at the saved cursor with no missed commit | no missed commits (gate) |
| Mutation receipt latency | `add-task` and `add-plan` 5-16 ms, during a 6-second verification hold | immediate (gate) |
| Finite waits | request and input-hold waits returned satisfied in 0.26-1.01 s; cursor wait on a finished run timed out at 3.01 s with exit 5 and a timeout outcome | timeout is an outcome, never an error (gate) |
| Idempotent replay | same key and content returned the original request, `duplicate: true`; exactly one task applied; changed content rejected with `idempotency_key_conflict` | apply once (gate) |
| Receipts across restart | accepted request stayed `applied`; the envelope caught before acceptance returned `rejected` with the never-applied reason; resubmission under a new key applied once | queryable after restart (gate) |
| Old-controller refusal | `cancel` against pre-contract metadata refused with `capability_unsupported`, "No request was queued", no dead letter; `version --contract v2 --json` reported `absent_metadata` with the derived base set | no dead letters (gate) |
| Import dispatch pause | with every worker slot free and T3 ready, no ordinary start for the 3-second window; in the journal, T3's first `start` event id is greater than the import's `applied` event id | T044 integration (gate) |
| Stop/start overlap | `stop --wait` returned the durable applied result for its session; the replacement controller finished with shutdown reason `complete` | never stops a new run (gate) |
| Observation commands per procedure | 1-18 per fixture (the 18 is the deliberate 13-page journal walk plus checks); every other procedure needed 9 or fewer | audit baseline: 125 status/events polls in the heaviest session |
| Private-state detours in procedures | 0 — no procedure read the state database, the acknowledgements directory, or agent session files | audit baseline: 60 of 80 calls in session d01 read `state.db` directly |
| Successful procedures | 8 of 8 fixtures completed using documented public commands | all common fixtures finish (gate) |

## Coverage note: quiet workers and interrupted exports

Quiet-worker classification (output older than the 300-second threshold
versus no output at all) is pinned by the unit fixtures in
`TestStatusCompactDistinguishesControllerCases`; a live quiet fixture would
wait five minutes or falsify stream timestamps, so the benchmark does not
re-stage it. Interrupted publication and export replays are covered by the
existing surgery fixtures (`test_publication_journal_before_apply`,
`test_export_journal_replays`); the benchmark contributes the interrupted
*acceptance* leg, which the audit incidents actually hit.

## Defect found and repaired by the fixtures

The `bootstrap-detached` fixture exposed a live gap the synthetic AX-004
tests missed: the startup seed a detached start saves before readiness
(AX-022) made compact status read the bootstrap window as an empty backlog.
Status reported `waiting: no_ready_task`, no bootstrap phase, and a spurious
`view_unavailable` finding while the controller was reading project
requirements. `internal/looper/status_compact.go` now treats a startup seed
as pre-backlog for evidence collection, phases, and the waiting summary, and
`TestStatusCompactDistinguishesControllerCases` pins the seed case beside
the no-state case.

## Release-gate condition for the contract default

`internal/looper/capabilities.go` gates the default contract transition on
"the AX-027 release gates pass and the release notes name the new default."
Every gate the fixtures assert passed in this run and in `make test`, so the
first half of that condition is satisfied; naming the new default in the
release notes remains the release action AX-028 prepares. This benchmark
does not change `DefaultContractVersion`.

## What these results do not claim

- Fixture success does not measure agent quality. The stub agent returns
  deterministic results; nothing here says a real coding agent works better
  or worse. AX-028 owns the fresh-agent evaluation protocol and the
  separately requested live test.
- Latency numbers describe this host under low load. The gates assert the
  bounds (bytes, page limits, immediate receipts), not the milliseconds.
- The audit call counts (626 calls; 125 polls; 60 database reads) describe
  the audited sample only, and the comparison here is procedure shape
  (bounded observation replaced repeated polling), not a controlled
  same-agent experiment.

## Reproducing

```sh
make build
python3 scripts/runtime-test.py                                  # all fixtures
LOOPER_BENCHMARK_RECORD=/tmp/measure.jsonl \
  python3 -m unittest -v runtime-test.BenchmarkTests              # from scripts/
```

The suite asserts the gates; the record file holds the measurements.
