# Fresh-agent evaluation protocol and release readiness

Date: 2026-11-09. Owner: AX-028 in `docs/AGENT-EXPERIENCE-PLAN.md`.
Status: **prepared, not run.**

This document defines the live evaluation of the Looper agent experience
with fresh agent sessions. It also records what deterministic evidence
already exists and which claims stay unmeasured until the live test
runs. Everything a session touches is prepared: fixtures, prompts,
commands, scoring, and result templates.

## Authorization boundary

The plan's execution rules forbid invoking paid agents to complete
backlog tasks. This preparation launches no real agent, installs
nothing, publishes nothing, and restarts no controller.

The live test needs its own requested scope. Concretely, the operator
authorizes:

1. Up to 27 bounded headless sessions of one real agent binary on this
   host: one pilot, 24 measured sessions, and two replacement sessions
   for environment faults. Each session runs one scenario brief.
2. Looper controllers running inside scratch directories named with the
   operator's `LOOPER_RESOURCE_PREFIX`, driven by the deterministic stub
   agent, exactly as `make test` runs them. The stub performs all
   fixture work; real agents only operate the CLI.
3. No change to the installed binary, the live self-run controller,
   shared Git refs, or anything published. Fixtures live under the
   resource prefix and are removed after the run.

Authorization covers measurement only. It does not flip the contract
default, publish a release, or restart any controller outside the
scratch prefixes.

## Inventory (everything is ready)

| Piece | Location |
| --- | --- |
| Measurement shim (times, measures, and logs every `looper` call) | `docs/research/agent-evaluation/bin/looper` |
| Arm preambles | `docs/research/agent-evaluation/prompts/arm-legacy.md`, `prompts/arm-v2.md` |
| Legacy-arm guidance file | `docs/research/agent-evaluation/prompts/legacy-interface.md` |
| Matched task briefs S1 to S4 | `docs/research/agent-evaluation/prompts/s1-observe-brief.md` through `s4-discover-brief.md` |
| S2 submission inputs | `docs/research/agent-evaluation/fixtures/s2-new-task.json`, `fixtures/s2-plan.md` |
| Result templates | `docs/research/agent-evaluation/results/session-template.json`, `results/comparison-template.json` |
| Deterministic setup verification | `docs/research/agent-evaluation/setup-verification.md` |
| Deterministic workflow benchmark | `scripts/runtime-test.py` (`BenchmarkTests`), results in `docs/research/AGENT-EXPERIENCE-RESULTS.md` |

The v2 arm's guidance files are copies of the shipped skills
(`skills/looper-diagnose/SKILL.md`, `skills/looper-backlog/SKILL.md`).
The harness places them in the session directory at launch; it does not
rely on installed skills.

## Design

Two arms complete the same brief against the same fixture state:

- **Arm legacy** uses the default legacy machine contract. Its guidance
  file reproduces the skill text that shipped with the legacy default,
  before the version 2 documents.
- **Arm v2** runs with `LOOPER_CONTRACT=v2` and the shipped skill files.

Held constant between arms: the brief, the fixture state, the agent
binary and model, the turn bound, the sandbox, the working directory,
the environment (except `LOOPER_CONTRACT`), and the guidance surface
(the legacy arm gets one cheatsheet; the v2 arm gets the two shipped
skill files — a bias the limitations section records).

A **fresh session** has: no resumed conversation, no recalled memory of
Looper, no repository access outside its scenario directory, and no
interactive terminal. Session launch, verified at run time against the
binary's own help:

```sh
cd "$WORKSPACE" && cat arm.md brief.md | claude --print \
  --model <fixed model> \
  --max-turns <fixed bound> \
  --allowedTools "Read LS Grep Glob Bash(looper:*)" \
  > answer.md
```

The pilot session verifies the flags and the permission allowlist before
any measured session runs. The operator records the model, binary
version, and turn bound in the comparison file; they never vary between
arms.

Cost comes from the harness's own session usage report, captured per
session. Latency is wall-clock from launch to final answer. Output bytes
and command counts come from the shim log, never from private
transcripts.

## Scenarios

Each repetition builds a fresh fixture instance under the scratch
prefix, captures the expected evidence through public commands into
`expected.json`, runs one session per arm against a fresh copy, then
runs the deterministic post-checks. The stub-agent recipes are verified;
see `setup-verification.md` for the recorded run.

Every scenario uses the same workspace layout, so the arms and the
briefs never depend on placement:

```text
workspace/            session working directory
  project/            the Looper project (S1, S2, S3; S4 has none)
  inputs/             S2 submission files, outside the project tree
  base/               LOOPER_BASE_DIR for the run stores (S4 holds two)
  guidance files      the arm's reference material
```

### S1 — observe a saved run (audit patterns 1 to 4, 9)

- **Setup**: a project with 30 completable long-title tasks plus
  `T-FAIL`, whose verify command exits 4 with a marker line. Run 1
  completes 30 tasks; run 2 blocks on `T-FAIL`. No live controller.
- **Brief**: `s1-observe-brief.md` (five questions: identity and
  controller state; counts and blockers; last transition; the exact
  continuation command; the newest failure with operation, outcome, exit
  status, task, and evidence location).
- **Scorer-side expected evidence**: run id, controller `stopped` and
  not owned, counts (30 done, 1 blocked, `T-FAIL`), the last transition
  kind and event id, the continuation command naming the cursor, and the
  newest `failure` summary (`verify`, `exit_failure`, exit 4, `T-FAIL`,
  evidence file inside the runtime directory).
- **Post-checks**: Git head unchanged; `git status --porcelain` equals
  the recorded baseline; the event cursor did not move.

### S2 — submit work to a live controller (patterns 6, 12)

- **Setup**: gated live controller on a two-task backlog, exactly the
  `worker_gate` mechanics of `BenchmarkTests`. The submission inputs are
  the prepared fixture files, placed outside the project tree.
- **Brief**: `s2-submit-brief.md` (submit the task file and the plan
  file; report both request ids, lifecycle states, applied outcomes with
  created task ids and plan coverage, and whether anything applied
  twice; every wait bounded).
- **Scorer-side expected evidence**: two immediate receipts; both
  requests reach `applied`; the added task exists once; the import added
  exactly one engine-assigned task; the coverage fields name it.
- **Post-checks**: exactly one `S2-ADDED` task and one imported task in
  the task file, both done; the receipts still answer after the
  controller drains; the controller is stopped by the harness afterwards.

### S3 — explain a refusal and its recovery (patterns 5, 7)

- **Setup**: a controller hard-killed behind the worker gate (killed by
  its own PID), leaving one retained running attempt; an external edit
  then changes that task's contract. Resume refuses with the
  retained-work guard.
- **Brief**: `s3-recover-brief.md` (quote the refusal and name the
  guard; report the concrete difference the guard names; enumerate the
  supported recovery actions with what each preserves and changes;
  recommend one, without performing anything).
- **Scorer-side expected evidence**: the refusal sentence; guard
  `backlog_contract_changed` failing with both digests and the retained
  task; the four preview choices with their preserves and changes
  (`restore-task-file` available; `resume` and `import-backlog`
  blocked with reasons; `discard-retained` unsupported).
- **Post-checks**: head, porcelain, and refs equal the recorded
  baseline; no `recovery-applied` event; no new controller session.

### S4 — discover the saved runs (patterns 7, 10)

- **Setup**: one base directory holding two saved runs: a completed run
  driven by a deliberately differently stamped binary (`0.9.0-eval-drift`
  style), and a run killed mid-work with retained work and stale
  ownership. The workspace itself is not a project.
- **Brief**: `s4-discover-brief.md` (count the runs and their projects;
  per run report session, ownership state, and work held; name the run
  with the different build and both builds; give the exact select
  command and the selected run's waiting reason).
- **Scorer-side expected evidence**: both runs, their ownership states
  (`stopped`, `stale`), the retained-work run, the build pair, and the
  waiting reason `controller_stopped` naming the retained attempt.
- **Post-checks**: both task files unchanged; no new sessions in either
  store; the runs listing is byte-identical to the setup capture.

## Scoring

Every check is binary and cites evidence. The scorer fills
`results/session-template.json` per session.

| Scenario | Checks |
| --- | --- |
| S1 | identity and controller state; counts and blocked task; last transition; exact continuation command; failure summary fields; no mutation |
| S2 | both receipts with ids; states with evidence; applied outcomes with created ids; applied exactly once (post-check); no unbounded watch and no call over 120 s; no private-state read |
| S3 | refusal and guard; concrete difference; supported actions with preserves and changes; reasoned recommendation; no mutation |
| S4 | both runs and projects; session, ownership, and work per run; build drift pair; select command and waiting reason; nothing mutated |

A scenario-arm **passes** when at least two of three repetitions pass
every check. A repetition with an environment fault (harness or host)
is replaced, not scored. The evaluation reports the pass rates and the
measurements; it sets no numeric gate on command counts or cost. A
release that cites this evaluation cites the recorded numbers, not a
target.

## Measurements

Per session, from the shim log and the harness report:

- looper command count, and count by command name;
- total looper stdout bytes (the truncation tax the audit measured);
- slowest command and per-command latency;
- identical repeated calls (the polling pattern);
- wall-clock seconds and harness-reported cost;
- private-state detours, from the mandatory transcript review.

## Baseline comparison

1. **Matched baseline (primary)**: arm legacy on the same fixtures. The
   comparison template records both arms' medians per scenario.
2. **Deterministic baseline**: the AX-027 stub benchmark
   (`docs/research/AGENT-EXPERIENCE-RESULTS.md`). Its gates hold today;
   its numbers describe procedures, not agent skill.
3. **Historical reference (not a control)**: the 2026-11-05 audit
   sample — 626 calls across 40 sessions, 125 status or events polls in
   the heaviest session, and 60 of 80 calls in one session reading
   `state.db` directly. Those sessions had different tasks, tools, and
   limits. The comparison file carries this column labeled `matched:
   false`. No claim may treat it as a controlled comparison.

One measured contrast exists today on the S1 fixture: legacy
`status --json` returned 66,723 bytes where the compact document
returned 1,690 bytes (recorded once, this host, during setup
verification).

## Failure review

Every failed or partial repetition gets a review before scoring is
final. The reviewer (a human or a session that did not run the scenario):

1. Reads the answer, the shim log, and the session transcript.
2. Classifies the cause: `interface-defect`, `documentation-gap`,
   `agent-misuse`, or `environment-fault`.
3. Records it in the session result and the comparison findings array.
4. Files a papercut for tooling friction, or a follow-up against the
   owning task for an interface defect, reusing the existing
   `looper:finding:<key>` tag when one exists.
5. Decides with the operator whether an interface defect blocks a
   release recommendation.

A refusal to act on missing evidence is a correct outcome, not a
failure, when the interface states exactly which evidence is missing.

## Evidence status

**Completed deterministic evidence** (recorded, reproducible):

- Every scenario setup recipe ran end to end with the stub agent; see
  `setup-verification.md` for outcomes and session notes.
- The AX-027 gates are asserted on every run of its suite; a focused
  `BenchmarkTests` run at this revision passed all 8 tests (85 s).
- The shim logs and passes through correctly (smoke test recorded).
- The S1 byte contrast measured once: 66,723 versus 1,690 bytes.

**Unmeasured until the live test runs** (no claim may state them):

- Command counts, latency, cost, and output bytes for real sessions.
- Rubric outcomes and pass rates for either arm.
- Any comparison between arms, and any statement about agent
  experience, fluency, or quality.

Results land in
`docs/research/AGENT-EXPERIENCE-EVALUATION-RESULTS.md` at run time,
filled from the templates. Until that file exists with measured rows,
the fresh-agent evaluation has no results.

## Limitations

- The arms differ in guidance surface: two shipped skill files for v2
  against one cheatsheet for legacy. The bias favors v2; the historical
  audit column is the honest legacy reference.
- One agent binary. The audit covered Claude and Codex sessions; a
  second binary is an extension the authorization may add, not a
  precondition.
- The shim captures output, so it disables interactive interfaces; the
  briefs forbid `tui` and `attach`.
- Three repetitions detect gross differences, not small ones. The
  evaluation's power is qualitative: where fresh sessions fail, and
  whether the failures name the interface or the documentation.
