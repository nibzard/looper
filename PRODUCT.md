# Looper product context

## Positioning

For a developer with written requirements and a backlog of specified
tasks, Looper is a loop controller that runs on the machine that holds
the repository: a laptop, a server, or a headless VM. It drives the
coding agents you already have, unmodified. Each task runs in a private
worktree with a fresh context. Looper checks the combined result and
publishes only what passes, then waits for the next edit.

Looper adds no cloud service of its own. It competes with nothing the
agent CLIs do. When the agents improve, the loop improves.

Best fit: greenfield projects and long backlogs, unattended. Observe
from any terminal, phone, or coding agent. Publication lands in the
working tree; pull requests stay the project's own choice.

## Register

Product. Looper is a terminal tool for developers who run coding agents.

## Purpose and users

Developers monitor tasks, inspect checks, and resume retained work from local or remote terminals.
Phone terminals need readable status and visible keyboard controls.

## Design principles

- Show the current operation and distinguish it from task execution.
- Use concise labels and familiar keyboard controls.
- Keep the existing terminal palette and monospace layout.
- Use text labels with color so color is never the only status indicator.
- Wrap content to the terminal width and preserve access to logs.
- Avoid decorative animation and web interface patterns.

These principles follow README.md and docs/UI-PLAN.md.
