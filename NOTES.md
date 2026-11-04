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

## Commonstate integration error, 2026-10-12, 19:51Z

The user reports `looper tui` exiting with "project files changed during
integration; verified work is retained without marking the task done".
Read-only status, doctor, events, Git snapshots, and check artifacts establish
the cause. No controller or project edits occur during this investigation.

- The failed restart is session `run-030588487ba971b6`, installed Looper
  0.8.2, revision `29377a8`. Events 8021 and 8022 record its start and error
  shutdown at 19:50:44Z. No controller owns the project at observation time.
- A retained publication belongs to T115, attempt `task-f0840342fc489adf`.
  Its verified commit is `8f0104cd27cdabe6b2584635439ff892ee4462e5`.
  The journal records `ok: true`; the retained verification log ends with
  passing tests. The task remains unfinished.
- The accepted tree is `0f96dc87d867b1a600d006a05da4cf4bdaa0ef80`.
  The candidate tree is `5a1b6465ab1ef188d8919146c290bb0f810db178`.
  Repeating both snapshot comparisons with a temporary Git index yields
  `c1991927972480a92f34027d4bbf6230db5d9655`, which matches neither.
  These comparisons leave the user's index and files unchanged.
- Relative to the accepted tree, 17 files differ: submission expiry wiring,
  operator configuration, documentation, and conformance coverage. Examples
  include `packages/server/src/proposals.ts`, `deploy/compose/docker-compose.yml`,
  and `conformance/expiry/expired-submission-refused-without-sweep.fixture.json`.
  Their modification times range from 18:39:58Z to 18:58:20Z. They precede
  T115's check completion around 19:26:46Z. File contents do not establish
  which person or process made those edits.
- `publish()` in `internal/looper/integration.go` accepts only the complete
  accepted tree or candidate tree. It applies the candidate only when the
  accepted-tree comparison matches. This restart fails that comparison and
  exits without applying the candidate. The guard protects existing edits.
  The message's "during integration" wording does not establish when edits
  occurred. The previous controller's exact exit reason remains unrecorded.

Evidence and separate binary patches for local edits and T115 are retained in
`/tmp/looper-integration-diagnosis/`. Recovery must preserve the local edits,
restore the accepted Looper tree, resume retained integration, then merge and
verify the local edits against the integrated result. Git HEAD is older than
Looper's accepted tree; restoring HEAD is not equivalent. Do not delete the
runtime state or mark T115 done by hand.

Smallest useful improvement: report the differing file paths and both expected
states in the integration error, with recovery guidance. A regression fixture
should edit an unrelated project file before publication, assert the diagnostic
names it, and confirm the file, task, and publication journal remain retained.
Existing publication guard tests pass with `go test ./internal/looper -run
'^TestPublish' -count=1`. No source change or live recovery is requested.

### Diagnostic fix

The user requests a fix. The integration error now identifies the task,
accepted tree, verified candidate tree, and retained runtime directory. It lists
up to 20 differing paths per tree and reports any remaining count. Recovery
guidance tells the user to preserve local edits, restore the accepted tree,
resume Looper, and merge and verify local edits afterward. The comparison and
publication rules do not change.

The new regression test adds an unrelated local file before application and
after application. Both cases reject publication and retain the file, staged
Git index, candidate files, task status, accepted tip, and durable journal.
The focused publication tests, full Go suite (`go test ./...`), static analysis
(`go vet ./...`), and diff checks pass. No installation or commonstate recovery
occurs as part of this diagnostic change.

### Authorized commonstate recovery, 20:04Z

The user asks the agent to perform recovery. A fresh observation confirms no
controller owns commonstate and the saved reverse patch still passes its check.
Back up the original local patch, verified T115 patch, current status, user
index, and task file to `/home/agent/.looper/recovery/commonstate-20261012/`.
Reverse only the saved local patch, then run `looper start` from commonstate.

The new session is `run-f7803453a9ce5f6b`, PID 2662749, release 0.8.2.
At 20:05:02Z, controller ownership and view matching are true. T115 is done,
the publication journal is clear, and the accepted tip is its verified commit.
Planner events continue through 8050. No task status is edited manually.

The local edits are restored in `/home/agent/commonstate-local-recovery`, branch
`recovery/local-edits-20261012`, based on T115's integration commit. Three
conflicts in two files combine the event notifier and expiry settings in
`http-server.ts` and `listener.ts`. Both settings remain in every affected
service object. The resolved changes are staged. A separate merged binary patch
is saved beside the original backup. No commit or publication occurs.

Pinned dependency installation, build, required-database tests, and staged diff
checks pass. Verification uses a temporary PostgreSQL 18.6 container on the
default bridge with an assigned loopback port, independent of the live run.
Service conformance passes all 48 regular cases and the recovered expiry case
(49 total, zero failures or errors). Temporary database cleanup succeeds.
Verification logs and exit codes are copied to the durable recovery directory.

At 20:09:05Z, the same controller owns commonstate, the view matches, T115 is
done, and the publication journal remains clear. The event cursor advances from
8033 to 8080 during this recovery. The live project retains only Looper-owned
changes; the recovered local edits stay staged in the separate worktree.

## Docker postgres accumulation, 2026-10-12, 20:25Z

Found during a host disk-space check. The root filesystem is 89 percent full
(129G of 146G used, 18G free). `/var/lib/docker` holds 40G and grew by 24G
during the inspection itself. `docker ps` lists 85 `looper_*` containers, all
running: 83 postgres instances plus two task-named databases. All are
`postgres:18.6-alpine` pinned by digest.

- **The accepted setup command provisions one postgres per task run.** The
  commonstate profile (`~/.looper/commonstate-5b2241b5/runtime/profile.json`)
  names the container from `LOOPER_RESOURCE_PREFIX`. Every task receives a
  unique prefix, so the `docker container inspect` guard never matches and
  each task starts a fresh container on the default bridge network. The
  database is required: the verify command (`pnpm test`) needs `DATABASE_URL`.
  Compose stays unusable on this host ("all predefined address pools have been
  fully subnetted"), so the setup script runs `docker run --network bridge`
  with a digest pin, a free loopback port, and matching durability settings.
- **Nothing removes the containers.** The setup script only creates. Task
  agents sometimes remove their own (`t128`, `T115` notes record "removed
  afterwards"), but no controller path enforces it. Containers from sessions
  that ended hours ago remain running. At observation, the live commonstate
  daemon (PID 2662749, session `run-f7803453a9ce5f6b`) dispatched new tasks
  every few minutes, so creation is ongoing.
- **Two containers are agent-made, not setup-made:** `looper_t132_pg` and
  `looper_2639a612be532232ed96_t128_pg`, private test databases named by
  their tasks.
- **Looper-side implication:** a session teardown or container lease that
  removes `LOOPER_RESOURCE_PREFIX` resources would stop the accumulation.
  Until then, growth is one container plus volume per task run.

Cleanup path: stop finished sessions, then remove `looper_*` containers and
their volumes when no live session owns the prefix. The commonstate session
was live at observation; do not stop it without user confirmation. Docker
(40G) plus `~/.looper` (9.9G) hold most of the recoverable space; the rest is
project build artifacts outside Looper's scope (`~/margin/target` 12G,
`~/uhm/target` 2.4G, Go build cache 2.2G).

### Accumulation audit, 20:26Z

The user asks to check this finding. Current Docker evidence confirms the
accumulation: 88 containers total, 87 running PostgreSQL containers. Of those,
86 have Looper-related names: 59 start with `looper_`, and 27 start with
`sw-looper`. All 86 have a Docker volume mount. Some started on October 11.
The remaining PostgreSQL container belongs to `shared-work-dev`.

Docker reports 127 volumes totaling 8.509 GB, including 1.921 GB classified
as reclaimable. The root filesystem remains 89 percent full with 18 GB free.
These figures do not independently verify the earlier 40 GB directory size
or its reported growth. Docker's aggregate storage report and directory size
measure different things.

The saved commonstate setup creates a container on the default bridge. It uses
`LOOPER_RESOURCE_PREFIX` and an inspect guard, but contains no `docker rm`,
`--rm`, or cleanup trap. The verify command contains no cleanup either.
Source confirms that the prefix derives from the operation artifact directory
(`agent.go:339`), not only from a task or controller session. Worker invocations
and candidate checks can therefore create separate resources for one task.
Revise the earlier "one container per task run" description to one per distinct
operation prefix that reaches setup. The actual container count per task can
be greater than one.

At 20:26:34Z, commonstate session `run-f7803453a9ce5f6b` still owns the project.
No containers, volumes, or networks are removed in this audit. Prefixes and
container age alone do not establish that deletion is safe. A lifecycle fix
needs explicit resource ownership and cleanup after workers and checks finish,
plus recovery for resources whose owner ended. A trap in the setup shell alone
would remove the database before the later worker or check uses it.

### Resource lifecycle implementation, 2026-10-12

The user authorizes the lifecycle fix and removal of confirmed leftovers.
The first regression run fails because both successful and failed checks leave
their fixture database behind. The worker fixture also lacks cleanup evidence.

Implement a project-independent cleanup contract. Setup and agents receive
`LOOPER_RESOURCE_CLEANUP_FILE`. They register an idempotent Bash script before
creating external resources. A durable `resources.json` records the operation
directory, prefix, controller session, process identity, status, and errors.
The worker owns the setup-to-report lifetime; the check owns setup, verification,
and assessment. Nested agent calls reuse that lifetime. Other agent operations
have separate leases. Cleanup runs after the operation finishes, not after setup.

Cancellation retains ownership and resources. Recovery preserves leases for
unfinished workers and checks, skips live owners, and reclaims completed work.
Cleanup failures retain a retry record and emit an event without changing the
verification outcome or consuming repairs. Cleanup runs from the artifact
directory with a separate 30-second deadline. Invalid ownership fails closed.
Linux uses the process start identity; Darwin uses the process start time from
`ps`. No broad Docker removal is built into Looper.

Commonstate's prepared setup registers Docker cleanup before provisioning. It
labels private containers with the exact operation prefix and artifact directory,
records their IDs, checks both labels before removal, and removes their anonymous
volumes. Two guarded settings requests are acknowledged as applied, with the
setup saved for the next controller run. A real Docker fixture confirms that
worker and check containers and their volumes disappear after completion.

The old commonstate controller receives a graceful drain request. T124 and T126
remain active at 20:49Z; their databases remain protected. Installation and
restart initially wait for the completed drain. After 25 minutes, the drain still
waits on T124's verification and T126's worker. Use the tested cancellation and
resume path to activate the fix. Verify the old session and process identity,
send SIGINT, and confirm shutdown event 8759 records cancellation with exit 130.
Both tasks retain their work and logs. Register their three remaining private
databases in durable leases with exact container IDs and zero-client guards.

Host cleanup removes 87 completed-operation PostgreSQL containers and their
unshared volumes. The audit maps exact prefixes to retained operation directories
and task state, checks recorded process identities, then rechecks zero connected
clients immediately before removing each container by ID. Two older T054
databases lack process sidecars but map to the completed task. The task-named
T132 database is separately identified in the prior audit and its task is done.
Thirty empty Compose networks belong to those retired operations and are removed
by ID. Shared development services, unfinished tasks, and unidentified volumes
remain untouched. Free root disk space increases from 18 GB to 23 GB.

Audit manifests, removal results, prepared setup, and the real Docker fixture
result are retained in `/home/agent/.looper/recovery/docker-cleanup-20261012/`.
The existing integration diagnostic changes and recovered commonstate branch
remain separate work. Source changes are not committed or published.

Final validation passes: the full `make test` checks (54 runtime tests), the
full race checks on the first candidate, the final Go race suite, and focused
ownership, deadline, failure, cancellation, and recovery tests. The additional
runtime test kills the controller with SIGKILL and confirms that its abandoned
lease survives until resumed work finishes. Both cancellation and SIGKILL
recovery fixtures pass. Darwin arm64 compilation passes; Darwin execution is
not tested here.

The final labeled Docker setup also records container IDs at creation and
removes a stale ID file before replacement. Its real Docker fixture passes after
this change. All three guarded setup requests have applied acknowledgements,
with the final script saved for the next controller run. The script remains
idempotent when a candidate operation reuses an artifact directory.

The normal fixed binary is installed and matches `build/looper` byte for byte.
It reports 0.8.2, revision `d66ef125c3a98bf914e3dddaa050af4028ba74bb`, modified
true. That intervening commit belongs to another session's notes work; preserve
it. The resource fix is still uncommitted. No version bump or publication occurs.
At 20:56Z, the old controller still drains: T124 has advanced to verification,
and T126 continues implementation. Do not claim the active controller uses the
installed fix until its replacement starts.

The first replacement, `run-d46bbe33227d38fb`, starts on the fixed build at
21:06Z. It resumes T124's verification and T126's worker. Recovery cleans both
T124 leases and retains T126's database until its worker finishes. Source and
logs remain retained; task status is not edited manually.

Activation exposes a separate command precedence defect. Retained operations
reuse the old plan's setup command before the controller replans, despite a
configured setup saved for the new run. Both new regression tests fail for this
reason. Workers and checks now apply the immutable operation's configured setup;
checks also apply its configured verification command. This keeps startup
configuration authoritative without changing scopes or repeating planning.
The focused tests pass. Repeat full validation and replace the controller with
the command-precedence correction before claiming cleanup is active.

The final command-precedence build passes `make test`, including all 55 runtime
tests, the Go race suite, static analysis, packaging, build stamps, and skill
checks. Both real-process recovery fixtures also change setup and verification
commands between runs and confirm that resumed workers and checks use the new
values. The real Docker fixture passes on the final installed binary. Darwin
arm64 compilation passes again. Final logs are copied to the recovery directory.

Cancel the interim session through the same retained-work path, capture its
remaining container IDs, and append guarded cleanup to its leases. Preserve any
cleanup the worker already registered. The final controller is
`run-ac9291e173998d32`, PID 1785667, started at 21:12:54Z. Its build matches the
installed normal binary: 0.8.2, revision `d66ef125`, modified true. At 21:16Z,
ownership and view matching are true. T124 is done; T126 is retained for resume
while the controller replans. The saved setup exactly matches the tested script,
and no nonempty setup environment override is present.

Three migrated T124 operation leases clean their old databases during recovery.
Two old T126 databases remain under retained leases until that task finishes.
The shared development PostgreSQL service remains outside cleanup. Unknown
unattached volumes also remain untouched. The final controller uses configured
commands at each execution boundary, even while its persisted plan still shows
the previous setup during replanning. No backlog status is edited manually.

## Barista bootstrap failure, 2026-10-13, 11:57 local

The user reports `looper` in barista failing at bootstrap with
`execution: exit status 1`. The bootstrap event log
(`~/.looper/barista-ec294870/runtime/bootstrap-35de505f7f0949b9/events-00.jsonl`)
shows the spawned `claude` CLI failed its first API call with HTTP 400,
"Credit balance is too low" (`billing_error`, request
`req_011Cfz37kF1SLYTgq1pEgBkq`). The profile pins `agent: claude`, which
authenticates through `ANTHROPIC_API_KEY`. Not a Looper or project defect:
the Anthropic API account behind that key has no remaining credit. Fix on
the user side by topping up the balance or switching the agent, then rerun.
The bootstrap died before task creation, so a fresh run starts clean.

Resolution: run `CLAUDE_BIN=claude-zai looper`. Looper resolves the claude
agent command from `CLAUDE_BIN` (config.go), so the user's existing Z.AI
wrapper supplies the token, base URL, and GLM model mapping. The `pass`
entry resolves without prompting. Caveat: the wrapper calls `pass show` per
agent invocation; a cold gpg-agent cache could block a headless run.

### Follow-up diagnosis after the rerun, 12:01 to 12:18 local

Four sessions in `state.db`, none reaching a controller:

- 11:57 `run-39e9fe669896fa8f`: the billing failure above, status failed.
- 12:01:43 `run-4f39faa5451b464d`: cancelled after 4.7 s, hooks and init
  only. Model glm-5.3[1m], apiKeySource none, so the wrapper was active.
- 12:01:51 `run-c7732d328275a1d1`: cancelled after 16 minutes. This is the
  real attempt. The bootstrap agent worked normally for about 2.5 minutes
  (32 assistant messages, 20 tool results, reading the design and code),
  with the last timestamped event at 10:04:23Z. The log then shows 5,222
  `thinking_tokens` ticker events with no further assistant output and one
  `api_retry` (error "unknown") at line 7518, spanning 13.5 minutes until
  the user interrupted. The turn stalled mid-completion on the gateway.
- 12:18:03 `run-005db0f2f6f1eb17`: cancelled after 1.9 s.

No Looper defect: sessions, processes, and cancellation are recorded
correctly, the barista tree is clean, and no backlog was created. The
wrapper's `API_TIMEOUT_MS=3000000` (50 minutes) removes timeout pressure,
so a stalled gateway stream hangs until a human interrupts instead of
erroring and retrying. A post-mortem probe through the same wrapper
returned promptly, so the stall was transient. Next step for the user:
rerun `CLAUDE_BIN=claude-zai looper` in barista. Optional hardening:
shorten `API_TIMEOUT_MS` in `~/bin/claude-zai` so dead streams fail fast.

### TUI shows STOPPED during bootstrap, 12:24 local

The successful rerun (session `run-2fc4e78638cb970b`, foreground) displays
"LOOPER STOPPED" in the TUI header while the bootstrap agent works. Cause:
`StatusLabel` (internal/ui/dashboard.go) reports STOPPED when the poll
returns no published controller view, and no view exists until the engine
starts after bootstrap. The run history lines come from state.db, so they
update while the banner claims STOPPED. Improvement candidate: a distinct
BOOTSTRAPPING label while `controller.json` names a live process that has
not yet published a view. The 12:24 bootstrap streamed about 9 KB/s on
glm-5.3 through the wrapper, healthy unlike the earlier stall.

### Bootstrap visibility implementation, 2026-10-13

Implements the improvement above, authorized by the user as "implement all":
a distinct BOOTSTRAPPING state, an evidence-driven braille spinner, and
last-activity age in the TUI.

- The observer synthesizes a snapshot before the controller publishes its
  first view: a live owner from `controller.json` plus a missing or stale
  `view.json` yields a bootstrap view carrying the newest
  `bootstrap-*/events-*.jsonl` path, size, and idle seconds
  (internal/looper/dashboard_data.go). A dead owner still reports nothing,
  so STOPPED keeps its meaning.
- The TUI shows BOOTSTRAPPING with a `bubbles` braille Dot spinner that
  animates only while the bootstrap event stream grows between polls, with
  a five second grace period for sparse writes. A stalled stream freezes
  the spinner and states "no agent output for Xm" after one minute. The
  board panel names the stream size, output age, and the [l] log shortcut.
- `plannerLog` accepts bootstrap streams, so [l] shows readable bootstrap
  agent activity through the existing activity parser.
- Coverage: label table case, board evidence and stall rendering, spinner
  growth lifecycle through `Update`, observer synthesis including stale
  view and dead owner cases, and bootstrap log path validation.

Uncommitted; no install or publication.

## Barista diagnosis, 2026-10-13, 11:21 to 11:24Z

Looper-diagnose observation of `/Users/air/dev/barista`, requested by the
user. Installed Looper 0.8.2, revision `397baaf09057`, modified false. No
fault is established. The run is healthy.

- Session `run-5b6a18d9ff145e6c` (PID 62187, `looper tui`, foreground)
  owns the project. It started at 11:18:40Z. Controller ownership and view
  matching are true.
- Board: 3 done, 52 todo, 0 blocked. T010 ("Brewing data framework") is in
  flight with zero repairs.
- The worker is active. Its agent stream grew from 1,568 to 23,789
  estimated thinking tokens during the observation, at roughly 113 tokens
  per second, with no `api_retry` events. This is a long thinking phase on
  a large task, not the gateway stall seen earlier the same day.
- The previous session `run-2fc4e78638cb970b` ended uncleanly. Resume
  events 201 and 202 record `previous_exit_reason: "unknown"` with last
  event 199. The detection and the retained-work resume worked as designed.
  No shutdown event exists, so the end was a hard kill or a closed
  terminal, not a SIGINT drain. Cause on the user side; no Looper defect.
- Usage at observation: 5.9M tokens, 2.46 USD estimated against 7.66 USD
  reported. This matches the known estimated-versus-reported provenance
  difference already documented. No new finding.

Next useful evidence if the run misbehaves later: the same status, doctor,
and event commands, plus the worker stream tail to separate generation
from a ticker-only stall.

## Test suite status on Darwin, 2026-10-13, 13:35Z

Correction to the diagnosis above: the unclean end of
`run-2fc4e78638cb970b` at about 13:07 local was caused by this Looper
development session, not the user. A `pkill -f "looper.t"` meant to stop a
stuck test binary also matched `looper tui` and killed the live barista
controller. Retained-work recovery behaved as designed; the 13:18 session
resumed with no lost work. Lesson recorded: process patterns must not
match live controllers.

Full-suite findings while validating the bootstrap visibility work:

- The `internal/ui` package passes with the new bootstrap coverage.
- The `internal/looper` package fails on Darwin at clean HEAD 397baaf
  (verified by stashing the working tree and reproducing). The failures
  are independent of the bootstrap visibility change.
- Failure class 1, path duality: TestParse* compare a `/var/folders/...`
  root against a `/private/var/folders/...` spelling of the same
  directory. Darwin resolves the `/var` symlink where the flag path does
  not. Nine config tests fail on string comparison.
- Failure class 2, publication guard: TestRunPublishesAfterAssessmentTimeout,
  TestRunNonzeroAssessmentExitStillPublishes,
  TestRunWithoutAssessmentAddsNoProcess,
  TestCancelDuringAssessmentResumesVerification, and
  TestLargeTaskRunsFromCompleteSnapshot report runtime files
  (`plan-*`, `state.db-*`, `index-*.lock`) inside tree comparisons, so
  publication refuses to apply candidates. Likely the same root: symlink
  evaluation confuses repo-root or worktree boundaries on Darwin.
- Failure class 3, daemon fixture hang: TestRunDaemonReadinessAndCleanup
  blocks on an `__exec codex exec` helper that never returns. The first
  timed-out run also left a detached test daemon behind; both were
  cleaned up. A wedged run may also swallow the Go test timeout.
- Prior validation of this tree ran on Linux. Darwin runtime validation
  was already an open follow-up; these are that follow-up arriving.

The bootstrap visibility change remains uncommitted with green UI tests,
a green build, and clean vet. Fixing the Darwin suite is separate work
that needs its own decision.

## Barista drain after sleep diagnosis, 2026-10-13, 13:08–13:10Z

Requested diagnosis using looper-diagnose; no restart, cancellation, project
edit, or implementation requested. Installed and running build: 0.8.2,
397baaf09057, unmodified. Session run-5b6a18d9ff145e6c, controller PID 62187.

- Observed: controller ownership and view matching remain true, draining=true,
  5/62 done, T009 implementing. Event cursor stays 536 across observations;
  last tool event is Bash at 12:31:19Z. Worker log stays 7,433,348 bytes,
  last modified 13:00:27Z, ending with api_retry (unknown error, attempt 1/10).
  No new output is observed after full wake. This supports a quiet API wait,
  not proof of a permanently hung process or a known network failure cause.
- macOS power log: clamshell sleep 14:31:44 local, intermittent dark wakes,
  full lid wake 15:06:25. Sleep plausibly interrupted the API connection;
  causation is not established. claude-zai sets API_TIMEOUT_MS=3000000
  (50 minutes), so a quiet request can wait a long time before failing.
- Controller, supervisor PID 57957 (matching 14:13:52 start), and Claude child
  PID 57958 remain alive. T009 has retained modified and newly created files
  in its private worktree, but no completed check or integration record.
- Confirmed Looper defect: doctor says worker_process_missing and status
  reports running=false for that live process. controller.go runIdentityCommand
  only reads Linux /proc; Darwin sidecars lack identity. observeWorker requires
  a nonempty matching identity and mistakes unknown for absent. Expected:
  platform-appropriate identity checks and explicit unknown evidence.
  Smallest fix: Darwin identity implementation shared with observation/timing;
  verify current-process observation on macOS plus exited and reused PID cases.
- Visibility defect: fresh STOPPING snapshots and Finishing active work hide
  the old worker activity and API retry. Expected: drain identifies the pending
  task, last output age and latest retry/wait evidence without asserting progress.
  Verify with a quiet stub worker during drain; snapshot refresh alone must not
  imply worker progress. Drain intentionally stops scheduling new tasks and
  waits for active work; waking does not clear drain.

Papercut reports: pc_01M4TP48PG72TBV66284QT3VM9 (drain visibility),
pc_01M4TP48Q844MED6GA2WX1HVVZ (Darwin false missing-process warning).
No live processes were signaled or restarted. Recovery, if requested, should
use a controlled interrupt retaining work followed by resume; avoid broad
process-name matching and account for the broken Darwin signal identity check.

### Drain resolved; log and statistics readability, 2026-10-13, 13:11–13:15Z

User asks whether Claude logs were checked, why the overview says the worktree
was removed, and requests easier logs/statistics formatting. Rechecked the
Looper-owned Claude stream (not private agent transcripts). At 13:11:24Z it
still ended in the same API retry, with no growth since 13:00:27Z. It then
recovered without intervention: event 537 at 13:11:56Z is fresh Bash activity;
538–539 record structured output and a result at 13:13:01Z. T009 passes its
executable check at 13:13:19Z (90 tests per retained result), integrates as event
545, and events 546–547 record a clean drained shutdown with exit 0. Two
follow-ups were added, leaving six completed tasks out of 64. No restart needed.

The worktree message originates only from attemptDiff. Successful tasks remove
their temporary worktrees in engine.result; retained logs remain available.
The user's log screen selects completed T010, while the overview names active
T009. T009's worktree was present during diagnosis. The original selection that
triggered the error is not retained, so T010 is likely, not proven. The global,
unqualified error persists on the overview and incorrectly suggests work loss.

Changes in this turn (preserve preceding uncommitted bootstrap work):
- Completed-task missing worktrees open an informational diff view naming the
  task and explaining cleanup, with directions to retained logs. Other stat
  errors retain the actual cause and task ID instead of claiming removal.
- API retry records become readable activity. Structured-only logs without
  recognized actions show an unknown-progress explanation rather than raw JSON;
  plain-text tool output remains available.
- Log presentation collapses repeated blank lines and highlights suites,
  summaries, passes, failures, and retries. All nonempty text remains available.
  Log/diff footers show back and scroll shortcuts; completed task logs are labeled.
- Statistics headings and effort bars have color, checks distinguish success
  and failure, daily headers share the data column widths, and leading days
  without recorded effort or usage are summarized rather than filling the chart.

Two new regression cases failed before the changes (cleanup error and raw retry
JSON). The full internal/ui suite passes, including viewport sizes and scrolling;
focused Looper diff/log/activity tests pass. go vet on both packages and diff
whitespace checks pass. Inspected the rendered 100x24 statistics fixture. Full
Darwin suite remains subject to the previously documented unrelated failures.
No install, commit, publication, or live-run mutation. The Darwin process identity
bug remains a separate unresolved issue; these edits do not fix process absent.
Papercuts: pc_01M4TPDZCFBXF1CDV9NXTDZE1C and pc_01M4TPDZD6DC0FDDWSB49RDJCN.

### Clear terminal outcome after TUI drain, 2026-10-13

The user shows the local TUI returning silently to the shell at 15:13:20 and
asks whether it died. Events 546–547 confirm the clean drain at 15:13:19;
runDashboardController discarded normal engine output and printed only errors
after Bubble Tea restored the terminal. The clean shutdown was invisible in
shell scrollback. Papercut pc_01M4TQESVAK9AHDV8SN8A72NBX.

The local dashboard wrapper now prints its recorded session exit reason after
returning from the TUI. Drain says active work finished and Looper stopped
cleanly, followed by a resume command with a reminder to use the usual agent
settings. Cancellation says work is retained; completion, no runnable tasks,
and dispatch limit have distinct messages. Missing or inconsistent exit records
fall back to the actual exit code and an events command, never an invented
success reason. Existing error output and exit codes are preserved. The query
matches the exact controller session, so another session cannot supply its reason.

Regression: drain and cancellation tests fail before the change due to empty
terminal output, then pass afterward. Focused local-dashboard drain, cancel,
error, session matching, completion-message and missing-record checks pass;
go vet and diff whitespace checks pass. The pre-existing full controller
completion fixture fails on this Darwin machine: despite its intended finished
state, it enters review and tries /bin/false (absent here). That unrelated
fixture issue is not changed. No installation, restart, or publication.

### Agent identity and overview hierarchy, 2026-10-13

User requests the current agent name and easier scanning of the overview,
with a screenshot of planning in barista. Planning/review snapshots now carry
an activity_agent; planning uses the captured operation configuration, and
clears the name when it ends. Controller metadata retains the bootstrap agent
for observers before the first snapshot. Worker rows show each attempt's agent.

The operation and agent appear before long activity paths. The overview uses
bold operation/project labels, green completion, blue work counts, yellow
blockers, and dim metadata, paths, queue/capacity and timing. Text labels stay
available without color. Existing unrelated working-tree changes are preserved.

Validation: full internal/ui suite; focused snapshot, planner, observer,
bootstrap and build checks; go vet for UI and Looper; diff whitespace check.
Agent visibility is checked at widths 45, 80, 100 and 160. No installation,
commit, publication or live-controller restart. The previously recorded full
Darwin suite failures remain outside this change.

### Startup label wording, 2026-10-13

Replace CONNECTING with LOADING while the TUI waits for its first snapshot.
The user pointed out that connecting suggests network activity; loading
describes fetching the initial presentation state for local and attached views.
Updated the existing state-label test; internal/ui tests pass.

### Commit and local installation validation, 2026-10-13

User authorizes committing all pending work, pushing main, building and
installing. Full UI suite, focused Looper dashboard/log/planner/bootstrap/
observer/snapshot/build checks, dashboard drain/cancel/error cases, and
go vet ./... pass. A broader selection reproduces the documented Darwin
failures in TestLargeTaskRunsFromCompleteSnapshot (runtime paths entering
publication comparisons) and TestDashboardControllerCompletion/complete
(absent /bin/false). These pre-existing issues remain unresolved.

## Commonstate check wedge, 2026-10-13, 06:35Z

The commonstate controller (run-ac9291e173998d32) sat idle from 03:40 to 05:54
local time because one check's verify command hung forever. Findings from the
post-mortem:

- The check ran `pnpm test` in worktree check-5e2945f97f4982f6 at integrate
  commit 0624940. A CLI test failed with EADDRINUSE on the fixed default
  operations port 127.0.0.1:9090 (a concurrent review session's `shared-work
  serve` held it). The failing start path leaks the already-bound main
  listener, so the test process never exits; `node --test` has no default
  timeout, so the suite hangs. Reproduced end to end outside the daemon.
- The verify process had no timeout and no liveness watchdog: "1 checking"
  persisted for ~3 hours while no task could finish, and the dead check held
  its leased Postgres container for the same period.
- Session cleanup retried and logged "context deadline exceeded" on every
  task completion while the check hung; it could not reclaim the task's
  resources until the verify process died.
- The verify log lagged about 30 lines behind the process (buffered recursive
  pnpm output), so the log looked frozen mid-suite and the spec reporter's
  failure detail (printed only at run end) was never captured. The recorded
  failure evidence carries no cause.
- I unblocked the run by signaling the stuck runner; the signal killed the
  parent (no --report-signal installed on that process), the check was
  recorded as failed, and the controller began repairing T151 although T151's
  own work was sound. The repair re-provisions the benchmark VM pair.

Follow-ups for looper: add a configurable timeout (or progress watchdog) to
check verify commands; flush or tail verify output so the log tracks the
process; capture partial output on abnormal termination so failure evidence
names the failing test; make sure a dead check's leased resources are
reclaimed without waiting for the next completion.

Resolution addendum, 2026-10-13: the commonstate-side follow-ups are closed.
Task T215 (98dc0b3) fixed the leaked-listener catch path and moved the CLI
suite's operations port to ephemeral; the runner-level per-test timeout
(--test-timeout, SHARED_WORK_TEST_TIMEOUT_MILLIS override) landed as PR #1 on
nibzard/commonstate from branch fix/port-collision-hang, commit 0d3f57c.
The looper-side follow-ups above (verify timeout, streamed output, partial
capture) remain open.

## Release 0.8.3, 2026-10-14

The user runs the release runbook and picks 0.8.3 from HEAD. The untagged
0.8.0 through 0.8.2 stay untagged by choice, so v0.8.3 is the first published
tag since v0.7.1. The 0.8.3 notes state that those entries are included.

Release notes land first (a67821f). The full suite passes on that tree: go
test, vet, build stamp checks, 55 runtime tests in 100.7 seconds, package
smoke, and skill regression. The version bump follows as its own commit
(80ba239). The release helper repeats the full suite against the bumped tree
(55 runtime tests in 107.7 seconds) before publishing.

Publication: tag v0.8.3 on 80ba239, main and tag pushed, GitHub release
created from docs/release-notes/0.8.3.md, and the tag archive downloaded,
verified, and hashed into Formula/looper.rb as f48cbdd. The installed binary
reports release 0.8.3, revision f48cbdd, modified false. That revision adds
only the formula change to the tagged content. Existing controllers keep
their previous executable.

## Commonstate follow-up control, 2026-10-14

The user requested fixes for cleanup, evidence retention, and follow-up
scheduling. Credential rotation, external publication controls, and live
infrastructure changes are outside this change.

- Observed: Commonstate T125/T137/T138/T141 repeatedly reported the same
  missing access. T170 emitted an operator-only publication follow-up;
  T179 then ran as a coding task. Retained prompts and structured results
  establish the sequence without relying on private agent reasoning.
- Cause: `addFinding` deduplicated only normalized titles and appended every
  finding as `todo`. No ownership marker survived into the ready queue.
- Change: follow-ups carry `owner`, `key`, and `existing_task_id`. Repeated
  keys or explicit task references record evidence in the task-linked journal.
  Existing acceptance criteria and completed status remain intact. Finding
  identity tags are excluded from task contract hashes, so new evidence
  cannot invalidate a running worker. Operator ownership remains in the hash.
  Keys use existing task tags, so project backlog schemas need no migration.
- Operator work becomes blocked with an explicit unblock condition and a
  `looper:operator` tag. The scheduler excludes that tag even after a generic
  retry. A narrow legacy wording check also excludes tasks explicitly
  assigned to an operator. This is scheduling policy, not a security sandbox
  or a substitute for enforcement of external-action permissions.
- Commonstate owns the maintained benchmark cleanup helper and its fake-HTTP
  regression tests. The helper uses Agentlab's supported POST destroy route,
  validates exact ownership, fails on unavailable inventory, and verifies
  physical absence. No Agentlab API extension was needed. Historical cleanup
  scripts and active resource leases remain unchanged.
- Recovered Commonstate evidence lives in `benchmarks/evidence/2026-10-14-recovery/`.
  Its manifest records hashes, source paths, redactions, and missing remote
  reports. The original T170 write-up remains missing; its patch is rebuilt
  from the retained commit. No measurements or infrastructure actions were repeated.

Regression tests first failed on duplicate creation, operator dispatch,
lost identity, reopening completed work, and invalidating active contracts.
A legacy wording regression also verifies that operator-facing product
requirements remain coding work.

Validation: `make test` passes, including 55 runtime tests, build stamp checks,
package smoke checks, and skill checks. After the final classifier refinement,
`go test ./...`, `go vet ./...`, and focused race tests pass. Commonstate's
29 root tests pass, including 12 cleanup cases. All 221 recovered file hashes
match, available source hashes match, and every archived JSON file parses.
Source changes are not installed, published, or applied to a running controller.


## GitPickle bootstrap sources and skill scope, 2026-10-14

GitPickle session run-0f555d74014a88ae accepted a bootstrap source pointing
at to-do.json.context/bootstrap-73c63b41735051bf/backlog.json. The retained
snapshot contains an empty backlog. Removing the bootstrap worktree made
that source unavailable to T001. This is a source validation defect, not
missing product requirements.

Bootstrap now rejects private context paths, absolute paths, and paths
outside the project before saving the backlog. The prompt explains source
ownership. A stub-agent regression reproduced acceptance before the fix.
Existing run state and contracts are not rewritten by this change.

Rename todo-json-manager to looper-backlog, git-conventional-commit to
git-commit, and release-runbook to github-release. Narrow backlog guidance
to Looper and prefer existing project release processes. Default installation
includes only looper-diagnose and looper-backlog; --with-workflow-skills adds
the generic helpers. Existing global skill copies are not removed implicitly.
Preserve the checkout's existing follow-up scheduling edits.

Validation: make test passes, including all Go tests, vet, build stamp checks,
55 runtime tests, package smoke, and skill helper checks. All four skill
manifests pass quick_validate.py. The bootstrap regression fails before the
fix and passes afterward; path tests include custom backlog names and valid
project context files. No live-agent skill activation evaluation was run.
No binary or global skills were installed, and no active run was changed.

## Latest-release installer, 2026-10-14

The user requests commit, push, build, and installation, plus a curl entrypoint
for future release installs. scripts/install-latest.sh resolves GitHub's latest
release redirect and downloads that tag's source archive over HTTPS. It builds
with the release's installer. The current v0.8.3 release has no binary assets.
The default installs only the binary; --with-skills opts into that release's
bundled skill policy. A pinned --version skips latest-release discovery.

Eight isolated tests verify argument handling, download failures, invalid
redirects and tags, missing installers, failed builds, and temporary cleanup.
The README distinguishes released code from main. Existing unrelated
follow-up scheduling edits remain outside this commit and installation.

## Unattended recovery implementation, 2026-10-15

Implement UNATTENDED-RECOVERY-PLAN.md in the existing controller and journal.
Keep the pending stable finding keys and operator ownership changes.

- Verification distinguishes failed commands, failed snapshots, and file changes.
  Retain a name/status comparison beside each check. Bound feedback to 20 paths.
- At a drained idle boundary, consult once for proven empty private bootstrap
  sources or repeated setup/check failures. Missing credentials alone do not
  trigger consultation. Reuse an optional proposal from task diagnosis.
- Save consultation identity before calling the agent. Save application and
  reopened task state through the existing export journal. Consume both original
  and corrected command identities. Restart resumes the retained retry.
- Only remove a private bootstrap source with a retained empty backlog snapshot.
  Preserve implementation acceptance criteria. Require retained work to reopen.
  Check private backlog edits separately because Git snapshots exclude that file.
- Persist previous and replacement effective commands in run state. Correct a
  setup override only when it references proven disposable context. Correct a
  verification override only when future task requirements preserve it. Later
  user command changes take precedence. Global profiles remain unchanged.
- Administrative closure requires the looper:orchestration tag, the removed
  private source in the task details, and a reason. It never adds integrations.
  Legacy recovery tasks without this evidence stay blocked.
- Show executable and controller builds in structured status. Show both builds
  in normal status when they differ and explain that restart changes the build.

The synthetic combined fixture retains T001 work, corrects its source and gates,
reports a generated .cache path, repairs its ignore rule, integrates T001, and
starts and integrates T002. It also runs after losing in-memory state directly
following repair application. All agent calls use local shell fixtures.

Validation: `make test` passes, including all 55 runtime fixtures, static
analysis, installer checks, build checks, packaging, and skill checks. Focused
race tests pass on the final controller changes. Diff checks pass. No installed
binary or unrelated live run changes.

Validation findings: an initial runtime pass times out on the first-commit
fixture. The next pass completes that fixture but fails the dispatch fixture's
two-second timing assumption. Replace that sleep with a bounded start barrier:
T002 remains active until T003 starts. This directly checks overlapping dispatch.
Both previously failing fixtures pass together. Recovery dispatch saves state
only when a recovery outcome changes. Retry failures and integrations now save
with their task results. A stale consultation returns to backlog refresh before
review or dispatch. The final full suite passes with all 55 runtime fixtures. Focused race tests
pass on the final changes, including retry outcomes and overlapping dispatch.

## Release preparation, 2026-10-15

The user authorizes committing all current changes, pushing, following the
release runbook, and local installation. Select 0.9.0 for unattended recovery
and finding ownership. Include the pending finding changes and working notes.
Commit release content first, then change VERSION in its own commit. The
combined source passes make test and focused race tests before preparation.
GitHub tags and release publication remain a separate runbook phase. Existing
controllers keep their executable; local installation does not restart them.

## GitPickle diagnosis, 2026-10-15, 05:09 UTC

Use the looper-diagnose skill for read-only observations of /home/agent/gitpickle.
Installed Looper is 0.9.0 at 8a5affd, modified false. Compare status at
05:09:12Z and 05:09:46Z: no controller ownership, no workers, unchanged accepted
revision and backlog, and no events after 1238. No restart or project edit.

The last controller is run-8f8986e91f7b7e90, Looper 0.8.3 at a09e08d.
Event 1238 records an error shutdown at 2026-10-14T23:15:01Z: no space left
on device while writing the task-file export. T006 has a matching context
mkdir failure. T012 exits 101; its Looper-retained log contains Codex recorder
errors with StorageFull and operating-system error 28. This establishes disk
exhaustion for both tasks. It does not identify which workload filled the disk.
Current capacity is 25 GB free, 88 percent used; inode usage is 9 percent.
The current project and Looper runtime total approximately 246 MB.

Seven implementations integrated. Nine tasks are done, including the two
administrative closures T038 and T040. Sixteen tasks are blocked: T006 and
T012 on disk exhaustion, four on provider resources, three on deployment or
performance resources, and seven on naming evidence. All 26 todo tasks depend
on blocked work. No task is ready. External blockers contain stale references
to the old bootstrap snapshot; accepted source_files contains only gitpickle.md,
and the workspace and corrected current-stage commands exist.

The export journal is pending. The project task file matches the saved prior
export hash and still shows T006 as doing; saved execution state shows it
blocked. Startup can replay this known export transition. T012 retains changed
archive code, documentation, ignore rules, and a new capture test. T006 retains
its workspace but has no product changes. Preserve both workspaces.

Recovery recommendation: let startup finish the pending export, explicitly
reopen T006 and T012 after checking available disk capacity, then resume retained
work. Version 0.9.0 does not automatically reopen disk-exhaustion blockers;
this is an external resource failure, not a proven orchestration contract defect.
Provider resources, deployment access, and the naming decision remain separate
inputs. No new Looper source defect is established by this incident.

Possible observation improvement: doctor should explicitly flag a pending
export and explain that the task file can lag saved execution state. Verify
with an export-journal fixture where saved T006 is blocked and its prior export
still says doing. Preserve the existing guarded journal replay behavior.

Evidence snapshots and structured task inspections are retained locally in
/tmp/looper-gitpickle-diagnosis-20261015/. Raw agent logs remain in the runtime
directory and are not copied into project documentation.

## GitPickle backlog staging, 2026-10-15

The user requests questions first, then backlog edits only. No controller,
implementation task, provisioning, correspondence, or product test is started.
Owner decisions: develop/test here with R2 and a disposable private GitHub test
repository; verify configured Cloudflare/GitHub access before provisioning.
Defer actual S3/B2 tests, the real-user study, fresh-VM deployment, and full-size
benchmarks until after implementation. Final VMs use AgentLab on pve2. Naming
clearance is the last final-validation prerequisite. Preserve original release
requirements rather than claiming those external checks passed.

Update the accepted backlog and its task-file projection through Engine.export
under the repository lock. Replay the previous pending export first. A one-shot
Go overlay helper calls only state/export functions; it starts no engine session
or agent. Back up SQLite, the accepted state, the original task file, and the
proposal in runtime/backlog-maintenance-ad73c53df82b425c before mutation.

Split local and final acceptance. T052 gates implementation/local evidence.
T053/T054 cover final S3/B2 access and evidence. T055 covers fresh pve2 VMs.
T056 gates complete public-release acceptance. T051 runs full-size benchmarks
only on the final pinned VM. T031/T032 defer the entire real-user study. T033
retains naming requirements after other final validation. T034 now prepares
local release documentation, with complete release acceptance retained in T056.

Administratively consolidate eleven duplicate resource/naming reports into
T004, T028, and T033. Their entries retain original details plus explicit
administrative closure reasons. These closures establish no product completion
or external evidence. Preserve all nine previously completed tasks, all attempt
records, seven integrations, and T006/T012 implementation contracts. Clear their
resolved disk blockers to queue retained work for a future run.

Validation: project JSON schema passes. Dependencies are acyclic, and T052 has
no deferred final gates in its dependency closure. State and task-file content
match; the export hash matches; export_pending is false. T004, T006, T012, and
T028 are ready. The controller remains stopped. Only the requested backlog and
its execution-state projection change in GitPickle.

## GitPickle follow-up diagnosis, 2026-10-15, 06:38 UTC

Compare status at 06:38:16Z and 06:38:34Z. The controller is stopped, workers
are absent, and no attempts or integrations follow backlog maintenance event
1239. This matches the owner's backlog-only instruction. Installed Looper is
0.9.0 at 8a5affd. The backlog has 33 todo, three final-stage blockers, and 20
done entries. Eleven done entries are administrative consolidations; actual
integrations remain seven. T004, T006, T012, and T028 are ready. T006/T012
retained workspaces still exist. The project task file matches saved state and
its export hash. No export is pending. No new Looper fault is established.

Disk capacity has fallen again: 192 GB used of 195 GB, with 2.4 GB free
(99 percent used). Current largest home-directory users include walgit at
54 GB, oss-ai-stack-map at 20 GB, and margin at 17 GB. walgit/target accounts
for 53 GB of the walgit total. GitPickle's Looper runtime is approximately
243 MB. These observations identify present storage use; they do not prove
which workload caused the previous disk-full shutdown or recent growth.
Reclaim appropriate build output before starting further builds, after checking
its owning workload. No files are deleted, no resources are provisioned, and
no controller is started during this diagnosis. Evidence is retained in
/tmp/gitpickle-diagnosis-repeat/.

## GitPickle check, 2026-10-15, 08:22 UTC

Disk now has 42 GB free (79 percent used). Session run-105e6eb1bd25faae runs
Looper 0.9.0 at 8a5affd with a matching live view. Between 08:22:27Z and
08:22:37Z the event cursor advances from 1744 to 1747, and T015 emits new
activity. Seven prior integrations increase to eleven: T012, T014, T006, and
T013 integrate at events 1343, 1409, 1551, and 1669. T015 is running; T018
and T028 are dependency-ready. No export is pending.

T004 is the new development blocker. Event 1452 and its saved summary report
verified GitHub identity and R2 listing access, but the configured GitHub token
lacks delete_repo for disposable-repository cleanup. No resources were created.
The three other blockers remain the explicitly deferred final gates. This
check changes no GitPickle files, permissions, resources, or controller state.

## GitPickle diagnosis, 2026-10-15, 13:18 UTC

Session run-105e6eb1bd25faae still owns the repository with a matching view,
Looper 0.9.0 at 8a5affd. Compare 13:18:38Z and 13:19:23Z: event cursor
2695 and 20 integrations stay unchanged; no workers or checks run. Event 2695
records waiting at 11:13:30Z. Disk has 54 GB free, 73 percent used. Nine
implementations integrate after the previous check: T015, T016, T018, T017,
T019, T020, T021, T028, and T024. The backlog has 33 done, 19 todo, and
four blocked tasks. Thirteen done entries are administrative; integrations
remain the product progress measure. No export is pending.

External blocker: T004 still requires disposable GitHub cleanup permission.
A read-only GitHub API request confirms the current OAuth scopes are gist,
read:org, repo, and workflow; delete_repo is absent. Every pending task has
T004 in its unresolved dependency ancestry. The remaining operator blockers
are the explicitly deferred T032, T033, and T053 final gates.

New Looper defect: confirmed review findings on completed coding tasks have
no runnable remediation. Review result review-058c32b98169f46f/result.json
reports three independent agent-owned findings with stable keys and parent IDs:
remote-archive-readback-verification and large-git-pack-capture refer to T012;
b2-durable-archive-publication refers to T006. Events 2685 through 2687 attach
these keys and evidence to those tasks, leave both done, and create no tasks.
The final review then waits despite reporting three implementation gaps.

The retained review reports a missing remote pack still producing a current
and verified result because readback uses the local cache; a 64 MiB child-output
limit preventing a roughly 68 MB pack from being published; and B2 capability
handling that prevents durable cloud publication. These are the review's
reproductions, not independently rerun experiments during this diagnosis.
Deferred actual S3/B2 provider tests do not establish that B2 implementation
already satisfies the product contract.

Cause confirmed in internal/looper/findings.go: existing_task_id selects a
completed task, the existing-task branch adds the new key and event, then
returns without scheduling work. This behavior was added to preserve completed
status and avoid repeated prerequisite tasks. It also suppresses distinct,
evidence-backed coding defects that refer to a completed implementation.

Expected: preserve completed implementation history while giving a new verified
agent defect a separately tracked, runnable repair outcome. Keep operator
prerequisite deduplication and active contracts unchanged. Smallest useful fix:
distinguish the completed source task from the active finding task, deduplicate
by stable defect identity, and route repeated reports to that repair task.
Already attached unresolved findings in retained journals need a recovery path.
Verify with a completed T012, two distinct defect keys referencing it, and a
second report of either key: two runnable repair tasks, stable deduplication,
and unchanged completed T012 contract/status. Include an operator duplicate
case and live-contract preservation coverage. No fix or project mutation is
performed during this diagnosis. Evidence lives in /tmp/gitpickle-diagnosis-current/.

## GitPickle GitHub permission and retry, 2026-10-15

The user completes GitHub CLI device authentication for delete_repo. A read-only
API response confirms scopes delete_repo, gist, read:org, repo, and workflow.
The user then authorizes reopening T004 for provisioning. The live controller
is still run-105e6eb1bd25faae. Remove T004's operator-only tag and obsolete
permission blocker from the idle backlog, retaining blocked status for an
explicit guarded retry. The controller imports the edit at event 2696 and
starts a synchronous review at event 2697.

Submit request requeue-cf69082b341f931b with the current session and task
revision through the dashboard request transport. The request is pending during
review. Verify its acknowledgement before claiming application or resubmitting.
The user's authorization now includes the T004 retry and its existing disposable
R2/GitHub provisioning scope. Other project defects and deferred gates remain
separate work. The controller is not restarted.

The review re-adds T004's operator tag at event 2711 despite the verified
permission and explicit user delegation. Request requeue-cf69082b341f931b
receives a conflict acknowledgement because that task revision changes. After
review completes, apply owner removal and todo status together in a guarded
atomic task-file edit. The controller imports the combined edit, retains T004's
agent ownership, and starts planning its retry at approximately 13:33 UTC.
No controller restart or direct database mutation is used. The normal requeue
transport cannot change ownership, and a separate ownership-only edit triggers
an unnecessary final review before retry. A combined delegation/requeue request
would avoid this delay and review-driven reversal.

Retry dispatch confirmed at 13:34 UTC: T004 is doing, with a running worker
using retained attempt task-1249c40e5cfd6e43 and fresh work session
24ef263ea0dac0ad. Worker activity appears through event 2804. This establishes
that the task is reopened and execution resumes, not that provisioning or
integration has completed. GitHub delete_repo remains verified.

## GitPickle remaining work, 2026-10-15, 13:48 UTC

After the authorized T004 retry, event 2859 reports a private GitHub test
repository and disposable R2 bucket created, with listing/readback/privacy and
cleanup checks passing. T004 remains blocked: Cloudflare scoped-token creation
returns HTTP 403, code 9109, and R2 S3-compatible runtime credentials are absent.
The GitHub delete_repo issue is resolved; R2 listing access alone does not
establish authority to mint credentials or run signed adapter tests. Do not
change token permissions or claim live provider acceptance during this check.

The latest completed review, ending at event 2883, reproduces the 64 MiB
capture limit and adds a restore-restart defect: accepted restore jobs remain
queued after reconstructing daemon state because restore execution queues are
initialized empty and only fresh plan_apply submits execution. B2 protection
also remains unsupported by the conditional-create path. Events 2873–2875
again attach distinct findings to completed T012/T014/T006 without runnable
repair tasks. Product remediation and the Looper finding-routing fix remain
separate required work. At 13:48 the controller owns the project and reviews
another refreshed baseline; integrations remain 20. No project edits occur.

## GitPickle R2 credential handoff, 2026-10-15

The owner supplies bucket-scoped R2 S3 credentials for gitpickle-dev-tests.
Store only the access key, secret key, and nonsecret storage configuration in
/home/agent/.config/gitpickle/r2-test.env with directory mode 0700 and file
mode 0600. Do not copy credential values into notes, task files, or output.
The separate Cloudflare API token is not stored.

A bounded signed S3 probe verifies listing, object write, readback, and delete
under gitpickle-development/.looper-access-check/. The exact owned temporary
object is removed. Its nonsecret receipt is r2-access-check.json beside the
private environment file. This verifies access, not the full provider contract.

Update T004 through a checked atomic backlog edit while the live controller
is idle. Remove operator ownership and queue todo in the same edit. The new
contract explicitly sources the private runtime file, permits only owned-prefix
object cleanup, and preserves the owner-provided bucket. Token creation and
its old HTTP 403 are no longer prerequisites. Keep GitHub disposable-fixture
cleanup and prior resource evidence separate. No secrets enter the backlog.
Controller import and retry dispatch remain to be confirmed.

Follow-up status confirms the live controller imports the credential handoff.
T004 is todo without operator ownership, and the private file mode is 0600.
No worker dispatch or full provisioning completion is claimed at this point.

## GitPickle review repair, 2026-10-15, 19:58 UTC

The owner requests fixes after diagnosis of session run-58f364dd4667b5a2.
Events 5178 and 5179 report unresolved B2 publication and guided setup defects
against completed T006 and T020. Event 5190 waits with no runnable work.
The running binary and source both use revision 8a5affd.

Fix finding handling by reopening completed coding tasks and appending repair
criteria and evidence to idle coding tasks. Keep completed attempt history,
operator ownership, blockers, and active or retained contracts protected.
This uses the existing task identity rather than the separate repair tasks
suggested by the earlier diagnosis. Multiple findings retain all repair criteria;
exact repeated reports do not duplicate them. Clear stale completion and plans.

Regression tests reproduce the original failure by ID, key, and title. Additional
cases cover multiple defects and retained attempts. Targeted tests pass.
The full check suite is in progress. No binary is installed or controller restarted.

Recover the live GitPickle run through its supported idle backlog refresh.
A guarded atomic edit reopens only T006 and T020 with the current review evidence
and concrete acceptance criteria. Validate the schema and dependency graph first.
Preserve the five external operator blockers and deferred live provider tests.
Back up the original and proposed task files in /tmp/gitpickle-review-repair-*.

Recovery confirmation: event 5191 imports the edit and event 5192 starts planning
in the original session. At 20:01:17 UTC, T006 has a running worker and T020
remains queued. The five operator blockers remain unchanged. These observations
confirm resumed execution, not completed product repairs.

Validation complete: make test passes, including Go tests, go vet, eight installer
tests, build stamp checks, 55 runtime tests, package smoke tests, and skill checks.
git diff --check passes. Changes remain uncommitted. The built fix is not
installed, so the live controller still uses its original binary.

The owner then requests installation. Run ./install.sh --skip-skills to install
only the fixed binary at /home/agent/.local/bin/looper. No controller restart
is requested or performed. Existing controllers retain their loaded binaries.

## Melt review recovery and clipboard, 2026-10-18

Observed Melt session run-19ebffa67b339858 on Looper 0.9.0, revision
8a5affd849b7 with local changes. Events 768-772 show a cancelled review,
restart, and zero workers despite three ready tasks. Five integrations
were saved, but reviewed_at was zero and review_pending was absent.
The milestone limit then prevented dispatch without scheduling a review.

The run loop now derives overdue reviews from integration counters and
persists review_pending before launching review. The regression fixture
loads the old inconsistent state, cancels two reviews, and confirms that
successful review permits worker dispatch. It uses only stub agents.
Idle snapshots expose the pending review as a dispatch reason.

Planning, bootstrap, import, and finding guidance now separates development
rounds and local proposal preparation from external publication. Findings
can provide defer_until. Looper retains these as blocked tasks with a
looper:deferred tag and a concrete resume condition. Generic retries cannot
dispatch them. They remain required work. Task views show deferred status;
doctor reports an informational finding instead of an urgent blocker.
Existing operator restrictions remain separate and unchanged.

The c shortcut copies full run state, the selected task and blockers, or
an open log, diff, or activity view. It copies data rather than a clipped
viewport, strips terminal controls, and does not intercept search input.
OSC 52 output goes through Bubble Tea's terminal release and restore path.
The notice reports a sent request, because terminals do not acknowledge
clipboard changes. Real terminal clipboard integration remains untested.

Preserved pre-existing edits to findings.go, findings_test.go, and this file.
No installation or live controller restart is part of this source change.

Validation: make test passes, including Go tests, static analysis, installer
and build checks, the runtime suite, package smoke tests, and skill checks.
Focused race checks pass for review cancellation and restart, deferral,
clipboard output, and narrow screens. Import and requeue regression tests
pass after the final deferral changes. The final binary builds, and
`git diff --check` passes. The binary remains in build/looper; it is not
installed. Existing controllers retain their loaded binaries.

The owner clarifies that copying should prepare a paste into a coding agent.
The c shortcut now creates a self-contained diagnosis request with the project
root, session, snapshot time, event cursor, selected task and blockers, and
commands to verify live state. It retains full blocker text and adds relevant
attempt paths and up to eight recent events. Large logs and diffs have labeled
excerpts. Old snapshots explicitly report an unavailable project root.
Focused clipboard, handoff, snapshot, and narrow-screen tests pass. The binary
builds and the diff check passes. This update is not installed.

The owner requests installation. Installed with ./install.sh --skip-skills.
The installed /home/agent/.local/bin/looper matches build/looper byte for byte.
Version reports 0.9.0 at 8a5affd849b7 with local changes. Existing controllers
are not restarted. New TUI sessions use the agent-handoff copy shortcut.

## Melt controller replacement and retained publication, 2026-10-18

Observed Melt session run-27e0c879608a4e5c end at event 3497 because
Git HEAD moved. T023 had passed its executable check. Its publication
remained retained. The new commit contained the accepted project files
plus the backlog files. A backed-up state repair allowed session
run-5110029aa6d38455 to publish T023 and continue.

The attached overview still showed 22/36 tasks while Tasks showed current
work. Source confirms that overview polling captured the original session,
while task inspection read the current database. An attached viewer now
follows a replacement controller. It rejects actions and task reads until
the replacement session is displayed. Session changes discard pending task
details and confirmation dialogs. Disconnected work shows its last phase
and unknown process status instead of claiming a running process.

Startup recovery now accepts a descendant commit on the same branch only
when committed and working project files equal the accepted tree or the
verified publication candidate. Backlog hashes, staged project changes,
branch identity, ancestry, and normal publication checks remain enforced.
Recovery records the old and new HEAD values. It does not rewrite Git history.
Doctor reports a moved branch and the last recorded shutdown. Status and
inspection label attempts retained when no controller owns the repository.

Isolated regression fixtures cover accepted and candidate commits, rejected
file/index/branch/backlog changes, replacement sessions, disconnected actions,
late task details, stale process labels, and recovery diagnostics. Existing
uncommitted work remains preserved. No live controller is restarted and no
binary is installed as part of this change.

Validation: make test passes, including Go tests, static analysis, installer
and build stamp checks, all 55 runtime tests, package smoke tests, and skill
checks. Focused race tests pass for attachment, startup recovery, diagnostics,
observation, and dashboard behavior. Final UI checks cover stale labels and
confirmation invalidation. The final binary builds and git diff --check passes.
The build remains local; existing controllers and installed binaries are unchanged.

## Melt clipboard through tmux and iPhone, 2026-10-18

The owner reports that dashboard c does not reach the iPhone clipboard while
using Termius through tmux on this VM. Read-only checks find tmux 3.4 with
set-clipboard external, allow-passthrough off, and an attached xterm-256color
client advertising the clipboard capability. Looper emits plain OSC 52.
Tmux documents that external rejects requests from applications inside panes.

Set set-clipboard on in the running tmux server and persist that option in
/home/agent/.tmux.conf. Keep passthrough disabled; plain OSC 52 uses tmux's
clipboard support. The option and Ms capability are verified after the edit.
No Looper restart or binary change is required. Actual iPhone paste remains
unverified and requires the owner to press c and paste on the phone.
If terminal clipboard support still fails, tmux show-buffer displays the
latest accepted payload for manual selection; do not claim this proves
Termius copied it into the iOS clipboard.

## Worktree consolidation, 2026-10-23

The owner requests consolidation of retained worktrees into the main branches.
The existing source edits are committed on main. Context notes record the backup
location and validation. The retained release worktree registration is pruned.

Validation: make test passes. This includes Go tests, go vet, installer and build
checks, all 55 runtime tests, package smoke checks, and skill regression checks.
Four runtime tests initially fail under concurrent builds. All four pass in an
isolated rerun, and the complete make test rerun passes. No binary is installed.
The Gitpickle controller is stopped for repository consolidation.

## Pylemma bootstrap schema failure, 2026-10-27

Observed session run-6ff531538c0cfed1 in /home/agent/pylemma.
Events 1–3 show bootstrap failing with json: unknown field "round".
The installed binary reports 0.9.0 at 8a5affd849b7 with local changes.
The retained bootstrap result contains task-level round fields and priorities
6 and 7. No backlog or saved run exists. The schema permits priorities 1–5
and does not permit round fields.

Bootstrap now supplies the full backlog schema and instructs agents to keep
development rounds in details or tags. Decode, backlog validation, and source
validation failures share one bounded correction attempt. A second rejection
reports the retained result path. Context and project mutation checks remain
in place. Strict decoding continues to reject unknown fields.

Stub-agent regression tests cover task and backlog unknown fields, invalid
priorities, invalid status, successful correction, and repeated rejection.
All bootstrap tests pass. The full make test suite passes, including all 55
runtime tests, static checks, installer checks, package checks, and skill checks.
git diff --check passes.
No Pylemma files are changed, no controller is started, and no binary is installed.

The owner requests installation and Pylemma recovery. Installed with
./install.sh --skip-skills. The installed binary matches build/looper byte
for byte and reports 0.9.0 at 9928fced909b with local changes.

Recovered Pylemma's to-do.json from the retained bootstrap result. Moved
each round value into a development-round tag and capped priorities at 5.
All 24 tasks, dependencies, acceptance criteria, and six blocked statuses
remain preserved. JSON Schema validation, unique IDs, dependency existence,
cycle checks, source existence, and comparison of preserved fields pass.
Original and corrected copies are in /tmp/pylemma-backlog-repair-yy_17khk.
Doctor reports no findings and no controller. No run is started; the owner
can rerun looper tui from /home/agent/pylemma.

## Slit Codex account lapse, 2026-10-30

The user reports `looper tui` in slit failing with `execution: exit status 1`.
Session `run-b4c61acbd2a1e1ef` (Looper 0.9.0, 9928fce with local changes)
ran normally from 12:54Z: bootstrap, three plans, and two integrated tasks.
At 13:13:27Z every Codex invocation started failing within 2.5 s with
`The 'gpt-6-astra' model is not supported when using Codex with a ChatGPT
account.` The owner confirms the cause: the Codex subscription lapsed at that
moment. `~/.codex/config.toml` pins `model = "gpt-6-astra"`; `auth.json` is
in chatgpt mode, so the CLI rejects that model on every call. Not a project
or Looper orchestration defect.

Observed handling: workers T007 and T015 blocked with work retained
(correct). The final review then retried the identical failing invocation
three times in eight seconds and the controller exited with error 1. Every
operator-visible message said only `execution: exit status 1; see <path>`;
the account error sat in the agent stream. Same diagnostic gap as the
barista `billing_error` incident on 2026-10-13.

Fixes implemented (uncommitted; preserved the pending Pylemma bootstrap
edits in the same tree):

- Agent failures now surface the agent's own error text. `streamErrorMessage`
  scans the event-stream tail for Codex `error`/`turn.failed` and Claude
  `is_error` result events and includes a bounded message in the
  `AgentError`: `configuration: exit status 1; agent reported: <text>; see
  <log>`. The text reaches worker blockers, review-error events, the
  shutdown record, and the TUI exit output without opening the log.
- Account and entitlement failures classify as `configuration` instead of
  `execution`: unsupported model for the account, credit balance, billing,
  quota, usage limit. Rate-limit wording stays transient and is checked
  first.
- The review retry loop stops immediately on `configuration` and
  `invalid_request` failures instead of a three-attempt burst. Other
  classes keep the existing three attempts. Work is retained either way.

User-side recovery for slit: restore Codex access (renew the subscription or
point `~/.codex/config.toml` at a model the ChatGPT account supports), rerun
`looper tui`, then requeue the two blocked tasks per the blocker procedure;
their work is retained.

Validation: the new runtime fixture (`review_account`) reproduces the
incident on the pre-change build from a clean HEAD worktree (three rapid
`execution: exit status 1` review errors, no cause) and passes after the
change (one attempt, cause in the review-error and shutdown records, task
work retained). `make test` passes with 56 runtime tests, and focused race
tests pass for the review paths.

Installed 2026-10-30 with `./install.sh --skip-skills` at the owner's
request. The installed `/home/agent/.local/bin/looper` matches
`build/looper` byte for byte and reports 0.9.0, revision 9928fced909b,
modified true. Existing controllers keep their loaded binaries; changes
remain uncommitted and unpublished.


## Stackfall retained-state recovery, 2026-11-02

The owner authorized repair after `looper tui` rejected the saved identity.
The saved task path was `init`; default TUI startup selected `to-do.json` and
bootstrapped before detecting the mismatch. An earlier controller had passed
T007 verification but stopped before publication because the backlog changed.

Recovered through native engine methods in an isolated copy, without agents.
Preserved the edited original backlog, all 28 tasks, and changed documents.
Completed the verified T007 publication, imported the edits as a new baseline,
and migrated the saved identity to the default `to-do.json` task path.
Applied the tested state and exact tree under the repository lock. All original
files and runtime records are backed up under
`/home/agent/.looper-recovery/stackfall-20261102T205212Z`.
Debug and Release builds and CTest pass. Doctor reports only deferred tasks.
No binary was installed and no agent run was started.

Confirmed diagnostic defect: on an unborn branch, doctor calls `rev-parse HEAD`
and compares its failed stdout (`HEAD`) against an empty saved head. Engine
Head uses `rev-parse --verify HEAD` and returns an empty value instead. The
reported branch movement was false. A future fix should share the checked
lookup and add an unborn-branch regression fixture. Startup should also check
saved task identity before bootstrap and report both mismatched task paths.
Full evidence and recovery checks are in `/notes/looper-diagnose-2026-11-02.md`.


## Stackfall full-run session analysis, 2026-11-03

Analyzed all 46 Claude transcripts of the stackfall run plus the runtime
artifacts. Full report with evidence: `/notes/stackfall-session-analysis-2026-11-03.md`.
Run topline: $162.71 recorded, 4,184 turns, 15/18 task checks passed first
try, working game delivered. Agent-side behavior was strong (no test
weakening, clean schema compliance, deep verification). The costly defects
are controller-side:

- T014 burned $23.90 over 6 attempts on a verify gate that cannot pass by
  construction: plan-3ec868cb authored a `git status --porcelain` cleanliness
  gate (induced by the plan prompt's "leave tracked files unchanged" line),
  while `prepare()` delivers the integrated candidate to the check worktree
  uncommitted. Repair feedback was byte-identical across all five retries,
  and the `diagnose()` repeat-escalation never fired because the feedback
  hash includes volatile `worktrees/check-*/` paths. Fix candidates: commit
  the candidate in the check worktree, lint verify commands against the
  delivery model, normalize the feedback hash, cap identical-signature
  repairs, and put attempt counts plus prior result summaries into repair
  prompts.
- Scheduling findings were never acted on: three reviews reported T023
  unblocked and T024 mis-owned, and the loop idled two hours until a human
  edited the backlog. A finding whose `unblock_when` is met should flip the
  task or halt-and-surface.
- Cost is dominated by cache reads (81-91%): the auto-compact threshold
  (~500K on the 1M window) is too high. Compacting at ~150K would cut ~30%
  per large session. `Read` injects 5.4x more context than `Bash`; full-file
  reads of `main.cpp`/`SPEC.md`/`backlog.json` repeat in every session. The
  `analyze_image` vision path on this gateway failed 4/4 and forced both
  auto-compactions; PIL pixel classification is the working standard.
- Minor: `shortText(b.Goal, 512)` truncates every task prompt mid-sentence;
  the 20:25 fail-fast discarded the verified T007 result.

## Stackfall improvement plan validation, 2026-11-03

Validated the plan against the current source, existing tests, and retained T014
feedback. The five repair inputs have distinct hashes from check paths and timings;
their command/error headers match. The repeat-detection defect is confirmed.
The plan misses a failureEvidence caller in recovery.go, repair prompt data flow,
and a defined traversal for review deltas. A header-only signature can hold distinct
test failures. Idle messages improve visibility but do not resolve scheduling.
Smart mode needs explicit model precedence; its role in the incident is unproven.
Six focused existing tests pass. No source implementation or live run is changed.
Full findings: /notes/looper-improvement-plan-validation-2026-11-03.md.

A revised improvement plan from the same analysis is in
`/notes/looper-improvement-plan-2026-11-03.md`. It now prioritizes five local
changes with no new saved fields or configuration options. Repeat detection keeps
diagnostic output and normalizes only generated check IDs and CTest durations.
A local experiment maps all five retained T014 inputs to the same comparison text;
changed command, error, and diagnostic variants stay distinct. Repair context uses
the existing feedback and result files. Startup checks identity before bootstrap.
Long goals refer to the complete accepted goal instead of a clipped value.
Review traversal and model-default changes are deferred. Waiting output is optional.
This revision changes documentation only; implementation and live validation remain.

## Stackfall improvement plan implementation, 2026-11-03

Implemented the five changes from `/notes/looper-improvement-plan-2026-11-03.md`
in the working tree at `9928fce` plus its existing local changes. No saved
fields, configuration options, agent roles, or scheduling rules were added; the
ladder, `MaxRepairs`, and the publication model are unchanged. All edits are
uncommitted alongside the earlier slit and Pylemma work; no binary is installed
and no live run is touched.

- **Check worktree contract:** the planner prompt states that setup and
  verification must preserve the project files they receive, that checks
  receive uncommitted candidate changes in a detached worktree, that an empty
  Git status or HEAD-derived candidate history must not be required, that
  Looper compares project trees to detect changes, and that workers implement
  ignore rules. The worker prompt carries the same explanation in one
  condensed sentence. Legitimate Git-metadata product checks stay valid.
- **Repeat detection:** `failureEvidence` keeps stripping `Artifacts:` lines
  and now normalizes only the observed incident noise: the generated
  16-hexadecimal `check-` identifier directly under `worktrees/` (replaced by
  `check-GENERATED`, with a non-hex boundary so project paths like
  `worktrees/check-1.patch` stay distinct) and the trailing duration of lines
  starting `N/M Test #K:` or `Total Test time (`. Seconds inside test names,
  counts, results, commands, errors, and other diagnostic text keep distinct
  signatures. `repair()` hashes the normalized evidence; `run.go` passes the
  raw candidate feedback so agents keep artifact paths and unmodified output.
  All five retained T014 feedback files map to one value under the final
  regexes; every meaningful variation tested stays distinct.
- **Startup identity:** `initializeState` loads saved state first and rejects
  a version, root, or task-file mismatch before bootstrap or schema writes,
  naming the saved and selected task paths. Saved state also suppresses
  bootstrap when the task file is missing on disk; that case now reports the
  missing saved task file instead of rebuilding the backlog or failing on a
  bare read error. Fresh bootstrap, legacy import, and matching-state recovery
  are unchanged.
- **Repair context:** after computing the signature and incrementing
  `a.Repairs`, `repair()` prepends a header with the repair count, the latest
  worker report bounded to one line (omitted when no summary exists), the
  attempt directory, and two guidance lines. The header and diagnosis text
  never enter the signature. The worker repair paragraph asks the worker to
  read the feedback and the earlier attempt records it names for the failure,
  without rereading every log or rerunning every command.
- **Long goals:** a goal over the 512-byte inline budget is replaced by an
  instruction to read and extract the `goal` field of the accepted backlog
  snapshot; short goals stay inline. `shortText` is unchanged.

Adversarial multi-agent review confirmed eight findings; all were fixed before
final validation: the CTest normalization replaced every seconds value on a
matching line (now only the trailing duration), the identifier pattern
accepted short decimal project paths (now exactly 16 hex with a boundary), the
normalized marker could collide with a literal path (now `check-GENERATED`),
`Total Test time` admitted non-CTest continuations (now requires the
parenthesis), the raw-feedback call site had no pinning test (a reviewer
proved it by reverting the call and passing the whole suite; now pinned by a
Go test and by the runtime fixture's retained `Artifacts:` assertion), the
missing-task-file case surfaced a bare read error, the version/root branch of
the identity check had no test, and this NOTES entry was missing. Reviewer
claims about indented CTest output and the recovery consultation gate were
refuted: the plan accepts other unstable output reaching the repair limit,
and the raw `verification failed` label already satisfied that consultation
gate before this change.

Validation: `go test ./...`, `go vet ./...`, `git diff --check`, and
`make test` pass, including all 56 runtime fixtures. The repeated-verification
fixture now runs a verify command that prints its changing check worktree path
and CTest timings; it asserts two repairs, one controller `diagnosis` event
beside the stub's `diagnose` record, a hold after the third matching failure,
and retained raw `Artifacts:` lines. One earlier `make test` run failed only
`TestCancelledCheckRetainsResourcesUntilRecovery` while review agents built
concurrently inside the repository; it passes in isolation and in the final
clean run, matching the documented concurrent-build flake class.

Remaining limits: prompt wording reduces recurrence but cannot repair saved
plans, task commands, or explicit verification overrides. Unrecognized
unstable output still reaches the existing repair limit by design. The
optional waiting summary was not implemented; deferred work in the plan is
unchanged. No live agent test, installation, or publication was performed.

## Improvement plan diff review, 2026-11-03

Recommend holding the commit until failureEvidence normalization is narrowed.
An exact-source Go reproduction confirms that different commands, project patch
filenames, and unrelated notworktrees directory paths can receive the same
comparison value. Normalize observed output only and require directory boundaries.
The startup identity error also suggests moving the task file, which leaves the
saved identity unchanged and cannot resolve the mismatch. Source edits remain
untouched. go test ./..., go vet ./..., and git diff --check pass during review.
Full findings: /notes/looper-improvement-review-2026-11-03.md.

## Improvement review fixes, 2026-11-03

Fixed both review findings at the owner's request. failureEvidence normalizes
generated check directories and CTest durations only inside observed output.
Directory boundaries preserve project patch filenames, unrelated notworktrees
directories, and suffixed check directory names. Command and error text stays
unchanged. Generated check directories at the end of output still normalize,
including Windows paths. The identity error recommends the saved task file and
no longer suggests moving it without changing saved identity.

New regression tests fail before the fixes and pass afterward. Focused repair,
recovery identity, feedback, and startup tests pass. The exact updated Go
normalizer maps all five retained T014 inputs to one comparison value.
Final validation: make test and git diff --check pass. This includes all Go
tests, static checks, installer and build checks, all 56 runtime fixtures,
package smoke tests, and skill helper checks.
Changes remain uncommitted. No binary is installed and no live run is changed.

## Stackfall dashboard and scheduling correction, 2026-11-03

Research and evidence: [/notes/stackfall-looper-diagnosis-2026-11-03.md](/notes/stackfall-looper-diagnosis-2026-11-03.md).

The user requests a fix after the dashboard lists completed tasks T001 and
T010 as repeated failures. The existing workspace change excludes done tasks
from attention based on retained repeat counters. Its existing regression
test fails against the original selector through a Go overlay, then passes
against the corrected selector. Active failures remain visible. No duplicate
implementation was added.

Stackfall session run-4a22b14f976cf978 ends cleanly at event 4043. With no
controller or retained work, the user-authorized backlog maintenance changes
only T028: add T030/T031/T032 to dependencies, clear obsolete blockers, remove
operator/deferred tags, and set todo. Keep T027 and all acceptance criteria.
JSON schema validation and Looper Backlog.Validate pass. Looper Backlog.Ready
returns exactly T028. This uses the editable backlog boundary supported by
initializeState; SQLite remains untouched until a later run imports it.
All other tasks, including physical and publication gates, are unchanged.
The original backlog is retained at /tmp/stackfall-diagnose/to-do-before-fix.json.

Validation for the authorized fix: go test ./... passes (Looper package
67.3 seconds), go vet ./... passes, and git diff --check passes. The original
selector fails TestWorkspaceAttentionAfterRepair/done; the corrected selector
passes. Installed the verified build with ./install.sh --skip-skills. cmp
confirms build/looper matches /home/agent/.local/bin/looper. The old binary is
retained at /tmp/stackfall-diagnose/looper-before-fix. No controller was
started. The next user-started run imports the corrected backlog and uses
the updated dashboard. Existing source edits remain uncommitted.

## Stackfall import scheduling and event fixes, 2026-11-03

Source: /notes/stackfall-looper-diagnosis-2026-11-03-latest.md. The owner
requests only the Looper fixes. Active STACKFALL work remains untouched.

Two isolated reproductions fail before the fixes: ten import polls emit ten
identical wait events, and a successful check integrates the first task but
exits at the dispatch limit with queued requirements still waiting. The latter
reproduces the boundary skipped between STACKFALL T025 and T026.

Move the retained-work wait event into the ImportWaiting transition. Suppress
per-import drain polling messages; the controller drain outcome already explains
why receipts stay queued. No receipt schema change is needed.

After a check drains all work, run the import pass again in the idle branch,
after baseline/input validation and before recovery, review, planning, or
new dispatch. Preserve stop precedence, cancellation, invalid-input handling,
and active contracts. An import failure becomes a failed receipt; it does not
starve the next queued import or create a retry loop.

Tests use stub agents and an explicitly released executable check. They cover
two queued imports, successful and conflicting proposals, explicit drain,
retained-work wait event counts, and drain event counts. The original selector
fails apply/failed scheduling cases; the drain case already passes and remains
protected. Existing retained-work and refreshed-baseline import tests pass.

Changes are limited to imports.go, the import boundary in run.go, the new
import_boundary_test.go, and this note. All earlier edits remain preserved.
No live agent smoke test, installation, publication, or controller restart.

Final validation: make test passes, including all Go tests, go vet, installer
and build checks, runtime fixtures, package smoke, and skill helper checks.
The new focused regressions also pass with the Go race detector enabled.
git diff --check passes. Validation log: /tmp/looper-import-fixes-make-test.log.
The rebuilt binary is only a build artifact; the installed binary and live
STACKFALL controller remain unchanged. No commit or push was requested here.

## Commit and install, 2026-11-03

Committed all accumulated batches since 8a5affd as cdafca9 "Harden
agent failures, bootstrap, and scheduling": the Pylemma bootstrap schema
fixes, the Slit agent-failure surfacing, the Stackfall improvement plan
with its review fixes, the import scheduling and event fixes, and the
attention-list correction. One commit because the batches share files
(run.go, engine.go, integration.go). Pushed to origin/main. A context
note with the incident history is attached under refs/notes/context and
pushed.

go build, go vet, and git diff --check pass before the commit. The
recorded make test result covers the identical tree. Built and installed
with ./install.sh --skip-skills; cmp confirms the installed binary
matches build/looper and reports 0.9.0 at cdafca95bbf8.

## Dev version stamping, 2026-11-03

The owner requires every build landed after fixes or features to carry a
bumped version. A binary built at 22e1941 still reported 0.9.0 because
VERSION changes only at release time. Chosen format (owner confirmed):
builds after the last VERSION bump stamp <next patch>-dev.<N>, where N
is the commit count since the bump; the revision hash stays in the
existing parenthetical. No release bump now; releases stay deliberate.

scripts/build-ldflags.sh derives the stamp when it is called without an
explicit version: it finds the last commit that touched VERSION, counts
the commits after it, and bumps the patch with base-10 arithmetic
(0.9.9 goes to 0.9.10). The Makefile passes no version by default and
still honors a VERSION override from the command line or environment.
Formula builds, explicit arguments, archive builds, and builds at the
bump commit keep the plain version.

build-test.py gains fixtures for the derived stamp (0.8.2 to 0.8.3-dev.1),
numeric patch arithmetic without carry, and the explicit-argument escape
hatch. docs/release.md documents the stamp; the github-release skill
notes that interim builds may report it.

Validation: build-test.py passes, the derived stamp on this repository
is 0.9.1-dev.4 at 22e1941, make -n confirms the override wiring, and
make test passes end to end, including package smoke and skill
regression.

## End-to-end bug review, 2026-11-03

Full report with all evidence and verifier verdicts:
/notes/looper-e2e-bug-review-2026-11-03.md

An orchestrated review covered the whole repository at the state
committed as cdafca9. Thirteen finders covered ten Go file groups, a
concurrency sweep, the then-uncommitted diff, and the scripts, shell,
Makefile, and Node adapter. Two independent adversarial verifiers
checked every finding; a finding counts as confirmed only when both
upheld it. A completeness critic then added four gap rounds (cost math,
settings queues, prompt transport, uninstall and release scripts).
110 agents total, all read-only; the working tree stayed untouched
during the run (cdafca9, 22e1941, and 2a37e4c were committed by the
owner mid-run; none invalidate the findings). go build and go vet pass.

Result: 52 confirmed (8 high, 16 medium, 28 low), 6 disputed, 3
refuted. The confirmed high findings:

- `looper tui` as the controller installs no signal handlers. SIGTERM
  closes the UI but leaves workers running with the terminal blocked in
  `<-done`; a second signal hard-kills the process with no shutdown
  event (internal/looper/cli.go:98).
- runIdentityCommand reads only Linux /proc, so on darwin liveness
  detection, statistics, and timing drop every live worker
  (internal/looper/controller.go:107, internal/looper/statistics.go:105).
- Request queues wedge permanently once the request directory holds more
  than 100 entries: claiming refuses, the error is only logged, and the
  idle loop spins without sleep with unbounded journal growth
  (internal/looper/run.go:513, internal/looper/settings.go:379).
- classifyFailure scans the tail of the agent's own event log, so
  ordinary project text that matches account or configuration patterns
  aborts the run (internal/looper/agent.go:206).
- The `looper tui` non-interactive fallback re-parses with nil args and
  silently drops --plan and every other flag (internal/looper/dashboard.go:28).
- uninstall.sh deletes skills the default install never installed,
  including user-authored ones (uninstall.sh:142).

No fixes were applied. The confirmed high and medium findings are
tracked as T018 through T036 in to-do.json; T037 triages the low and
disputed findings. Each task cites its finding location and ends with an
acceptance clause. The six disputed findings still need an owner
decision.

## Backlog audit against the codebase, 2026-11-03

Every task in to-do.json was audited against the tree at 2a37e4c
(52 agents: one auditor per task over a frozen backlog snapshot, an
adversarial confirmer for every verdict that would change the backlog,
and one hygiene pass; no file in the repository was modified).

Result for the seventeen done tasks: six verified outright (T002, T004,
T005, T007, T010, T016) and eleven partial. Of the partial ones, ten
deliver the capability but lack discriminating tests for some
acceptance clauses; their gaps are consolidated in new task T038, and
T003 and T012 carry appended audit notes (formula drift and a NO_COLOR
wording correction). T011 was reopened by the owner's decision: the
first-run form can never appear on the looper tui path (cli.go:98
dispatches before the form gate at cli.go:146), the same dispatch-order
defect as T018; it now depends on T018.

Result for the twenty open tasks: seventeen confirmed open as written;
T020, T027, and T036 were corrected. T020's citations were off (the
logging callers are settings.go:662-666, dashboard_data.go:239-242, and
imports.go:142-145; the no-sleep return is run.go:526); T027
undercounted (24 unpriced glm-* ids, not 8); T036 conflated two
distinct defects (unguarded value-option reads on all bash versions and
empty-array expansions before bash 4.4). Notes-level corrections went
into T019 (controllerAlive also breaks looper stop on darwin),
T026 (stranding mechanism is the clearSessionFiles skip at
settings.go:429-432), T028 (the loss window ends at the delete; the
naive fix would break resume-after-drain), and T037 (triage scope is
C25-C52 plus D1-D6; four lows are already folded into T020, T027, T029,
and T036).

New tasks: T038 pins the unpinned acceptance criteria of T001 through
T017; T039 records the detached mode in controller metadata
(publishController always writes "foreground"). Overlap guards:
T022 depends on T018 and T029 depends on T020, so two workers cannot
make conflicting edits to one function.

The hygiene pass claimed T023 sits at priority 2; the snapshot shows
priority 1, so that item was discarded. The backlog now holds 39 tasks:
16 done and 23 todo.

## Stackfall promise review, 2026-11-03

Full report with evidence: /notes/stackfall-promise-review-2026-11-03.md.
The question: across all stackfall sessions (10 controller sessions, 81
transcripts, $304.17 reported total), did looper deliver its README and
manifesto promise? Verdict: yes on the core loop, with integrity intact;
three of the five cdafca9 fixes verified live, two unexercised.

- Fix scorecard live on run-0a86489 (0.9.1-dev.5): import flood gone
  (490,671 poll events over 3 h on the old build; six on the new build),
  import starvation gone (five queued imports processed within 21 min of
  the first check drain), no cleanliness gate composed since 07:30 while
  every prompt from 13:15 carries the delivery-model wording. Repeat
  detection and the repair header remain unexercised: no repeated check
  failure and no repair has occurred since the fixes landed.
- New finding: import conflicts are terminal. Four of five owner-queued
  plan imports ended status failed with sound scope reasoning (two wanted
  to land before the integrated T026; one contradicted SPEC v1.0
  exclusions; one cross-referenced the rejected plan), but no task,
  blocker, or board entry records the follow-up. The plans live only in
  runtime/acks/. A conflict should become a blocked task with its unblock
  condition.
- Deferred gaps confirmed live again: scheduling findings stayed tag-only
  (T028 wiring raised five times over 2.5 h until a human edit), and the
  cost shape is unchanged (compaction at 537K-1.39M; the live T015 gate
  session is the costliest yet at ~$35 and still running).
- Accounting hole: the 15:39Z upgrade stop killed a healthy 77-minute T026
  session mid-edit; its usage report is empty, so cancelled spend is
  invisible. The restart recovered the work only because the agent diffed
  the work copy.
- Hygiene: main is stale since the human's 14:41Z publish (T026's
  integration exists only on refs/looper-tasks), and the project root sits
  on an unborn master. Working-tree publication is correct; git
  presentation confuses humans.


## Stackfall scheduling fixes, 2026-11-03

Evidence and research notes: /notes/stackfall-looper-diagnosis-2026-11-03.md.
Session run-187bae9c1e92c4ba waits at event 496478 with zero ready tasks.

- Confirmed ownership cause: addFinding calls holdForOperator on an idle
  existing software task when an operator follow-up references it. Preserve
  existing ownership instead. New operator tasks still stay outside the queue.
  The regression fails for todo, blocked, and done tasks before the fix.
- Confirmed missing dependency path: import proposals only add tasks and cannot
  attach a new layout task to an existing final gate. Add an explicit gates
  field that only appends dependencies to idle tasks. Validate the combined
  graph before mutation. Preserve completed and retained attempt contracts.
- Confirmed import protocol gap: accepted scheduling deviations have no field
  separate from blocking conflicts. Add adjustments to the prompt, proposal,
  durable receipt, acknowledgement, and CLI output. Keep true conflicts fatal.
  This changes the structured contract, without guessing intent from prose.
- Regression fixtures use stub agents. They cover dependency scheduling,
  atomic rejection of unsafe gates, receipt retention, and ownership.
- The Stackfall backlog repair is separately authorized by the user's
  "fix all" instruction. Preserve its active controller and unrelated changes.
  New source behavior applies only to future controllers; no installation or
  restart is required for the authorized backlog correction.

Validation: isolated `make test` passes, including 56 runtime tests,
Go tests, static analysis, installer tests, build checks, package checks,
and skill checks. Focused race tests pass. The same Stackfall controller
accepted the backlog correction at event 496479 and resumed planning.
Only T015, T029, and T037 changed; device and publication gates remain.
No binary installation, controller restart, commit, or publication occurs.

## Skill installation ownership, T023, 2026-11-03

The installer now records owned skill names in `.looper-installed-skills`
inside the selected skills directory. Reinstalls update owned skills and keep
earlier receipt entries, including optional workflow skills. Existing skills
without receipt entries are skipped, so installation does not claim user files.
Ownership is recorded before each copy so an interrupted copy remains removable.

Uninstall validates the complete receipt before removing files. It removes
only recorded skill directories, then removes the receipt. It needs no source
skill tree. Without a receipt, it preserves legacy skills. Invalid receipts,
including symlinks and incomplete records, stop the command before removal.
The generated receipt is ignored by Git.

Validation: nine ownership regression tests pass. They cover the required
default install, a user-authored `git-commit` skill, and selective removal.
They also cover existing selected skills, workflow reinstalls, updates,
missing source files, dry runs, skip options, legacy installs, invalid receipts,
symlinks, prefixes, and custom locations. The package smoke test passes with
the real binary and the same acceptance case. Eight release installer tests,
the build, shell syntax checks, and `git diff --check` pass.
All installation fixtures stay in private temporary directories. No shared
installation, controller, backlog, Git reference, or repository setting changes.

## T024 retained merge failures, 2026-11-03

Both retained-work merge paths discarded Git errors. Reopening a blocked task
or repairing a result could launch a worker with unresolved conflicts.

Check both merge results. Remove the failed temporary worktree and hold the
attempt with durable conflict feedback. Preserve the original worktree, base,
result commit, and prior verification evidence. Include Git standard output
in merge errors because Git writes conflict paths there. Only count a dispatched
attempt as active when its worker actually starts.

Regression tests create real divergent commits. They cover conflicting and clean
merges for reopening and repair, saved feedback, retained edits, and failed
worktree removal. A full run test confirms that a reopened conflict exits idle
without waiting for a worker. Both conflict tests fail against the original
implementation through a Go overlay and pass with the fix. Ignore generated
`to-do.json.context/` output; existing rules already cover build output.

The runtime conflict fixture expected a repair worker to receive unresolved
conflicts. Update it to the T024 contract: one task integrates, the conflicting
task blocks with its original edits, feedback names the conflict, and no repair
worker starts. The updated fixture passes. Focused Go race tests also pass.

Final validation: `make test` passes, including all 56 runtime fixtures,
package smoke tests, and skill helper checks. Focused Go tests pass with the
race detector. `git diff --check` passes. No installation, commit, push, or
backlog edit is performed.

Repair 1 resolves the integration conflict in this file. Preserve the integrated
T023 ownership note and the retained T024 diagnosis and validation notes.
No implementation conflict remains. The focused retained-merge tests pass again
with the race detector on the integrated tree.
The integrated repair passes `make test`, including all 56 runtime tests,
package smoke tests, and skill helper checks. Both Git whitespace checks pass.

## Catalog pricing coverage, T027, 2026-11-03

The pricing resolver previously searched only the inferred provider. This
excluded GLM entries from other providers and both Codex Mini aliases.

- Resolve explicit provider IDs within that provider only. Preserve the
  Anthropic, OpenAI, and Z.AI defaults when they have a matching rule.
- Otherwise, search exact catalog IDs before model patterns. Catalog order
  resolves provider ties. Document these defaults in `prices/README.md`.
- Treat the catalog's empty price object as an explicit free marker.
  Missing rates, unsupported units, and unknown context tiers stay unknown.
- Iterate all 1,783 entries across 45 providers in the acceptance test.
  Include all 163 free markers. Check both provider-specific and bare IDs.
  Exercise scalar text estimates and the previously excluded model names.
- Ignore Go test binaries and the default coverage output. No build output
  is tracked. The pinned snapshot and its checksum remain unchanged.

Focused pricing, usage, and statistics tests pass with the race detector.
`go test ./...`, `go vet ./...`, and `git diff --check` also pass.

T027 repair 1 preserves both the integrated T024 notes and the catalog pricing
notes. The integration conflict affects only this file; pricing code and tests
merge without conflicts.

Repair validation: all 1,783 catalog entries resolve, including 163 free
markers. Focused race tests and `go vet ./...` pass. The first full Go run
times out in `TestImportAtCompletedCheckBoundary/failed` while waiting for
`Checking T001`. That test passes alone, and the full `go test ./...` rerun
passes. Both Git whitespace checks pass, with no unresolved index entries.

## T026 plan-import crash recovery, 2026-11-03

Startup preserved plan requests in `processing/`, but only claimed files in
`requests/`. A crash after claim left the submitter polling indefinitely.
Session cleanup also removed terminal acknowledgements after request removal.

Recover processing claims after state recovery and pending export recovery,
under the repository lock. Replay a receipt by request identifier before reading
source paths. Without a receipt, validate and capture the unaccepted source
again. Compare its digests with applied receipts through normal acceptance.
Accept the recovered claim across controller sessions. Ordinary unclaimed
requests retain the existing stale-session behavior. Keep retained-work and
publication boundaries unchanged.

Restore acknowledgements from saved receipts. Mark plan acknowledgements with
an optional request type, so session cleanup preserves terminal answers even
for digest replays and rejected requests without their own receipt. Keep claims
when state saves or acknowledgement writes fail. Check request removal errors.
No SQLite schema or task transition changes are required.

Regression tests kill a real helper controller process after claim, capture,
queued receipt creation, application, and request removal. They also cover a
new request with previously applied content. Two restarts must retain exactly
one receipt and one added task, and the original submitter poll must resolve.
Accepted imports recover after source deletion. Additional tests cover receipt
save failures, acknowledgement write failures, and a missing unaccepted source
that must return an explicit error instead of leaving a pending poll.

Generated test coverage is ignored in `/coverage.out`; existing `/build/`
rules cover build output and the temporary regression overlay. All fixtures
use private temporary repositories and databases. No shared service, backlog,
repository setting, installation, or publication changes are required.

Validation: `make test` passes, including Go tests, static analysis, installer
checks, build checks, all 56 runtime fixtures, package smoke tests, and skill
helper checks. The new recovery tests pass with the race detector. An overlay
that disables startup recovery makes the claim regression fail with the
original still-pending poll. Remove that temporary overlay before static
analysis because Go discovers `.go` files even under Git-ignored directories.
`git diff --check` passes. No commit, push, or backlog edit is performed.

T026 repair 1 preserves the integrated T027 pricing notes and test output
ignore rules alongside the retained import recovery changes. The integration
conflicts affect only `.gitignore` and this file.

Repair validation: `make test` passes on the integrated tree, including all
56 runtime fixtures, package smoke tests, and skill checks. The import crash
recovery tests pass with the race detector. Both Git whitespace checks pass,
and no unresolved index entries remain. Existing ignore rules cover generated
build output, Go test binaries, and coverage output.

## Negative persisted intervals, T030, 2026-11-03

Timing reads returned an error when a saved session or process ended before
its start. This also stopped statistics, status, and task inspection queries.
Clamp the end to the start during reads and warn on standard error. Keep the
saved timestamps, database schema, and JSON structure unchanged.

Regression tests cover both tables through a read-only connection. A negative
interval contributes zero to project totals, task totals, and daily statistics.
Healthy intervals still count. Plain and JSON status and inspect commands
remain usable. A Go overlay restores the original error branch and confirms
that the new tests fail against it, without changing the candidate files.

Focused timing, task timing, and statistics tests pass with the race detector.
`go test ./...`, `go vet ./...`, and the Git whitespace check pass.
Existing ignore rules cover
build output, Go test binaries, and coverage output. Tests create their database
and warning files in private temporary directories and remove them automatically.

T030 repair 1 resolves the notes conflict with the integrated T026 recovery
work. Preserve both sections. The timing implementation and regression tests
merge without conflicts. The acceptance tests and focused timing, task timing,
and statistics tests pass with the race detector on the integrated tree.

The first repair run of `make test` times out in the existing
`TestImportAtCompletedCheckBoundary/failed` test while waiting for
`Checking T001`. The complete boundary test passes alone on the same tree.
This matches the timeout already recorded under T027 repair validation.

The integrated repair passes the full `make test` rerun, including all
56 runtime fixtures, package smoke tests, and skill checks. Both Git whitespace
checks pass. No unresolved index entries remain. No backlog, shared Git
reference, repository setting, installation, or publication changes occur.

## T025 completed runs with oversized logs, 2026-11-03

The agent runner rejected event logs above 64 MiB before reading the Codex
result file. A completed run therefore lost its valid result.

Read a bounded tail for oversized event logs. Remove the first partial event
and prepend a JSON line with type `looper.log_truncated`. Atomically replace
only the retained event log. Include the marker within the 64 MiB limit.
Keep result-file limits, result validation, cancellation, and process failure
handling in place. Logs within the limit remain unchanged.

The acceptance test runs the full controller with a stub Codex process.
The process writes a valid result, creates a file, and emits 100 MiB of output.
The executable check verifies the file. The controller publishes the change
and exports the task as done. The retained log contains the marker and final
event. Additional tests cover the size boundary, missing logs, malformed
results, and process errors after oversized output. Focused tests pass with
the race detector.

Existing ignore rules cover `*.jsonl`, `/build/`, `*.test`, and `coverage.out`.
No generated build artifacts are tracked. All test fixtures use private
temporary directories. No external service or shared database is required.

Validation: `go test ./...`, `go vet ./...`, focused race tests, and
`git diff --check` pass. A temporary Go overlay restores the capped log read.
The acceptance test then fails because the task remains blocked (exit code 2).
The overlay uses the candidate source and does not change project files.
No backlog, shared Git reference, repository setting, or installation changes
are made.

## T028 startup drain requests, 2026-11-03

`NewEngine` deleted the stop file before publishing controller identity. This
could discard a request written by `looper stop` or the SIGTERM handler during
startup. Preserve the file through initialization and consume it at the final
safe boundary, after active work drains and state saves successfully. Report
request removal errors instead of claiming a completed drain.

Regression tests write the request before engine creation and after engine
creation. Both runs drain before planning or dispatch, leave the task pending,
and remove the completed request. A new engine then executes the pending task
and completes normally. A separate test covers request removal failure.
Existing signal tests confirm that SIGTERM writes the same request without
cancelling work, while SIGINT cancels.

Focused lifecycle tests pass with the race detector. Temporary Go overlays
restore the startup wipe and retain the completed stop file separately. Each
mutation makes the acceptance test fail for the expected reason. The overlays
use candidate source files and leave project files unchanged.

Existing `.gitignore` rules cover `/build/`, `*.test`, and `coverage.out`.
No generated build artifacts are tracked. Tests use private temporary
repositories and databases; they require no external services.

The first `make test` run fails in the existing
`TestCancelledCheckRetainsResourcesUntilRecovery` test: its completed recovery
retains the fixture database. The test passes alone on the same candidate.
The full checks are rerun without changing that test or the resource code.

The full `make test` rerun passes: Go tests, static analysis, installer checks,
build checks, all 56 runtime fixtures, package smoke tests, and skill checks.
The final focused lifecycle tests pass with the race detector. The Git
whitespace check passes. No backlog, shared Git reference, repository setting,
installation, or publication changes are made.


## T031 unchanged verification during run recovery, 2026-11-04

Run recovery applied the replacement-command guard to every proposal. This
rejected an unchanged command such as `head -c 1 retained.txt` because it
contains no recognized check keyword. Compare the proposal with the effective
recorded command before applying that guard. Include plan commands, explicit
overrides, and saved recovery corrections in that comparison. Preserve all
other recovery validation and the guard for changed commands.

Regression tests cover all three command sources, rejected replacements, and
retained-work validation. Controller tests recover the original keyword-less
command, integrate the retained task, and complete its successor. They also
cover restart after the recovery proposal is applied. Focused tests pass with
the race detector. A temporary Go overlay restores the unconditional guard;
both controller cases then fail because recovery is rejected. The overlay
uses candidate source files and leaves project files unchanged.

Existing `.gitignore` rules cover `/build/`, `*.test`, and `coverage.out`.
No generated build artifacts are tracked. Tests use private temporary
repositories and databases. No external services or shared resources are
required. No backlog, shared Git reference, repository setting, installation,
or publication changes are made.

Final validation: `make test` passes, including all 56 runtime fixtures,
package smoke tests, and skill helper checks. Focused race tests and
`git diff --check` pass.

## T032 nested modules in verification detection, 2026-11-04

The nested-project scan skipped markers that matched the root marker. A root
`go.mod` and a child `go.mod` therefore selected `go test ./...`, which skips
the child module. Count every supported child marker in the existing shallow
scan. Preserve ignored directories and the precedence of explicit Makefile
test targets.

Follow `docs/UI-PLAN.md`, section "Verify command detection": multiple project
roots leave the override empty so the planner selects verification. Do not
replace the planner's command with a parent-only check. Tests cover matching
Go, JavaScript, and Rust markers, ignored directories, and Makefile precedence.
A real Go fixture proves that a root-only check misses a child failure. The
planned command checks both modules, catches the failure, and passes after
the child is repaired. Empty verification remains invalid.

Focused detection tests pass with the race detector. A temporary Go overlay
restores the same-marker exclusion. All three matching-marker cases fail,
and the Go fixture confirms the child failure is missed. The overlay uses
candidate source and leaves project files unchanged.

Existing `.gitignore` rules cover `/build/`, `*.test`, and `coverage.out`.
No generated build artifacts are tracked. Tests use private temporary
directories and require no external services. No backlog, shared Git reference,
repository setting, installation, or publication changes are made.

Validation: Go tests, static analysis, installer checks, and build checks pass.
The first `make test` run reaches runtime fixtures but fails because the root
filesystem fills. Git writes and SQLite report storage errors. Rerun the
remaining checks with a private `LOOPER_RESOURCE_PREFIX` directory under
`/dev/shm`, without deleting shared files or changing tests. All 56 runtime
fixtures, package smoke tests, and skill helper checks pass there. Temporary
files are removed automatically. `git diff --check` passes.

T032 repair 1 confirms that the combined check also failed from exhausted
disk space, rather than a detection failure. Preserve the retained fix and
regression tests. The complete `make test` command passes with `TMPDIR` in a
private `LOOPER_RESOURCE_PREFIX` directory under `/dev/shm`. All 56 runtime
fixtures pass. The temporary directory is removed after the command exits.
Detection tests also pass with the race detector. Existing ignore rules cover
all generated output; no build artifacts are tracked. No shared files,
repository settings, or backlog files are changed.

## T033 settings on the first keypress, 2026-11-04

The settings key set an exit action without returning the quit command.
The next key triggered that command through the dialog handler. Return
`tea.Quit` in the same update that handles `s`. The existing wrappers then
open the settings form. Keep the current dashboard view during this handoff
because settings has no inline dialog view.

The regression test fails before the fix for both `s` and `S`, with local
and attached controls. It passes after the fix and verifies that the view
does not become blank. Tests also preserve read-only rejection and the
existing add-plan, drain, exit, help, tail, and unknown-key behavior.

Validation passes: `go test ./internal/ui -count=1`, the full interface suite
with the race detector, focused Looper dashboard and attachment tests, and
`git diff --check`. Existing ignore rules cover `/build/`, `*.test`, and
`coverage.out`; no generated build artifacts are tracked. This change needs
no external services, installation, publication, or backlog changes.

## Default coding agent changed to Claude, 2026-11-04

Owner feedback after the self-run: the controller used codex workers
because the non-interactive `looper start` path cannot ask the first-run
form, and the built-in default was codex. The owner discovered this only
after workers were running and had to fix it with `looper config` by
hand (the project profile now pins agent=claude and verify=make test).

Change (commit 468284a): the built-in default agent is now claude, the
first-run form preselects it through the same default, and a controller
start with no saved profile and no explicit agent prints a one-line
notice naming the agent and pointing at `looper config`. JSON output is
unchanged; TestParseDefaults and the new TestDefaultAgentNotice pin the
behavior.

The commit was staged selectively: settings.go and README.md also carry
uncommitted self-run task work (T023, T026), which stays in the working
tree for the resumed run to publish. The installed binary is 0.9.1-dev.9
built from 468284a alone (previous binary kept at
/tmp/looper-before-claude-default); the running stackfall controller
keeps its own image and is unaffected.

## Add tasks through the controller, 2026-11-04

Reviewed Codex session `01a1a3d0-10a2-7460-be34-e15714b6dfcb` from
Stackfall. It added six refactoring tasks by editing `to-do.json` because
only `add-plan` existed. Add `looper add-task <file.json> [--json]` for one
task or a batch. Reuse the typed controller request transport. Apply at loop
boundaries, including idle polling, without stopping workers. Validate the
combined backlog before mutation. Preserve existing metadata and attempts.
Reject external edit conflicts, stale sessions, duplicate IDs, invalid
statuses, unknown dependencies, and cycles. Export through the existing
journal. Requests use the existing session lifetime; they are not durable
imports across controller restarts. A timeout does not cancel a request.
Existing controllers need a later restart to support the new request type.
No live runs, backlog files, installations, or publications are changed.

Validation: focused task-add, request transport, requeue, and parser tests pass
with the race detector. `go vet ./internal/looper` and `git diff --check` pass.

The full `go test ./...` run fails in agent fixtures with `invalid_result:
final output must contain one JSON object`. A temporary Go overlay removes
the new request handler from the run loop. With that overlay,
`TestRunWithoutAssessmentAddsNoProcess` still fails with the same planner
error. This failure occurs independently of task submission. The interface
package passes. The overlay leaves repository sources unchanged.

## Claude-default test regression and tui recovery refusal, 2026-11-04

`looper tui` refuses to start with "the project branch moved" because the
killed self-run saved its session head before four commits landed; recovery
accepts only a new commit whose tree equals the run's last published tree,
so that session can never resume. The refusal is correct. The run's
integrated work sits uncommitted in the working tree and a fresh session
will carry it as the baseline.

Preparing the restart exposed a regression from the Claude default
(468284a): the agent stubs answered only through the codex result file,
and codex and claude parse different channels - codex reads the
--output-last-message file, claude reads a structured_output envelope from
stdout, and the claude adapter passes no --output-last-message at all, so
the unattended-recovery stub crashed on an empty redirect under set -e.
Commit fc79fa9 makes the stubs answer on both channels, guards the empty
redirect, and sets both bins in the two default-coupled fixtures. The full
package passes at fc79fa9 on a frozen tree; the working tree still holds
the killed run's task work and the owner's concurrent agent edits, so a
full make test runs once that work lands.

Restart procedure once the tree is quiet: make test, archive
~/.looper/looper-f42bdd84 (keep runtime/profile.json), then looper start
--jobs 1 --once; the profile pins agent=claude and verify=make test.

## Self-run restarted on Claude, 2026-11-04

The owner said "you fix it". The killed run's T025 work was half-landed:
its oversized-log fixture emitted the result envelope before the bulk
output, so the 64 MiB tail truncation dropped it and the claude adapter
saw no result ("invalid summary", then the dispatch limit). The fixture
now re-emits the envelope after the bulk output with a leading newline,
matching how real Claude streams end.

Full make test then passed on the combined tree (the killed run's stage-1
work, the concurrent add-task/config work, and the T025 finish), landed
as d26f12e with the whole validated state. Binary 0.9.1-dev.11 installed
(previous kept at /tmp/looper-before-d26f12e). The dead session moved to
~/.looper/looper-f42bdd84-killed-selfrun; profile.json kept, pinning
agent=claude and verify=make test.

Fresh controller: session run-5aff45ad4e2d0a66, jobs 1, --once, and the
first worker dispatch runs under claude. Remaining ready work: T034
(redoing, it was in flight at the kill), T035, T036, T038, T039, T040,
T041; eleven tasks stay blocked for stage 2 and the owner.
