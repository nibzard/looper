# Autonomous Agent Wrappers

This guide explains how to run Codex CLI and Claude CLI as non-interactive autonomous agents from Bash.

The central idea is simple:

Treat Claude or Codex as a short-lived worker process inside a controlled Bash loop, not as a chat session.

That framing is what makes autonomous agent wrappers reliable. The shell script owns state, scheduling, validation, recovery, and completion. The agent only performs one bounded unit of work and returns a machine-readable result.

The examples below use `task`, `queue`, and `state file` terminology, but the same control-plane pattern works for many workflows:

- coding projects
- article or newsletter writing
- research pipelines
- email classification and reply drafting
- support ticket processing
- document review and editing

## Main Lessons

1. Use a layered config model: defaults in variables, then environment overrides, then CLI flags.
2. Keep CLI selection and dispatch explicit so behavior is swappable and testable.
3. Build command arrays, not shell strings, to avoid quoting and spacing bugs.
4. Normalize and validate user inputs early to prevent invalid runtime states.
5. Gate required dependencies with fast checks so failures happen early and clearly.
6. Use a machine-readable contract for expected output from the agent.
7. Ask for JSON-only output whenever possible.
8. Capture raw output plus a canonical final message file per run.
9. Prefer a unified JSONL event log for debugging and automation.
10. Add tolerant parsers because response shapes drift in real usage.
11. Support both streaming progress and silent modes without changing control flow.
12. Design for resumability: bootstrap missing state, validate schema, recover interrupted work, and continue safely.
13. Keep per-tool flag builders separate because Codex and Claude do not expose the same interfaces.
14. Distinguish orchestration failures from task-result failures.
15. Apply state transitions deterministically after each run.
16. Make external hooks explicit instead of baking side effects into the core loop.
17. Include a final review or completion pass.
18. Add bounded safety rails like max iterations and retry caps.
19. Print run metadata up front for reproducibility.
20. Separate runner logic from use-case logic so the wrapper can be reused.

## Non-Interactive Agent Model

The most important design choice is to stop thinking in terms of an ongoing conversation.

For autonomous use, each agent run should look like this:

1. The wrapper selects one task.
2. The wrapper builds a prompt with the task, rules, and output contract.
3. The wrapper starts a fresh Codex or Claude process.
4. The agent works only on that task.
5. The agent returns a final JSON summary.
6. The wrapper validates the summary.
7. The wrapper updates persistent state.
8. The process exits.

Fresh process per iteration is better than one long-lived interactive process because it reduces context drift, prompt accumulation, memory contamination, and undefined workflow state.

## Required Architecture

If you want this pattern to work for any application, content workflow, or operations queue, keep the same fixed control-plane design:

1. `state file`
The source of truth for pending work, often `to-do.json`.

2. `schema`
A JSON Schema or equivalent validation rule set for the state file and agent output.

3. `runner`
Functions that invoke `codex` or `claude` non-interactively.

4. `prompt builder`
A function or heredoc that tells the agent what one run must do.

5. `summary parser`
Logic that extracts the final machine-readable result from tool output.

6. `state applier`
Deterministic code that mutates the state file based on the validated summary.

7. `recovery logic`
Repair, retry, and state rollback paths for interrupted or malformed runs.

8. `completion logic`
A clear done condition so the loop stops intentionally instead of drifting or hanging.

## Setup Recipe

### 1. Define one unit of work per run

The wrapper must force one bounded task per agent process.

Good examples:

- implement one feature
- fix one bug
- draft one article section
- classify one email batch
- review one repository state
- update one document or dataset

Bad examples:

- finish the whole project
- keep working until everything feels complete
- do whatever seems most important

Single-task runs are easier to validate, easier to retry, and easier to recover from.

### 2. Persist state outside the agent

Do not rely on the agent to remember prior work. Persist task state in a file the wrapper owns.

Minimal example:

```json
{
  "schema_version": 1,
  "context_files": ["brief.md", "audience.md"],
  "tasks": [
    {
      "id": "T1",
      "title": "Draft introduction for launch article",
      "priority": 1,
      "status": "todo"
    }
  ]
}
```

The agent is stateless between runs. Your script is the system of record.

### 3. Start every run with fresh context

Spawn a new process on every iteration:

- `codex exec ...`
- `claude -p ...`

Do not keep one long-lived session open and keep feeding it work. That mode is convenient for humans but weak for automation.

### 4. Build command arrays, not command strings

This is a core Bash pattern to copy directly.

```bash
CODEX_FLAGS=(
  exec
  -m "$CODEX_MODEL"
  -c "model_reasoning_effort=$CODEX_REASONING_EFFORT"
  --cd "$WORKDIR"
)

if [ "$CODEX_YOLO" -eq 1 ]; then
  CODEX_FLAGS+=(--yolo)
fi

CLAUDE_FLAGS=(
  --output-format stream-json
  --include-partial-messages
  --verbose
  --dangerously-skip-permissions
  --add-dir "$WORKDIR"
)

if [ -n "$CLAUDE_MODEL" ]; then
  CLAUDE_FLAGS+=(--model "$CLAUDE_MODEL")
fi
```

Then invoke them safely:

```bash
cmd=("$CODEX_BIN" "${CODEX_FLAGS[@]}" --json --output-last-message "$LAST_MESSAGE_FILE" -)
"${cmd[@]}"
```

### 5. Force a machine-readable output contract

Do not build automation around natural-language summaries if you can avoid it.

Require a final JSON object like this:

```json
{
  "task_id": "T123",
  "status": "done",
  "summary": "Implemented login form with validation",
  "files": ["src/auth/login.ts", "src/ui/LoginForm.tsx"],
  "blockers": []
}
```

Recommended fields:

- `task_id`
- `status`
- `summary`
- `files`
- `blockers`

Recommended `status` values:

- `done`
- `blocked`
- `skipped`

### 6. Put the contract directly in the prompt

The prompt must define the scope, allowed actions, and required final output.

Reusable template:

```text
You are running in a non-interactive autonomous work loop.

Goal: complete exactly one task from "state.json" per run.
Selected task:
- id: T123
- title: Draft introduction for launch article
- status: doing

Rules:
- Read "state.json" and follow the schema in "state.schema.json".
- Read every file listed in context_files and treat them as source material.
- If the workflow is not file-based, use the structured context provided with the task.
- Work only on the selected task id.
- Keep scope tight.
- If blocked, set status to "blocked" and include blocker notes.
- If completed, set status to "done".
- Do not ask for confirmation.

Return only a JSON object:
{"task_id":"T123","status":"done","summary":"...","files":["..."],"blockers":[]}
If no task was executed, use status "skipped" and task_id null.
```

This contract is what keeps end-of-run behavior unambiguous. Good autonomous wrappers do not leave completion semantics up to interpretation.

### 7. Capture the final message separately from the raw stream

You need two outputs per run:

1. `raw event log`
For debugging and forensic analysis.

2. `normalized final summary`
For state transitions and automation.

For Codex, the cleanest pattern is:

```bash
codex exec \
  -m "$CODEX_MODEL" \
  --cd "$WORKDIR" \
  --json \
  --output-last-message "$LAST_MESSAGE_FILE" \
  -
```

For Claude, a practical non-interactive pattern is:

```bash
claude -p "$prompt" \
  --output-format stream-json \
  --include-partial-messages \
  --verbose \
  --dangerously-skip-permissions \
  --add-dir "$WORKDIR"
```

Claude usually requires post-processing of the JSON stream to reconstruct the final answer.

### 8. Keep a JSONL event log

Append every structured event to a JSONL file. This gives you:

- traceability
- progress display
- later parsing
- postmortem debugging
- reproducibility

Minimal annotation pattern:

```bash
annotate_line() {
  local line="$1"
  local label="$2"
  local iteration="$3"

  jq -c \
    --arg label "$label" \
    --argjson iter "$iteration" \
    '. + {run_label:$label, run_iteration:$iter}' <<<"$line"
}
```

### 9. Validate agent output before mutating state

The wrapper should behave like a scheduler, not like a passive log collector.

Before applying a summary:

- ensure the output is valid JSON
- ensure required fields are present
- ensure `task_id` matches the selected task
- ensure `status` is allowed
- ensure referenced files and blockers have the right types

Minimal validation example:

```bash
summary_matches_selected() {
  local expected_id="$1"
  local summary_id
  local summary_status

  summary_id=$(jq -r '.task_id // empty' "$LAST_MESSAGE_FILE")
  summary_status=$(jq -r '.status // empty' "$LAST_MESSAGE_FILE")

  [ -n "$summary_id" ] || return 1
  [ -n "$summary_status" ] || return 1
  [ "$summary_status" != "skipped" ] || return 1
  [ "$summary_id" = "$expected_id" ]
}
```

If the summary does not validate, do not blindly mutate the task file.

### 10. Apply state transitions deterministically

Do not ask the agent to be the final authority on control-plane state.

The wrapper should update `to-do.json` itself:

```bash
apply_summary_to_todo() {
  local task_id status now tmp
  task_id=$(jq -r '.task_id // empty' "$LAST_MESSAGE_FILE")
  status=$(jq -r '.status // empty' "$LAST_MESSAGE_FILE")
  now=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
  tmp=$(mktemp)

  jq --arg id "$task_id" \
     --arg status "$status" \
     --arg now "$now" \
     '.tasks |= map(
        if .id == $id then
          .status = $status
          | .updated_at = $now
        else
          .
        end
     )' "$TODO_FILE" > "$tmp" && mv "$tmp" "$TODO_FILE"
}
```

This is the correct split of responsibilities:

- the agent reports what happened
- the wrapper decides what state change is allowed

### 11. Add recovery paths for bad runs

Autonomous scripts will eventually fail. Design for it up front.

Important recovery patterns:

1. Reset interrupted work
If the script dies with tasks marked `doing`, move them back to `todo`.

2. Repair invalid task files
If `to-do.json` drifts out of schema, run a dedicated repair pass.

3. Bootstrap missing state
If the task file does not exist, create it with a bootstrap prompt.

4. Bound the loop
Always cap iterations with something like `MAX_ITERATIONS`.

5. Degrade safely
If summary parsing fails, revert temporary state and skip apply.

Minimal interrupted-work recovery:

```bash
recover_task_states() {
  local tmp now
  now=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
  tmp=$(mktemp)

  jq --arg now "$now" '
    .tasks |= map(
      if .status == "doing" then
        .status = "todo"
        | .updated_at = $now
      else
        .
      end
    )
  ' "$TODO_FILE" > "$tmp" && mv "$tmp" "$TODO_FILE"
}
```

### 12. Add a final review phase and a real done condition

Autonomous loops need an explicit stop condition.

Good patterns:

- a review pass that checks whether more tasks are needed
- a final marker like `project-done`
- a retry cap if the review agent keeps failing

Without an explicit completion marker, the script either exits too early or loops forever.

## Reusable Bash Patterns

### Dependency checks

```bash
require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Error: required command not found: $1" >&2
    exit 1
  fi
}
```

### Input normalization

```bash
lowercase() {
  printf "%s" "$1" | tr '[:upper:]' '[:lower:]'
}

normalize_agent() {
  case "$(lowercase "$1")" in
    codex) echo "codex" ;;
    claude) echo "claude" ;;
    *) echo "$1" ;;
  esac
}
```

### Safe truncation for logs

```bash
shorten() {
  local input="$1"
  local max="${2:-120}"
  if [ ${#input} -gt "$max" ]; then
    printf "%s..." "${input:0:max}"
  else
    printf "%s" "$input"
  fi
}
```

### Workspace-specific log directory

```bash
hash_path() {
  if command -v shasum >/dev/null 2>&1; then
    printf "%s" "$1" | shasum | awk '{print substr($1,1,8)}'
    return 0
  fi
  printf "%s" "$1" | cksum | awk '{print $1}'
}

resolve_log_dir() {
  local root="$1"
  local name slug hash
  name=$(basename "$root")
  slug=$(printf "%s" "$name" | tr -cs 'A-Za-z0-9._-' '_' | sed 's/^_//;s/_$//')
  hash=$(hash_path "$root")
  echo "${HOME}/.agent-runs/${slug}-${hash}"
}
```

### Strip code fences from agent output

```bash
strip_json_fence() {
  local text="$1"
  local first_line last_line
  first_line=$(printf "%s" "$text" | sed -n '1p')
  last_line=$(printf "%s" "$text" | sed -n '$p')

  if printf "%s" "$first_line" | sed -n '/^```/p' >/dev/null 2>&1 &&
     printf "%s" "$last_line" | sed -n '/^```/p' >/dev/null 2>&1; then
    printf "%s" "$text" | sed '1d;$d'
    return 0
  fi

  printf "%s" "$text"
}
```

### Extract JSON from mixed output

```bash
extract_json_from_text() {
  local text="$1"
  local candidate=""

  if printf "%s" "$text" | jq -e . >/dev/null 2>&1; then
    printf "%s" "$text"
    return 0
  fi

  candidate=$(printf "%s" "$text" | awk '
    BEGIN { inside=0 }
    /^```/ {
      if (inside == 0) { inside=1; next }
      else { exit }
    }
    { if (inside == 1) print }
  ')

  if [ -n "$candidate" ] && printf "%s" "$candidate" | jq -e . >/dev/null 2>&1; then
    printf "%s" "$candidate"
    return 0
  fi

  return 1
}
```

### Codex runner

```bash
run_codex() {
  local label="$1"
  local prompt="$2"
  local cmd=("$CODEX_BIN" "${CODEX_FLAGS[@]}" --json --output-last-message "$LAST_MESSAGE_FILE" -)

  printf "%s" "$prompt" | "${cmd[@]}" 2>&1 | tee -a "$LOG_FILE"
}
```

### Claude runner

```bash
run_claude() {
  local label="$1"
  local prompt="$2"
  local output_file
  output_file=$(mktemp)

  "$CLAUDE_BIN" -p "$prompt" "${CLAUDE_FLAGS[@]}" 2>&1 | tee "$output_file" | tee -a "$LOG_FILE"

  local text normalized
  text=$(cat "$output_file")
  normalized=$(strip_json_fence "$text")
  extract_json_from_text "$normalized" > "$LAST_MESSAGE_FILE" || true
  rm -f "$output_file"
}
```

### Single dispatcher

```bash
run_with_agent() {
  local agent="$1"
  local label="$2"
  local prompt="$3"

  case "$agent" in
    claude) run_claude "$label" "$prompt" ;;
    codex|*) run_codex "$label" "$prompt" ;;
  esac
}
```

### Minimal loop skeleton

```bash
iteration=0

while true; do
  iteration=$((iteration + 1))

  if [ "$iteration" -gt "$MAX_ITERATIONS" ]; then
    echo "Reached max iterations."
    break
  fi

  ensure_valid_todo

  if ! has_open_tasks; then
    run_review_pass "$iteration"
    ensure_valid_todo
    if ! has_open_tasks; then
      echo "No open tasks remain."
      break
    fi
    continue
  fi

  selected_task_id=$(current_task_id)
  set_task_status "$selected_task_id" "doing"

  prompt=$(build_iteration_prompt "$selected_task_id")
  run_with_agent "$ITER_AGENT" "iter-$iteration" "$prompt"

  if summary_matches_selected "$selected_task_id"; then
    apply_summary_to_todo
  else
    echo "Warning: summary did not validate; reverting task state." >&2
    set_task_status "$selected_task_id" "todo"
  fi
done
```

## Practical Guidance For Codex And Claude

### Codex

Codex is easiest to automate when you can rely on:

- `exec`
- `--json`
- `--output-last-message`
- `--cd`
- `--yolo` or equivalent non-interactive mode when appropriate

For non-interactive wrappers, Codex is strongest when the final structured message is clean and directly capturable. That makes it useful not only for code changes, but also for structured content workflows and other file-backed tasks.

### Claude

Claude is workable in autonomous mode, but the wrapper usually has to do more output normalization. In practice:

- use `-p` with a fully composed prompt
- use a machine-readable output format if available
- capture the stream
- reconstruct the final text or JSON result
- validate aggressively before applying state

The wrapper around Claude should assume that partial messages and stream events may need interpretation. That applies whether the work item is code, content, research notes, or message handling.

### For both

Always:

- bind the working directory explicitly
- disable interactive confirmations in the prompt
- require a final JSON object
- validate that JSON before mutating persistent state
- separate agent output from state transition logic

## Common Failure Modes

### The agent worked on the wrong task

Mitigation:

- include the selected task id explicitly in the prompt
- require the same task id in the final JSON
- reject or quarantine mismatched summaries

### The agent returned prose instead of JSON

Mitigation:

- say "Return only a JSON object"
- strip code fences
- extract JSON from mixed text
- fail closed if no valid JSON is found

### The wrapper was interrupted mid-run

Mitigation:

- mark active work as `doing`
- recover `doing` back to `todo` on startup

### The state file became invalid

Mitigation:

- validate on every iteration
- run a repair pass
- stop the loop if repair fails

### The loop never terminates

Mitigation:

- maintain a formal done marker
- run a final review pass
- cap retries and max iterations

## How To Reuse This For Another Workflow

If you want to reproduce this pattern for a different codebase or use case, keep the engine and replace only the domain-specific pieces.

Keep these parts:

- config handling
- logging
- Codex runner
- Claude runner
- summary extraction
- validation
- state application
- recovery
- loop control

Customize these parts:

- state schema
- task fields
- task selection policy
- prompt text
- summary schema
- completion criteria
- hook behavior

Examples of use cases that fit this pattern:

- autonomous feature backlog execution
- bug triage and one-fix-per-run loops
- article drafting pipelines
- newsletter production
- email labeling and reply-drafting queues
- support ticket triage
- documentation migration
- code review queues
- refactor campaigns
- dependency update campaigns
- test-fix loops

## Minimal Checklist

Use this checklist before you trust a wrapper in unattended mode:

1. Does every run have exactly one bounded task?
2. Is task state persisted outside the agent?
3. Can the wrapper start fresh without relying on chat memory?
4. Does the prompt require a final JSON object?
5. Does the wrapper validate the final JSON before applying it?
6. Does the wrapper log raw output for debugging?
7. Can interrupted work be recovered safely?
8. Is there a repair path for invalid state files?
9. Is there a final review and explicit done marker?
10. Is there a max-iteration or retry cap?

If the answer to any of these is no, the wrapper is not yet ready for reliable autonomous use.

## Summary

The durable pattern is not "use AI in Bash".

It is:

Use Bash as the deterministic control plane and use Codex or Claude as replaceable non-interactive workers.

That separation of responsibilities is what makes autonomous agents usable across real repositories, writing systems, inbox workflows, research pipelines, and other operational queues.
