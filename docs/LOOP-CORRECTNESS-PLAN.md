# Loop correctness implementation plan

Status: implemented 2026-10-12; recovery validation follow-up passes.
Created 2026-10-11.

Make completion use one review contract, keep an idle controller alive during
invalid backlog edits, and bound optional assessment commands. Implement these
changes in that order. Each change must pass its acceptance checks before the
next begins.

The decisions follow [MANIFESTO.md](../MANIFESTO.md): remove competing contracts,
wait for the next human edit, and keep optional work from stopping the loop.
The existing publication journal and executable acceptance checks remain the
authority for applying code.

## Scope and evidence

| Change | Priority | Confirmed behavior |
| --- | --- | --- |
| Use one completion contract | Medium | A new legacy marker overrides `complete: false` when ordinary tasks are done. |
| Wait for corrected backlog input | Medium | Invalid JSON or a task status stops an idle continuous controller. Saved work survives. |
| Bound optional assessment commands | Low | A custom assessment can hold publication beyond the worker timeout. |

The Measuretwice adapter already declares a 30-second evaluation deadline.
The third change adds a controller limit around any assessment command.
It does not assume that the supplied adapter hangs.

Automatic task splitting, verification rewriting, broad alias removal, and
changes to live settings are outside this plan. The product comparison remains
separate in [EXPERIMENT-PLAN.md](../EXPERIMENT-PLAN.md).

## 1 Use one completion contract

Primary files: `internal/looper/integration.go`, `scripts/runtime-test.py`, and
`skills/looper-backlog/SKILL.md`.

Remove the review path that accepts task additions through edits to the private
backlog. Review findings come from the structured `findings` response.
Completion comes from an explicit `complete: true` response, with every ordinary
task done and no accepted findings left to implement.

Reject a review that changes the private backlog. Compare the parsed backlog
with the accepted backlog, including its goal and source files. Perform this
check before applying findings or completion. Keep the existing check for
changes to project code.

Change the runtime fixture to return the current review response. Remove its
default marker insertion. Update the shipped backlog skill to describe this
response and the controller's ownership of task changes.

Preserve loading of existing backlogs that contain historical markers. Such
markers remain excluded from scheduling, but cannot authorize a new completion.
Keep saved state and migration formats unchanged. This change applies to new
review passes; it does not rewrite historical review records.

Acceptance checks:

- [x] `complete: false` leaves the goal unfinished when every task is done.
- [x] `complete: true` completes a final review when every task is done.
- [x] A blocked task or an accepted open finding prevents goal completion.
- [x] Adding a marker or another task through a private backlog edit rejects
  the review. The controller retains its accepted backlog and completion state.
- [x] Changing the private goal or source files also rejects the review.
- [x] An existing historical marker cannot override `complete: false`.
- [x] Structured findings still create work, and the runtime suite uses the
  current review response without legacy completion writes.

## 2 Wait for corrected backlog input

Primary files: `internal/looper/run.go`, `internal/looper/imports.go`,
`internal/looper/view.go`, and the related controller and dashboard tests.
Inspect `internal/looper/dashboard_data.go` for requeue interactions.

Handle invalid input as a recoverable condition only after successful
initialization, at an idle boundary in a continuous run. Identify parse and
backlog validation errors with a typed error. File access, Git, storage, and
publication errors retain their existing failure behavior.

Read the backlog once per refresh. Validate and hash the same bytes before
accepting a new baseline. On invalid input, preserve the last accepted state and
the user's file. Pause planning, review, dispatch, and backlog writes until a
valid edit arrives. Do not run tasks from an older contract during this wait.

Use the existing polling interval and waiting presentation. Report the task
file path, the error, and the action: correct the file to continue. Emit the
diagnostic on entry and when the problem changes, rather than on every poll.
Keep the waiting view current and suppress the completion banner while input
needs correction. Keep this condition transient; no persisted task phase or
database schema change is needed.

Check idle input before operations that can apply queued backlog changes.
Keep plan imports queued while input is invalid. Requeue requests must either
remain pending or receive the existing conflict response with a correction hint.
They must never overwrite the invalid file. Continue to honor stop and interrupt
requests. Pending requests must not cause a busy polling loop.

After correction, use the normal refresh path and resume within one poll
interval. Clear the waiting diagnostic even when the corrected bytes match the
previous accepted file. Invalid startup input and invalid input under `--once`
still exit with code 1. Active-work contract conflicts retain their existing
publication protection.

Acceptance checks:

- [x] Malformed JSON and an invalid task status keep an idle continuous run
  alive without launching agents or changing the saved backlog.
- [x] A corrected backlog containing a new runnable `todo` task resumes work
  in the same process. The new task completes and publishes. Completed
  implementation from before the wait does not repeat.
- [x] Restoring the previous valid file clears the diagnostic and waiting view.
- [x] Stop drains with code 0 during the wait. Interrupt returns code 130.
- [x] Invalid startup input and invalid idle input under `--once` return code 1.
- [x] Queued imports and requeue requests preserve the invalid file and remain
  actionable after correction. Repeated polls do not flood events or spin.
- [x] Git and storage failures remain fatal. Edits during active work still
  prevent publication against a changed contract or project baseline.

## 3 Bound optional assessment commands

Primary files: `internal/looper/integration.go` and focused process tests.

Use a fixed 60-second controller deadline for the assessment process. This
proposed limit leaves time around the adapter's 30-second evaluation deadline
for startup and report writing. Add no public setting in this change.

Create the deadline from the controller context. Use the existing process
runner so timeout terminates the process group and releases temporary resources.
Keep assessment execution synchronous and retain its evidence files.

On an assessment deadline, record an advisory error with the command and log
location. Continue only after checking that the candidate tree remains exactly
as verified. An assessment that changes code still fails verification.
Controller cancellation must propagate as cancellation, rather than become an
advisory timeout that permits publication.

Use an internal helper with a supplied duration for tests. Production passes
the fixed deadline; tests use a short duration without changing global state.

Acceptance checks:

- [x] A hanging assessment reaches its deadline and its process group stops.
- [x] An unchanged candidate publishes after an assessment timeout, and the
  advisory error remains available for inspection.
- [x] Nonzero assessment exit remains advisory; a passing result still publishes.
- [x] An assessment that changes project files cannot publish, including when
  the assessment subsequently times out.
- [x] Controller cancellation retains work. Restart completes verification and
  publication without repeating implementation.
- [x] An empty assessment command adds no process or delay.

## Validation and delivery

Review follow-up: `TestRunWaitsForCorrectedInputAndResumes` adds a task already
marked `done`. It verifies refresh and review, but does not verify resumed
implementation. Add a real controller test with a new `todo` task. Count
worker runs by task, and confirm that the controller session stays the same.
`TestCorrectionRunsNewTaskWithoutRepeatingCompletedWork` now passes this
check with separate worker counts and a published file.

The follow-up is the first stage of
[PROMPT-TRANSPORT-PLAN.md](PROMPT-TRANSPORT-PLAN.md).

1. Add focused regression coverage with each implementation change. Use local
   agent stubs, real Git repositories, and observable process or file events.
2. Run the relevant Go tests and runtime cases after each change. Test recovery
   paths and negative controls alongside the successful path.
3. Update `README.md` and `docs/ARCHITECTURE.md` for the resulting behavior.
   Include the assessment deadline and the boundary of idle input recovery.
4. Run `make test` and `make race` after the complete implementation.
5. Review the final diff against the acceptance checks. Confirm that the review
   contract has one completion path and publication still uses the checked tree.

Deliver three focused changes in the order above, with their tests and relevant
documentation. Paid model calls and the comparison pilot are separate work.
