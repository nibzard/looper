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
in `/home/agent/looper-pre-scrub-2026-11-19.bundle` (all refs, tip
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
- Still open from the cleanup review: the license (T129, MIT decided),
  moving internal working documents out of the tree, and CI.


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
  no ID and is not detected; a review must still name it.
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
  all (Charlie T040's summary "waits for save work to resume") stays
  outside the check, as documented for T124.

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
