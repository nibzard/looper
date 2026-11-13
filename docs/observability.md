# Observe runs

These commands help a person or a coding agent watch a run without changing it.

Run these commands inside the project directory:

```sh
looper status --contract v2 --json
looper events --contract v2 --json
looper inspect T054 --contract v2 --json
looper doctor --contract v2 --json
```

These commands read saved runtime records. They do not start workers or
change task status. Stopping an event observer does not drain the
controller. Each version 2 document is one bounded JSON object with
`limits` and `detail` routes naming what it omitted and where to read it.

`looper wait` replaces a polling loop with one bounded call. It observes one
predicate — a cursor change, a task status, a request outcome, or the run's
end — and returns one outcome document:

```sh
looper wait --contract v2 --request req-1c0ffee --timeout 30s --json
looper wait --contract v2 --task T054 --status done --timeout 120s --json
```

`--timeout` is required. Exit 0 means the condition held, exit 5 means the
bound expired, and exit 6 means the caller interrupted the wait. A timeout
or an interruption cancels nothing: the request, task, and controller keep
their state. Mutation receipts and the compact status document name the
exact wait that observes their outcome.

`status --json` includes the saved state and an `observation` object with
`schema_version: 1`. It reports controller ownership, the published view,
session matching, and observations for active attempts.

Use `looper --version --json` to read the executable's release, Git revision,
and modified-source flag. `observation.controller.build` identifies a live
controller's executable. A durable `session` event retains that build identity
after the controller exits. Older controller records omit it; builds from source
archives without Git metadata report revision `unknown`.

`status` also reports project timing. `status --json` includes a `timing`
object with `schema_version: 1`. All durations use seconds:

- `elapsed_seconds`: the span from the first recorded event or process to the
  latest recorded activity. A live controller extends this span to now.
- `active_seconds`: time with at least one supervised process running.
  Overlapping processes count once.
- `worker_seconds`: the sum of supervised process durations. Parallel work
  counts for each process. This includes agents, setup, verification, and review.
- `controller_seconds`: controller runtime, including idle waits.

These values measure wall time, not processor use or human effort. The database
stores controller sessions and process starts, ends, and outcomes. A normal stop
records an end time. A crash leaves the end unknown. Dead processes do not accrue
time while Looper is stopped. `unknown_sessions` and `unknown_processes` report
those gaps; the totals include only known intervals and verified live processes.

`recorded_since` identifies when database timing begins. On startup, Looper
imports existing process timestamp files without counting them twice. Earlier
controller durations remain unknown. `history_complete: false` identifies old
history or missing end times. Before migration, read-only status reports
`available: false` and null durations. Observers do not migrate the database.

`timing.tasks` provides per-task durations from saved attempt and process records.
`looper inspect T054 --json` returns the same task report in `timing`. Plain
`inspect` also prints these durations:

- `worker_seconds`: worker agent and worker setup time, including repairs.
- `check_seconds`: integration setup, verification, and assessment time.
- `other_seconds`: other task processes, including diagnosis and hooks.
- `active_seconds`: task process intervals with overlaps counted once.
- `elapsed_seconds`: the span between the first and last recorded task activity,
  including gaps between attempts.

Category totals count each process, including overlapping processes. The task
report includes attempt counts and unknown process ends. Missing history sets
`history_complete: false`. A task without process records has `available: false`
and null durations. Project `worker_seconds` retains its broader definition above.

`status --json` also includes a `statistics` object with `schema_version: 1`.
It exposes project totals, usage coverage, model breakdowns, and fourteen
UTC daily buckets used by the terminal statistics view. Missing costs are null.

Usage records preserve `agent_provider` and `reported_cost_basis` from public
agent counters. Statistics expose the reported cost basis for totals and models.
Absent provenance is `unknown`; combined incompatible bases are `mixed`.
Provider API estimates and agent-reported costs can differ. These fields do not
establish the actual charge, and Looper does not rescale prices to match a ratio.

Reviews publish their log path through `review-activity`. A `review-heartbeat`
event appears every 30 seconds while the agent runs. It reports output size and
the last output timestamp. Log silence alone does not establish a stalled review.
The heartbeat does not change review tools, completion rules, or timeouts.

Repair diagnoses and run-recovery consultations publish their stream through
`diagnosis-activity` and `run-recovery-activity`, with a heartbeat event every
30 seconds while the consultation runs. The published view reports the
`diagnosing` or `recovering` state, the agent, and the last output time. A
consultation blocks the controller loop, so worker activity keeps publishing
through it and parallel work stays visible. The consultation keeps its normal
review tools and result contract.

`events --json` prints one JSON event per line. Save its `id` and reconnect
with `looper events --json --follow --after 123`. Without `--follow`, the
command replays saved events. Worker activity includes the session, task,
attempt, phase, and log path in `data`. Older events can omit `data`.

Worker, verification, launcher, and resource-cleanup failures journal one
bounded summary as a `failure` event: the operation, the tool or agent, the
exit status when the process ran, a stable outcome, a short cause, and
Looper-owned evidence references. The excerpt is at most 2 KiB, keeps the tail
of the failing tool's own output, and is scrubbed of credential shapes; agent
session streams are referenced, never quoted. Retrieve every summary with one
query:

    looper events --contract v2 --json --kind failure
    looper events --contract v2 --json --kind failure --task T015

A failed check also retains its summary as `failure.json` beside its logs.
A cleanup that failed only on already-absent paths reports the outcome
`cleanup_already_absent`, which stays distinguishable from an exit failure of
the cleanup logic itself.

A `shutdown` event records the controller session, reason, exit code, optional
error, and last event ID. Reasons include `complete`, `drained`, `cancelled`,
`error`, `panic`, `no_runnable_tasks`, and `dispatch_limit`. The session end and
event commit together after supervised work stops. A restart links its `resume`
event to the previous exit. If the previous session has no end record, a
`recovery` event reports an unclean exit and the last recorded event ID. The
cause remains unknown because an external kill cannot record its own cause.

`inspect` returns a task's blockers, retained attempts, repair feedback,
verification records, and local log paths. Unknown task IDs return an error.

`doctor --json` returns findings and suggested actions. It warns about
blocked tasks and missing worker processes. Quiet output is informational.
Exit code 1 means a diagnostic error; warnings return 0. The plain command
also checks installation dependencies and the backlog.

Controller ownership, process presence, and output activity are separate
observations. `progress: "unknown"` means that output alone cannot prove
useful progress. Log modification times measure output, including
diagnostic output. A quiet test command is not evidence of a stalled
worker. Missing process metadata means the process state is unknown, which
is expected for older runs.

New Claude workers stream output into Looper-owned logs. Worker activity
summaries omit command arguments, tool output, and private reasoning. Raw
logs remain local and can contain sensitive project data. Inspect them only
when needed. Existing runs keep their current executable and output mode
until you restart them normally.

Use this feedback procedure to improve Looper:

1. Capture status, task inspection, diagnostics, and relevant event IDs.
2. Reproduce the failure in an isolated regression test.
3. Change Looper and run its tests.
4. Install the fix for subsequent runs.
5. Observe the same case and confirm the result.

Diagnostics do not automatically change code, restart workers, or clear
blockers. The event journal provides evidence for this procedure; it is not
an execution replay engine.
