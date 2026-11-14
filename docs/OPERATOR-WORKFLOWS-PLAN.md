# Plan for operator workflows after 0.10.0

Date: 2026-11-14.

Status: implementation proposal. This document does not change runtime behavior.

Source baseline: `413502bd8d13cf08b491d01468f4d21324365330`.
Line references below refer to this commit. They can move during implementation.
The installed executable reports `0.10.0 (1d7a614f9c25)`.
The local controller is stopped. The external run in the feedback was not inspected.
Treat its task IDs, failures, and manual recovery steps as operator reports.

## Outcomes and priority

| Priority | Feature | Observable outcome |
| --- | --- | --- |
| 1 | Checked publication to the project branch | Commit accepted work without a manual reset or file comparison. |
| 2 | Fresh requeue with stable task identity | Retry with current role settings while preserving task references and attempt history. |
| 3 | Non-interactive configuration | Define machine agents and select project roles without a terminal or handwritten configuration files. |
| 4 | Structured blocker reasons | Distinguish operator prerequisites, deferred work, execution failures, and unknown causes through structured output. |

The first two features remove the most delicate manual steps.
Configuration makes provider changes repeatable.
Blocker reasons make monitoring more useful without automatic retries.

Recommended delivery order: shared contract rules, publication, configuration,
fresh requeue, then blocker classification. Fresh requeue can use the existing
configuration form before the non-interactive configuration work lands.
The priority order and implementation order can therefore differ.

## Current behavior and source map

### Publication and branch recovery

| Reference | Verified behavior | Design consequence |
| --- | --- | --- |
| [Recovery documentation, line 43](recovery.md#L43) | Accepted files appear as unstaged edits. The project branch and index stay unchanged. | A branch handoff adds behavior beyond current publication. |
| [Repository exclusions, line 113](../internal/looper/git.go#L113) | The task file, its schema, and an internal runtime directory are excluded from snapshots. | The accepted source tree is not necessarily a complete project commit tree. |
| [`Repository.SnapshotFrom`, line 195](../internal/looper/git.go#L195) | Snapshots use a temporary index and account for known ignored files. | Reuse its content rules for apply-time checks. Keep preview free of object writes. |
| [`Repository.Commit`, line 285](../internal/looper/git.go#L285) | Internal commits use the Looper identity. | A project commit needs a separate author and signing policy. |
| [`Repository.Ref`, line 295](../internal/looper/git.go#L295) | Ref updates do not supply an expected old object ID. | Add a compare-and-swap update for the project branch. |
| [`Repository.Apply`, line 411](../internal/looper/git.go#L411) | Publication applies a checked binary patch to working files. | A branch handoff must not call this to overwrite unrelated edits. |
| [`Engine.publish`, line 380](../internal/looper/integration.go#L380) | Verified work updates the private ref, accepted tree, and integration state. | Handoff starts from `State.Published`, after publication has completed. |
| [`State`, line 509](../internal/looper/types.go#L509) | `Head`, `Branch`, `Ref`, `Tip`, `Published`, and `Publication` have separate roles. | Do not treat the run tip as interchangeable with the project branch tip. |
| [`compareBranchRecovery`, line 37](../internal/looper/branch_recovery.go#L37) | Resume compares branch ancestry, staged changes, and retained trees. | A handoff commit must satisfy resume checks. |
| [`recoverCommittedHead`, line 275](../internal/looper/branch_recovery.go#L275) | Owner commits can update the saved baseline. | Integrate with this behavior rather than bypassing recovery. |
| [`applyRecoveryRequest`, line 97](../internal/looper/recovery_apply.go#L97) | Recovery checks a token before applying a supported choice. | Reuse its checked-operation pattern. |
| [`publish-retained` refusal, line 170](../internal/looper/recovery_apply.go#L170) | Retained publication requires controller resume. | This choice does not currently commit accepted work to the project branch. |
| [`applyStoppedRecovery`, line 475](../internal/looper/recovery_apply.go#L475) | A stopped operation holds the repository lock and uses an engine without workers. | Use this ownership model for stopped publication and stopped requeue. |
| [`recoveryToken`, line 1286](../internal/looper/recovery_preview.go#L1286) | Working evidence includes a digest of changed path lists. | A publication token needs file-content evidence, not only changed path names. |

### Requeue and attempt reuse

| Reference | Verified behavior | Design consequence |
| --- | --- | --- |
| [`requeueTask`, line 226](../internal/looper/dashboard_data.go#L226) | The dashboard submits a requeue request with a task revision. | Preserve this request path for ordinary retry. |
| [`applyRequeueRequest`, line 266](../internal/looper/dashboard_data.go#L266) | Requeue clears blockers and sets `todo`. It retains attempts. | Add an explicit fresh mode. Do not change the existing mode silently. |
| [`TestRequeuePreservesContractAndRetainedAttempt`, line 200](../internal/looper/dashboard_data_test.go#L200) | A test requires retained work to survive ordinary requeue. | Keep this test and add separate fresh-mode tests. |
| [`Attempt`, line 437](../internal/looper/types.go#L437) | Each attempt stores its agent profile name and retained artifacts. | Keep the old attempt as history. Create a new attempt for fresh work. |
| [`Engine.start`, line 1248](../internal/looper/engine.go#L1248) | Dispatch selects a resume attempt or a matching blocked attempt before creating one. | Retire every eligible old attempt for the task, or dispatch can select it again. |
| [Retained attempt reset, line 1298](../internal/looper/engine.go#L1298) | Reuse clears result fields and removes the worker receipt. | Fresh mode must avoid this path for retired attempts. |
| [New attempt creation, line 1322](../internal/looper/engine.go#L1322) | A new attempt stores the current configured worker name. | Fresh mode naturally selects the current worker when dispatch creates the attempt. |
| [Worker invocation, line 1093](../internal/looper/engine.go#L1093) | Execution uses `a.Agent`. | An old attempt can keep `claude` after the worker role changes. |
| [Profile resolution, line 108](../internal/looper/agent.go#L108) | Invocation resolves a profile and records the executable and adapter. | The code does not prove that an entire original command is replayed unchanged. |
| [`canonicalPayload`, line 53](../internal/looper/idempotency.go#L53) | Request digests cover operation fields and omit transport fields. | Add fresh-mode evidence to the digest. |
| [`cancelBlockingAttempt`, line 102](../internal/looper/cancel.go#L102) | Several phases are terminal for cancellation checks. | Audit phase-dependent logic when adding attempt retirement. |

### Configuration and blocker diagnosis

| Reference | Verified behavior | Design consequence |
| --- | --- | --- |
| [`runConfig`, line 207](../internal/looper/settings.go#L207) | Offline configuration takes the repository lock. The editor requires a terminal. | Route explicit edits before the terminal check. |
| [`runConfigOnline`, line 1213](../internal/looper/settings.go#L1213) | Online edits become settings requests. The editor also requires a terminal. | Explicit edits should use the same submission path. |
| [`patchProfile`, line 735](../internal/looper/settings.go#L735) | Typed patches validate known project fields. | Extract a shared patch service instead of adding a separate serializer. |
| [`applySettingsRequest`, line 799](../internal/looper/settings.go#L799) | Session and profile digests guard online application. | Keep these checks for headless clients. |
| [`applyLiveSettings`, line 969](../internal/looper/settings.go#L969) | Some settings apply at boundaries. Others wait for restart or remain overridden. | Return a per-field effect. Saving does not imply immediate application. |
| [`Profile`, line 20](../internal/looper/profile.go#L20) | Project roles use `agent` and `review_agent`. Unknown keys survive saves. | Map public `worker` and `reviewer` names onto existing fields. |
| [`AgentCatalog`, line 45](../internal/looper/agent_profiles.go#L45) | Machine definitions and role defaults are separate from project preferences. | Give machine edits an explicit scope. |
| [`AgentProfile.validate`, line 67](../internal/looper/agent_profiles.go#L67) | Commands are executable names or absolute paths. No shell expression is accepted. | Preserve these rules in non-interactive input. |
| [`saveAgentCatalog`, line 142](../internal/looper/agent_profiles.go#L142) | Catalog writes use a lock and revision check. | Reuse this concurrency protection. |
| [`HealthFinding`, line 242](../internal/looper/observe.go#L242) | Findings have code, severity, task ID, message, and action. | Add a version 2 projection for structured blocker reasons. |
| [`diagnose`, line 250](../internal/looper/observe.go#L250) | Deferred tasks are informational. Other blocked tasks receive the same warning. | Preserve the distinction that already exists. Add the missing reasons. |
| [`Task`, line 15](../internal/looper/types.go#L15) | Blockers are free-text strings. | Persist reason metadata separately from the text. |
| [`Task.Contract`, line 164](../internal/looper/types.go#L164) | Status and blocker text do not define the acceptance contract. | Reason metadata must also stay outside the acceptance contract. |
| [`holdForOperator`, line 215](../internal/looper/findings.go#L215) | Operator prerequisites receive a control tag and resume instructions. | This is a reliable classification source. |
| [`deferTask`, line 227](../internal/looper/findings.go#L227) | Deferred work receives a control tag and condition. | This is another reliable classification source. |
| [`Engine.hold`, line 1367](../internal/looper/engine.go#L1367) | Runtime holds currently pass a string into a blocked summary. | Pass a typed reason from the failure site. |
| [`applyResult`, line 1373](../internal/looper/engine.go#L1373) | Task application accumulates blocker text across attempts. | Define when current reason metadata replaces old metadata. |
| [`FailureSummary`, line 71](../internal/looper/failure.go#L71) | Failure summaries retain bounded structured evidence. | Link reason metadata to this evidence without copying raw logs. |

## Shared implementation rules

### Contracts and compatibility

The current client command list is in
[`capabilities.go:44`](../internal/looper/capabilities.go#L44).
The current request vocabulary starts at
[`capabilities.go:54`](../internal/looper/capabilities.go#L54).
The parser starts at [`config.go:679`](../internal/looper/config.go#L679).
Command dispatch includes mutations at [`cli.go:231`](../internal/looper/cli.go#L231).

Apply these rules to every feature:

- Add commands and flags to the parser, help registry, capability output, and command registry together.
- Preserve the legacy default. Do not combine this work with the version 2 default transition.
- Keep existing legacy documents byte-compatible for existing records and invocations.
- Define version 2 success and failure documents before adding command handlers.
- Add valid and invalid fixtures for each new shape.
- Keep compact responses within their documented budgets. Add detail routes for omitted evidence.
- Keep previews read-only. Opening a stopped database must not create write-ahead log sidecars.

The compatibility policy is in
[the migration guide, line 12](AGENT-CONTRACT-MIGRATION.md#L12).
Schema validation instructions are in
[the contract fixture guide, line 23](contracts/README.md#L23).
The byte guard is
[`TestLegacyContractBytesAreFrozen`, line 30](../internal/looper/legacy_bytes_test.go#L30).

Use separate version 2 output structures where shared runtime structures would
otherwise add fields to legacy documents. Test an old record and a new record
through both output paths. Frozen fixtures alone do not cover new metadata.

### Durable mutation rules

[`Store.Save`, line 92](../internal/looper/store.go#L92) persists the state document.
[`RequestRecord`, line 60](../internal/looper/requests.go#L60) stores mutation outcomes.
Use the existing receipt vocabulary and idempotency behavior where applicable.

- A successful receipt must describe a durable mutation.
- A refused request must preserve files, task state, attempts, and refs.
- A retried request must not create another commit or retire another attempt.
- Persist a recovery journal when one operation changes both Git state and database state.
- Do not claim a transaction spans SQLite, Git refs, and the index. It does not.
- Every partial operation must have a deterministic inspection and completion path.
- Run no agents during preview, configuration validation, or stopped mutation application.

## Feature 1: checked publication to the project branch

### Proposed command and behavior

```sh
looper publish --preview --contract v2 --json
looper publish --apply --token <token> --message "Accept completed work" \
  --idempotency-key handoff-2026-11-14 --contract v2 --json
```

Choose a single project commit as the first implementation.
Its parent is the current checked project `HEAD`.
Its source content comes from the accepted tree in `State.Published`.
It advances the current saved branch, which can be `main` or another branch.
It preserves private run refs and attempt refs.

This provides the requested branch handoff without adopting private implementation
history. Private history can include snapshots and intermediate integration work.
Directly adopting the run ref is outside the first implementation.
Document this decision in the command help and design rationale.

The command requires a stopped controller and no surviving Looper child process.
A retained interrupted attempt does not itself prevent publication of earlier
accepted work. Its partial content stays outside the project commit.
Pending publication does prevent the handoff. Show the retained result and the
resume route rather than treating the unaccepted candidate as accepted work.

Do not require a fully completed backlog. A stopped run can publish accepted work
while other tasks remain blocked or interrupted. Publishing must not mark those
tasks done or erase their attempts.

### Construct the exact commit tree

Do not commit `State.Published` directly without accounting for excluded paths.
The task file and schema can be tracked in the project but absent from that tree.
An unchecked direct commit can therefore delete them.

Define the commit tree as follows:

| Path category | Commit content | Guard |
| --- | --- | --- |
| Accepted source paths | Entries from `State.Published`, including modes and symlinks | Working source content must still match the accepted tree. |
| Tracked task file | The current accepted exported bytes | Require a valid backlog and no pending export or unimported semantic edit. |
| Tracked task schema | The existing checked project schema bytes | Validate the schema under existing project rules. Bind its content to the token. |
| Untracked task file or schema | Leave untracked | Show this omission explicitly. Do not introduce tracking automatically. |
| Runtime paths | Preserve existing tracked entries from project `HEAD`; add no runtime files | Never import database, logs, or private configuration. Report pre-existing tracked runtime paths. |
| Other excluded paths | Preserve project `HEAD` unless an explicit rule above applies | Refuse a modified excluded path without a defined inclusion rule. |
| Unrelated source changes | No commit | Refuse and list the paths. |

Enumerate exclusions using the repository object. Do not hard-code `to-do.json`.
Use repository-relative paths and preserve exact entry modes.
Account for ignored files that are tracked by the accepted tree.
Use object IDs and raw path bytes. Do not assume every path contains plain text.

The accepted snapshot can contain source changes that existed before the run.
The command cannot infer authorship from tree equality. Preview the complete diff
against project `HEAD`, including such changes, before application.

### Preview evidence and refusal conditions

Return a bounded document with:

- Project root, store identity, store epoch, and saved run ref.
- Current branch, old commit, accepted source tree, and proposed full tree identity.
- Complete change count, a bounded path list, and a full-detail route.
- Control files included or omitted, with their digests.
- Index state, controller ownership, retained attempt count, and pending publication state.
- Author identity, signing policy, and the fact that commit hooks are not invoked.
- Named guard verdicts, refusal reasons, and a token.

A read-only preview can derive a canonical tree-entry digest without writing a Git
tree object. Apply can construct the actual tree and verify the same entry digest.
Avoid `git add`, `write-tree`, `hash-object -w`, and write-mode database access in preview.

Refuse when any required evidence is unknown. Specific refusal cases include:

- A live controller or surviving child process owns relevant work.
- Detached or unborn `HEAD`, another checked-out branch, or an unsupported repository layout.
- Staged changes, index conflicts, an index lock, or a merge/rebase operation in progress.
- A pending publication, pending task export, or unresolved handoff journal.
- Source content differs from the accepted tree, including ignored accepted files.
- Task bytes contain an unimported semantic change.
- Required author identity or signing support is unavailable.

Use a content-sensitive token. Bind the full index identity, source tree evidence,
control-file bytes, branch, `HEAD`, run state, and journal revision to it.
Changing the contents of an already-listed dirty file must invalidate the token.
The current path-list digest at
[`recovery_preview.go:1344`](../internal/looper/recovery_preview.go#L1344)
is insufficient for this operation.

The commit message and final signing choice can be supplied at apply time.
Include them in the idempotency payload. They must not change the selected tree.

### Apply and crash recovery

Implement a dedicated `publish.go` service. Share ownership and state validation
helpers with stopped recovery. Keep branch handoff separate from `Engine.publish`.

Use this sequence:

1. Hold the repository lock. Re-read ownership, child processes, state, branch, files, and token evidence.
2. Acquire the real Git index lock. Prepare the replacement index in a private file and validate its tree.
3. Create the project commit with configured Git identity and signing policy. Persist a prepared handoff journal and protect its objects with a private ref.
4. Recheck the selected branch and source evidence. Advance the branch with an expected-old-object comparison.
5. Install the prepared index through the held index lock. Leave working file bytes unchanged.
6. Reconcile the saved baseline through the existing owner-commit rules. Persist the terminal receipt and journal outcome.
7. Release locks and report the old commit, new commit, accepted source tree, and retained work.

The journal must record the old and new commit IDs, branch, old index digest,
prepared index location, expected final index tree, source evidence, request ID,
idempotency key, and operation state.

| Failure point | Restart behavior |
| --- | --- |
| Before the prepared journal | Branch and index remain unchanged. Unreferenced objects do not count as publication. |
| After preparation, before branch update | Verify evidence again. Complete or report a prepared operation without moving the branch silently. |
| After branch update, before index installation | Recognize the exact new commit. Complete index installation only if the index still matches recorded old evidence. |
| After index installation, before state update | Recognize the new branch and index. Reconcile the baseline and finish the original receipt. |
| After receipt persistence, before output | Return the original outcome on retry. Create no second commit. |
| An external command changes branch or index during recovery | Report a conflict. Preserve both states and provide exact evidence. Do not reset or overwrite the external change. |

Recover only locks and temporary files proven to belong to this operation.
Never delete an arbitrary existing `.git/index.lock`.
An index lock protects cooperating Git commands. The Looper lock does not prevent
arbitrary external file edits. Recheck content before branch advancement and
preserve any later file edits as working changes. Do not overwrite them.

Use a dedicated project-commit helper. Do not reuse `Repository.Commit`, which
forces the Looper identity. Honor configured signing when enabled. Report signing
failures before the branch moves. This first version does not invoke commit hooks;
state that in preview and documentation because the commit uses an immutable tree.
Existing Looper checks remain the evidence for acceptance. Do not claim they ran
again during handoff.

### Recovery integration

Add a `commit-published` choice to recovery preview when branch handoff is relevant.
Its apply mode points to `looper publish`; it does not silently resume a controller.
Keep `publish-retained` as the existing candidate-publication concept.
Explain the difference in the preview text.

Check whether `recoverCommittedHead` accepts the composed full tree after tracked
control files are restored. Add a dedicated reconciliation helper only if needed.
Do not clear `State.Publication`, replace `State.Tip`, or mark retained tasks done
as a shortcut. A pending handoff journal must be resolved before controller startup
can dispatch or perform another publication.

### Tests and acceptance criteria

Extend [branch recovery tests](../internal/looper/branch_recovery_test.go#L16),
[publication snapshot tests](../internal/looper/publication_snapshot_test.go#L219),
and [stopped recovery lock tests](../internal/looper/recovery_apply_test.go#L912).
Add `publish_test.go` with temporary repositories and stub processes.

| Test | Required result |
| --- | --- |
| Completed drain with accepted edits | One project commit contains exactly the previewed changes. |
| Interrupted run with earlier accepted work | Commit accepted work; retain incomplete task identity and artifacts. |
| Tracked task file and schema | Keep them in the commit with the previewed bytes. |
| Untracked control files | Leave them untracked and state the omission. |
| Accepted file becomes ignored | Verify and commit its accepted content. |
| Accepted ignored file changes or disappears | Refuse without changing branch or index. |
| File mode, symlink, rename, deletion, unusual path | Preserve the exact tree entries. |
| Different content at the same dirty path after preview | Reject the stale token. |
| Branch moves or index changes after preview | Reject without restoring old state. |
| Live owner or surviving worker | Refuse before mutation. |
| Crash at each journal boundary | Complete once or report a preserved conflict. |
| Same key and payload | Return the same commit and receipt. |
| Same key with another message or tree | Return an idempotency conflict. |
| Publish followed by controller resume | Resume passes guards and does not repeat completed worker work. |
| Preview against a stopped store | Files, refs, objects, database, and sidecars remain unchanged. |

Feature acceptance: an operator can drain or interrupt, preview, commit accepted
work, and resume without a manual reset or loss of retained work.

## Feature 2: fresh requeue with stable task identity

### Proposed command and modes

```sh
looper requeue T025 --contract v2 --json
looper requeue T025 --fresh --contract v2 --json \
  --idempotency-key retry-T025-provider-change
looper requeue T025 --fresh --preview --contract v2 --json
```

Ordinary requeue retains the current behavior: keep partial implementation and
the attempt's profile name. Fresh requeue preserves the task but retires reusable
attempts. The next dispatch creates a new attempt from the accepted run tip.

| Property | Ordinary retry | Fresh retry |
| --- | --- | --- |
| Task ID, dependencies, acceptance contract | Preserve | Preserve |
| Previous attempts, refs, logs, worker receipts | Follow current reuse behavior | Preserve as historical evidence |
| Partial unaccepted implementation | Reuse | Retain in old artifacts; exclude from the new workspace |
| Attempt ID | Can reuse | New ID at dispatch |
| Worker profile name | Saved attempt name | Current worker role at dispatch |
| Repair counters and failure state | Existing retry rules | Start clean on the new attempt |
| Task status after successful request | `todo` | `todo` |

Current settings means the controller snapshot and effective role at dispatch.
It does not mean rereading a machine catalog during active work.
If the profile definition exists only in a newly saved catalog, the command must
report that a controller restart is required.

Do not add a separate `--agent` override in the first version.
Use `config --set worker=...` followed by fresh requeue.
Report the effective worker preview and its application boundary.

### Task and attempt guards

Live ordinary retry keeps the existing blocked-task and global idle guards.
Live fresh retry initially requires the same idle boundary. It must also reject
pending publication, a pending cancellation for the task, and a changed backlog.

Stopped fresh retry runs under the repository lock without launching agents.
Allow a blocked task, or an interrupted `doing` task whose processes have ended.
For an interrupted task, require the accepted project tree and task contract to
match saved state. Refuse unresolved publication and unreadable process evidence.

Reject completed, cancelled, deferred, and operator-only tasks.
Do not clear control tags or remove a prerequisite through retry.
Dependencies remain unchanged and can still prevent dispatch.
The receipt must distinguish “requeued” from “ready to dispatch.”

### Retirement model and dispatch changes

Add optional retirement metadata to `Attempt`, with:

- Retirement request ID and timestamp.
- Reason code `fresh_requeue`.
- The retained result and artifact references, through existing attempt fields.
- The successor attempt ID when a new attempt is created.

Keep the historical phase as evidence. Use an explicit retirement predicate
rather than changing an attempt to `done` or `cancelled`.
An optional metadata object keeps older state documents readable by the new build.

At fresh application, retire all attempts for the task that could be selected for
resume or blocked reuse. Retiring only the newest attempt is insufficient:
`Engine.start` searches backward and can find an older matching attempt.
Never retire an accepted completed result as if it were failed work.

Audit all attempt selectors and phase checks, including:

- [`Engine.start`, line 1248](../internal/looper/engine.go#L1248): skip retired attempts in both selection loops.
- [Startup recovery, line 721](../internal/looper/engine.go#L721): do not relaunch retired work.
- [`activeAttempt`, line 52](../internal/looper/observe.go#L52): retirement cannot count as live work.
- [`retained`, line 467](../internal/looper/engine.go#L467): distinguish historical artifacts from resumable work.
- [`replaySafeRetainedWork`, line 835](../internal/looper/engine.go#L835): do not replay retired worker receipts.
- [Recovery attempt listing, line 500](../internal/looper/recovery_preview.go#L500): retain historical detail without advertising resume.
- [`cancelBlockingAttempt`, line 102](../internal/looper/cancel.go#L102): retired attempts do not hold cancellation.
- [`amendHoldsContract`, line 71](../internal/looper/amend.go#L71): retired attempts do not prevent a valid later amendment.
- [`protectedScopePaths`, line 131](../internal/looper/branch_recovery.go#L131): review scope protection without weakening accepted-work protection.
- [`taskView`, line 140](../internal/looper/view.go#L140): show the current attempt and preserve history.
- [`inspectAttemptOf`, line 574](../internal/looper/inspect_compact.go#L574): expose retirement and successor references in version 2.

Also audit the phase checks in `run.go`, `retention.go`, `findings.go`, and
`recovery.go`. Use a repository-wide search during implementation. The list above
is a starting inventory, not proof that every selector is covered.

Fresh mode must never delete the old `receipt.json`, worktree, result commit, or
failure summary. The retained-reuse reset at `engine.go:1298` applies only to
ordinary retry.

### Transport and persistence

Add a distinct public and internal request type, `requeue-fresh`.
Continue using `requeue` for ordinary retry.
This avoids an older controller accepting the request while ignoring a new flag.
Require advertised support before a client submits `requeue-fresh`.

The request includes task ID, task revision, and an attempt-set revision.
The attempt-set revision covers IDs, phases, result identities, and retirement
metadata. A task revision alone does not capture concurrent attempt changes.
Add this evidence to `SettingsRequest` and `canonicalPayload`.

Stage retirement, task reset, plan invalidation, and the request receipt together.
Reuse the export rollback pattern in
[`applyRequeueRequest`, line 332](../internal/looper/dashboard_data.go#L332)
and [`stagedBacklogRequest`, line 191](../internal/looper/add_task.go#L191).
Include cloned attempt metadata in rollback. A shallow attempt slice is insufficient.

For stopped application, use a bare engine under the lock, as stopped recovery does.
Do not call controller initialization or planning.
On a crash after application but before dispatch, retired attempts must stay retired.
The task remains queued for a future controller start.

### Dashboard and documentation

Keep the existing retry action. Add a separate “Start a fresh attempt” action.
Show the retained partial work, current role choice, and the effect on reuse.
Use explicit user selection because discarding reuse changes retry semantics.
This selection belongs to the product action, not an extra approval for normal retry.

The current dashboard callback is wired at
[`dashboard.go:163`](../internal/looper/dashboard.go#L163).
The current retry interaction is in
[`workspace.go:329`](../internal/ui/workspace.go#L329).
Use the same service from the dashboard and command line.

### Tests and acceptance criteria

- Keep the existing ordinary requeue tests unchanged.
- Test a task with multiple blocked attempts. Fresh dispatch must reuse none of them.
- Change the worker from `claude` to a stub `claude-alt` profile. Verify a new attempt uses that profile.
- Verify the old invocation and receipt files remain byte-identical after fresh application and dispatch.
- Keep task ID, dependency references, contract digest, and creation time unchanged.
- Verify fresh work starts from the accepted tip and excludes old partial edits.
- Test live and stopped paths, interrupted work, missing artifacts, and ended child processes.
- Refuse done, cancelled, deferred, operator-only, active, and pending-publication cases without mutation.
- Inject export and persistence failures. Restore staged retirement metadata on rollback.
- Test duplicate requests, changed payloads, stale task revisions, and stale attempt revisions.
- Verify unsupported controllers receive no envelope and cannot downgrade fresh mode to ordinary retry.
- Restart after retirement and before dispatch. Create one successor and retain the original history.
- Test paged history and statistics with retired attempts. Do not count retirement as task completion.

Feature acceptance: replacing a provider or escaping unusable retained state no
longer requires cancelling and recreating the task under another ID.

## Feature 3: non-interactive configuration

### Proposed command surface

Keep bare `looper config` as the interactive editor.
Add explicit modes that never require terminal input:

```sh
looper config --show --contract v2 --json
looper config --set worker=claude-alt --set reviewer=claude \
  --contract v2 --json
looper config --set jobs=2 --set review_every=0 --preview \
  --contract v2 --json
looper config --unset reviewer --contract v2 --json
looper config --scope machine \
  --set agents.claude-alt.adapter=claude \
  --set agents.claude-alt.command=/usr/local/bin/claude-alt \
  --contract v2 --json
looper config --scope machine --set worker=claude-alt --contract v2 --json
```

Project scope is the default. Machine scope defines executable profiles and
defaults for new projects. A machine command must work outside a Git repository.
Do not open a project repository or initialize a backlog for machine-only edits.

### Field rules

| Public field | Project representation | Machine representation |
| --- | --- | --- |
| `worker` | `Profile.Agent` | `AgentCatalog.Worker` |
| `reviewer` | `Profile.ReviewAgent` | `AgentCatalog.Reviewer` |
| `jobs` | `Profile.Jobs` | Unsupported |
| `model`, `claude_model`, `effort` | Existing profile fields | Use a named profile field instead |
| `verify_command`, `setup_command`, `review_every` | Existing profile fields | Unsupported |
| `agents.<name>.adapter` | Unsupported | Registered adapter name |
| `agents.<name>.command` | Unsupported | Executable name or absolute path |
| `agents.<name>.model`, `agents.<name>.effort` | Unsupported | Supported profile overrides |

Parse each assignment at the first `=`. Preserve remaining characters exactly.
Use field-specific types. Reject unknown fields and invalid numeric values.
Reject duplicate assignments and set/unset conflicts rather than relying on order.
Machine profile names can contain dots. Parse the final known field suffix;
do not split the full path into unrestricted dot segments.

Require adapter and command together when defining a new machine profile.
Allow partial updates for an existing valid definition.
Validate the complete resulting catalog before saving it.
Reject removal of a profile that still supplies a machine default.
Project references in other repositories cannot be enumerated reliably. State
that limitation if profile deletion is added; deletion is not needed for this first version.

Use `--unset` for inherited or following behavior. In particular, unset project
reviewer follows the worker under existing resolution rules. It must not replace
that rule with a saved machine reviewer default.
Keep `jobs=0` as automatic concurrency and `review_every=0` as an explicit value.
Preserve unrelated known values and unknown profile keys.

Do not accept credentials, shell fragments, or inline executable arguments.
Use the existing executable validator at `agent_profiles.go:67`.
Paths with spaces remain valid when passed as one absolute-path argument.

### Shared service and operation modes

Extract a typed configuration patch service from the editor and settings handler.
It should accept a scope, a loaded revision, and explicit changed fields.
It should return validated new state and per-field outcomes without saving it.
Both the form and command line use this service.

| Mode | Read path | Write path | Result |
| --- | --- | --- | --- |
| Show | Read-only catalog/profile and resolution | None | Saved values, effective values, sources, and controller snapshot differences |
| Preview | Read-only patch validation | None | Exact changed fields, effective values, application boundaries, revision token |
| Project edit, stopped | Repository lock, profile revision | Atomic profile save and durable result | Applied saved settings; no controller starts |
| Project edit, live | Verified controller and profile revision | Existing settings request | Immediate receipt; `--wait` can return the decision |
| Machine edit | Catalog revision | Catalog lock and `saveAgentCatalog` | Saved catalog revision; current controllers keep snapshots |

Separate project and machine writes. Reject mixed-scope changes in one command.
This avoids promising atomicity across a machine file and a controller request.
Users can define a machine profile, then select it in the project.

Retain existing `--reset` behavior and keep it exclusive with patch modes.
Keep `--test-agent` exclusive with mutation, show, and preview modes.
Only `--test-agent` launches a live probe and consumes agent usage.

`--show` and `--preview` must work with piped input and output.
They must not create a runtime directory, profile, catalog, or database sidecar.
Show malformed saved input as an error with a repair route. Do not silently replace it.

### Concurrency, validation, and receipts

Reuse `patchProfile` and `applySettingsRequest` for live project changes.
Extract role-name validation so the stopped path applies the same rules.
The interactive and non-interactive paths must agree about valid profiles.

For live requests, retain `ProfileDigest`, session binding, durable receipts,
idempotency keys, bounded waits, and the current source precedence rules.
Map public field aliases before computing the canonical payload digest.
Equivalent assignments through aliases must have the same digest.

For machine edits, reuse the catalog lock and revision comparison.
Add an optional `--if-revision` check for scripts that read, decide, then write.
Use the same revision in preview output.
Keep machine edits as file transactions with a revision result; do not imply that
they are controller requests or appear in a project's request history.
Reject project request-only flags on machine scope unless a separate machine
receipt store is deliberately designed later.

Stopped project edits need a defined durable result even when no run exists.
Use a project settings journal outside `state.db`, stored beside `profile.json`.
Record the old profile digest, intended new digest, canonical patch, caller key,
and result. Persist preparation before replacing the profile. On restart, compare
the actual digest with the old and new digests. Complete the original result when
the new digest matches. Refuse an unexpected third digest without replacing it.
Use an atomic journal write and preserve unresolved entries during retention.
Keep at most the newest 200 terminal entries, matching the project receipt policy.
This supports idempotent replay without creating a fake controller run.

Return per-field outcomes such as:

- Saved and effective for the next controller start.
- Saved and applied at the next dispatch boundary.
- Saved but overridden by a flag or environment setting.
- Saved but requires a controller restart to load a new machine definition.
- Unchanged because the requested saved value already matches.

Validate all input before the first write. A multi-field error writes nothing.
Executable lookup can be a separate static check. Never run `--version` or a live
agent merely to validate a command definition. Preserve current form behavior
where practical, and make missing executable evidence explicit.

### Tests and acceptance criteria

Extend [settings tests](../internal/looper/settings_test.go#L167),
[agent profile tests](../internal/looper/agent_profiles_test.go#L28), and
[catalog concurrency tests](../internal/looper/agent_profiles_test.go#L229).

- Use closed stdin and a buffer for stdout. Explicit configuration must succeed without a terminal.
- Test project selection, machine definition, machine defaults, show, preview, and unset.
- Verify machine edits outside a repository and with `LOOPER_AGENTS_FILE` overrides.
- Test names with dots, paths with spaces, values with `=`, invalid adapters, and unsupported overrides.
- Verify complete validation before writes and preservation of unknown project keys.
- Test unset reviewer following behavior, automatic jobs, and explicit zero review frequency.
- Test stale catalog revisions, stale profiles, concurrent writers, and stale controller sessions.
- Test live application outcomes under flag and environment overrides.
- Verify new machine profiles apply only after the current controller restarts.
- Verify no agent launch, usage, runtime creation, or sidecar creation during show and preview.
- Keep first-run and interactive form tests passing through the shared validator.
- Test new modes against the parser's incompatible-flag checks and structured error contracts.

Feature acceptance: an operator can define `claude-alt`, select it for one role,
inspect the effective result, and change it during a run through supported commands.

## Feature 4: structured blocker reasons

### Proposed model

Keep free-text `blockers` for human evidence.
Add one optional current `block_reason` object to the task model:

```json
{
  "class": "failure",
  "code": "agent_quota_exhausted",
  "retry": "after-change",
  "attempt_id": "task-example",
  "source": "controller"
}
```

The code above is proposed vocabulary, not a claim that this exact failure code
already exists. Build the stable code list from actual structured failure paths.

| Class | Meaning | Default version 2 severity | Retry implication |
| --- | --- | --- | --- |
| `operator` | A person or external owner must satisfy a prerequisite | `info` | Requires recorded external action |
| `deferred` | Work is intentionally postponed until a condition is met | `info` | Requires the resume condition and explicit reopening |
| `failure` | An execution or orchestration failure stopped work | `warning` | Follow the typed retry guidance |
| `unknown` | Available structured evidence does not establish a reason | `warning` | Inspect evidence before choosing an action |

Use `retry` values `yes`, `after-change`, `no`, and `unknown`.
The reason class alone must never authorize an automatic retry.
Quota, billing, invalid configuration, and permanent failures can all have class
`failure` while requiring different operator actions.

The object represents the current primary dispatch barrier. Historical blocker
strings and prior attempt reasons remain evidence. Operator and deferral tags are
authoritative gates. If both tags exist, report deferred as the primary class,
expose the operator gate separately in inspection, and keep both dispatch guards.
Do not imply that one reason object exhaustively lists every prerequisite.

Bound `code`, `source`, and reference lengths. Keep raw error output, executable
arguments, secrets, and private agent reasoning out of this object.

### Persistence and classification rules

Add the field to `Task`, the embedded backlog schema, and
[`docs/to-do.schema.json`](to-do.schema.json#L1).
The embedded schema starts at [`types.go:47`](../internal/looper/types.go#L47).
Keep the shipped and embedded schema equality test passing.
Add the field to strict decoders and field allowlists where necessary.

Because the compatibility policy currently describes version 1 task documents,
document this optional schema extension and downgrade behavior explicitly.
Older strict readers can reject the new field. Keep their refusal actionable;
do not claim that a file with new metadata works on every older binary.
Determine whether a schema-version bump or an explicit migration gate is required
before exporting the field by default. This is a release gate, not an incidental code choice.

Clear `block_reason` in `Task.Contract` before hashing.
Keep `hashJSON(task)` sensitive to the reason for mutation revision checks.
This preserves acceptance identity while detecting concurrent task updates.
Include the new field in semantic backlog comparison because reason edits change
task content even though they do not change the acceptance contract.

Centralize classification in a helper that accepts typed inputs.
Do not search blocker text for words such as “quota,” “operator,” or “later.”

| Source | Classification action |
| --- | --- |
| `holdForOperator` | Set `operator` with a stable prerequisite code. |
| `deferTask` | Set `deferred` and preserve the concrete condition. |
| Controller launch, quota, billing, timeout, and verification failures | Set `failure` using existing typed error evidence. |
| Repair limit or repeated diagnosis failure | Set `failure` with an exhaustion code and an attempt reference. |
| Worker reports an unmet product requirement | Use a validated explicit reason if available; otherwise use `unknown`. |
| Invalid task quarantine | Set `failure` with an invalid-contract code and `after-change`. |
| Dependency prevents readiness | Report a dependency gate separately; do not invent an execution failure. |
| Imported task with no reason and no control tag | Use `unknown` when blocked. |
| Imported task with an operator or deferred control tag | Derive the matching class from the tag. |
| Successful completion, cancellation, or accepted retry | Clear the current reason. Preserve historical attempt evidence. |

Modify the typed hold API rather than attaching a classifier after a string is
formatted. Start from [`Engine.hold`, line 1367](../internal/looper/engine.go#L1367),
[`Engine.repair`, line 204](../internal/looper/integration.go#L204), and
[`holdQuarantined`, line 110](../internal/looper/recovery.go#L110).
Audit every assignment to `Status = "blocked"` and every blocker append.

Current `applyResult` merges blocker history. The current primary reason should
replace prior current metadata when a new execution blocks the task.
Successful task completion clears it. Do not infer the current class from stale
historical blocker strings.

Allow a checked `amend` operation to correct current reason metadata on an idle
blocked task. Validate the class and code. Do not allow this patch to remove
operator or deferred control tags, change ownership, or bypass a retained contract.
Update the amendable-field registry and documentation together.

### Observation, output, and monitoring

Use a version 2 finding projection with optional `reason_class`, `reason_code`,
`retry`, and `attempt_id` fields. Keep existing finding codes where possible:
`task_deferred` remains available, and `task_blocked` gains structured context.
This avoids requiring consumers to replace every existing code filter.

The strict version 2 finding schema is at
[`doctor.schema.json:57`](contracts/schemas/v2/doctor.schema.json#L57).
It currently rejects unknown fields. Update the schema and fixtures with the code.
Update the analogous status finding projection and compact inspection contract.
The inspect task shape starts at
[`inspect_compact.go:129`](../internal/looper/inspect_compact.go#L129).

Keep legacy finding shape and severity unchanged for existing invocations.
Use a separate projection rather than adding fields directly to every serialized
`HealthFinding`. Check `--full`, inspection, events, and nested task output too.
Optional runtime fields can otherwise escape through shared serialization.

Plain doctor can show the reason class beside each blocked task.
Doctor still returns findings successfully; warning findings do not become a
transport error. Update the dashboard task detail with class, code, and action.

Document a monitor policy based on fields:

| Evidence | Monitor behavior |
| --- | --- |
| Operator or deferred class | Show expected waiting. Do not page merely because status is blocked. |
| Failure class with `retry=yes` | Report a retry candidate. Require the caller's retry policy. |
| Failure class with `retry=after-change` | Show the required change, such as billing or configuration repair. |
| Failure class with `retry=no` | Escalate the permanent failure without retrying. |
| Unknown class or unknown retry guidance | Preserve a warning and link to inspection. |

### Migration and tests

Load old state without writing it during observation.
Derive `deferred` and `operator` from existing control tags.
Use `unknown` for remaining legacy blocked records unless typed retained evidence
establishes a reason. Do not rewrite the backlog merely because doctor ran.
Persist normalized metadata only during an authorized mutation or controller export.

Extend [deferred diagnosis tests](../internal/looper/deferred_test.go#L45),
[observation tests](../internal/looper/observe_test.go#L14),
the schema fixtures, and the frozen legacy tests.

- Verify each class, default severity, action, and retry value.
- Verify quota and billing failures remain non-retrying until the required change.
- Verify a transient failure and an operator prerequisite produce different structured evidence.
- Verify a blocked worker report without typed evidence remains unknown.
- Load old snapshots with operator tags, deferred tags, both tags, and neither tag.
- Verify observation creates no metadata writes or database sidecars.
- Verify reason-only updates preserve `Task.Contract` and change the mutation revision.
- Test import, amend, retry, cancellation, successful completion, and repeated failure transitions.
- Keep old blocker text as history while replacing the current primary reason.
- Test malformed metadata, invalid enum values, overlong values, and conflicting tags.
- Validate version 2 doctor, status, and inspect documents against updated schemas.
- Verify legacy output omits new fields, including nested tasks and full-detail paths.
- Verify compact response budgets and detail routes with long evidence.

Feature acceptance: a monitor can distinguish expected waiting from execution
failure without parsing blocker text or treating every failure as retryable.

## Work packages and dependencies

These are planning labels. They are not task IDs and do not modify the backlog.

| Package | Deliverable | Depends on | Completion evidence |
| --- | --- | --- | --- |
| OW-01 | Command syntax, document schemas, compatibility policy, and receipt decisions | None | Reviewed examples and valid/invalid fixtures |
| OW-02 | Read-only branch handoff evidence and exact full-tree composition | OW-01 | Preview tests, including tracked control files and content changes |
| OW-03 | Journaled project commit, branch update, and index installation | OW-02 | Crash-boundary and concurrency tests |
| OW-04 | Recovery choice, resume integration, and publication documentation | OW-03 | Drain/interrupt-to-publish-to-resume fixture |
| OW-05 | Shared typed configuration patch service | OW-01 | Interactive and headless validation parity |
| OW-06 | Headless project and machine configuration commands | OW-05 | No-terminal, concurrency, and live receipt tests |
| OW-07 | Attempt retirement and fresh request type | OW-01 | State, dispatch, rollback, and compatibility tests |
| OW-08 | Requeue CLI, stopped path, and dashboard action | OW-07 | Stable-ID provider-switch fixture; use OW-06 when available |
| OW-09 | Block reason model, source classification, and migration policy | OW-01 | Typed failure and legacy-state tests |
| OW-10 | Version 2 reason output and monitor documentation | OW-09 | Schema, severity, byte compatibility, and budget checks |
| OW-11 | Combined operator workflow and release notes | OW-04, OW-06, OW-08, OW-10 | Full acceptance fixture and required repository checks |

Keep each package small enough to review with its tests.
Avoid combining project commit creation and attempt retirement in one patch.
Schema and capability support must land with the behavior they advertise.

## Combined acceptance fixture

Use temporary repositories and stub agents. No live model usage is needed.

1. Define two stub machine profiles and select different worker and reviewer roles through headless configuration.
2. Run one accepted task and one task that reports a typed quota failure. Verify the failure stops retrying.
3. Stop the controller. Preview and publish accepted work. Verify the branch, index, control files, and retained failed attempt.
4. Change the worker role. Fresh-requeue the failed task under its original ID. Verify the old artifacts remain unchanged.
5. Restart. Verify a new attempt uses the new worker and the original dependent tasks remain connected.
6. Add operator, deferred, and unknown blocked fixtures. Verify doctor exposes the expected classes and severity.
7. Complete the retried task, publish again, and resume once more. Verify no duplicate commit, repeated completed work, or lost history.

Also run a crash variant of steps 3 and 4 at each durable boundary.
Inspect public receipts and events to verify the result. Private agent transcripts
are not required evidence.

## Validation commands

The commands below are planned checks. They have not run for this document.
Adjust test-name patterns to match the final names.

```sh
go test -timeout 30m ./internal/looper \
  -run 'Test(Publish|Resume|RecoveryApply|Requeue|FreshRequeue)'
go test -timeout 30m ./internal/looper \
  -run 'Test(Config|Settings|Profile|AgentProfiles|Deferred|BlockReason)'
go test ./internal/contractschema
go test -timeout 30m ./internal/looper \
  -run 'Test(LegacyContract|Capabilities|ClientCommands)'
go test -race -timeout 30m ./internal/looper \
  -run 'Test(Publish|FreshRequeue|Requeue|Settings|AgentProfiles)'
make test
```

The suite timeout is intentional. [The Makefile, line 4](../Makefile#L4) explains
that durable-write tests need a larger budget on a loaded host.
Run `make race` if concurrency changes affect broader controller paths.
Record the actual commands and outcomes in the implementation handoff.
Do not regenerate legacy fixtures merely to make a compatibility failure pass.

## Decisions to settle during design review

The recommendations above provide defaults. These decisions must be explicit
before the relevant implementation package is complete:

| Decision | Recommended choice | Reason |
| --- | --- | --- |
| Commit shape | One project commit with current `HEAD` as parent | Avoid importing private intermediate history. |
| Project commit identity | User Git identity and configured signing | Keep project authorship separate from internal Looper commits. |
| Commit hooks | No automatic hooks in the first version; disclose this in preview | Prevent unpreviewed changes to the accepted tree. |
| Live publication | Require a stopped controller | Keep ownership and branch mutation boundaries clear. |
| Fresh retry transport | New `requeue-fresh` request type | Prevent silent downgrade by older controllers. |
| Retry settings boundary | Current controller role at dispatch | Match existing configuration snapshots and dispatch behavior. |
| Stopped settings receipts | A project settings journal beside the profile | Support idempotency without inventing a run. |
| Machine and project edits | Separate commands by scope | Avoid a false cross-file transaction guarantee. |
| Task schema extension | Explicit migration and downgrade policy before default export | Older strict readers can reject new task fields. |
| Legacy blocked tasks | Derive from control tags; otherwise unknown | Avoid guessing from historical prose. |
| Automatic healing | Exclude from this work | Classification and retry operations need to exist before an automatic policy uses them. |

## Release and documentation checklist

- Update command help, `docs/contracts/commands.json`, capability fixtures, and request-type schemas.
- Update `docs/recovery.md`, `docs/runtime-requests.md`, `docs/AGENT-PROFILES.md`, and task documentation where present.
- Document publish commit contents, omitted control files, hooks, signing, and crash completion.
- Document ordinary versus fresh retry with a provider-switch example.
- Document headless field names, inheritance, scope, revision checks, and application boundaries.
- Document blocker class semantics, unknown legacy state, and retry guidance.
- Add release notes with migration and downgrade limits. Preparation does not publish a release or replace an installed binary.

Before editing tracked files during implementation, run `looper status` again.
Use a separate worktree if a controller owns the repository.
Preserve existing uncommitted accepted work and all retained run artifacts.
