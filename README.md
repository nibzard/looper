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

Install the latest published release with:

```sh
curl -fsSL https://raw.githubusercontent.com/nibzard/looper/main/scripts/install-latest.sh | bash
```

This downloads the release source from GitHub and builds it with Go. It
requires Bash, curl, tar, Git, and Go 1.22 or later. It installs the binary
in `~/.local/bin` without sudo. Run the same command to update. Existing
controllers keep their current binary until you restart them.

To inspect the script first or select an installation directory:

```sh
curl -fsSL https://raw.githubusercontent.com/nibzard/looper/main/scripts/install-latest.sh -o /tmp/looper-install.sh
bash /tmp/looper-install.sh --bin-dir "$HOME/.local/bin"
```

Use `--version v0.8.3` to select a release. Add `--with-skills` to install
that release's bundled skills; older releases can include the old skill names.
The latest release can lag behind `main`. To build the current source:

1. Install the command:

   ```sh
   git clone https://github.com/nibzard/looper.git
   cd looper
   ./install.sh
   ```

   This installs `looper-diagnose` and `looper-backlog`. Use `--skip-skills`
   to install only the binary.
   Add `--with-workflow-skills` to also install `git-commit` and
   `github-release`. Skills are optional; Looper enforces task contracts
   without them. The release skill prefers the project's release process.

   Earlier installations used `todo-json-manager`, `git-conventional-commit`,
   and `release-runbook`. Remove those old directories from your skills
   directory after preserving any local edits. Installation does not remove
   or rename existing user skills.

   Installation skips existing skills unless a previous Looper install owns them.
   It records installed skills in `.looper-installed-skills` in the skills directory.
   Run `./uninstall.sh` with the same location options to remove the recorded skills.
   Without a receipt, uninstall preserves all skills, including skills from older installations.

   Both scripts honor `LOOPER_USER_PREFIX` as the default prefix instead of
   `~/.local`. An explicit `--prefix` still wins. Test fixtures that exercise
   the installers bind this variable (plus a temporary `HOME`) so they can
   never write to the real user prefix.

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
looper runs
looper recovery --preview
looper preflight
looper tail --follow
looper stop
```

`looper help` prints the command overview and `looper help <command>` prints
one command's own help: its examples, the documents it emits, and its
pagination, wait, and exit semantics. A word that names no command and no
file is a usage error, so a mistyped command cannot start a run.

`looper stop` stops new dispatch and lets active work finish. A stop request
binds to the controller that owns the repository. `looper stop` writes a
request only while a controller runs, so a stop without a running controller
changes nothing and a later start runs normally. A request that arrives
during startup still drains that controller at its first boundary. A request
left by a controller that already exited is discarded when the next
controller starts. A request from an older Looper version names no owner.
The next controller honors such a request only when it arrived after that
controller started. An interrupt stops child processes and retains partial
work. Run `looper` again to resume.

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

An external edit that lands while work runs pauses dispatch the same way.
The edit imports at the first boundary with nothing in flight, and the
interrupted results continue against the new baseline. A stop during that
wait drains at the same boundary: the results stay retained and your edited
file stays untouched.

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

Setup receives `LOOPER_RESOURCE_CLEANUP_FILE`, a private path for a Bash cleanup
script. Write the script before you create external resources. Looper runs it
after the worker or check finishes, including setup and verification failures.
Make cleanup idempotent and check ownership before you remove a resource. Use
labels and container IDs for Docker resources; do not remove resources by a
shared name alone. Remove private anonymous volumes with their containers.
Use `io.looper.resource-prefix` and `io.looper.artifact` labels with the values
of `LOOPER_RESOURCE_PREFIX` and `LOOPER_ARTIFACT_DIR`. Preserve cleanup that
setup already registers when you add more resources.

Looper records each operation's owner, prefix, and cleanup status in
`LOOPER_ARTIFACT_DIR/resources.json`. Cancellation retains resources for recovery.
After retained work finishes, Looper retries its cleanup. Restart also retries
failed cleanup and cleans abandoned resources for completed work. Cleanup has
a 30-second limit and a separate `cleanup.log`. A cleanup failure preserves the
task result, emits a `resource-cleanup-error` event, and journals a bounded
`failure` summary with the script's exit status, its own log excerpt, and the
retained evidence; a cleanup that failed only on already-absent paths reports
the outcome `cleanup_already_absent`, so it never reads as a failure of the
cleanup logic itself. The wrapper runs the script under `set -e`; the
documented check-before-remove idiom `[ -d path ] && rm -rf path` survives a
clean path when commands follow it, but a script whose final command is that
idiom reports its own status 1 with nothing left to remove, and plain Bash
does the same without the wrapper. Projects without a registered cleanup
script keep their existing resource behavior.

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
retains the last known state. An attached dashboard reconnects when a replacement
controller appears. Task details refresh with the new session. Actions wait until
the dashboard displays that session. Quiet worker output does not prove a failure.

Add `--no-animation` to any dashboard start to disable animated motion. The
bootstrap activity marker stays a fixed bullet instead of a spinner, and
every status line, key, and refresh keeps its behavior. The option changes
presentation only; `--json` output and normal execution are unchanged.

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

Copy uses the terminal clipboard protocol (OSC 52). It preserves full lines,
even on a narrow screen. In task views, it copies the selected task and its
resume condition. In the overview, it copies the run state and blockers.
Paste the result into an agent to request diagnosis and fixes within the existing scope.
It includes the project root, session, event cursor, and commands to verify live state.
Large logs and diffs use labeled excerpts. Task blockers remain complete.
The terminal must allow clipboard writes. For tmux, enable `set-clipboard on`
and use a terminal with OSC 52 support. The notice confirms that Looper sent
the request; the terminal does not acknowledge clipboard updates.

Future-round tasks can retain `status: "blocked"` with the tag `looper:deferred`
and a concrete `unblock_when` condition. They appear as deferred in task views
and under the Waiting filter. They remain required work and cannot dispatch
through a generic retry. When the condition is met, remove the tag and set
`status: "todo"`. Required operator ownership remains a separate restriction.


| Key | Action |
| --- | --- |
| `c` | Copy an agent handoff with run state and relevant evidence. |
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

Follow-ups identify an `owner` (`agent` or `operator`), a stable outcome
`key`, and an `existing_task_id` (empty for new work). Reuse the existing
task ID or key when the same prerequisite appears under a different title.
Looper records new evidence in the task-linked event journal without reopening
completed work or changing an active worker's acceptance criteria.
Keys persist as `looper:finding:<key>` tags in the existing backlog format.

Operator follow-ups enter the backlog as blocked with a `looper:operator`
tag. They stay outside the worker queue even if a generic retry sets them
to `todo`. Legacy tasks that explicitly assign an action to an operator
also stay outside the queue. After the operator completes the action,
record the evidence and mark it done. To delegate different work, change
the task contract and remove the operator tag. Follow-ups do not grant
permission to act outside the original task's authority.

When an external prerequisite is missing, a blocked task records the reason
and an unblock condition. Resolve it, then change the task to `todo` while
Looper is idle. Looper reuses its partial work. It does not repeatedly
dispatch blocked tasks without that change. Editing a retained task's
requirements starts a fresh attempt.

## Agent configuration

Run `looper config` to add agent commands and choose separate worker and reviewer
profiles. Each command uses a supported adapter: Claude Code, Codex, or Zcode.
For example, `anthropic` can run `claude` while `glm` runs `claude-zai`, both
through the Claude Code adapter:

```sh
looper --agent glm --review-agent anthropic
```

Machine commands live in `~/.config/looper/agents.json` (or the XDG configuration
location). Project role choices stay in the existing project profile. The first
interactive start opens the same setup flow. Fresh unattended projects require
an explicit agent or saved choice; `--yes` explicitly accepts defaults.
Inspection commands never open setup.

Saving configuration runs no agents. Use `looper config --test-agent glm` for an
explicit live check that consumes usage. See [agent profiles](docs/AGENT-PROFILES.md)
for configuration, migration, permission behavior, and adding future adapters.

## Models

The legacy default agent is Claude. A named profile can select its own model
and effort. Empty profile values use the agent's settings. Existing model
flags and environment variables remain available as overrides.

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

Plan imports keep accepted scheduling adjustments separate from unresolved
conflicts. Optional ordering before a completed task can become follow-up work
after that task. The import receipt and acknowledgement retain the adjustment.
Imports can also add prerequisites to an existing idle task through `gates`.
These additions cannot remove dependencies, create cycles, or change completed
or retained work. A follow-up report preserves an existing task's ownership.
Use a separate task for a distinct operator prerequisite.

Before waiting on blocked work, Looper can repair proven orchestration defects.
It consults the review agent once for unchanged source and command contracts.
Repairs apply after workers and checks drain. Looper retains implementation
work and runs integration checks with the corrected commands. The repair and
its retry limit survive restart. Unreadable product requirements and external
prerequisites stay blocked.

Recovery events distinguish consultation, application, integration, and successor
dispatch. Verification that changes project files reports the paths and retains
`file-changes.txt` beside the check logs. Status shows both executable and
controller builds when they differ. An installed update takes effect for a
controller after restart.

Your branch, `HEAD`, and staged index remain unchanged. Successful changes
appear as unstaged edits. Looper retains private refs for snapshots and
results. It does not automatically commit, push, or open pull requests on
your branch.

A repository lock covers all linked worktrees. Concurrent outside edits
during integration stop publication and retain the result. An external
task-file edit imports at the next boundary where no work is in flight, and
at startup when every retained attempt holds a replayable result — a result
commit or a worker receipt. Work that would rerun its worker still asks you
to restore the saved baseline and task contract before resuming. Do not delete
the runtime directory or private refs while you need recovery. Logs and refs
are retained; there is no automatic retention limit yet.

`looper recovery --preview` explains what a guard refused before you change
anything. The preview is read-only: it changes no file, database record,
process, or Git ref. It compares the task file, branch, working tree, and
any retained publication against the saved contract, reports each startup
guard verdict, and lists the supported recovery choices with what each
preserves or changes. Each preview carries a token; `looper recovery --apply
<choice> --token <token>` applies one enumerated choice and re-derives the
token first, so a stale preview is refused without changing anything. A live
controller applies the request at its boundary; a stopped run applies under
the repository lock without launching agents. Add `--contract v2 --json` for
the machine document.

`looper status --json` returns the saved state. `looper tail --json` reads
events. `looper doctor` checks local dependencies and the backlog. Exit
codes are 0 for a completed finite run or drained stop, 2 for blocked work
or a dispatch limit, 1 for a controller failure, and 130 for interruption.

`looper preflight` shows the execution configuration a start would use before
any agent launches: which agent binaries resolve where, which agents, models,
and effort apply and where each value came from (flag, environment, saved
profile, the saved plan, or a planner choice), the launch mode, once
semantics, and the effective verify and setup commands. The preview is
read-only — no controller starts, no agent runs, nothing is billed — and
credentials, environment values, and raw agent arguments stay out of it.
What Looper cannot verify without launching an agent is listed as unchecked.
A missing agent binary or core tool exits 1 like doctor. Add
`--contract v2 --json` for the machine document; it stays within 8 KiB
whatever the configured values hold. A foreground `looper` run
prints the same facts as one concise launch line before it starts; on a
resume the line's verify and setup commands name the layer that will supply
them (the configuration, a recovered command, or the saved plan), because it
reads the saved run read-only like preflight.

`looper inspect <task> --contract v2 [--json]` returns one bounded task
view: the contract and blockers, the current attempt with a stated feedback
excerpt, the newest checks, timing provenance, and the routes that page the
attempt (`--after`) and check (`--checks-after`) history without duplication
or gaps. The document stays within 8 KiB no matter how many repairs a task
accumulated; `--full` returns the complete unpaged report.

`looper runs [--limit N]` lists the saved runs under the configured base
directory (`LOOPER_BASE_DIR`, default `~/.looper`), newest first: each run's
project root, whether it still exists, its retained work, its session count
with the last recorded outcome, controller freshness (owning, stale,
stopped, or unknown), build evidence, and the exact `cd` command that
selects it. The listing is read-only — no run is started, stopped, or
changed — and reads only Looper-owned runtime directories; private agent
session directories are never scanned. `--limit` bounds the page (default
20, at most 100) and the document reports what it omitted. An empty base
directory is a bounded result with a start hint. Add `--contract v2 --json`
for the machine document. No-run errors from other commands name the
resolved project root, task file, and searched base and runtime locations,
and point at `looper runs` for discovery.

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
| `--no-animation` | off | Dashboard without animated motion; status stays readable |
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

### Resume after committing published work

If you commit files that Looper has already published, restart with `looper`.
Looper accepts a descendant commit on the same branch when the committed files
and working files match its accepted tree or its verified publication candidate,
or when the commit changes no file inside a retained or published attempt's
write scope. It preserves your commit and finishes retained publication through
the usual checks.
Staged project changes, other file edits, branch switches, and changed backlog
contracts require reconciliation. `looper doctor --json` reports a moved branch
and the last recorded shutdown before you attempt recovery.

### Commit to the project while a run is live

You can commit files the run does not depend on while the controller keeps a
run live. When the commit is a descendant of the run's baseline and changes no
file inside a retained or published attempt's write scope, the next boundary
accepts it: the run folds the commit into its baseline, records a `recovery`
event that names the accepted commit and its changed paths, and continues.
A held publication re-verifies against the folded baseline before it
publishes. A commit that changes a file inside a write scope competes with
retained work; the run retains everything and the resume requires
reconciliation.

### Add tasks during a run

Use `looper add-task tasks.json` to add tasks through the active controller.
The file contains one task object or an array of task objects:

```json
[
  {
    "id": "T044",
    "title": "Share storage loading checks",
    "priority": 2,
    "details": "Share bounded file reads. Preserve the independent parsers.",
    "steps": ["Run the storage tests."],
    "depends_on": []
  }
]
```

The status defaults to `todo`. You can also supply `blocked`. Use unique IDs
and priorities from 1 through 5. Dependencies can refer to existing tasks or
other tasks in the batch. The controller validates the complete batch before
adding it. Existing tasks and active attempts stay intact.

Add `--json` for a structured acknowledgement. The command returns a receipt
at once and the controller owns the outcome; query it with `looper requests
<id>`. Add `--wait` to wait for the applied acknowledgement, bounded by
`--wait-timeout` (default 30 seconds). A timeout reports the request ID; the
request can still apply later. Check `looper status` before submitting it
again. Requests belong to the current controller session. If that session
ends before application, inspect the backlog before resubmitting. An invalid
batch adds no tasks.

Pass `--idempotency-key <key>` to make a retry safe. The key is scoped to
this project and this operation: repeating the same key with the same task
batch returns the original request instead of adding the tasks twice, and a
changed batch under the same key is rejected as a conflict. Distinct keys
with identical content both apply, so intentional repeats stay possible. A
key is 1 to 128 bytes without control characters. Keys answer from the
retained request receipts: a request that is still running keeps its receipt,
and at most the newest 200 decided receipts stay per run, so a key whose
receipt was pruned behaves as fresh.

This command needs an active controller built with support for `add-task`.
Installing a new binary does not update an existing controller. Use
`looper add-plan requirements.md` when the planner should create tasks from
requirements. `add-plan` and `looper config` accept `--idempotency-key` with
the same semantics; for `add-plan` the key addresses the plan content, not
its path.

### Cancel tasks during a run

Use `looper cancel <task-id>` to withdraw open work through the active
controller. The command reads the task from your task file and sends its
revision with the request: if the task changed, completed, or left the
backlog since you read it, the controller refuses with a conflict and
changes nothing. Done work is accepted history and cannot be cancelled.

A task that other live tasks depend on is refused by name, because a
cancelled dependency never satisfies a dependent. Pass `--cascade` to cancel
the transitive live dependent set in one decision; dependents that are
already done keep their history.

When the target has work in flight, the cancellation is recorded and waits:
the receipt reports `waiting` with the reason `active_work`, the running
worker finishes first — its verified result still passes the checks and is
never discarded — and the cancellation applies at the first boundary where
the work drained. If the work completed first, the task stays done and the
request records that outcome. A restart between the decision and the drain
keeps the decision.

The command returns a receipt at once; query it with `looper requests <id>`.
Add `--wait` for one bounded wait that reports the record, and
`--idempotency-key` with the same semantics as `add-task`. Cancelled tasks
carry the `cancelled` status in the task file, never `done`: they stay in
the backlog for inspection, never re-enter the ready set, and `looper
status --contract v2` counts them under `work.counts.cancelled`. This
command needs an active controller built with support for `cancel-task`.

### Amend tasks during a run

Use `looper amend <task-id> <task.json>` to change one task contract through
the active controller. The file carries the complete amended task: title,
priority, details, steps, `depends_on`, files, blockers, tags, and
`unblock_when`. Identity, status, and `created_at` are not amendable, and the
control tags Looper owns (`project-done`, `looper:operator`, `looper:deferred`,
`looper:finding:*`) cannot be added or removed through a request.

The request carries the task revision your task file held. If the task
changed, left the backlog, or a held task-file edit is waiting for its
boundary import, the controller refuses with a conflict and changes nothing.
Dependency cycles, unknown dependencies, duplicate identifiers, and invalid
write paths are refused before anything commits.

Work that holds the task contract — running, verifying, ready, integrated, or
resume attempts — is refused with the boundary named, because a contract
cannot change under a worker. Wait for the work to finish or cancel it, then
amend the drained task. A done task is never rewritten: the changed
requirements create a follow-up task named `<task-id>-amend-<n>` while the
accepted task, its attempts, and its results stay as they were. A cancelled
task refuses and points at `looper add-task`.

The command returns a receipt at once; query it with `looper requests <id>`,
add `--wait` for one bounded wait, and pass `--idempotency-key` with the same
semantics as `add-task`. This command needs an active controller built with
support for `amend-task`. Installing a new binary does not update a running
controller.
