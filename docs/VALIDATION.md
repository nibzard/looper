# Rewrite validation

Validated on Linux on 2026-10-11.

## Automated checks

- `make test` passed Go unit tests, `go vet`, 39 integration cases, package tests,
  and skill-helper tests before the final recovery additions.
- The final `make race` passed Go unit tests and all 42 integration cases with the
  race detector enabled. The added cases cover completed worker receipts, legacy
  blocked work, and repair after a setup failure.
- A source installation into a temporary prefix with spaces passed. The installed
  binary and compatibility launcher ran, and uninstall removed both.
- The final source cross-compiles for macOS ARM64 with `CGO_ENABLED=0`. This is a
  compile check; macOS runtime behavior was not exercised on this Linux machine.
- Shell syntax checks and `git diff --check` passed.
- Four adapter tests passed through the actual local Measuretwice 0.1.0 library.
  They cover pass, fail, review, and oversized evidence. The installed TypeSafe
  SDK 0.6.0 exports the expected `systemOne` method.

The integration cases use the actual Looper executable and real Git repositories.
Agent output is deterministic. Cases cover publication before and after file
application, pending task exports, Python migration, blocked partial work,
conflict repair, combined checks, speculative checks, hard kills, locks, outside
edits, continuous task discovery, milestone review, report recovery, advisory
signals, and preservation of the user's index and branch.

## Live Codex run

The `looper-live-test` skill ran the Go executable in a temporary project. It
selected `T2` before `T10` at equal priority. Codex created `README.md`. Looper
checked the combined result and marked only `T2` done. The dispatch limit produced
the expected exit code 2.

A second invocation resumed the same project, created `hello.txt`, and passed
`test -s README.md && test -s hello.txt`. A fresh Codex review reported the goal
satisfied. The controller returned 0.

The saved state reports two integrations, two passing executable checks, one
complete final review, and both tasks done. Temporary artifacts remain at:

```text
/tmp/tmp.376mGvVQbC/project
/tmp/tmp.376mGvVQbC/run.log
/tmp/tmp.376mGvVQbC/logs/project-e6f8a44d/runtime/state.db
```

The live run used the installed Codex configuration. It does not establish a
benchmark result for any model family. Measuretwice provider calls and judgment
accuracy were not tested live. Its scripted cases validate integration behavior,
not qualification for enforcement.
