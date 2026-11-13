# Looper working notes

This file holds the current working notes for the Looper repository:
diagnosis findings, decisions, and follow-ups. Read it at the start of a
session. Record new notes under the matching section below. Do not let it
grow into a history log again.

Compressed on 2026-11-13. Before that, the file held 9,829 lines of dated
history. Everything older than 2026-11-13 was cut to the archive index at
the end of this file. The full text is available in three places:

- `/notes/NOTES.md.full-2026-11-13.md` — a complete copy.
- Commit `c27b369b` on `refs/looper/run-da156c1fa2a7e8c0` — the same
  content minus the last 2026-11-13 proposal section.
- `git log -S"<text>" --all -- NOTES.md` — search across the history.

## Current state, 2026-11-13

- A detached controller has run since 2026-11-09: session
  `run-b1b5da12326f7eb5`, run ref `refs/looper/run-da156c1fa2a7e8c0`,
  tip `c27b369b`. The tip is one commit ahead of `main` (`3e52b5e`).
  A tui has been attached since 2026-11-05.
- The working tree holds substantial uncommitted work beyond this file:
  about 99 modified files and 79 untracked paths. Reconcile it with the
  run ref at an owner boundary.
- The installed binary is `0.9.1-dev.12` from `3e52b5e`, built
  2026-11-04. It is older than this tree. Do not assume the binary
  matches the source.
- Backlog in `to-do.json`: 113 tasks done, 9 blocked, 1 todo.

## Open items

- `T117` (todo): controller assembly cannot transport a tracked schema
  at the control-file path.
- Blocked tasks, from `to-do.json`:
  - `T019` process identity checks on darwin; needs a macOS host.
  - `T037` and `T043` triage and operator decisions for disputed
    findings.
  - `T053` deploy the integrated build to the live controller.
  - `T054` identify the one-time race-suite failure from T052
    verification.
  - `T108` trim the fsync-bound suite; blocked on `T113`.
  - `T113` run setup builds GOCACHE from a relative
    LOOPER_RESOURCE_PREFIX.
  - `T120` authorize and run the fresh-agent live evaluation.
  - `T121` decide the release that flips the version 2 contract default.
- Provider follow-up, 2026-11-13: add per-role provider selection if one
  run needs GLM workers and Anthropic Claude review. See the provider
  section below.
- The agent configuration redesign at the end of this file is a
  proposal. It awaits an owner decision and is not approved work.

## Standing cautions

- Do not edit tracked repository files while the controller integrates
  work. A concurrent NOTES.md append stopped an integration on
  2026-11-05. The incident and the recovery procedure are recorded in
  `/notes/looper-selfrun-monitoring-2026-11-05.md` and in the full-history
  backup.
- Workers run on Claude, not Codex. The default changed in commit
  `468284a` (2026-11-04).
## Claude provider selection after subscription migration, 2026-11-13

Local inspection only. No agent adapter or runtime configuration was changed.
The working tree already contains substantial uncommitted work.

- `internal/looper/config.go` accepts `codex`, `claude`, and `zcode` as agent
  kinds. It defaults to `claude` and resolves one `ClaudeBin` from
  `CLAUDE_BIN`, defaulting to `claude`.
- On this host, plain `claude` now authenticates with Claude Pro. The separate
  `claude-zai` wrapper selects Z.ai GLM-5.3, a 1M context limit, 300k compaction,
  and disabled Artifact. Shell aliases are not executable agent kinds.
- Z.ai selection requires `CLAUDE_BIN=claude-zai looper --agent claude`.
  Use an absolute wrapper path for launch environments with a different PATH.
- Worker and review roles share `ClaudeBin` and `ClaudeModel`. They cannot
  select the Anthropic and Z.ai Claude entry points independently in one run.
  Pairing either Claude provider with Codex uses the existing separate agent
  kinds. A distinct provider profile per role would require an implementation
  change.
- `agent.go` explicitly passes `--dangerously-skip-permissions` for Claude,
  including planning. Planning limits tools to Read, Glob, and Grep and disables
  MCP and slash commands. Removing the shared global permission bypass does
  not change Looper's explicit invocation behavior.
- `CLAUDE_MODEL` controls Claude separately. `--model` sets both Claude and
  Codex model values; avoid it when their model IDs differ. With no Claude
  override, the selected entry point supplies the default model.
- `agent.go` sends Claude prompts through stdin and requests stream JSON plus
  an output schema. Wrapper argument forwarding supports that invocation shape.
  No live Looper worker was used to verify the Z.ai structured-output path.
- Invocation records retain agent kind `claude` for either provider and omit
  the selected executable. New failure summaries can include the binary path.
  Provider identity is therefore not uniformly visible across records.
- Installed `looper --version` returns `0.9.1-dev.12 (3e52b5e)`; its executable
  is dated November 4. Do not assume this source checkout matches that binary.
  `looper help preflight` and `looper help settings` unexpectedly entered the
  run path from `/home/agent`; both inspection processes were interrupted.
  No home backlog exists. The command mismatch was logged as a papercut.

Follow-up: add explicit provider selection per role if one run needs GLM
workers and Anthropic Claude review. Keep executable and provider identity in
invocation records. Existing controllers require separate inspection before
any provider migration; this audit did not restart them.

## Proposed agent configuration redesign, 2026-11-13

Discussion proposal only; not approved for implementation.

Replace agent-kind selection as the user-facing abstraction with named agent
profiles. A profile binds an executable to a supported adapter and optional
model/effort settings. `claude` and `claude-zai` can both use the Claude Code
adapter while remaining independently selectable for worker and review roles.
Keep authentication in each agent's existing login or wrapper.

Use `looper config` as the single editor for machine agent profiles and project
role assignments. Discover known commands, allow executable names or paths, and
request an adapter when a custom command's protocol is unknown. An arbitrary
binary is not compatible merely because it exists. Do not evaluate shell input.

On a new project, interactive commands that start agent work should enter the
same setup flow and then continue the requested operation. Help, version,
status, logs, stop, listing, and inspection commands must remain free of setup
side effects. Unknown commands must fail before any project initialization.
Non-interactive runs need complete explicit or saved settings; report missing
configuration with a structured error rather than prompting or choosing a
provider silently.

Show executable resolution, adapter, role, model source, and permission behavior
before saving. Static validation must not spend model usage. Offer a separate,
explicitly invoked structured-output/tool smoke test. Record resolved profiles
and adapter versions in each invocation. Active work keeps its starting
configuration; changes apply at a documented boundary.

Keep machine executable definitions separate from project preferences. Prefer
flags over project settings over machine defaults, with visible setting sources.
Migrate existing agent fields and `CLAUDE_BIN` without discarding saved state.
Initial implementation should support named profiles for existing adapters;
third-party adapter protocols can follow as separate work.

# Archive index

One line per removed section, in original order. Find the full
text in `/notes/NOTES.md.full-2026-11-13.md` or commit `c27b369b`
(`refs/looper/run-da156c1fa2a7e8c0`).

### Undated headings, all from the margin-run diagnosis, 2026-10-12
- Finding 1: Controller exits leave no record
- Finding 2: Findings can be identical twins
- Finding 3: Per-task timing is not queryable
- Not Looper defects
- Suggested order
### 2026-10
- Prompt transport work, 2026-10-12
- Run monitoring, 2026-10-12
- Diagnosis fixes, 2026-10-12
- Release preparation, 2026-10-12
- Review of monitoring actions, 2026-10-12
- Implementation and local release, 2026-10-12
- Notes action review, 2026-10-12, 19:27Z
- Commonstate integration error, 2026-10-12, 19:51Z
- Docker postgres accumulation, 2026-10-12, 20:25Z
- Barista bootstrap failure, 2026-10-13, 11:57 local
- Barista diagnosis, 2026-10-13, 11:21 to 11:24Z
- Test suite status on Darwin, 2026-10-13, 13:35Z
- Barista drain after sleep diagnosis, 2026-10-13, 13:08–13:10Z
- Commonstate check wedge, 2026-10-13, 06:35Z
- Release 0.8.3, 2026-10-14
- Commonstate follow-up control, 2026-10-14
- GitPickle bootstrap sources and skill scope, 2026-10-14
- Latest-release installer, 2026-10-14
- Unattended recovery implementation, 2026-10-15
- Release preparation, 2026-10-15
- GitPickle diagnosis, 2026-10-15, 05:09 UTC
- GitPickle backlog staging, 2026-10-15
- GitPickle follow-up diagnosis, 2026-10-15, 06:38 UTC
- GitPickle check, 2026-10-15, 08:22 UTC
- GitPickle diagnosis, 2026-10-15, 13:18 UTC
- GitPickle GitHub permission and retry, 2026-10-15
- GitPickle remaining work, 2026-10-15, 13:48 UTC
- GitPickle R2 credential handoff, 2026-10-15
- GitPickle review repair, 2026-10-15, 19:58 UTC
- Melt review recovery and clipboard, 2026-10-18
- Melt controller replacement and retained publication, 2026-10-18
- Melt clipboard through tmux and iPhone, 2026-10-18
- Worktree consolidation, 2026-10-23
- Pylemma bootstrap schema failure, 2026-10-27
- Slit Codex account lapse, 2026-10-30
### 2026-11
- Stackfall retained-state recovery, 2026-11-02
- Stackfall full-run session analysis, 2026-11-03
- Stackfall improvement plan validation, 2026-11-03
- Stackfall improvement plan implementation, 2026-11-03
- Improvement plan diff review, 2026-11-03
- Improvement review fixes, 2026-11-03
- Stackfall dashboard and scheduling correction, 2026-11-03
- Stackfall import scheduling and event fixes, 2026-11-03
- Commit and install, 2026-11-03
- Dev version stamping, 2026-11-03
- End-to-end bug review, 2026-11-03
- Backlog audit against the codebase, 2026-11-03
- Stackfall promise review, 2026-11-03
- Stackfall scheduling fixes, 2026-11-03
- Skill installation ownership, T023, 2026-11-03
- T024 retained merge failures, 2026-11-03
- Catalog pricing coverage, T027, 2026-11-03
- T026 plan-import crash recovery, 2026-11-03
- Negative persisted intervals, T030, 2026-11-03
- T025 completed runs with oversized logs, 2026-11-03
- T028 startup drain requests, 2026-11-03
- T031 unchanged verification during run recovery, 2026-11-04
- T032 nested modules in verification detection, 2026-11-04
- T033 settings on the first keypress, 2026-11-04
- Default coding agent changed to Claude, 2026-11-04
- Add tasks through the controller, 2026-11-04
- Claude-default test regression and tui recovery refusal, 2026-11-04
- Self-run restarted on Claude, 2026-11-04
- Symlinked assessment entry, 2026-11-04
- Acceptance pinning tests, T038, 2026-11-04
- Detached controller mode, T039, 2026-11-04
- Dashboard animation opt-out, T040, 2026-11-04
- Dashboard plan-import results, 2026-11-04 (T041)
- Add-task request clearing the live plan, 2026-11-04
- Root snapshots skip the runtime tree, 2026-11-04
- Invalid startup plans rejected before work, 2026-11-04
- Dispatch guards for a nil plan, 2026-11-04
- Bootstrap board clipped to short screens, 2026-11-04
- Import analysis activity evidence (T051), 2026-11-04
- Diagnosis consultation activity evidence (T055), 2026-11-04
- Self-run completion 2026-11-04
- Add-plan spaces, 2026-11-04 (T034 redo)
- T036 shell option and array guards, 2026-11-04
- Installer fixtures stay out of the real user prefix, 2026-11-04 (T056)
- Held backlog import at check boundaries, 2026-11-04 (T057)
- Delta review assessment, 2026-11-04
- Corrected delta fix plan, 2026-11-04
- Delta task batch prepared, 2026-11-04
- Nil-plan retained-work recovery, P2-001, 2026-11-04
- Claude result envelopes, P2-002, 2026-11-04
- Shared agent default in settings paths, P2-003, 2026-11-04
- Stackfall blocked-run diagnosis, 2026-11-04
- STACKFALL device feedback work, 2026-11-04
- STACKFALL scope correction and reproduced save stalls, 2026-11-04
- Looper retained-work diagnosis, 2026-11-05
- Retained P2-003 recovery, 2026-11-05
- Held-backlog stop precedence, T066, 2026-11-05
- Scoped stop requests, P2-004, 2026-11-05
- Merge failure classification, 2026-11-05 (P2-005)
- Shallow-clone development stamps, 2026-11-05 (P2-006)
- Dashboard control-character escaping, 2026-11-05
- Hold correction wait request spin, 2026-11-05
- Measuretwice scripted test description, 2026-11-05
- Import replay orphaned captures, 2026-11-05 (T069)
- Agent-to-CLI interaction audit, 2026-11-05
- Agent experience proposal, 2026-11-05
- Agent experience task submission, 2026-11-05
- Agent experience contracts, 2026-11-05 (AX-001)
- Findings durability staging, 2026-11-05 (AX-002)
- Capability advertisement, 2026-11-05 (AX-008)
- AX-003 consistent state and event cursors, 2026-11-05
- AX-014 actionable structured errors, 2026-11-05
- AX-006 bounded and filterable event queries, 2026-11-05
- AX-015 explain retained-work guards, 2026-11-05
- AX-004 bounded status and piped liveness, 2026-11-05
- AX-007 structured identities on lifecycle events, 2026-11-05
- AX-016 read-only recovery preview, 2026-11-06
- AX-005 bounded task inspection and paged history, 2026-11-06
- AX-018: semantic backlog comparison, 2026-11-06
- AX-021: failure summaries with bounded public evidence, 2026-11-06
- AX-023: execution preflight and the launch description, 2026-11-06
- AX-024: executable identity accurate during installs, 2026-11-06
- AX-025: saved-run discovery and searched-path reporting, 2026-11-06
- T100: legacy status document regains its frozen shape, 2026-11-06
- T098: byte-level fixtures for the legacy JSON commands, 2026-11-06
- T099: the interrupted suite must not orphan the import-crash helper, 2026-11-06
- T101: the compact inspection keeps its published byte budget, 2026-11-06
- T102: preflight budget holds for any configuration, 2026-11-06
- T103: the launch line reports resume command provenance, 2026-11-06
- T105: observations stop creating WAL sidecars on stopped runs, 2026-11-07
- T106: the import-boundary drain wait survives a loaded host, 2026-11-07
- T107: byte-level fixtures for the remaining legacy documents, 2026-11-07
- T108: trim the fsync-bound suite and size budgets to load, 2026-11-07
- T109: the remaining v2 documents keep their published byte budgets, 2026-11-07
- Host git configuration isolation for tests, 2026-11-07 (T110)
- T112: the plan-import acknowledgement's conflicts and error keys get a recorded variant, 2026-11-07
- T111: the goroutine-Fatal class stays fixed by a guard, 2026-11-07
- T114: digest-deduplicated replay event, 2026-11-07
- T020: request queue overflow recovery, 2026-11-07
- T044: pause ordinary dispatch after accepting a plan import, 2026-11-07
- T018: controller signal handling on the tui path, 2026-11-07
- T021: agent failure classification reads the process and stream errors only, 2026-11-08
- T046: keep the controller running across an owner commit mid-run, 2026-11-08
- AX-001 review repair: ownership map status refresh, 2026-11-08
- T047: quarantine one invalid task instead of failing startup, 2026-11-08
- T058: startup import of a task-file edit left by a cancelled hold, 2026-11-08
- T011: first-run form on the unattached tui path and the missing pins, 2026-11-08
- T022: flags preserved through the non-interactive tui and attach fallbacks, 2026-11-08
- T029: startup settings sweep safe for live submitters, 2026-11-08
- Durable request receipts, 2026-11-08 (AX-009)
- AX-010 working notes, 2026-11-08
- AX-011 working notes, 2026-11-08
- AX-012 working notes, 2026-11-08
- AX-019 working notes, 2026-11-08
- AX-017 checked recovery application, 2026-11-09
- AX-020 working notes, 2026-11-09
- T118: legacy status hid the AX-009 receipts and AX-019 cancellations, 2026-11-09
- T116: startup plans submitted from the owned non-interactive tui fallback, 2026-11-09
- T119: engine held and startup imports refused on a retained-contract edit, 2026-11-09
- AX-013 working notes, 2026-11-09
- AX-022 working notes, 2026-11-09
- AX-026 working notes, 2026-11-09
- AX-027 working notes, 2026-11-09
- AX-028 working notes, 2026-11-09
- T122: legacy status hid the AX-022 startup seed, 2026-11-09
- T115: ownership-map status sync check, 2026-11-09
- T123: start --plan double-start refusal suggests add-plan, 2026-11-10

## Named agent profiles implemented, 2026-11-13

The source now implements the agent configuration proposal above.

- Machine profiles bind an executable to a registered adapter, with optional
  model and effort settings. Project worker and reviewer roles select names
  independently. Claude subscription and Z.ai wrappers can share the Claude
  Code adapter without sharing their command or model selection.
- `looper config` edits machine commands, defaults for new projects, and
  project choices. Known commands are prefilled; custom wrappers select an
  interface. The review shows resolved commands and permission behavior.
- First interactive starts use the same command setup and confirmation flow.
  Fresh unattended projects require an explicit or saved agent choice, with
  `--yes` retaining explicit acceptance of legacy defaults. Inspection and
  unknown-command behavior stays free of setup and execution side effects.
- Existing project profiles and executable/model environment settings remain
  supported. Saved following-reviewer choices override machine reviewer
  defaults. Controllers keep a snapshot of machine commands; new definitions
  can be saved for restart without changing active operations.
- The adapter interface owns arguments, prompt transport, result envelopes,
  result extraction, and permission reporting. Registered capabilities feed
  the configuration form. Pi remains a future protocol implementation.
- Invocation records identify the profile, adapter version, executable,
  model, and effort. The optional `config --test-agent` command checks a
  private fixture and structured output. Its version 2 JSON result has a
  published schema and fixtures.
- Tests isolate each project fixture's machine catalog. Test command guards
  reject accidental use of installed agents instead of spending live usage.
  Zero-valued runner configurations no longer fall back to installed commands.

Validation includes terminal setup/cancellation, custom command entry, role
resolution, migration, live settings requests, per-role concurrency, and
structured-result contracts. Command-level tests pass for the legacy Claude
adapter and distinct worker/reviewer profiles. Live fixture checks passed
through both claude-zai and the Anthropic subscription. Focused race checks
pass. Final targeted checks and build results are retained under
`/tmp/looper-agent-profiles-handoff-tests.log`.

The complete regression suite was stopped to honor the request to finish
quickly. Earlier attempts exposed terminal-test cleanup and catalog-isolation
issues; their focused regressions pass after the fixes. Do not describe the
full suite as passed. No installed binary was replaced, real machine agent
catalog created, or existing controller restarted. Use `build/looper config`
from the target project to configure the source build.

User documentation: [agent profiles](docs/AGENT-PROFILES.md).
