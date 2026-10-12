# Looper

Give Looper a repository and written requirements. It runs your coding
agents, checks their work, and applies what passes.

Leave it overnight. Come back to the terminal to see what finished and
what needs you.

When the goal is complete, it waits for your next edit.

![A recorded run: parallel workers, the events view, and one blocked task
with its reason](docs/assets/loop.gif)

## Quickstart

You need Go 1.22 or later, Git, Bash, and an authenticated `codex`,
`claude`, or `zcode` command. Linux and macOS are supported. The binary needs
no Python, Node, or SQLite command at runtime. Repository tests use Python.
The optional Measuretwice adapter uses Node.

1. Install the command:

   ```sh
   git clone https://github.com/nibzard/looper.git
   cd looper
   ./install.sh --skip-skills
   ```

2. In your project's repository root, describe the intended outcome and
   exclusions in a file such as `PROJECT.md`.

3. Start the loop:

   ```sh
   cd /path/to/project
   looper
   ```

If `to-do.json` is absent, Looper asks the planning agent to create tasks
from the existing requirements. A project without Git gets a local repository
unless `LOOPER_GIT_INIT=0` is set.

Edit the task file or project requirements while Looper is idle. It reads
the changes and continues. Use `--once` when you want a finite run.

```sh
looper --once
looper --jobs 4 --check-jobs 2
looper --agent claude --review-agent codex
looper --agent zcode --review-agent zcode
looper tui
looper status
looper tail --follow
looper stop
```

`looper stop` stops new dispatch and lets active work finish. An interrupt
stops child processes and retains partial work. Run `looper` again to resume.

## How it works

Each task starts with fresh agent context.

1. Read the goal, task dependencies, and acceptance criteria.
2. Plan independent write scopes, shared resources, setup, and verification.
3. Implement ready tasks in private worktrees.
4. Combine each result with the current integration revision and run checks.
5. Publish only the exact checked tree, then mark its task complete.
6. Review complete product workflows at milestones and at the end.
7. Add tasks with evidence from review and continue.

A task dependency becomes ready only after integration. Work on unrelated
tasks continues during repairs. Repeated failures trigger a fresh diagnosis.
A task that still fails becomes blocked; its code and logs remain available.

Reviews report gaps as structured findings and set `complete` explicitly when
the goal is satisfied. The task file belongs to the controller: a review that
edits it, including through a legacy `project-done` marker, is rejected
whole. Existing markers still load and stay excluded from scheduling, but
only an explicit `complete: true` response with every task done and no open
finding finishes a goal.

In a continuous run, an invalid task file at an idle boundary pauses the
loop instead of stopping it. Looper keeps the last accepted state, leaves
your file untouched, and waits for a corrected edit. The wait reports the
file path, the problem, and the action, and clears when any valid edit
arrives. Queued plan imports wait with it; requeue requests receive a
conflict with a correction hint. Stop still drains with code 0, an interrupt
returns 130, and invalid input at startup or under `--once` exits with
code 1.

Looper chooses a worker limit automatically, up to four workers. Setup and
checks share one execution slot by default. The plan identifies files and
resources that tasks can change to avoid conflicts. Looper checks the actual
Git changes during integration and sends conflicts back to a worker for
repair. If you enable multiple check slots, checks can overlap. A result
can publish only while the revision it was checked against remains current.

Each operation receives a unique `LOOPER_RESOURCE_PREFIX`. Its child
processes receive a private `TMPDIR` and `LOOPER_ARTIFACT_DIR`. Use these
for databases and other test resources.
`TMPDIR` is a short directory under `/tmp`, with mode 0700, private to
each child process. Looper removes it after the process exits or is
cancelled. Store retained evidence in `LOOPER_ARTIFACT_DIR`; do not use
`TMPDIR` to pass files between setup, workers, and checks. An abrupt
controller termination can leave temporary directories behind.
Let the operating system choose available server ports. Worktrees share Git
metadata; they are workspace isolation, not a security sandbox.

Worker agents run with permission checks disabled. Claude Code receives
`--dangerously-skip-permissions`. Codex receives
`--dangerously-bypass-approvals-and-sandbox`. Set `CODEX_YOLO=0` to give
Codex a workspace-write sandbox instead. zcode workers run in its
full-access `yolo` mode. Run Looper only in repositories whose contents you
trust.

## Dashboard

Start the terminal user interface (TUI) with `looper tui`. It starts a
controller or attaches to the active run. The overview shows the goal,
controller activity, snapshot age, progress, attention items, and active
workers. Recent outcomes stay visible even when worker events fill the log.
Integration counts cover the current session, the last hour, and the last
24 hours. Looper reads these counts from the journal, so they survive the
viewer's event buffer limit.

For an unattended run, use `looper start`, then attach with `looper tui`.
Keep the viewer in tmux or reconnect later. In attached mode, `q` detaches
without stopping the controller. A missing snapshot shows `UNKNOWN` and
retains the last known state. Quiet worker output does not prove a failure.

![The dashboard: run state and progress in the header, working and blocked
tasks, recent events](docs/assets/tui.png)

| State | Color | Meaning |
| --- | --- | --- |
| `RUNNING` | Green | The controller is active; the activity identifies its work. |
| `WAITING` | Yellow | No tasks are ready, or the task file needs correction. |
| `STOPPING` | Yellow | Current work finishes before the controller exits. |
| `BLOCKED` | Red | Work needs attention. |
| `COMPLETE` | Green | The goal is met; Looper waits for new work. |
| `STOPPED` | Gray | No controller is active. |
| `UNKNOWN` | Yellow | The snapshot is unavailable; displayed work is the last known state. |

Text labels remain visible without color. Press `u` to browse all tasks,
including completed and blocked tasks. Search with `/` and filter with `f`.
Press Enter to inspect task details, dependencies, recovery instructions,
changed files, and recorded checks. Wide terminals show the inspector beside
the task list. Smaller terminals open it as a separate screen.

Common keys: `l` opens task logs, `t` shows activity, and `p` shows statistics.
Press `b` for the overview or `d` for the selected task's diff.
Press `?` for all controls.

The statistics view refreshes every five seconds. It shows elapsed time, active
time, combined worker time, controller time, task integrations, repairs, and
verification results. A daily chart compares worker hours with integrations,
token usage, and estimated API cost. Wide terminals show totals beside the chart.
Small terminals stack the sections; use the arrows or Page Down to scroll.

Token totals come from Codex and Claude usage reports. Cached input and output
remain separate. Looper does not estimate tokens from text length or infer a
model from a CLI default. Usage becomes available when an agent report arrives.
On startup, Looper imports retained reports without counting them twice.

Dollar estimates use a pinned [Pydantic GenAI Prices](https://github.com/pydantic/genai-prices)
v2 snapshot. Each stored report retains the pricing revision and date.
The view shows the share of reports with known prices. Unknown models,
nonstandard service tiers, and context tiers that cannot be inferred from
aggregate usage remain unpriced. Agent-reported costs stay separate.
API estimates do not represent invoices or subscription charges.

Refresh model prices with `python3 scripts/update-prices.py`, then rebuild.
The terminal interface works offline and never downloads prices during a run.
See [the pricing notes](internal/looper/prices/README.md) for supported fields.

<details>
<summary>Complete key reference, planning view, and small screens</summary>

Shortcut labels use lowercase keys, such as **[l] Logs** and **[q] Exit**.
Letter shortcuts accept lowercase and uppercase keys. You do not need Shift.

| Key | Action |
| --- | --- |
| `u` | Browse all tasks. |
| Enter | Inspect the selected task. |
| `/` / `f` | Search tasks / change the task filter. |
| `r` | In the inspector, confirm blocker resolution and request a requeue. |
| `l` | Open the selected task's log, or the planner log while planning. |
| `t` / `b` | Open activity / return to the overview. |
| `h` | In activity, toggle recent history from all runs. |
| `j` / `k`, arrows | Select tasks or scroll the current view. |
| Page Up / Page Down | Scroll the board or current view. |
| End | Resume live activity after scrolling. |
| Tab | Cycle overview, tasks, and activity. |
| Esc | Return from the inspector or finish a search. |
| `d` | Show the selected task's diff. |
| `a` / `s` | Add a plan / open settings when controls are available. |
| `x` | Request a drain: stop new dispatch and finish current work. |
| `q` | Detach an observer or open the controller exit choices. |
| `ctrl+c` | Interrupt a controller and retain work, or detach an observer. |
| `?` | Show help. |

Scrolling activity pauses the displayed text while new events arrive.
The view reports the new event count. End resumes live activity.
This pauses the viewer only; the controller continues to work.

Requeue requests apply only while Looper is idle. Resolve the blocker first,
then press `r` in its inspector and confirm with `y`. The controller rejects
changed tasks and requests from earlier sessions. Retained work stays
available for reuse. A read-only observer cannot requeue tasks.

There is no command to pause execution. A drain waits for current planning
or agent work to finish. To stop immediately, choose **interrupt; retain
work** in the exit dialog. Restarting retains task progress, but unfinished
planning runs again.

The dashboard shows `PLANNING` before implementation starts. Its timer
continues while the agent runs. Press `l` to read the planner log and `t` to
read events. During implementation, `l` opens the selected worker's latest
output. During verification, it opens that worker's check output. Empty logs
explain that the agent can buffer its response. The log view refreshes each
second. Agent attempts, tool activity, and retry delays appear in the event
history. The header reports time since the last observed agent output.
Controller waiting messages do not count as agent output. Claude planning
streams file reads and searches. Its available tools are Read, Glob, and
Grep; shell commands, delegation, skills, and external tool servers are
disabled for planning. Codex planning uses its read-only sandbox. The
planning prompt tells both agents to identify commands without running
builds, tests, benchmarks, or dataset generators. Planning has no timeout.
Press Ctrl+C to interrupt and retain work.

Looper sends Codex and Claude prompts through standard input. Each operation
retains its exact `prompt.txt` and records transport and prompt size in
`invocation.json`. Zcode still receives its prompt as an argument. Its prompt
is visible through process inspection and can reach an execution limit.

Large backlogs, imported requirements, and repair feedback stay in context
files. Looper keeps durable copies beside the attempt and readable copies
inside the private worktree. Small task contracts stay inline. Workers read
requirements relevant to their task; final review covers every applied import.
Missing or changed imported evidence stops the operation without dropping scope.
Restore the recorded evidence before restarting.

Planner prompts have a 4 KiB size target. Other prompts have a 16 KiB target.
Exceeding a target does not reject work. These byte targets help compare runs;
they are not model token limits. Reading context files also consumes tokens.

In mobile terminals and tmux panes, the overview uses a compact summary.
Task details remain available through the inspector. Use `j` and `k`,
arrow keys, or Page Up and Page Down to navigate. The header and controls
stay visible when the keyboard reduces the pane height. The footer shows
common controls. Press `?` for settings, plan imports, drain, and all keys.
The overview shows only the current run's outcomes. Information
uses neutral text; green marks successful results, yellow marks waiting or
blocked work, and red marks errors.

</details>

## Tasks and checks

A minimal task file looks like this:

```json
{
  "schema_version": 1,
  "goal": "Implement the workflows in PROJECT.md",
  "source_files": ["PROJECT.md"],
  "tasks": [
    {
      "id": "T001",
      "title": "Implement the first complete workflow",
      "priority": 1,
      "status": "todo",
      "details": "Describe observable acceptance criteria here.",
      "depends_on": []
    }
  ]
}
```

Statuses are `todo`, `doing`, `blocked`, and `done`. Priorities range from 1
to 5. Looper rejects dependency cycles and unknown fields instead of
dropping them. Task fields also support `steps`, `files`, `blockers`, `tags`,
`created_at`, `updated_at`, and `unblock_when`.

The planning agent finds project commands. You can supply known commands:

```sh
export LOOPER_SETUP_COMMAND='npm ci'
export LOOPER_VERIFY_COMMAND='npm test && npm run build'
looper
```

Setup runs in each implementation and verification worktree. Setup and
checks must leave tracked and unignored project files unchanged. Workers
must include ignore rules and remove tracked build artifacts in their
implementation. Looper checks product behavior in a fresh integrated
worktree. Checks must not depend on Looper-private refs or worker commit
history.

Setup, verification, and assessment command bodies run from private script
files beside their logs. Bash loads each script in a login shell with no
positional arguments. Command bodies do not appear in process arguments.
The script filename appears in `BASH_SOURCE`; a top-level `return` ends the
script. Use `exit` when you need to end the shell.

Looper checks arguments and environment sizes before launch. A launch failure
identifies the recorded inputs and stops with retained work. It does not use
implementation repair cycles. An agent that starts and exits unsuccessfully
uses the existing failure handling.

Reviews run after five integrations by default. They compare the product
against requirements, exercise workflows, and report missing capabilities.
Worker reports can also propose follow-up tasks with acceptance criteria and
observed evidence. These findings must stay within the project goal. Agents
can miss defects or report problems that do not exist. Use executable
acceptance checks where you can.

When an external prerequisite is missing, a blocked task records the reason
and an unblock condition. Resolve it, then change the task to `todo` while
Looper is idle. Looper reuses its partial work. It does not repeatedly
dispatch blocked tasks without that change. Editing a retained task's
requirements starts a fresh attempt.

## Models

The default agent is Codex. Looper uses the model configured in each agent
unless you select one explicitly.

```sh
looper --model YOUR_MODEL --effort high
looper --agent claude --review-agent codex
```

`CODEX_MODEL`, `CLAUDE_MODEL`, and `CODEX_REASONING_EFFORT` can configure
the agents separately. `--smart` selects `CODEX_SMART_MODEL` (default
`gpt-6-astra`) and `CODEX_SMART_REASONING_EFFORT` (default `high`).
Availability depends on your agent installation and account. Invocation
records store the requested configuration; raw agent logs retain any model
identity reported by the provider.

### Use zcode

Install and authenticate the command from
[ZCode](https://github.com/zai-org/ZCode). The executable must be on your
`PATH`. Set `ZCODE_BIN` if it is installed elsewhere.

```sh
looper --agent zcode --review-agent zcode
ZCODE_BIN=/path/to/zcode looper --agent zcode
```

`--agent zcode` selects zcode for workers. `--review-agent zcode` selects it
for planning, diagnosis, and product review. If no review agent is configured,
Looper uses the worker agent. zcode uses its own configured model;
Looper's Codex and Claude model options do not select a zcode model.

Looper invokes zcode with `--output-format stream-json` and `--mode yolo`.
It reads the response from the final `result` event. The command must exit
successfully, and the response must contain a valid JSON object. Looper
rejects intermediate responses and streams with no final result. It also
accepts one JSON object inside a Markdown code fence.

zcode has no schema flag. Looper states the output contract in the prompt
and validates the returned task report. Planning uses the same full-access
mode because zcode's plan mode returns prose instead of the required JSON.
The inspection-only prompt limits planning behavior; it does not enforce
read-only access to tools.

The controller does not set a token budget. Task dispatch is unlimited by
default. If you add workers, account for extra resource use and conflicts.

## State and recovery

The default state directory is:

```text
~/.looper/<project-name>-<path-hash>/runtime/
  state.db
  history/
  worktrees/
  task-*/
  plan-*/
  review-*/
```

SQLite stores execution state. Edit `to-do.json` to change the backlog;
Looper imports those edits. A journal tracks task-file exports and code
publication. After a restart, Looper recognizes whether an operation was
published and resumes it without marking unchecked work done. Completed worker receipts avoid repeating
implementation after a controller crash. Invalid final reports get a
separate report recovery attempt with code changes prohibited.

Your branch, `HEAD`, and staged index remain unchanged. Successful changes
appear as unstaged edits. Looper retains private refs for snapshots and
results. It does not automatically commit, push, or open pull requests on
your branch.

A repository lock covers all linked worktrees. Concurrent outside edits
during integration stop publication and retain the result. Restore the saved
baseline and task contract before resuming interrupted work. Do not delete
the runtime directory or private refs while you need recovery. Logs and refs
are retained; there is no automatic retention limit yet.

`looper status --json` returns the saved state. `looper tail --json` reads
events. `looper doctor` checks local dependencies and the backlog. Exit
codes are 0 for a completed finite run or drained stop, 2 for blocked work
or a dispatch limit, 1 for a controller failure, and 130 for interruption.

## Configuration

Start with `looper`. Use these settings to change concurrency, timeouts,
and other controller behavior:

| Setting | Default | Purpose |
| --- | --- | --- |
| `LOOPER_JOBS` / `--jobs` | `auto` | Maximum implementation workers |
| `LOOPER_CHECK_JOBS` / `--check-jobs` | `1` | Concurrent setup and checks |
| `LOOPER_REVIEW_EVERY` / `--review-every` | `5` | Integrations between product reviews; 0 disables milestones |
| `MAX_ITERATIONS` / `--max-iterations` | `0` | Dispatch limit; 0 means unlimited |
| `LOOPER_MAX_REPAIRS` | `6` | Repair limit per attempt |
| `LOOPER_POLL_SECONDS` | `30` | Idle polling interval |
| `LOOPER_TIMEOUT_SECONDS` | `0` | Worker timeout; 0 means unlimited |
| `LOOPER_GIT_TIMEOUT_SECONDS` | `600` | Timeout for one Git command; 0 means unlimited |
| `--verbose` | off | Print the run configuration at start |
| `LOOPER_RATE_LIMIT_MAX` | `12` | Maximum agent invocations for a transient failure |
| `LOOPER_RATE_LIMIT_DELAY` | `60` | Initial retry delay, in seconds |
| `LOOPER_BASE_DIR` | `~/.looper` | State and artifact location |
| `CODEX_BIN`, `CLAUDE_BIN`, `ZCODE_BIN` | `codex`, `claude`, `zcode` | Installed agent commands |
| `LOOPER_HOOK` | unset | Optional executable called after a result |

The hook receives task ID, status, summary path, and attempt ID. It has a
30-second timeout. Hook failures do not roll back task results, and delivery
is not guaranteed.

## Measuretwice

Measuretwice is optional. The adapter in
[`adapters/measuretwice`](adapters/measuretwice/README.md) checks task
acceptance against the patch and verification evidence. It uses an
unvalidated exploration profile in shadow mode.

A `fail` or `review` result requests an independent product review. It does
not reject a change by itself. The reviewer must reproduce a defect before
adding a repair task. Provider errors remain visible in the records. Large
evidence packets produce `review` without silent truncation. No advisory
result replaces tests. Each assessment command runs under a fixed 60-second
controller deadline. A timed-out assessment records an advisory error beside
its evidence and the verified result still publishes. An assessment that
changes project files cannot publish.

## Development

Follow [`MANIFESTO.md`](MANIFESTO.md) when changing Looper: one purpose,
simple before easy, do not make me think, believe in the loop, and take
things away.

```sh
make test
make race
make test-measuretwice   # optional adapter dependencies required
```

The integration suite uses the actual executable, real Git repositories, and
local agent stubs. It covers parallel checks, dependency ordering, repair,
publication, migration, process cleanup, blocked work, and continuous
operation. The optional adapter tests use the actual Measuretwice library
with scripted judgments. They validate plumbing, not judgment accuracy.

The source entry point is `cmd/looper/main.go`. The controller lives in
`internal/looper`. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for
state invariants and failure behavior. See
[the validation record](docs/VALIDATION.md) for the automated checks and
live Codex run.

See [the practices report](docs/research/PRACTICES-REPORT-2026-10-11.md)
for a dated comparison with native agent capabilities and other local
orchestrators.

See [interface dependencies](docs/dependencies.md) for the pinned packages.
See [migration instructions](docs/migration.md) before upgrading an older runner.

## Observe runs from a coding agent

Run these commands inside the project directory:

```sh
looper status --json
looper events --json --follow
looper inspect T054 --json
looper doctor --json
```

These commands read saved runtime records. They do not start workers or
change task status. Stopping an event observer does not drain the
controller.

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

`events --json` prints one JSON event per line. Save its `id` and reconnect
with `looper events --json --follow --after 123`. Without `--follow`, the
command replays saved events. Worker activity includes the session, task,
attempt, phase, and log path in `data`. Older events can omit `data`.

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
