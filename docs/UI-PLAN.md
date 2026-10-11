# Plan: CLI styling, run profiles, and a live dashboard

Improve the command line interface (CLI), then add saved settings and a
full-screen terminal user interface (TUI). Add a detached controller after
the dashboard and its control protocol work with a foreground controller.
Support additional Markdown requirements at startup and during development.
Plan import is agreed scope for this implementation.

## Goals

- Make terminal output readable with status labels, color, and clear layout.
- Save per-project settings and offer a short first-run form.
- Import additional requirements from Markdown at startup or during a run.
- Show active work, ready tasks, blockers, and recent events in a dashboard.
- Apply supported settings during a run without disrupting active work.
- Run a detached controller and observe it from another terminal.
- Preserve existing machine output through `--json` for existing workflows.
  Define additive machine records for the new plan-import commands.

## Non-goals

- Do not change task transitions, publication rules, or the acceptance invariant.
- Do not change the SQLite table schema for presentation features.
  Plan imports add optional receipt metadata to the persisted state JSON.
- Do not add a web interface, remote control, sound, or permission features.
- Do not add Windows support. The existing implementation targets Linux and macOS.

Controller lifecycle and configuration handling do change. Those changes need
separate tests from rendering.

## Current state

- `internal/looper/engine.go` writes events to SQLite and prints their messages.
- `internal/looper/cli.go` implements status, event polling, and stop requests.
- `internal/looper/run.go` owns task transitions and worker dispatch.
- Workers and checks run in separate goroutines. They read engine fields.
- `checkSlots` limits concurrent setup and check commands. `capacity` limits workers.
- `Agent` holds a separate copy of `Config`.
- `Parse` resolves environment variables, flags, automatic jobs, and review defaults.
- An empty verify override lets the planner choose an executable check.
  `Plan.Validate` rejects an empty verification command.
- SIGTERM currently cancels work. The stop file instead requests a drain.
- The `__exec` helper kills its process group when the controller exits.
- `go.mod` has one direct dependency: `modernc.org/sqlite`.

## Part 1: CLI output

### Output modes

Keep rendering separate from stored events and machine output.

| Mode | Behavior |
|---|---|
| Interactive CLI | Status labels, glyphs, timestamps, and terminal-width layout |
| Pipe or `--plain` | Plain text, no ANSI escapes, animation, or decorative glyphs |
| `--json` | Existing machine format; no form, styling, dashboard, or human notices |
| TUI | Full-screen dashboard only when explicitly requested and supported |

A TTY is an interactive terminal. Detect the actual input and output streams,
including writers supplied by tests. Unknown writers use plain output.
Disable color when `NO_COLOR` is set. Disable interactive rendering for
`TERM=dumb`. Color must never be the only status indicator.

`--plain` suppresses forms as well as full-screen rendering. `--json` takes
precedence over every presentation option. Keep existing `status`, `tail`,
and run JSON serialization paths separate from human rendering.

### Style layer

Add `internal/looper/style.go` for rendering and terminal capability handling.
Start with no new direct dependency for CLI styling. Reuse existing platform
support where practical. If width detection needs a dependency, document that
choice before implementation instead of writing a fragile terminal parser.

- Map event kinds to labels and optional glyphs: done `✓`, failure `✗`, repair
  `↻`, hold `⏸`, start `▸`, and progress `…`.
- Use green for done, blue for doing, and yellow for blocked.
- Show a timestamp before interactive event lines.
- Wrap long summaries and indent continuation lines by terminal display width.
- Handle narrow terminals, wide characters, and terminal resize events.
- Escape terminal control characters from task text and logs in human views.
  Preserve raw stored records and JSON output.

Example interactive event:

```text
14:02:11 ✓ T003 done  Add SQLite store
```

### Status and doctor

Use a compact board for interactive `looper status`:

```text
Backlog  ████████░░░░░░░░  5/12 complete
▸ In flight   T004 checking   T007 implementing
▶ Ready       T009 Refactor store · T010 Add stop tests
⏸ Blocked     T012 — missing database credentials
State    ~/.looper/myapp-3f2a/runtime/state.db
```

Keep `--ls` filtering and the plain status summary. Derive ready tasks from
`Backlog.Ready()`. Show explicit blockers separately from dependency waits;
a task waiting for a dependency is not necessarily marked `blocked`.

Doctor shows one result per check, plus a fix hint for each failure.

### Idle and completion

Distinguish success, waiting for work, blocked work, drain, dispatch limit,
and failure. Do not show a success banner merely because workers are idle.
The default controller keeps waiting after the goal is satisfied. `--once`
retains its existing completion and exit-code behavior.

Show counts and elapsed time for the current controller session. Label
cumulative project counts separately. Do not invent missing historical timing.

## Part 2: Run profile and first-run form

### Profile location and schema

Use `<project>/runtime/profile.json`, where `<project>` is the first path
returned by `projectPaths`. Its second path already includes `runtime`.
This is a directory under `LOOPER_BASE_DIR`, not the source repository.

```json
{
  "version": 1,
  "agent": "codex",
  "review_agent": "",
  "jobs": 0,
  "model": "",
  "claude_model": "",
  "effort": "",
  "verify_command": "",
  "setup_command": "",
  "review_every": 5
}
```

Rules:

- Empty `review_agent` follows the resolved coding agent.
- `model` selects the Codex model; `claude_model` selects the Claude model.
  Empty model fields use the corresponding agent's own configuration.
- Empty `effort` uses Codex's own configuration. Claude does not use this field.
- `jobs: 0` stores auto mode. Resolve its effective count at controller startup.
- Missing keys use defaults. Preserve unknown keys when saving version 1 files.
- Reject malformed files, invalid known values, and unsupported versions with
  the profile path and a fix hint. Do not overwrite them with defaults.
- Validate before writing. Use `atomicWrite` with mode `0600`.
- Keep the profile when resetting `state.db`.

### Resolution and precedence

Resolve each setting from these sources, in order:

1. Explicit flags
2. Present environment variables
3. Saved profile
4. Built-in defaults

Track source presence separately from the value. For example, `--agent codex`
overrides a saved `claude` value even though `codex` is the built-in default.
An explicitly empty environment variable also counts as present.

Split parsing from final resolution. Merge first, then validate and resolve
automatic jobs and the review-agent fallback. Preserve existing behavior for
`--all`, `--interleave`, `--model`, and `--smart`, including their ordering.
`--model` continues to set both agents' model overrides. Add regression cases
for these options before changing `Parse`.

Keep saved preferences separate from effective settings. A one-run flag must
not become a saved preference. Show the effective value and its source in
settings. Flags and environment variables remain authoritative for that run;
a conflicting profile edit is saved for a later run and reported as overridden.

### When to ask

Ask the first-run form only when all conditions hold:

1. The command starts a new controller: `run`, `start`, or unattached `tui`.
2. No profile exists, and no controller already owns the repository.
3. Input and output are TTYs, and the terminal supports interaction.
4. No setting flags or setting environment variables were supplied.
5. None of `--yes`, `--plain`, `--json`, or `LOOPER_NO_PROMPT=1` is active.

Ask before acquiring the repository lock. Recheck ownership and profile
existence after acquiring the lock. Never replace a profile created while
the form was open. Cancellation exits without starting work or saving answers.

When prompting is skipped, resolve normal settings and continue. Do not save
one-run overrides or create a profile implicitly. A completed first-run form
or explicit `looper config` creates the profile. `start` asks in the parent;
the detached child never reads terminal input.

### Forms

Use `charmbracelet/huh` for three inline first-run questions:

```text
Coding agent        codex › claude
Review agent        same as coding agent › codex › claude
Parallel sessions   auto › 1 › 2 › 4 › 8
```

Enter accepts the highlighted choice. Share field definitions and validation
with the full settings editor. The full editor also exposes both model fields,
effort, verify command, setup command, and review frequency. Accept any positive
session count in the full editor, including values outside the first-run list.

### Verify command detection

Detection supplies a candidate override when creating a profile. It never
replaces an explicit command. Inspect files without running commands.

| Evidence at the repository root | Candidate |
|---|---|
| `Makefile` with an explicit `test` target | `make test` |
| `go.mod` | `go test ./...` |
| `package.json` with a test script and an unambiguous package manager | The matching manager's test command |
| `Cargo.toml` | `cargo test` |

Use this order for a single project. Respect JavaScript lockfiles and package
manager declarations. For conflicting evidence or multiple project roots,
leave the override empty and let the planner select the verification command.
Do not treat a detected command as proof that checks cover the whole product.

When a profile is saved, show the selected command and its source. Suppress
human notices under `--json`. An empty override preserves planner behavior:
`Plan.Validate` still requires executable verification before work proceeds.
There is no review-only acceptance path.

## Part 3: Settings during a run

### Editors and ownership

`looper config` edits the full profile. Without a controller, it saves changes
under the repository lock, then releases the lock without starting the engine.
With a controller, it submits a request through the protocol in Part 6.
The editable TUI uses the same protocol and validation.

`looper config --reset` deletes the profile under the same ownership rules.
During a run, reset leaves effective settings unchanged until restart. The
next eligible interactive run asks again. No settings editor writes the
profile directly while a controller owns the repository.

### Application rules

| Setting | Applies when no higher-priority source overrides it |
|---|---|
| Parallel sessions | At the next dispatch boundary |
| Coding agent | On a new task attempt |
| Review agent | On the next planning or review pass |
| Models and effort | On the next worker launch or planning/review pass |
| Verify and setup commands | On the next controller run |
| Review frequency | On the next controller run |

Existing attempts retain their recorded coding agent, including repairs.
A repair launch can use newly resolved model and effort settings. Clearly
report fields that are saved, applied, overridden, or require restart.

The engine loop owns settings application, but workers still run concurrently.
Capture immutable execution configuration and an `Agent` value before each
worker, check, planning, or review operation starts. All retries within that
operation use the captured values. Workers must not read mutable engine
configuration after launch.

Changing `jobs` updates the configured worker limit and scheduler capacity.
It never replaces `checkSlots`, which belongs to `--check-jobs`. Keep that
semaphore stable for the controller lifetime. Decreasing jobs lets active
workers finish and blocks new dispatch until capacity is available. Increasing
jobs preserves existing memory and failure backoff limits. Repairs count as
workers and must respect the limit too.

Process requests at loop boundaries, including idle polling. Do not interrupt
synchronous planning or review to apply settings. Show requests as pending
until a boundary is reached; a client timeout does not mean cancellation.

## Part 4: Dashboard

### Command surface

| Command | Behavior |
|---|---|
| `looper` | Start a foreground controller with CLI output |
| `looper start` | Start a detached controller |
| `looper tui` | Attach with controls, or start a controller under the dashboard |
| `looper attach` | Attach as a read-only observer; never start a controller |
| `looper config` | Edit saved settings and request supported live changes |
| `looper add-plan <path>` | Submit additional requirements to an active controller |
| `looper status`, `tail`, `stop`, `doctor` | Preserve existing command semantics |

`run`, `start`, and `tui` accept repeatable `--plan <path>` options.
See Part 7 for import behavior and recovery.

Require both input and output TTYs for the dashboard. With `--plain`, `--json`,
or unsupported streams, `tui` uses the CLI for a new controller and event
following for an existing one. `attach` falls back to event following.

A saved database does not prove a controller is active. If no controller owns
the repository, `attach` exits with `No active run. Start one with: looper`.
If ownership exists but controller metadata cannot be verified, report that
condition instead of starting a competing engine.

### Layout and interaction

```text
LOOPER   CONTROLLING   12m elapsed   7/12 complete
2 implementing · 1 checking

T004  Migrate store to SQLite     implementing  repair 1
T007  Add stop-command tests      checking
T009  Repair todo schema          implementing  repair 2

READY    T012 Rate-limit backoff · T013 Docs pass
BLOCKED  T015 — missing database credentials

14:02:11  ✓ T003 done  Add WAL mode
14:02:09  ↻ T009 repair 2/6  verification failed

a add plan · s settings · t tail · d diff · q exit · x drain · ? help
```

Use a scrollable worker list, ready and blocked sections, and a bounded event
tail. Preserve selection by attempt identifier when rows change. Small
terminals collapse sections into selectable views. Support terminal resize,
keyboard scrolling, empty states, and visible errors.

`q` detaches an observer without stopping the controller. In controlling mode,
`q` explains that the engine is local and offers drain-and-exit or cancel.
It cannot leave that engine detached. `x` confirms a drain request. Read-only
`attach` hides and rejects plan import, settings, and drain actions. Ctrl-C exits observers;
in controlling mode it retains the existing interrupt-and-retain-work behavior.
Restore the terminal after normal exit, errors, and interrupts.

A diff view reads the selected attempt's worktree. Disable external diff
helpers and text conversion. Cap output and handle worktree removal during
reads. Previewing a changing worktree is not acceptance evidence.

Show successful completion only when the existing engine reports goal
satisfaction. The continuous controller then returns to its waiting view.
Make motion optional with `--no-animation`; status remains readable without
animation. Defer confetti and activity sparklines until core behavior works.

### Data contract

Events contain messages, not a complete dashboard state. Do not parse message
text to recover worker state. Add a presentation snapshot with controller
identity, session start, backlog, attempts, phases, counters, and event cursor.
The owner loop creates copies; the UI never reads mutable engine objects.

Publish the latest snapshot atomically to `runtime/view.json` for observers.
Use a version and controller session identifier. Clear or ignore stale views
when ownership ends. Load persisted state for recovery; the view file is never
an input to task execution. Missing timing fields render as unknown.

Use bounded, nonblocking notification channels in process. Coalesce snapshots
when rendering falls behind. The event journal remains authoritative for event
history. Observers poll snapshots and page events by identifier, with a bounded
initial tail. A slow or closed dashboard must never block the engine.

Use a read-only store connection for observers. The current `OpenStore` performs
schema setup, so do not reuse it unchanged for read-only attachment. Handle
missing state, controller exit, and read failures without blanking the last
valid view. Mark stale data visibly.

### Libraries and package direction

Use Bubble Tea for the program loop, Lip Gloss for layout, Bubbles for common
controls, and Huh for forms. Verify compatible versions and available components
before adding dependencies. Add Glamour only if Markdown summaries need it.
Measure build time and binary size rather than estimating their increase.

Keep `internal/ui` independent of `internal/looper`. Use a small shared package
for view and settings types, or inject callbacks with neutral types. CLI
orchestration can import the UI without creating an import cycle. Renderers
accept values and return output; they do not start engines or write profiles.

## Part 5: Controller lifecycle

### Paths and identity

Use `runtime :=` the second result from `projectPaths` for all paths below:

- `profile.json`: saved preferences.
- `controller.json`: session identifier, process ID, project identity, and mode.
- `looper.pid`: optional compatibility record containing the process ID.
- `daemon.log`: detached standard output and standard error.
- `view.json`: latest presentation snapshot.
- `requests/`, `processing/`, and `acks/`: typed controller request files.
- `imports/`: captured plan files, analysis artifacts, and bootstrap staging records.

Every controller, including foreground runs, publishes controller metadata
while holding the existing repository lock. The lock remains authoritative.
Use metadata to locate the controller; a process ID alone can be reused.
If the lock is held but metadata is missing or inconsistent, report ownership
as unknown. Never signal an unrelated process based on a stale pidfile.

### Detached startup

1. Resolve settings and any form in the parent. Pass a validated configuration
   and its source metadata to a distinct internal daemon entry point.
2. Start the child in a new session with `setsid`. Redirect input to `/dev/null`
   and output to `runtime/daemon.log`.
3. The child acquires the repository lock, validates startup configuration,
   opens the store, and publishes its controller metadata.
4. Send success or a structured startup error through a dedicated readiness pipe.
   Success means controller ownership is established; planning may still be pending.
5. The parent waits for readiness, child exit, or a bounded timeout. Report the
   log path on failure. On timeout, terminate and reap only the child it created.
6. After success, close the startup pipe and let the parent exit. The controller
   remains alive and owns its workers' lifetime pipes.

Do not reuse `RunChild` unchanged. Its lifetime pipe deliberately kills child
processes when their parent exits. Keep that behavior for worker subprocesses.
The daemon readiness pipe must not act as a lifetime pipe.

A simultaneous second start fails on repository lock acquisition. Stale files
cannot establish readiness or block startup when the lock is free. Only the
current session can remove its metadata during cleanup.

### Drain and interrupt

Route SIGTERM to the same drain request as `looper stop`, without cancelling
the execution context. Stop new task dispatch and finish active work using the
existing drain policy. Release the lock and remove owned metadata on exit.
A drain during startup remains pending until initialization reaches a boundary.

Keep SIGINT as explicit cancellation, with retained work for recovery. Separate
signal handling from the current shared `signal.NotifyContext` path. Test both
signals; do not describe them as interchangeable.

## Part 6: Settings request protocol

Use this protocol for foreground and detached controllers. Offline profile
edits acquire the same repository lock, so startup cannot race with an editor.

1. The client writes a versioned request atomically to `requests/<id>.json`.
   Include a unique identifier, target controller session, expected profile
   digest, and changed fields, or a reset operation.
2. The controller claims a request by renaming it into `processing/`. A client
   never overwrites an existing request. Process one claimed request at a time.
3. Validate the schema, session, values, and profile digest. Reject stale edits
   with an acknowledgement that tells the editor to reload.
4. Atomically save the profile, then apply eligible effective settings at the
   loop boundary. Record the resulting profile digest and per-field outcomes.
5. Write `acks/<id>.json` atomically before removing the claimed request. Emit
   a settings event that includes the request identifier. The client reads its
   own acknowledgement rather than treating any settings event as success.
6. On restart, reject settings requests targeting an earlier session. Report that a
   profile save may have completed before a crash; reload the profile before
   resubmitting. Never replay an old live mutation into a new controller.

Use assignment patches, not increment operations. Duplicate request identifiers
return the recorded acknowledgement during the session. Retain acknowledgements
for the session and remove old settings files only after acquiring ownership.
Plan-import requests follow the durable receipt rules in Part 7; session cleanup
must not delete their pending requests or referenced artifacts.
Bound file sizes and pending request counts. Invalid requests receive an error
without stopping the controller. A conflict never silently overwrites another
editor's changes.

`--reset` deletes the saved file but does not change effective settings mid-run.
The acknowledgement distinguishes reset from live application. Direct manual
file edits take effect on the next run; the controller checks the file digest
before any requested save and reports conflicts.

## Part 7: Add requirements from a plan file

### Commands and user flow

Use an explicit `--plan` option. Keep the existing positional argument for the
JSON task file, so Markdown cannot be mistaken for a backlog.

```bash
# Resume an existing project and add the UI requirements.
looper --plan docs/UI-PLAN.md

# Start a detached controller with additional requirements.
looper start --plan docs/UI-PLAN.md

# Start or attach through the dashboard and submit the plan.
looper tui --plan docs/UI-PLAN.md

# Submit to an existing controller without starting another one.
looper add-plan docs/UI-PLAN.md
```

Allow repeated `--plan` options. Treat the files in one invocation as one
import, so the planner can identify overlapping requirements. Paths resolve
from the invoking directory. Reject unreadable files, directories, empty
files, invalid text, and oversized input before starting work. Start with a
combined limit of 1 MiB and show a useful error when it is exceeded.

In an editable TUI, `a` opens **Add plan** with a file path field. After
submission, show **Queued**, **Waiting for active work**, **Planning**, and
**Applied**, or a specific failure. Show the resulting task identifiers and
any requirements already covered. Submission authorizes task creation and
normal execution; do not require a second approval for generated tasks.
Closing an observer does not cancel an accepted request.

`add-plan` requires an active controller. Otherwise, suggest
`looper --plan <path>`. `run` and `start` retain their double-start error and
suggest `add-plan` when a controller already exists. `tui --plan` submits to
the controller it attaches to. These options are invocation inputs, not saved
profile settings. Read-only `attach` does not accept `--plan`.

### Capture requirements once

Copy the submitted bytes into immutable runtime artifacts before acknowledging
the request as queued. Record the original path, content digest, request
identifier, and project identity. Later edits to the source file cannot change
a queued import. Retain the copies for workers, review, and recovery.

Accept files outside the repository. The controller provides captured text
explicitly to planning, worker, and review prompts; workers must not depend on
access to the original path. Keep import provenance in state metadata rather
than adding runtime paths to `source_files`, whose entries describe repository
files. Existing repository source files remain available as context.

Submitting a plan does not authorize importing unrelated working-tree edits.
Keep existing baseline and publication checks. At a safe boundary, use the
normal baseline refresh before planning when local files have changed. If
retained work prevents refresh, report the conflict and preserve the request.
An external plan file avoids introducing a source-tree edit during active work.
Do not weaken retained-work checks to admit a newly edited Markdown file.

### Extend bootstrap into a requirements importer

Extract shared requirements analysis from `bootstrap`. There are two modes:

- Without a backlog, bootstrap reads project context and all explicitly supplied
  plans together. Create one backlog, then perform the normal execution planning.
- With a backlog, import reads the current accepted revision, complete backlog,
  prior imported requirements, and new captured text. Return proposed additions,
  coverage references, and unresolved conflicts. Do not regenerate the backlog.

Use the configured review agent in a private inspection worktree. Capture its
configuration as for other review operations. Verify that analysis did not
change code. The agent returns structured data; only the controller writes
the backlog or persistent state.

Preserve existing task identifiers, statuses, acceptance criteria, dependencies,
and attempt history. New tasks start as `todo`, with concrete acceptance criteria
and executable verification requirements. The controller allocates unique task
identifiers and resolves proposal-local dependencies. Validate the combined
backlog for unknown dependencies and cycles before applying anything.

Compare each new requirement with existing tasks and implemented behavior.
Already covered requirements reference those tasks or evidence instead of
creating duplicates. A changed requirement creates follow-up work; it does
not reopen or rewrite a completed task. Reject conflicting requirements with
an explanation when they would require silently replacing existing scope.
The initial feature supports additive scope, not removal of earlier requirements.

Keep the existing project goal. Persist imported requirements as additional
scope and include them in execution planning and final review. Final review
must cover both the original goal and all applied imports. Merely adding tasks
without updating review context is insufficient.

### Safe application and recovery

Reuse the request transport from Part 6 with a typed `plan-import` request.
Import receipts differ from live settings: accepted imports survive controller
restart. Persist queued ownership in state before acknowledging acceptance.
A queued artifact without that receipt is unaccepted and must be resubmitted.

1. Validate and capture the request. Pause new ordinary task dispatch once an
   import is accepted. Allow active attempts, repairs, checks, and publications
   to reach the existing safe boundary; do not cancel them.
2. Recover retained work first on restart. Import only when no unfinished attempt
   or pending publication owns the current contract. Show the reason for waiting.
3. Refresh the baseline through existing checks, then analyze the captured plans.
   Record the backlog digest and accepted revision used for analysis.
4. Check that those inputs still match. Validate all additions before mutation.
   A failed analysis leaves the backlog unchanged and releases the dispatch pause.
5. Persist additions, imported scope, and the applied receipt together in the
   existing state record. Export through the existing journaled `export()` path.
   Recover an interrupted export before acknowledging success or dispatching work.
6. Clear `Plan`, `PlanSignature`, `Finished`, and `ReviewedTip` when scope expands.
   Recompute execution planning before dispatch and require a fresh final review.
7. Acknowledge with added task identifiers, coverage results, and any next steps.
   Resume scheduling. A stop request takes precedence; queued imports remain for
   the next run rather than keeping a draining controller alive.

Persist optional import records in the state JSON, with defaults for older
records. Each receipt records request identity, content digests, captured
artifact locations, status, added tasks, and analysis evidence. This does not
require a new SQLite table. Omit empty import fields from JSON so existing
status output stays unchanged for projects without imports. Projects with imports
expose these new optional fields. Define JSON request results with request ID,
status, added task IDs, coverage, and errors; emit no human notices in that mode.
Include this metadata in state compatibility tests.

Replaying an accepted request or an already applied set of content digests
returns its recorded result without adding tasks again. Changed contents form
a new import and require semantic comparison against prior scope. Do not claim
that matching titles alone prevents duplicates. Keep captured artifacts as long
as the associated state and retained attempts reference them.

A scope change with zero new tasks still needs recorded coverage and a fresh
final review. For a fresh project, persist bootstrap import receipts with the
initial backlog using a recoverable staging record; a crash between file
creation and state initialization must not cause a second import.

## Build order

1. Separate parsing and resolution. Add profile validation, source tracking,
   detection, and regression tests for existing options.
2. Add first-run and full settings forms, offline locking, reset, and form gates.
3. Add CLI styling, the status board, doctor hints, and plain-output fallbacks.
4. Add immutable operation configuration, live worker limits, controller identity,
   and the settings request protocol for foreground runs.
5. Add presentation snapshots, read-only store access, and bounded event paging.
   Build controlling and read-only dashboard modes with terminal cleanup.
6. Add detached startup, readiness reporting, signal handling, and automatic
   attachment for `tui`. Enable attached controls through the existing protocol.
7. Add the requirements importer, startup `--plan`, `add-plan`, and the TUI form.
   Validate import recovery, platform behavior, and output compatibility before
   adding optional visual effects.

Suggested files: `profile.go`, `detect.go`, `style.go`, `settings.go`,
`view.go`, and `daemon.go` under `internal/looper`; models, views, styles, and
forms under `internal/ui`. Add `requirements.go` for shared bootstrap and import
analysis, plus `imports.go` for durable requests, validation, and application.
Keep shared contracts free of engine dependencies.

## Validation and acceptance

### Configuration

- [ ] Explicit values equal to defaults still override the profile.
- [ ] Empty environment values, aliases, smart mode, and agent-specific models
      preserve existing behavior.
- [ ] Profile writes preserve auto mode and never save one-run overrides.
- [ ] Invalid profiles fail clearly without being overwritten.
- [ ] Forms run only for eligible controller starts; cancellation has no side effects.
- [ ] Reset works offline and online; only an eligible later run asks again.
- [ ] Detection respects package managers and leaves ambiguous commands to planning.
- [ ] Missing detection never bypasses executable verification.

### Concurrency and control

- [ ] Race-detector tests cover model, effort, agent, and session changes during work.
- [ ] Increasing or decreasing jobs preserves active work and the check semaphore.
- [ ] Repairs respect worker limits and retain their recorded coding agent.
- [ ] Slow or closed observers cannot block task execution.
- [ ] Concurrent settings edits receive distinct acknowledgements or explicit conflicts.
- [ ] Tests cover duplicate requests, invalid input, stale sessions, and crashes
      before and after profile save and acknowledgement.
- [ ] Pending changes remain visible during planning, review, and idle waiting.

### Interface and compatibility

- [ ] Render tests cover narrow terminals, long text, wide characters, and empty data.
- [ ] Piped and plain output contain no generated ANSI escapes or animations.
- [ ] Existing workflows retain their JSON output when no imports exist. Compare
      deterministic fixtures byte for byte with fixed times and identifiers.
- [ ] Import JSON results and optional state fields have explicit compatibility tests.
- [ ] Terminal tests cover resize, form cancellation, keyboard actions, and cleanup.
- [ ] Read-only attachment rejects control actions and exits without stopping work.
- [ ] The dashboard distinguishes success, blocked work, idle waiting, drain, and error.
- [ ] Diff previews handle removed worktrees and bounded output.

### Lifecycle

- [ ] A detached controller survives parent exit and still manages worker cleanup.
- [ ] Startup reports child errors and timeouts without leaving orphan controllers.
- [ ] Double starts, stale metadata, and unavailable metadata do not create two owners.
- [ ] Foreground controllers can be discovered and observed from another terminal.
- [ ] SIGTERM and `looper stop` drain active work; SIGINT cancels and retains work.
- [ ] The exiting controller removes only metadata belonging to its own session.
- [ ] Run existing regression checks and targeted integration tests on Linux and macOS.

### Plan imports

- [ ] `looper --plan docs/UI-PLAN.md` extends an existing backlog without resetting it.
- [ ] Fresh bootstrap includes explicitly supplied plans exactly once.
- [ ] TUI submission and `add-plan` use the same importer as startup.
- [ ] Imports during work wait safely, preserve attempts, and resume scheduling.
- [ ] External files remain available through captured text after their source is removed.
- [ ] Repeated imports and crash recovery do not create duplicate tasks.
- [ ] Changed plans create additive work or report conflicts without rewriting old tasks.
- [ ] Invalid proposals leave the backlog unchanged and release the dispatch pause.
- [ ] Combined backlog validation rejects missing dependencies and cycles.
- [ ] Tests cover crashes around acceptance, state save, export, and acknowledgement.
- [ ] Old state files load without import metadata; new metadata survives recovery.
- [ ] Final review includes imported scope, including imports with no new tasks.
- [ ] Stop, baseline conflicts, and retained work preserve pending imports visibly.

## Risks and limits

- UI dependencies add build and binary costs. Record measured changes.
- Live settings require configuration isolation across every worker call path.
- Planning and review can delay settings acknowledgements and graceful drain.
- The view file and profile are separate from SQLite. Session identity and atomic
  writes prevent stale presentation data from becoming execution authority.
- Manual profile edits do not participate in locking. Digest checks detect stale
  editor state, but simultaneous external writes remain unsupported.
- Historical worker timing and activity charts need additional instrumentation.
  Keep those features deferred instead of deriving them from message text.
