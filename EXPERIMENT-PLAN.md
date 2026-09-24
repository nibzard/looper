# Looper comparison pilot

Status: designed, not run. This pilot uses paid model calls.

## Question

Does Looper improve task completion, interruption recovery, or audit quality
enough to justify its maintenance cost?

## Fixture and arms

Freeze one small repository at a commit with independent acceptance tests.
Create fresh clones for every run. Use three task cases: a feature, a task
blocked by a missing credential, and a run interrupted at a fixed point.

Compare these four arms:

1. The Looper revision before the controller changes, frozen as a separate
   revision. Record the exact commit and working-tree patch.
2. `codex exec` with a durable task file and acceptance criteria. Use one
   scripted launch per case, with no Looper state machine.
3. One headless Claude Code launch per case, with a Stop hook that runs the
   acceptance command. Do not add an external task loop to this arm.
4. The Looper revision after the controller changes, frozen at a commit.

Run each arm twice for each case. This makes 24 runs in total. Use the same
model, effort, tool access, time limit, and spending cap where the clients
allow them. Record any difference that cannot be matched.

## Measurements

The harness, not the agent, checks the final repository and task state. For
each run, record:

- Whether the acceptance tests pass and whether old tests regress.
- Whether a blocked task stays blocked without a false `done` result.
- Whether interrupted work resumes or stops in a clear state.
- Number of human interventions and their reasons.
- Elapsed time, model tokens, and cost from each client's telemetry.
- Whether the audit log explains the chosen task, result, and failure.

Judge each case separately. Report success as a count out of two repeats per
arm and case. Report median cost only for runs that meet the same acceptance
criteria. Do not combine the three cases into an unexplained score.

## Decision

Keep Looper if it improves blocked-task handling or recovery without a lower
completion rate or a material cost increase. Archive it if a direct client arm
matches those outcomes with less maintenance. Treat a tie or conflicting
results as inconclusive, then add repeats before making a product decision.
