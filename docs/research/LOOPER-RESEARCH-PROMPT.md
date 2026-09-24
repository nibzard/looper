# Research prompt: Is Looper still useful?

Historical prompt used for the 2026-09-23 report. Some paths below were
archived or removed after the research.

You are an independent researcher of coding agents and developer tools.
Assess whether Looper still solves a real problem. Give a recommendation that a
maintainer can act on. Use current evidence as of the day you conduct the
research. Record that date at the top of your report.

## Project context

Looper is a local runner for autonomous coding tasks. Its active implementation
is `bin/looper.sh`. It stores tasks in `to-do.json`, selects one task per
iteration, starts a fresh Codex or Claude process, and records a structured
summary. It can create and repair the task file, alternate agents, keep logs,
run hooks, and request a final review. The repository also contains an early Go
scaffold and a proposed migration in `MIGRATION.md`. Do not treat that proposal
as an implemented product.

Read the repository before assessing it. Start with `README.md`,
`bin/looper.sh`, `scripts/smoke.sh`, `scripts/regression.sh`,
`AUTONOMOUS-AGENTS.md`, `MIGRATION.md`, `Makefile`, and the Go scaffold. Inspect
the current Git status. Distinguish working behavior, documented behavior,
tests, and plans. If you cannot access the repository, state that limit and
request these files. Do not infer implementation details from this prompt alone.

## Main decision

Answer this question: **Should Looper continue as a separate tool?** Evaluate
four concrete paths:

1. Maintain the Bash runner with a smaller scope.
2. Complete the proposed Go rewrite.
3. Replace most of Looper with native Codex or Claude features, and keep only a
   thin adapter where needed.
4. Archive Looper and document a supported alternative.

Identify the user and workflow for which each path makes sense. State what
Looper offers that the current native tools do not provide. State which Looper
features are now redundant, costly, or likely to lower task quality.

## Questions to investigate

### Current agent capabilities

- Which current OpenAI and Anthropic models are suitable for long coding tasks?
  Verify model names, availability, effort controls, context behavior, and
  relevant costs from official sources. Separate API features from Codex CLI,
  Codex app, Claude API, and Claude Code features.
- How long can the native tools work without an external task loop? What do
  they provide for plans, progress, context compaction, resume, memory,
  checkpoints, background work, schedules, hooks, logs, and review?
- Which guarantees come from the model, which come from its agent runtime, and
  which still require an external controller?
- What changes when Looper starts a fresh process for every task? Assess
  context loss, state recovery, isolation, cost, and error detection.
- Does alternating Codex and Claude improve results in a repeatable way? What
  coordination costs or incompatible instructions does it introduce?

### Official prompting advice

Compare Looper's actual bootstrap, repair, iteration, review, and summary
prompts with current guidance from OpenAI and Anthropic. Examine:

- Outcome and acceptance criteria versus detailed process instructions.
- Prompt length, repeated instructions, and conflicting instructions.
- Model specific guidance for reasoning effort and thinking controls.
- Tool use, planning, verification, and when to stop or ask a question.
- Persistent project instructions, skills, and on demand context.
- Structured output through CLI or API controls versus asking for JSON in text.
- Prompt injection from project files and other untrusted input.
- Long task state, compaction, and handoff across sessions.
- Whether the same prompt should serve Codex and Claude.

For each material prompt rule, cite the exact repository location, the relevant
vendor guidance, and the expected benefit or harm. Mark vendor claims as
vendor claims. Do not assume that a published prompt example improves Looper
without a test.

### Product and engineering value

- Does `to-do.json` add value over native plans, issue trackers, or a Markdown
  status file? Consider task selection, dependencies, blocked tasks, auditability,
  and concurrent runs.
- Which Looper behaviors need deterministic code? Review task selection,
  status changes, summary validation, retries, final review, and completion.
- Does an autonomous review pass improve quality, or can it create false
  confidence? Compare it with independent tests and human review.
- Is the dual agent feature a core use case, a useful option, or maintenance
  cost without measured value?
- What are the costs of the Bash runner and the proposed Go rewrite? Include
  testability, installation, portability, maintenance, and migration risk.
- Are permissions, unattended execution, Git changes, log contents, and
  repeated model calls acceptable for the intended users?
- What is the smallest product that preserves Looper's unique value?

### Alternatives

Compare Looper with the current native Codex and Claude workflows. Include
officially supported automation or agent interfaces where relevant. Compare
other tools only when they address the same user problem. Verify each
alternative's current features from its own documentation. Avoid a broad
catalog of unrelated agent frameworks.

## Evidence rules

- Use repository code and tests as primary evidence for Looper behavior.
- Use current OpenAI and Anthropic documentation as primary evidence for their
  products and prompting advice. Check publication and update dates. Do not
  cite a page that postdates the research date.
- Link to the exact page and section for each important claim. Record the
  access date for pages that change often.
- Separate observed behavior, documentation, vendor claims, and your own
  inference. Flag contradictions between a vendor blog post and product docs.
- Identify missing data. State how to obtain it. Do not invent prices,
  benchmark results, model access, or feature support.
- Give evidence against your preferred recommendation. State what finding
  would change your mind.

## Evaluation design

Design a small, reproducible comparison of:

1. Looper with its current task file and prompts.
2. A native Codex workflow without Looper.
3. A native Claude Code workflow without Looper.
4. A thin hybrid, if the research finds a plausible one.

Use the same repository, task specification, acceptance checks, and resource
limit for each approach. Include at least one new feature, one bug fix, one
blocked task, one interrupted run, and one long task that spans context limits.
Repeat runs enough to expose variance. Track end to end completion, test
results, regressions, human interventions, elapsed time, token use, total cost,
and the ability to explain or resume a run. Define success before running the
comparison. Report failures and abandoned runs.

If you cannot run paid model experiments, give an executable study plan and
label the product recommendation as provisional. Do not substitute a demo or
vendor benchmark for this comparison.

## Required report

Write a report with these sections:

1. **Decision in one page:** recommended path, target user, confidence, and
   the strongest evidence for and against it.
2. **What Looper does today:** a concise map of its active code, prompts,
   state, and failure handling, with file references.
3. **Capability comparison:** a table that distinguishes each product surface
   and shows whether Looper adds, duplicates, or weakens a capability.
4. **Prompt audit:** a table of current prompt instructions, vendor advice,
   observed or expected effects, and proposed edits.
5. **Experiment:** methods, results if available, and limits. Include the
   task set and raw measurements needed to repeat the study.
6. **Recommendation:** keep, narrow, rewrite, integrate, or archive. Explain
   the decision rule and the evidence that could reverse it.
7. **Action plan:** no more than five ordered changes for the next two weeks.
   Include a stop condition for work that fails to show value.
8. **Sources:** exact URLs, publication or update dates, and access dates.

Provide two revised prompt examples only if the audit shows a clear need: one
for Codex and one for Claude. Keep each example short. Explain which rules
belong in the runtime or project instructions instead of the task prompt.

## Official starting points

Check for newer guidance before using these pages:

- [OpenAI model guidance](https://developers.openai.com/api/docs/guides/latest-model)
- [OpenAI prompt engineering](https://developers.openai.com/api/docs/guides/prompt-engineering)
- [OpenAI agent runtime options](https://developers.openai.com/api/docs/guides/agents)
- [OpenAI guidance on skills and project prompts](https://developers.openai.com/blog/rethinking-skills-and-prompts-for-gpt-6-astra)
- [OpenAI example of long coding tasks](https://developers.openai.com/blog/run-long-horizon-tasks-with-codex)
- [Anthropic prompting best practices](https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/claude-prompting-best-practices)
- [Claude Code best practices](https://code.claude.com/docs/en/best-practices)
- [Claude Code project memory](https://code.claude.com/docs/en/memory)
