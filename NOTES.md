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

- No controller runs. The detached controller from 2026-11-09 (session
  `run-b1b5da12326f7eb5`, build `0.9.1-dev.12`) was interrupted with
  SIGINT on 2026-11-13 at about 20:20Z. Task state is retained. Its
  run ref is `refs/looper/run-da156c1fa2a7e8c0`, tip `c27b369b`.
- Released `0.10.0`: tag `v0.10.0` at `1d7a614`, pushed with `main`,
  GitHub release from `docs/release-notes/0.10.0.md`, formula updated
  at `03a39be` with the verified archive checksum. `make test` passed
  on the release tree and on the tree with the commit after it.
- Commit `e8b2f65` (after the tag) makes an unattached `looper tui`
  start a detached controller and attach to it; `--foreground` keeps
  the controller in the dashboard process. Ships in the next release.
- The installed binary is `0.10.0` from `1d7a614`.
- Backlog in `to-do.json`: 113 tasks done, 9 blocked, 1 todo.

## Open items

- Operator workflows implemented on 2026-11-14 from
  [operator workflows plan](docs/OPERATOR-WORKFLOWS-PLAN.md). All four
  features and the combined acceptance fixture are done; see the
  implementation section at the end of this file. The 2026-11-14 operator
  feedback assessment that drove the plan is satisfied: checked branch
  handoff (`looper publish`), fresh attempt under the same task ID
  (`looper requeue --fresh`), non-interactive configuration
  (`looper config --set/--unset/--show/--preview`), and the structured
  blocker reason (`block_reason`).

- Operator feedback assessed on 2026-11-14 against current source; all four
  priorities it raised are implemented as the packages above.

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
- The agent configuration redesign is implemented. See the last
  section of this file.
- Stop ignored by an old controller, 2026-11-13. `looper stop` (build
  `0.9.1-dev.12`) wrote a request during a final review. The stop file
  was gone a minute later, no stop or drain event was recorded, and
  the controller imported new file changes and started a second review
  at event 17810. It exited only on SIGINT. Reproduce against current
  source: stop during a review while a tracked file changes.
- Idle reviews on non-source edits. An idle, complete run started a full
  final review (events 17736 and 17810) because agents edited NOTES.md
  and source files. The review re-ran `make test` on a tree it had
  already verified. Consider skipping or shortening a review when only
  files outside `source_files` and the task file change, and an option
  that exits an idle complete run.

## Standing cautions

- Do not edit tracked repository files while the controller integrates
  work. A concurrent NOTES.md append stopped an integration on
  2026-11-05. The incident and the recovery procedure are recorded in
  `/notes/looper-selfrun-monitoring-2026-11-05.md` and in the full-history
  backup.
- Workers run on Claude, not Codex. The default changed in commit
  `468284a` (2026-11-04).
- Before you edit tracked files, run `looper status`. An idle
  controller imports any tracked-file change and can start a new review.
  The rule is also in `.agents/AGENTS.md`.
- `maybeFirstRun` re-resolves the configuration by re-parsing the
  argument list after the first-run form saves a profile. A Config
  field set only on the struct in a test is dropped. Pass such flags in
  the argument list (seen with `--foreground` in the first-run PTY
  test, 2026-11-13).

## Claude provider selection after subscription migration, 2026-11-13

Local inspection only. No agent adapter or runtime configuration was changed.
The working tree already contains substantial uncommitted work.

- `internal/looper/config.go` accepts `codex`, `claude`, and `zcode` as agent
  kinds. It defaults to `claude` and resolves one `ClaudeBin` from
  `CLAUDE_BIN`, defaulting to `claude`.
- On this host, plain `claude` now authenticates with Claude Pro. The separate
  `claude-alt` wrapper selects alt-provider GLM-5.3, a 1M context limit, 300k compaction,
  and disabled Artifact. Shell aliases are not executable agent kinds.
- alt-provider selection requires `CLAUDE_BIN=claude-alt looper --agent claude`.
  Use an absolute wrapper path for launch environments with a different PATH.
- Worker and review roles share `ClaudeBin` and `ClaudeModel`. They cannot
  select the Anthropic and alt-provider Claude entry points independently in one run.
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
  No live Looper worker was used to verify the alt-provider structured-output path.
- Invocation records retain agent kind `claude` for either provider and omit
  the selected executable. New failure summaries can include the binary path.
  Provider identity is therefore not uniformly visible across records.
- Installed `looper --version` returns `0.9.1-dev.12 (3e52b5e)`; its executable
  is dated November 4. Do not assume this source checkout matches that binary.
  `looper help preflight` and `looper help settings` unexpectedly entered the
  run path from `/home/user`; both inspection processes were interrupted.
  No home backlog exists. The command mismatch was logged as a papercut.

Follow-up: add explicit provider selection per role if one run needs GLM
workers and Anthropic Claude review. Keep executable and provider identity in
invocation records. Existing controllers require separate inspection before
any provider migration; this audit did not restart them.

## Proposed agent configuration redesign, 2026-11-13

Discussion proposal only; not approved for implementation.

Replace agent-kind selection as the user-facing abstraction with named agent
profiles. A profile binds an executable to a supported adapter and optional
model/effort settings. `claude` and `claude-alt` can both use the Claude Code
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

### Undated headings, all from the hotel-run diagnosis, 2026-10-12
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
- Echo integration error, 2026-10-12, 19:51Z
- Docker postgres accumulation, 2026-10-12, 20:25Z
- Charlie bootstrap failure, 2026-10-13, 11:57 local
- Charlie diagnosis, 2026-10-13, 11:21 to 11:24Z
- Test suite status on Darwin, 2026-10-13, 13:35Z
- Charlie drain after sleep diagnosis, 2026-10-13, 13:08–13:10Z
- Echo check wedge, 2026-10-13, 06:35Z
- Release 0.8.3, 2026-10-14
- Echo follow-up control, 2026-10-14
- Bravo bootstrap sources and skill scope, 2026-10-14
- Latest-release installer, 2026-10-14
- Unattended recovery implementation, 2026-10-15
- Release preparation, 2026-10-15
- Bravo diagnosis, 2026-10-15, 05:09 UTC
- Bravo backlog staging, 2026-10-15
- Bravo follow-up diagnosis, 2026-10-15, 06:38 UTC
- Bravo check, 2026-10-15, 08:22 UTC
- Bravo diagnosis, 2026-10-15, 13:18 UTC
- Bravo GitHub permission and retry, 2026-10-15
- Bravo remaining work, 2026-10-15, 13:48 UTC
- Bravo R2 credential handoff, 2026-10-15
- Bravo review repair, 2026-10-15, 19:58 UTC
- Foxtrot review recovery and clipboard, 2026-10-18
- Foxtrot controller replacement and retained publication, 2026-10-18
- Foxtrot clipboard through tmux and iPhone, 2026-10-18
- Worktree consolidation, 2026-10-23
- Delta bootstrap schema failure, 2026-10-27
- Golf Codex account lapse, 2026-10-30
### 2026-11
- Alpha retained-state recovery, 2026-11-02
- Alpha full-run session analysis, 2026-11-03
- Alpha improvement plan validation, 2026-11-03
- Alpha improvement plan implementation, 2026-11-03
- Improvement plan diff review, 2026-11-03
- Improvement review fixes, 2026-11-03
- Alpha dashboard and scheduling correction, 2026-11-03
- Alpha import scheduling and event fixes, 2026-11-03
- Commit and install, 2026-11-03
- Dev version stamping, 2026-11-03
- End-to-end bug review, 2026-11-03
- Backlog audit against the codebase, 2026-11-03
- Alpha promise review, 2026-11-03
- Alpha scheduling fixes, 2026-11-03
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
- Alpha blocked-run diagnosis, 2026-11-04
- ALPHA device feedback work, 2026-11-04
- ALPHA scope correction and reproduced save stalls, 2026-11-04
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
  independently. Claude subscription and alt-provider wrappers can share the Claude
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
through both claude-alt and the Anthropic subscription. Focused race checks
pass. Final targeted checks and build results are retained under
`/tmp/looper-agent-profiles-handoff-tests.log`.

The complete regression suite was stopped to honor the request to finish
quickly. Earlier attempts exposed terminal-test cleanup and catalog-isolation
issues; their focused regressions pass after the fixes. Do not describe the
full suite as passed. No installed binary was replaced, real machine agent
catalog created, or existing controller restarted. Use `build/looper config`
from the target project to configure the source build.

User documentation: [agent profiles](docs/AGENT-PROFILES.md).

## Quick wins after the leftover controller, 2026-11-13

- The README is 235 lines. Reference text moved, unchanged, into seven
  pages under `docs/`: install, dashboard, tasks and checks, agents,
  recovery, runtime requests, and observability.
- `.agents/AGENTS.md` tells agents to check for a live controller and
  not to discard published, uncommitted work.
- `looper stop --interrupt` sends SIGINT through `signalController`,
  which checks the recorded session and process start time first.
- Plain `looper doctor` names the live controller and warns when its
  build differs from the executable.
- Plain status labels the phase age as `last output … ago`. It is the
  time since the agent's last output, not the phase duration.

## Operator workflows implemented, 2026-11-14

Implementation of [operator workflows plan](docs/OPERATOR-WORKFLOWS-PLAN.md).
OW-01 through OW-04 and OW-07/OW-08 landed in commit `9b3775e`
(`looper publish` and `looper requeue --fresh`). The remainder landed
after it:

- OW-05/OW-06, headless configuration: `looper config` serves
  `--show`, `--preview`, `--set`, `--unset`, project and machine scope,
  `--if-revision`, and a stopped-project journal beside `profile.json`.
  Public worker/reviewer aliases canonicalize before the payload digest,
  so `worker=` and `agent=` share one idempotency identity. The version 2
  document is `docs/contracts/schemas/v2/config.schema.json`. Core code:
  `internal/looper/config_cmd.go`.
- OW-08 leftover: the dashboard requeue dialog gained **[f] Start
  fresh**; both dashboard control sites submit `requeue-fresh`.
- OW-09/OW-10, structured blocker reasons: `Task.BlockReason`
  (`class`, `code`, `retry`, `attempt_id`, `source`) with classification
  from typed sources only, control tags authoritative, `Contract()` clear
  while `hashJSON(task)` tracks reason edits. Version 2 doctor findings,
  status findings, and compact inspection carry the reason; operator and
  deferred downgrade to informational severity. Legacy documents omit the
  field everywhere, including nested tasks and `--full`. `looper amend`
  can correct reason metadata on an idle blocked task. Core code:
  `internal/looper/block_reason.go`.
- Schema decision: `to-do.schema.json` keeps `schema_version` 1 and gains
  the optional `block_reason` (additive). Older Looper binaries decode
  the file and ignore the field; an external validator pinned to the
  pre-extension schema may reject it. Recorded in
  `docs/AGENT-CONTRACT-MIGRATION.md` and the 0.11.0 release notes.
- OW-11: combined acceptance fixture
  `TestOperatorWorkflowAcceptanceFixture` runs all seven plan steps
  (headless config, accepted work beside a typed quota failure,
  publication, provider switch with fresh requeue, restart under the new
  worker, doctor classes, second publication with clean resume). The
  crash variants of steps 3 and 4 stay covered by the publish journal
  tests and the requeue rollback test.
- Documentation: publish section in `docs/recovery.md`, requeue section
  in `docs/runtime-requests.md`, headless configuration in
  `docs/AGENT-PROFILES.md`, monitor policy in `docs/observability.md`,
  four change-log entries in `docs/AGENT-CONTRACT-MIGRATION.md`, and
  `docs/release-notes/0.11.0.md` (prepared; no release published, no
  installed binary replaced).

Validation outcomes, 2026-11-14:

- `go test ./... -count=1 -timeout 40m`: pass, after one guard fix. The
  publish help example's quoted `--message "Accept completed work"` failed
  `TestCommandHelpCoversTheSurface`, which split examples on raw
  whitespace; the guard now splits shell-style, honoring double quotes.
- `go test ./internal/contractschema -count=1`: pass.
- `go test ./internal/looper -run 'Test(LegacyContract|Capabilities|ClientCommands)' -count=1 -timeout 30m`: pass.
- `go test -race ./internal/looper -run 'Test(Publish|Requeue|Settings|AgentProfiles|Config)' -count=1 -timeout 40m`: pass.
- `go test -race ./internal/looper -run 'TestOperatorWorkflow|TestRequeueFresh|TestBlockReason|TestControlTags|TestApplyResult' -count=1 -timeout 30m`: pass.
- `make test`: the first run failed only in `scripts/runtime-test.py`
  under host load (7 problems, 2 954 s against a ~380 s reference). The
  same 73 runtime tests pass alone in 576 s; `go vet`, the installer,
  build, and skill scripts passed in the same make run, and
  `package-smoke.sh` plus `skill-regression.py` pass separately. The
  failure class matches the documented budget flakes, not a regression.

## README rewrite and figure recorder, 2026-11-15

- The README is 177 lines. It leads with what Looper does, a five-step
  quickstart with an example `PROJECT.md`, and what Looper does to the
  repository. The configuration table and the exit codes moved, unchanged,
  to `docs/configuration.md`. Two stale claims were fixed: "Looper does not
  commit" now names `looper publish`, and the agent example uses real
  profile names.
- `scripts/record-demo.py` re-records `docs/assets/loop.gif` and
  `docs/assets/tui.png` from a real `looper tui` run with a stub `claude`.
  It uses a private tmux socket, a 120x26 pane, and a temporary `HOME`.
  Run `make build` first. The October figures were a manual capture that
  showed `codex` planning and the old key hints.
- Recorder gotchas: unittest writes `__pycache__`, which fails the
  changed-files check unless the project ignores it. A worker must report
  `block_reason`, or the inspector shows `unknown (unknown)`. Killing tmux
  does not stop a detached controller; the script interrupts through the
  exit dialog.
- Dashboard defects seen while recording, not fixed:
  - The task view (`u`) opens with the cursor on the second row (T001),
    not on the blocked task that sorts first.
  - A multi-line blocker in the overview Attention section leaves about
    ten blank lines under the first line.
