# Context transport results

Date: 2026-10-12. Agent: Codex, using its configured default model and effort.

Both context arms pass every fixture check: six runs per arm.
File context reduces the starting prompt and measured input tokens.
It takes longer in this small sample.

## Method

Each case runs twice with inline context and twice with file context.
Arm order alternates on the second repeat.
Both arms use the current standard-input transport and the same agent configuration.
This isolates context selection from operating-system argument limits.

The fixture contains 500 completed tasks and one current task.
An applied requirement specifies the exact output for empty input.
The requirement appears between repeated background notes.

- Planning must return the current task scope and the executable acceptance command.
- Repair must produce a valid task report and pass an independent executable grader.
- Review must report the observed empty-input defect without changing project files.

The repair grader runs its own assertions, rather than trusting the agent report or edited tests.
The review defect is also exposed by the supplied acceptance test.
These cases establish file access and basic behavior; they do not isolate retrieval of an undocumented requirement.

## Measurements

The table reports medians from two runs. Input tokens include cache reads and repeated turns.

| Case | Context | Passes | Prompt bytes | Input tokens | Output tokens | Seconds |
| --- | --- | --- | --- | --- | --- | --- |
| Plan | inline | 2/2 | 440,072 | 219,208 | 422 | 28.4 |
| Plan | files | 2/2 | 2,348 | 88,616 | 736 | 32.4 |
| Repair | inline | 2/2 | 49,154 | 123,737 | 556 | 36.3 |
| Repair | files | 2/2 | 2,221 | 110,295 | 740 | 43.8 |
| Review | inline | 2/2 | 438,582 | 327,150 | 392 | 33.6 |
| Review | files | 2/2 | 934 | 113,370 | 984 | 49.2 |

## Interpretation

Keep standard-input transport and durable context files.
Keep small task contracts inline so workers can start without an extra read.
Keep byte targets advisory. A shorter prompt alone does not prove a cheaper or better run.
Use recorded model usage to compare future changes.

The sample is small and uses one synthetic repository.
Some calls overlap with a separate live smoke test, so elapsed times are observational.
The client does not expose its default model identifier in the recorded usage events.
No monetary cost comparison is reported.
Planning and repair use the production prompt builders.
Review uses a fixed evaluation prompt and the production file-reference helper.
Final infrastructure fixes do not change these prompts.

## Live controller test

The live controller selects T2 before T10 at equal priority.
T2 publishes a nonempty README and becomes done. T10 remains todo.
Exit code 2 is expected because the dispatch limit leaves work pending.

The first smoke run exposed a Git exclusion failure for an ignored context directory.
The fix removes controller-owned context from the temporary index after staging.
A regression test covers this case. The second live smoke run passes.

## Evidence

- Comparison records: [results.json](/tmp/looper-context-eval-20261012/results.json).
- Each comparison directory retains the prompt, invocation, context, agent transcript, and output.
- Successful smoke project: [project](/tmp/tmp.v3lLpwyLH0/project).
- Successful smoke log: [run.log](/tmp/tmp.v3lLpwyLH0/run.log).
- Failed smoke log: [run.log](/tmp/tmp.up1oUZ5uer/run.log).

## Validation status

Focused transport, context, recovery, and runtime regression tests pass.
`make test` and `make race` pass, including 53 runtime cases in each run.
Race checks disable the sanitizer exit delay for supervised helper processes.
This uses the same setting as the existing runtime race checks.
See the [Go race detector options](https://go.dev/doc/articles/race_detector#Options).
Darwin arm64 compilation passes. Darwin runtime validation remains a release follow-up.
Zcode transport remains unchanged; no live Zcode binary is available in this environment.
