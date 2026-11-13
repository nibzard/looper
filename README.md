# Looper

Give Looper a repository and written requirements. It runs your coding
agents, checks their work, and applies what passes.

Leave it overnight. Come back to the terminal to see what finished and
what needs you.

When the goal is complete, it waits for your next edit.

![A recorded run: parallel workers, the events view, and one blocked task
with its reason](docs/assets/loop.gif)

> [!WARNING]
> Worker agents run with permission checks disabled. Run Looper only in
> repositories whose contents you trust. See
> [agent permissions](docs/agents.md#agent-permissions).

## Quickstart

You need Linux or macOS, Go 1.22 or later, Git, Bash, and an authenticated
`claude`, `codex`, or `zcode` command.

1. Install Looper:

   ```sh
   curl -fsSL https://raw.githubusercontent.com/nibzard/looper/main/scripts/install-latest.sh | bash
   ```

   The script builds the latest release and installs `looper` in
   `~/.local/bin`. Run it again to update. For other options, see
   [Install Looper](docs/install.md).

2. In the root of your project, describe the outcome you want and what is
   out of scope. Use a file such as `PROJECT.md`.

3. Start the loop:

   ```sh
   cd /path/to/project
   looper
   ```

If `to-do.json` does not exist, the planning agent creates tasks from your
requirements. A project without Git gets a local repository unless you set
`LOOPER_GIT_INIT=0`.

Looper does not commit, push, or open pull requests. Your branch, `HEAD`,
and staged index stay unchanged. Accepted changes appear as unstaged edits.
Review them and commit them yourself.

## Everyday commands

| Command | What it does |
| --- | --- |
| `looper` | Starts or resumes a continuous run in the foreground. |
| `looper --once` | Runs until the current backlog finishes, then exits. |
| `looper start` | Starts a detached controller for an unattended run. |
| `looper tui` | Starts a background run if none is active, then opens the dashboard. `q` can detach. |
| `looper status` | Shows the saved state of the run. |
| `looper tail --follow` | Streams events. |
| `looper preflight` | Shows the configuration a start would use. No agent runs. |
| `looper stop` | Stops new dispatch and lets active work finish. |
| `looper stop --interrupt` | Stops at once and keeps partial work, like Ctrl+C. |
| `looper add-task tasks.json` | Adds tasks to a live run. |
| `looper add-plan requirements.md` | Asks the planner to add tasks from requirements. |
| `looper runs` | Lists saved runs on this machine. |
| `looper recovery --preview` | Explains a refused start and lists recovery choices. |
| `looper doctor` | Checks dependencies and the backlog. |

Run `looper help <command>` for the options and exit codes of one command.
A word that names no command and no file is a usage error, so a typo cannot
start a run.

To stop at once, press Ctrl+C or run `looper stop --interrupt`. Looper
stops child processes and keeps partial work. Run `looper` again to
resume.

## How it works

Each task starts with fresh agent context.

1. Read the goal, task dependencies, and acceptance criteria.
2. Plan independent write scopes, shared resources, setup, and verification.
3. Implement ready tasks in private Git worktrees.
4. Combine each result with the current integration revision and run checks.
5. Publish only the exact checked tree, then mark its task complete.
6. Review complete product workflows at milestones and at the end.
7. Add tasks with evidence from the review and continue.

A dependent task becomes ready only after its dependency integrates. Work on
unrelated tasks continues during repairs. Repeated failures trigger a fresh
diagnosis. A task that still fails becomes blocked. Its code and logs stay
available.

Looper chooses up to four workers automatically. It compares the actual Git
changes during integration and sends conflicts back to a worker for repair.
A result publishes only while the revision it was checked against is still
current.

A goal is complete only when a review returns `complete: true`, every task
is done, and no finding is open. Looper then waits. Edit the task file or
the requirements, and it continues.

## The task file

Looper keeps the backlog in `to-do.json`. A minimal file looks like this:

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

The planning agent finds your setup and test commands. To supply them
yourself:

```sh
export LOOPER_SETUP_COMMAND='npm ci'
export LOOPER_VERIFY_COMMAND='npm test && npm run build'
looper
```

Edit `to-do.json` while Looper is idle. Looper imports the change at the
next safe boundary. To change a live run, use `looper add-task`,
`looper cancel`, or `looper amend`. See
[Tasks and checks](docs/tasks-and-checks.md) and
[Change the backlog during a run](docs/runtime-requests.md).

## Agents

Run `looper config` to name agent commands as profiles and to choose the
worker and reviewer profiles. Each profile uses a supported adapter:
Claude Code, Codex, or Zcode. Saving configuration runs no agents.

```sh
looper --agent glm --review-agent anthropic
looper --model YOUR_MODEL --effort high
```

`--agent` selects the worker profile. `--review-agent` selects the profile
for planning, diagnosis, and product review. See
[Agents and models](docs/agents.md) and
[agent profiles](docs/AGENT-PROFILES.md).

## Configuration

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
| `--foreground` | off | `looper tui` runs the controller in the dashboard; exiting stops the run |
| `LOOPER_RATE_LIMIT_MAX` | `12` | Maximum agent invocations for a transient failure |
| `LOOPER_RATE_LIMIT_DELAY` | `60` | Initial retry delay, in seconds |
| `LOOPER_BASE_DIR` | `~/.looper` | State and artifact location |
| `CODEX_BIN`, `CLAUDE_BIN`, `ZCODE_BIN` | `codex`, `claude`, `zcode` | Installed agent commands |
| `LOOPER_HOOK` | unset | Optional executable called after a result |

The hook receives the task ID, status, summary path, and attempt ID. It has
a 30-second timeout. A hook failure does not roll back the task result, and
Looper does not guarantee delivery.

The controller does not set a token budget. More workers use more
resources and cause more conflicts.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | A finite run completed, or a stop drained. |
| 1 | The controller failed, or the input is invalid. |
| 2 | Work is blocked, or the dispatch limit was reached. |
| 130 | The run was interrupted. |

## Documentation

- [Install Looper](docs/install.md): release, source, skills, and uninstall.
- [Dashboard](docs/dashboard.md): states, keys, statistics, and cost estimates.
- [Tasks and checks](docs/tasks-and-checks.md): task fields, follow-ups,
  blocked work, and test resources.
- [Agents and models](docs/agents.md): Claude, Codex, zcode, and permissions.
- [State and recovery](docs/recovery.md): the runtime directory, restarts,
  and your own commits during a run.
- [Change the backlog during a run](docs/runtime-requests.md): add, cancel,
  and amend tasks.
- [Observe runs](docs/observability.md): bounded JSON for coding agents,
  timing, and failure summaries.
- [Agent contracts](docs/AGENT-CONTRACTS.md): version 2 schemas and rules.
- [Architecture](docs/ARCHITECTURE.md): state invariants and failure
  behavior.
- [Migration](docs/migration.md): read before you upgrade an older runner.

## Measuretwice

[Measuretwice](adapters/measuretwice/README.md) is an optional adapter. It
checks task acceptance against the patch and verification evidence. A
`fail` or `review` result requests an independent product review. It does
not reject a change by itself, and it does not replace tests.

## Development

Follow [`MANIFESTO.md`](MANIFESTO.md) when you change Looper: one purpose,
simple before easy, do not make me think, believe in the loop, and take
things away.

```sh
make test
make race
make test-measuretwice   # needs the optional adapter dependencies
```

The integration suite uses the real executable, real Git repositories, and
local agent stubs. The entry point is `cmd/looper/main.go`. The controller
is in `internal/looper`. See [the validation record](docs/VALIDATION.md)
for the automated checks and live runs, and
[interface dependencies](docs/dependencies.md) for the pinned packages.
