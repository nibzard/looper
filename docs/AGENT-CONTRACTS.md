# Looper agent contracts

Date: 2026-11-05. Owner: AX-001 in `docs/AGENT-EXPERIENCE-PLAN.md`.

This document defines the public contracts that the CLI, the doctor command,
and the dashboard share, and the rules for changing them. Machine-readable
artifacts live in `docs/contracts/`:

| Path | Contents |
| --- | --- |
| `schemas/legacy/*.schema.json` | Frozen schemas for the JSON documents Looper emits today. |
| `schemas/v2/*.schema.json` | Schemas for the contract version 2 documents the agent-experience plan adds. |
| `fixtures/v2/<name>/valid`, `fixtures/v2/<name>/invalid` | Example documents and rejected documents for each version 2 schema. |
| `fixtures/legacy/*.shape.json` | Recorded structures of today's documents, produced from a fixed scenario. |
| `exit-codes.json` | Transport exit codes, their meanings, and today's mapping. |
| `request-states.json` | The durable request lifecycle shared by every mutation. |
| `commands.json` | The public command surface, the documents each command emits, and the owning task for each version 2 document. A test parses every listed invocation and requires the parser's command list to match, so a new command cannot ship without a contract entry. |

## Why

The audit behind the agent-experience plan found agents paying repeated costs
to observe a run, submit work, recover from refusals, and discover state. The
fix is one set of query and operation contracts, not one new format per
command. Today's machine output also has a compatibility promise from
`docs/UI-PLAN.md`: existing `--json` workflows keep byte-compatible output.
This document resolves that promise with versioned contracts and an explicit
migration, never a silent shape change.

## Contract versions

Every machine-readable response belongs to a contract version.

- Version 1, named `legacy`, is the shape Looper emits today. It is frozen:
  additive changes are not allowed without a migration entry. See
  `docs/AGENT-CONTRACT-MIGRATION.md`.
- Version 2, named `v2`, is the bounded, structured contract this plan builds.
  Its documents carry `contract_version: 2`, a `kind`, and a `schema_version`.

A caller selects a version explicitly:

```
looper status --contract v2 --json
LOOPER_CONTRACT=v2 looper status --json
```

Selection is valid for every command: under version 2 every failure returns
the structured error document (AX-014). Success documents ship per command
with their owning tasks; until then a command keeps its legacy success
document, which a caller detects by the missing `contract_version` field.

Without a selection, commands emit the legacy shape. The default changes only
through the documented default transition in the migration guide, after the
AX-027 release gates pass. An unknown value is a usage error, not a fallback.

`--full` requests the complete unbounded response in whichever version is
selected. Compact is the default for version 2 status and inspection; it is
not a truncation of an error, and it never emits invalid JSON.

## Document rules

1. A finite query returns exactly one JSON document on standard output. The
   document validates against its schema in `docs/contracts/schemas/`.
2. NDJSON is reserved for explicit streaming: `looper events --follow` and
   `looper tail --follow`. Each line is one JSON document. A finite event
   query under version 2 returns one page document instead.
3. Human-readable text and machine documents never mix on standard output.
   Under `--json`, no human notices are printed, even when one command does
   several things.
4. Bounded collections report what they omitted: `limits.omitted` counts and
   `detail` routes name the command that returns the rest.
5. Plain-text output renders from the same summary the JSON document carries.
   It is a rendering choice, not a second contract.

## Command map

| Command | Legacy document | Version 2 document | Owning task |
| --- | --- | --- | --- |
| `status --json` | `schemas/legacy/status.schema.json` (complete state inline) | `schemas/v2/status-compact.schema.json`, `--full` keeps the complete state | AX-004 |
| `inspect <id> --json` | `schemas/legacy/inspect.schema.json` (unpaged history) | `schemas/v2/inspect-compact.schema.json`, `--full` keeps the complete unpaged document | AX-005 |
| `events --json` | NDJSON, one `schemas/legacy/event-ndjson.schema.json` document per line | `schemas/v2/events-page.schema.json` for finite queries, NDJSON unchanged for `--follow` | AX-006 |
| `doctor --json` | `schemas/legacy/doctor.schema.json` | `schemas/v2/doctor.schema.json`: same findings shape, plus bounded request summaries; findings never change the exit code under version 2 | AX-010, AX-014 |
| `version --json`, `--version --json` | `schemas/legacy/version.schema.json` | capabilities document | AX-008 |
| capabilities entry | none | `schemas/v2/capabilities.schema.json` | AX-008 |
| `add-task --json` | `schemas/legacy/settings-ack.schema.json` | `schemas/v2/request-receipt.schema.json`, then `schemas/v2/request-record.schema.json` | AX-009, AX-011 |
| `add-plan --json` | `schemas/legacy/plan-import-ack.schema.json` | receipt, then record | AX-009, AX-011 |
| `config`, `requeue`, `stop` | acknowledgement files in `runtime/acks/` | receipt, then record | AX-009, AX-011 |
| `cancel <task-id> --json` | `schemas/legacy/settings-ack.schema.json` (behind `--wait`) | receipt, then record, for the `cancel-task` request type | AX-019 |
| `amend <task-id> <file> --json` | `schemas/legacy/settings-ack.schema.json` (behind `--wait`) | receipt, then record, for the `amend-task` request type | AX-020 |
| `wait` | none | `schemas/v2/wait.schema.json`: one structured outcome for a changed cursor, task state, request outcome, or run outcome | AX-013 |
| `recovery --preview` | none (plain text renders under both contracts) | `schemas/v2/recovery-preview.schema.json` | AX-016 |
| `recovery --apply <choice> --token` | request receipt, then record (same documents as the other mutations; a stopped run reports the decided record at once) | receipt, then `schemas/v2/request-record.schema.json` | AX-017 |
| `preflight` | none (plain text renders under both contracts) | `schemas/v2/preflight.schema.json` | AX-023 |
| `runs` | none (plain text renders under both contracts) | `schemas/v2/runs.schema.json` | AX-025 |
| `requests [id]` | none (plain text renders under both contracts) | `schemas/v2/request-record.schema.json`: one record by identifier or one bounded page | AX-010 |
| `help [<command>]` | none (text under both contracts; `--json` is a usage error naming the capabilities document) | none | AX-026 |
| any failure | free-form stderr text | `schemas/v2/error.schema.json` | AX-014 (served) |

`docs/contracts/commands.json` is the machine-readable form of today's
surface: a test parses every listed invocation and fails when the parser
accepts a command the registry does not list. Rows above for commands that do
not exist yet name the task that adds them; they enter the registry when they
parse.

## Contextual help

Every command serves its own help block, and one overview serves the whole
surface (AX-026). Three spellings reach a block, and all three serve the same
text:

```
looper help status
looper status --help
looper status help
```

Each block carries the usage line, two or three sentences of what the command
does, runnable examples, the documents the command emits — the input schema
the caller writes and the output schema each contract version serves — and
the semantics a caller cannot guess: pagination, streaming, waits, and exit
behavior. The overview keeps the option table and the one-line surface list.

- Every example is an invocation the parser accepts as written, and every
  document a block names exists in `docs/contracts/`; `TestCommandHelpCoversTheSurface`
  and `TestCommandHelpMatchesContractRegistry` in `internal/looper` refuse a
  block that drifts from either.
- A topic that names no command is a usage error (exit 2), not an empty
  answer.
- A bare word that names no command and no file is an unknown command (exit
  2), never a task-file positional: a mistyped command cannot start a run.
  A positional selects a task file when it carries a path separator, a
  `.json` suffix, or names a file that exists.
- Help is text for people and agents at a terminal. `--json` on `help` is a
  usage error that routes to the machine-readable surface: the capabilities
  document at `looper version --contract v2 --json`.


## Shared summary model

Plain output, JSON output, and the dashboard read one summary model:

- Identity: project, run, controller session, and both builds.
- Ownership: lock evidence, metadata freshness, view match.
- Work: counts, a bounded active list, controller phases.
- Obstruction: waiting reason, request counts, findings.
- Motion: the last durable transition and the continuation cursor.

The dashboard's `runtime/view.json` snapshot stays a presentation artifact.
It is never an input to task execution, and its event cursor is not the
authoritative event position (AX-003 reads state and cursor from one database
snapshot).

Since AX-004 the model is one document, `looper.status.compact` in
`internal/looper/status_compact.go`. The JSON document and the piped plain
rendering come from the same structure, so the two outputs cannot disagree.
Its bounded inputs are the one-snapshot state read, the shared observation,
the newest durable transition (one indexed row), the public request
directories, and the published view for controller phases and freshness; it
never builds the timing or usage statistics the complete document carries.
A document over its 8 KiB budget renders again with smaller string bounds and
shorter bounded collections, and `limits` records the bounds that applied
with the counts of what was omitted, so truncation is stated, never silent.
Every string bound counts the bytes the JSON encoder emits, so saved content
that escapes heavily — an ampersand becomes six bytes — shrinks with the
ladder instead of expanding past the fit that stated it, and route commands
gate the identifier they embed in emitted bytes. The ladder ends in a
terminal fit whose strings are one rune at worst and whose collections are
empty, so the document it renders is identity, counts, and cursor alone and
a valid document always fits whatever the saved records hold.

## Cursor scope

- Event identifiers are scoped to the retained event store, not to a
  controller session. Continuation across a controller restart stays valid.
- `--after` is exclusive. `after: 0` reads from the beginning of the store.
- A finite page fixes its `high_water` when it starts, so concurrent writers
  cannot keep the page open. New events arrive on the next page.
- A filtered query advances its continuation even when the filter matches
  nothing, so filtering never stalls.
- Each store carries an identity and an epoch. A cursor from another store or
  an earlier epoch is invalid and returns `cursor_invalid` with an action,
  never a silently empty page.
- Session-scoped positions, where they exist, name their controller session in
  the cursor and never borrow another session's position.

AX-003 implements these rules at the store layer, which the documents above
build on: `Store.Snapshot` reads the saved state and its committed
high-water mark from one database snapshot, `Store.EventsPage` serves
exclusive-after pages with a high-water mark fixed at page start, and
`Store.ValidateCursor` refuses foreign identities, earlier epochs, positions
beyond the retained store, and session positions whose controller ended. The
store's identity and epoch live in the `store_meta` table; a store from
before that table existed keeps an unknown identity, and bare `--after`
identifiers validate against the committed high-water mark alone. The
published `runtime/view.json` snapshot is presentation only: since AX-003 the
`event_cursor` in status and doctor observations is the store's committed
high-water mark, so stopped runs without a view report a continuable
position.

## Event identity

Every lifecycle event the controller writes carries the structured identity
of its cause in its recorded payload, beside the message text that stays for
people (AX-007). The finite event contract surfaces the same fields on each
item: `task_id`, `attempt_id`, `controller_session`, `phase`, and
`request_id`.

- `controller_session` names the controller session that wrote the event.
  Every engine-owned event carries it.
- `task_id`, `attempt_id`, and `phase` name the attempt an event concerns and
  the attempt phase recorded when the event was written: `start`, `check`,
  `repair`, `result`, `blocker`, `defer`, `scope`, `diagnosis`,
  `assessment`, `hook-error`, and the worker activity streams.
- `request_id` names the durable request an event reports, so an outcome in
  the journal correlates with its receipt: `import` and `import-error`,
  `add-task`, `settings`, and `requeue`. An import or add-task that mutates
  several tasks lists them as a `tasks` array in the payload instead of
  naming one `task_id`.
- A field is written only when it applies. A historical record from before
  structured identities existed stays readable and surfaces no identity; it
  is explicitly incomplete, not wrong. Such records never match a task
  filter, which reads the structured `task_id` alone.
- Message text never decides identity, and no code parses it. The message
  keeps its exact wording; the identity rides beside it.

## Finite waits

`looper wait` answers "tell me when this changed" with one bounded call
instead of a polling loop (AX-013). Four predicates share one command, one
document, and one set of rules:

| Predicate | Invocation | Satisfied when |
| --- | --- | --- |
| `cursor_change` | `--cursor <event-id>` | The store's committed high-water mark passes the exclusive position. |
| `task_state` | `--task <task-id> [--status <status>]` | The task's saved status reaches the target; without `--status`, when it differs from the status observed at invocation, a task that appears counting as a change. |
| `request_outcome` | `--request <request-id>` | The durable request record reaches any terminal state — applied, rejected, failed, or cancelled. |
| `run_outcome` | `--run` | The bound controller session ends: a recorded outcome with its reason and exit code, or an unclean end when the controller process died without recording one. |

- `--timeout <seconds>` is required and names a finite bound (a bare number
  counts seconds; `30s` names its unit). The bound is what makes the wait
  finite, so it never defaults.
- The document (`looper.wait`) names the condition, the `outcome`
  (`satisfied`, `timeout`, or `cancelled`), the evidence the last observation
  read — the task's status, the request's full record, or the session's end —
  and always the store's committed cursor, so observation continues without a
  gap whatever the outcome was. Exit codes follow the registry: 0 satisfied,
  5 timeout, 6 cancelled. Unlike every other nonzero exit, the timeout and
  cancellation codes carry the `looper.wait` document, because the outcome is
  the result the caller asked for, not a failure of the command.
- Waits observe only. A timeout or an interruption cancels nothing: the
  request keeps its envelope and receipts, the task keeps its state, the
  controller keeps running, and the wait's own attach closes on every
  path. The attach follows the one-shot decision (T105): a run no live
  controller owns attaches immutable, so a wait on a cleanly stopped run
  creates nothing beside its database, and each observation re-attaches
  through the WAL read when a controller appears — by live metadata, a
  waiting write-ahead log, or a database a short-lived writer already
  checkpointed — so no commit is missed however briefly the writer lived. A
  writer that appeared mid-wait leaves its own sidecars behind; a wait never
  removes a run's files. `detail.follow` names `looper events --follow`,
  which stays the streaming alternative for callers that can hold a stream.
- The first observation happens before any sleep, so a condition that already
  holds returns at once, and one more observation follows an expired bound or
  an interruption before the outcome is reported, so a transition that commits
  during the last sleep is never missed.
- Targets bind to identity as appropriate: the cursor to the retained event
  store (the store identity and epoch ride in the document), the task and the
  request to the project's saved run, and the run outcome to one controller
  session — the live owner when the wait began, else the newest recorded
  session. A replacement controller's session never answers for the bound
  one; a restart marks the bound session `interrupted` and satisfies the wait.
- The task predicate samples the saved status, so a status that changes twice
  between two observations can pass unseen; `done` and `cancelled` are
  terminal and cannot be missed. The other three predicates read monotonic
  records — the journal, the durable receipts, and the session rows.
- Plain output renders the same document as text under both contract
  versions; `--json` selects the version 2 document and refuses the legacy
  contract with `contract_version_required`.

Every mutation receipt carries the wait object naming this command and its
bound (see Requests below), so a caller that just submitted work reads its
next step from the receipt itself.

## Failure summaries

Every worker, verification, launcher, and resource-cleanup failure journals
one bounded summary as a `failure` lifecycle event beside its structured
identity (AX-021). One focused query retrieves every summary a run recorded:

```
looper events --contract v2 --json --kind failure
looper events --contract v2 --json --kind failure --task T015
```

Each summary rides in the recorded payload under `failure` and carries:

- `operation`: the class that failed — `worker`, `planning`, `review`,
  `import`, `bootstrap`, `consultation` (the prompt-budget role for agent
  operations), `launch`, `setup`, `verify`, or `resource-cleanup`.
- `tool`: the agent kind with its resolved binary, or the script that ran.
  Command arguments never enter a summary.
- `exit_status` when the process ran, `signal` when it was killed; a launcher
  failure reports no exit status at all, which is itself the fact.
- `outcome`: a stable classification — `exit_failure`, `launch_failed`,
  `invalid_result`, `error_reported`, `cancelled`, `timeout`, `tree_changed`,
  `signaled`, `retry_limit`, `cleanup_already_absent`, or `unknown`. An exit
  failure of the cleanup logic and a cleanup that failed only on
  already-absent paths (the retry case for a script that finished its
  removals) are distinguishable by outcome, exit status, and excerpt — the
  audit of the reported cleanup wrapper failure found the generated wrapper's
  `set -e` does not break the documented check-before-remove idiom
  mid-script, so the wrapper is unchanged and the summaries carry the
  distinction.
- `cause` and `context`: the agent's own reported failure text, or a bounded
  excerpt of the failing command's or cleanup script's own log. The excerpt is
  at most 2 048 bytes, keeps the tail where failures are reported, is
  sanitized of control sequences, and is scrubbed of credential shapes
  (headers, assignments, URL user information, well-known key prefixes,
  private key blocks). Agent session streams are referenced as evidence and
  never excerpted: private reasoning does not enter summaries.
- `evidence`: Looper-owned references — the event log, invocation record,
  command script, verify log, or cleanup log — that stay inside the runtime
  directory. A failed check also retains its summary as
  `checks/<commit>/failure.json` beside its logs, so the record survives even
  when the journal is unread.

Cleanup failures keep their `resource-cleanup-error` notice with its
unchanged message and gain the summary beside it. `worker_activity` events
add one bounded, scrubbed error line to "Tool reported an error", so severity
is readable from the journal without opening a raw session log. A failure
that ends a run maps onto the version 2 error document (`agent_failed`,
 `launch_failed`, `verification_failed`, `cleanup_failed`) with the exit
status and outcome in `observed`, the evidence references, and the focused
query as a next action.

## Requests

Every mutation is a durable request: `add-task`, `add-plan`, `settings`,
`requeue`, `stop`, `cancel-task`, `amend-task`, and `recovery`.
The lifecycle states, their writers, and their terminal flags are
defined once in `docs/contracts/request-states.json`; the receipt and record
schemas enumerate exactly those names, and a test cross-checks the three
lists.

- A returned receipt names a durable request. It is not an applied result.
- Since AX-011 every mutation command returns that receipt immediately, under
  both contract selections: `add-task`, `add-plan`, and online `config`
  publish their envelope and print `looper.request.receipt` at once, and
  `stop` returns the same shape naming the derived stop request identity.
  Plain text renders the same facts. The controller's own phase never
  silences the caller.
- The receipt's `wait` object names the finite wait that observes this
  request and the bound it suggests (AX-013): `looper wait --contract v2
  --request <id> --timeout 30s`, with the seconds the command's own `--wait`
  would use (30; add-plan 60). The note states the rule the whole wait
  contract keeps: a timeout is an outcome of the wait, never a cancellation
  of the request.
- Waiting is explicit and bounded: `--wait` opts in and `--wait-timeout
  <seconds>` bounds it (default 30; add-plan 60). A satisfied wait reports
  the request record under version 2 — the applied task identifiers, plan
  coverage, adjustments, and gates — and the acknowledgement document the
  command printed before under the legacy contract. `looper stop --wait` is
  the durable stop wait AX-022 owns; the flag refuses there today.
- A wait timeout is an outcome of the wait. It never cancels or rejects.
- A caller-supplied idempotency key scoped to the project and operation makes
  retries safe: same key and payload returns the original request, changed
  payload is a conflict, and identical content under different keys stays
  possible (AX-012).
- Pass a key with `--idempotency-key <key>` on `add-task`, `add-plan`,
  `cancel`, and `config`. The option is a usage error everywhere else; `looper stop`
  derives its request identity from the controller it targets and is
  retry-safe without one. A key is 1 to 128 bytes of valid UTF-8 without
  control characters, a bound the receipt schema publishes and the envelope
  loader enforces, so a hand-written envelope cannot smuggle a larger one
  into the durable receipts.
- The key resolves inside the request type that recorded it and inside one
  run's receipts, so the same string under another operation or another
  project is a different key. The payload digest is canonical — transport
  fields such as the request identifier, the session, and the submitter never
  take part, so a retry after a controller restart still matches — and a plan
  import's digest addresses the captured plan bytes, never their paths.
- Resolution happens twice. The submitting command answers from the retained
  receipts before it publishes: a match returns the original request as the
  immediate receipt with `duplicate: true` (or as the record a `--wait`
  reports when the original already decided), and a changed payload is the
  structured `idempotency_key_conflict` refusal. The controller resolves the
  key again when it claims the envelope, so two submissions that race ahead
  of any receipt still cause one mutation: the second answers from the first
  request's receipt. A key whose original is still running is refused with
  the original named, never re-driven.
- Receipt retention bounds the guarantee. A request that has not decided
  keeps its receipt, and at most the newest 200 decided receipts stay per
  run, oldest pruned first. A key therefore answers for as long as its
  receipts are retained; once they are pruned, the same key behaves as fresh.
  Acknowledgement files are separate transport and are swept after ten
  minutes, which is why the receipts, not the acknowledgements, are the
  durable answer a key resolves against.
- A controller advertises the request types it supports. Validation happens
  before an envelope is published and again when the controller claims it, so
  an unsupported request is refused instead of becoming a dead letter (AX-008).

## Retained-work guard evidence

A guard that retains work never asks a caller to guess (AX-015). Every guard
document carries the resolved project, task-file, and runtime paths, the
expected and observed digests or revisions, and the retained task identities
in `observed.retained_tasks`, with `looper inspect <id> --json` as the
read-only route into one retained attempt.

- Changed paths stay bounded: the listing names the first 20 paths, a sibling
  `_count` key carries the complete number, and an evidence entry names the
  `git diff --name-only` command that lists every difference.
- A comparison that cannot run is unknown evidence, never an empty listing:
  `observed.<comparison> = "unknown"` plus the reason in `unknown_evidence`.
- The backlog guards add the parsed comparison: which task contracts changed,
  were added, or were removed. A formatting-only edit reports an identical
  parsed backlog while the digest still differs; the guard keeps refusing
  until AX-018 defines semantic acceptance.
- A branch guard reports an unborn HEAD as `head: "unborn"` with
  `unborn_head: true`, instead of presenting an empty revision as movement.
- The publication collision keeps the code `project_tree_changed` and expects
  two trees (`accepted_tree`, `verified_tree`) with the changed paths for
  each, so it never reads as the startup tree guard.
- Every guard error document carries `looper recovery --preview --contract
  v2 --json` as its first next action: the preview (below) is the supported
  recovery operation a refusal points at.

## Recovery preview

`looper recovery --preview` turns a guard refusal into a decision basis
without acting (AX-016). It is read-only: the store opens through a
read-only connection, the git queries run with optional locks disabled, and
no file, database record, process, or Git ref changes.

- The document (`looper.recovery.preview`, version 2 only for `--json`)
  compares the task file, the branch, the working tree, and a retained
  publication against the saved contract, then reports the startup guard
  verdicts with the same codes the guard errors carry.
- The comparisons state what they cover. The working-tree listing names
  tracked, staged, and untracked non-ignored paths and says that the startup
  guard additionally hashes ignored content; a comparison that cannot run
  reports `unknown` with its reason, never an empty difference.
- The choices enumerate what is supported — `resume` (apply through
  startup), `publish-retained`, `restore-task-file`, `import-backlog` — each
  with what it preserves and what it changes, and refuse force actions:
  `discard-retained` is listed as `unsupported`, and a restore whose discard
  cannot be evaluated is `unsafe` with the missing evidence named.
- The `token` value is a digest of the saved run (store identity, export
  digest, head, branch, tip, published tree, retained attempt contracts,
  retained publication) and the observed task-file digest, head, branch, and
  working-tree listings. The checked application AX-017 owns re-derives it
  and rejects a stale preview.
- The document carries the shared 8 192-byte budget, and `limits.bytes_budget`
  is a true statement about the emitted bytes. Every content-derived string —
  digests, heads, branch names, listing paths and commands, guard details,
  choice text — is bounded by the bytes the JSON encoder emits, so
  escape-heavy saved content shrinks with the fit ladder instead of expanding
  past it. Digest- and revision-shaped fields are bounded by the pattern
  maximum their schema already pins, which a legitimate value never exceeds,
  and the ladder ends in a terminal fit whose strings are one rune at worst,
  so a valid document always fits whatever the saved records hold.
- Plain output renders the same document as text under both contract
  versions; `--json` selects the version 2 document. With no saved run the
  command returns `run_not_found`.

## Recovery application

`looper recovery --apply <choice> --token <token>` applies one choice the
preview enumerated (AX-017). Two transports share one checked application:
a live controller claims a `recovery` request envelope at a loop boundary —
the same common request protocol `cancel-task` uses, with the immediate
receipt, the durable record, the caller key, and the claim-time capability
and session gates — and a stopped run applies under the repository lock the
controller itself takes, with no agent launched.

- The application re-derives the preview token from the state it is about to
  change and refuses a stale preview without mutation: a changed task file,
  project, or saved run never replays a preview over new work. The refusal
  is a rejected record naming the stale preview.
- `restore-task-file` returns the task file to the bytes the saved run last
  accepted, discarding the external edit. The restore source is byte-pinned:
  the retained accepted copy when it holds exactly the saved digest, else the
  canonical projection of the saved backlog when its bytes digest to the same
  value. Without a byte-exact source the application keeps the edit and
  reports the limitation. The write is atomic, so a crash leaves either the
  edit or the complete accepted bytes; a crash between the write and its
  receipt leaves a recoverable outcome — the next preview reports the match
  and a retry refuses without mutating anything.
- `import-backlog` applies the external task-file edit. An equivalent edit
  (AX-018) adopts the observed bytes as the accepted export without moving
  the baseline. A real edit imports through the T058 retained-backlog import
  — the same path the live boundary and the startup recovery run, never a
  second import — and applies only when every retained result replays, so a
  resumed run re-prepares and re-verifies the retained commits without
  repeating implementation. An attempt without a replayable result keeps the
  edit and reports the concrete limitation naming the restore alternative.
- A live controller owns its drained boundary: an import while work is in
  flight is refused, because the boundary that drains the work imports the
  edit itself. A restore applies at any boundary. A stopped run has no live
  work and applies either choice under the lock.
- The unsupported choices stay refused with their concrete limitation:
  `discard-retained` because no supported discard exists, `resume` and
  `publish-retained` because the controller start owns them (`looper`). An
  unknown choice is refused naming the choice. Every refusal retains the
  work and records a rejected record.
- `--wait` waits bounded for the decided record against a live controller
  (a timeout leaves the request running); a stopped-run application decides
  synchronously and reports the record at once. `--idempotency-key` makes
  retries return the original request; a changed token under the same key is
  a conflict. With no saved run the command returns `run_not_found`.

## Preflight

`looper preflight` shows the execution configuration before agents launch
(AX-023). It is read-only: the profile and any saved run open for reading,
executables resolve through PATH, ownership probes an existing lock without
creating one, and no controller, agent, runtime directory, or saved record is
created.

- The document (`looper.preflight`, version 2 only for `--json`) resolves
  executable paths, the coding and review agents with their sources, models
  and effort with their sources and resolutions, the launch mode, once
  semantics, and the effective verification and setup commands with the layer
  that supplied each: flag, environment, profile, the saved plan, a recovered
  command, or the planner at first run.
- Provenance is exact, including the cases that tempt a guess: an empty
  review agent reports `derived` from the coding agent, an unset model
  reports `provider_default` (the agent binary decides at launch; Looper does
  not resolve the provider default), and a model for an agent kind that is
  not configured reports `not_applicable`.
- Credentials, environment values, and raw agent arguments never enter the
  document. Command text is scrubbed of credential shapes, the environment
  section lists recognized variable names and presence only, and the sandbox
  posture is a named mode, never the raw argument list.
- What the report did not verify stays in `unchecked`: agent authentication,
  quota, and billing are never probed, provider default models stay
  unresolved, and a planner-chosen verify command names its detected
  candidate as a hint only.
- The document carries the shared 8 192-byte budget, and `limits.bytes_budget`
  is a true statement about the emitted bytes. Every content-derived string —
  a binary's requested and resolved values from the environment, a command's
  text and first word, model and effort values, plan paths, finding messages —
  is bounded by the bytes the JSON encoder emits, so escape-heavy values
  (an ampersand becomes six bytes) shrink with the ladder instead of
  overflowing it. The ladder ends in a terminal fit whose strings are one
  rune at worst, so the document stays valid JSON at or under the budget
  whatever the configuration holds; the schema-constrained literals (agent
  names, sandbox modes, sources, states) never shrink, so every fit renders
  a schema-valid document.
- Missing agent binaries and missing core tools are error findings that make
  the command exit 1, like doctor. Missing command executables, an
  unreadable saved run, unreadable startup plans, and a controller that owns
  the repository are warnings. A saved run whose state path cannot be read
  for any reason other than absence is an unreadable saved run: the warning
  keeps the reason, and the recovered-command and saved-plan layers report
  the planner instead of being silently dropped.
- Plain output renders the same document as text under both contract
  versions; `--json` selects the version 2 document.
- Before a foreground run starts, the same facts are emitted as one concise
  line (plain output) or one NDJSON `launch` event (version 2 JSON output)
  ahead of any controller or agent. The line reads the saved run read-only,
  exactly as preflight does, so the effective verification and setup commands
  name the layer that will supply them on a resume — the configuration, a
  recovered command, or the saved plan — instead of defaulting to the planner.
  A saved run that cannot be read supplies no layer, matching the preflight
  fallback. Every value the line embeds — the resolved binary path, the
  command text and its source, the model and effort — is scrubbed and clamped
  by the bytes the JSON encoder emits, so an oversized or escape-heavy
  configured value cannot grow the plain line or the launch event without
  limit. The legacy JSON stream keeps its byte-compatible shape.

## Run discovery

`looper runs` answers "which saved runs exist here, and how do I select
one" (AX-025). It scans only the resolved configured base directory
(`LOOPER_BASE_DIR`, default `~/.looper`) and only Looper-owned runtime
directories named `<project>-<hash>`; private agent session directories are
never scanned, and no run is started, stopped, or mutated.

- The document (`looper.runs`, version 2 only for `--json`) lists one entry
  per run, newest activity first: the project root and task path the saved
  state records, whether that root still exists, the retained work with
  bounded task identities, the backlog and session counts, the last known
  outcome exactly as the store recorded it (with the shutdown reason and
  exit code when one was recorded), ownership freshness, build evidence,
  and the exact `cd`-based command that selects the run.
- Ownership freshness comes from controller metadata plus process liveness:
  `owning`, `stale`, `stopped`, or `unknown`. A clean shutdown removes its
  own metadata, so `stopped` is normal; `stale` names a controller whose
  process is gone. The repository lock is not probed for foreign projects.
- Build evidence survives the controller's own record: when
  `controller.json` is gone, the newest session event still carries the
  build recorded when that session began.
- Missing and unreadable records stay entries with their reason, never a
  failed listing: a runtime directory without state records reports
  `missing`, an unreadable database reports `unreadable` with the error,
  and an entry whose records cannot be read keeps an explicit hint to where
  they stay instead of guessing a root.
- The page is bounded (`--limit`, default 20, at most 100) and the byte
  budget may shrink it further; `limits` states the bound that applied and
  `omitted` counts the rest. Every string bound counts the bytes the JSON
  encoder emits, so escape-heavy identities shrink with the fit ladder
  instead of expanding past it, and the ladder ends in a terminal fit that
  keeps the newest run alone with one-rune strings — so the listing stays at
  or under its published 8 192-byte budget for any content and
  `limits.bytes_budget` is a true statement about the emitted document. An
  empty base directory is a bounded result with a start hint, not an error;
  a base directory that cannot be read returns `base_unreadable`.
- Reading a run's records creates no file: a run no live controller owns
  opens immutable, so a plain read-only attach cannot materialize the
  WAL sidecars beside the database. A live controller, or a write-ahead log
  a crashed controller left behind, uses the through-WAL read its own
  recovery already owns.
- The one-shot observation commands attach the same way (T105): `status`,
  `inspect`, `events`, `tail`, `doctor --json`, `preflight`, and
  `recovery --preview` read a cleanly stopped run immutable, so they leave
  no `state.db-wal` or `state.db-shm` beside its database. A followed
  event stream and the dashboard hold one connection across the
  controllers that may start later, so they keep the through-WAL read. A
  finite wait (AX-013) starts from the same one-shot decision and
  re-attaches through the WAL read once a controller appears, so it leaves
  a stopped run untouched without missing a later controller's commits.
- No-run errors across the CLI name the searched locations: the resolved
  project root, the task path, the base directory, and the runtime state
  location, with `looper runs --contract v2 --json` as the discovery next
  action (AX-025).

Plain output renders the same document as text under both contract
versions; `--json` selects the version 2 document.

## Request queries

`looper requests` answers "what happened to the work I submitted" without
the acknowledgement directory or the state database (AX-010). The records
come from the durable receipts every mutation commits (AX-009) and from the
public envelope directories, so a run whose controller stopped answers too.

- `looper requests <id> --contract v2` returns one `looper.request.record`:
  the lifecycle state from `docs/contracts/request-states.json`, the age,
  the wait reason a waiting request names with its own age, the deciding
  controller session, the affected tasks, the plan coverage an import
  reported (added tasks, covered requirements, conflicts, adjustments), the
  recorded outcome, and safe error evidence for a rejection or failure.
- `looper requests --contract v2 [--after <id>] [--limit N]` returns one
  bounded page of the same records, oldest first. `--after` is exclusive;
  the page's continuation command names the exact next call. Terminal
  receipts prune from the oldest at the retention bound, so pages stay
  reachable; a continuation identifier the run no longer holds returns
  `request_after_unknown` with a restart action instead of a silently
  empty page.
- A request no receipt or envelope names returns `request_not_found` with
  the count of records the run does hold. A run without saved state returns
  `run_not_found` naming the searched locations.
- The document stays at or under 8 192 bytes through a fit ladder that
  shrinks strings first and drops collection entries second, counting the
  bytes the JSON encoder emits; the list reports what the page omitted, so
  every record stays reachable through its pages.
- Status and doctor carry the bounded pending-request summary: queued
  envelopes, waiting durable requests of every type, the oldest age, and —
  in the doctor's version 2 document — one bounded `waiting_on` list with
  the reason each request names. Both name `looper requests --contract v2
  --json` as the detail route.
- Doctor findings never change the exit code under version 2: a command
  that completed exits 0 while reporting an unhealthy run, exactly as the
  exit-code registry states. The legacy doctor keeps its frozen exit 1 on
  error-severity findings.

Plain output renders the same document as text under both contract
versions; `--json` selects the version 2 document and refuses the legacy
contract with `contract_version_required`.

## Task cancellation

`looper cancel <task-id> [--cascade]` withdraws open work through the same
checked request protocol as every other mutation (AX-019). The public
request type is `cancel-task`; it returns the immediate receipt, accepts
`--wait`, `--wait-timeout`, and `--idempotency-key`, and records durable
receipts a `looper requests` query reads like any other request.

- **Checked, never blind.** The request carries the task revision the
  caller read (the CLI computes it from the task file it was invoked
  against). The controller compares it against the live backlog: a task
  that changed, completed, left the backlog, or was already cancelled is a
  conflict that changes nothing, and a held task-file edit waits for its
  boundary import exactly as add-task and requeue do. Cancellation never
  overwrites a concurrent edit and never bypasses the export and
  publication checks: the new statuses commit through the same staged
  backlog protocol, with the same rollback on a failed export.
- **Cancelled is not done.** A cancelled task carries the `cancelled`
  status, its own terminal representation in `to-do.schema.json`. It never
  counts as a satisfied dependency, never re-enters the ready set, gets no
  plan write scope, and never inflates the done count. The compact status
  document counts it under `work.counts.cancelled` (additive; documents
  without cancellations keep their shape).
- **Dependents never strand silently.** A task with live dependents — not
  done, not already cancelled — is refused with the dependent identifiers
  in the message. `--cascade` cancels the transitive live set in one
  decision; done dependents keep their accepted history and are not
  re-cancelled.
- **Pending versus active cancellation.** With no target work in flight
  the cancellation is active: it applies at the boundary that claims it.
  When any task in the decided set holds an active attempt (running,
  verifying, or a retained result waiting on its check), the cancellation
  is decided and pending: the receipt waits with the reason `active_work`,
  the decision is durable in the saved state, dispatch skips the decided
  set, and the drive applies it at the first boundary where the work
  drained. A restart between decision and drain retains the decision. A
  waiting acknowledgement exits 0 — the decision is recorded, not failed.
- **Active results are preserved.** The in-flight worker finishes first;
  its verified result still passes the publication checks. A task that
  completed before the cancellation applied keeps its accepted result, and
  the cancellation records a rejected outcome naming it — it never discards
  work that finished. Retained results stay beside their attempts for
  `looper inspect`, whatever the task status.
- **Retries and restarts.** `--idempotency-key` scoped to `cancel-task`
  makes retries return the original outcome; a changed payload under the
  key is the usual conflict. The cancelled statuses, the receipts, and any
  pending decision all survive a restart.

Outcomes are observable through `looper requests <id>` (waiting reason,
affected tasks, the decided set, the recorded summary), `looper status`
(the `cancelled` count, and the tasks themselves through
`--ls cancelled`), `looper inspect <task-id>` (the `cancelled` status in
the task contract; the published status and inspect schemas accept it),
and the journal (`cancel-pending` and `cancel`
events carrying the controller session, the request identity, and the task
identifiers).

## Task amendment

`looper amend <task-id> <task.json>` changes one task contract through the
same checked request protocol (AX-020). The public request type is
`amend-task`; it returns the immediate receipt, accepts `--wait`,
`--wait-timeout`, and `--idempotency-key`, and records durable receipts a
`looper requests` query reads like any other request.

- **Checked, never blind.** The request carries the task revision the
  caller's task file held and the complete amended task. The controller
  compares the revision against the live backlog: a task that changed or
  left the backlog is a conflict that changes nothing, and a held task-file
  edit waits for its boundary import exactly as add-task and cancel do.
  Concurrent amendments therefore conflict without loss — the second one's
  revision no longer matches, and its rejected receipt keeps its own
  outcome beside the first one's application.
- **Amendable and not.** The amendment may change title, priority,
  details, steps, `depends_on`, files, blockers, tags, and `unblock_when`.
  Identity, status, and `created_at` are not amendable: a renamed task is
  a different task, status transitions belong to the engine and
  `looper cancel`, and creation history stays with the accepted record.
  Control tags — `project-done`, `looper:operator`, `looper:deferred`,
  `looper:finding:*` — cannot be added or removed, because they encode
  engine-owned decisions (completion, ownership, deferral, finding
  identity); the task-file edit path remains the route for those, with the
  import guard it already answers to.
- **Validated before committing.** The amended backlog must validate —
  dependency cycles, unknown dependencies, and duplicate identifiers are
  refused — and every entry of the amended `files` list must be a valid
  write path (relative to the repository root, inside it, literal). An
  invalid amendment changes nothing; it is refused, never quarantined.
- **The safe boundary.** Work that holds the task's contract — running,
  verifying, ready, integrated, or resume attempts, exactly the set the
  startup contract guard compares — cannot change under a worker: the
  amendment is refused naming the held phase, and the caller waits for the
  work to finish (or cancels it) before amending the drained task. Done
  and blocked attempts keep their recorded results for inspection; the
  resume path already skips a blocked attempt whose contract moved.
- **Completed work stays historical.** A done task is never rewritten: the
  amendment creates a follow-up task `<id>-amend-<n>` carrying the changed
  requirements (status `todo`, a fresh identity), while the accepted task,
  its attempts, and its results stay byte-identical. A cancelled task
  refuses; decided work is not amended, and `looper add-task` serves new
  requirements.
- **Plan safety.** An in-place amendment clears the plan signature and
  drops the task's retained plan scope, so the task holds the whole-tree
  default — parallel dispatch waits — until the next planning pass rebuilds
  against the amended contract and write scope. A files-only change does
  not move the task's `Contract()` hash, so the explicit clear is what
  retires the stale scope.
- **Retries and restarts.** `--idempotency-key` scoped to `amend-task`
  makes retries return the original outcome; a changed payload under the
  key is the usual conflict. Applied amendment receipts and the accepted
  history both survive a restart: the receipt commits in the same save as
  the mutation.

Outcomes are observable through `looper requests <id>` (the affected task,
the changed fields, the follow-up identity for completed work), `looper
inspect <task-id>` (the amended contract and the untouched attempts beside
it), and the journal (`amend` events carrying the controller session, the
request identity, the task identifier, and the changed fields).

## Transport exit codes

Exit codes report how a call ended on the transport. Operation state is a
field of the returned document. The codes, their meanings, retry guidance,
and today's mapping are in `docs/contracts/exit-codes.json`. In short:

| Code | Name | Meaning |
| --- | --- | --- |
| 0 | ok | The command completed; read the document for the outcome. |
| 1 | operation_failed | The operation ran and ended rejected or failed, or the command refused to act. |
| 2 | usage | The command line itself was invalid; nothing was attempted. |
| 3 | unavailable | The run, store, or controller could not be read. |
| 4 | incompatible | Contract version or capability mismatch, or a stale controller session. |
| 5 | wait_timeout | A finite wait timed out; the operation continues. |
| 6 | cancelled | The caller interrupted the command; work is retained. |

Today's commands return only 0, 1, and 2 under the legacy contract. Since
AX-014, a call that selects version 2 returns the full set for failures: 3
for state a command could not read, 4 for capability, contract, and session
mismatches, and 5 for a bounded wait that timed out, each with one
`looper.error` document on standard output. The `legacy_equivalent` field in
the registry names where each outcome lands for callers that have not opted
in. Streaming commands (`run`, `start`, `tui` under `--json`) append the
error document to their progress stream; AX-022 owns their version 2
lifecycle contract. That contract is fixed: the launch event is the first
value, journal events stream only after startup succeeded — a startup that a
guard refuses emits no journal line, and its unclean-previous-session note
waits for the store instead — and the error document, when the command
fails, is always the final value.

## Compatibility fixtures

Two executable guards keep these contracts honest:

1. `go test ./internal/contractschema` validates every version 2 fixture
   against its schema, rejects every invalid fixture, and cross-checks the
   exit-code and request-state registries against the schemas.
2. `go test ./internal/looper -run 'TestLegacyContract|TestCommandSurface'`
   produces today's documents through the production code paths from one fixed
   scenario, validates them against the frozen legacy schemas, compares their
   structure with the recorded shape fixtures, and checks that the command
   registry in `docs/contracts/commands.json` matches the parser.
3. `go test ./internal/looper -run 'TestCommandHelp|TestSkill'` keeps the
   contextual help and the agent skills executable: every command the client
   lists has a help block whose examples parse and whose documents exist and
   match the registry, every `looper` line a skill teaches parses, and an
   unknown command or help topic is a usage error that starts nothing.

The recorded shapes are structural fingerprints: object keys, array item
structures, and JSON types. Values, timestamps, and paths do not appear, so
the fixtures stay stable across runs while still catching an added, removed,
renamed, or retyped field anywhere in the document. The scenario in
`internal/looper/compatibility_test.go` populates the optional state fields
(imports, publication, recoveries, recovered commands, export boundaries,
review markers, attempt summaries and verification records), so their shapes
are pinned too. Two fields stay loosely typed in the schema: the presentation
`view` inside the observation, which is not a public contract, and
`pending_run_repair`, whose analysis payload the controller owns.

To change a contract shape deliberately:

1. Make the change.
2. Record the new shape: `go test ./internal/looper -run TestLegacyContract
   -update-contract-fixtures`.
3. Update the affected schema in `docs/contracts/schemas/`.
4. Add a migration entry to `docs/AGENT-CONTRACT-MIGRATION.md`.
5. Run `go test ./...`.

For legacy documents this sequence is only permitted for the migration paths
the guide names. An additive change to a legacy document still breaks
byte-compatibility and must ship behind contract version 2 instead.

## Inventory of existing consumers and schemas

### CLI commands and their documents

| Surface | Today's behavior |
| --- | --- |
| `status --json` | `statusReport` in `internal/looper/observe.go` embeds the `*State` inline plus `observation`, `timing`, and `statistics`. The projection hides saved-state machinery the frozen schema lists no key for: the `export_semantics` fingerprint (T100), the `requests` receipts (AX-009), and the `cancellations` list (AX-019) stay in the saved state only. Unbounded otherwise. Since AX-004 `--contract v2` serves the bounded `looper.status.compact` document and `--full` keeps this complete document. |
| `inspect <id> --json` | One document with the task, every attempt, workers, checks, log paths, and task timing. Unpaged. Since AX-005 `--contract v2` serves the bounded `looper.inspect.compact` document — the task contract with its digest and bounded blockers and excerpts, the current attempt, paged attempt history (`--after`, one-based ordinals) and check history (`--checks-after`, check row identifiers), timing provenance, evidence references, and `limits`/`detail` routes — and `--full` keeps this complete document. |
| `events --json` | NDJSON from `c.After` (default zero), draining 500-row pages. Since AX-006 the new `--limit`, `--task`, and `--kind` flags bound or filter the same lines when passed, `--full` drains explicitly, and `--contract v2` returns one bounded `looper.events.page` instead. |
| `doctor --json` | Observation plus findings with `code`, `severity`, `message`, `suggested_action`. |
| `version --json` | Build identity: release, revision, modified. `--contract v2` serves the capabilities document, including the live controller's advertised request types. |
| `add-task --json` | Prints a `SettingsAck` after waiting up to 30 seconds. |
| `add-plan --json` | Prints a `planImportAck` after waiting up to 60 seconds. |
| `status`, `inspect`, `events`, `doctor` (plain) | Counts, blockers, timing, and stored event messages. Since AX-004 a piped plain status renders the compact summary: controller state, active work with output evidence, the waiting reason, findings, and the cursor. An interactive terminal keeps the status board. |
| `stop` | Text outcome, exit 1 when nothing was stopped. Binds the request to the owning session. |

### Consumers

- `skills/looper-diagnose/SKILL.md` teaches the version 2 workflows: the
  compact `status --contract v2`, `doctor --contract v2`, the `requests`
  receipt query, focused `inspect --contract v2` with paged history, the
  `--kind failure` evidence query, cursor-based `--after` continuation, the
  finite `looper wait`, `runs` discovery, and the read-only recovery preview
  — with no private-state detours. `TestSkillCommandsParse` in
  `internal/looper` parses every `looper` line the skill teaches, so a
  renamed command fails the skill that still names it.
- `skills/looper-backlog/SKILL.md` teaches the request commands for live
  changes (add-task, add-plan, cancel, amend, with receipts, bounded waits,
  and caller keys) and keeps file edits for requested maintenance and idle
  runs, with the recovery preview as the supported path for a refused edit.
- `scripts/smoke.sh`, `scripts/runtime-test.py`, `scripts/build-test.py`,
  `scripts/install-latest-test.py`, and the package smoke checks call the CLI
  and parse its output.
- `internal/ui` renders the dashboard from `runtime/view.json` snapshots and
  the read-only store; it must not grow a second mutation path.
- `adapters/measuretwice` receives assessment evidence.
- Agents themselves: the audit recorded 626 calls across 40 sessions.

### On-disk schemas

| File | Schema | Version field |
| --- | --- | --- |
| `to-do.json` | `to-do.schema.json`, written beside the task file only when no schema exists (runtime output this repository ignores; the shipped reference copy is tracked at `docs/to-do.schema.json`); the same document is the `backlogSchema` constant in `internal/looper/types.go` | `schema_version: 1` |
| `runtime/profile.json` | `internal/looper/profile.go`; unknown keys are preserved | `version: 1` |
| `runtime/controller.json` | `ControllerInfo` in `internal/looper/controller.go` | `version: 1` |
| `runtime/view.json` | `ui.ViewSnapshot` in `internal/ui/view.go` | `version` per snapshot |
| `runtime/requests/<id>.json`, `processing/`, `acks/<id>.json` | `SettingsRequest` and `SettingsAck` in `internal/looper/settings.go` | `version: 1` |
| plan-import requests and receipts | `planImportRequest`, `ImportRecord` in `internal/looper/imports.go` | request `version: 1` |
| `runtime/stop` | scoped `stopRequest` | `version: 1` |
| import artifacts and context records | captured plans, `context.json`, `invocation.json` | recorded per artifact |

### In-tree fixes these contracts reuse

| Fix | Status | Use in this plan |
| --- | --- | --- |
| P2-004 scoped stop requests | done | The stop request type in version 2 reuses the session binding in `internal/looper/cli.go` and `controller.go`. |
| P2-006 shallow-history build stamps | done | `builds.client` and `builds.controller` reuse `currentBuild` and `scripts/build-ldflags.sh`; identity stays executable-derived (AX-024). |
| Detached readiness pipe | done | `run-*.started_at` and startup state come from the readiness handshake in `internal/looper/daemon.go`; AX-022 defines start success as a queryable run. |
| T026 plan-import crash recovery | done | The durable import receipt already survives restart; version 2 generalizes it to every request (AX-009). |
| T028 stop during startup | done | Startup-aware stop handling stays the reference for `waiting_on: startup`. |
| T050 nil-plan dispatch guards | done | Compact status reports the missing-plan wait reason instead of a new state. |
| T052 root snapshot exclusion | done | Observers read the store, not the runtime tree; status stays bounded without snapshot interference. |
| T057 held backlog edits, T066 stop during hold, T068 hold-wait pacing | done | The `waiting` request state and `backlog_hold` waiting reason describe these boundaries. |
| T069 import replay orphaning | done | Receipt replay rules in AX-009 keep `readPlans`/`persistPlans` ownership. |
| T041/T051/T055 activity evidence | done | Compact active-work entries carry the same bounded activity facts. |
| Doctor finding pattern | done | The version 2 error contract extends `HealthFinding`'s code, message, and action shape to every failure (AX-014). |

## Ownership and overlap

See `docs/AGENT-EXPERIENCE-OWNERSHIP.md` for the map covering every task in
the plan, including the prerequisite tasks this plan must not duplicate: T020
and T029 own request-queue recovery and restart-safe sweeping, T044 owns the
import dispatch pause, and T058 owns startup import of a retained backlog
edit. The map's status column follows the accepted backlog, and
`TestOwnershipMapStatusesMatchBacklog` in
`internal/looper/ownership_map_test.go` fails the suite when a row trails it
(T115).
