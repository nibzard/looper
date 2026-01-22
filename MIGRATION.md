# Go Rewrite Migration Spec

This document specifies a full rewrite of Looper in Go. It is deliberately
opinionated: the loop is the product, prompts are the value, and everything
else (UX, devex, observability) is support.

## Principles

- The loop is all you need. Everything else is UX, devex, and observability.
- Prompts are the primary product surface, but they are internal assets. End
  users should not see or edit prompts by default.
- Keep the system simple and robust, not over-engineered.
- Assume AI agents can self-heal and repair tasks; provide guardrails, not rigid
  bureaucracy.
- Breaking changes are acceptable if they improve the product.

## Goals

- Full rewrite in Go with a clean, testable core.
- Prompts as first-class assets with simple templating (developer-editable,
  user-hidden).
- Deterministic task selection and summary application.
- Clear, structured logging and tailing.
- Optional TUI that adds real observability value (not required).

## Non-Goals

- Backward compatibility with existing CLI flags/env vars.
- Preserving the shell script implementation.
- Building a complex UI or plugin ecosystem.
- Writing prompts into user repos or exposing them by default.

## Architecture Overview

High-level data flow:

1) Load config + prompts
2) Load/validate to-do.json
3) Select task
4) Render prompt
5) Run agent
6) Parse summary
7) Update tasks + logs
8) Repeat or review

Core packages (Go modules):

- `cmd/looper`: CLI entrypoint and subcommands
- `internal/config`: config loading + defaults (flags, env, config file)
- `internal/prompts`: prompt store + templating renderer
- `internal/todo`: schema, parsing, selection, and updates
- `internal/loop`: orchestration state machine
- `internal/agents`: codex/claude runners
- `internal/logging`: JSONL log writer + tail support
- `internal/hooks`: hook invocation
- `internal/ui`: optional TUI (Bubble Tea)

## CLI Spec (breaking changes allowed)

Primary commands:

- `looper run [path]`: run the loop (default)
- `looper doctor [path]`: validate deps + task file
- `looper tail [--follow]`: tail last log
- `looper ls <status> [path]`: list tasks by status
- `looper version`

Core flags (illustrative):

- `--todo <path>`: task file (default: `to-do.json`)
- `--prompt-dir <path>`: prompt directory override (dev-only, hidden)
- `--max-iterations <n>`
- `--agent <codex|claude|schedule>`
- `--schedule <codex|claude|odd-even|round-robin>`
- `--review-agent <codex|claude>`
- `--repair-agent <codex|claude>`
- `--log-dir <path>`
- `--apply-summary <true|false>`
- `--ui <none|tui>`

## Prompt System (first-class)

### Storage

Prompts live as files in an internal directory, editable by developers:

```
prompts/
  bootstrap.txt
  repair.txt
  iteration.txt
  review.txt
  summary.schema.json
```

Prompts are shipped with the binary and loaded from an internal location
(for example `$XDG_DATA_HOME/looper/prompts` or embedded). Developers can
override with a hidden `--prompt-dir` or `LOOPER_PROMPT_DIR`.

### Rendering

Use Go `text/template` with strict missing key behavior. Keep variables minimal:

- `{{.TodoPath}}`
- `{{.SchemaPath}}`
- `{{.WorkDir}}`
- `{{.SelectedTask.ID}}`
- `{{.SelectedTask.Title}}`
- `{{.SelectedTask.Status}}`
- `{{.Iteration}}`
- `{{.Schedule}}`
- `{{.Now}}` (UTC timestamp)

### Visibility

Prompts are not exposed by default. Only developers/operators should access
them via an internal override path. Prompt contents are never written into user
projects.

#### Internal override workflow (dev-only)

Two safe options for prompt iteration without exposing prompts to end users:

1) **Build tag gate**
   - Enable `--prompt-dir` and `--print-prompt` only when compiled with a
     `devprompts` build tag.
   - Production builds omit those flags entirely.

2) **Env gate**
   - Enable overrides only when `LOOPER_PROMPT_MODE=dev` is set.
   - In normal usage, the flags are ignored or hidden from help output.

### Design Rules

- Prompts are plain text files, not compiled into code (unless embedded for
  distribution).
- Keep them short and focused.
- Defaults ship with the binary, but are not materialized into user projects.

## Task File Spec

Continue using `to-do.json` with the existing schema and fields. The Go rewrite
should:

- Validate with a full JSON Schema library.
- Provide a minimal fallback validation for schema_version/source_files/tasks.
- Enforce status values (`todo|doing|blocked|done`).
- Keep formatting stable (2-space indent) when writing.

Selection algorithm (deterministic):

1) Any task with `doing` (lowest id wins).
2) Otherwise highest priority `todo` (priority 1 highest), tie-break by id.
3) Otherwise highest priority `blocked`, tie-break by id.

When a task is selected, set it to `doing` before running the agent.

## Loop Flow Spec

1) Ensure schema exists.
2) Bootstrap `to-do.json` if missing.
3) Validate tasks; repair via agent if invalid.
4) While tasks remain:
   - Select task
   - Mark as doing
   - Render prompt (iteration)
   - Run agent
   - Parse summary JSON
   - If summary task_id mismatches selection:
     - Warn + skip summary apply (repair flow can handle)
   - Apply summary to task file
5) If no tasks remain:
   - Run review pass
   - Append project-done if review adds no tasks

## Agents

Codex and Claude are adapters that execute external binaries:

- `codex exec` with `--json` and `--output-last-message` when available
- `claude` with `--output-format stream-json` and parse last message

The agent layer should:

- Stream logs to JSONL in a structured format
- Surface errors and exit codes explicitly
- Honor per-agent timeouts

## Logging and Observability

Log format:

- JSONL with event types (assistant_message, tool, command, error, summary)
- `last-message.json` always written when summary is expected

Log layout:

```
~/.looper/<project>-<hash>/
  <timestamp>-<pid>.jsonl
  <timestamp>-<pid>-iter-1.last.json
```

Observability goals:

- CLI shows key progress lines (task selection, summary, errors).
- `looper tail --follow` displays the most recent activity.

## Optional TUI (Charm)

Charm stack findings (from upstream docs):

- **Bubble Tea**: Go TUI framework based on Elm Architecture, supports inline
  and full-window TUIs, with a framerate-based renderer, mouse support, and
  focus reporting. Source: https://github.com/charmbracelet/bubbletea
- **Bubbles**: UI components (list, table, viewport, spinner, progress, help).
  Source: https://github.com/charmbracelet/bubbles
- **Lip Gloss**: declarative terminal styling, CSS-like, with color profile
  handling. Source: https://github.com/charmbracelet/lipgloss

### Does it make sense?

Yes, but only if it materially improves observability:

- Live task list with statuses
- Real-time log tail
- Current iteration + agent status
- Summary/result panel

The TUI must be optional:

- Default remains CLI
- TUI behind `--ui tui` or `looper tui`

Keep the TUI thin: it should subscribe to the same log stream and state as the
CLI, not introduce separate logic.

## Configuration

Prefer a single config file (simple, editable) in TOML:

```
looper.toml
```

The config should cover:

- Paths (todo, prompt dir, log dir)
- Agents (binary, model, flags)
- Loop settings (max iterations, schedule)
- Output settings (progress, json log)

Env vars can override config for quick use.

## Small Feature Ideas (simple, optional)

- `--print-prompt` to dump the rendered prompt before running (dev-only).
- `looper doctor` for dependency and config validation.
- `looper prompts edit <name>` for dev builds (hidden).

## Migration Plan (Phased)

1) **Foundation**
   - Config loader + prompt renderer
   - Task parsing + selection
2) **Core Loop**
   - Agent execution, summary parsing, task updates
   - JSONL logging
3) **Bootstrapping + Repair**
   - Schema generation, bootstrap, repair
4) **UX/DevEx**
   - `doctor`, `tail`, `ls` (+ internal prompt tooling for dev builds)
5) **Optional TUI**
   - Bubble Tea UI reading the same logs/state
6) **Stabilization**
   - Integration tests and smoke tests

## Testing Plan

- Unit tests for task selection and summary application.
- Golden tests for prompt rendering.
- Integration test with stub agent binary to simulate runs.
- Smoke test that bootstraps a temp project and runs one iteration.

## Risks and Mitigations

- **Prompt drift**: keep prompts in files and version them.
- **Agent mismatch**: enforce summary/task ID checks.
- **Complexity creep**: keep TUI optional and thin.
- **Schema rigidity**: support self-healing repair flows.

## Open Questions

- Should prompts be embedded in the binary or loaded from a packaged data dir?
- Should dev-only prompt editing be a build tag or a hidden flag?
