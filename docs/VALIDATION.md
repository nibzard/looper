# Rewrite validation

Validated on Linux on 2026-10-11.

## Agent-experience benchmark (AX-027, 2026-11-09)

`scripts/runtime-test.py` grew a `BenchmarkTests` suite that reproduces the
verified 2026-11-05 CLI audit incidents with the stub agent and drives every
recovery procedure through documented public commands. The suite covers large
histories, submission receipts across a controller crash and restart,
idempotent replay and conflicts, foreground and detached bootstrap, a
pre-contract controller, the accepted-import dispatch pause, and stop/start
overlap. Set `LOOPER_BENCHMARK_RECORD=<path>` to append one JSON line per
public command with exit code, seconds, and output bytes.

Recorded results live in `docs/research/AGENT-EXPERIENCE-RESULTS.md`; the
gates are asserted on every run: compact status stays within 8 KiB, finite
event pages respect their limit, continuation walks miss no committed event,
idempotent keys apply once, accepted receipts stay queryable after restart,
and every fixture finishes without private-state reads.

The bootstrap fixture found and repaired one defect: the startup seed a
detached start saves before readiness made compact status report the
bootstrap window as `no_ready_task` with a spurious `view_unavailable`
finding (`internal/looper/status_compact.go`).

## Fresh-agent evaluation preparation (AX-028, 2026-11-09)

The live evaluation of the agent experience is prepared, not run:
`docs/research/AGENT-EXPERIENCE-EVALUATION.md` defines the protocol
(two arms, four scenarios from the verified audit patterns, three
repetitions each), and `docs/research/agent-evaluation/` holds the
fixtures: the measurement shim, arm preambles, matched briefs, S2
submission inputs, and the result templates. No real agent session,
installation, publication, or controller restart happened. The live
test needs its own requested scope; the protocol names exactly what to
authorize.

What was verified deterministically on this host, recorded in
`docs/research/agent-evaluation/setup-verification.md`: the shim logs
and passes through every call; all four scenario setup recipes build
their intended state with the stub agent (mature run with a failure
event; live gated controller applying both S2 submissions exactly once;
the retained-work refusal with its read-only preview; two-run discovery
with build drift); and the AX-027 `BenchmarkTests` suite passes (8
tests, 85 s, focused run). One byte contrast was measured on the S1
fixture: legacy `status --json` 66,723 bytes against 1,690 for the
compact document. Everything else — command counts, latency, cost,
rubric outcomes, any arm comparison — stays unmeasured until the
authorized live run.

`docs/release.md` gained the agent-experience release checks: the
contract default transition steps, schema and command documentation,
platform-gap disclosure (darwin identity, Linux-only PTY coverage,
compile-only cross-build), the rule that installed controllers keep
their build, and how to record the evaluation's status.

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
