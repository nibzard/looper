# Looper research report: should it continue as a separate tool?

Research date: 2026-09-23. All web sources accessed on this date.
Machine state observed on this date: `codex-cli 0.155.1`, `claude 2.1.280`,
`jq` and `jsonschema` installed. Repository at commit `f2516f7` plus uncommitted
working-tree changes described below.

Evidence tiers used in this report:

- **Observed**: behavior verified on this machine (code read, tests run).
- **Vendor claim (verified)**: vendor documentation claim that survived 3-vote
  adversarial verification (24 of 25 claims survived; 1 refuted).
- **Vendor claim (extracted)**: quoted from vendor documentation but not put
  through the 3-vote verification pass.
- **Third-party (extracted)**: non-vendor source, not put through verification.
- **Inference**: this report's own reasoning, flagged as such.

The product recommendation is **provisional**: the controlled comparison the
brief demands (section 5) was designed but not run, because it requires paid
model calls. An executable study plan is included.

---

## 1. Decision in one page

**Recommendation: narrow Looper (path 1) into a thin, vendor-neutral controller
(path 3). Cancel the Go rewrite (path 2). Do not archive (path 4).**

Keep the deterministic shell around unattended runs: task selection and status
transitions in `to-do.json`, summary validation, task-file restoration on
failure, iteration caps and exit codes, JSONL audit logs, post-iteration hooks,
and cross-vendor process management. Remove what the native tools now do
better: planning, prompting, verification prose, JSON extraction, and review
choreography.

**Target user.** A single maintainer who wants an unattended, auditable backlog
runner over local repositories, with deterministic state that survives process
restarts and does not depend on one vendor's session features.

**Confidence.** Medium. The capability evidence is strong (24 vendor claims
verified 3-0). The comparative performance evidence is absent (no experiment
run), so the keep-versus-archive call rests on documented native limits plus
one official endorsement of the architecture.

**Strongest evidence for keeping a thin controller:**

- Anthropic's current best-practices guide explicitly endorses an external
  script looping headless `claude -p` over a file-based task list, and grounds
  it in measured context behavior: "a clean session with a better prompt
  almost always outperforms a long session with accumulated corrections"
  (vendor claim, verified 3-0). Independent corroboration: Chroma's "Context
  Rot" study, July 2025, found all 18 tested frontier models degrade with
  longer input (third-party, extracted).
- Native completion enforcement has hard limits: the Claude Code `/goal`
  evaluator is a small model, not a deterministic check; Stop hooks are
  overridden after 8 consecutive blocks (raiseable via
  `CLAUDE_CODE_STOP_HOOK_BLOCK_CAP`); Codex scheduled tasks are created from
  the ChatGPT web or desktop app only, and web tasks cannot touch local
  folders; Claude Code scheduled tasks are session-scoped and stop when the
  session exits (vendor claims; the first three verified, the last extracted).
- Looper's own regression suite passes (observed), and its deterministic
  guarantees — selection order, rollback on invalid summary, exit codes — are
  exactly the parts no native feature provides.

**Strongest evidence against (and for archiving):**

- Codex now ships `/plan`, `/goal`, durable-file-memory guidance, and a
  vendor-reported single-session run of about 25 hours, about 13M tokens, about
  30k lines of code with self-verification per milestone (vendor claim,
  verified; one anecdote, labeled "an experiment, not a production rollout"
  by its author).
- Claude Code ships `/goal` evaluators that work in headless `-p` mode, Stop
  hooks, fresh-context subagent review, checkpoints per turn, and
  schema-enforced structured output in headless mode (vendor claims, verified).
- Looper's own maintenance record: four model-default rewrites in about seven
  months, a Go migration stalled at task 1 of 16 for 12 days (observed), and
  uncommitted churn at research time. The tool spends more effort tracking
  vendors than its users spend using it.

**What would change the recommendation.** Run the section 5 experiment. If a
native arm (Codex `/goal` + durable files, or Claude Code headless + Stop hook
+ `/goal`) matches Looper on end-to-end completion, regressions, and human
interventions at equal or lower cost, archive Looper and document that
workflow. If the thin-controller arm clearly beats both native arms, keep
Looper in exactly that shape and stop expanding it.

---

## 2. What Looper does today

Observed from the working tree on 2026-09-23 unless noted. Active
implementation: `bin/looper.sh` (2,345 lines, single Bash file). The Go
scaffold (`cmd/looper/main.go`, `internal/*/doc.go`) totals 20 lines of
package stubs with an empty `main()`; it is a plan, not a product
(`MIGRATION.md`, February 2026, proposes the rewrite).

### 2.1 Code map

| Area | Location | Observed behavior |
|---|---|---|
| Config and CLI | `bin/looper.sh:86-148` | Env defaults + flags; `--smart` swaps model and effort |
| Agent scheduling | `:367-458` | `codex`, `claude`, `odd-even`, `round-robin`; per-role repair and review agents |
| Logging | `:707-891` | JSONL per run under `~/.looper/<slug>-<hash>/`, annotated with run id, label, iteration; `--tail`, `--tail --follow` |
| Task file | `:893-1130` | Writes `to-do.schema.json` if missing; validates with `jsonschema` or jq fallback |
| Selection | `:1064-1088` | `doing` (lowest id) > `todo` (priority 1 first, natural id tie-break) > `blocked`. `depends_on` exists in the schema (`:936`) but selection ignores it |
| Codex runner | `:1680-1716`, `:2068-2087` | `codex exec -m <model> -c model_reasoning_effort=<effort> --cd <dir> --yolo --json --output-last-message <file> -` |
| Claude runner | `:1629-1664`, `:2089-2097` | `claude -p <prompt> --output-format json --dangerously-skip-permissions --add-dir <dir>`; tolerant text/JSON recovery (`:1359-1627`) |
| Summary contract | `:743-758`, `:1747-1772` | JSON object `{task_id, status: done|blocked|skipped, summary, files, blockers}`; wrapper rejects summaries whose `task_id` differs from the selected task |
| Deterministic apply | `:1774-1818` | Wrapper applies status, `files[]`, `blockers[]` itself via jq (`LOOPER_APPLY_SUMMARY=1`) |
| Review pass | `:1820-1873`, `:2159-2194` | When no open tasks: agent reviews repo, appends new tasks or a `PROJECT-DONE` marker (tag `project-done`); 3 consecutive failures abort with exit 1 |
| Recovery | `:1033-1062`, `:2118-2155` | `doing` → `todo` reset only at the max-iterations boundary; interrupted runs (SIGINT) exit 130 without reset, but `doing` tasks are re-selected first on the next run |
| Failure handling | `:2259-2304` | Non-429 agent failure or invalid summary aborts the whole run after restoring the task-file backup; 429 detected by grepping the last result line, retried with exponential backoff (60 s to 600 s cap, ±15% jitter, max 12 per iteration) |
| Rate limiting | `:98-101`, `:865-873`, `:2267-2284` | As above |
| Hooks | `:2326-2330` | `LOOPER_HOOK <task_id> <status> <last_message_json> <label>` after each iteration |
| Doctor, ls | `:212-256`, `:2024-2045` | Dependency and file checks; list tasks by status |

### 2.2 Prompts (heredocs inside `looper.sh`)

- **Iteration** (`:2230-2257`): names the agent "Ralf"; gives selected task id,
  title, status; about 12 rules (read the task file and schema, read
  `source_files`, keep scope tight, set statuses, use jq, Conventional Commits
  one commit per task, no amend, no confirmation); asks for the JSON summary
  object as text.
- **Review** (`:1830-1861`): "review the codebase file-by-file as a senior
  developer reviewing a junior's work"; may only edit the task file; must
  append new tasks or the `PROJECT-DONE` marker.
- **Bootstrap** (`:1948-1964`): scan for project docs, create `to-do.json`,
  "add as many actionable tasks that are needed", all `todo`.
- **Repair** (`:1908-1920`): fix the task file to schema, preserve intent.

### 2.3 Tests (observed passing on 2026-09-23)

- `scripts/smoke.sh`: stub `codex`; asserts selection order (T2 before T10),
  model flags in argv (`gpt-6-sol`/`medium`, `--smart` → `gpt-6-astra`/`max`),
  file creation, schema bootstrap, exit code 2 at `MAX_ITERATIONS=1`, doctor.
- `scripts/regression.sh` (untracked, new): summary mismatch rollback; missing
  summary; agent failure rollback; review failure retried 3 times then exit 1;
  good review marker; Claude flag set (no streaming flags); missing Claude
  binary; `run` alias; jq-only fallback validation; code-fence stripping.

Not covered by tests: the 429 retry path, `--tail --follow`, hooks, bootstrap
through the real prompt flow (the stub writes files directly), and the Claude
output-recovery branches beyond the simple case.

### 2.4 Working-tree and history state (observed)

- Uncommitted: model rename `gpt-5.6-terra` → `gpt-6-sol`, smart effort `high`
  → `max`, removal of `LOOPER_SUMMARY_RETRY_MAX`, addition of
  `--review-agent`/`LOOPER_REVIEW_AGENT`, `make test` wiring, new
  `scripts/regression.sh`.
- `README.md:117` still says "The final review pass always uses Codex",
  contradicting the new `--review-agent` flag documented at `README.md:79-80`.
- Commit history: 2026-02-17, 2026-03-07, 2026-03-11, 2026-03-31, 2026-04-13,
  2026-09-11. Model defaults changed in `8d6a4c9` (gpt-5.4), `48022f7`,
  `365461c`, and the current uncommitted change: four rewrites in about seven
  months, each requiring a `smoke.sh` edit because the test pins model names
  (`scripts/smoke.sh:135,165`).
- The `doctor` file at the repo root is a task backlog the Go migration
  apparently created by dogfooding Looper: 16 tasks, `T001` stuck in `doing`
  since 2026-09-11 with the scaffold still empty 12 days later. `run.json` is
  an empty leftover backlog.

---

## 3. Capability comparison

Legend: **A** = Looper adds value over natives; **D** = duplicates a native
feature; **W** = Looper's version is weaker than the native one; **—** = not
applicable. Vendor statements are claims, marked V; verified ones carry (3-0).

| Capability | Looper (observed) | OpenAI Codex (CLI/app) | Anthropic Claude Code | Verdict |
|---|---|---|---|---|
| Task selection, priorities, statuses | Deterministic jq over `to-do.json` | `/plan` decomposes into reviewable steps (V, 3-0); durable plan files recommended (V, 3-0) | Plan mode; `/batch` splits work across 5-30 subagents (V, extracted) | **A** for determinism and cross-run persistence; native plans live inside one session's state |
| Completion enforcement | `PROJECT-DONE` marker + no-open-tasks check + exit codes | `/goal`: goal text is prompt and completion criteria (V, 3-0) | `/goal` re-checked by a small-model evaluator each turn, works headless (V, 3-0) | **D**, with a native caveat: the Claude evaluator is a small model (V, 3-0); Looper's check is deterministic |
| Verification | None in the wrapper; prompt asks the agent to self-check | Astra "runs tests and checks its work" unprompted (V, 3-0); guidance: put verification criteria in the goal (V, 3-0) | Stop hooks run a check script and block turn end until it passes; cap 8 blocks (V, 3-0) | **W**: Looper leaves verification entirely to prose; Claude Code offers the stronger deterministic gate |
| Final review pass | Cross-file review by a second agent run | — | Fresh-context subagent review of diff + criteria, recommended (V, 3-0); "will usually report some findings even when work is sound" (V, 3-0) | **A** if cross-vendor (see below); otherwise **D** |
| Structured output | JSON-in-text with tolerant parsing (~150 lines) | `--output-schema` supported by the CLI; Looper implements it but disables it by default (`looper.sh:109`, `:1691-1694`) | `claude -p --output-format json --json-schema <schema>` returns a parsed `structured_output` (V, 3-0; live-tested on this machine) | **W**: both CLIs now enforce schemas; Looper parses prose instead |
| Context management | Fresh process per task; 90 s ± 20% sleep between iterations | Auto-compaction; single 25 h / 13M-token run reported (V, 3-0, anecdote) | Auto-compaction; "clean session with a better prompt almost always outperforms a long session" (V, 3-0) | **A** for drift avoidance; matches Anthropic's endorsed pattern |
| Memory across runs | `to-do.json` + `source_files` + git history | Durable file memory named "the most important technique" (V, 3-0) | CLAUDE.md and auto memory load every session (V, extracted) | **D**: same file-based pattern, different file names |
| Resume after interruption | Re-selects `doing` tasks on next start; task-file rollback on failure | `codex exec` designed for scripted, unattended runs (V, extracted) | `--continue`/`--resume`; checkpoints per turn survive resume (V, extracted) | **A/D** mixed: Looper's resume is whole-iteration, not mid-task |
| Scheduling | Any cron/nohup wrapper; loop delay with jitter | Scheduled tasks managed from web/desktop app only; web tasks cannot touch local folders (V, 3-0 qualification) | Scheduled tasks fire only while a session is running and idle (V, extracted) | **A**: unattended local scheduling still needs an external runner |
| Hooks | One post-iteration script hook | Documented hooks workflow (V, 3-0) | Rich hook system (V, 3-0) | **D** |
| Audit log | Annotated JSONL per run, `--tail` | Session logs | Session logs, checkpoints | **A** for cross-run, machine-parsable, vendor-neutral audit |
| Dual-vendor | Codex and Claude runners, 4 schedules | OpenAI publishes a Claude Code plugin that delegates to Codex for review and tasks (V, extracted, one-directional) | Same plugin, from the Claude side | **A** in principle; value unmeasured (see 3.1) |
| Permissions | `--yolo` (Codex) and `--dangerously-skip-permissions` (Claude) always on for iterations | Approval modes configurable | Anthropic recommends `--allowedTools` to scope batch permissions (V, extracted) | **W**: Looper's blanket skip is broader than vendor guidance for batch work |

### 3.1 Dual-agent alternation

The odd-even and round-robin schedules (`README.md:63-117`) have no measured
benefit anywhere in the verified evidence (negative finding; absence of
evidence, not evidence of absence). Current guidance cuts against sharing one
prompt across vendors: "Guidance that helps Sol or Luna may overconstrain
GPT-6 Astra" (V, 3-0), and OpenAI's prompt-engineering guide says different
model types need different prompting (V, 3-0).

Two extracted (not verified) results are relevant and point the same way:

- A controlled experiment on 116 LiveCodeBench tasks (arXiv 2607.21656,
  2026-07-22): Claude reviewing Codex-written drafts raised the pass rate from
  71.6% to 89.7%; Codex reviewing Claude-written drafts lowered it from 91.4%
  to 82.8%. Cross-model review is asymmetric, not uniformly good.
- Greptile's "model inversion" post (2026-07-21): both Claude and GPT review
  pipelines found more high-severity bugs in the other model's code than in
  their own.

Inference: alternation of implementers is an unmeasured hypothesis with known
coordination costs (different output contracts, different instruction
sensitivity). Cross-vendor review has some measured support but with
directionality that matters. Looper should keep one implementer and make the
review agent configurable, which the working tree already does
(`--review-agent`).

### 3.2 Comparable alternatives (extracted, not verified)

- `cook` (github.com/rjcorwin/cook): npm CLI orchestrating Claude Code, Codex,
  and OpenCode; 371 stars, latest commit to main 2026-04-18. Same problem
  space; adoption and failure reports unmeasured here.
- `openai/codex-plugin-cc` (created 2026-03-30, Apache-2.0): runs Codex from
  inside Claude Code for reviews and task delegation.
- Anthropic's own best-practices "fan out across files" recipe (native
  `/batch` first, external `claude -p` loop as the script-driven alternative)
  is the officially supported competitor to Looper's core loop (V, 3-0).

---

## 4. Prompt audit

Rules are quoted from the working tree. "Effect" is observed where tests
cover it, otherwise inference from vendor guidance. Vendor claims are marked V
(3-0 where verified).

| # | Looper rule (location) | Vendor guidance | Expected effect | Proposed edit |
|---|---|---|---|---|
| 1 | "Just for fun we are naming you Ralf..." (`looper.sh:2232`) | None supports persona filler; Astra is more sensitive to instructions in files and prompts (V, 3-0) | Noise in every iteration prompt; no measured benefit | Delete |
| 2 | ~12 process rules per iteration: read schema, read `source_files`, use jq, commit conventions (`:2242-2252`) | Skills and prompts "written as elaborate itineraries or recipes... can now hinder results"; "describe the result you want, not only the activity" (V, 3-0) | Over-specification risk on GPT-6-class models; rules re-sent and re-paid every iteration | Keep outcome + scope + commit policy; move conventions to `AGENTS.md` (Codex) and `CLAUDE.md` (Claude) |
| 3 | "Return only a JSON object: {...}" (`:2254-2256`) plus ~150 lines of tolerant parsing (`:1359-1627`) | Use CLI/API structured-output controls instead of JSON-in-text; `claude -p --json-schema` verified; Codex `--output-schema` exists and Looper already implements it (`:1691-1694`) but defaults it off (`:109`) | Whole class of "invalid summary" failures; parsing drifts when CLI output changes | Claude: pass `--json-schema`, read `structured_output`. Codex: default `CODEX_ENFORCE_OUTPUT_SCHEMA=1`. Delete tolerant parsing |
| 4 | Agent told to set task status itself ("If completed, set status to done", `:2246-2247`) while the wrapper also applies the summary deterministically (`:1774-1818`) | Looper's own guide has the right split: "the agent reports what happened; the wrapper decides what state change is allowed" (`AUTONOMOUS-AGENTS.md`) | Two writers of one file; rollback can clobber agent edits; conflicting instructions | Remove status-edit rules from the prompt; the wrapper stays the sole writer |
| 5 | No acceptance criteria anywhere: tasks have free-form `details`; iteration prompt says "Implement the task fully" (`:2246`) | Goal guidance: include outcome, constraints, and verification criteria up front (V, 3-0); Astra "can feel more tentative about when to stop" (V, 3-0) | Stopping quality depends on the model's judgment alone | Require one acceptance line per task at bootstrap; surface it in the iteration prompt |
| 6 | Verification left to the agent; the wrapper never runs tests | Astra self-verifies; test-encouragement text "can lead to unnecessary testing" (V, 3-0). Claude Code: make the check a `/goal` condition or Stop hook so a script verifies deterministically (V, 3-0) | Prose verification is weaker than a script gate and unmeasured | Add a wrapper-level verify step (`make test` or configured command) whose exit code gates `done` |
| 7 | Review prompt: read the repo "file-by-file as a senior developer reviewing a junior's work" (`:1832-1839`) | Anthropic recommends a fresh-context reviewer that sees the diff and the criteria and reports gaps; warns it "will usually report some findings even when work is sound" (V, 3-0) | Whole-repo read is slow and untargeted; findings without criteria invite false positives | Scope review to the diff since run start plus acceptance criteria; require evidence per finding |
| 8 | `PROJECT-DONE` marker protocol, review re-run until marker (`:1845-1857`, `:2186-2191`) | Same goal as native completion criteria (V, 3-0); no vendor guidance about marker tasks | Works, but the marker is a workaround for missing native stop conditions and pollutes the task list | Keep in the thin controller; encode done-criteria in tasks so the review has something to check |
| 9 | One prompt text serves Codex and Claude (`:2260`, `run_with_agent`) | Prompting is model-specific (V, 3-0); different model types need different prompting (V, 3-0) | Shared rules tuned for one model risk overconstraining the other | Split the small per-agent remainder; shared policy goes to `AGENTS.md`/`CLAUDE.md` |
| 10 | Skills shipped to `~/.codex/skills` only (`install.sh`, `skills/`) | OpenAI: audit skills and `AGENTS.md`-type files; unclear or conflicting guidance can pause work early (V, 3-0) | Claude iterations get no skills; Codex gets unaudited ones | Ship the same policy as `CLAUDE.md` for Claude; audit skill text per current guidance |
| 11 | "Do not ask for confirmation" plus blanket `--dangerously-skip-permissions` / `--yolo` (`:2089-2093`) | Anthropic: "Use `--allowedTools` to scope permissions for batch operations" (V, extracted) | Acceptable for a trusted single-user runner; broader than vendor guidance | Add an allow-list option; document the trust assumption in the README |
| 12 | Bootstrap: "Add as many actionable tasks that are needed" (`:1958`) | Goal-writing guidance: outcome, constraints, verification (V, 3-0) | Task quality is unconstrained; downstream iterations inherit it | Bootstrap must write acceptance criteria and priorities from the source docs |
| 13 | Injection surface: agent-authored task text and `source_files` steer later runs (`:1948-1964`, review pass edits the same file the next iteration obeys) | OpenAI warns instructions in repo files can steer the model and recommends auditing them (V, 3-0) | A self-injection loop: one compromised or confused run reshapes all later runs | Wrapper-side schema validation already helps; document that `to-do.json` is trusted input and review it after bootstrap |

### Revised iteration prompts (short, per the audit above)

Rules that belong in project instructions, not the task prompt: commit
conventions, jq-for-edits, source-doc policy, persona. Rules that belong in
the runtime, not any prompt: status writes, summary schema, verification.

Codex (`codex exec -` stdin):

```text
Complete one task in this repository.

Task: <id> <title> (doing)
Done means: <acceptance line from the task>
Constraints: keep scope to this task; follow AGENTS.md; do not amend history;
one conventional commit for the finished work.

If you cannot finish, stop and say why.
```

Claude (`claude -p` with `--json-schema <summary.schema.json>`):

```text
Complete one task in this repository.

Task: <id> <title> (doing)
Done means: <acceptance line from the task>
Constraints: keep scope to this task; follow CLAUDE.md; do not amend history;
one conventional commit for the finished work.

Return the structured summary. If you cannot finish, return status "blocked"
with the reason in blockers[].
```

---

## 5. Experiment

**Status: designed, not run.** Running it needs paid API calls on the order of
4 arms × 5 tasks × 3 repeats × 2 model calls per iteration. Both CLIs are
installed on this machine, so the plan is executable as written. Until it is
run, the recommendation stays provisional.

### Methods

Fixture repository (identical for all arms): a small Python or Go project with
a test suite, a linter, and five seeded defects, generated once and frozen at
a known commit.

Task set (from the brief):

1. New feature: add a `--json` output flag to the existing CLI (acceptance:
   new tests pass, old tests unchanged).
2. Bug fix: fix seeded defect #1 (acceptance: its failing test now passes).
3. Blocked task: a task that requires a missing API key (acceptance: the run
   must mark it blocked, not fake completion).
4. Interrupted run: kill the runner mid-task at a fixed wall-clock point;
   measure recovery to a correct state (acceptance: no corrupted state, work
   resumes or rolls back cleanly).
5. Long task: a migration across 30 files sized to exceed one context window
   (acceptance: all references updated, tests pass).

Arms:

- A. Looper as shipped (current prompts and defaults).
- B. Native Codex: `/goal` with durable plan and status files per OpenAI's
  long-running-work guidance, one session per task set.
- C. Native Claude Code: headless `claude -p` loop per Anthropic's fan-out
  recipe, with a Stop hook running the test suite and `/goal` conditions from
  acceptance lines.
- D. Thin hybrid: Looper reduced per section 7 (schema-enforced output,
  pruned prompts, wrapper-run verification, single implementer plus
  cross-vendor review).

Controls: same fixture commit, same acceptance checks, same per-arm budget
(for example 20 USD or 5M tokens, whichever first), `MAX_ITERATIONS=50`,
three repeats per arm per task in fresh clones.

Metrics recorded per run: end-to-end completion against acceptance lines;
independent test results (run by the harness, never by the agent under test);
regressions (harness diff review); human interventions (count and kind);
elapsed time; token use per CLI telemetry; cost at list prices; ability to
explain the run (structured log present and parses) and to resume after the
interrupted-run task.

Success definition (set before running): an arm wins if it completes at least
17 of 20 task instances (4 tasks × 5 arms-adjusted instances, repeats
collapsed by majority) with zero undetected regressions and the lowest median
cost. Looper's deterministic value is confirmed if arms A or D beat B and C on
interrupted-run recovery and audit completeness at similar cost.

### Limits

No results exist yet. Do not treat vendor benchmarks (including the 25-hour
anecdote) as substitutes. Prices below are API list prices and change; vendor
claims are self-reports. Claude per-token prices for the current lineup were
not among the verified claims — pull them from
`platform.claude.com/docs/en/models/overview` before costing the runs.

---

## 6. Recommendation

**Narrow to the thin controller.** Decision rule: keep only behaviors that
need deterministic code outside the agent — selection order, status writes,
summary validation, rollback, iteration caps, exit codes, audit logs, hooks,
and process management across vendors. Drop everything the runtimes now own.

Path-by-path:

1. **Maintain Bash with a smaller scope — yes, as the end state.** The
   2,345-line script shrinks by roughly half: delete tolerant parsing, prune
   prompts, delegate conventions to project instruction files.
2. **Complete the Go rewrite — no.** Zero implementation after 12 days, no
   evidence the Bash script's limits (testability, portability) have bitten in
   practice — the regression suite covers the state machine — and a rewrite
   multiplies exactly the vendor-tracking maintenance that is already the
   project's main cost. Archive `MIGRATION.md` and the scaffold or delete
   them.
3. **Replace most of Looper with native features, keep a thin adapter —
   yes, this is the same end state as 1.** The adapter keeps: task file,
   selection, validation, logs, hooks, cross-vendor runners. It drops:
   prompt engineering, verification prose, JSON recovery, review
   choreography.
4. **Archive — only if the experiment says so.** Current evidence blocks
   archiving now: Anthropic explicitly endorses the external `claude -p` loop
   pattern, and the documented native limits (small-model goal evaluation,
   8-block Stop-hook cap, app-managed Codex scheduling, session-scoped Claude
   Code scheduling) leave real unattended work for a deterministic runner.

What Looper offers that natives do not (inference from the verified limits):
vendor-neutral deterministic state, cross-run audit, exit codes for CI, and
unattended scheduling on a plain machine.

Redundant now: JSON-in-text parsing (Claude `--json-schema`, Codex
`--output-schema`), planning prompts (`/plan`, plan mode), review
choreography (subagent review, `--review-agent`-style delegation), memory
files (CLAUDE.md, AGENTS.md, durable plan files).

Costly or quality-lowering: model-name pinning (four rewrites in seven
months, each with a test edit), dual-writer task state, one prompt for two
vendors, alternation schedules with no measured benefit, blanket permission
skips, abort-on-first-failure (only 429s retry) in a tool meant for unattended
runs.

Smallest product that preserves the unique value (inference): about 300 lines
of Bash — select task, mark doing, run `codex exec` or `claude -p` with
schema-enforced output, validate the summary, apply status, log JSONL, honor
`MAX_ITERATIONS` and the hook, restore on failure. Everything else goes.

Evidence that could reverse this: the experiment showing native arms matching
Looper on recovery, audit, and regressions at equal or lower cost; or either
vendor shipping deterministic, CLI-managed completion gates and cross-run
task ledgers.

---

## 7. Action plan (two weeks)

1. **Commit or discard the working tree; fix `README.md:117`.** The untracked
   regression suite should land regardless.
2. **Switch to schema-enforced output.** Claude: `--json-schema`, read
   `structured_output`. Codex: default `CODEX_ENFORCE_OUTPUT_SCHEMA=1`.
   Delete the tolerant parsing block and its tests; add regression cases for
   schema rejection.
3. **Prune prompts and stop pinning models.** Apply audit rows 1, 2, 4, 5, 9,
   12; move conventions to `AGENTS.md`/`CLAUDE.md`; read model and effort
   from environment or `codex` config instead of code defaults, and stop
   asserting model names in `smoke.sh`.
4. **Cancel the Go rewrite.** Remove or archive `MIGRATION.md`, `cmd/`,
   `internal/`, `go.mod`, and the stale `doctor` backlog. Declare Bash the
   implementation in the README.
5. **Run the pilot experiment** (arms A-D, tasks 1, 2, and 4 only, two
   repeats) and record results next to this report.

Stop condition: if after steps 2 and 3 a 10-iteration live pilot does not cut
prompt tokens by at least half and eliminate invalid-summary failures, or if
the pilot experiment shows native arms B or C matching arm D on completion and
recovery at equal or lower cost, stop development and archive Looper with a
README pointing to the winning native workflow.

---

## 8. Sources

All accessed 2026-09-23. "Verified 3-0" means the claim survived three
adversarial verification votes; "extracted" means fetched and quoted without
that pass.

OpenAI:

- Latest-model guide (GPT-6 family, reasoning effort rules, Astra behavior,
  AGENTS.md/skills sensitivity): https://developers.openai.com/api/docs/guides/latest-model — undated page; verified 3-0.
- API pricing (gpt-6-sol $2.00/$10.00; gpt-6-luna $0.10/$0.50; gpt-5.3-codex
  $1.75/$0.175 cached/$14.00; gpt-5.4 $2.50/$15.00 per 1M, <272K context):
  https://developers.openai.com/api/docs/pricing — verified 3-0.
- "Rethinking skills and prompts for GPT-6 Astra" (itineraries hinder;
  Astra self-verifies; stopping behavior; model-specific guidance):
  https://developers.openai.com/blog/rethinking-skills-and-prompts-for-gpt-6-astra — published 2026-09-11; verified 3-0.
- "Run long horizon tasks with Codex" (25 h run, durable project memory):
  https://developers.openai.com/blog/run-long-horizon-tasks-with-codex — dated 2026-02-23 by search metadata, page undated; verified 3-0 with hedges.
- Codex docs, now canonical at learn.chatgpt.com (`developers.openai.com/codex`
  308-redirects there): /plan, /goal, scheduled tasks, long-running work,
  non-interactive `codex exec`: https://learn.chatgpt.com/codex ,
  https://learn.chatgpt.com/codex/long-running-work ,
  https://learn.chatgpt.com/docs/developer-commands?surface=cli — verified 3-0
  except the non-interactive-mode positioning quote (extracted).
- Prompt-engineering guide (model-specific prompting):
  https://developers.openai.com/api/docs/guides/prompt-engineering — extracted.
- GPT-5.5 retirement from Codex on 2026-10-14: "What's new" entry dated
  2026-09-14 to 18 on https://learn.chatgpt.com/codex — verified 3-0.
- `/plan` unavailable in non-interactive exec mode: openai/codex issue #3641
  (2025-09-15) — third-party, extracted; qualification only.
- codex-plugin-cc (Codex from inside Claude Code):
  https://github.com/openai/codex-plugin-cc — created 2026-03-30; extracted.

Anthropic:

- Claude Code best practices (external `claude -p` loop recipe; `/batch`;
  context-degradation premise; adversarial review step; `/goal` and Stop-hook
  verification; 8-block cap): https://code.claude.com/docs/en/best-practices —
  undated; verified 3-0.
- Headless mode and `--json-schema` structured output:
  https://code.claude.com/docs/en/headless — verified 3-0 including a live
  test on this machine (claude 2.1.280).
- Hooks guide (Stop-hook cap, `CLAUDE_CODE_STOP_HOOK_BLOCK_CAP`):
  https://code.claude.com/docs/en/hooks-guide — verified 3-0.
- `/goal` evaluator: https://code.claude.com/docs/en/goal — verified 3-0.
- Subagents: https://code.claude.com/docs/en/sub-agents — verified 3-0 (with
  the nuance that subagents still load CLAUDE.md context).
- Scheduled tasks (session-scoped):
  https://code.claude.com/docs/en/scheduled-tasks — extracted.
- Checkpointing (per-turn checkpoints, survive resume):
  https://code.claude.com/docs/en/checkpointing — extracted.
- Memory (fresh context each session; CLAUDE.md and auto memory):
  https://code.claude.com/docs/en/memory — extracted.
- Models overview (Opus 5.5 default; Fable 5.1 for long-horizon agentic work;
  long-horizon state tracking across context windows):
  https://platform.claude.com/docs/en/models/overview — positioning verified
  3-0; the state-tracking sentence is extracted. The stronger claim that
  Anthropic positions model choice over external loops was refuted 0-3 and is
  excluded.
- Prompting best practices:
  https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/claude-prompting-best-practices — extracted.
- Think-tool article (native extended thinking now preferred; update dated
  2025-12-15; URL moved to
  https://www.anthropic.com/engineering/claude-think-tool, original 2025-03-20):
  extracted.

Third-party (extracted, not verified):

- Cross-model writer/reviewer experiment, 116 LiveCodeBench tasks:
  arXiv 2607.21656, published 2026-07-22, https://arxiv.org/abs/2607.21656 .
- Greptile, "model inversion" review experiment (2026-07-21):
  https://www.greptile.com/blog/model-inversion .
- Chroma, "Context Rot" (July 2025): 18 frontier models degrade with longer
  input.
- `cook`, an orchestrating CLI over Claude Code, Codex, and OpenCode:
  https://github.com/rjcorwin/cook — 371 stars, last commit to main
  2026-04-18.

Repository (observed, primary evidence): `bin/looper.sh`, `README.md`,
`AUTONOMOUS-AGENTS.md`, `MIGRATION.md`, `Makefile`, `scripts/smoke.sh`,
`scripts/regression.sh`, `install.sh`, Go scaffold, `doctor`, `run.json`,
git history through `f2516f7` plus the uncommitted working tree.

Missing data and how to obtain it: Claude per-token prices for the current
lineup (models overview page); Codex CLI structured-output support equivalent
to `--json-schema` (check `codex exec --help` on this machine or the CLI
reference); measured alternation benefit and comparable-tool adoption (run
the section 5 experiment and a `cook` trial on the same fixture).
