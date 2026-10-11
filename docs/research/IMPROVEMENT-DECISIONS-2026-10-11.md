# Improvement decisions from the 2026-10-11 benchmark

Decision date: 2026-10-11. This note records which findings from the
zcode benchmark and the live adapter verification turn into changes, and
which do not, with the reasoning judged against `MANIFESTO.md`. Evidence
comes from `BENCHMARK-ZCODE-VS-CLAUDE-2026-10-11.md` and the git notes on
the adapter commits.

## Inputs

- Benchmark: one task, two harnesses, the same underlying model
  (GLM-5.3). Claude Code finished in 479 s with 83,812 input tokens and a
  published result. zcode blocked after 2049 s and 3,263,146 input
  tokens with nothing published.
- Live zcode verification (2026-10-11): end-to-end run that surfaced the
  `stream-json` contract, the plan-mode prose behavior, and the `TMPDIR`
  socket-path failure.
- Manifesto rules used as the decision standard:
  - "Pass butter" — one purpose; do the deterministic thing the big
    runtimes do not (`MANIFESTO.md:7`).
  - "Simple before easy" — the simplest thing that could possibly work;
    one obvious way (`MANIFESTO.md:23`).
  - "Do not make me think" — fewer states beat better explanations
    (`MANIFESTO.md:36`).
  - "Believe in the loop" — deterministic beats clever inside the loop;
    "when a turn fails, make the next turn smaller, not smarter"
    (`MANIFESTO.md:48`, line 56).
  - "Take things away" — "Remove them or wire them. Nothing ships 'just
    in case'" (`MANIFESTO.md:60`, line 67).

## Decisions

| Finding | Decision | Change |
|---|---|---|
| Build-artifact trap burns a repair cycle | Do | One sentence in `planPrompt` |
| Repair evidence grows without a bound | Do | Byte cap on appended feedback |
| `TMPDIR` can exceed the Unix socket path limit | Do, carefully | Short private `TMPDIR` with a retention answer |
| No guard against unsatisfiable verify commands | Do not build | None; make the spiral cheap instead |
| zcode permission and planner parity gaps | Defer | Documentation only |

## Do: teach the planner the loop's own contract

Finding. In both benchmark arms the first worker committed
`__pycache__/*.pyc` artifacts. `python3 -m unittest` regenerates them,
and the engine's changed-files invariant rejects the verification
(`internal/looper/integration.go:65`: "Verification changed project
files or failed to capture its result"). Each arm paid one repair cycle
to learn a rule the controller already enforces.

Change. Add one sentence to `planPrompt`
(`internal/looper/agent.go:333`): verify commands must not regenerate
tracked or unignored files, and setup must add ignore rules for build
output when needed.

Reasoning. The engine already enforces this invariant deterministically;
the prompt merely fails to state it, so agents discover it by failing.
Stating an existing rule is wiring, not adding (`MANIFESTO.md:67`). The
rule is general — any build system, not only Python bytecode — and it is
controller-side knowledge no agent CLI vendor will ever ship, so it
passes the absorption test (`PRACTICES-REPORT-2026-10-11.md`, section 6,
lesson 1). The existing prompt already carries the neighboring rules
("Setup must recreate ignored dependencies in each worktree. Never share
mutable build output between worktrees."), so the sentence completes a
contract the prompt already implies.

## Do: cap repair evidence

Finding. `MaxRepairs` (`internal/looper/config.go:18`, default 6) bounds
repair attempts, but nothing bounds the size of the evidence appended to
each attempt. In the first zcode run the diagnosis prompt reached 573,848
input tokens; in the complete run, diagnosis calls carried 322,497 and
421,256. `failureEvidence` (`internal/looper/engine.go:417`) already
strips operation paths before comparing failures across repairs, but the
text still grows monotonically.

Change. Truncate appended repair and diagnosis evidence to a fixed byte
budget, keeping the most recent content and noting the truncation.

Reasoning. The manifesto states the rule directly: "when a turn fails,
make the next turn smaller, not smarter" (`MANIFESTO.md:56`). A byte cap
is that rule made literal: deterministic, no new states, no model
judgment, and it bounds the cost of every pathological loop, including
the unsatisfiable-verify spiral below. A bound needs no paragraph of
justification; it is the same shape as `MaxRepairs` and the dispatch
limit.

## Do carefully: shorten `TMPDIR`

Finding. `resourceEnv` (`internal/looper/agent.go:300`) gives each
attempt `TMPDIR=<runtime>/task-<id>/work-<session>/tmp`. Toolchains that
bind Unix sockets under `TMPDIR` — the zcode stack does — fail with
`listen EINVAL` once the path approaches the kernel's roughly 104-byte
`sun_path` limit. The benchmark needed a wrapper that remaps `TMPDIR` to
a short path (recorded in the design note on commit `b27673a`).

Change. A short private directory, for example
`/tmp/looper-<hash8>`, created with mode 0700 and unique per attempt.

Reasoning. The documented invariant in `README.md` is "a unique
`LOOPER_RESOURCE_PREFIX`, a private `TMPDIR`, and `LOOPER_ARTIFACT_DIR`
... use these for databases and other test resources". The invariant is
private, unique, per-attempt — not "located under the runtime
directory". A short path preserves every promised property while
respecting an operating-system limit the controller cannot negotiate.
This is not a hardcode: it applies to all agents and all projects.

Open wrinkle. Attempt directories are retained ("no automatic retention
limit yet", `README.md`), so short directories need the same lifecycle
or an explicit cleanup, otherwise they accumulate in `/tmp`. This change
touches worker isolation, so it ships as its own small change with
tests, not as a drive-by.

## Do not build: verify-command guards or a re-plan path

Finding. The zcode planner authored a verify command containing
`git merge-base --is-ancestor <task-ref> HEAD` checks against
Looper-private refs. Squash-and-integrate discards candidate lineage, so
the checks can never pass. The loop spent 14 invocations and 3,263,146
input tokens, both diagnosis calls correctly proved the check
unsatisfiable, and the run ended blocked with nothing published.

Options considered and rejected:

- **Plan-time lint** rejecting verify steps that reference
  `refs/looper-tasks/` or ancestry checks. Rejected: it is a deny-list
  of observed patterns. The next agent invents a different
  unsatisfiable check — a coverage threshold, a timing assertion, a
  network probe — and the list grows forever. Partial coverage bought
  with permanent surface is the definition of a temporary fix.
- **Diagnosis-triggered verify re-plan**: when the diagnosis attributes
  failure to the check rather than the code, re-plan the verify command
  instead of repairing code again. Rejected twice over. It lets model
  judgment steer control flow of the deterministic shell, against
  "deterministic beats clever inside the loop" (`MANIFESTO.md:56`), and
  it adds a state to the loop machine, against "fewer states beat better
  explanations" (`MANIFESTO.md:46`).

Reasoning. The manifesto's answer to this finding already exists:
bounded repairs, then block, retain the diagnosis, and idle until a
human edits the backlog. An unattended run has nobody to make the
judgment call, and the controller correctly refuses to make it itself.
The blocked outcome is the product working as designed; what was wrong
was the price. The two accepted changes above are what make that outcome
cheap: the prompt line removes the common trap, the evidence cap bounds
the burn.

Revisit trigger. If unsatisfiable verify commands keep appearing once
real users exist, that evidence justifies a real design — per the
manifesto, a feature that needs a paragraph to justify needs a deadline
to prove value or die (`MANIFESTO.md:69`).

## Defer: zcode parity knobs

Finding. zcode workers always run `--mode yolo` with no
`CODEX_YOLO`-style escape to a restricted mode, and the planner's
restriction is prompt-borne only, weaker than codex's read-only sandbox
and Claude Code's tool restriction.

Reasoning. Both gaps are recorded in the design note on `b27673a`. A
restriction knob that no user has requested is speculative surface:
"Nothing ships 'just in case'" (`MANIFESTO.md:67`). zcode's CLI may
change; tracking it preemptively is the vendor-chasing the report warns
about. Defer until a real zcode user hits a real need.

## References

- `docs/research/BENCHMARK-ZCODE-VS-CLAUDE-2026-10-11.md` (commit
  `cbedc4a`) — the benchmark this note decides on.
- `docs/research/PRACTICES-REPORT-2026-10-11.md` — absorption thesis,
  differentiators to protect, section 9 recommendation 7 (the unrun
  pilot; the benchmark harness in `/tmp/bench/` is a working seed).
- Commits `ef80ecc`, `b27673a` — the zcode adapter and its live-contract
  fixes, with `design` and `todo` git notes carrying the `TMPDIR`
  finding and the verification story.
- Code anchors: `internal/looper/agent.go:300` (`resourceEnv`),
  `internal/looper/agent.go:333` (`planPrompt`),
  `internal/looper/engine.go:417` (`failureEvidence`),
  `internal/looper/integration.go:65` (changed-files invariant),
  `internal/looper/config.go:18` (`MaxRepairs`).
- `MANIFESTO.md` — the decision standard, quoted per section above.
