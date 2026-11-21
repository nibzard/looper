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

- `T117` implemented on 2026-11-19; see the T117 section at the end.
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
  - Resolved 2026-11-19 by T128: a review records its coverage (tree,
    completion verdict, imports seen), and the next cycle skips the
    agent when the content is unchanged or the change is outside
    `source_files` with the goal already closed; open work gets a
    focused pass over the changed paths. Repeated unchanged findings
    escalate one operator decision per target. The exit option already
    exists as `--once`.

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
- Dashboard defects seen while recording, fixed 2026-11-18 in T132 (see
  the "Dashboard figure defects fixed" section below):
  - The task view (`u`) opens with the cursor on the second row (T001),
    not on the blocked task that sorts first.
  - A multi-line blocker in the overview Attention section leaves about
    ten blank lines under the first line.

## Public-launch cleanup review, 2026-11-15

The repository at `github.com/nibzard/looper` is already public. Review
of the tracked tree, the pushed refs, and the published tags:

- No `.looper` directory is tracked anywhere, on `main`, or in any
  pushed ref. The reported "two `.looper` dirs" were in the public
  `nibzard/ohya` repository: two `looper_<hash>/tNNN` runtime
  directories (five check logs) committed by accident because ohya's
  `.gitignore` lacks the `looper_*/` entry this repository has. That
  repository's remote also published a `refs/looper/run-*` state ref
  and a merged `looper-parked-docs` branch. Fixed on 2026-11-15:
  ohya commit `af02a90` removes the directories and ignores
  `looper_*/`; both internal refs were deleted from the ohya remote.
  Three `refs/notes/*` refs (82 notes, scanned clean of private
  strings) remain published there pending an owner decision.
  Separately, contract
  fixtures and tests here embed literal paths such as
  `/home/user/.looper/alpha-b17f/runtime`; about 50 tracked files
  mention `.looper` and 35 mention `/home/user`.
- No LICENSE file exists. A public launch needs a license decision.
  Resolved 2026-11-20: the owner chose MIT on 2026-11-18 and `LICENSE`
  is in the tree; see the section at the end.
- Private project names (Alpha, Bravo, Charlie, Delta,
  Echo) and the `claude-alt`/alt-provider provider details appear in
  tracked files: `NOTES.md` (27 lines), `UNATTENDED-RECOVERY-PLAN.md`,
  `docs/research/MONITORING-ACTIONS-2026-10-12.md`, fixtures under
  `docs/contracts`, and tests.
- Pushed to the public remote: `refs/notes/{adr,context,design,todo}`
  with 65 notes; 8 lines in the blobs carry private names or
  `/home/user` paths.
- Published tags carry the old internal `NOTES.md`: 42 private-name
  lines at `v0.8.3`, 27 at `v0.10.0`. Scrubbing them needs a history
  rewrite or tag deletion; decide before promoting the repository.
  Resolved 2026-11-19 by the history rewrite below.
- Two tracked symlinks are broken: `.claude/skills/release-runbook` and
  `.codex/skills/release-runbook` point to a missing
  `skills/release-runbook`.
- Commit metadata includes `reviewer <reviewer@example.com>` (21
  commits) and one commit with a personal Gmail address; fixing needs
  the same history rewrite. Resolved 2026-11-19: every commit author
  and committer now reads `nibzard <nibzard@users.noreply.github.com>`.
- Root and `docs/` hold internal working documents next to user docs:
  `EXPERIMENT-PLAN.md`, `UNATTENDED-RECOVERY-PLAN.md`,
  `AUTONOMOUS-AGENTS.md`, `PRODUCT.md`, and in `docs/` the seven
  `*-PLAN.md`/review/prep files plus `docs/research/` (22 files).
  `MANIFESTO.md` is linked from the README Development section.
- No secrets found: no API keys, tokens, or private keys in the tracked
  tree. All regex hits were `task-<hex>` false positives.
- No CI workflows exist (`.github/` is absent).

Proposed order: pick the license; scrub `main` (private names, notes
refs, broken symlinks, internal-doc moves); then decide on the history
rewrite for tags and commit metadata. Fixture paths are byte-pinned, so
neutralizing them also regenerates fixtures and their tests.

## History scrub, 2026-11-19

The owner chose the full history rewrite (option 3 of the recorded
decision). `git filter-repo` ran twice over all 459 commits with a
literal replacement table and forced identities; the pre-scrub state is
in `/home/user/looper-pre-scrub-2026-11-19.bundle` (all refs, tip
`07fa9be`).

- Project names map to NATO codenames: Alpha, Bravo, Charlie, Delta
  (both spellings), Echo, Foxtrot, Golf, Hotel (phrase-level rules keep
  plain-English "margin"), India. Case variants are covered.
- `claude-zai`/`zai` became `claude-alt`/`alt`, `Z.ai` became
  `alt-provider` (capitalized `Z.AI` in the pricing README survived and
  stays as public provider data).
- `/home/agent` became `/home/user` across content, including the
  byte-pinned fixtures; both fixture and generator sides changed
  consistently.
- Every commit author and committer now reads
  `nibzard <nibzard@users.noreply.github.com>`.
- `internal/looper/prices/data.json` was restored to its pre-scrub
  content after the rewrite: its `zai-org/glm-*` model slugs are public
  catalog data, and renaming them would break price matching. The
  catalog carries no private strings.
- Local `refs/notes/*` still hold pre-rewrite notes commits (old
  identities in notes commit metadata). The remote copies were deleted;
  do not push `refs/notes/*` again before they are rebuilt.
- GitHub consequences: every tag points at a new object, so the
  published release archives for `v0.10.0` and `v0.11.0` no longer
  match their tags, and the Homebrew formula checksum for `v0.10.0`
  is stale. Re-cut or drop those releases; the next release records
  fresh checksums.
- Still open from the cleanup review: moving internal working documents
  out of the tree, and CI. The license closed on 2026-11-20 (T129):
  `LICENSE` is in the tree and the next release notes carry it.


## Delivery audit, 2026-11-17

Research only. See [the delivery audit](/notes/looper-delivery-audit-2026-11-17.md)
for runtime evidence, Codex review references, proposed changes, and open questions.

- Six retained projects show 455 integrations at the initial observation.
  Charlie adds integration 456 during the audit: T090 fixes the hidden player,
  with 1,135 passing cases in its retained verification log.
- Main current gap: reviews cannot reconcile existing blocked/deferred tasks.
  Ohya T093 readiness appears in seven reviews before it starts. Charlie
  T077 is reported delivered seven times and remains blocked. Source confirms
  findings.go only reopens idle done/todo coding tasks; it preserves blocks.
- Circular conditions keep Charlie T027, T040, and T066 out of the queue.
  Core progression, brewing, and saves remain incomplete despite green checks.
- Current 0.11.0 reviews stop both Ohya and Charlie on timed provider quotas
  classified as configuration failures. Observed 429 wording misses the
  transient pattern; usage-limit wording matches configuration.
- Concurrent note/demo edits and commits still stop safe publication. Ohya
  remains stopped at event 12757 with verified T140 work retained. Guards
  preserve competing edits; recovery remains an operator cost.
- Cleanup errors repeat across a few artifacts. External resource leakage
  is unverified. Historical Alpha import flooding is guarded in current
  source; do not list it as an established current defect.
- Runs, project backlogs, binaries, and session data remain unchanged.
  Four Papercut reports record reconciliation, quota, cleanup, and large
  legacy status output. No implementation or restart was performed.

## Review reconciliation implemented, 2026-11-18 (T124)

Implements audit finding 1: findings can now reconcile blocked, deferred,
and wrongly owned tasks instead of only recording evidence.

- `Finding` gains `transition` (`resume`, `complete`, `reassign`) and
  `revision`. The review prompt names the verified revision and the three
  transitions. The worker prompt teaches none of it: the 4 KiB worker prompt
  budget has about 170 bytes of headroom, and a worker cannot know the
  accepted revision mid-flight, so its transition would be refused anyway.
- Guards, in order: existing target, revision equals the accepted revision
  the stage records (`e.State.Tip`), no attempt holds the contract (the same
  boundary `amendHoldsContract` draws), then the gate checks. `resume` also
  requires every dependency done. Transitions change only fields outside
  `Contract()`, so retained work keeps its acceptance identity.
- `resume` sets an idle blocked task to `todo` and clears blockers,
  `unblock_when`, and `block_reason`. `complete` closes a task whose outcome
  exists without a worker; repeats on the done task record evidence only.
  Completed work is never reopened by a transition.
- Operator gates, deferrals, ownership changes, and a blocked task whose own
  condition names itself are escalated, not applied: one operator decision
  task keyed `looper:finding:reconcile-<task>`, with the refusal reason and
  a concrete action (task-file edit at a safe boundary, or `looper cancel`).
  Later reports add evidence to it, never a second decision.
- Plain agent findings that repeat against a blocked agent task (its key tag
  already recorded, no state change) escalate the same decision once.
  Operator-owned findings and gated tasks keep the blocked task itself as
  their stable action, so `TestFindingKeyDeduplicatesRenamedBlocker` and
  `TestFindingReferencesExistingTask` keep one task.
- A report against a decision task records evidence only: the decision task
  carries a `looper:finding:reconcile-<id>` tag, and such a target never
  escalates a second decision about the decision.
- Tests: `internal/looper/reconcile_test.go` (16 cases, including the real
  review path with a stub agent and the staged rollback beside a failed
  export). Validation: focused runs pass; the full package suite and
  `make test` results are recorded below.
- Second defect class caught while implementing: a retained worker summary
  keeps its proposal so recovery can replay it, and the complete status
  document embeds attempts inline, so `transition`/`revision` would have
  leaked into `attempts[].summary.followups[]` — the frozen legacy status
  schema pins that shape with `additionalProperties: false` (the
  T100/T118/T122 class). `statusReport` now clears both keys on the
  projection copy, the frozen-shape scenario carries a proposal, and the
  shape fixtures were re-recorded with `-update-contract-fixtures`. Legacy
  `inspect` leaves them visible: its schema keeps attempt items open.

Validation, 2026-11-18:

- `go test ./... -count=1 -timeout 40m`: pass (looper package 549 s).
- `go vet ./...`: pass. `gofmt` clean on every changed file.
- `go test -race ./internal/looper -run TestReconcil`: pass.
- `python3 scripts/runtime-test.py` after `make build`: 73 tests pass in
  278 s. The installer, package-smoke, and skill scripts of `make test` were
  not rerun; this change touches the engine and documents only.

Limits found while implementing, not fixed here:

- Circular detection is textual: a condition that names the blocked task's
  own ID. Charlie T040's wording ("waits for save work to resume") carries
  no ID and is not detected; a review must still name it. Closed 2026-11-20
  by T140; see the T140 section at the end.
- A worker follow-up that carries a transition must report the accepted
  revision, which a worker cannot know mid-flight; such reports are refused
  with a journal note. Reviews, which learn the revision from the prompt,
  are the intended source.

## Timed quota pause implemented, 2026-11-18 (T125)

Implements audit finding 3: timed provider quota exhaustion no longer stops
a controller as a configuration failure. Core code:
`internal/looper/quota.go`, classification in `internal/looper/agent.go`.

- `classifyFailure` gains a `quota` kind, checked first. Two shapes match:
  the five-hour usage-limit wording with a reset, and the observed
  `API Error: Request rejected (429)` rejection (Ohya event 12262, Charlie
  event 8271 both carry `Usage limit reached for 5 hour. Your limit will
  reset at <wall clock>`). Untimed quota text ("this account quota is
  exhausted", billing, credit) stays `configuration`: no reset exists, so an
  operator must act. Ordinary rate limits stay `transient`.
- `quotaResetTime` parses the provider's reset as reported: zone-free wall
  clocks and clock times resolve on the local clock (the agent process
  shares the controller's clock), zoned timestamps keep their zone, and
  `in 2h 6m` style relatives add to now. A clock time already past names
  tomorrow. The reported text is retained verbatim in the pause event and
  the hold blockers; no outage duration is derived from it.
- `AgentError` carries the parsed `Retry`. The in-run transient retry loop
  never retries a quota window; the controller owns the wait.
- The controller pauses the affected agent profile
  (`Engine.quotaPauses`): dispatch gates on the worker profile, review,
  planning, and queued plan imports gate on the reviewer profile. Worker
  attempts hold with `block_reason` code `quota_window` (class `failure`,
  retry `yes`) and the provider's reset text; `quota` and `quota-resume`
  journal events carry `profile`, `retry_at`, `reset_text`, and `windows`.
- At each loop boundary `expireQuotaPauses` resumes reopened windows and
  requeues the quota-held tasks with the accepted-requeue transition (only
  fields outside `Contract()`). Pauses live for the session; a restart
  rediscovers a closed window from the provider's next failure because
  quota-held tasks and pending reviews persist in the saved state.
  Erratum, 2026-11-18 (T146): that rediscovery claim held only for work a
  restart actually dispatches. A quota-held task is blocked, so nothing
  dispatches it and no provider failure can re-arm the pause; a restart
  mid-window stranded the held task past the recorded retry until an
  operator requeued it. Startup now re-arms the pause from the saved
  state's held tasks — see the T146 section.
- Spending policy reuses the existing rate-limit knobs:
  `LOOPER_RATE_LIMIT_MAX` bounds consecutive quota windows without an
  integration (past it, work stays blocked for an operator), and
  `LOOPER_RATE_LIMIT_DELAY` is the pause when the provider reports no reset.
  One wait is capped at 24 h, so a weekly window costs one probe per day.
- Startup bootstrap still exits on a quota error with the reset text in the
  message: no loop exists to hold the pause before the first task file
  exists. Queued imports wait for the reviewer window instead of failing.

Tests: `internal/looper/quota_test.go` (classification and reset shapes, the
stub-agent acceptance routes, the budget boundary, and the credential stop).
The OW-11 fixture's untimed quota case keeps its configuration class and its
single attempt. Docs updated: `docs/observability.md` (monitor policy),
`docs/configuration.md` (env scope), `docs/AGENT-CONTRACT-MIGRATION.md`
(change-log row).

## Circular-blocker doctor warning implemented, 2026-11-18 (T135)

Implements QUICK-WINS-PLAN section 1: `looper doctor` now warns about a
blocked task whose resume condition requires the task itself. Core code:
`internal/looper/observe.go`, text output in `internal/looper/cli.go`.

- The finding code is `task_circular_condition`, severity warning. It rides
  beside `task_deferred` or `task_blocked`, because a deferred task can
  record a condition only its own implementation can satisfy (the audit's
  Charlie T027/T066 shape). Doctor changes nothing; the exit code policy
  for warnings is unchanged (legacy warnings return 0, version 2 exits 0).
- Detection is textual, bounded, and covers both recorded shapes: the
  identifier in requirement position (for, until, on, by, after, when,
  need, require, await directly before it) and the identifier in subject
  position with its own progress as the required event ("T027 starts
  implementation", "T066 is unblocked"). Honest mentions stay quiet:
  consequence connectors ("then", "before"), an external clause before a
  comma ("after the operator grants access, T027 resumes"), an external
  cue after the identifier ("T027 starts verification when the migration
  lands"), a possessive mention ("T065's unblock condition"), a passive
  deliverer ("itself unblocked by T118's push"), and a colon label
  ("T052 unblocks: a device is attached"). Verified against the retained
  Charlie and Ohya backlogs: Charlie warns on T027, T033, T038, T040,
  T045, T049, T050, T054, T055, and T066 — every one a "starts
  implementation"/"is unblocked" deferral — and Ohya warns on T040 alone;
  both audit circular classes are covered with no false positives found.
- The finding keeps warning severity beside operator and deferral reason
  classes in version 2 doctor and compact status (`doctorFindingViews`,
  `statusFindingsFor`): expected waiting ends where only the blocked task's
  own progress could satisfy the condition. Monitor policy row added to
  `docs/observability.md`.
- The T124 reconcile guard in `findings.go` stays broader on purpose: it
  escalates on any mention of the task itself, because an automatic
  transition must err toward the operator, while this warning must stay
  quiet on honest conditions.
- Plain text reads the task file (`c.Todo`); JSON doctor diagnoses the
  saved state, as it always has. A condition that names no identifier at
  all (Charlie T040's summary "waits for save work to resume") stayed
  outside the check, as documented for T124, until T140 extended it on
  2026-11-20; see the T140 section at the end.

Tests: `internal/looper/circular_condition_test.go` (detection table, legacy
JSON, version 2 JSON with the deferral exemption, plain text with the
unchanged task file, compact status).

Validation, 2026-11-18:

- The detector was also replayed against the retained Charlie and Ohya
  saved states (read-only); the warnings it produced are listed above.
- `go test -race ./internal/looper -run 'TestCircularResumeCondition|
  TestDoctorWarnsOnCircularResumeCondition|TestCompactStatusKeepsCircularWarning'`:
  pass.
- `go vet ./...`: pass. `gofmt` clean on every changed file.
- `go test ./... -count=1 -timeout 40m`: the looper package ran 2 326 s
  under host load and failed two timing tests, `TestAddTaskRequestWhileWorkerRuns`
  (run timed out) and `TestCancelDuringAssessmentResumesVerification`
  (assessment never started); both pass alone in about two seconds, the
  documented budget-flake class (see T108), not a regression. Every other
  package and test passed.
- End-to-end with the built CLI on a fixture carrying the real retained
  conditions: text doctor warns for the circular shapes, stays quiet for
  the colon-label, external-cue, and consequence shapes, exits 0, and
  leaves the task file byte-identical.

## Quota failure diagnostics implemented, 2026-11-18 (T136)

Implements QUICK-WINS-PLAN section 2's reporting patch on top of T125's pause
machinery: the agent failure classification itself now reports a timed quota
failure as a diagnostic. Core code: `internal/looper/agent.go` (diagnostic
construction and the action text), `internal/looper/failure.go` (the summary
field, outcome code, and message rendering), `internal/looper/errors.go`
(the error document action).

- A `quota`-classified agent failure's bounded summary (AX-021) gains
  outcome `quota_window` and an `action` string: work is retained, no
  in-run retry is scheduled, Looper resumes at the parsed reset, and past
  the pause budget the operator reruns Looper after the window or requeues
  the held task (`looper requeue`). A rejection without a reset names the
  configured wait instead of a time. The provider's reset wording stays in
  `cause` exactly as reported — timestamps without a time zone included —
  and no outage duration is derived from it.
- The human failure line renders `<operation> quota window (exited N):
  <reset text>` instead of a bare exit failure, so plain `looper events`
  self-diagnoses. The version 2 error document leads `next_actions` with
  the failure's own action, which is what the startup bootstrap exit path
  shows: no loop exists there to hold a pause, so the operator sees the
  resume step in the error itself.
- Every other class is unchanged: invalid credentials stay
  `configuration`, ordinary rate limits stay `transient`, and their
  summaries keep empty `action` and byte-identical rendering. Existing
  fixtures do not change; the new keys ride only on the new class.
- Acceptance reconciliation with T125: the plan text proposed keeping the
  stop-and-retain behavior, but T125 landed first with the wider scope the
  plan section now records (profile pause, automatic requeue at the recorded
  reset within `LOOPER_RATE_LIMIT_MAX` windows). The criteria hold as
  written at the classification layer: a stub emitting the observed message
  produces the quota diagnostic with reset text and action; credentials and
  rate limits keep their classes; a quota failure retains work and burns no
  in-run retry (the controller, not the retry loop, owns the wait).
- `status_compact_test.go` was committed unformatted at HEAD (a T135
  leftover); `gofmt -w` realigned one struct field.
- Race repaired in T125's pause gates, found by the full-suite run: a quota
  deadline that rolls over mid-iteration opened the review gate before the
  loop boundary consumed the pause, so the restarted review raced ahead of
  its own `quota-resume` event (`TestReviewQuotaPauseResumesAutomatically`
  failed about one run in six with "no quota-resume event"). `quotaPaused`
  now keeps a pause closed until `expireQuotaPauses` consumes it at the
  boundary, and `quotaWaitMessage` names the same records, so the resume
  journal and the requeue always precede any gate. Verified with 60
  consecutive review-route runs and 5× the other quota routes.

Tests: `internal/looper/quota_test.go` gained the T136 agent-level cases
(observed message with reset, rejection without reset, credentials and rate
limits) and controller-level assertions that the journaled worker failure
summary carries the quota outcome and action.

Validation, 2026-11-18:

- `go test ./internal/looper -run 'TestAgentQuota|TestClassifyTimedQuota|
  TestQuotaResetTime' -count=1`: pass.
- `go test ./internal/looper -run 'TestWorkerQuotaPauseResumesAutomatically|
  TestReviewQuotaPauseResumesAutomatically|TestPlanningQuotaPauseResumes|
  TestCredentialFailureStillStopsTheRun|TestQuotaPauseBudgetHoldsWorkForOperator'
  -count=1`: pass.
- `go test ./internal/looper -run 'TestAgentFailure|TestFailure|TestError|
  TestEvents|TestLegacyContract|TestPublicError' -count=1` and
  `go test ./internal/contractschema -count=1`: pass.
- `go vet ./...`: pass. `gofmt` clean on every changed file.
- After the gate repair: `TestReviewQuotaPauseResumesAutomatically -count=60`
  and the other four quota routes at `-count=5`: pass.
- `go test ./internal/looper -count=1 -timeout 40m`: pass (567.7 s). The
  first run, before the gate repair, failed only
  `TestReviewQuotaPauseResumesAutomatically` with "no quota-resume event";
  the repair above closes that race. Every other package passed then too.
- `make build` then `python3 scripts/runtime-test.py`: 73 tests pass in
  268 s. The installer, package-smoke, and skill scripts of `make test`
  were not rerun; this change touches the engine and documents only.

## Compact status in agent examples implemented, 2026-11-18 (T137)

Implements QUICK-WINS-PLAN section 3: the agent-facing monitoring examples
now teach the bounded compact status and link to `--full`. Documentation
only; no code, schema, or document shape changed.

- `docs/AGENT-CONTRACTS.md` gains a "Routine monitoring" subsection under
  Contract versions: the compact call, the document it returns
  (`controller.state` with its five values, `cursor.event_id`, limits and
  detail routes), the `--full` route for the detailed state, and the
  statement that plain `looper status --json` keeps its legacy format.
- `docs/observability.md` now says its `status` sections describe the
  complete document, names both transports that serve it (plain `--json`
  under the legacy default, `--contract v2 --full --json` under version 2),
  and routes routine monitoring to the compact document.
- `docs/recovery.md` and `.agents/AGENTS.md` (root `AGENTS.md` symlinks to
  it) use the compact call for their monitoring examples and name `--full`.
- Already correct, unchanged: `looper help status` (`internal/looper/help.go`)
  leads with `looper status --contract v2 --json` and teaches `--full`;
  `skills/looper-diagnose` teaches the compact call with detail routes;
  `docs/AGENT-CONTRACT-MIGRATION.md` keeps the compatibility table.
- Legacy examples stay only where they explain compatibility, per the plan.

Example run (built CLI 0.11.1-dev.11, stub agent, private base directory
under the task resource prefix): a live controller returned
`controller.state: "controlling"` with the in-flight attempt and
`cursor.event_id: 11` in 1 812 bytes; after the run finished, the stopped
document reported `controller.state: "stopped"` and cursor 19 in
1 342 bytes. Both validate against
`docs/contracts/schemas/v2/status-compact.schema.json`. Plain
`looper status --json` returned the complete legacy document (7 033 bytes
on this two-task fixture), validates against the frozen legacy schema, and
carries no `contract_version` or `kind` key; `--contract v2 --full --json`
served the same document shape (only `observation.observed_at` differed
between the two captures). This is the bounded contrast the 2026-11-17
audit Papercut recorded: the retained projects returned about 750 KiB from
plain `status --json`.

Validation, 2026-11-18:

- `go test ./internal/contractschema -count=1`: pass.
- `go test ./internal/looper -run 'TestCommandHelp|TestSkill' -count=1`:
  pass (help and skill guards).
- `go test ./internal/looper -run 'TestLegacyContract|TestCommandSurface|
  TestStatusCompact|TestCompactStatus' -count=1`: pass.

## Cleanup failure grouping implemented, 2026-11-18 (T138)

Implements QUICK-WINS-PLAN section 4: repeated identical resource-cleanup
failures group per artifact inside one controller session. Core code:
`internal/looper/resources.go` (grouping, summaries), one Engine field in
`engine.go`, and the closing sweep call in `run.go`'s shutdown defer.

- The first cleanup failure of an artifact reports exactly as before: the
  unchanged `resource-cleanup-error` notice (its message is pinned by
  `TestCleanupFailuresDistinguishAbsentPathsFromExitFailures`) and the
  bounded `failure` summary (AX-021). Identical repeats journal neither;
  `cleanupFailures` counts them per artifact directory. The 2026-11-17
  audit counted 224 identical notices for three Ohya artifacts; a session
  now emits one notice plus summaries.
- Summaries: every 10th identical repeat (`cleanupSummaryInterval`) and the
  session's closing sweep (after the final `sweepResources` in the `Run`
  defer) journal one `resource-cleanup-summary` event with the session's
  repeat count — `Temporary resources remain in <dir> after N identical
  cleanup failures this session: <err>. Cleanup retries at the next
  completion or restart.` A summary without new repeats is itself noise, so
  the closing sweep stays quiet when nothing repeated since the last report.
- A different artifact keeps its own group. A changed error on the same
  artifact reports at once in full, because a changed cause is new
  information; the new cause's count restarts at its first failure.
- Cleanup behavior is untouched: every sweep boundary still re-runs
  `cleanResourceLease`, a failing lease stays `cleanup_pending` with its
  error, and a later success marks it `cleaned` and drops the group, so a
  new failure of the same artifact is a new episode that warns again. The
  group map is owner-goroutine session state, like `quotaPauses`; nothing
  persists, and a new session starts fresh counts (its first failure warns).
- Scope kept bounded to the plan: the lease read/validate error paths in the
  same walk (unreadable `resources.json`, invalid lease) are not grouped;
  the plan and audit evidence name cleanup failures, and those paths have
  no structured event or repeat evidence. The walk-level error stays
  ungrouped because it is not artifact-scoped. T143 later grouped the
  read/validate paths with the same mechanism.

Tests: `internal/looper/resources_test.go` gained five cases (one warning
pair plus a correct closing count, the interval bound, per-artifact and
changed-error separation, success clearing the lease and resetting the
group, and the real `Run` path through a stub-agent session), plus two
read helpers.

Built-CLI demo (stub agent, two planted leases whose scripts fail, one
runtime under the task resource prefix, read back with `looper events
--json`): two sessions over two failing artifacts journaled 4 notices, 4
cleanup failure summaries, and 4 closing summaries ("after 2 identical
cleanup failures this session" — each session swept twice); the session
after the scripts were fixed journaled 0 cleanup events and both leases
read `cleaned`.

Validation, 2026-11-18:

- `go test ./internal/looper -count=1 -timeout 40m`: pass (569.9 s; the
  full-repo run below is the same package under the same build).
- `go test ./... -count=1 -timeout 40m`: pass (looper 568.4 s).
- `go test -race ./internal/looper -run 'TestRepeatedCleanupFailures|
  TestCleanupFailureInterval|TestCleanupFailureGroups|TestCleanupSuccessClears|
  TestRunSummarizesRepeated|TestCleanupFailuresDistinguish|TestWorkerCleansResources|
  TestCancelledCheckRetains' -count=1`: pass.
- `go vet ./...`: pass. `gofmt` clean on every changed file.
- `make build` then `python3 scripts/runtime-test.py`: 73 tests pass in
  286 s. The installer, package-smoke, and skill scripts of `make test`
  were not rerun; this change touches the engine and documents only.

## Startup bootstrap quota wait implemented, 2026-11-18 (T141)

Closes the T125 limit: a timed quota window during startup bootstrap no
longer ends the start. Core code: `internal/looper/quota.go`
(`bootstrapAndWait`, `waitBootstrapQuota`), the call in `initializeState`,
and the drain exit in `run.go`.

- Design choice: a startup wait, not a scheduled restart. The wait keeps
  the AX-022 readiness contract unchanged — the detached seed stays
  queryable through the whole window, the parent already reported ok, and
  the `quota`/`waiting` events stream like every journal line. A scheduled
  restart would need a new launcher owner and could not answer `looper
  stop` between exits.
- `bootstrapAndWait` retries bootstrap after each window. Each closure
  records the same provider pause the loop records (`profile`, `retry_at`,
  `reset_text`, `windows`); the pause is consumed on this goroutine (no
  loop boundary exists yet), so a later closure counts a fresh window. The
  pause budget mirrors `expireQuotaPauses`: past `LOOPER_RATE_LIMIT_MAX`
  windows without an integration the start ends for an operator with the
  provider's reset text and the T136 recovery action in the error.
- The wait watches three exits: cancellation (SIGINT) ends the start at
  once (exit 130); a drain request ends it cleanly — `bootstrapDrainExit`
  records the applied stop receipt in the saved state (the detached seed
  holds it), removes the request, journals `stop`, and the session records
  a drained exit 0; and a task file the operator supplies is adopted
  (bootstrap exists to create it), never overwritten. The adoption drops
  the bootstrap staging record of supplied plans, so the ordinary startup
  import analyzes them against the operator's backlog.
- The adoption path leaves the recorded pause open: the provider window is
  still closed, so the loop the adoption reaches gates review and planning
  on it until `expireQuotaPauses` reopens it. Untimed quota text and
  credentials never reach the wait; their classes still end the start.
- Every startup guard is untouched and still runs before bootstrap: plan
  validation, saved-identity checks, the missing-task-file refusal, the
  strict parse, and the seed-adoption logic. A drain that arrives during
  the bootstrap agent call itself (not the wait) still waits for the first
  loop boundary, exactly as `TestStopWaitDuringStartupDrainsAtFirstBoundary`
  pins.

Tests: `internal/looper/quota_startup_test.go` — the resume route (one
closure, one `quota-resume`, two bootstrap attempts, the once run completes
over the bootstrapped backlog), the budget boundary (2 closures, 1
`budget_spent` notice, exit 1 naming the budget and the reset text, no task
file), the drain (exit 0, drained session, applied receipt in the seed,
stop file removed), cancellation (exit 130, no task file), and the
operator-file adoption (file adopted with the plan rerouted to the ordinary
import, staging record removed, run completes after the window it still
waited out).

Validation, 2026-11-18:

- `go test ./internal/looper -run 'TestBootstrapQuota' -count=5`: pass;
  `-race -count=2`: pass.
- The T125/T136 quota routes, the AX-022 startup and stop-wait suites, and
  the bootstrap/daemon/import/legacy groups: pass.
- `go test ./internal/looper -count=1 -timeout 40m`: pass (630 s), and
  `go test ./... -count=1 -timeout 40m`: pass (looper 625.5 s).
- `go vet ./...`: pass. `gofmt` clean on every changed file.
  `go test ./internal/contractschema -count=1`: pass.
- `make build` then `python3 scripts/runtime-test.py`: 73 tests pass in
  283 s. The installer, package-smoke, and skill scripts of `make test` were
  not rerun; this change touches the engine startup path and documents only.

Limit left open, not fixed here: a task file that appears while the
bootstrap agent call itself runs (a window of minutes, unchanged by this
task) is still overwritten by the completed bootstrap result. The quota wait
— the window this task enlarged to hours — adopts it instead.

## Broken release-runbook symlinks removed, 2026-11-18 (T131)

- Commit `a09e08d` renamed `skills/release-runbook` to
  `skills/github-release` and left `.claude/skills/release-runbook` and
  `.codex/skills/release-runbook` pointing at the removed path. They were
  the only broken links in the tracked tree (`find . -xtype l` outside
  `.git`).
- Removed both links instead of restoring a target. Every script and
  document references `skills/github-release` directly, and
  `docs/install.md` calls `release-runbook` an old name that users remove.
  The emptied `.claude/` directory went with them; a fresh clone never
  had it. `.codex/skills/looper-live-test` stays: it is a real tracked
  directory that `scripts/skill-regression.py` exercises.
- Nothing referenced the links. A search over the Go sources, scripts,
  `Makefile`, and the installers finds no `release-runbook` or
  `.claude/skills` use.
- Validation: `find . -xtype l -not -path './.git/*'` lists nothing;
  `scripts/skill-regression.py`, `scripts/skill-install-test.py`
  (10 tests), and `go test ./internal/looper -run 'TestSkill' -count=1`
  pass. The remaining root links (`AGENTS.md`, `CLAUDE.md`) resolve.

## Lease read/validate warning grouping implemented, 2026-11-18 (T143)

Closes the T138 scope limit: the sweep's lease read and validate warnings
group per artifact exactly like cleanup failures. Core code:
`internal/looper/resources.go` (`repeatSweepWarning` replacing
`repeatCleanupFailure`, `clearLeaseWarning`, the kind-tagged
`cleanupFailureGroup`), one comment in `engine.go`. `run.go` is untouched —
the closing sweep in the `Run` defer already calls
`summarizeCleanupFailures`, which now summarizes these groups too.

- First occurrence reports exactly as before: the unchanged
  `resource-cleanup-error` notice (`Cannot read resource ownership in
  <path>: <err>` for a record that cannot parse or be read, the validation
  error text for an invalid record). Identical repeats count silently per
  lease directory, every `cleanupSummaryInterval` repeats and the session's
  closing sweep journal one `resource-cleanup-summary` event with the
  session's repeat count — `Cannot read resource ownership in <dir> after N
  identical warnings this session: <err>. The sweep retries at the next
  completion or restart.` / `Cannot validate resource ownership in <dir>
  …`. The cleanup-failure summary keeps its pinned T138 bytes.
- Group identity is (directory, kind, cause): a changed problem — unreadable
  becomes invalid, or the error text changes — reports at once because a
  changed cause is new information, and the new cause's count restarts.
- Recovery drops the group: a record that reads and validates again ends its
  episode (`clearLeaseWarning` after validation succeeds), so a later
  problem of the same artifact reports in full. Cleanup-failure groups
  survive that clear — only a cleanup run that succeeded clears them — or
  the clear would re-report a persistently failing cleanup at every sweep.
- Scope stays as the task records it: the read/validate paths journal no
  structured `failure` summary (T138 left them without one), and the
  walk-level error stays ungrouped because it is not artifact-scoped.

Tests: `internal/looper/resources_test.go` gained four cases (read repeats
with one notice, no failure summary, and one closing count; validate repeats
whose recovered record drops the group and whose new episode reports; the
interval bound with a changed problem reporting immediately; and the real
`Run` shutdown path through a stub-agent session), plus two lease fixtures.
The T138 suite is unchanged and passes: cleanup grouping keeps its exact
counts, and the pinned notice and summary messages still match.

Validation, 2026-11-18:

- `go test ./... -count=1 -timeout 40m`: pass (looper 631.5 s).
- `go test -race ./internal/looper -run 'TestRepeatedCleanupFailures|
  TestCleanupFailureInterval|TestCleanupFailureGroups|TestCleanupSuccessClears|
  TestRunSummarizesRepeated|TestCleanupFailuresDistinguish|TestWorkerCleansResources|
  TestCancelledCheckRetains|TestRepeatedLeaseReadWarnings|
  TestRepeatedLeaseValidateWarnings|TestLeaseWarningInterval' -count=1`: pass.
- `go vet ./...`: pass. `gofmt` clean on every changed file.
- `make build` then `python3 scripts/runtime-test.py`: 73 tests pass in
  305 s. The installer, package-smoke, and skill scripts of `make test`
  were not rerun; this change touches the engine sweep and documents only.

## Past quota reset clock and dated retry lines, 2026-11-18 (T145)

Fixes the T125 parse defect the backlog recorded: a provider reset clock
already past by seconds to minutes (provider clock skew or delivery delay —
the incident message read "reset at 11:37" at 11:39:28 local) rolled to
tomorrow and parked work for up to 24 hours. Core code:
`internal/looper/agent.go` (the tolerance), `internal/looper/quota.go`
(the dated lines).

- `quotaResetTime`'s clock branch keeps a reported clock that is past by
  at most `quotaResetPastTolerance` (10 minutes) on today instead of
  rolling it to the next day. The past value reaches `pauseOnQuota`,
  whose existing "zero or already passed" branch applies the
  rate-limit fallback wait (`LOOPER_RATE_LIMIT_DELAY`), so the pause
  reopens imminently. A clock past beyond the tolerance still names
  tomorrow, and dated timestamps keep their reported value — the same
  past-value shape a zone-free stamp already produced.
- `quotaRetryClock(retry, now)` renders every human quota line: the clock
  alone while the retry lands today (byte-identical to the old text), and
  `2006-01-02 15:04:05` — the layout the T136 recovery action already
  uses — the moment it does not. Wired into the three lines the finding
  named: the `quota` pause event, the in-run waiting line
  (`quotaWaitMessage`), and the bootstrap wait line
  (`waitBootstrapQuota`). Payload keys and the RFC3339 `retry_at` value
  are unchanged.
- Scope kept tight: `quotaRecoveryAction` and the held attempt's
  `UnblockWhen` keep rendering a parsed past reset as reported (the
  pinned T136 fixture already carries a yesterday stamp there); the
  pause event names the deadline the controller actually honors.

Tests: `TestQuotaResetTimeShapes` gained the minutes-past and
tolerance-boundary shapes; `TestQuotaRetryClockNamesTheDateWhenNotToday`
pins the renderer; `TestQuotaPastClockReopensImminently` is the incident
route (one pause, retry_at one fallback wait after the event, resume,
completion); `TestQuotaMessagesNameTheDateWhenRetryIsNotToday` and
`TestBootstrapQuotaWaitNamesTheDateWhenRetryIsNotToday` pin the dated
lines through the real loop and startup paths. With the tolerance
temporarily zeroed, the shape and route tests fail on the rolled date and
the parked task, so the coverage catches the defect class.

End-to-end with the built CLI (stub bootstrap failing once, private base
directory under the task resource prefix): the incident message
"…Your limit will reset at 12:26" read at 12:28:50 local journaled
`retry_at` nine seconds out (the configured fallback), printed "the start
waits and retries at 12:28:59" with no date, and the start completed in
15 s. A reset reported 25 hours out journaled tomorrow's deadline and
both the pause event and the bootstrap wait line named the date;
`looper stop --wait` still drained the start cleanly.

Validation, 2026-11-18:

- `go test ./internal/looper -count=1 -timeout 40m`: pass (790.6 s).
- The quota, failure, events, legacy-contract, bootstrap, daemon, and
  stop-wait groups named in the T136/T141 notes: pass.
- `go vet ./...`: pass. `gofmt` clean on every changed file.
- `go test ./... -count=1 -timeout 40m`: see the full-suite line in the
  task report.

## Dashboard figure defects fixed, 2026-11-18 (T132)

Both defects recorded on 2026-11-15 while re-recording the README figures
are fixed in `internal/ui`.

- Task-view cursor. The workspace selection materialized from the head of
  the first task list and then never moved, so a task that blocked later
  and sorted first left `u` opening on the stale second row. An unset
  selection is now implicit: the overview marker and the task table follow
  the ordered head on every snapshot. Navigation and `enter` (inspect) pin
  the selection, and a pinned selection still survives reordering until
  its task disappears, so `preserveTaskSelection` and its snapshot/tick
  call sites are gone.
- Attention-row blank lines. A blocker is free agent text and can span
  lines. `ansi.Truncate` in charmbracelet/x v0.2.3 stops collecting
  printable cells at the width limit but keeps writing non-printable
  bytes, so every newline survived as a blank display line: a blocker
  wider than the terminal rendered its first line, the ellipsis, then a
  column of blank lines. `taskRow` now flattens free text to one display
  line (`plainText` plus whitespace collapse) before the width clamp,
  matching how every other row builder sanitizes agent text. The full
  blocker stays readable in the inspector.

Verification, 2026-11-18:

- New pinned tests `TestWorkspaceTaskViewOpensOnSortingHead` and
  `TestWorkspaceAttentionFlattensMultiLineBlocker` in
  `internal/ui/workspace_test.go`; both fail on the pre-fix code.
- `go test ./internal/ui/... ./internal/contractschema/... -count=1`:
  pass. `go test ./internal/looper -run 'Test(Dashboard|Attachment|
  Observer|Compact|Status|Inspect)' -count=1`: pass. `go vet`, `gofmt`
  clean.
- Live: `scripts/record-demo.py` copied under the task resource prefix
  with a 12-line blocker and a private tmux socket re-ran the recorded
  flow against the rebuilt binary. The attention row renders one line
  with a single blank line before Scheduling, and after `u` and `k` the
  cursor and detail pane sit on T004, the blocked task that sorts first.

## Quota pause restored across restarts, 2026-11-18 (T146)

Closes the T125 restart gap: a controller stopped inside a quota window
re-arms the pause at startup, so the held task's recorded promise holds
across the restart instead of stranding the work for an operator requeue.
Core code: `internal/looper/quota.go` (`restoreQuotaPauses`), startup hook
in `internal/looper/engine.go` (`initialize`).

- The defect: `resumeQuotaTasks` ran only from `expireQuotaPauses`, which
  reads the session-memory `quotaPauses` map initialized empty in
  `NewEngine`. A restart mid-window left the held task blocked, and a
  blocked task is never dispatched, so no provider failure could re-arm
  the pause — the controller idled at "Waiting for changed requirements"
  past the recorded retry until `looper requeue` rescued it. The T125
  note above carries the erratum.
- The fix reads the record the held task already persists: the typed
  `quota_window` block reason, the blocked attempt's agent profile, and
  the `unblock_when` promise `holdQuota` wrote
  (`quotaRetryFromUnblock` parses the RFC3339 stamp after the fixed cue,
  period trimmed). One pass groups holds by profile and arms
  `providerPause` entries before the loop starts, so the ordinary
  boundary machinery — expiry, dispatch gate, waiting line, budget —
  behaves exactly as in the session that recorded the pause.
- A window whose recorded time already passed resumes at the restart's
  first boundary (the evidence scenario: stopped 11:49:28, restarted
  11:50:43 after reopen). A promise without a time ("when the provider
  window reopens") requeues at startup, because the saved state cannot
  say whether the window is closed; a still-closed window re-arms from
  the provider's next failure, the discovery dispatched work always had.
  The restore keeps `pauseOnQuota`'s 24 h daily cap and starts a fresh
  pause-budget cycle (`consecutive: 1`): the previous session's window
  count lived only in its memory.
- `quotaHoldsWork` now also holds the idle boundary while any profile's
  pause has blocked quota work, not only ready work, and
  `quotaWaitMessage` names every paused profile that holds work — so the
  controller reports the quota line with its retry clock instead of the
  generic changed-requirements line while quota-held work remains.
- Journal: one `quota-restore` event per affected profile, payload keys
  `profile`, `retry_at`, `windows` (1), with `requeued: true` marking the
  no-recorded-time variant. `resumeQuotaTasks` was split into
  `requeueQuotaHeld` (the transition) plus its events; the transition
  itself is unchanged. The restore is idempotent behind `quotaRestored`,
  because `Run` initializes again behind harnesses that pre-install a
  plan.
- Scope: reviewer-side pauses still self-heal on restart, because a
  pending review is dispatched and its failure re-arms the pause; only
  blocked tasks needed the record. Operator and deferral control tags
  stay authoritative — a restored window never reopens those holds.

Tests: `internal/looper/quota_restart_test.go` — the mid-window restart
acceptance route (restore keeps the recorded `retry_at`, quota wait line
while held, no generic waiting line before the resume, one resumed
attempt), the restart-after-reopen route, the no-recorded-time requeue,
the promise parse table, and the boundary-gate unit case.

Validation, 2026-11-18:

- Live, built CLI on a private fixture (stub worker failing once with a
  75 s window): session 1 held T012 blocked with reason `quota_window`
  and promise "retries automatically at 13:29:01+01:00"; stopped
  13:27:58 mid-window; session 2 restarted 13:28:08, journaled
  `quota-restore` with the same retry, waited on the quota line, and at
  13:29:01 requeued and dispatched T012 automatically — done at 13:32:14
  with no operator requeue; "Waiting for changed requirements" appeared
  only after the work completed. (The stub's mark file briefly pointed
  at a moved directory, so the worker re-failed twice more after the
  resume; the journal shows those windows re-arming and expiring
  in-session on top of the restored pause, which is the designed
  budget-bounded behavior.)
- `go test ./internal/looper -run 'Quota' -count=1`: pass.
- `go vet ./...`, `gofmt` clean on changed files.
- `go test ./... -count=1 -timeout 40m`: pass (exit 0); the looper
  package was rerun alone on the final tree after a behavior-preserving
  cleanup inside `restoreQuotaPauses`: pass (663 s, exit 0).

Docs updated: `docs/observability.md` (restart paragraph, monitor row),
`docs/AGENT-CONTRACT-MIGRATION.md` (change-log row).

## Tracked reference schema implemented, 2026-11-19 (T117)

A project can now declare a tracked reference schema with
`LOOPER_SCHEMA_REFERENCE=<repo-relative path>` (for example
`docs/to-do.schema.json`). The controller resolves the declaration in
`OpenRepository` (`Repository.SchemaReference`) and respects it in both
places that transport the control-file schema:

- `Repository.Workspace` seeds every assembled tree from the reference
  bytes directly (`schemaSeed`), so a drifted run-root copy can never
  reach a worktree while the declaration is in effect — not even a copy
  that drifts again after startup.
- `Engine.reconcileSchemaControlFile` (called from `initializeState`
  before bootstrap or resume assembles anything) refreshes the run-root
  control file to the reference bytes whenever they differ, recording one
  `schema-refreshed` journal event. Without a declaration both sites keep
  the earlier behavior exactly: the built-in default is written only when
  the file is absent, and an existing copy is never touched.

Without a declaration nothing changes; with one, a declaration that names
a path outside the repository (refused in `Parse` with
`schema_reference_invalid`), a control or runtime path, a symlink, a
directory, a missing file, or invalid JSON (refused in
`setSchemaReference`) fails before any seed honors it halfway. The
declaration is per controller environment: every CLI invocation that
assembles or publishes (publish, recovery, settings) should carry the
same variable, or it simply sees no declaration.

Tests: `internal/looper/schema_reference_test.go` — startup refresh with
the journal event, worktree seeding from tracked content beside the
checked-out reference, mid-run root drift held out, undeclared behavior
pinned byte-for-byte, the refusal table, and the `Parse` env handling.

Validation, 2026-11-19:

- Live, built CLI on a private fixture (stub agent failing fast): with
  the declaration, startup journaled `schema-refreshed` and the drifted
  39-byte control file became byte-identical to the committed 2,257-byte
  reference; a second declared run recorded no further refresh; a
  declaration naming the control file itself and an absolute path both
  refused with their teaching messages before any agent ran.
- `go vet ./...` and `gofmt`: clean. Every package except the looper
  package: pass. `go test ./internal/looper -count=1 -timeout 40m`:
  pass (775.8 s). A first full-package run under heavy machine load
  (1,526 s, 2.3× the usual duration) failed only
  `TestWorkerQuotaPauseResumesAutomatically` and
  `TestPlanningQuotaPauseResumes`; both pass `-count=3` in isolation and
  the whole quota family passes repeatedly — the known timing-sensitive
  quota-resume routes (see the T136 notes above), not this change.
- The adapter suite `node --test adapters/measuretwice/adapter.test.mjs`
  cannot run in this worktree: `node_modules` was never installed
  (`Cannot find package 'measuretwice'`). Pre-existing environment gap;
  no adapter code changed.
- This worktree's own runtime copy at the root (`to-do.schema.json`,
  ignored, untracked) was the stale 1,667-byte pre-cancellation schema
  the T117 evidence recorded; refreshed it to the shipped 2,257-byte
  default. A future controller started with
  `LOOPER_SCHEMA_REFERENCE=docs/to-do.schema.json` now performs that
  refresh itself.

Docs updated: `docs/configuration.md` (the env var row and a paragraph on
the control-file contract), the `backlogSchema` comment in
`internal/looper/types.go`.

## Config show settings line separator fixed, 2026-11-19 (T133)

`looper config --show` ran the effective-settings value into the next
sentence: `renderConfigShow` wrote `EffectiveSettings(c)` with `fmt.Fprint`
and the string carries no trailing newline, so the output ended
`... review frequency 5 [profile]No controller owns this repository; ...`
on one line. Core code: `internal/looper/config_cmd.go` (one `Fprintln`).

- The fix terminates the line at the render site. `EffectiveSettings`
  keeps its no-newline contract; the interactive editor path
  (`settings.go`, `fmt.Fprintln(out, "Effective settings:", ...)`) already
  terminates the line and is unchanged. The machine-scope text and every
  other config renderer already ended their lines; a scan found no other
  run-on in the file.
- The version 2 JSON document is untouched (only the plain-text renderer
  changed), so the config schema fixtures stay byte-identical.
- Regression test `TestConfigShowTextKeepsTheSettingsLineSeparate` pins
  both cases the finding names: exact whole lines for the label, the
  no-profile sentence, and the controller sentence, plus the saved-profile
  case where `[profile]` values reach the effective line. It fails on the
  unfixed code with the finding's exact run-on shape.
- Live, built CLI in a scratch project under the task resource prefix:
  the no-profile case prints the settings value, `No saved profile
  exists.`, and the controller sentence on three lines; the saved-profile
  case ends the settings value at `review frequency 2 [profile]` with the
  controller sentence on its own line; `--contract v2 --json` serves the
  unchanged document.

## Quota reset cue parsing restored, 2026-11-20 (T148)

Fixes the regression the pass-4 review of `7ca99ed` recorded
(`looper:finding:quota-reset-cue-parse-regression`): `quotaResetTime`
(`internal/looper/agent.go`) returned the first full datetime found
anywhere in a quota-classified message, so a message carrying an incident
stamp beside its reported reset paused until the wrong time. T142 wrote
the cue-governed parse (commit `db6f073` on
`refs/looper-tasks/task-cbf679c7f38a5b70`), but that commit was never
integrated — T145 and the quick-wins landing `41433c1` were written
against the tree without it, and T142 was then cancelled on the premise
the parsing had landed.

- The T142 implementation is recovered verbatim: `quotaResetCue` matches
  one reset cue (`reset`, `resume`, `retry`, `reopen` — the words the
  clock reading always required) and the stamp or clock time it introduces
  on the same line within 32 characters; `quotaStampTime` and
  `quotaClockTime` hold the per-value parsing. Order: cued value, relative
  delay, then the first-full-datetime fallback for a message with no cue
  wording (the T125 reading, kept for every message that reports only its
  reset).
- One deliberate merge with T145, which landed after T142 was written and
  is pinned by its own tests: the ten-minute past tolerance stays inside
  `quotaClockTime`, so a reported clock minutes past keeps today and the
  pause's rate-limit fallback reopens it. Restoring `db6f073` byte for
  byte would have reverted that accepted fix.
- Tests recovered with it: the seven shape rows in
  `TestQuotaResetTimeShapes` (both evidence orders, request and
  maintenance stamps beside cued resets, uncued values below a cued
  clock, a bare uncued stamp, a relative delay outranking a distant
  uncued stamp) and `TestAgentQuotaResetPrefersCuedResetTime` through a
  stub agent.
- Acceptance probe from the finding, run both ways: input
  `usage limit exceeded at 2026-11-20T09:00:00+01:00; your quota resets
  at 2026-11-20T23:55:00+01:00` returns 09:00 on the pre-fix tree (the
  recorded FAIL) and the cued 23:55 on this tree.
- Limit T142 recorded stands: a stamp that happens to sit within 32
  characters after a cue word still answers for that cue; telling those
  apart needs provider sentence structure no observed message supplies.

Validation, 2026-11-20:

- `go test ./internal/looper -count=1 -timeout 40m`: pass (1 719.9 s under
  host load). `go vet ./...`: pass. `gofmt` clean on every changed file.
  `go test ./internal/contractschema ./internal/ui -count=1`: pass.
- `go test -race ./internal/looper -run 'Quota' -count=1`: pass (128.9 s).
- Live, built CLI on a private fixture under the task resource prefix
  (stub worker failing once, incident stamp one hour back, cued reset
  eight seconds out): the journal `quota` event recorded `retry_at`
  exactly the cued reset — never the incident stamp — the window expired
  with the quota wait line, `quota-resume` requeued T1, and the second
  attempt completed the work (T1 done, no blockers).

## Config unset review_every fixed, 2026-11-20 (T149)

Finding, `looper:finding:config-unset-review-every`: `looper config
--unset review_every` (plain and `--preview`) exited 2 with
`review_every must be a nonnegative integer; "" is not`, while
`--unset jobs` exited 0 and saved. Two defects stacked:

- `projectPatchFields` coerced the empty unset value to `"0"` only for
  `jobs`, so an unset `review_every` failed numeric validation and the
  removal branch in `applyProjectPatch` stayed unreachable. The coercion
  now runs for every numeric setting the patch unsets.
- Even with the removal branch reachable, the removal could not persist:
  `Profile.MarshalJSON` wrote a hardcoded `review_every: 5` whenever the
  pointer was nil, so every saved profile carried the key and an unset
  re-baked the default as a saved value. The marshaler now omits the key
  for a nil pointer, matching the struct tag and the load semantics
  (absent key → nil pointer → built-in default). Every form-based writer
  sets a non-nil pointer through `ProfileFromSettings`, so only patch
  results can omit the key.

Behavior after the fix, probed with the built CLI in a scratch project:
plain and preview unset exit 0, the outcome line reads
`review_every: 7 -> <nil> (saved)`, the saved profile drops the key, and
`config --show` reports `review frequency 5 [default]`. `--unset jobs`
is unchanged (jobs 0 is auto mode, a saved value, not a removal).

Known edge, recorded not fixed: under a live controller the settings
request protocol carries assignments only, so an unset submits
`review_every: 0` and the controller saves `review_every: 0` (periodic
review disabled) rather than removing the key (default cadence). That
matches how `--unset jobs` behaves on the live path and predates this
fix; expressing a per-field removal in the request protocol is a
protocol change outside this task.

Tests: `TestConfigUnsetNumericProjectSettings` covers preview unset
(removal reported, nothing written) and plain unset beside `--unset
jobs` (key gone, jobs 0, journal done), with both documents checked
against the v2 config schema.

Validation, 2026-11-20:

- `go test -timeout 55m -count=1 ./internal/looper`: pass (775.4 s; the
  Go 10m default panics mid-suite on this fsync-bound package, so every
  full-suite run needs the Makefile's 30m floor or better).
- `go test -count=1` over every other package (contractschema, ui):
  pass. `go vet ./...`: pass. `gofmt` clean on every changed file.

## Controller commits use the configured Git identity, 2026-11-20 (T151)

Finding, `looper:finding:controller-commit-identity-hardcoded`:
`Repository.Commit` (`internal/looper/git.go`) hardcoded the
`GIT_AUTHOR_*` and `GIT_COMMITTER_*` variables to
`Looper <looper@localhost>`, so every controller commit made after the
history scrub carried a machine identity the scrub decision did not choose
(11 unpushed commits on main carried it at the finding).

- `commitIdentity` resolves the identity through `git config --get
  user.name` / `user.email` — the same merged-configuration read
  `collectPublishEvidence` uses — so the repository's configured identity
  reaches every controller commit: snapshot, complete, retain-partial,
  integrate, inspect, baseline refresh, and the branch-recovery seed. A
  complete configured identity wins (author and committer); the
  deterministic `Looper <looper@localhost>` pair is the fallback only when
  nothing complete is configured, because Git refuses an empty ident and a
  controller must integrate work with no operator present. A partial
  configuration falls back whole, mirroring the publish guard's
  completeness rule instead of mixing a configured half with a synthetic
  one.
- `Merge` carried a second hardcoded copy (`-c user.name=Looper -c
  user.email=looper@localhost`). `git merge --no-commit` creates no commit,
  but Git refuses to start it without an identity (verified: `fatal: empty
  ident name … not allowed`), so those flags were operational. The merge
  now rides the same resolved identity through environment variables; no
  commit records it.
- Tests: `internal/looper/git_identity_test.go` — the configured local
  identity, the harness-global identity (the scope walk), the
  deterministic fallback with every scope empty, a partial configuration
  ignored, and the merge path on an identity-free repository (the merge
  must run, stop at its base, and still deliver the content). The two
  configured-identity tests fail on the pre-fix helper with the finding's
  shape; the merge test fails with Git's empty-ident refusal when the
  identity environment is dropped.
- Live, built CLI on private fixtures under the task resource prefix: with
  local `user.name`/`user.email` set, Snapshot, Complete, and Integrate
  carried the configured pair as author and committer and the accepted
  work still landed as unstaged edits; with `GIT_CONFIG_GLOBAL` and
  `GIT_CONFIG_SYSTEM` pointed at `/dev/null` and no local identity, the
  same three commits carried `Looper <looper@localhost>` and the run
  completed; an unset-variable probe confirmed the host global scope is
  honored (`reviewer <reviewer@example.com>` appeared).
- Open operator items, not fixable here: this repository resolves no local
  identity and the host global is `reviewer <reviewer@example.com>`, not
  the scrub-decided `nibzard <nibzard@users.noreply.github.com>`, so the
  owner records the decision with `git config user.name nibzard` and
  `git config user.email nibzard@users.noreply.github.com` in the
  repository (workers do not edit repository configuration), and the 11
  unpushed commits ahead of `origin/main` still carry the machine
  identity, which only an owner-side re-author or acceptance can settle.

Validation, 2026-11-20:

- `go test ./internal/looper -count=1 -timeout 40m`: pass (765.5 s).
- `go vet ./...`: pass. `gofmt` clean on every changed file.
- `go test ./internal/contractschema ./internal/ui -count=1`: pass.
- `go test -race ./internal/looper -run 'TestControllerCommit|
  TestMergeRunsOnARepositoryWithoutConfiguredIdentity|TestPublish|
  TestIntegrate|TestPrepare' -count=1`: pass.

Repair round 1, 2026-11-20: the combined check failed `make test` on a
stalled host, not on the identity change. The check run's own goroutine
dump showed one fsync inside a recovery subtest stalled for minutes while
the suite kept progressing, and the two recorded failures were both
fsync-stall artifacts: `TestQuotaPastClockReopensImminently` expired a
×3-scaled 30 s poll while T1 was still mid-flight, and the suite alarm
(a fixed `GO_TEST_TIMEOUT ?= 30m`) aborted a slow-but-progressing suite
at 1800 s. The host read PSI "full" avg60 ≈ 85 percent both during the
check and live during this repair, the worst pressure on record
(`loadbudget_test.go`'s cap was calibrated at 65).

- `quotaWait` now scales its bound against the pressure the host reports
  while the wait runs, not only at entry: the 60 s pressure average lags
  a burst, so a stall that arrives mid-wait must extend an already
  started wait the way `testBudget` sizes a new one.
- The past-clock test's terminal wait raised from a 30 s base to 90 s: it
  covers the longest chain in the file (pause, fallback, resume, requeue,
  the worker's second run) and a live reproduction at PSI ≈ 85 expired
  the ×5.9-scaled 30 s bound at 232 s with T1 verifiably progressing
  ("doing", not parked). The imminence itself is asserted from the
  journaled `retry_at` against the pause time, which do not stretch with
  the host; only the failure bound moved.
- `GO_TEST_TIMEOUT` now starts at 30 m and grows with the same pressure
  anchors through `scripts/go-test-timeout.sh` (floor 30 m, cap 180 m,
  the six of `maxBudgetFactor`), so a progressing suite on a stalled host
  runs to completion while a wedged one still fails inside three hours.
  An explicit `GO_TEST_TIMEOUT=...` still bounds a run on demand.

Repair validation, 2026-11-20, live at PSI "full" avg60 80-90:

- `TestQuotaPastClockReopensImminently`: pass (175.4 s) with the widened
  bound, after the reproduced failure at 232.6 s.
- `TestRecoveryApplyUnsupportedChoicesRetainWork`: pass (149.0 s, six
  subtests) — never failing, only slow.
- Identity and budget tests: pass; `go vet ./internal/looper`: pass;
  `gofmt` clean.
- Full `make test` under the scaled budget (resolved to 116 m at
  launch): completed with the two further stall failures recorded in
  the continuation below; the quota and recovery tests named in the
  check failure both passed inside it.

Repair round 1 continued, 2026-11-20: a first full `make test` under
the scaled budget (116 m) confirmed the quota and recovery repairs —
both recorded failures passed — and surfaced two more sites of the same
stall class, both deadline bounds around one engine run:

- `TestAddTaskRequestWhileWorkerRuns` had already scaled its context
  (testBudget(30 s)) but the bound was sized once at entry and pressure
  deepened mid-run: expired at 105 s with the run healthy.
- `TestCompletedCodexRunWithOversizedLogAppliesResult` used an unscaled
  fixed 30 s context: expired at 35 s while the worker pushed 100 MiB
  through a stalled pipe.

A context deadline cannot grow after creation, so `runCtx`
(`loadbudget_test.go`) cancels from a watchdog that re-sizes the bound
against live pressure every second — the same semantics quotaWait now
applies to polls. The three evidenced sites converted (the add-task
run, the oversized-log run, and the `runClaude` helper's minute); a
deadline a test forces as its mechanism (an interruption the run is
expected to hit) stays fixed and is not a conversion candidate. All
converted tests passed live, as fast as the eased host allowed (9.6 s,
4.8 s, sub-second Claude cases).

27 further fixed `context.WithTimeout` failure bounds remain
across the suite (count: `grep -rn "context.WithTimeout"
internal/looper/*_test.go | grep -v testBudget`). Each is a latent
flake of this class; converting them needs the per-site judgment above,
so the class is reported rather than swept blindly here.

Final validation, 2026-11-20: full `make test` under the scaled budget
(resolved to 39 m at launch on the eased host): pass. `go test ./...`
green with internal/looper at 600.5 s, `go vet` pass, and the install,
skill, build, runtime, package-smoke, and skill-regression steps all
passed. The gate is green end to end with the identity change and the
stall repairs in.

## MIT license added, 2026-11-20 (T129)

The owner's 2026-11-18 decision (MIT) is now in the tree:

- `LICENSE` at the root carries the canonical MIT text, copyright
  `2026 nibzard`, the identity every post-scrub commit already carries.
- The README states the license in a new License section.
- `Formula/looper.rb` declares `license "MIT"`. Homebrew audit expects
  the stanza, and the next release re-cuts the formula checksum anyway.
- `docs/release-notes/0.12.0.md` drafts the release note. The version
  number is provisional until the release decision; rename the file if
  the next release picks another number.

No per-file headers: MIT does not require them and the tree has no
header convention, so the repository-wide `LICENSE` file is the whole
grant. T153's operator prerequisite completed when the owner recorded
the choice in T129's details; marking T153 done stays with the backlog
owner, because workers do not edit the backlog.

## Post-scrub private-form regressions fixed, 2026-11-20 (T152)

Three live sites had reintroduced forms the 2026-11-19 scrub removed:

- The T128 review-cost fixture comment named a private project where it
  meant the Charlie codename; `reconcile_test.go` already used the
  codename for the same T077 reference.
- The agents guide's `glm` example named the private provider command
  instead of `claude-alt` (e7c985c over-reverted the line). The profile
  catalog in `docs/AGENT-PROFILES.md` already showed the public form.
- The scrub record's backup-bundle path named the private home instead
  of `/home/user`.

All three carry the public forms now. The mapping bullets in the
History scrub section above still name both sides of each
transformation, as intended.

`scripts/scrub-check.py` keeps new content from reintroducing the
private forms: `make scrub-check` (and the first step of `make test`)
fails when tracked content carries them again, case-insensitively. The
check exempts History scrub lines that name a mapping itself, and it
never matches the public `zai-org` catalog slugs restored in
`data.json` because it searches for longer private forms only. The
private literals are assembled from fragments inside the script, so
the script itself does not carry them; the other private project names
stay out of the tree entirely, so the guard covers only the three
forms this repo's own record names or fixed.

## Live settings removal for review_every, 2026-11-20 (T154)

Closes the T149 known edge, `looper:finding:config-live-unset-removal`:
under a running controller, `config --unset review_every` submitted
`review_every: 0` and the controller saved 0 (periodic review
disabled) while the stopped path removed the key (default cadence 5).

- Protocol change, additive within envelope version 1: a settings
  request may carry `null` for exactly one field, `review_every`
  (`removableSettingKey`). `applySettingsRequest` accepts it and
  `patchProfile` turns it into a removal (`p.ReviewEvery = nil`), so
  the saved profile drops the key and the built-in default applies.
  Every other field keeps the unchanged `must not be null` refusal,
  because its zero value is a saved state (empty string, jobs 0 auto).
- `projectPatchFields` renders an unset `review_every` as `null`; the
  jobs unset keeps `0`. The removal branch that T149 added after
  `patchProfile` is gone: null now flows through one code path, so the
  stopped, preview, and live paths share the patch renderer, the
  profile applier, and the payload digest.
- A numeric `--set review_every=null` stays refused (one removal
  spelling). The interactive form and the dashboard still diff numeric
  snapshots through `changedFields`, so they assign numbers as before.
- Compatibility: an older controller refuses the null envelope with
  `review_every must not be null` and changes nothing; an older client
  submitting 0 still assigns 0. An idempotency key reused from the old
  `0` submission conflicts, as any changed payload does. The ack
  outcome is `saved; applies on the next controller run`; the
  effective cadence keeps its snapshot mid-run.
- Tests: `TestApplySettingsRequestRemovesOptionalReviewEvery`,
  `TestApplySettingsRequestRefusesNullOutsideOptionalKey` (protocol),
  `TestProjectPatchFieldsRemovalSpelling` (renderer), and
  `TestConfigLiveUnsetReviewEveryRemovesSavedCadence` (live round trip
  on the lock-and-metadata harness: envelope carries null, profile
  drops the key, ack applied). All four fail on the pre-fix tree;
  `TestConfigUnsetNumericProjectSettings` (T149) keeps pinning the
  stopped path.

Validation, 2026-11-20:

- Live, built CLI on a private fixture under the task resource prefix
  (stub agent, detached controller): stopped set saved
  `review_every: 7`; live `config --unset review_every --wait`
  applied; the saved profile dropped the key; a fresh invocation
  reported `review frequency 5 [default]`; the durable receipt read
  `applied_fields: [review_every]`,
  `restart_required_fields: [review_every]`; `stop --wait` drained
  cleanly.
- `go test ./... -count=1 -timeout 59m`: pass (internal/looper
  759.5 s). `go vet ./...`: pass. `gofmt` clean. `make scrub-check`:
  clean. `-race` on the settings/config-live families: pass.
- `make build` then `python3 scripts/runtime-test.py`: 73 tests pass
  in 315.8 s. The installer, package-smoke, and skill-regression steps
  of `make test` were not rerun; this change touches the settings
  protocol, the config command, and documents only.

## Fixed test-deadline bounds converted to runCtx, 2026-11-20 (T155)

Closes the leftover the T151 repair round reported: the 27 fixed
`context.WithTimeout(context.Background(), <fixed>)` failure bounds in
`internal/looper/*_test.go` are now pressure-scaled `runCtx(t, base)`
bounds. Count after the change:
`grep -rn "context.WithTimeout" internal/looper/*_test.go | grep -v
testBudget` returns 8, all mechanism sites.

- 19 grep sites converted (20 bounds: the transport site split its two
  arms): event_identity, context_live (`runContextEval`, the opt-in
  paid comparison), import_recovery (both crash-helper children),
  recovery (`testUnattendedRecovery`), resume (the second-attempt
  run), retained_merge (all four), operator_workflow (`runEngine`),
  detect, noanimation, failure (`cleanupAuditCase`), transport (the
  missing-report arm), request_lifecycle (the crash helper),
  verbose (both), publication_snapshot, and requeue.
- Per-site judgment, applied to the rule and not only to the task's
  illustrative file list:
  - The four retained_merge bounds are failure guards, not mechanisms:
    no retained_merge test asserts `DeadlineExceeded`, the healthy run
    or reopen ends on its own, and an expired deadline fails the test
    ("worker did not finish", or exit 130 against the expected blocked
    exit 2). The task text named retained_merge among mechanism files,
    but those four sites never force the deadline, so they convert.
  - The transport hang arm does force its deadline (it asserts
    `DeadlineExceeded` after a report that never comes) and the task's
    file list omitted it. The site is split: the hang arm keeps a
    fixed 200 ms context; the missing-report arm converts its 5 s
    guard to `runCtx`.
- Kept fixed, each a deadline the test itself forces:
  `TestIdleControllerDoesNotPlanWithoutWork` (dashboard),
  `TestOverdueReviewSurvivesCancellationAndRestart` (review_recovery),
  both memory throttle tests,
  `TestNilPlanRecoverySurvivesCancellationAndRestart`,
  `TestRunResumesOneOfTwoOverlappingRetainedAttempts` (resume), and
  `TestTimingRecordsProcessOutcomes` (200 ms over a `sleep 10`
  command). Each asserts `DeadlineExceeded` or reads the cancellation
  as its outcome.
- Four files dropped their `context` import after the conversion
  (context_live, retained_merge, detect, noanimation).

Validation, 2026-11-20:

- All 27 affected tests plus the seven unchanged mechanism tests pass
  together (`go test -run` over the full set, 34.1 s).
- Full `internal/looper` package suite under the scaled budget: pass
  (594.1 s, exit 0; the first attempt was stopped by the runner's
  10-minute default background limit, not by a test failure, and the
  rerun with a longer window completed). `go vet ./internal/looper`:
  pass. `gofmt` clean. `go test ./internal/contractschema
  ./internal/ui`: pass. `make scrub-check`: clean.

## Worker reconciliation enabled, 2026-11-20 (T139)

Closes the T124 limit `looper:finding:worker-reconciliation-source`: a worker
summary's follow-ups can now carry the reconciliation transitions, so reviews
are no longer the only source.

- The agent summary schema (`internal/looper/agent.go`, the structured-output
  contract each adapter enforces) gains optional `transition` and `revision`
  on each follow-up object. Before this, `additionalProperties: false` made a
  worker's report of those fields invalid before it ever reached the engine.
- The revision guard splits by source (`findingStage.verifiedRevision`,
  `internal/looper/findings.go`). A review still names the accepted revision
  its prompt taught (`e.State.Tip`, unchanged; the existing
  `TestReconciliationRefusesStaleRevision` keeps passing). A worker names the
  base its worktree assembled from, and that base counts only while the
  accepted revision still
  is that base (a blocked report with nothing integrated since dispatch) or
  the verification commit that attempt's own verified work became on top of it
  (`a.Verification.Commit` with `a.Verification.Base == a.Base`, the completed
  path after `publish`). A check that merged another attempt's work first —
  `a.Verification.Base != a.Base`, the chained-candidate case — refuses the
  report: the tree moved past what that worker read. The refusal note teaches
  the revision the worker should have carried, or names the unread work.
- `applyResult` opens `beginWorkerStage(a)`, so the guard reads the reporting
  attempt. Review and skip paths keep `beginFindingStage`. A retained result
  replays with its saved base and verification, so a restart re-applies the
  same transition exactly once.
- The worker prompt (`workerPrompt`, now taking the attempt base) names the
  base revision and teaches both fields. The 4 KiB budget the long-goal
  scenario pins (`TestWorkerPromptRefersToCompleteGoalWhenLong`) had 180 bytes
  of headroom while the teaching costs about 200, so three template phrases
  were shortened without dropping an instruction: "in your implementation" out
  of the ignore-rules line, "until recovery completes" → "for recovery", and
  "The registered Docker cleanup can then find each owned container" → "The
  Docker cleanup then finds each owned container". The scenario measures
  3 916 bytes before and 4 070 after (26 spare), and a new test pins both the
  teaching and the budget.
- Docs: `docs/tasks-and-checks.md` records both report sources and the guard;
  `docs/AGENT-CONTRACT-MIGRATION.md` carries the dated row. The frozen status
  projection already cleared both keys from `attempts[].summary.followups[]`
  (T124), so no served document or fixture changes.

Tests: `internal/looper/worker_reconcile_test.go` — resume from the base on a
blocked report, resume after the attempt's own verification, refusal for a
baseline the attempt never read, refusal for a revision it cannot know, a
baseless synthetic attempt refused, the review guard unchanged, the schema
enum, the prompt teaching inside the budget, the full run through the fake
worker agent (the stub copies the base revision out of its own prompt and the
resumed task then runs to completion), and the retained-state replay.

Limits kept, not fixed here:

- A worker whose check chained behind another attempt's candidate (the
  `checks[len-1]` base in `Run`) reports against a base the controller has
  already moved past; its reconciliation is refused and the next review
  reconciles. Widening that needs reasoning about co-worker diffs the worker
  never saw.
- The prompt still teaches no `defer_until` on a reconciliation; that
  combination is refused by `add`, as before.

Validation, 2026-11-20:

- Live, built CLI on a private fixture under the task resource prefix (stub
  worker that copies the base revision out of its own prompt): T1's report
  carried `transition: resume` with its worktree base, and the journal
  recorded `T2  Reconciled resume at <verification commit> ... Evidence: T1
  wrote the input file at <base>`; T2 then dispatched and completed; the final
  review closed the goal; exit 0; one `Reconciled resume` and zero `Ignored
  reconciliation` events. The saved attempt retains the proposal (transition
  plus the base revision) beside its base and verification, and plain
  `status --json` shows the retained follow-up with neither key, so the frozen
  projection holds. A second fixture whose stub reported a foreign 40-hex
  revision was refused: `it verified revision deadbeef... while the controller
  accepted <verification commit>; a worker reconciliation names the revision
  its worktree started from (<base>)`, and T2 stayed blocked.
- Focused suites and the broad regression set (reconcile, findings, results,
  reviews, prompts, context, compatibility, legacy, compact status, quota):
  pass. `go vet ./...`: pass. `gofmt` clean. `make scrub-check`: clean.
  `go test ./internal/contractschema ./internal/ui -count=1`: pass.
- `go test -timeout 40m -count=1 ./internal/looper`: pass (631.9 s, exit 0).
  The first attempt was stopped by the runner's ten-minute background limit
  with the package mid-run (the T155 runner-limit class), not by a test; the
  rerun under a 45-minute window completed green.

## Identifier-free circular wording detected, 2026-11-20 (T140)

Closes the limit the T124 and T135 sections recorded
(`looper:finding:circular-condition-semantics`): a condition that names no
task identifier — the audit's Charlie T040 wording "waits for save work to
resume" — escaped both the reconcile guard and the doctor warning, and a
review's `resume` transition would have been applied to it.

- Doctor warning (`circularResumeCondition`, `internal/looper/observe.go`)
  gains two readings. The subject-position identifier check gains the
  resumption events, so the retained Charlie unblock wording "T040
  auto-save work resumes" warns exactly like "T040 starts implementation".
  A new `idFreeCircularWording` reads a condition that names no task at
  all: when the words before one progress event name the task's own
  implementation, its own deferral round, or a significant word of its
  title — its own scope — the condition requires the task itself. A
  deferral reference paired with its resolution word inside one clause
  ("its deferred tag" with "lost", "lifted", "cleared") counts the same
  way. A significant title word is a title token after dropping function
  words, the event vocabulary, and project vocabulary nearly every title
  carries ("work", "system", "section", "test"); "save" is the scope word
  that catches T040 ("Auto-save system working per section 18").
- Quiet by construction, each exemption mirroring the identifier check: a
  condition naming any task identifier keeps its positional reading; an
  event governed by an external cue after it ("resumes when the migration
  lands") or in a preceding clause ("after the migration lands, the save
  work resumes") explains a consequence; a consequence connector ("then",
  "before") does the same; a cue in the same clause before the event is
  requirement position, not a consequence ("waits until the save work
  resumes" warns); and a subject outside the title scope names other work
  ("waits for the art direction pass to resume" stays quiet on the
  auto-save task). The "Deferred: " label is stripped before reading, so
  the boilerplate never pairs with a resolution word by itself.
- Consequence reading repaired in the reopened session (the first attempt
  stopped at the usage limit before its suite finished): the connector and
  cue exemptions first read only the word directly before the event or
  mention, so subject words in between re-introduced false positives the
  resumption vocabulary made reachable — "after the operator grants
  access, the T027 work resumes", "T085 must land before the T027 work
  starts", "the migration lands, then the save work resumes", and "the
  migration must land before the save work resumes" all warned. Both paths
  now split the text before the event at the last clause boundary
  (`clauseBefore`): a consequence connector anywhere in the event's own
  clause, or an external cue anywhere before that clause, explains the
  event. A conjunction without a cue or connector ("the operator grants
  access, and the T027 work starts") still warns, and every pinned
  circular shape keeps warning.
- Reconcile guard (`circularConditionReason`, `internal/looper/findings.go`,
  replacing the bool `circularCondition`): the any-mention reason is
  unchanged byte-for-byte; the identifier-free shape escalates with its own
  reason, "the unblock condition requires task <id>'s own work, so no
  independent prerequisite can satisfy it". Both the transition guard and
  the repeated-plain-finding path use it.
- Replay validation, read-only, against the retained Charlie and Ohya
  saved states (current `state.db` plus the five Charlie history
  snapshots): the new shape flags exactly two conditions, Charlie T054
  ("Deferred: Real audio and art production begins, before version 1
  ship") and T063 ("Deferred: The cafe building art pass starts"). Both
  are re-wordings of circular blockers the T135 replay already flagged
  with their identifier ("T054 real audio production begins") or that
  carry no independent owner at all; no previously quiet honest condition
  changed verdict. The findings.go any-mention guard also reports
  mention-only tasks (T051, T052, T074, T077, T082) exactly as before.
- Docs: `docs/observability.md` (the three textual shapes and the new
  honest examples), `docs/tasks-and-checks.md` (the escalation wording),
  and a dated row in `docs/AGENT-CONTRACT-MIGRATION.md`.

Tests: `internal/looper/circular_condition_test.go` gained eleven table
cases (the identifier-free circular shape, the identifier-plus-resumption
shape, the deferral-round shape, the honest quiet shapes, and the five
consequence-repair cases: cue-clause, connector, identifier-free connector,
identifier-free before, and the still-warning conjunction) and the doctor
fixture gained T106/T107 (warn, quiet); `internal/looper/reconcile_test.go`
gained `TestReconciliationEscalatesIdentifierFreeCircularCondition`
(escalation, its reason, one decision across repeated reports).

Limits kept, not fixed here:

- The scope match is exact-word after splitting. A condition that
  references the task's scope in a different form (Charlie T066's
  "brewing" against its title's "brewed") stays quiet unless an
  identifier appears; stemming would trade that quiet for new false
  positives, and the recorded circular wording repeats title words.
- The scope proxy cannot tell two tasks that share a subject ("the cafe
  building art pass" on a cafe-building task). Every state the replay
  checked flags such a condition only when no tracked task owns the
  event, which is the class the audit defined; an operator decision
  records the real prerequisite either way.

Validation, 2026-11-20 (reopened session):

- `make test` on the final tree: pass — build, scrub-check, `go test`
  (internal/looper 623 s), vet, the install and package scripts, and
  skill regression. The first two attempts were stopped by the runner's
  ten-minute background limit with the package mid-run (the T155
  runner-limit class), not by a test; the detached rerun completed green.
- `go test -race ./internal/looper -run 'TestCircularResumeCondition|
  TestDoctorWarnsOnCircularResumeCondition|TestCompactStatusKeepsCircularWarning|
  TestReconciliationEscalates|TestRepeatedPlainFindingEscalatesOneDecision'`:
  pass. `gofmt` and `go vet` clean on every changed file.
- The read-only replay against the retained Charlie and Ohya saved states
  was rerun after the consequence repair: every verdict above is
  unchanged, and no previously quiet honest condition changed.
- End-to-end `looper doctor` on a throwaway fixture: the identifier-free
  and deferral shapes warn, the connector and cue-clause shapes stay
  quiet, the task file stays byte-identical, and warnings keep exit 0.

## Bootstrap adopts a task file supplied during the agent call, 2026-11-20 (T144)

Closes the limit the T141 section recorded: `bootstrapResponse` ended with an
unconditional `writeJSON(e.Config.Todo, b)`, so a task file created while the
bootstrap agent call itself ran — a window of minutes — was overwritten by
the completed bootstrap result. T141's adoption covered only the quota wait
it added. Core code: `internal/looper/engine.go` (the guard in
`bootstrapResponse`, the `importID` parameter), `internal/looper/quota.go`
(`adoptSuppliedTaskFile`, the shared helper both windows call).

- The guard sits where the agent call returns, not only in front of the final
  write: any file present at that point appeared during the call, because
  `initializeState` reaches bootstrap only when the task file is absent.
  Checking before validation also adopts an answer that would have been
  refused, instead of spending a correction round over a file that already
  supersedes it; `TestBootstrapAdoptsSuppliedFileBeforeTheCorrectionRound`
  pins that with a call log proving bootstrap ran exactly once.
- The adoption is the T141 wait adoption, factored into
  `adoptSuppliedTaskFile`: the operator's bytes stay on disk, the startup
  parse below the bootstrap branch validates them like any startup input,
  the completed result is discarded with its workspace, one `bootstrap`
  journal event names the window it appeared in, and supplied startup plans
  lose their bootstrap staging record so the ordinary startup import analyzes
  them against the operator's backlog (otherwise `adoptStagedImports` would
  record them as covered by a bootstrap that never produced the file).
- Everything else on the start path is untouched: the missing-task-file
  refusal, saved-identity checks, the quota wait and its budget, drain and
  cancellation exits, and the startup seed. A quota pause armed by an earlier
  closure stays armed across the adoption, exactly as the T141 wait adoption
  leaves it, so the loop the adoption reaches still gates review and planning
  until `expireQuotaPauses` reopens the window.
- Residual window, accepted: the stat and the write are two steps, so a file
  created in the microseconds between them is still overwritten. The task
  contract prescribes the stat guard; closing the gap fully needs an
  exclusive-create write path, which would change `writeJSON` for every
  caller.
- A supplied file that is invalid gets the same treatment as any invalid
  startup input: the adoption keeps it on disk, the strict startup parse
  refuses it, and the start exits naming the file and the parse error. The
  operator's bytes are never overwritten to make a start succeed.

Tests: `internal/looper/bootstrap_supply_test.go` — the acceptance route
(the stub writes the operator's file inside its bootstrap invocation, then
answers with the competing bootstrap backlog; the operator's bytes survive
on disk and as the saved baseline, the staging record of a supplied startup
plan is removed, the plan reaches the ordinary import records, and the once
run completes) and the correction route above. Both fail on the pre-fix
tree: the first loses the adoption event, the second exits 1 with "bootstrap
backlog rejected after correction".

Validation, 2026-11-20:

- `go test ./internal/looper -run 'TestBootstrap|TestQuota|TestStartup|
  TestImport|TestLegacyContract' -count=1`: pass (131 s), and
  `go test -race ./internal/looper -run 'TestBootstrap|TestQuota'`: pass.
- `go vet ./...`: pass. `gofmt` clean on every changed file.
- Live, built CLI on a private fixture under the task resource prefix
  (stub bootstrap writing the operator's file inside its invocation, then
  answering with a competing bootstrap backlog): `looper --once --agent
  codex` journaled "The task file appeared during the bootstrap agent call;
  startup adopts the operator's file and skips bootstrap", the task file and
  the saved baseline kept the operator's goal and task, the bootstrap
  backlog never landed, and the run finished "Project goal satisfied."
  (exit 0). With `--plan`, the same run queued the plan through the ordinary
  startup import ("Analyzing imported requirements"), left no bootstrap
  staging record, and kept the operator's backlog authoritative.
- Full-package and full-repo results are recorded in the task report.

## Full-codebase reliability, correctness, and simplification audit, 2026-11-20

Read-only review of the complete codebase (`cmd/looper`, `internal/looper`,
`internal/ui`, `internal/contractschema`, `install.sh`, `uninstall.sh`,
`Makefile`, and `scripts/*`) at commit `d0b01fc` (`0.12.1-dev.1`), verified
against source and baseline checks (`go vet ./cmd/... ./internal/...` pass,
`go test -timeout 40m ./cmd/... ./internal/...` pass in 778.6 s, and
`staticcheck ./cmd/... ./internal/...` with 17 concrete findings). No
production code was modified during this review.

### 1. Controller main loop, lifecycle, signals, and process management

- **Main loop boundary gaps (`looper stop` ignored across synchronous phases;
  explains the 2026-11-13 "Stop ignored" incident)**
  (`internal/looper/run.go:148-681`, `internal/looper/controller.go:326-339`,
  `internal/looper/planning.go:22,25,61,68`,
  `internal/looper/review_activity.go:28,31,76`,
  `internal/looper/engine.go:505,506`,
  `internal/looper/run.go:1104,1113`):
  - `draining := e.drainingNow()` is sampled only at the top of each loop
    iteration (`run.go:148`) and re-checked only before entering
    `reviewCycle` (`run.go:462, 531`).
  - There is no `e.drainingNow()` or external task-file edit check after
    synchronous `e.processControllerRequests(ctx)` (`run.go:235`, which runs
    multi-minute `processPlanImports`), `e.recoverRun(ctx)` (`run.go:549`),
    `e.reviewCycle(ctx, ...)` (`run.go:475, 578`), or `e.plan(ctx)`
    (`run.go:484, 631`).
  - If `looper stop` arrives during a milestone or final review that produces
    new ready findings (`len(ready) > 0`), `Run` immediately enters a
    multi-minute `e.plan(ctx)` (`run.go:631`) and falls straight through to
    worker dispatch (`run.go:646`) with the stale `draining == false` variable
    from the start of the iteration, spawning new workers after stop was
    requested.
  - Under `--once`, if `looper stop` arrives during a final review that
    produces no ready tasks (`len(ready) == 0`), `run.go:603-612` exits
    immediately as `"complete"` or `"no_runnable_tasks"` without honoring the
    drain, and `defer e.releaseStopRequests()` (`run.go:78`,
    `controller.go:326-339`) marks the stop request `RequestRejected`
    (`"the controller ended before it could honor this stop request"`).
  - Simultaneously, `planning.go:22,25,61,68`, `review_activity.go:28,31,76`,
    `engine.go:505,506`, and `run.go:1104,1113` call `e.publishView(false)`
    instead of `e.publishView(e.drainingNow())` (unlike
    `consultation_activity.go:64`), erasing `draining: true` from `view.json`
    throughout reviews and planning.
  - **Fix**: Re-check `if e.drainingNow()` (and `continue` to the top of the
    loop so the drain exit path runs) after `processControllerRequests`,
    `recoverRun`, `reviewCycle`, and `plan`. Remove the `draining bool`
    parameter from `publishView` and have `snapshot()` read `e.drainingNow()`
    directly.

- **External `to-do.json` edit during `reviewCycle` or `applyPlanImport`
  poisons `ExportPending` and `ExportBefore`**
  (`internal/looper/engine.go:363-398`,
  `internal/looper/integration.go:624-679`,
  `internal/looper/findings.go:74-182`,
  `internal/looper/imports.go:748-768`,
  `internal/looper/run.go:505, 578-598, 1002-1027`):
  - `e.export()` (`engine.go:363-373`) sets
    `e.State.ExportBefore = e.State.ExportHash` and
    `e.State.ExportPending = true` and saves `e.State` to SQLite *before*
    reading `to-do.json` from disk and checking for an external edit.
  - If the operator edited `to-do.json` while `reviewCycle` or
    `applyPlanImport` was running, `e.export()` returns
    `pendingExportGuardError` (`*retentionError`) at `engine.go:387`.
  - Both `findings.rollback()` (`integration.go:633, 668`,
    `findings.go:170-182`) and `staged.rollback()` (`imports.go:766`) restore
    only in-memory backlog fields, leaving `ExportPending = true` and
    `ExportBefore = <old hash>` in both `e.State` and SQLite.
  - Furthermore, `run.go:578-598` treats `*retentionError` (and even
    `ctx.Err()`) from `reviewCycle` as a retryable `review-error`
    (`reviewFailures++`, `continue`). On the next iteration, `refreshIdle()`
    (`run.go:505, 1002-1027`) imports the edited `to-do.json` and updates
    `e.State.ExportHash = <new hash>` without clearing `ExportPending` or
    `ExportBefore`. The next two `reviewCycle` retries fail immediately in
    `e.export()` because `disk == e.State.ExportBefore` compares `<new hash>`
    against stale `<old hash>`, aborting the controller after 3 wasted
    reviews with `state.db` wedged at `ExportPending = true`.
  - **Fix**: In `e.export()`, verify `to-do.json` on disk against `expected`
    before setting `ExportPending = true` and saving `e.State` (or clear and
    persist `ExportPending = false` and `ExportBefore = ""` on pre-write
    guard rollback). In `run.go:578`, check `ctx.Err()` and `*retentionError`
    before treating a `reviewCycle` error as a retryable agent failure.

- **Cancelled attempt permanently starves a reopened task and causes a 50 ms
  busy-spin**
  (`internal/looper/run.go:658-666`, `internal/looper/cancel.go:412`,
  `internal/looper/view.go:77`):
  - `commitCancellation` (`cancel.go:412`) sets `a.Phase = "cancelled"`.
    While `view.go:77` and `findings.go:250` exclude `a.Phase == "cancelled"`,
    the worker dispatch check at `run.go:660` checks only:
    `a.Task.ID == task.ID && a.Phase != "done" && a.Phase != "blocked" && a.Phase != "resume"`.
  - If a cancelled task is later reopened to `"todo"` in `to-do.json`,
    `Backlog.Ready()` includes it, `run.go:660` permanently treats it as
    `claimed = true` and never dispatches it, `--once` never terminates
    because `len(ready) > 0`, and continuous mode busy-spins every 50 ms
    running `refreshIdle()` (including git snapshots).
  - **Fix**: Add `&& a.Phase != "cancelled"` at `run.go:660` (or centralize
    in an `attemptClaimsTask(a)` helper).

- **`controllerAlive` never checks process start identity and fails
  unconditionally on macOS (`T019`)**
  (`internal/looper/controller.go:18-32, 110-152`,
  `internal/looper/resources.go:129-140`,
  `internal/looper/dashboard_attachment.go:54-65`):
  - Despite its doc comment (*"It compares the recorded process start time
    when the host exposes it"*), `ControllerInfo` has no `Identity` field and
    `controllerAlive` (`controller.go:112-120`) calls
    `_, err := runIdentityCommand(info.PID)` and discards the returned start
    time, returning `true` for any reused PID on Linux.
  - `signalController` (`controller.go:140-152`) reads `controller.json` and
    sends `SIGINT` to `info.PID` without verifying `info.Session` against a
    target session (even in `dashboard_attachment.go:57-61`, where
    `readController` is checked and then re-read inside `signalController`)
    and without comparing process start identity, risking sending `SIGINT` to
    an unrelated process if the PID was recycled.
  - `runIdentityCommand` (`controller.go:123-138`) reads `/proc/<pid>/stat`
    only (Linux-only), whereas `resources.go:129-140` already implements
    cross-platform `resourceProcessIdentity(pid)` using `/proc/<pid>/stat` on
    Linux and `ps -p <pid> -o lstart=` on Darwin. Because
    `runIdentityCommand` is Linux-only, `controllerAlive`,
    `signalController`, `submitterAlive`, and live `Store.Timing` /
    `Store.Statistics` all fail unconditionally on macOS.
  - **Fix**: Unify `runIdentityCommand` and `resourceProcessIdentity`, record
    `Identity string` in `ControllerInfo` when writing `controller.json`,
    compare `current == info.Identity` when non-empty, and pass the expected
    session ID into `signalController`.

- **Signal handlers installed in `looper start` parent and before daemon
  readiness / `maybeFirstRun`**
  (`internal/looper/cli.go:325`, `internal/looper/daemon.go:172, 212`):
  - `Main` (`cli.go:325`) calls `installControllerSignals(c, cancel)` for
    both `"run"` and `"start"` *before* `maybeFirstRun` and *before*
    `StartDetached`.
  - When a user runs `looper start` or is inside the interactive first-run
    form, the parent CLI process traps `SIGINT` and `SIGTERM`: pressing
    Ctrl+C is swallowed once, and `SIGTERM` writes a stray `stop` file into
    the runtime directory from the parent CLI instead of terminating it.
  - In `RunDaemon` (`daemon.go:212`), `installControllerSignals` is installed
    before `NewEngine` and readiness. If `NewEngine` hangs before writing
    `readyFile`, `StartDetached` times out after 10 s and sends `SIGTERM`
    (`daemon.go:172`); the child's `SIGTERM` handler intercepts it, writes a
    `stop` file instead of exiting, and forces `StartDetached` to hang an
    extra 5 s until `SIGKILL`.
  - **Fix**: Install controller signal handlers only in the actual controller
    process (`runCLIController` after `maybeFirstRun`, `RunDaemon` after
    `NewEngine` acquires the repository lock, and `runDashboardController`).

- **`runProcess` deferred `SIGKILL` runs after `cmd.Wait()` and SQLite fsync**
  (`internal/looper/process_unix.go:195-259`):
  - Line 195 registers `defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)`,
    which executes at function return—*after* `cmd.Wait()` (line 235) reaps
    the process group leader and *after* `writeProcessObservation` and
    `store.endProcess` (SQLite fsync, lines 253-257).
  - Once `cmd.Wait()` reaps the process group leader, the OS kernel can
    recycle the PID/PGID during the SQLite fsync, and the deferred `SIGKILL`
    can kill an unrelated process group.
  - **Fix**: Send `syscall.Kill(-pid, syscall.SIGKILL)` immediately before or
    immediately upon `cmd.Wait()` returning (or in the cancellation goroutine
    while the leader is still un-reaped), never after `cmd.Wait()` and
    post-wait I/O.

- **`readStopRequest` turns a racing `ENOENT` into a synthetic active stop
  request**
  (`internal/looper/controller.go:194-212`):
  - `readStopRequest` calls `os.Stat(path)` and then `os.ReadFile(path)`. If
    the `stop` file is removed between `Stat` and `ReadFile`, `os.ReadFile`
    returns `ENOENT`, and lines 202-204 return `&stopRequest{}` (an active,
    unscoped stop request that matches any session!) instead of `nil`.
  - **Fix**: Call `os.ReadFile(path)` directly and return `nil` when
    `os.IsNotExist(err)`.

- **`Engine.Close` omits `clearController` and `clearView`**
  (`internal/looper/engine.go:88-96`, `internal/looper/lifecycle.go:34-43`):
  - If a caller invokes `e.Close()` after `NewEngine` without running
    `e.Run(ctx)` (or if initialization after `NewEngine` aborts before
    `Run`'s `defer e.finishSession`), `controller.json` and `view.json` are
    left behind on disk after `e.lock.Close()`.

---

### 2. Git integration, branch recovery, plan imports, and `looper publish`

- **`looper publish` uses `--git-common-dir` instead of `--git-dir` for
  worktree-private `index`, `index.lock`, and merge state**
  (`internal/looper/publish.go:206-215, 325-327, 940-954, 1303, 1323, 1417-1429`):
  - `publish.go` resolves `index`, `index.lock`, and
    `MERGE_HEAD` / `CHERRY_PICK_HEAD` / `REVERT_HEAD` / `rebase-merge` /
    `rebase-apply` under `r.Common` (`git rev-parse --git-common-dir`).
  - Per `gitrepository-layout(5)`, `index` and sequencer/merge-state files
    are per-worktree (`--git-dir`), whereas `--git-common-dir` points to the
    shared main repository `.git` directory.
  - Running `looper publish` inside a linked Git worktree (which `AGENTS.md`
    explicitly recommends for concurrent edits) overwrites the main
    checkout's `.git/index` and checks merge state in the wrong worktree.
  - **Fix**: Add `GitDir string` (`git rev-parse --git-dir`, resolved to an
    absolute path) to `Repository` and use `r.GitDir` for `index`,
    `index.lock`, and `mergeState`.

- **`completePublishJournal` leaks the `index.lock` file descriptor**
  (`internal/looper/publish.go:1303-1315`):
  - `completePublishJournal` opens `index.lock` with `os.OpenFile` and uses a
    `defer` that closes `lockFile` only if `lockFile != nil`. At line 1315,
    right after `os.Rename(lockPath, filepath.Join(ev.repo.Common, "index"))`,
    it sets `lockFile = nil` without calling `lockFile.Close()`, leaking the
    open file descriptor on every publish.

- **`resolvePublishJournal` skips safety guards on startup recovery**
  (`internal/looper/publish.go:1215-1294`, `internal/looper/engine.go:711`):
  - `NewEngine` automatically runs `resolvePublishJournal` on startup.
  - When `j.Phase == publishPrepared` and `j.NewCommit == ""`, it calls
    `completePublishJournal(ev, j)` using fresh `ev` without checking
    `ev.publishable` or `ev.mergeInProg` (unlike normal publish at line 996),
    committing even if the user started a merge/rebase or modified the tree
    after the crash.
  - When `j.Phase == publishCommitted`, line 1270 calls
    `writePublishIndex(ev.repo.Common, j.NewCommit, j.IndexEntries)` *before*
    line 1277 checks `head == j.NewCommit && branch == j.Branch`, overwriting
    `.git/index` even if the operator moved `HEAD` or switched branches after
    the crash.

- **`looper publish --preview` crashes on unborn `HEAD`, creates `looper.lock`,
  and collects evidence twice**
  (`internal/looper/publish.go:351-355, 868-881, 909`):
  - In `collectPublishEvidence.compose()` (`publish.go:351-355`),
    `lsTreeEntries(ev.repo.Root, ev.head)` runs unconditionally even when
    `ev.head == ""` (unborn branch), failing with
    `git ls-tree "": fatal: Not a valid object name` and aborting
    `looper publish --preview` with `state_unreadable` instead of returning
    the `head_attached` refusal guard that `evaluate()` recorded at line 299.
  - `publishOpenEvidence` (`publish.go:909`) calls `repositoryOwned(c)`
    (which creates `looper.lock` via `O_CREATE`) instead of
    `repositoryOwnedNoCreate(c)`, violating the read-only contract of
    `--preview`.
  - `runPublish` (`publish.go:868-881`) calls `publishOpenEvidence` (which
    runs `collectPublishEvidence`) and then `buildPublishPreview` (which runs
    `collectPublishEvidence` a second time, spawning ~10 redundant `git`
    subprocesses).

- **`Repository.Apply` uses unhardened porcelain `git diff` / `git apply`**
  (`internal/looper/git.go:515-525`):
  - `Apply` runs `r.output("diff", "--binary", "--full-index", before, after, "--")`
    and pipes it to `git apply --index --3way --whitespace=nowarn`.
  - Unlike `Snapshot` and `attemptDiff` (which pass `--no-ext-diff --no-textconv`),
    `Apply` does not disable external diff drivers, textconv, or custom
    prefix settings (`diff.noprefix=true`, `diff.mnemonicPrefix=true`), so
    user/repo `.gitconfig` settings can produce patches `git apply` rejects.
    Also, if `before == after` (or the diff is empty), `patch` is empty and
    `git apply` exits with `error: unrecognized input`.
  - **Fix**: Short-circuit `if before == after` and
    `if len(bytes.TrimSpace(patch)) == 0`, and use
    `diff-tree --binary --full-index --no-ext-diff --no-textconv --src-prefix=a/ --dst-prefix=b/ -p`.

- **`recoverCommittedHead` crashes `Engine.publish` on empty or excluded-only
  owner commits, and `foldOwnerCommit` mutates state before commit/save**
  (`internal/looper/branch_recovery.go:366-402, 450-513`,
  `internal/looper/integration.go:389-397`,
  `internal/looper/run.go:353-362`):
  - When `Engine.publish()` detects that `HEAD` moved during a check, it
    calls `e.recoverCommittedHead("publish")` (`integration.go:389`).
  - If the owner made an empty commit (`git commit --allow-empty`) or
    committed only excluded files (`to-do.json`), `recoverCommittedHead`
    takes the unchanged-tree fast path (`matched == true`,
    `branch_recovery.go:366-402`): it updates `e.State.Head = current.head`,
    does *not* requeue `e.State.Publication`, and returns `(true, nil)`
    (`moved = true`).
  - `Engine.publish()` sees `if moved { return errBaselineMoved }`, and
    `run.go:358` sees `e.State.Publication != nil` and aborts the controller
    with `"the accepted commit left the publication of <ID> unrequeued"`.
  - In `foldOwnerCommit` (`branch_recovery.go:450-513`), `dirty` is only
    checked against `delta` (`s.Head..current.head`), not whether `delta`
    overlaps `diff(s.Head, s.Published)`; if they overlap,
    `r.Apply(s.Head, current.head)` on a worktree seeded from `s.Tip` can
    absorb uncommitted working-tree edits into `s.Tip`. `foldOwnerCommit`
    also mutates `a.Phase = "ready"` and `e.State.Publication = nil`
    (lines 483-491) before `r.Commit`, `r.UpdateRef`, and `e.save()`, without
    rolling back on error.

- **`reviewScope` skips final reviews for new tasks outside `source_files`
  and strands runs after cancelling the last blocked task**
  (`internal/looper/review_scope.go:51-133`,
  `internal/looper/integration.go:624-635`):
  - `ReviewedTree` and `ReviewedComplete` are set when a review completes
    (`integration.go:628-629`) and are never cleared when tasks are added,
    amended, cancelled, or integrated.
  - `coversSources(s, changed)` (`review_scope.go:121-133`) checks whether
    changed paths overlap `s.Backlog.Sources`, which bootstrap defines as
    requirements documents (for example, `["PROJECT.md"]`, `engine.go:412`), not the
    project's source code files.
  - **False completion without review**: After a run completes
    (`ReviewedComplete = true`, `ReviewedTree = T1`,
    `Backlog.Sources = ["PROJECT.md"]`), adding a task via `looper add-task`
    sets `Finished = false` while `ReviewedComplete` remains `true`. When the
    worker modifies code outside `Backlog.Sources` (`Published = T2`),
    `reviewScope` at final review (`final == true`) sees
    `s.ReviewedComplete == true` and `!coversSources(s, changed)`
    (`review_scope.go:76-81`) and returns `skip: true`, marking
    `e.State.Finished = true` without ever reviewing the new task's code.
  - **Stranded run after cancellation**: If a final review runs while a task
    is blocked (`ReviewedTree = T1`, `ReviewedComplete = false`,
    `Finished = false`) and the operator then cancels that last blocked task
    (`looper cancel`), `Published` is still `T1`. The next
    `reviewCycle(ctx, true)` sees `s.Published == s.ReviewedTree`
    (`review_scope.go:59`), returns `skip: true`, and sets
    `e.State.Finished = e.State.ReviewedComplete` (`false`). Under `--once`,
    the controller exits with code 2 (`no_runnable_tasks`) instead of 0
    (`complete`); in continuous mode, `Finished` remains permanently `false`.
  - **Fix**: Clear `ReviewedComplete = false` whenever tasks are added,
    reopened, amended, or cancelled (or bind `ReviewedTree` to a backlog
    status/contract digest), and never skip a final review when
    `s.Finished != s.ReviewedComplete` or `s.Integrated > s.ReviewedAt`.

---

### 3. Persistence, recovery preview/apply, receipts, and idempotency

- **`recovery_preview.go` diverges from startup and `recovery_apply.go`**
  (`internal/looper/recovery_preview.go:419-441, 587-614, 691-800, 1174-1269`,
  `internal/looper/recovery_apply.go:106-340`,
  `internal/looper/requeue.go:350-430`):
  - **Published new files falsely fail `project_tree_changed`**: Because
    Looper publishes accepted work as unstaged working-tree edits (`r.Apply`
    + `r.Reset`), any new file created in `s.Published` is untracked in
    `HEAD` and appears in `git ls-files --others --exclude-standard`
    (`ev.untracked`). Unlike `Engine.reconcile` (which compares a full
    snapshot against `s.Published`), `recovery_preview.go:697-702` flags any
    non-empty `ev.untracked` as `project_tree_changed: fail`, falsely blocking
    `discard-retained` and `reset-to-baseline` whenever a run created a new
    file.
  - **`already_applied` publication falsely fails `project_tree_changed`**:
    In `workingTreeComparison` (`recovery_preview.go:588-598`), when
    `s.Publication != nil` and `ev.publicationOutcome == "already_applied"`,
    `expected` is set to `s.Publication.Before` instead of
    `s.Publication.Tree`, so `git diff p.Before` reports the already-applied
    publication as unexpected working-tree drift.
  - **`collectListings` swallows git errors as a clean tree**: In
    `recovery_preview.go:419-441`, `if out, err := gitCommand(...); err == nil`
    silently ignores git errors on `diff --name-only` and `ls-files --others`,
    leaving `ev.dirty` and `ev.untracked` empty so `matches_published`
    evaluates to `"pass"` when git fails.
  - **`commitRecoveryReceipt` never sets `rec.Ack`**: In
    `recovery_apply.go:250-280`, `rec.Ack = &ack` is omitted, so if the
    controller crashes after `commitRecoveryReceipt` saves to SQLite and
    before writing `ackDir/<id>.json`, `replayedAck` (`receipt.go:256`) fails
    to reconstruct the ack on restart.
  - **Stopped recovery and stopped requeue skip `answerFromRetainedKey`**:
    Neither `applyStoppedRecovery` (`recovery_apply.go:316`) nor
    `runRequeueStopped` (`requeue.go:350`) checks `answerFromRetainedKey`, so
    replaying a stopped-mode `--idempotency-key` fails with conflict instead
    of replaying the retained receipt. `runRequeueStopped` also omits the
    `s.Version != StateVersion || s.Root != c.Root || s.Todo != c.Todo`
    check that `applyStoppedRecovery` enforces.
  - **`applyRestoreTaskFile` refuses to restore a deleted `to-do.json`**:
    `recovery_preview.go:751, 1050` offers
    `looper recovery --apply restore-task-file` when `to-do.json` is missing
    (`os.IsNotExist`), and `Engine.restoreExport` restores a missing file on
    startup, yet `applyRestoreTaskFile` (`recovery_apply.go:173-176`) rejects
    `if ev.todoErr != nil` for all errors including `os.IsNotExist`.

- **Idempotency & `--wait` contract bugs**
  (`internal/looper/idempotency.go:160-264`,
  `internal/looper/receipt.go:203-214`,
  `internal/looper/imports.go:420-434`,
  `internal/looper/config_cmd.go:765-788, 1146`):
  - **`answerFromRetainedKey` with `--wait` returns exit code 0 on rejected /
    failed receipts and skips waiting on in-flight receipts**
    (`idempotency.go:239-263`): It calls `emitMutationReceipt` (which always
    returns `exitOK = 0`), whereas normal `--wait` returns
    `exitFromReceipt(c, rec)` (non-zero on `RequestRejected` /
    `RequestFailed`) and waits via `waitSubmittedReceipt` when `rec.State` is
    still `RequestAccepted` or `RequestWaiting`.
  - **`replayIdempotentAck` caches `ack` before mutating it and writes
    incomplete rejected receipts** (`idempotency.go:186-215`): Line 187 calls
    `e.rememberAck(req.ID, ack)` *before* lines 206-213 set
    `ack.Status = AckConflict` and `ack.Message = ...` for an in-flight
    original request, caching an empty `AckError` in `e.acks[req.ID]`. Lines
    191-194 (`if ack.Message == ""`) are dead code because
    `if original.Ack != nil` returned at line 188, and lines 197-200 append a
    raw `RequestRecord` missing `Session`, `CreatedAt`, `DecidedAt`, `Ack`,
    and `AffectedTasks`.
  - **Conflict rejections stamp the colliding `IdempotencyKey` and new
    `PayloadDigest` onto the rejected receipt**: When a request fails an
    idempotency conflict check (`conflict != nil`),
    `rejectRequestReceipt(req, ack)` (`receipt.go:203-214`) copies
    `req.IdempotencyKey` and the conflicting `PayloadDigest` onto the new
    `RequestRejected` receipt. Because `findIdempotencyRecord` scans
    oldest-first and `trimRequestRecords` prunes past 200 receipts, once the
    original receipt is pruned, the rejected conflict receipt hijacks
    ownership of `IdempotencyKey` with the conflicting digest. Similarly,
    `dedupImportReceipt` (`imports.go:428`) copies `applied.IdempotencyKey`
    onto a later duplicate import that carried a different key or no key.
  - **`runConfigProjectStopped` ignores idempotency payload conflicts, and
    `configPatchMap` conflates `--set field=""` with `--unset field`**:
    `runConfigProjectStopped` (`config_cmd.go:1146`) calls
    `answerFromRetainedKey` without checking whether a retained receipt with
    the same key had a different `PayloadDigest`, silently applying the
    conflicting mutation under the existing key. And `configPatchMap`
    (`config_cmd.go:765-788`) serializes both `--set model=""` and
    `--unset model` to `""` (and `--set jobs=0` and `--unset jobs` to `"0"`),
    so `configPayloadDigest` produces the same digest for distinct operations.

- **Secret scrubber corrupts `Authorization: Bearer`/`Basic` headers and leaks
  tokens / 24-line-truncated PEM keys**
  (`internal/looper/failure.go:228-263`):
  - In `secretScrubbers` (`failure.go:251-252`), regex 1
    (`(?i)\b(authorization|proxy-authorization)(\s*:\s*)(\S+)`) runs before
    regex 2 (`(?i)\b(bearer)(\s+)([A-Za-z0-9._~+/=-]{8,})`). On
    `Authorization: Bearer <jwt>`, regex 1 matches `\S+` against `"Bearer"`
    and replaces it with `Authorization: [redacted] <jwt>`; regex 2 then sees
    `[redacted]` instead of `Bearer` and fails to match, leaking `<jwt>` (and
    `Basic <base64>`) in cleartext into public events and failure summaries.
  - `boundedContextOf` (`failure.go:228-246`) slices output to the last 24
    lines *before* calling `scrubSecrets`. Because a 2048-bit PEM private key
    is ~27 lines, slicing to 24 lines cuts off
    `-----BEGIN RSA PRIVATE KEY-----` while keeping 23 lines of base64 key
    material and `-----END RSA PRIVATE KEY-----`, causing the multiline PEM
    regex to miss it.
  - **Fix**: Update the header regex to consume an optional scheme before the
    credential
    (`(?i)\b(authorization|proxy-authorization)(\s*:\s*(?:(?:bearer|basic|token|digest)\s+)?)(\S+)`),
    and run `scrubSecrets` before slicing lines in `boundedContextOf`.

---

### 4. Agents, quota pauses, observation, and dashboard (`internal/ui`)

- **Quota pause edge cases and 50 ms busy-spin**
  (`internal/looper/run.go:229, 449-526`,
  `internal/looper/recovery.go:332-376`,
  `internal/looper/imports.go:610-633`,
  `internal/looper/agent.go:468`,
  `internal/looper/quota.go:235-246`):
  - `expireQuotaPauses(backlogHold)` (`run.go:229`) runs when idle
    (`backlogHold == false`) *before* `refreshIdle()` (`run.go:505`). If
    `to-do.json` was edited while a task was paused on quota,
    `expireQuotaPauses` calls `e.export()` before `refreshIdle` imports the
    edit, crashing the controller with `pendingExportGuardError`.
  - When retained ready work exists with `e.State.Plan == nil` and the
    reviewer profile is quota-paused (`run.go:449-493`), `e.plan(ctx)` is
    skipped (`!e.reviewerQuotaPaused()`), `e.State.Plan == nil` hits
    `continue` at line 492 before reaching `quotaIdleWait` (line 526), and
    the controller busy-spins at 50 ms (`idlePoll`) for up to 24 hours
    without emitting a waiting event.
  - `recoverRun` (`recovery.go:332-376`) and `processPlanImports`
    (`imports.go:610-633`) do not check
    `e.pauseOnQuota(e.Config.ReviewAgent, "", err)` or `ctx.Err()` on
    `Consult` / `AnalyzePlanImport` failure: `recoverRun` logs
    `diagnosis-error` and increments `RunRecoveryCount` (burning the 1-attempt
    recovery budget on a 429 or Ctrl+C), and `processPlanImports` permanently
    marks the import `ImportFailed` on a race with a newly encountered
    reviewer quota error or context cancellation.
  - `quotaResetRelative` (`agent.go:468`) requires `[,\s]+` between duration
    units, so compact durations like `"Resets in 4h57m58s"` match only `"4h"`
    and drop `"57m58s"`, waking 58 minutes early. Change `[,\s]+` to
    `[,\s]*`.
  - `quotaHeldAttempts` (`quota.go:235-246`) does not check
    `!attemptRetired(a)`, so after `looper requeue --fresh` retires a
    quota-held attempt, the retired attempt still matches
    `quotaHeldAttempts`, re-arming expired pauses on startup
    (`restoreQuotaPauses`) and emitting duplicate `quota-resume` events.

- **`pollWorkerActivity` skips worker logs created in the first milliseconds
  after controller start**
  (`internal/looper/observe.go:1126-1131`):
  - `e.controller.Started` (formatted as `time.RFC3339Nano`) is parsed with
    `time.Parse(time.RFC3339, e.controller.Started)` (which in Go still
    parses fractional nanoseconds) and compared via
    `st.ModTime().Before(start)`. Because filesystem mtime granularity is
    often coarser (1 ms–10 ms) than `CLOCK_REALTIME`, any worker log created
    within the first few milliseconds after controller startup has
    `st.ModTime().Before(start) == true` and is skipped until a subsequent
    write advances its mtime. Truncate `start` to `time.Second`.

- **Dashboard (`internal/ui` and `internal/looper/dashboard.go`) issues**
  (`internal/looper/dashboard.go:259, 573-622`,
  `internal/ui/dashboard.go:153-154, 417-426`,
  `internal/ui/workspace.go:880-894`):
  - In `tasksScreen` (`internal/ui/workspace.go:880-894`), when
    `m.width >= 110`, `left := truncate(scope, m.width) + "\n" + m.workspace.taskTable.View()`
    truncates `scope` (or `m.workspace.search.View()`) to `m.width` instead
    of `listWidth`, causing `lipgloss.NewStyle().Width(listWidth).Render(left)`
    at line 893 to wrap the header onto a second line and push the bottom row
    of `taskTable` past `paneHeight`.
  - In `internal/ui/dashboard.go:417-426`, pressing `[l]` on a selected
    attempt looks up `m.logTask` only in `m.snap.Attempts` (active attempts)
    unless `m.hasWorkspace()` has a matching `m.workspaceTask()`. Also,
    `workerLog` (`internal/looper/dashboard.go:597-602`) only includes
    `checks/*/setup.log` and `checks/*/verify.log` when `attempt.Phase` is
    `"verifying"`, `"done"`, or `"blocked"`, omitting `"cancelled"`.
  - In `internal/ui/dashboard.go:153-154`, `case "add-plan":` in
    `RunDashboard` is unreachable dead code because `dialog == "add-plan"` is
    handled inline in `dialogKeys`. In `internal/looper/dashboard.go:259`,
    `submitSettingsFromForm` takes an unused `in *os.File` parameter.

---

### 5. Staticcheck findings: tautological test assertions, overwritten assignments, and dead code

Running `~/go/bin/staticcheck ./cmd/... ./internal/...` flagged 17 items, all
verified against source:

- **Tautological `x != x` test assertions (`SA4000`)**:
  1. `internal/looper/backlog_equivalence_test.go:373`:
     `if e.State.PlanSignature != beforeSig || !e.State.Finished || e.State.ReviewedTip != e.State.ReviewedTip`
     — compares `e.State.ReviewedTip` against itself instead of a captured
     `beforeTip`.
  2. `internal/looper/recovery_apply_test.go:607`:
     `if !strings.Contains(stderr.String(), "unknown recovery choice") || digestFile(c.Todo) != digestFile(c.Todo)`
     — compares `digestFile(c.Todo)` against itself instead of a pre-call
     digest.
- **Overwritten / unused assignments (`SA4006`)**:
  3. `internal/looper/engine.go:728`: `b = clone(s.Backlog)` inside
     `if s.ExportPending` is never read before `b, err = parseBacklog(data)`
     overwrites it at line 742.
  4. `internal/looper/publish.go:207`: `entries := map[string]string{}` is
     allocated and immediately overwritten at line 212.
  5. `internal/looper/publish.go:208`: `head := map[string]string{}` is
     allocated and immediately overwritten at line 216.
- **Unused functions, constants, types, and struct fields (`U1000`)**:
  6. `internal/looper/branch_recovery.go:20`: unused const `branchDetached`.
  7. `internal/looper/branch_recovery.go:33`: unused field
     `branchRead.unborn`.
  8. `internal/looper/config.go:619`: unused map `configModeOptionNames`.
  9. `internal/looper/daemon.go:233`: unused function `daemonTail`.
  10. `internal/looper/engine.go:29`: unused field `Engine.Agent`
      (`Engine.Runner` is used everywhere).
  11. `internal/looper/publish.go:112`: unused field
      `publishEvidence.workingChanges`.
  12. `internal/looper/recovery_apply.go:589`: unused const
      `recoveryApplyUsage`.
  13. `internal/looper/retention.go:100`: unused function `diffLines`.
  14. `internal/looper/store.go:115, 183, 397`: `Store.Events`,
      `EventCursor.Scope`, and `Store.EventsPage` have no production callers
      (only their own unit tests in `store_test.go`; superseded by
      `RecentEvents` and `queryEvents`).

---

### 6. High-payoff simplifications and deduplication (~2,500–3,000 LOC reduction)

1. **Unify the 7 controller request processors and 6 staged backlog mutation
   handlers (~600 LOC savings)**:
   - `processSettingsRequests` (`settings.go:326`), `processAddTaskRequests`
     (`add_task.go:244`), `processRequeueRequests` (`dashboard_data.go:242`),
     `processRequeueFreshRequests` (`dashboard_data.go:380`),
     `processCancelRequests` (`cancel.go:166`), `processAmendRequests`
     (`amend.go:211`), and `processRecoveryRequests`
     (`recovery_apply.go:204`) are 7 identical claim-apply-writeAck-remove
     loops.
   - Inside `applyAddTaskRequest`, `applyRequeueRequest`,
     `applyRequeueFreshRequest`, `applyCancelRequest`, `applyAmendRequest`,
     and `applyRecoveryRequest`, the 35-line request preamble (in-memory
     `e.acks`, durable `replayedAck`, `resolveIdempotencyKey` +
     `replayIdempotentAck` + conflict rejection, `refuse`,
     `defer e.rememberAck`, session match check, and external task-file edit
     check) and the 25-line `stagedBacklogRequest` export/rollback epilogue
     are duplicated 6 times.
   - Extracting `e.drainRequestType(reqType, handler)` and
     `e.withBacklogMutation(req, payload, mutateFn)` eliminates ~600 lines
     and ensures all mutations stage and roll back `ExportPending` /
     `ExportBefore` and `rec.Ack` uniformly.

2. **Unify `findingStage` (`findings.go`), `stagedBacklogRequest`
   (`add_task.go`), and `stagedImport` (`imports.go`)**:
   - All three snapshot and roll back `Engine.State` around `e.export()`, but
     each snapshots a different subset of `State` fields (and none restores
     `ExportPending` / `ExportBefore`). A single `e.beginStateStage()` fixes
     the rollback bug everywhere at once.

3. **Unify the 8 v2 contract fit-ladder / byte-budget engines (~400 LOC
   savings)**:
   - `status_compact.go`, `inspect_compact.go`, `eventsquery.go`,
     `requestsquery.go`, `recovery_preview.go`, `publish.go`, `preflight.go`,
     and `runs.go` each define their own `<kind>Fit` loop and
     `<kind>Encoded(doc)` JSON length measurer. A single generic helper
     `renderWithinBudget[F any, D any](budget int, fits []F, render func(F) D) D`
     replaces all 8 copies.

4. **Share working-tree and task-file guard evaluation across startup,
   `recovery --preview`, `recovery --apply`, and stopped `requeue` (~500 LOC
   savings)**:
   - `Engine.reconcile` / `Engine.restoreExport` (`engine.go`),
     `collectRecoveryPreviewEvidence` (`recovery_preview.go`),
     `applyRecoveryChoice` (`recovery_apply.go`), and `runRequeueStopped`
     (`requeue.go`) each re-implement working-tree comparison, task-file
     equivalence classification, and state identity validation slightly
     differently—causing the divergence bugs in Section 3 above.

5. **Consolidate controller activity trackers (`planning.go`,
   `review_activity.go`, `consultation_activity.go`)**:
   - The three files contain near-identical `begin*Activity`,
     `update*Activity`, and `end*Activity` functions differing only in the
     activity state string, and two of the three hardcode
     `e.publishView(false)`.

6. **Speed up the 13-minute `internal/looper` test suite (`T108`)**:
   - Caching a template `.git` repository in `TestMain` (copying it per test
     instead of spawning 4–6 `git` subprocesses per test fixture) and
     providing an opt-in test knob to relax SQLite `synchronous=FULL` /
     directory fsyncs in unit tests that do not test crash durability will
     reduce suite wall time by 3–5×.

---

## Audit implementation, 2026-11-21

The full-codebase audit above (recorded 2026-11-20 at `d0b01fc`) was
implemented as unstaged edits on top of `d0b01fc`. Status per section:

### Section 1 (controller loop, lifecycle, processes) — done

- `export()` reordered: the on-disk digest is verified before
  `ExportPending`/`ExportBefore` are touched, so a refused export leaves no
  stale guard state behind (engine.go).
- `Engine.Close()` added; `cli.go`, `daemon.go`, and the dashboard wrapper
  defer it, so the store and the published controller record are released on
  every exit path.
- Controller identity: `ControllerInfo.Identity` records
  `/proc/<pid>/stat` field 19 (Linux) or `ps -p <pid> -o lstart=` (darwin)
  via one shared `runIdentityCommand`; `controllerAlive` compares it, so a
  recycled PID can no longer read as a live controller. `resourceProcessIdentity`
  was removed; callers share the new command.
- Signals install after the first-run form and the start branch, only in the
  process that will own the controller; `readStopRequest` pins the stop file
  inode with an open handle against the unlink-race; early-error paths in
  `process_unix.go` now kill the process group through one `killAndWait`
  helper, and a `reaped` flag stops the panic path from double-waiting.
- Drain re-checks: the loop refreshes `draining` after
  `processControllerRequests`, and the retained nil-plan block routes
  `drainingNow()` to the drain exits instead of the dispatch gate.
- Review failures: `ctx.Err()` exits 130; a `*retentionError` exits once
  with the guard's restore demand instead of burning the review budget.
- Dispatch claim skips attempts whose phase is `cancelled`.

### Section 2 (git, publish, branch recovery) — done

- `Repository.GitDir` resolved once (`rev-parse --absolute-git-dir`); the
  publish index-lock and index-install paths use the worktree git dir, the
  repository lock keeps the shared common dir, and `collectPublishEvidence`
  stats `index.lock`/merge state on the worktree dir.
- `publishCommitted` detects a moved `NewCommit`/branch conflict before
  installing the index; `compose()` tolerates an unborn project HEAD; the
  publication evidence file handle is closed before release; the preview
  path uses the no-create ownership probe.
- `Apply` hardened: before==after short-circuit, `--no-ext-diff
  --no-textconv` with fixed prefixes, empty-patch short-circuit.
- `foldOwnerCommit` commits and moves the ref before any state mutates and
  rolls back fully on a save error; the pending publication it requeues
  moves with the same transaction. A matched-path requeue in
  `recoverCommittedHead` was tried and reverted: the startup application
  completes a pending publication against the moved head (that is the
  accepted-resume contract `TestResumeAfterCommittingPublishedFiles`
  pins), and only the fold requeues one. The audit's `r.Apply` overlap
  concern in the fold path does not match the current code (no apply call
  there) — recorded as not applicable.

### Section 3 (persistence, recovery, receipts, idempotency) — done

- Recovery preview: `collectListings` records the first failed query in
  `ev.readErr`; the working-tree comparison reports unknown on it, the
  branch comparison names it in the document, and `publicationOutcome`
  refuses to derive an `applies` verdict from an unreadable local-edit
  listing.
- `already_applied` publications compare directly against `p.Tree`; only
  `applies_at_resume` keeps the before-basis filtering.
- New read-only `untrackedDrift` filter: untracked paths the resume tree
  carries byte-identically (Looper's published unstaged edits) are no longer
  reported as `project_tree_changed` drift. One `ls-tree` plus one batched
  `hash-object --stdin-paths` (no `-w`); unhashable paths count as drift.
- Settings journal: entries record `unsets` separately, so `--set field=""`
  and `--unset field` are distinct edits; `runConfigProjectStopped` now
  refuses a reused key with different content (`idempotency_key_conflict`)
  instead of silently applying. Legacy entries without the unset spelling
  keep their old comparison. The live wire protocol treats the two
  spellings as one operation (both clear the field), so its payload digest
  intentionally stays shared.
- Stopped recovery/requeue replay receipts and the task-file restore
  acceptance are in (`answerFromRetainedKey` before stopped application;
  `applyRestoreTaskFile` accepts a missing file; identity check in
  `runRequeueStopped`).
- Idempotency: `answerFromRetainedKey` with `--wait` polls the in-flight
  original and returns exit 1 on rejected/failed receipts;
  `replayIdempotentAck` caches the ack after the in-flight adjustments;
  conflict rejections never inherit the colliding key; import mirrors clear
  the key fields.
- Secret scrubber: `boundedContextOf` scrubs before the line window is
  taken, and the header regex consumes the optional auth scheme, so
  `Authorization: Bearer <jwt>` loses the token, not the word "Bearer".
  Covered by `TestBoundedContextScrubsBeforeSlicing`.

### Section 4 (quota, observation, dashboard) — done

- `expireQuotaPauses` defers a resume that hits the export guard: the
  deadline re-arms and the boundary imports the external edit first, so a
  mid-pause `to-do.json` edit no longer crashes the controller.
- Retained nil-plan work under a closed reviewer window sleeps the poll
  interval with the quota wait message instead of a 50 ms busy-spin.
- `recoverRun` drops its started record and returns the error on quota or
  cancellation (the one-attempt budget stays unspent); `processPlanImports`
  keeps the receipt in its current state instead of failing it.
- `quotaResetRelative` joins units with `[,\s]*`, so `4h57m58s` parses in
  full.
- `quotaHeldAttempts` skips retired attempts, so `requeue --fresh` work no
  longer re-arms expired pauses at startup.
- `pollWorkerActivity` truncates the controller start to the second, so
  logs created in the first milliseconds are observed.
- Dashboard: the tasks header truncates to the pane width; `[l]` resolves
  the task from active attempts or any task's retained attempt
  (`taskOfAttempt`); `workerLog` includes check logs for `cancelled`
  attempts; the dead `add-plan` exit case and the unused `in *os.File`
  parameter are gone.

### Section 5 (staticcheck) — done

- Tautological assertions fixed with captured before-values
  (`backlog_equivalence_test.go`, `recovery_apply_test.go`); SA4006 test
  values fixed (`consultation_activity_test.go`, `import_view_test.go`,
  `lifecycle_test.go` ×2); `SchemaPath` uses unconditional `TrimSuffix`.
- Removed: `branchDetached`, `branchRead.unborn`, `configModeOptionNames`,
  `daemonTail`, `Engine.Agent` (field, literal, and two test assignments),
  `publishEvidence.workingChanges`, `recoveryApplyUsage`, `diffLines`,
  `EventCursor.Scope`, and the earlier `clone`/map SA4006s.
- Kept on purpose: `Store.Events` and `Store.EventsPage` have no production
  callers but ~10 test files read them; deleting them would rewrite the
  suites for no behavioral gain. `ST1005` error-string style in the
  dashboard files was also left: those strings are the UI's sentence-case
  voice, deliberately unchanged.

### Section 6 (simplifications) — deferred

The six refactors are real but each crosses 8–15 files of guarded mutation
code that milestone reviews validated per task (T018–T148, AX-001–AX-028).
Doing them in the same change as this audit's correctness fixes would make
the diff unauditable. Proposed order when picked up: `beginStateStage`
first (it fixes the shared rollback gap), then the request-processor
unification, then the fit-ladder engine, with the suite speed-up (T108)
last since it changes test infrastructure only.

### Follow-up fixes the focused runs caught

Three first-draft implementations failed their focused suites and were
corrected in the same pass:

- The drain flag: computing `Draining` from the stop file at snapshot time
  loses the flag at the drain exits, which publish after the request is
  consumed. The flag is now engine state (`Engine.draining`) that the loop
  sets when it observes a bound request and the snapshot reads directly.
- Journal resolution: the prepared-journal guard evaluated
  `ev.publishable`, which includes the `journal_resolved` guard — that one
  always fails while resolving this very journal, so every prepared
  resolution reported conflict. `resolutionGuardsPass` now evaluates every
  guard except `journal_resolved`.
- Stopped idempotent replays: answering a stopped-mode retry purely from
  the retained receipt skips the mirror receipt the replay contract owes
  (AX-012). The stopped recovery and requeue paths now replay through the
  bare engine (`replayIdempotentAck`), which records the mirror, and refuse
  changed payloads with the shared `idempotency_key_conflict` error.

The full `make test` run caught two more, both in the review-scope shape
and the matched resume path:

- The shape guard now fires only when a recorded shape exists: synthetic
  test states and runs saved before the field carry none, and an empty
  `ReviewedBacklog` must stay inert instead of forcing every review full.
  The shape also normalizes `doing` to `todo` — dispatch is the
  controller's own transition, and every review boundary sits between
  dispatches, so ordinary progression must not widen a review's scope. The
  legacy status projection clears `ReviewedBacklog` beside the other T128
  coverage fields, keeping the frozen document's shape.
- The matched-path publication requeue (see Section 2 above) broke the
  accepted-resume contract; it is reverted.

### Validation

- `go build`, `go vet`, `gofmt` clean.
- Staticcheck: every SA4000/SA4006/U1000/S1017/S1030 finding is resolved,
  including five extra pre-existing SA4006 test bindings the audit's list
  missed (`worker_reconcile_test.go`, `preflight_test.go`, `runs_test.go`,
  `internal/ui/clipboard_test.go`) and one S1030. What remains by choice:
  ST1005 (the dashboard files' error strings are the UI's sentence-case
  voice), S1016 (struct-literal-to-conversion suggestions in
  `receipt.go`/`requestsquery.go`), and S1011 (one append-spread loop in
  `retention.go`) — behavior-neutral style the audit did not request.
- `scripts/scrub-check.py`: clean.
- Focused suites: config, quota, scrubber, drain/resume lifecycle,
  equivalence, recovery preview/apply, publish, imports, idempotency,
  dashboard, and view tests all pass (three regressions found by the
  focused runs were fixed and re-verified; see the environment note).
- Full `make test` gate: green (exit 0) — build, scrub-check, the whole
  `go test` suite over `cmd`/`internal` (looper 606 s under the host's IO
  pressure), vet, the install/build/package smoke scripts, and the skill
  regression checks.

### Environment note

`TestStopWaitEndsWhenControllerExitsWithoutHonoring` failed three times
during this work with the stop-wait timeout signature. The failures
reproduce on the unmodified baseline under the same host pressure
(/proc/pressure/io "full" avg60 80–90), with goroutine dumps showing the
controller wedged in `syscall.Fsync` for minutes — the documented T151
fsync-stall class, not a regression. Re-validated in isolation after the
audit edits whenever the suite runs.

The same class touched two drain tests once:
`TestStartupDrainAndResume/before_engine=true` and
`TestStartupDrainScopedRequests/session` failed with `resume after drain:
130 context deadline exceeded` in one batch (the shared `testBudget`
context covers two fsync-bound engine runs, and the drain consumed it),
while their ordering twins passed in the same batch. Both tests pass on a
plain rerun of the same tree — timing, not a wedge.

### Review of the implementation, 2026-11-21

A diff review of the unstaged audit work found seven defects in the new
code and two older ones. All nine are fixed, and each has a regression test
that fails without its fix.

- `requeue.go` (stopped path): the idempotency lookup ran before
  `req.Type` became `requeue-fresh`, so a keyed retry of `requeue --fresh`
  searched the plain requeue type and applied again. The type is now set
  before the lookup. Test: `TestRequeueFreshCLIStoppedKeyedRetryNeverReapplies`.

- `review_scope.go` and `engine.go` `applyResult`: `backlogShape` moved on
  every task close, so after ordinary progress every review ran at full
  scope and the T128 focused and skip modes stopped working. Decision: the
  dispatched task's own worker outcome (done, blocked, or cancelled) is
  progression. `applyResult` records the shape again after that flip when
  the shape still matched before it. Follow-up tasks, reopens, additions,
  and closes made outside the controller still widen the review. Test:
  `TestApplyResultKeepsReviewShapeForOwnTask`.

- `config_cmd.go`: a current journal entry with no unsets stored nil, which
  looked like a legacy entry, so `--unset X` replayed a recorded
  `--set X=`. `unsets` now always serializes (no `omitempty`;
  `configPatchUnsets` never returns nil), so only a legacy entry lacks the
  field. The matching logic is one helper, `configJournalMatches`. Test:
  `TestConfigJournalKeepsSetEmptyAndUnsetDistinct`.

- `quota.go` `requeueQuotaHeld`: a refused export left the held tasks
  half-requeued in memory, and the re-armed pause could no longer find
  them. The backlog, `Finished`, and `PlanSignature` now roll back on an
  export error. Test: `TestQuotaRequeueRollsBackOnRefusedExport`.

- `daemon.go`, `cli.go`, and `dashboard.go` signals: moving the handlers
  after `NewEngine` left a window where SIGTERM took the default action and
  left `controller.json` and `view.json` behind. Installing them before the
  lock (the older foreground path) let a process that never got the lock
  overwrite another controller's stop file. `controllerSignals` now
  installs before the lock and only latches signals. `own()`, called after
  `NewEngine` succeeds, replays them. An early SIGTERM still drains at the
  first boundary. Tests: `TestControllerSignals` and
  `TestSigTermBeforeEngineDrainsAtFirstBoundary`, which now asserts that no
  stop file appears before `own()`.

- `consultation_activity.go`: the tick published the cached drain flag, so
  a stop sent during a long consultation stayed hidden. The new helper
  `refreshDraining` (also used at the two loop sites) samples the request
  before each publication.

- `branch_recovery.go` fold: the ref moved before the publication's attempt
  was resolved, and a failed save did not move the ref back. The attempt
  now resolves first, and a save failure restores the ref to the old tip.
  Test: `TestFoldUnknownAttemptLeavesRefUntouched`.

- `failure.go` (older defect): the 4 KiB byte window could start or end
  inside a private key block. An open BEGIN line in the dropped prefix is
  now carried into the window, and two scrubbers redact a partial block at
  either edge. Test: `TestBoundedContextScrubsPartialKeyBlocks`.

Follow-up resolved the same day: the canonical payload digest
(`idempotency.go`) no longer covers `TaskRevision` or `AttemptsRevision`.
The CLI reads those at submission, so they are preconditions, not caller
content. They stay on the request, and the controller still refuses a
request read against a stale task. Before this change, a keyed retry after
a successful `cancel`, `amend`, or `requeue` read moved revisions and
always got `idempotency_key_conflict`. It now replays. Receipts written
before the change still match through `legacyRequestPayloadDigest`, which
rebuilds the old digest with the exact old field order, and
`payloadDigestMatches` accepts either digest. All comparison sites use the
matcher: claim-time `resolveIdempotencyKey`, `answerFromRetainedKey`, and
the stopped requeue and recovery paths. Docs: `AGENT-CONTRACTS.md` (key
section) and a 2026-11-21 row in `AGENT-CONTRACT-MIGRATION.md`. Tests:
`TestCancelKeyedRetryAfterSuccessReplays` (includes the legacy-digest
case), `TestAmendKeyedRetryAfterSuccessReplays`, and
`TestRequeueFreshCLIStoppedKeyedRetryReplays`.

Validation: `go vet` and `gofmt` clean; the full `make test` gate passed
(exit 0) after these fixes, at host io pressure avg60 about 37, and again
after the digest follow-up.
