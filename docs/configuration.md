# Configuration

Save options with `looper config`, or set them with environment variables
or command-line flags. A flag wins over the environment. The environment
wins over the saved profile. Run `looper preflight` to see the configuration a
start would use. No agent runs.

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
| `LOOPER_SETUP_COMMAND` | detected | Setup command for each check, such as `npm ci` |
| `LOOPER_VERIFY_COMMAND` | detected | Verification command, such as `npm test` |
| `--verbose` | off | Print the run configuration at start |
| `--no-animation` | off | Dashboard without animated motion; status stays readable |
| `--foreground` | off | `looper tui` runs the controller in the dashboard; exiting stops the run |
| `LOOPER_RATE_LIMIT_MAX` | `12` | Maximum agent invocations for a transient failure; also the quota-window pause budget, in runs and the startup bootstrap wait |
| `LOOPER_RATE_LIMIT_DELAY` | `60` | Initial retry delay, in seconds; also the quota pause when the provider reports no reset time |
| `LOOPER_GIT_INIT` | `1` | Create a Git repository for a project without one; `0` refuses |
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

Run `looper help <command>` for the exit codes of one command.
