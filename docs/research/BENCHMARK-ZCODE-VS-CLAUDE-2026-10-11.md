# Looper benchmark: zcode versus Claude Code harnesses

Research date: 2026-10-11. This note records a small controlled
comparison: the same controller, the same task, and the same underlying
model, run through two agent harnesses. It accompanies
`PRACTICES-REPORT-2026-10-11.md` and the zcode adapter added the same day.

## Question

With the model held constant, what do the harnesses cost in time and
tokens, and do they finish the loop?

## Method

- Controller: the Go binary at commit `b27673a`, one task, `--once
  --jobs 1 --yes --plain`, fresh state per arm.
- Task: identical one-task repositories. Implement `wordstats.py`
  (`count_words`, `top_words` with punctuation stripping and alphabetical
  tie-breaks) plus a unittest suite. Acceptance: `python3 -m unittest`
  exits 0.
- zcode arm: the CLI built from `zai-org/ZCode` source, run through a
  tsx wrapper with an API-key login (`GLM-5.3` coding plan). The wrapper
  maps the per-attempt `TMPDIR` to a short path; the zcode stack binds
  Unix sockets under `TMPDIR` and exceeds the socket path limit
  otherwise.
- Claude arm: Claude Code 2.1.280 through this machine's gateway. Result
  events report `glm-5.3` and `glm-5.3[1m]`, so the underlying model
  matches the zcode arm.
- One complete run per arm, plus one earlier partial zcode run that was
  killed at 570 seconds.
- Tokens are summed from the usage objects in each harness's final
  result events. Input counts exclude cache reads.

## Results

| | zcode harness | Claude Code harness |
|---|---|---|
| Wall time | 2049 s (34 min) | 479 s (8 min) |
| Outcome | exit 2; blocked; nothing published | exit 0; goal satisfied; published |
| Agent invocations | 14 | 6 |
| Input tokens | 3,263,146 | 83,812 |
| Output tokens | 134,581 | 30,250 |
| Cache-read tokens | 2,996,096 | 1,228,416 |
| Total processed | about 6.26 M | about 1.31 M |
| Input per invocation | 65 k to 486 k | 6.7 k to 23 k |

## Findings

1. **Both arms hit the same trap.** The first worker in each arm
   committed `__pycache__/*.pyc` artifacts. Running `python3 -m unittest`
   regenerates them, so the changed-files check failed. The trap is
   environmental, not a harness defect.
2. **The escapes diverged completely.** Claude Code recovered in one
   repair: the third worker found the root cause (`.pyc` headers store
   source mtime, so every checkout stales them), untracked the artifacts,
   added `.gitignore`, and finished. zcode never escaped: its planner
   authored a verify command containing git ancestry checks
   (`git merge-base --is-ancestor <task-ref> HEAD`) that the
   squash-and-integrate model structurally cannot satisfy. Content was
   byte-identical and tests passed in every worktree, but the silent
   ancestry step failed each verification until repair limits blocked
   the task. The published tree ended without source files.
3. **Diagnosis quality did not save the run.** Both zcode diagnosis
   calls correctly traced the mechanism, proved all candidate commits
   shared one tree, and predicted that the harvest step would discard
   any lineage fix. Correct analysis of an unsatisfiable check is still
   unactionable.
4. **Context size is the quiet headline.** For the same Looper prompts,
   zcode carried 65 k to 486 k input tokens per invocation where Claude
   Code carried 6.7 k to 23 k. zcode's harness accumulates tool-read
   context and re-sends it; Claude Code keeps turns small. The roughly
   ten-times per-call gap (median 154 k versus 14 k) compounds through
   the repair loop into a 39 times total input gap.
5. **The verify command is a risk class of its own.** The gate is only
   as good as the agent-authored verify command. A vendor-neutral
   controller cannot know an ancestry check contradicts the integration
   model; it fails honestly until blocked. Candidate product answer: a
   plan-time lint that rejects verify steps referencing Looper-private
   refs, or a plan review pass before first dispatch.

## Caveats

- One complete run per arm. The earlier partial zcode run showed the
  same verify-authorship pathology, so the pattern repeated across two
  independent zcode runs, but sample size stays small.
- zcode ran through a tsx development wrapper, adding a few seconds per
  invocation. Negligible against 34 minutes.
- Verify-command authorship is stochastic; a different zcode plan might
  verify with the unittest alone. The systematic signal is the context
  size gap.
- The partial zcode run did publish before spiraling; the complete run
  published nothing. Outcome variance across runs is real.

## Artifacts

`/tmp/bench/` holds both result files, per-invocation token tables, run
logs, and state directories. The zcode adapter's design notes, including
the `TMPDIR` constraint, are attached as git notes to the adapter
commits.
