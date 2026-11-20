# Looper

Looper turns a requirements file into finished, tested code. It splits the
work into tasks and runs Claude Code, Codex, or Zcode on them in parallel.
It checks every result before it accepts it. Start it in the evening. In the
morning, review the diff and the tasks that need you.

![looper tui: planning, two parallel workers, four tasks integrated, and one
task blocked because Redis is not installed](docs/assets/loop.gif)

> [!WARNING]
> Worker agents run with permission checks disabled. Run Looper only in
> repositories whose contents you trust. See
> [agent permissions](docs/agents.md#agent-permissions).

## Quickstart

You need Linux or macOS, Go 1.22 or later, Git, Bash, and an authenticated
`claude`, `codex`, or `zcode` command.

1. Install Looper. The script builds the latest release into `~/.local/bin`.

   ```sh
   curl -fsSL https://raw.githubusercontent.com/nibzard/looper/main/scripts/install-latest.sh | bash
   ```

2. In your project, write what you want in `PROJECT.md`:

   ```markdown
   # jobq

   A durable job queue with an HTTP API and a command-line client.
   Jobs survive a restart. Failed jobs retry with backoff.
   Out of scope: authentication and a web UI.
   ```

3. Start the run and open the dashboard:

   ```sh
   looper tui
   ```

   The first start asks which agent to use. The planner turns your
   requirements into `to-do.json`, and the workers start.

4. Close the dashboard with `q`, then **Detach**. The run continues. Open the
   dashboard again with `looper tui`.

5. When the dashboard says `COMPLETE` or `BLOCKED`, review the result with
   `git diff`, and commit it.

## What Looper does to your repository

Looper never commits on its own. Your branch, `HEAD`, and staged index stay
unchanged. Each accepted task appears in your working tree as unstaged
edits, after its checks pass. Commit them yourself, or let
`looper publish` make one checked commit.

Workers write in private Git worktrees, not in your working tree. State and
logs go to `~/.looper`. A project without Git gets a local repository
unless you set `LOOPER_GIT_INIT=0`.

## How a run works

1. The planner reads your requirements and writes tasks with dependencies
   and acceptance criteria. It also finds your setup and test commands.
2. Up to four workers implement ready tasks in parallel. Each task starts
   with fresh agent context.
3. Looper merges each result with the latest accepted work and runs your
   checks. A conflict or a failed check goes back to a worker for repair.
4. Only a tree that passes reaches your working tree. Then the task is
   done, and the tasks that depend on it become ready.
5. A reviewer agent tests complete workflows every few tasks and at the end.
   It adds tasks for the gaps it finds.

A task that fails again after a fresh diagnosis becomes blocked. Its code
and logs stay available, and unrelated work continues. When the reviewer
reports the goal complete and no task is open, Looper waits. Edit the
requirements or the task file, and it continues.

## Commands you use often

| Command | What it does |
| --- | --- |
| `looper tui` | Starts a background run if none is active, and opens the dashboard. |
| `looper` | Runs in the foreground. Ctrl+C stops at once and keeps partial work. |
| `looper status` | Shows the state of the run. |
| `looper add-plan more.md` | Asks the planner to add tasks from new requirements. |
| `looper requeue T004` | Retries a blocked task after you fix its blocker. |
| `looper stop` | Lets active work finish, then stops. Add `--interrupt` to stop at once. |
| `looper config` | Chooses the agents and models for workers and reviewers. |
| `looper doctor` | Checks dependencies and the backlog. |

Run `looper` again to resume a stopped run. Run `looper help` for all
commands, and `looper help <command>` for the options of one command.

## Tasks and checks

`to-do.json` holds the backlog. The planner writes it, but you can also
write it yourself:

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

To set the setup and test commands yourself:

```sh
LOOPER_SETUP_COMMAND='npm ci' LOOPER_VERIFY_COMMAND='npm test' looper tui
```

Edit `to-do.json` only while Looper is idle. During a run, use
`looper add-task`, `looper cancel`, or `looper amend`.

## Agents

The first interactive start asks which agent commands to use. Run
`looper config` to change them later. You can choose one profile for
workers and another for planning and review. To choose for one run:

```sh
looper --agent claude --review-agent codex --model YOUR_MODEL --effort high
```

## Documentation

- [Install Looper](docs/install.md): releases, source builds, skills, and
  uninstall.
- [Dashboard](docs/dashboard.md): states, keys, statistics, and cost
  estimates.
- [Tasks and checks](docs/tasks-and-checks.md): task fields, follow-ups,
  blocked work, and test resources.
- [Change the backlog during a run](docs/runtime-requests.md): add, cancel,
  and amend tasks.
- [Agents and models](docs/agents.md) and
  [agent profiles](docs/AGENT-PROFILES.md).
- [Configuration](docs/configuration.md): all settings and exit codes.
- [State and recovery](docs/recovery.md): the runtime directory, restarts,
  and your own commits during a run.
- [Observe runs](docs/observability.md) and
  [agent contracts](docs/AGENT-CONTRACTS.md): JSON output for scripts and
  coding agents.
- [Architecture](docs/ARCHITECTURE.md): state invariants and failure
  behavior.
- [Migration](docs/migration.md): read this before you upgrade an older
  runner.
- [Measuretwice](adapters/measuretwice/README.md): an optional adapter that
  checks task acceptance against the patch.

## Development

Read [`MANIFESTO.md`](MANIFESTO.md) before you change Looper.

```sh
make test
make race
python3 scripts/record-demo.py   # re-record the README figures
```

The integration suite runs the real executable against real Git
repositories with stub agents. The entry point is `cmd/looper/main.go`, and
the controller is in `internal/looper`. See
[the validation record](docs/VALIDATION.md).

## License

Looper is released under the [MIT License](LICENSE).
