# Looper

A task runner for the Codex CLI with optional Claude Code CLI (`claude`)
interleaving. Looper finds independent work in a JSON backlog and runs it in
separate Git worktrees. Each task gets fresh context. Looper checks combined
changes before it updates your project and marks tasks done.

The entrypoint is `bin/looper.sh`. The parallel coordinator is
`lib/looper_parallel.py` and uses the Python standard library.

## Quick Start
```bash
./install.sh
looper.sh
```

If `to-do.json` is missing, Looper bootstraps it from project docs, validates it
against `to-do.schema.json`, and repairs it if needed.

No parallel setup is required. Run from the Git repository root with Python
3.9 or later available. Use `looper.sh --jobs 1` for the sequential runner.

## What It Does
- Bootstraps `to-do.json` and `to-do.schema.json` if missing.
- Validates `to-do.json` (jsonschema if available, jq fallback).
- Repairs invalid task files via Codex or `claude`.
- Plans independent tasks and project checks from the repository itself.
- Runs ready tasks in isolated worktrees, with one task per agent session.
- Gives priority to tasks that unblock longer dependency chains.
- Tests combined changes before it publishes them and releases dependent tasks.
- When tasks are exhausted, runs a review pass; it must append a final
  `project-done` marker task if no new work is found.
- Supports interleaving `claude` for iterations and a configurable review agent.
- Enforces structured summaries and logs JSONL per run.
- Applies validated model summaries back into `to-do.json`.

## Install
```bash
./install.sh
```

Common options:
```bash
./install.sh --codex-home ~/.codex
./install.sh --prefix /opt/looper
./install.sh --skip-skills
```

Tip: If `~/.local/bin` is not on PATH, add it to your shell profile.

## Homebrew
```bash
brew install <tap>/looper
looper-install --skip-bin
```

`looper-install` installs skills into `~/.codex/skills` by default.

## Usage
```bash
looper.sh [to-do.json]
looper.sh run [to-do.json]
looper.sh --ls todo [to-do.json]
looper.sh --tail --follow
looper.sh --doctor [to-do.json]
looper.sh --interleave
looper.sh --smart
looper.sh --jobs 3
looper.sh --jobs 1
looper.sh --iter-schedule odd-even
looper.sh --iter-schedule round-robin --rr-agents claude,codex
```

## Interleaving
`--interleave` runs iterations with `claude` and keeps the
final review pass on Codex. Iteration schedules are configurable:

- `--iter-schedule codex` (default)
- `--iter-schedule claude`
- `--iter-schedule odd-even` (odd uses Codex, even uses `claude`)
- `--iter-schedule round-robin` (uses `--rr-agents`, default `claude,codex`)

`claude` runs with `--dangerously-skip-permissions` and `--output-format json`.

Optional overrides:
```
--odd-agent <codex|claude>
--even-agent <codex|claude>
--rr-agents <comma-separated list>
--repair-agent <codex|claude>
--review-agent <codex|claude>
```

Use `--smart` (or `-s`) to select the smart Codex configuration.
Normal mode uses `gpt-6-sol` with `max` reasoning effort.
Smart mode uses `gpt-6-astra` with `max` reasoning effort.

`--interleave` also defaults repair to `claude`; use `--repair-agent codex` to keep Codex.

### Examples

Use Claude for all iterations, Codex for review:
```bash
looper.sh --interleave
```

Alternate between Codex (odd) and Claude (even):
```bash
looper.sh --interleave --iter-schedule odd-even
```

Flip the pattern (Claude on odd, Codex on even):
```bash
looper.sh --interleave --iter-schedule odd-even --odd-agent claude --even-agent codex
```

Round-robin through multiple agents:
```bash
looper.sh --interleave --iter-schedule round-robin --rr-agents claude,codex,claude,codex
```

Custom pattern (2x Claude, then Codex, repeating):
```bash
looper.sh --iter-schedule round-robin --rr-agents claude,claude,codex
# Pattern: claude → claude → codex → claude → claude → codex → ...
```

The final review pass uses Codex by default. Set `--review-agent claude` to use Claude.

## Task File (to-do.json)
`to-do.json` is the source of truth for the loop and must match
`to-do.schema.json`. Each worker receives one task. The coordinator selects:

1. Unfinished work retained from an earlier run.
2. Ready tasks whose dependencies are integrated and done, ordered by priority.
3. Within one priority, tasks on longer remaining dependency chains, then task ID.

The sequential runner keeps its original order: `doing` first, then ready
`todo` tasks by priority and task ID. You do not need new backlog fields for
parallel execution.

Looper does not retry blocked tasks automatically. If no task can run, it exits
with code 2. Resolve a blocker and change its status to `todo` to retry it.
Task IDs must be unique. Each dependency must name another task in the file.
Put a clear acceptance criterion in each task's `details` field.

Minimal example:
```json
{
  "schema_version": 1,
  "source_files": ["README.md"],
  "tasks": [
    {
      "id": "T1",
      "title": "Add README",
      "priority": 1,
      "status": "todo"
    }
  ]
}
```

## Parallel execution

`--jobs auto` is the default. Looper starts with a limit of up to four workers,
based on available processors and memory. It reduces dispatch under resource
pressure, when checks accumulate, or after integration failures. Successful
integrations gradually restore the initial limit. `--jobs N` sets the maximum;
the coordinator can still use fewer workers. `LOOPER_JOBS` sets the same default.

Before work starts, a planning agent reads the backlog, code, and repository
instructions. It identifies file scopes, shared interfaces, dependency setup,
and integration checks. Tasks with overlapping scopes or shared resources run
sequentially. Missing scopes are treated as project-wide. This internal plan
does not change your backlog or require manual file ownership rules.

Each task runs in a separate worktree. Looper captures its changes in an internal
commit. A separate worktree combines the result with integrated work and runs
the planned checks. `LOOPER_VERIFY_COMMAND`, when set, overrides the planned
integration check. Setup and verification have a separate concurrency limit.
Checks invoked directly by agents still follow the repository's instructions.

Checks can run concurrently on a chain of candidate commits. Each candidate
includes the changes ahead of it. Publication follows that order.
Only a result tested against the current integration base can be published.
If that base changes, Looper rebuilds and rechecks the candidate. Conflicts and
failed checks return to the task worker for up to two repair attempts. Failed
work stays isolated. Dependent tasks start as soon as their prerequisites
integrate; they do not wait for unrelated workers.

`MAX_ITERATIONS` limits task dispatches in a run. Integration repairs do not
consume additional dispatches. At the limit, Looper drains running tasks and
checks. It performs final review when all tasks finish. Review can add tasks;
only a successful review can add the `project-done` marker. Automatic dispatch
does not use the sequential runner's `LOOP_DELAY_SECONDS` pause.

One process lock covers a repository and its linked worktrees, including
sequential runs. A second Looper invocation exits without claiming tasks.

## Recovery and project files

Parallel mode leaves your branch and staged changes intact. It snapshots
tracked and nonignored untracked project files into an internal Git reference.
Workers start from that snapshot, including uncommitted edits. Repositories
without an initial commit are supported. Verified changes
return to your working directory as unstaged edits. Looper does not push them.
Ignored dependencies and build output are recreated by the planned setup command.

Only the coordinator writes your task file. It stores attempt records, summaries,
check logs, and a publication journal under
`~/.looper/<project>-<hash>/parallel/`. These records remain available even when
`CODEX_JSON_LOG=0` disables the consolidated activity log.

Run the same command after interruption. Looper stops surviving attempt
processes, resumes partial work, rechecks pending results, and completes any
recorded publication before it releases dependencies. It retains failed and
blocked worktrees for inspection. Successful worktrees are removed; their
commits remain reachable through internal `refs/looper` and `refs/looper-tasks`
references.

If you change project files, the branch, or an active task's requirements while
unintegrated work is retained, Looper stops and retains its work. It does not overwrite
those changes or apply a result to changed acceptance criteria. Avoid editing
the project while Looper is running. After a run drains all attempts, the next
run can start from a new snapshot of your edited project.

Automatic mode uses the sequential runner when worktrees are unavailable:
outside a repository root, with submodules, or with
unresolved index conflicts. Explicit `--jobs N` values above one report these
conditions as errors. Without Python, automatic mode also uses the sequential
runner; repository locking and durable worktree recovery require Python.


## Logs and output

Parallel runs show running tasks, verification, completed counts, and task results.
Use `--tail --follow` for the latest activity. Detailed agent output and check
logs remain in each attempt directory.

The sequential runner shows the following output:

```text
Looper
  Project   /home/agent/measuretwice
  Agents    claude | review: claude | repair: claude
  Claude    default model
  Run       Up to 50 iterations | pause: 90s
  Tasks     to-do.json
  Backlog   12/15 done | 3 ready | 0 waiting | 0 blocked
  Log       .../20260924-101305-651973.jsonl

10:13:05  [1/50] T063  claude
  Obtain human review of public challenge labels
  Blocked  2m 08s | 0 files | 12/15 done
    The public labels need human review.
    Blocker: A reviewer must approve the challenge labels.

10:16:48  [2/50] T071  claude
  Publish API, schema, and CLI references
  Done  4m 12s | 3 files | 13/15 done
    Added the references and checked the examples.
```

Each task shows its start time, iteration, agent, elapsed time, and reported file count.
The result includes the agent's summary and blocker reasons.
Looper validates the summary and applies the task update before it reports a task as done.
If you set `LOOPER_VERIFY_COMMAND`, that command must also succeed.

During a run, a live line shows elapsed time or the pause countdown.
Completed entries stay in terminal scrollback.
Redirected output uses plain text with no colors or live line updates.
Set `NO_COLOR=1` to disable colors, or `TERM=dumb` to disable all terminal formatting.
Set `CODEX_PROGRESS=0` to hide live activity and agent events.

The final report separates results from this run and the full backlog.
It shows why Looper stopped, blocked tasks, their reasons, and the next action.
`ready` counts tasks that can run. `waiting` counts tasks with unfinished dependencies.
Backlog counts exclude the final `project-done` marker.
Looper skips the pause when no next task can run or the iteration limit is reached.

Use `looper.sh --verbose` (or `-v`) to include configuration details and agent events.
You can also set `LOOPER_VERBOSE=1`.

Logs are stored per project under:
```
~/.looper/<project>-<hash>/
```

Each run produces:
- `<timestamp>-<pid>.jsonl` full JSONL trace
- `<timestamp>-<pid>-<label>.last.json` the last assistant message

`looper.sh --tail --follow` watches the latest run in real time.

## Hooks
Set `LOOPER_HOOK` to run a script after each iteration:
```
LOOPER_HOOK=/path/to/hook.sh
```

The hook receives:
```
<task_id> <status> <last_message_json> <label>
```

## Configuration
Environment variables (defaults in parentheses):

- `MAX_ITERATIONS` (50)
- `CODEX_MODEL` (gpt-6-sol)
- `CODEX_SMART_MODEL` (gpt-6-astra)
- `CODEX_REASONING_EFFORT` (max)
- `CODEX_SMART_REASONING_EFFORT` (max)
- `CODEX_YOLO` (1)
- `CODEX_FULL_AUTO` (0)
- `CODEX_PROFILE` (empty)
- `CODEX_JSON_LOG` (1)
- `CODEX_PROGRESS` (1; show live activity and agent events)
- `LOOPER_VERBOSE` (0; show configuration and agent events)
- `NO_COLOR` (empty; any nonempty value disables colors)
- `CODEX_ENFORCE_OUTPUT_SCHEMA` (1)
- `LOOPER_VERIFY_COMMAND` (empty)
- `CLAUDE_BIN` (claude)
- `CLAUDE_MODEL` (empty)
- `LOOPER_ITER_SCHEDULE` (codex)
- `LOOPER_ITER_ODD_AGENT` (codex)
- `LOOPER_ITER_EVEN_AGENT` (claude)
- `LOOPER_ITER_RR_AGENTS` (claude,codex)
- `LOOPER_REPAIR_AGENT` (codex)
- `LOOPER_REVIEW_AGENT` (codex)
- `LOOPER_INTERLEAVE` (0)
- `LOOPER_BASE_DIR` (~/.looper)
- `LOOPER_APPLY_SUMMARY` (1; other values are not supported)
- `LOOPER_GIT_INIT` (1)
- `LOOPER_HOOK` (empty)
- `LOOP_DELAY_SECONDS` (90)

Set `LOOPER_VERIFY_COMMAND='make test'` to check a completed task before Looper
marks it done. Use a command that works in the target project. Looper runs it
from the project directory.

In sequential mode, an iteration stops with an error if the agent fails, returns an invalid summary,
or fails verification. Looper restores the task file to its prior state. Code
edits and commits remain for inspection. A failed review never marks the
project done. Reaching the iteration limit with open tasks returns exit code 2.

## Git Behavior
If the project is not a git repo and `LOOPER_GIT_INIT=1`, Looper runs `git init`.
If git is unavailable or init fails, Codex runs with `--skip-git-repo-check`.

## Included skills

This repo ships a small, focused skills bundle:

- `git-conventional-commit`: Create scoped commits and preserve unrelated edits.
- `todo-json-manager`: Maintain task backlogs and report verified Looper outcomes.
- `release-runbook`: Prepare and publish releases within the requested scope.

They are installed into `~/.codex/skills` by default.
Run `./install.sh` to update Looper and the bundled skills.

The release helper requires `--test-cmd` or an explicit `--skip-tests`.
It runs checks after version updates. Dry runs stay offline and leave files unchanged.
Use the helper only when publishing is authorized.
See its [usage reference](skills/release-runbook/references/helper.md) for staging and recovery rules.

The local `looper-live-test` skill is in `.codex/skills` and is not part of the installed bundle.
It checks task selection, task status, and file creation in a temporary project.
Routine checks use local stubs; a live smoke test uses a real agent.

## Dev Notes
```bash
make install
make uninstall
make smoke
make test
```

The shell runner is in `bin/looper.sh`. The worktree scheduler and recovery
logic are in `lib/looper_parallel.py`. `scripts/parallel-test.py` exercises
real Git repositories with local agent stubs, including process interruption,
speculative verification, conflict repair, and preservation of staged files.
See [the comparison pilot](EXPERIMENT-PLAN.md) for the remaining product test.
The dated [research report](docs/research/LOOPER-RESEARCH-REPORT.md) records the
earlier decision to keep a smaller Bash controller. The [Go proposal](docs/archive/MIGRATION.md)
is archived.
