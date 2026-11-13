# Agents and models

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
explicit live check that consumes usage. See [agent profiles](AGENT-PROFILES.md)
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

## Use zcode

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

## Agent permissions

Worker agents run with permission checks disabled. Claude Code receives
`--dangerously-skip-permissions`. Codex receives
`--dangerously-bypass-approvals-and-sandbox`. Set `CODEX_YOLO=0` to give
Codex a workspace-write sandbox instead. zcode workers run in its
full-access `yolo` mode. Run Looper only in repositories whose contents you
trust.
