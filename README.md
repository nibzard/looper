# Looper

A deterministic, autonomous loop runner for the Codex CLI with optional Claude Code CLI (`claude`)
interleaving. It processes exactly
one task per iteration from a JSON backlog, with fresh context each run, and
keeps a JSONL audit log for traceability.

The active implementation is `bin/looper.sh`.

## Quick Start
```bash
./install.sh
looper.sh
```

If `to-do.json` is missing, Looper bootstraps it from project docs, validates it
against `to-do.schema.json`, and repairs it if needed.

## What It Does
- Bootstraps `to-do.json` and `to-do.schema.json` if missing.
- Validates `to-do.json` (jsonschema if available, jq fallback).
- Repairs invalid task files via Codex or `claude`.
- Runs one task per iteration (doing > runnable todo).
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
`to-do.schema.json`. The loop chooses a single task each iteration:

1. Any task in `doing` (lowest id wins).
2. Otherwise, the highest priority `todo` task whose `depends_on` tasks are done.

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

## Logs and Output
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
- `CODEX_PROGRESS` (1)
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

An iteration stops with an error if the agent fails, returns an invalid summary,
or fails verification. Looper restores the task file to its prior state. Code
edits and commits remain for inspection. A failed review never marks the
project done. Reaching the iteration limit with open tasks returns exit code 2.

## Git Behavior
If the project is not a git repo and `LOOPER_GIT_INIT=1`, Looper runs `git init`.
If git is unavailable or init fails, Codex runs with `--skip-git-repo-check`.

## Included Skills
This repo ships a small, focused skills bundle:
- `git-conventional-commit`
- `todo-json-manager`
- `release-runbook`

They are installed into `~/.codex/skills` by default.

## Dev Notes
```bash
make install
make uninstall
make smoke
make test
```

The runner is in `bin/looper.sh`; read its docstring for its behavior.
See [the comparison pilot](EXPERIMENT-PLAN.md) for the remaining product test.
The dated [research report](docs/research/LOOPER-RESEARCH-REPORT.md) records the
decision to keep a smaller Bash controller. The [Go proposal](docs/archive/MIGRATION.md)
is archived.
