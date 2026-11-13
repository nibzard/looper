# Agent profiles

An agent profile names an executable and the interface Looper uses with it.
The executable selects authentication and the provider. The adapter handles
prompt transport, command arguments, planning restrictions, and result decoding.

For example, `claude` and `claude-zai` use the Claude Code adapter. They can
select different providers and models. A shell alias such as `zolo` is not an
executable; configure its underlying command instead.

## Configure agents

1. Run `looper config` in the project.
2. Select **Add an agent command** or **Edit an agent command**.
3. Enter a profile name and an executable name or absolute path. Select the
   interface the command implements. A wrapper uses its underlying interface.
4. Set a model and effort, or leave them empty to use the agent's settings.
5. Continue to project settings. Choose the worker, reviewer, and session count.
6. Review the executable paths, models, and permission behavior. Confirm saving.

Known commands are available as starting choices. Installed `claude-zai`
wrappers are discovered separately from plain `claude`. Custom commands require
an explicit adapter choice. Looper never runs a command through a shell to
interpret configuration. Use a wrapper if an executable needs extra arguments.

Authentication stays in the agent's login or wrapper. Agent profiles do not
store credentials or configure login. Saving does not contact a model provider.
Cancelling before confirmation saves neither scope.

The editor can also set defaults for new projects. Existing project choices
remain authoritative. Each role can select a different profile of the same
adapter, with independent model and effort values. The reviewer profile also
handles planning, diagnosis, and product review.

## Configuration locations

Machine commands and defaults live at:

```text
$XDG_CONFIG_HOME/looper/agents.json
```

Without `XDG_CONFIG_HOME`, the path is `~/.config/looper/agents.json`.
`LOOPER_AGENTS_FILE` selects a different file, for example in automation.
Project role choices remain in the existing `runtime/profile.json` under the
project's Looper state directory. Configuration files use mode `0600`.

Here is a machine catalog for this host:

```json
{
  "version": 1,
  "agents": {
    "anthropic": {
      "adapter": "claude",
      "command": "claude"
    },
    "glm": {
      "adapter": "claude",
      "command": "claude-zai"
    },
    "codex": {
      "adapter": "codex",
      "command": "codex"
    }
  },
  "worker": "glm",
  "reviewer": "anthropic"
}
```

Each definition also accepts optional `model` and `effort` strings when its
adapter supports them. Do not place provider tokens in this file.

Select profiles for one invocation:

```sh
looper --agent glm --review-agent anthropic
```

Or save those names through `looper config`. Project profiles use the existing
`agent` and `review_agent` fields, so no new project file is necessary.

## Defaults, overrides, and migration

Role selection uses this order, highest priority first:

1. Explicit command flags.
2. Existing environment overrides.
3. Saved project choices.
4. Machine defaults.
5. Legacy defaults for existing backlogs, or explicitly accepted defaults.

The built-in profile names `claude`, `codex`, and `zcode` remain available.
Existing saved profiles and `CLAUDE_BIN`, `CODEX_BIN`, and `ZCODE_BIN` continue
to work. These executable variables override only their corresponding built-in
profile names; they do not redirect a separately named `glm` profile.

Old project model fields and model environment variables remain overrides.
`--model` still sets both Codex and Claude values for compatibility. Prefer
model settings on named profiles when roles need different models.
`CLAUDE_MODEL` and `CODEX_MODEL` retain their adapter-specific meanings.
An explicit empty environment or flag value selects the agent's model default.
The editor exposes existing shared overrides so you can clear them.

A controller snapshots machine definitions when it starts. Editing an executable,
model, or effort in the catalog does not change an active controller. Restart it
to load those changes. Project role edits can apply at the next dispatch boundary
when the selected profiles already exist in that controller's snapshot. New
profiles apply after restart. In-flight operations keep their starting settings.
Concurrent machine editors detect changed files and refuse to overwrite them.

## First use and scripts

Interactive `looper`, `looper run`, `looper start`, and an unattached `looper tui`
open setup when a project has no saved profile. The requested operation continues
after setup. Explicit flags prefill setup and retain their precedence for that
invocation. `--yes` explicitly accepts defaults and skips the form.

Help, version, status, logs, stop, and listing commands never open setup.
Unknown commands fail before project initialization. Preflight remains a static
inspection of a potential run.

Without an interactive terminal, a new project must provide an explicit agent,
a saved project choice, or a machine worker default. Otherwise Looper returns
`configuration_required` with instructions for fixing it. It never opens a form
in a pipe. An existing backlog retains the legacy default until configured.
`--yes` is an explicit request to accept defaults, including the default agent.

## Test a command

Run a live check only when you want to spend agent usage:

```sh
looper config --test-agent glm
```

This creates a private fixture directory and requests a structured result.
For Claude Code and Codex, it also checks that the event stream contains a tool
call. Zcode currently reports that tool-call verification is unavailable.
The test preserves private invocation and event evidence outside project state.
No test runs automatically during setup or preflight. Add `--contract v2 --json`
to receive the result defined by `contracts/schemas/v2/agent-test.schema.json`.

Invocation records include the profile name, adapter, adapter version, resolved
executable path, and requested model and effort. The provider's reported model
remains available in its raw agent log; Looper does not infer authentication
from an executable name.

## Adapter capabilities

| Adapter | Model override | Effort override | Structured result | Planning restrictions |
| --- | --- | --- | --- | --- |
| Claude Code (`claude`) | Yes | Yes | Schema | Read, Glob, and Grep; external tools disabled |
| Codex (`codex`) | Yes | Yes | Schema | Read-only sandbox |
| Zcode (`zcode`) | No | No | Validated final result | Inspection-only prompt with full tool access |

Planning uses the restrictions in the table. Review and diagnosis use worker
permissions and their task instructions.

Looper workers explicitly bypass permission checks for Claude Code and use
Zcode's full-access mode. Codex workers bypass approvals and the sandbox unless
`CODEX_YOLO=0` selects workspace-write. The configuration review states this
behavior. Your agent's normal interactive permission settings do not override
Looper's explicit invocation arguments.

## Add an adapter

A new protocol implements `agentProtocol` in `internal/looper/agent_adapters.go`:
argument construction, prompt transport, result envelope selection, and final
result extraction, and permission behavior. Register its capabilities in
`agentAdapters`; the form lists registered adapters automatically. Add protocol
fixtures and invocation tests. Add activity
and usage decoders when that protocol supplies them. Keep command definitions
and project roles independent of protocol details.

Pi is a future adapter candidate. Registering an executable named `pi` does not
make it compatible. Verify its actual headless input, tool, and result interface
before implementing that adapter. Existing profiles and role selection need no
new project configuration mechanism when the adapter is added.
