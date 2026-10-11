# Looper against current practices: comparison report

Research date: 2026-10-11. This report compares the Go controller at v0.6.1
with the local coding-agent orchestrator ecosystem and with the native
capabilities of Claude Code, Codex, and GitHub Copilot. It also revisits the
2026-09-23 report in `LOOPER-RESEARCH-REPORT.md`.

Method: five parallel research streams, all conducted on 2026-10-11:

- A code map of this repository (module structure, engine loop, integration,
  recovery, settings surface, tests, complexity hotspots).
- A survey of local parallel coding-agent orchestrators, verified against each
  project's own repository and documentation.
- The current Claude Code documentation and changelog.
- The current Codex documentation, releases, and OpenAI guidance pages.
- The canonical sources behind the simplicity principles used in
  `MANIFESTO.md`.

Evidence tiers:

- **Observed**: read in this repository's code or tests.
- **Vendor claim**: stated in vendor documentation or a release note.
- **Third-party**: stated by another project or publication.
- **Inference**: this report's own reasoning, flagged as such.

## 1. Summary

**Looper's ingredients are all present in the October 2026 ecosystem. Its
full combination is present nowhere else.** Worktree isolation, file-based
task backlogs, verification gates, cross-vendor review, and terminal
dashboards are each mainstream. No surveyed tool combines planning, parallel
worktree workers, deterministic integrated checking, working-tree
publication, and a continuous idle loop. That combination is Looper's
defensible ground.

Where Looper fits current practice:

- Worktree-per-task isolation is the standard answer to parallel execution.
  An April 2026 survey of nine open source orchestrators found they "all
  solve parallel execution through git worktrees" (third-party).
- A verification gate before completion matches vendor guidance exactly.
  Claude Code best practices: without a check, "you become the verification
  loop" (vendor claim).
- Cross-vendor implement-and-review is now an official OpenAI pattern
  (`codex-plugin-cc`, about 34,000 stars). Looper generalizes it.
- Schema-enforced agent output, recommended by the September report, is
  implemented for both agents (`internal/looper/agent.go:100-130`, observed).

Where Looper leads the ecosystem:

- Deterministic integrated checking of combined results is rare. Only
  gastown's Refinery merge queue and YYLO's validation boundaries compare.
  Most tools gate on human diff review plus CI.
- A continuous idle loop that watches the task file for human edits has no
  verified match in any surveyed tool. Native scheduling stays session-scoped
  (Claude Code) or desktop-app-managed (Codex).

Where Looper exceeds its own needs:

- The recovery, settings, and import machinery goes beyond the ecosystem
  norm. The code map found dead generality: an attempt phase that is compared
  in nine places but never assigned, import fields that are written but
  never read, a flag-replay layer, and legacy aliases carried from the Bash
  era. Section 7 lists them.
- The strategic risk is absorption, not competition. Native runtimes now
  ship managed worktrees, parallel subagents, batch fan-out, and dynamic
  workflows. The features Looper must not duplicate are the ones the vendors
  will own within a release or two.

The September report recommended a thin Bash controller. The rewrite that
followed resolved that report's practical concerns (untestable state, no
parallelism, a stalled migration) but went far past "thin" in presentation
and lifecycle machinery. The unrun comparison pilot in
`EXPERIMENT-PLAN.md` remains the missing evidence for the value claim.

## 2. The ecosystem in October 2026

All rows verified against each project's own repository on 2026-10-11.

| Tool | Focus | Isolation | Combine results | State | UI |
|---|---|---|---|---|---|
| claude-squad (8,590 stars) | Manage many agent sessions | Git workspace per task, tmux | Manual: push branch or check out into main workspace | Config plus tmux sessions | Go TUI |
| vibe-kanban (28,327 stars) | Kanban over agent work | Branch plus workspace per issue | Pull request per task; manual conflict repair | SQLite | Local web |
| Conductor (closed source) | Mac desktop parallel agents | Workspace per task | Pull request per task; checks ride on GitHub CI | App storage | Desktop |
| Nimbalyst (ex-Crystal, 1,866 stars) | Parallel sessions, opt-in worktrees | Worktree per session, one click | Not documented; diffs to approve | App storage | Desktop |
| cook (371 stars) | Composable operators over agents | Docker sandbox per run | `pick` / `merge` / `compare` resolvers | Files | CLI |
| Taskmaster (28,199 stars) | PRD to dependency-ordered tasks | None | Sequential, one agent | Files in `.taskmaster/` | CLI, MCP |
| Backlog.md (6,999 stars) | Markdown task kanban | None | "One task = one context window = one PR" | Markdown files | CLI, kanban, web |
| spec-kit (140,806 stars) | Specify, plan, tasks, implement | None | None; single-session process | Spec files | Agent chat |
| agent farm (921 stars) | 20 to 50 agents on one checkout | File locks on a shared tree | Commits into the shared checkout | Files, HTML reports | tmux |
| gastown (18,325 stars) | Coordinator plus worker agents | Worktree-backed storage | Refinery merge queue: batch, verify the merged stack, bisect failures | Git-backed issues | TUI |
| ordewell (197 stars) | Plan, run, integrate | Branch per task | Land on one integration branch in plan order | Files | CLI |
| amux (162 stars) | Worktree workspaces | Worktree per agent, tmux | Local `git merge --no-ff`; never pushes | Files | Go TUI |
| sortie (198 stars) | Tracker tickets to sessions | Worktrees | Not documented | SQLite | Go TUI |

Also observed: dmux (1,798 stars), parallel-code (1,042 stars), YYLO
(typed validation before merges). OpenHands (90,586 stars) is a self-hosted
platform with container-per-conversation isolation, not a local worktree
orchestrator. Manifest, named in earlier surveys, is unreachable and appears
dead.

Common patterns, in order of how mainstream they are:

- **Mainstream**: worktree or branch per task; a board or kanban over agent
  sessions; human diff review as the gate; pull request per task; one
  sequential loop until the task list is done; task state as files, usually
  Markdown.
- **Present but less common**: best-of-N races (cook); Docker isolation
  (cook, OpenHands); SQLite state (vibe-kanban, sortie); supervisor daemons
  (gastown).
- **Rare**: merge queues with deterministic verification; idle daemons;
  direct working-tree writes at scale.

Churn is severe. vibe-kanban's company shut down in April 2026 after
concluding that "the vast majority are free users and we couldn't find a
business model". cook, Crystal, Taskmaster, and claude-squad have slowed or
stalled. The community catalog keeps a "Resting" watchlist of stalled
projects. Native runtimes absorbed several of their functions.

## 3. Native platform capabilities

### Claude Code (v2.1.296)

- **Agent teams** remain experimental and disabled by default. They use a
  shared task list and mailbox files, but "Agent teams don't isolate
  teammates in worktrees" and "Spawning teammates also requires an
  interactive session" (vendor claims). Teams do not run headless and cannot
  resume in-process teammates.
- **Parallel machinery that does run headless**: subagents with optional
  worktree isolation and background mode; background sessions under a
  supervisor daemon that moves dispatched sessions into isolated worktrees;
  `/batch`, which splits one change across 5 to 30 subagents, each in its own
  worktree; and dynamic workflows, scriptable fan-out with resumable runs and
  per-agent output schemas, available in `claude -p` and the Agent SDK
  (vendor claims).
- **Worktrees are enforced, not advisory**: `claude --worktree`, the
  `EnterWorktree` tool, `.worktreeinclude` for ignored files, and cleanup
  sweeps. Headless runs leave worktrees locked on disk (vendor claim).
- **Headless**: `--json-schema` returns validated structured output;
  `--permission-mode` and `--permission-prompts none` support unattended
  runs; `--bare` is "the recommended mode for scripted and SDK calls" and
  will become the default for `-p` (vendor claims).
- **Scheduling stays session-scoped**: `/loop` and cron tasks "only fire
  while Claude Code is running and idle" and expire after seven days (vendor
  claim).
- **The acknowledged gap matches Looper's core**: the best-practices fan-out
  recipe loops `claude -p` over a task list and leaves the driver loop to
  "your own script" (vendor claim).

### Codex (CLI 0.162.1)

- Subagents are official and on by default, with custom agent definitions.
  Release 0.162.0 added managed git worktree tools and cloud thread resume
  (vendor claims).
- `codex exec` runs read-only by default; `--sandbox workspace-write`,
  `--output-schema`, and `resume` support scripted runs (vendor claims).
- Long-horizon guidance recommends durable files: a frozen spec, a plan with
  milestones and acceptance criteria, a runbook, and a status file, plus
  worktrees to isolate runs. "Run chats concurrently, but avoid letting two
  chats change the same files" (vendor claims). This is Looper's model.
- Scheduled tasks are managed from the desktop app; web tasks cannot touch
  local folders; the CLI provides no scheduler, queue, merge, or retry policy
  for many parallel local runs (vendor claims and documented absence).

### GitHub Copilot cloud agent

Renamed in April 2026. It plans, codes, and opens pull requests in
GitHub-hosted ephemeral environments. It cannot touch local machines. Local
work hands off to VS Code or Copilot CLI (vendor claims).

### Cross-vendor

OpenAI maintains `codex-plugin-cc`, which runs Codex review inside Claude
Code and can gate Claude's Stop hook on Codex findings. No Anthropic
equivalent exists in the other direction (observed absence).

### Capability comparison

| Capability | Native surface | Looper | Verdict |
|---|---|---|---|
| Parallel workers, headless | Subagents, `/batch`, workflows, Codex subagents | Controller-owned worktrees and processes | Looper duplicates the isolation, adds scheduling policy |
| Isolation | Enforced worktrees, managed worktree tools | `git worktree add --detach` plus private resources | Native enforcement is newer; Looper needs controller ownership for integration |
| Task state | Session-scoped plans; durable file guidance | SQLite plus an editable `to-do.json` projection | Looper adds cross-run, cross-vendor state |
| Completion gate | Stop hooks, `/goal`, acceptance criteria in prompts | Seven-condition acceptance invariant with executable checks on the combined tree | Looper adds; strongest differentiator |
| Local scheduling | Session-scoped loops; desktop-app tasks | Daemon, drain, continuous idle loop | Looper adds |
| Cross-vendor | Official one-direction plugin | Both agents as first-class workers and reviewers | Looper adds |
| Permissions | `--permission-mode`, `--allowedTools`, sandboxes | Blanket skip flags for workers | Looper is broader than vendor guidance (see section 6) |

## 4. Where Looper fits

1. **Worktree-per-task** is the ecosystem-standard answer to parallel
   execution, and Looper's implementation is stricter than most: detached
   worktrees, per-process resource prefixes, private `TMPDIR`, and an
   artifact directory (`README.md`, observed).
2. **File-based backlog** matches the norm. Taskmaster, Backlog.md, cook, and
   spec-kit all keep task state in files. OpenAI's long-horizon guidance
   prescribes durable files for plan and status (vendor claim). Looper
   diverges in format: JSON with a schema instead of Markdown. The rationale
   is deterministic validation and unknown-field rejection. That rationale
   holds, but Markdown is what humans and agents in this ecosystem edit most
   easily.
3. **Verification before done** matches vendor guidance and exceeds the
   ecosystem norm. Anthropic: give the agent "a check it can run... It's the
   difference between a session you watch and one you walk away from" (vendor
   claim). Looper checks the combined tree, not just each worker's branch,
   and refuses publication when the checked base is stale.
4. **Read-only planning** matches the emerging pattern. ordewell advertises
   "a planner that cannot write". Looper already restricts the planner:
   Codex gets a read-only sandbox; Claude gets Read, Glob, and Grep only
   (`internal/looper/agent.go:107-125`, observed).
5. **Cross-vendor review** is now official on one side. Looper's
   `--review-agent` generalizes `codex-plugin-cc` in both directions.
6. **Schema-enforced output** is implemented for both agents, with a tolerant
   fallback only for recovery (`internal/looper/agent.go:100-130,192`,
   observed). This closes the main prompt-audit finding of the September
   report.
7. **Terminal dashboard and detached controller** fit the TUI tier
   (claude-squad, amux, sortie). The mobile-terminal work is unusual and
   grounded in `PRODUCT.md`.

## 5. Where Looper diverges

1. **Deterministic integrated checking.** Looper runs setup and verification
   on a candidate that merges the worker's commit with the integration tip,
   rejects checks that mutate the tree, and publishes only when the checked
   base is still current (`docs/ARCHITECTURE.md`, observed). Only gastown's
   Refinery does comparable work: batch verification on the merged stack with
   bisection of failures. This is the feature that answers Anthropic's "you
   become the verification loop" warning, and it should survive every
   simplification pass.
2. **The continuous idle loop.** No surveyed tool idles while waiting for
   human edits to a task file. Native loops end or stay session-scoped.
   Looper treats the controller as a long-lived part of the project, like a
   build daemon. This is a product bet, not an ecosystem convention; it is
   defensible exactly because nothing native covers it.
3. **Working-tree publication without pull requests.** The TUI tier does
   this (amux, ordewell, claude-squad's checkout); the GUI products default
   to pull requests. Looper's choice matches its product statement: pull
   requests and remote services belong to the project, not to the tool.
4. **Recovery depth.** The publication journal, worker receipts, export
   boundaries, and report recovery go beyond anything surveyed. This depth is
   Looper's answer to a real problem (a restart must not mark unchecked work
   done), and the test suite exercises it. It is also the highest-cost
   subsystem to maintain. The September report's maintenance warning applies
   here more than anywhere else.

## 6. Lessons

1. **Absorption is the strategic threat.** Native runtimes now ship the
   orchestration conveniences that killed vibe-kanban: worktrees, batch
   fan-out, background sessions, subagents. Looper's survival depends on
   doing the things that need a deterministic, vendor-neutral controller:
   the acceptance invariant, the integrated check, the idle loop, and
   cross-vendor dispatch. Every feature outside that set should justify
   itself against "will the vendor ship this?".
2. **Decide the permission model deliberately.** Workers run with blanket
   skip flags (`--dangerously-skip-permissions`,
   `--dangerously-bypass-approvals-and-sandbox`, observed). Claude Code now
   offers `--permission-mode` and `--allowedTools`; Codex offers sandbox
   modes. Looper's design treats the worktree as the boundary and says so
   ("workspace isolation, not a security sandbox"). Consider offering a
   scoped-permission mode, and document the trust assumption in the README.
3. **Decide the `--bare` question.** Anthropic recommends `--bare` for
   scripted Claude calls, but it skips project instructions such as
   `CLAUDE.md`. Looper's workers should keep project context, so the current
   choice is defensible. Record that reasoning where the command is built,
   because `--bare` will become the `-p` default.
4. **Markdown is the ecosystem's task format.** JSON plus a schema gives
   validation; Markdown gives easy human and agent editing. If humans edit
   tasks often in practice, a Markdown projection is worth considering. Low
   priority; do not churn the format for fashion.
5. **Learn from gastown's queue**: verification gates on the merged stack
   and bisection of failures. Looper already recombines obsolete candidates;
   bisection is a refinement, not a gap.
6. **Learn from the graveyard**: vibe-kanban died when the harnesses
   absorbed its board; cook and Crystal stalled. The pattern says: stay
   small, stay local, stay vendor-neutral, and delete features that track
   vendor churn. This is also the core of `MANIFESTO.md`.
7. **Run the pilot.** `EXPERIMENT-PLAN.md` defines the comparison and the
   decision rule. Until it runs, the claim "Looper beats native workflows on
   recovery and blocked-task handling" is unmeasured. A reduced pilot
   (two arms, two cases, two repeats) would already move the claim from
   provisional to evidenced.

## 7. Complexity audit

Observed in the code map on 2026-10-11. These are the KISS findings: places
where the implementation carries more than it uses.

| Item | Location | Finding | Suggested action |
|---|---|---|---|
| `abandoned` attempt phase | `run.go:269,331`; `engine.go:213,303,597`; `imports.go:328`; `view.go:50,68`; `observe.go:49` | Compared in nine places, never assigned | Normalize legacy phase values at load, then remove the comparisons |
| `ImportRecord.Evidence`, `AcceptedRevision`, `BacklogDigest` | `types.go:381-383`; `imports.go:375-377` | Receipt fields written, never read; the drift check itself exists inline (`imports.go:355,365`) | Drop the fields or read them in replay and doctor output |
| `Backlog.Project`, `Task.Steps` | `types.go:21,32` | Accepted by the schema, never read by the engine | Remove from the schema or use them |
| Flag replay layer | `config.go:91-113,349-379` | Exists only to preserve last-flag-wins ordering | Replace with a simple map |
| Double check limiting | `engine.go:40,468-479`; `run.go:159` | Semaphore plus a length check bound the same thing | Keep the semaphore, delete the length check |
| Legacy aliases | `--smart`, `--interleave`, `--all`, `--ls` | Bash-era compatibility, documented as aliases | Time-box to a removal release |
| Live-settings protocol | `settings.go:281-703` | 423 lines of request, claim, validate, ack files | Consider: controller reads the profile at loop boundaries and reports outcomes as events; keep ack files only if concurrent editors are a real case |
| Legacy migration | `migration.go` (120 lines) | One-shot import of the Python controller's state | Time-box to a removal release |
| Measuretwice adapter | `adapters/measuretwice` | Optional, advisory, unvalidated | Freeze until the pilot runs; cut if the core claim stands without it |
| Toolchain pin | `go.mod` | go1.22 pins huh v0.6.0 and bubbletea v1.1.0; newer Charm releases need go1.23 | Either plan the toolchain bump or freeze UI feature work |

None of these break behavior. All of them add surface that a future
maintainer must understand. The manifesto's rule applies: remove them or
wire them.

Actions on 2026-10-11: row one is removed (the dead phase, with the phase
set documented on the `Attempt` type); row two is removed (the unread
receipt fields; the refreshed-baseline regression test keeps its assertion
on the state tip); row four is replaced by inline replay functions with
identical ordering semantics. Row five is not a duplicate: the semaphore
bounds concurrent shell commands across workers and checks together, while
the chain bound limits speculative checks. A comment now records that at
`Engine.command`. Row three stays, because `steps` and `project` are
documented task-file fields. Rows six and later remain open decisions.

The think-heavy surface, measured against Krug's rule (see `MANIFESTO.md`):

- Good: one default command; a three-question first-run form; plain output
  when piped; color never the only signal; case-insensitive letter keys; a
  doctor command with fix hints.
- Heavy: 25 flags, about 27 environment variables, and 14 subcommands; four
  settings states (saved, applied, overridden, restart); four import states;
  `Q` semantics that depend on mode. Documentation covers all of it, but
  each state a user must hold in mind is a cost. The lever is fewer states,
  not more help text.

## 8. The September question revisited

The 2026-09-23 report recommended a thin Bash controller and cancelling the
Go rewrite. What happened instead: the Go controller shipped as v0.5.0 and
v0.6.x, with 42 integration cases, race-detector runs, and a live test.

What the new evidence says:

- The report's practical concerns were real and are now resolved. The Bash
  runner could not test its state machine, could not parallelize, and its
  migration had stalled. The rewrite addressed exactly those points
  (observed).
- The report's prompt and output findings were adopted. Schema-enforced
  output is implemented for both agents; the prompts are schema-bound and
  outcome-focused (observed).
- The report's core argument survived: the deterministic shell is the value.
  October's ecosystem confirms that the gaps the report identified (local
  scheduling, deterministic completion, cross-vendor dispatch) remain open in
  the native tools (vendor claims, documented absences).
- The report's warning also survived: maintenance cost. The Go controller
  now carries a settings protocol, an import subsystem, a TUI, and the
  legacy surface listed in section 7. None of the surveyed tools carries
  this much machinery around their core loop, and the category's failure
  mode is exactly this weight.

Verdict (inference): the controller core is validated by current practice
and by the native gaps. The surface area is where the KISS discipline
applies. The September report was right about what to keep and wrong only
about how much shell the kept parts need; the answer to "how much" is now
testable by running the pilot.

## 9. Recommendations

Ordered by cost. Items 1 to 3 are cheap and safe.

1. Remove the dead generality in section 7, rows one to five. Each is a
   small, test-covered deletion.
2. Record the `--bare` and permission reasoning next to the command
   construction in `internal/looper/agent.go`.
3. Set removal dates for the legacy aliases and the Python migration path.
4. Decide the live-settings protocol's future: simplify to
   profile-read-at-boundary with event reporting, or keep the full
   request-and-ack protocol for concurrent editors. Do not keep it by
   default inertia.
5. Run a reduced pilot from `EXPERIMENT-PLAN.md`: Looper versus one native
   arm, two cases, two repeats. Record results next to the plan.
6. Freeze optional layers (Measuretwice) until the pilot runs.
7. Protect the differentiators from feature creep: integrated checking, the
   idle loop, working-tree publication, cross-vendor dispatch. When a new
   feature proposal lands on one of these, it must simplify them, not
   extend them.

Stop condition: if the pilot shows a native arm matching Looper on recovery
and blocked-task handling at equal or lower cost, stop adding surface and
shrink toward the differentiators, or archive with a pointer to the winning
workflow.

## 10. Sources

Accessed 2026-10-11 unless noted. Vendor pages that print no date are cited
by version number where possible.

Ecosystem: smtg-ai/claude-squad; BloopAI/vibe-kanban and its shutdown note;
conductor.build; stravu/crystal; nimbalyst/nimbalyst; rjcorwin/cook;
eyaltoledano/claude-task-master; MrLesk/Backlog.md; github/spec-kit;
Dicklesworthstone/claude_code_agent_farm; gastownhall/gastown;
ordewell/ordewell; andyrewlee/amux; standardagents/dmux;
johannesjo/parallel-code; yylo-dev/yylo; sortie-ai/sortie;
andyrewlee/awesome-agent-orchestrators. Surveys: Augment Code (April 2026),
Tembo.io (June 2026), Codacy (September 2026).

Claude Code: agent-teams, agents, sub-agents, agent-view, worktrees,
headless, hooks, checkpointing, scheduled-tasks, workflows,
agent-sdk/overview, best-practices, and /batch documentation at
code.claude.com; the changelog at github.com/anthropics/claude-code;
"A harness for every task: dynamic workflows in Claude Code" (June 2026) at
claude.dev; "How we built our multi-agent research system" (June 2025) at
anthropic.com/engineering.

Codex and OpenAI: learn.chatgpt.com non-interactive mode, subagents, slash
commands, automations, long-running work, and enterprise cloud-local access
pages; openai/codex releases; "Run long horizon tasks with Codex",
"Rethinking skills and prompts for GPT-6 Astra", and "Shell + Skills +
Compaction" at developers.openai.com; DevDay 2026 recap; openai/codex and
chatgpt.com/codex pricing; openai/codex-plugin-cc.

GitHub: Copilot cloud agent documentation and changelog entries at
github.blog and docs.github.com.

Simplicity sources for the manifesto: dreamsongs.com (Gabriel);
Simple Made Easy (Hickey, 2011); Cunningham and Beck via c2.com and Artima;
Krug, "Don't Make Me Think"; clig.dev; McIlroy in Salus, "A Quarter Century
of UNIX"; Pike, "Notes on Programming in C"; PEP 20; Brooks, "The
Mythical Man-Month"; Boyd, "A Discourse on Winning and Losing"; Vitsœ on
Rams; Sutton, "The Bitter Lesson"; the butter-passing robot, Rick and Morty
season 1, episode 9 (2014).

This repository: `README.md`, `docs/ARCHITECTURE.md`, `docs/UI-PLAN.md`,
`PRODUCT.md`, `AUTONOMOUS-AGENTS.md`, `EXPERIMENT-PLAN.md`,
`docs/research/LOOPER-RESEARCH-REPORT.md`, `docs/VALIDATION.md`, and the
source under `cmd/`, `internal/`, and `adapters/`.
