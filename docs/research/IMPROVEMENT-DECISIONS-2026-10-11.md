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
| Build-artifact trap burns a repair cycle | Do | State the contract in planner and worker prompts |
| Agent invocations consume large contexts | Defer | Measure supplied prompts separately from tool reads |
| `TMPDIR` can exceed the Unix socket path limit | Do | Short private directory with process ownership and cleanup |
| No guard against unsatisfiable verify commands | Defer | Explain integration constraints; retain bounded repairs |
| zcode permission and planner parity gaps | Defer | Documentation only |

## Do: state the verification contract

Finding. In both benchmark arms the first worker committed
`__pycache__/*.pyc` artifacts. `python3 -m unittest` regenerates them,
and the engine's changed-files invariant rejects the verification
(`internal/looper/integration.go:65`: "Verification changed project
files or failed to capture its result"). Both arms encountered a rule the
controller already enforces. Claude
Code recovered; zcode also encountered an unsatisfiable verification step.

Change. State the contract in `planPrompt` and `workerPrompt`: setup
and verification must leave tracked and unignored project files unchanged.
Workers must include ignore rules in their implementation and remove
tracked build artifacts when necessary. Setup recreates ignored dependencies;
it must not edit ignore rules during verification. Ignore rules alone
cannot exclude files that Git already tracks.

Explain that checks run in a fresh integrated worktree and verify product
behavior. Checks must not depend on Looper-private refs or worker commit
history. This is an execution constraint the planner needs to know.

Reasoning. The engine already enforces this invariant deterministically;
the prompt merely fails to state it, so agents discover it by failing.
Stating an existing rule is wiring, not adding (`MANIFESTO.md:67`). The
rule applies to any build system. It describes Looper's execution model,
which Looper must explain to each agent. The existing prompt already
carries the neighboring rules
("Setup must recreate ignored dependencies in each worktree. Never share
mutable build output between worktrees."), so these instructions complete a
contract the prompt already implies.

## Defer: cap repair evidence

Finding. `MaxRepairs` bounds repair attempts. The benchmark records large
input token totals for diagnosis invocations, including 322,497 and
421,256 in the complete run. These totals include context collected during
the invocation; they do not measure the supplied prompt alone.

The code replaces repair feedback on each repair. `logTail` already limits
individual log excerpts to 16,000 bytes. Diagnosis also directs the agent
to read retained attempt records. A cap on supplied feedback cannot bound
those tool reads or the harness's resulting context.

Decision. Defer a new evidence cap. First measure supplied prompt bytes
separately from context collected through tool reads. Add a limit only if
that evidence identifies a controller-owned source of excessive input.

Reasoning. A deterministic limit fits Looper when it bounds a demonstrated
problem. This benchmark does not establish that appended feedback causes
the observed cost. Repair limits bound attempts, not the cost of each
agent invocation. Do not claim that failure is cheap without measuring it.

## Do: shorten `TMPDIR`

Finding. The previous `resourceEnv` placed `TMPDIR` under the runtime
operation directory. Toolchains that bind Unix sockets under `TMPDIR` can
exceed the operating system's socket path limit. The live zcode run failed
with `listen EINVAL` until a wrapper supplied a short path.

Change. `runProcess` creates a directory under `/tmp` with `os.MkdirTemp`
for each child process that receives Looper resource variables. The
fixed short root avoids long inherited `TMPDIR` and runtime paths.
Directories have mode 0700 and separate names. Creation errors stop the
operation. The process runner owns cleanup after exit, failure, or
cancellation, once it terminates the child process group.

`LOOPER_RESOURCE_PREFIX` remains scoped to the operation. `TMPDIR` is
private to each child process, including retries. Setup, workers, and
checks must not pass files through `TMPDIR`. Retained evidence belongs
in `LOOPER_ARTIFACT_DIR`, which keeps its existing retention behavior.

Reasoning. Looper owns the execution environment. Its directory layout
must support ordinary tools, including tools that bind Unix sockets.
This fixes that responsibility without adding an agent-specific rule,
a configuration knob, or another loop state.

Lifecycle limit. An abrupt controller termination can bypass cleanup and
leave temporary directories behind. This change does not add a retention
service. Tests cover socket binding under a deep runtime path, directory
permissions, isolation, and cleanup after success, failure, and cancellation.

## Defer: verify-command guards or a re-plan path

Finding. The zcode planner authored a verify command containing
`git merge-base --is-ancestor <task-ref> HEAD` checks against
Looper-private refs. Squash-and-integrate discards candidate lineage, so
the checks can never pass. The loop spent 14 invocations and 3,263,146
input tokens, both diagnosis calls correctly proved the check
unsatisfiable, and the run ended blocked with nothing published.

Options considered and deferred:

- **Plan-time lint** rejecting verify steps that reference
  `refs/looper-tasks/` or ancestry checks. Deferred: it is a deny-list
  of observed patterns. The next agent invents a different
  unsatisfiable check. One observed pattern does not justify a growing
  list of special cases. First explain the integration contract.
- **Diagnosis-triggered verify re-plan**: when the diagnosis attributes
  failure to the check rather than the code, re-plan the verify command
  instead of repairing code again. Deferred: it adds another recovery
  transition and lets diagnosis change the verification gate. That needs
  evidence and a design that preserves the acceptance criteria.

Reasoning. Keep bounded repairs, then block, retain the diagnosis, and
wait for a human to edit the backlog. State the integration contract
before adding another recovery mechanism. The contract clarification can
prevent known misunderstandings, but does not guarantee planner compliance.
Neither accepted change bounds the token cost of an agent invocation.

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
- Code anchors: `internal/looper/agent.go` (`resourceEnv`, `planPrompt`,
  `workerPrompt`), `internal/looper/process_unix.go` (`runProcess`),
  `internal/looper/integration.go` (`repair`, `diagnose`, `logTail`,
  changed-files invariant), and `internal/looper/config.go` (`MaxRepairs`).
- `MANIFESTO.md` — the decision standard, quoted per section above.
