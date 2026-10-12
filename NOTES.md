# Diagnosis findings from the margin run, 2026-10-12

Source: looper-diagnose observation of the `margin` project, session
`run-a2af312a6b687569`, Looper 0.7.1. Evidence window: event log
2026-10-11T11:37Z to 2026-10-12T14:31Z, 3,407 events.

## Finding 1: Controller exits leave no record

- **Observed:** Eight `resume` events in the margin log, zero `stop`
  events, no retained `daemon.log`. T050's worker was cut mid-flight
  three times in 52 minutes (13:19, 13:41, 13:57 on 2026-10-11), each
  time with fresh worker output before the restart. A double resume
  occurred at 05:50:08 and 05:52:15 on 2026-10-12. T052's overnight
  attempt (8 h 23 m of worker output) ended with no event; the 09:41
  repair cites "recorded failure evidence" that never appeared in the
  log.
- **Expected:** Every controller exit writes a durable event that
  states the reason. A resume states how the previous session ended.
- **Cause (confirmed in source):** The only exit path that logs is the
  graceful drain, `internal/looper/run.go:223`. The SIGINT cancel,
  panic, and external kill paths write nothing. The `resume` event,
  `internal/looper/engine.go:354`, carries no cause and no link to the
  previous session's end. The installed 0.7.1 retains no daemon log.
- **Change:** Emit a shutdown event with a reason from every exit path
  (SIGINT, drain, panic through `defer`, wait failure). On resume,
  detect the missing stop event and record that the previous session
  ended uncleanly, with the last seen event id.
- **Verify:** Fixture test with a stub agent. Send SIGINT to the
  controller and assert the event exists. Send SIGKILL and assert the
  next resume marks the unclean end.
- **Caution:** The uncommitted LOOP-CORRECTNESS work (owner-goroutine
  event rule) may already cover event durability at shutdown. Verify
  that before writing new code.

## Finding 2: Findings can be identical twins

- **Observed:** Margin events 3152 and 3153 are byte-identical
  "Ignored follow-up" messages. A reader cannot tell one follow-up
  logged twice from two distinct follow-ups.
- **Expected:** Each finding event identifies its subject.
- **Cause:** The finding message does not include the follow-up
  identifier or title.
- **Change:** Include the follow-up identifier in the finding message.
  One line.
- **Verify:** Unit test that two findings about different follow-ups
  produce distinguishable messages.

## Finding 3: Per-task timing is not queryable

- **Observed:** `timing.available` is false on the installed 0.7.1.
  Reconstructing per-task durations required mining 2,775
  `worker_activity` records, and the result still mixes worker time
  with check time.
- **Expected:** `looper status` reports per-task elapsed time without
  manual event mining.
- **Cause:** The timing and statistics subsystem
  (`internal/looper/timing.go`, `internal/looper/statistics.go`) is
  uncommitted and unreviewed, and the installed binary predates it.
- **Change:** No new code. Review and land the existing work.
- **Verify:** After landing and installing, `looper status --json`
  on a new run reports non-null `worker_seconds`.

## Not Looper defects

Do not chase these in Looper. All are margin-side and tracked in its
backlog:

- Root disk at 98 to 99 percent full caused an IO_FAILED flake class
  (margin T092).
- Benchmark exclusivity waits are per margin's own spec (T082, T087).
- Budget misses are real product work (T084, T090, T091).
- Repair cycles on an environmental fault are honest verification per
  margin SPEC 18.4, not a control loop defect.
- Check serialization is the `check_limit` knob, not a defect.

## Suggested order

1. Commit LOOP-CORRECTNESS. It gates the rest.
2. Fold Finding 2 into that work.
3. Verify Finding 1 against LOOP-CORRECTNESS, then implement what is
   missing.
4. Review and land the statistics subsystem (Finding 3).

The live margin controller still runs 0.7.1. None of this reaches it
until a run is restarted.

## Prompt transport work, 2026-10-12

The completed goal implements the revised prompt transport plan. Existing margin
diagnosis findings above remain separate work.

- Keep hard limits at the execution boundary. Prompt byte targets remain
  advisory: 4 KiB for planning and 16 KiB for other roles.
- Send Codex and Claude prompts through standard input. Zcode retains its
  documented argument transport until a live binary verifies another method.
- Keep complete context in durable files and private worktree copies. Workers
  receive focused task inputs. Final review retains every applied requirement.
- Missing, unreadable, or changed applied artifacts stop the operation. The
  controller preserves their receipts for recovery.
- Report helper and target launch failures through a separate pipe. An exit
  code of 127 alone does not establish a launch failure. Launch failures retain
  work and do not consume implementation repairs.
- Run setup, verification, and assessment from private script files. Preserve
  raw logs. Normalize the script path in repair feedback so repeated failures
  retain the same identity.

The first live smoke test found a Git exclusion error when an agent ignored
the context directory. The fix removes owned context from the temporary index
after staging. Regression coverage includes ignored context and changed
ownership markers. The second smoke test passes: T2 publishes a nonempty
README, and T10 remains pending.

The context comparison runs planning, repair, and review twice per arm.
All 12 runs pass independent fixture checks. File context reduces median input
tokens for planning and review, but takes longer. Repair results vary. Two
repeats do not establish a general quality gain. Keep small task contracts
inline and measure actual usage, including file reads.

The recovery test now completes a new runnable task after an invalid backlog
edit. It confirms that completed work does not repeat in the same controller
session.

`make test` and `make race` pass on the final implementation, including the
script-error normalization change. Race-enabled helper processes use
`GORACE=atexit_sleep_ms=0`; the default sanitizer exit delay caused test timeouts.
Darwin arm64 compilation passes. Darwin runtime validation and a live Zcode
transport test remain release follow-ups. No change is installed or published.

Detailed evidence: [context transport results](docs/research/CONTEXT-TRANSPORT-RESULTS-2026-10-12.md).

## Run monitoring, 2026-10-12

A recurring observer watches the two live runs every 10 minutes. Session job
`00b819ee`. Snapshots and diffs live in `/tmp/looper-monitor/`. Baseline taken
at 15:32Z; ticks through 16:00Z. The margin run (`run-a2af312a6b687569`, PID
2077775) used the old installed binary until 15:59Z, then restarted as
`run-08d65e21efda86a7` (PID 319425) on the 17:00 local install with worker
limit 2. The commonstate run (`run-dac67b00a161a719`, PID 2861537, started
15:01Z) uses the same new binary with timing, statistics, and prompt
transport.

### Margin state

Parked since 15:18Z (event 3630): 65 of 94 done, 28 blocked, 1 todo, 0 ready,
0 in flight, 10 integrated. All 28 blocked tasks share one failure class:
`execution: exit status 126`, a command that exists but cannot execute. Each
blocker retains its evidence file under `margin-e7d63a59/runtime/`. The
controller idles at zero usage. One root cause, 28 blockers.

### Commonstate state

Healthy and active. 77 of 119 done, 13 integrated, 1 in flight at 15:54Z.
T105 passed its check and integrated at 15:39Z; T112 is in repair round 2.
Twelve blocked tasks group into three classes:

- 9 tasks: repair limit reached after `pnpm db:up` failed because Docker
  cannot create networks ("all predefined address pools have been fully
  subnetted"). The plan setup script documents a `docker run --network
  bridge` workaround, but the check setup script does not use it. Margin-side
  fix, one backlog edit.
- 2 tasks: no 26.2 host pair reachable. Environmental.
- 1 task: C-18 needs a product decision on configurable submission expiry.

Churn: 57 repairs, 66 failed checks against 13 passes. Usage at 15:54Z:
89 reports, 537.4M tokens (509M cache reads), 159.8 USD estimated, 368.5 USD
reported, model glm-5.3. Active intervals burn roughly 5 to 11 USD per
10 minutes. Worker saturation near 100 percent in every observed interval.

### Findings for Looper development

- **Finding 2 (identical findings) looks fixed in the new binary.** Observed:
  events 6732 to 6734 carry task IDs and titles ("T117 Added follow-up: ...").
  Expected: distinguishable findings. Verify against source and add a
  regression check if LOOP-CORRECTNESS did not already cover it.
- **Finding 3 (timing) confirmed live on both sides.** Margin, old binary:
  `timing.available` false, statistics null. Commonstate, new binary:
  `timing.available` true, `worker_seconds` advances with wall time, pricing
  populated, 89 of 89 reports priced. Landing the statistics subsystem
  resolves it; restart margin's run to benefit.
- **New: circuit breaker for identical execution failures.** Observed: margin
  spent 38 attempts, 28 of them against the same exit 126 launch error, one
  task at a time. Expected: after N identical execution failures, stop
  scheduling tasks that use the same launcher and surface one aggregated
  blocker. Change: count consecutive identical execution errors per agent
  command in the engine; block with a single combined reason. Verify: fixture
  test where the agent command is not executable; assert later tasks are not
  attempted.
- **Parked runs cost nothing.** A drained controller emits a clear waiting
  event and burns no usage. No timeout or restart pressure is needed.

### Blocker response procedure

Blocking is triage, not failure. The controller already skips blocked tasks
and continues until nothing is ready. To unblock: read `blocked.reasons` and
`unblock_when`, open the cited evidence file, fix the class (host, setup
script, or spec decision), then set the task to `todo`. The controller
watches the backlog and resumes without repeating finished work. Use
`looper stop` only when a parked run should end.

### Post-restart learnings, 16:00Z tick

- **Durable stop confirmed live.** The old margin session ended with event
  3631 at 15:59:39 ("Stopped after draining active work"), the event class
  that was entirely absent from 3,407 events on 0.7.1. This exercises the
  graceful-drain path only; SIGINT, SIGKILL, and panic paths remain
  unverified in a live run.
- **Timing history imports on restart.** The restarted margin session
  reports `timing.available` true with 107,556 worker-seconds reconstructed
  from retained records. Finding 3 is closed on both live runs.
- **A restart does not clear blockers.** The new margin session re-planned
  and re-parked within the same second (events 3632, 3633). Blockers record
  conditions in the world, not controller state. Counters were identical
  across the restart: no work repeated. Worker limit is session
  configuration: margin restarted with 2 implementing workers while
  commonstate keeps 1.
- **`timing.tasks` is not live yet.** Both runs return null for it. The
  17:00 local install predates the later timing-task change. Verifying a
  fix requires knowing which build a run uses; the working tree moves
  faster than the install.
- **`worker_seconds` sums processes.** Commonstate advanced 700 s over
  roughly 360 s of wall time while a worker and a check overlapped. Treat
  it as utilization, not elapsed time; concurrent workers can exceed 100
  percent.
- **Repairs convert within limits.** T112 failed one check, then repair
  round 2 integrated cleanly at 15:54:11 with the recorded failure
  evidence. The repair limit matters only when conversion stops, as on the
  nine Docker-blocked tasks.
- **Estimated and reported cost diverge.** At 16:00Z commonstate reports
  160.4 USD estimated against 370.0 USD reported, a factor of 2.3. Check
  the pricing table before budgeting on `estimated_usd`.

### Monitoring close-out, 18:25Z

The observer stopped after 15:32Z to 18:25Z (17 ticks plus baseline).
Margin ended stopped and resumable (last event 3634). Commonstate ended
healthy: 83 of 130 done, 19 integrated, 13 blocked, T121 in flight,
session `run-dac67b00a161a719` alive throughout.

Window totals for commonstate: done 76 to 83, integrated 12 to 19,
backlog 116 to 130, tokens 519.8M to 627.6M, cost 154.6 to 186.3 USD
estimated and 356.5 to 428.6 USD reported, worker time plus 12,543 s.
Completions: T105, T110, T112 (repair round 2), T119, T120, T116.
T114 blocked on missing infrastructure with three named reasons.

Additional learnings from the final hour:

- **The review phase is event-silent; the planner is not.** A 27.5
  minute milestone review emitted one start event and nothing else, while
  planners stream activity plus 30 s controller heartbeats ("Controller
  waiting for planner, last activity ..."). Extend those heartbeats to
  review; an events-only operator currently cannot distinguish a long
  review from a hang.
- **Two worker slots doubled throughput.** With one slot, one task
  completed every 15 to 30 minutes; with two, every 7 to 10 minutes,
  and worker_seconds tracked the in-flight count exactly (601, 1201,
  1327 s per 600 s interval). No contention anomalies appeared.
- **Reviews keep the backlog honest.** Fourteen new tasks were filed
  during the window and the done count still rose by seven. "Total done"
  is a moving target; integrated count is the stable progress measure.
- **Zero repair churn in the final hour.** All six completions passed
  their checks on the first attempt after the milestone review replanned
  the work. The earlier 57-repair churn concentrated on tasks whose
  check setup hit the Docker network limit.
- **Blocking classes seen in practice:** environment (Docker address
  pools, no host pair, no Proxmox credentials), product decision (C-18,
  T121, T127), and launcher failure (margin exit 126). The system files
  its own unblock work: T125 turns the missing agentlabd deployment
  into a backlog task.
- **Duplicate follow-ups are ignored with the subject named** (events
  7094, 7688), confirming the Finding 2 fix live, twice.
- **Usage cost lands in lumps.** Reports arrive when an agent run
  completes, so a 10 minute interval shows either zero cost or the full
  cost of a finished task (0.5 to 8 USD observed). Per-interval cost
  deltas measure completions, not spend rate.

## Diagnosis fixes, 2026-10-12

This goal addresses Findings 1, 2, and 3 in the source tree. The earlier
findings describe the installed margin run and remain as historical evidence.

- The controller commits a structured shutdown event and session end together.
  It records completion, drain, cancellation, errors, dispatch limits, and
  controller panic. It waits for supervised work to stop before recording exit.
  A recording failure leaves the session open for the next startup to detect.
- Restart reports the previous exit reason and event ID. An unclean exit reports
  the last event ID and an unknown cause. SIGKILL cannot record its own cause.
  Controller panic retains the original panic after recording its exit.
- Rejected findings include a sanitized title and a short content identifier.
  Distinct unnamed findings remain distinguishable. Duplicate handling is unchanged.
- Existing timing records lacked a task report. `status --json` now exposes
  `timing.tasks`; `inspect` exposes the matching task timing in JSON and text.
  Worker, check, and other durations include all saved attempts and repairs.
  Active time merges overlaps. Elapsed time includes gaps between task processes.
  Missing records remain unavailable or partial; unknown ends add no duration.
- Runtime coverage found that read-only stores lacked their artifact directory.
  Preserve the directory so observers classify worker and check records correctly.
  A regression test exercises timing through a read-only store.

Focused coverage includes atomic exit writes, controller panic, rejected findings,
SIGINT, SIGKILL recovery, timing across repairs, overlapping checks, and missing
history. The runtime timing fixture uses a start barrier to guarantee overlap.
`make test` and `make race` pass, each with all 53 runtime tests. The checks
also include Go tests, static analysis, package fixtures, and skill checks.
Darwin arm64 compilation passes.

The live margin controller remains unchanged. Changes remain uncommitted.
Package checks install only in temporary fixtures. No change is installed for
the live run, and no restart or publication occurs. Darwin runtime validation
remains a follow-up.

## Release preparation, 2026-10-12

The release runbook selects 0.8.0 for the new context and observation features.
An isolated candidate includes 63 product and documentation files. Shared
source changes remain outside the real index until the user confirms scope,
as required by `docs/release.md`. Local agent instructions, AXINBOX metadata,
working notes, the simplification review, and the edited improvement research
remain excluded.

The candidate uses VERSION 0.8.0 and passes `make test` and `make race`, including
all 53 runtime tests in each run. The normal binary reports `looper 0.8.0`.
Darwin arm64 compilation and candidate whitespace checks pass. The pinned price
snapshot matches its source checksum. Source files still match the candidate.

The working VERSION and installed binary remain 0.7.1. No content commit,
version bump, user installation, tag, push, or GitHub release occurs during
preparation. Temporary packaging fixtures test installation. The live margin
controller remains unchanged. Commit release content before the version-only
bump, then build and install after scope confirmation. Publication needs an
explicit request.

The exact file list and remaining steps are in `docs/RELEASE-PREP-0.8.0.md`.
Release notes are in `docs/release-notes/0.8.0.md`.

## Review of monitoring actions, 2026-10-12

Reviewed the 16:00Z retained snapshots, current launch handling, public usage
records, and focused regression coverage. No live run or project backlog changes.

- Finish the existing release for shutdown records and task timing. Retained
  live databases contain no structured shutdown records, and the snapshots have
  no task timing. Those observations describe older builds, not missing source fixes.
- Do not add a circuit breaker based only on exit 126. Current source stops
  the controller on confirmed launcher failures and retains work without repairs.
  A process that starts and returns 126 remains an execution failure. One margin
  blocker log is empty, so its underlying cause remains unknown. Diagnose the
  launcher or wrapper before choosing a shared-failure mechanism. Empty logs
  alone do not prove a launch failure.
- Audit cost provenance before changing rates. The mismatch exists within
  individual stored usage reports, not only dashboard aggregation. A commonstate
  sample records 0.59735 USD estimated and 1.485591 USD reported for the same
  model usage. Estimated cost uses the pinned provider rates; reported cost comes
  from public agent fields. Neither value alone establishes the actual charge.
  Compare endpoint, model, cache counters, and the agent's rate basis. Do not
  apply a fixed multiplier from the aggregate ratio.
- Treat Docker network exhaustion, unavailable hosts, and expiry policy as
  project or environment work. Verify the exact check setup failure before
  choosing a backlog or setup edit. Current Looper uses the accepted setup
  command for both workers and checks; the notes do not establish a distinct
  controller-side omission.
- Add build provenance as a useful follow-up. Several different binaries retain
  version 0.7.1. A commit or build identifier in version and session metadata
  would make deployment verification direct. It need not block the prepared release.
- Leave parked controllers alone. No evidence here calls for a timeout or an
  automatic restart. Historical blockers need their underlying conditions fixed.

Focused tests for launcher failure, retained work, controller panic, and read-only
per-task timing pass. This review changes notes only; the release candidate is unchanged.

## Implementation and local release, 2026-10-12

The user approves the proposed plan and requests a goal. At this goal's start,
another session has already committed the 0.8.0 content and bump in `d87ca35`.
Preserve those commits. Follow-up diagnostics first land as 0.8.1, then the
verified build-stamp correction lands as 0.8.2. No published tag is replaced.

Implemented and committed:

- `a83d909`: retain executable identity in JSON version output, controller
  metadata, and durable session events. Retain public agent provider and cost
  basis in usage records and report unknown or mixed bases in statistics.
  Publish review log identity and 30-second heartbeats. Preserve agent tools,
  result contracts, cancellation cleanup, and existing acceptance checks.
- `ffc70a3`: synchronize the existing parallel runtime fixture. The first full
  candidate run reproduces a timing flake: one short worker ends about one
  millisecond before the other starts. The shared start barrier fixes the test
  without changing production scheduling.
- `a81b33b`: record verified database recovery and mark the old release
  preparation report as historical.
- `5724707`: version-only 0.8.1 bump. Installation exposes missing automatic
  Go VCS metadata in the clean worktree, even with `-buildvcs=true`.
- `1d7629d`: explicitly stamp revision and modified-source state in normal
  builds, race builds, and installation. Fixtures cover clean and modified
  repositories, detached worktrees, version overrides, and archives inside an
  enclosing repository. Archives retain revision `unknown`.
- `29377a8`: version-only 0.8.2 bump after final checks.

Final 0.8.2 validation passes: `make test`, `make race`, all 53 runtime tests in
both runs, build-stamp fixtures, static analysis, packaging, skill checks, and
Darwin arm64 compilation. A final isolated runtime check passes on the clean
release build. It checks task timing and session build identity against Git HEAD.
The installed binary matches the clean build byte for byte and reports:
release 0.8.2, revision 29377a8f7f9083d71f32ffd055aa37d71ec02b3e, modified false.
Installation updates future runs and installed skills; existing controllers
retain their original executable. No branch push, tag, GitHub release, or
Homebrew formula update occurs. Darwin runtime validation remains a follow-up.

Diagnosis decisions:

- All 28 retained margin exit-126 logs are empty. Prompts are small. The lost
  evidence cannot establish the cause. A fixture confirms that current launch
  handling distinguishes a non-executable target from a started target returning
  126. No circuit breaker based only on that exit code is added.
- The cost sample calculates exactly to 0.59734892 USD from the pinned rates.
  The agent reports 1.485591 USD with cost basis unknown. Preserve provenance
  rather than changing rates or applying a multiplier.

Project recovery progress:

- A fresh 18:40Z observation shows margin stopped, and commonstate active with
  84 done and 13 blocked. Nine blockers cite the old Compose address-pool failure.
- The accepted setup now contains a default-bridge workaround. An isolated
  fixture from the accepted source tip runs that database portion and
  `pnpm db:check` successfully against PostgreSQL 18.6. The SQL round trip takes
  36 milliseconds. Its temporary container and volume are removed successfully.
- Nine guarded requeue requests target T057, T059, T100, T102, T104, T106,
  T107, T109, and T111. A guarded settings request saves the tested setup for
  the next controller run. All ten requests are still pending during synchronous
  review at the final acknowledgement check. Submission is not application.
  Request records: `/tmp/looper-release-requeue-requests.json` and
  `/tmp/looper-release-setup-request.json`.
- T050 needs the expiry-policy choice: an operations setting or a bounded
  per-request field. Keep the seven-day default until the user answers.
- T108, T113, and T114 need the two-host benchmark profile in SPEC section 26.2.
  Recorded evidence says no usable provisioning credentials or second dedicated
  host are available. Host information and configured access remain required.

The goal remains active for controller acknowledgements and unresolved project
inputs. Do not restart the active controller, guess a product policy, or report
unacknowledged requests as applied. Preserve the other session's pending notes
and AXINBOX metadata. Detailed diagnosis: `docs/research/MONITORING-ACTIONS-2026-10-12.md`.

### Goal continuation audit, 19:11Z

The previous goal turn makes concrete progress: the final 0.8.2 build is
installed and verified. This continuation checks current state. Main remains
at 29377a8. The installed release still reports that exact revision with a
false modified flag. Only existing working notes and AXINBOX metadata are dirty.

The commonstate process, PID 2861537, is confirmed live with `ps`. It moves
from review into planning, and planner activity advances. Ten queued requests
remain unacknowledged, with no request-error events. A bounded wait polls the
same live process and request IDs. Do not resubmit or restart it because a
client observation times out. Completion remains unproven until acknowledgements
arrive and required project policy and host inputs are supplied.

### Blocked audit, 19:12Z

The original implementation turn and both continuations retain the same missing
project inputs: the expiry-policy choice and usable benchmark-host access.
The previous continuation is a verified wait on live PID 2861537 and the exact
queued request IDs. This audit reconfirms that process, advancing planner events,
and nine requeues plus one setup request still pending. The release remains
installed at 0.8.2, revision 29377a8, modified false.

No safe implementation step remains without the missing inputs or a controller
boundary that acknowledges requests. Do not guess the policy, invent host access,
restart the live controller, or claim queued requests are applied. The blocked
threshold now reaches three consecutive goal turns. Mark the goal blocked;
leave the authorized requests queued and the live controller running. Resume
when the user supplies the project inputs or controller acknowledgements arrive.


## Notes action review, 2026-10-12, 19:27Z

Read the full notes and check current request acknowledgements. The installed
binary remains 0.8.2 at revision 29377a8, modified false. Earlier shutdown,
finding identity, timing, build identity, cost provenance, and review visibility
work is complete. Darwin runtime validation and live Zcode transport validation
remain follow-ups. Publication remains outside this review.

The ten requests are no longer pending. The setup request
`settings-545679dbaf4e41e2` is applied: saved for the next controller run.
All nine requeue requests have conflict acknowledgements: "Wait until active
work finishes before requeuing this task." They did not reopen the tasks.
Retry requires fresh guards after checking the current task and session state.

Commonstate PID 2861537 exists at the first process check, then disappears
during this inspection. At 19:26:57Z, status and doctor report no controller
ownership and retained work. Process records show T115, T132, and T128 ending
around 19:26:46Z. The last retained event is 8020 at 19:26:42Z; no exit reason
is recorded. The cause remains unknown. Do not infer a crash or request failure.
The retained view reports 84 done, 14 blocked, and 20 integrated. It does not
match an active controller.

Next project actions: establish why the controller stopped, retry the nine
requeues with current guards, and resume retained work with the saved setup.
The expiry-policy choice and benchmark-host access remain required inputs.
This review updates notes only. It does not restart a controller, change a
project backlog, resubmit requests, install a build, or publish a release.
