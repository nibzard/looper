# Looper

Looper keeps a local development process running. It plans tasks, runs coding
agents in private Git worktrees, checks their combined changes, and integrates
successful work. It uses Codex or Claude Code through their installed commands.

**Go is the default implementation starting with v0.5.0.** Both `looper` and
`looper.sh` run the Go controller. The Bash script stays as a compatibility
launcher for existing scripts and command paths; it is not a separate engine.

The controller is one Go binary. SQLite stores execution state. Git stores code
snapshots. Sequential and parallel runs use the same engine. Pull requests and
remote services are optional parts of your project, not requirements of Looper.

## Install and start

You need Go 1.22 or later to build, Git, Bash, and an authenticated coding agent.
The runtime supports Linux and macOS. The binary needs no Python, Node, or SQLite
command. Python is used by the repository tests. Node is optional for Measuretwice.

From the Looper checkout, install the command:

```sh
./install.sh --skip-skills
cd /path/to/project
looper
```

Run `looper` from your project’s repository root. Keep the intended outcome and exclusions in a file
such as `PROJECT.md`. If `to-do.json` is absent, Looper asks the planning agent to
create tasks from the existing requirements. A project without Git gets a local
repository unless `LOOPER_GIT_INIT=0` is set.

Looper waits for new work after completing the current goal. Edit the task file
or project requirements while it is idle. Looper reads the changes and continues.
Use `--once` when you want a finite run.

```sh
looper --once
looper --jobs 1
looper --jobs 4 --check-jobs 2
looper --agent claude --review-agent codex
looper status
looper tail --follow
looper stop
```

`looper stop` stops new dispatch and lets active work finish. An interrupt stops
child processes and retains partial work. Run `looper` again to resume.
`bin/looper.sh` remains a compatibility launcher for the Go binary.

## Dashboard and mobile terminals

Start the terminal user interface (TUI) with `looper tui`. It starts a controller
or attaches to the active run. The header shows run state, elapsed time,
completion, and active worker counts.
Each worker has a separate row for its task ID, phase, and wrapped title.
Planning and review activity appear when no workers are active.

| State | Color | Meaning |
| --- | --- | --- |
| `RUNNING` | Green | The controller is active; the activity identifies its work. |
| `WAITING` | Yellow | No tasks are ready. |
| `STOPPING` | Yellow | Current work finishes before the controller exits. |
| `BLOCKED` | Red | Work needs attention. |
| `COMPLETE` | Green | The goal is met; Looper waits for new work. |
| `STOPPED` | Gray | No controller is active. |

Text labels remain visible without color. There is no pause command.

Shortcut labels use brackets, such as **[L] Logs** and **[Q] Exit**.
Letter shortcuts accept lowercase and uppercase keys. You do not need Shift.

| Key | Action |
| --- | --- |
| `L` | Open the selected worker’s log, or the planner log while planning. |
| `T` / `B` | Open current-run events / return to the board. |
| `H` | In the events view, toggle recent history from all runs. |
| `J` / `K`, arrows | Select workers; scroll at the ends of the list. |
| Page Up / Page Down | Scroll the board or current view. |
| `D` | Show the selected task’s diff. |
| `A` / `S` | Add a plan / open settings when controls are available. |
| `X` | Request a drain: stop new dispatch and finish current work. |
| `Q` | Detach an observer or open the controller exit choices. |
| `Ctrl+C` | Interrupt a controller and retain work, or detach an observer. |
| `?` | Show help. |

A drain waits for current planning or agent work to finish. To stop immediately,
choose **interrupt; retain work** in the exit dialog. Restarting retains task
progress, but unfinished planning runs again.

The dashboard shows `PLANNING` before implementation starts. Its timer continues
while the agent runs. Press `l` to read the planner log and `t` to read events.
During implementation, `l` opens the selected worker’s latest output. During
verification, it opens that worker’s check output. Empty logs explain that the
agent can buffer its response. The log view refreshes each second.
Agent attempts, tool activity, and retry delays appear in the event history.
The header reports time since the last observed agent output. Controller waiting
messages do not count as agent output. Claude planning streams file reads and
searches. Its available tools are Read, Glob, and Grep; shell commands, delegation,
skills, and external tool servers are disabled for planning. Codex planning uses
its read-only sandbox. The planning prompt tells both agents to identify commands
without running builds, tests, benchmarks, or dataset generators. Planning has no
timeout. Press Ctrl+C to interrupt and retain work.

In mobile terminals and tmux panes, the header and controls wrap to fit the pane.
Use `j` and `k`, arrow keys, or Page Up and Page Down to scroll the board.
The header and controls stay visible when the keyboard reduces the pane height.
The footer shows common controls. Press `?` for settings, plan imports, drain,
and the complete list of controls. The board shows only the current run’s events.
Information uses neutral text; green marks successful results, yellow marks
waiting or blocked work, and red marks errors.

## The development loop

1. Read the goal, task dependencies, and acceptance criteria.
2. Plan independent write scopes, shared resources, setup, and verification.
3. Implement ready tasks in private worktrees.
4. Combine each result with the current integration revision and run checks.
5. Publish only the exact checked tree, then mark its task complete.
6. Review complete product workflows at milestones and at the end.
7. Add evidence-backed findings to the backlog and continue.

A task dependency becomes ready only after integration. Work on unrelated tasks
continues during repairs. Repeated failures trigger a fresh diagnosis. A task
that still fails becomes blocked; its code and logs remain available.

The default worker limit is automatic, up to four workers. Setup and checks share
one execution slot by default. Declared write scopes and resources prevent known
collisions. Actual Git changes control integration. Unexpected conflicts return
to a worker for repair. With multiple check slots, speculative checks overlap;
a result can publish only when its checked base is still current.

Each process receives a unique `LOOPER_RESOURCE_PREFIX`, a private `TMPDIR`, and
`LOOPER_ARTIFACT_DIR`. Use these for databases and other test resources. Let the
operating system choose available server ports. Worktrees share Git metadata;
they are workspace isolation, not a security sandbox.

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

Statuses are `todo`, `doing`, `blocked`, and `done`. Priorities range from 1 to 5.
Looper rejects dependency cycles and unknown fields instead of dropping them.
Task fields also support `steps`, `files`, `blockers`, `tags`, `created_at`,
`updated_at`, and `unblock_when`.

The planning agent finds project commands. You can supply known commands:

```sh
export LOOPER_SETUP_COMMAND='npm ci'
export LOOPER_VERIFY_COMMAND='npm test && npm run build'
looper
```

Setup runs in each implementation and verification worktree. Checks must not
leave changes to tracked source files or unignored generated files. Keep build
output ignored. Looper checks the combined result, not just each worker's branch.

Reviews run after five integrations by default. They compare the product against
requirements, exercise workflows, and report missing capabilities. Worker reports
can also propose follow-up tasks with acceptance criteria and observed evidence.
These findings must stay within the project goal. This remains model judgment;
executable acceptance checks provide stronger evidence where available.

When an external prerequisite is missing, a blocked task records the reason and
an unblock condition. Resolve it, then change the task to `todo` while Looper is
idle. Looper reuses its partial work. It does not repeatedly dispatch blocked tasks
without that change. Editing a retained task's requirements starts a fresh attempt.

## Models

The default agent is Codex. Looper uses each installed agent's configured model
unless you select one explicitly. This preserves your provider configuration
and avoids treating an alias as proof of a particular model.

```sh
looper --model YOUR_MODEL --effort high
looper --agent claude --review-agent codex
```

`CODEX_MODEL`, `CLAUDE_MODEL`, and `CODEX_REASONING_EFFORT` can configure the agents
separately. `--smart` selects `CODEX_SMART_MODEL` (default `gpt-6-astra`) and
`CODEX_SMART_REASONING_EFFORT` (default `high`). Availability depends on your agent
installation and account. Invocation records store the requested configuration;
raw agent logs retain any model identity reported by the provider.

There is no token budget in the controller. The dispatch limit is unlimited by
default. Spend more computation on independent review, diagnosis, and acceptance
checks. Increasing worker count alone can increase conflicts and resource use.

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

SQLite owns execution state. `to-do.json` is its editable projection. A journal
protects task-file exports and code publication. A restart recognizes an operation
before or after publication and resumes it without marking unchecked work done.
Completed worker receipts avoid repeating implementation after a controller crash.
Invalid final reports get a separate report recovery attempt with code changes
prohibited.

Your branch, `HEAD`, and staged index remain unchanged. Successful changes appear
as unstaged edits. Looper retains private refs for snapshots and results. It does
not automatically commit, push, or open pull requests on your branch.

A repository lock covers all linked worktrees. Concurrent outside edits during
integration stop publication and retain the result. Restore the saved baseline
and task contract before resuming interrupted work. Do not delete the runtime
directory or private refs while you need recovery. Logs and refs are retained;
there is no automatic retention limit yet.

`looper status --json` returns the saved state. `looper tail --json` reads events.
`looper doctor` checks local dependencies and the backlog. Exit codes are 0 for a
completed finite run or drained stop, 2 for blocked work or a dispatch limit,
1 for a controller failure, and 130 for interruption.

## Measuretwice

Measuretwice is optional. The adapter in
[`adapters/measuretwice`](adapters/measuretwice/README.md) checks task acceptance
against the patch and verification evidence. It uses an unvalidated exploration
profile in shadow mode.

A `fail` or `review` result requests an independent product review. It does not
reject a change by itself. The reviewer must reproduce a defect before adding a
repair task. Provider errors remain visible in the records. Large evidence packets
produce `review` without silent truncation. No advisory result replaces tests.

## Configuration

Most projects need only `looper`. These settings cover exceptional cases:

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
| `CODEX_BIN`, `CLAUDE_BIN` | `codex`, `claude` | Installed agent commands |
| `LOOPER_HOOK` | unset | Optional executable called after a result |

The hook receives task ID, status, summary path, and attempt ID. It has a 30-second
timeout. It is best effort and is not a transactional delivery mechanism.

## Migration

Existing version 1 task files remain supported. The Go controller imports an
unfinished Python parallel journal from the same project directory. It preserves
the original journal and refs, converts the plan, and resumes retained work.
Completed legacy runs need only their existing task file. Migration tests cover
publication after files were applied but before status was saved.

Run `make build`, then use the new launcher. Stop an old runner before switching.
Do not run an old runner against state after the Go controller has imported it.
Older sequential logs remain on disk but are not imported into SQLite.

The default run is now continuous. Add `--once` to scripts that expect completion.
`--all`, `--interleave`, `--smart`, `--doctor`, `--tail`, and `--ls` remain aliases.
Odd/even and round-robin agent schedules are replaced by explicit coding and
review agents. Submodules and unresolved index conflicts are not supported.

The Homebrew stable formula builds the Go release. Both stable and `--HEAD`
install `looper` and the `looper.sh` compatibility launcher. The old Bash runner
is no longer maintained as a separate execution engine.

## Development

```sh
make test
make race
make test-measuretwice   # optional adapter dependencies required
```

The integration suite uses the actual executable, real Git repositories, and
local agent stubs. It covers parallel checks, dependency ordering, repair,
publication, migration, process cleanup, blocked work, and continuous operation.
The optional adapter tests use the actual Measuretwice library with scripted
judgments. They validate plumbing, not judgment accuracy.

The source entry point is `cmd/looper/main.go`. The controller lives in
`internal/looper`. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for state
invariants and failure behavior. See [the validation record](docs/VALIDATION.md)
for the automated checks and live Codex run.

### UI dependencies

The interface layer adds these direct dependencies. Versions are pinned to the
newest releases that build with Go 1.22:

- `charmbracelet/huh` v0.6.0, `bubbletea` v1.1.0, `bubbles` v0.20.0, and
  `lipgloss` v0.13.0 for forms, the dashboard, and text layout.
- `mattn/go-isatty` for terminal detection.
- `mattn/go-runewidth` for display-width text wrapping.

`internal/ui` never imports `internal/looper`. The pinned set keeps the
toolchain local; Glamour was not added because Markdown rendering is not
needed yet.


## Observe runs from a coding agent

Run these commands inside the project directory:

```sh
looper status --json
looper events --json --follow
looper inspect T054 --json
looper doctor --json
```

These commands read saved runtime records. They do not start workers or change
task status. Stopping an event observer does not drain the controller.

- **Status:** Existing state fields remain available. The new `observation`
  object has `schema_version: 1`, controller ownership, the published view,
  session matching, and observations for active attempts.
- **Events:** Each line is one JSON event. Save its `id` and reconnect with
  `looper events --json --follow --after 123`. Without `--follow`, the command
  replays saved events. Worker activity includes the session, task, attempt,
  phase, and log path in `data`. Older events can omit `data`.
- **Inspect:** Returns a task's blockers, retained attempts, repair feedback,
  verification records, and local log paths. Unknown task IDs return an error.
- **Doctor:** Returns structured findings and suggested actions. Warnings
  include blocked tasks and missing worker processes. Quiet output is
  informational. Exit code 1 means a diagnostic error; warnings return 0.
  The plain command retains its installation and backlog checks.

Controller ownership, process presence, and output activity are separate
observations. `progress: "unknown"` means that output alone cannot prove useful
progress. Log modification times measure output, including diagnostic output.
A quiet test command is not evidence of a stalled worker. Missing process
metadata means the process state is unknown, which is expected for older runs.

New Claude workers stream output into Looper-owned logs. Worker activity
summaries omit command arguments, tool output, and private reasoning. Raw logs
remain local and can contain sensitive project data. Inspect them only when
needed. Existing runs keep their current executable and output mode until you
restart them normally.

Use this feedback procedure to improve Looper:

1. Capture status, task inspection, diagnostics, and relevant event IDs.
2. Reproduce the failure in an isolated regression test.
3. Change Looper and run its tests.
4. Install the fix for subsequent runs.
5. Observe the same case and confirm the result.

Diagnostics do not automatically change code, restart workers, or clear blockers.
The event journal provides evidence for this procedure; it is not an execution
replay engine.
