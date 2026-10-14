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
