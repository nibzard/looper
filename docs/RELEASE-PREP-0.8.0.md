# Release preparation for 0.8.0

Historical record: another session committed the 0.8.0 content and version in
`d87ca35`. The follow-up diagnostics are prepared as 0.8.1. See
[the monitoring actions](research/MONITORING-ACTIONS-2026-10-12.md). The scope
and status below describe the earlier preparation window.

Status: candidate verified; shared-change confirmation is pending. The working
`VERSION` and installed binary remain at 0.7.1. No commit, tag, push, or GitHub
release has occurred. Packaging checks install only in temporary fixtures.

The candidate includes completion checks, input recovery, prompt transport,
context files, lifecycle records, timing, statistics, and related documentation.
Some source changes originate from other sessions. The repository runbook
requires scope confirmation before those changes enter a commit.

Base commit: `d2581329a23aab66c1c6f995636840f34d5b0ee2`.

Candidate directory: `/tmp/looper-release-0.8.0-bf399qtf/candidate`.

Candidate tree with the version bump: `f3ebbba155edc9c25aebcc68291f4b96d789372a`.

## Proposed release files

- `Makefile`
- `README.md`
- `docs/ARCHITECTURE.md`
- `docs/LOOP-CORRECTNESS-PLAN.md`
- `docs/PROMPT-TRANSPORT-PLAN.md`
- `docs/release-notes/0.8.0.md`
- `docs/research/CONTEXT-TRANSPORT-RESULTS-2026-10-12.md`
- `go.mod`
- `internal/looper/agent.go`
- `internal/looper/assessment_test.go`
- `internal/looper/cli.go`
- `internal/looper/command.go`
- `internal/looper/context.go`
- `internal/looper/context_live_test.go`
- `internal/looper/context_test.go`
- `internal/looper/controller.go`
- `internal/looper/dashboard.go`
- `internal/looper/dashboard_data.go`
- `internal/looper/engine.go`
- `internal/looper/exec_limits.go`
- `internal/looper/exec_limits_darwin.go`
- `internal/looper/exec_limits_linux.go`
- `internal/looper/git.go`
- `internal/looper/imports.go`
- `internal/looper/imports_test.go`
- `internal/looper/inputwait_test.go`
- `internal/looper/integration.go`
- `internal/looper/lifecycle.go`
- `internal/looper/lifecycle_test.go`
- `internal/looper/main_test.go`
- `internal/looper/observe.go`
- `internal/looper/planner_activity_test.go`
- `internal/looper/prices/LICENSE`
- `internal/looper/prices/README.md`
- `internal/looper/prices/data.json`
- `internal/looper/prices/source.json`
- `internal/looper/pricing.go`
- `internal/looper/process_test.go`
- `internal/looper/process_unix.go`
- `internal/looper/publication_snapshot_test.go`
- `internal/looper/requirements.go`
- `internal/looper/review_test.go`
- `internal/looper/run.go`
- `internal/looper/statistics.go`
- `internal/looper/statistics_test.go`
- `internal/looper/store.go`
- `internal/looper/task_timing.go`
- `internal/looper/task_timing_test.go`
- `internal/looper/timing.go`
- `internal/looper/timing_test.go`
- `internal/looper/transport_test.go`
- `internal/looper/types.go`
- `internal/looper/usage.go`
- `internal/looper/view.go`
- `internal/ui/dashboard.go`
- `internal/ui/dashboard_test.go`
- `internal/ui/statistics.go`
- `internal/ui/statistics_test.go`
- `internal/ui/workspace.go`
- `internal/ui/workspace_test.go`
- `scripts/runtime-test.py`
- `scripts/update-prices.py`
- `skills/todo-json-manager/SKILL.md`

The version bump changes only `VERSION`, after content commits.

## Excluded working changes

- `.agents/AGENTS.md`
- `.axinbox/drops/01M4P8AXGSMEXWQ7Q75XK5FTX9.json`
- `AGENTS.md`
- `CLAUDE.md`
- `NOTES.md`
- `docs/SIMPLIFY-REVIEW-2026-10-12.md`
- `docs/research/IMPROVEMENT-DECISIONS-2026-10-11.md`

`NOTES.md` and this preparation report remain local working records.

## Validation results

- `make test`: passes, including all 53 runtime tests, Go tests, static analysis,
  package smoke, and skill regression.
- `make race`: passes, including all 53 runtime tests and race-enabled Go tests.
- The final normal build reports `looper 0.8.0`.
- Darwin arm64 compilation passes. Darwin runtime validation remains deferred.
- The embedded price data matches the checksum in its source receipt.
- Candidate index whitespace checks pass. The proposed source files still match
  the verified snapshot after testing.

Remote `main` points to `521102aa2d36aaf1cd8e5102c19fcf7310724abf`.
Local `main` includes two subsequent documentation commits. No remote `v0.8.0`
tag exists at inspection time.

## Remaining steps

1. Confirm inclusion of changes from other sessions.
2. Commit release content with context notes.
3. Commit the version bump alone.
4. Build, install, and verify the installed version.

Publication requires a separate request. The Homebrew formula stays at the
published version until the new tag is available.
