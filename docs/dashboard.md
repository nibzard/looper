# Dashboard

Start the terminal user interface (TUI) with `looper tui`. If no run is
active, it starts a detached controller, like `looper start`, and then
attaches to it. Otherwise it attaches to the active run. The overview shows the goal,
controller activity, snapshot age, progress, attention items, and active
workers. Recent outcomes stay visible even when worker events fill the log.
Integration counts cover the current session, the last hour, and the last
24 hours. Looper reads these counts from the journal, so they survive the
viewer's event buffer limit.

Because the controller runs in the background, you can close the dashboard
and reconnect later with `looper tui`. Press `q` to open the exit choices:

- **[d] Drain, then detach**: finish current work, then stop the controller.
- **[c] Interrupt; retain work**: stop at once, like `looper stop --interrupt`.
- **[q] Detach; work continues**: close the dashboard and keep the run going.
- **[esc] Stay**: return to the dashboard.

A read-only observer (`looper attach`) detaches at once. To keep the
controller inside the dashboard process, start with `looper tui --foreground`.
Then the exit choices are drain and interrupt, because the run cannot
continue without the dashboard. A missing snapshot shows `UNKNOWN` and
retains the last known state. An attached dashboard reconnects when a replacement
controller appears. Task details refresh with the new session. Actions wait until
the dashboard displays that session. Quiet worker output does not prove a failure.

Add `--no-animation` to any dashboard start to disable animated motion. The
bootstrap activity marker stays a fixed bullet instead of a spinner, and
every status line, key, and refresh keeps its behavior. The option changes
presentation only; `--json` output and normal execution are unchanged.

![The dashboard: run state and progress in the header, working and blocked
tasks, recent events](assets/tui.png)

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
See [the pricing notes](../internal/looper/prices/README.md) for supported fields.


## Key reference

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
| `q` | Open the exit choices: drain, interrupt, or detach. An observer detaches at once. |
| `ctrl+c` | Detach from a background controller. Under `--foreground`, interrupt and retain work. |
| `?` | Show help. |

Scrolling activity pauses the displayed text while new events arrive.
The view reports the new event count. End resumes live activity.
This pauses the viewer only; the controller continues to work.

Requeue requests apply only while Looper is idle. Resolve the blocker first,
then press `r` in its inspector and confirm with `y`. The controller rejects
changed tasks and requests from earlier sessions. Retained work stays
available for reuse. A read-only observer cannot requeue tasks.

There is no command to pause execution. A drain waits for current planning
or agent work to finish. To stop immediately, choose **Interrupt; retain
work** in the exit dialog. Restarting retains task progress, but unfinished
planning runs again.


## Planning view and prompts

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


## Small screens

In mobile terminals and tmux panes, the overview uses a compact summary.
Task details remain available through the inspector. Use `j` and `k`,
arrow keys, or Page Up and Page Down to navigate. The header and controls
stay visible when the keyboard reduces the pane height. The footer shows
common controls. Press `?` for settings, plan imports, drain, and all keys.
The overview shows only the current run's outcomes. Information
uses neutral text; green marks successful results, yellow marks waiting or
blocked work, and red marks errors.
