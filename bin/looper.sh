#!/usr/bin/env bash

: <<'LOOPER_DOC'
Codex RALF Loop (looper.sh)
---------------------------
Purpose:
  Run Codex CLI in a deterministic, autonomous loop that processes one task
  per iteration from a JSON backlog (to-do.json), with fresh context each run.
  The loop bootstraps tasks when missing, validates schema, repairs invalid
  task files, logs JSONL output, and optionally applies deterministic status
  updates based on the model's final JSON summary.

Usage:
  looper.sh [--interleave] [--iter-schedule <schedule>] [to-do.json]
  looper.sh --ls <status> [to-do.json]
  looper.sh --tail [--follow]

Core behavior:
  - Creates to-do.schema.json if missing.
  - Creates to-do.json (via Codex) if missing.
  - Validates to-do.json (jsonschema if available; jq fallback).
  - Repairs to-do.json via Codex or claude if schema validation fails.
  - Runs Codex or claude exec in a loop, one task per iteration.
  - Runs a final review pass when all tasks are done, which may add new tasks
    or append a project-done marker (set only by the review pass).
  - Stores a source_files list in to-do.json for ground-truth project docs.
  - Expects the final response to be JSON and logs it for hooks/automation.
  - Stores one JSONL log per invocation in ~/.looper/<project>-<hash>/ by default.
  - Initializes a git repo automatically if missing (optional).
  - Provides a --ls mode to list tasks by status (todo|doing|blocked|done).
  - Provides a --tail mode to print the last activity from the latest log.
  - Provides a --tail --follow mode to keep printing new activity.
  - Prints the current task id and title per iteration.

Environment variables:
  MAX_ITERATIONS           Max iterations (default: 50)
  CODEX_MODEL              Model (default: gpt-5.2-codex)
  CODEX_REASONING_EFFORT   Model reasoning effort (default: xhigh)
  CODEX_YOLO               Use --yolo (default: 1)
  CODEX_FULL_AUTO          Use --full-auto if not using --yolo (default: 0)
  CODEX_PROFILE            Optional codex --profile value
  CODEX_JSON_LOG           Enable JSONL logging (default: 1)
  CODEX_PROGRESS           Print compact progress (default: 1)
  CODEX_ENFORCE_OUTPUT_SCHEMA  Validate final summary via JSON Schema (default: 0)
  CLAUDE_BIN                claude CLI binary (default: claude)
  CLAUDE_MODEL              claude model (default: empty)
  LOOPER_ITER_SCHEDULE     Iteration schedule (codex|claude|odd-even|round-robin)
  LOOPER_ITER_ODD_AGENT    Odd-iteration agent (default: codex)
  LOOPER_ITER_EVEN_AGENT   Even-iteration agent (default: claude)
  LOOPER_ITER_RR_AGENTS    Round-robin agents list (default: claude,codex)
  LOOPER_REPAIR_AGENT      Repair agent (codex|claude; default: codex)
  LOOPER_INTERLEAVE        Enable interleave defaults (default: 0)
  LOOPER_BASE_DIR          Base log dir (default: ~/.looper)
  LOOPER_APPLY_SUMMARY     Deterministically apply summary to to-do.json (default: 1)
  LOOPER_GIT_INIT          Run git init if missing (default: 1)
  LOOPER_HOOK              Optional hook called after each iteration:
                           <hook> <task_id> <status> <last_message_json> <label>
  LOOP_DELAY_SECONDS       Sleep between iterations (default: 0)

Notes:
  - If CODEX_YOLO=1, --full-auto is ignored.
  - Logs are stored per project (repo root or current directory) and keep
    the full JSONL stream plus a "last message" JSON file.
  - The loop never asks for confirmation; it is designed for autonomous runs.
LOOPER_DOC

# Table of contents
# - Configuration and globals
# - CLI parsing and scheduling
# - Logging and progress output
# - Schema and task selection
# - Agent runners (codex/claude)
# - Summary handling and hooks
# - Main loop and entrypoints

set -u
set -o pipefail

MAX_ITERATIONS=${MAX_ITERATIONS:-50}
TODO_FILE=${TODO_FILE:-to-do.json}
SCHEMA_FILE="${TODO_FILE%.json}.schema.json"

CODEX_BIN=${CODEX_BIN:-codex}
CODEX_MODEL=${CODEX_MODEL:-gpt-5.2-codex}
CODEX_REASONING_EFFORT=${CODEX_REASONING_EFFORT:-xhigh}
CLAUDE_BIN=${CLAUDE_BIN:-claude}
CLAUDE_MODEL=${CLAUDE_MODEL:-}
LOOP_DELAY_SECONDS=${LOOP_DELAY_SECONDS:-0}
WORKDIR=$(pwd)
LOOPER_BASE_DIR=${LOOPER_BASE_DIR:-${LOOPER_LOG_DIR:-"$HOME/.looper"}}
LOOPER_LOG_DIR=""
SUMMARY_SCHEMA_FILE=""
CODEX_JSON_LOG=${CODEX_JSON_LOG:-1}
CODEX_PROGRESS=${CODEX_PROGRESS:-1}
CODEX_PROFILE=${CODEX_PROFILE:-}
CODEX_ENFORCE_OUTPUT_SCHEMA=${CODEX_ENFORCE_OUTPUT_SCHEMA:-0}
CODEX_YOLO=${CODEX_YOLO:-1}
CODEX_FULL_AUTO=${CODEX_FULL_AUTO:-0}
LOOPER_ITER_SCHEDULE=${LOOPER_ITER_SCHEDULE:-codex}
LOOPER_ITER_ODD_AGENT=${LOOPER_ITER_ODD_AGENT:-codex}
LOOPER_ITER_EVEN_AGENT=${LOOPER_ITER_EVEN_AGENT:-claude}
LOOPER_ITER_RR_AGENTS=${LOOPER_ITER_RR_AGENTS:-claude,codex}
LOOPER_REPAIR_AGENT=${LOOPER_REPAIR_AGENT:-codex}
LOOPER_INTERLEAVE=${LOOPER_INTERLEAVE:-0}
ITER_SCHEDULE_SET=0
REPAIR_AGENT_SET=0
LOOPER_APPLY_SUMMARY=${LOOPER_APPLY_SUMMARY:-1}
LOOPER_GIT_INIT=${LOOPER_GIT_INIT:-1}
LOOPER_HOOK=${LOOPER_HOOK:-}
RUN_ID=""
LOG_FILE=""
LAST_MESSAGE_FILE=""
LAST_MESSAGE_TEMP=0
CAPTURE_LAST_MESSAGE=0

usage() {
    echo "Usage: looper.sh [--interleave] [--iter-schedule <schedule>] [to-do.json]"
    echo "       looper.sh --ls <status> [to-do.json]"
    echo "       looper.sh --tail [--follow|-f]"
    echo "       looper.sh --doctor [to-do.json]"
    echo "       looper.sh --check [to-do.json]"
    echo "Options: --interleave, --iter-schedule <codex|claude|odd-even|round-robin>"
    echo "         --odd-agent <codex|claude>, --even-agent <codex|claude>"
    echo "         --rr-agents <claude,codex>, --repair-agent <codex|claude>"
    echo "         --claude-bin <path>, --claude-model <model>"
    echo "Env: MAX_ITERATIONS, CODEX_MODEL, CODEX_REASONING_EFFORT, CODEX_JSON_LOG, CODEX_PROGRESS"
    echo "Env: LOOPER_BASE_DIR, CODEX_PROFILE, CODEX_ENFORCE_OUTPUT_SCHEMA, CODEX_YOLO, CODEX_FULL_AUTO"
    echo "Env: CLAUDE_BIN, CLAUDE_MODEL, LOOPER_ITER_SCHEDULE, LOOPER_ITER_ODD_AGENT"
    echo "Env: LOOPER_ITER_EVEN_AGENT, LOOPER_ITER_RR_AGENTS, LOOPER_REPAIR_AGENT"
    echo "Env: LOOPER_INTERLEAVE"
    echo "Env: LOOPER_APPLY_SUMMARY, LOOPER_GIT_INIT, LOOPER_HOOK, LOOP_DELAY_SECONDS"
}

require_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "Error: required command not found: $1" >&2
        exit 1
    fi
}

doctor_check_cmd() {
    local cmd="$1"
    local label="${2:-$1}"
    local required="${3:-1}"

    if command -v "$cmd" >/dev/null 2>&1; then
        echo "ok: $label ($cmd)"
        return 0
    fi

    if [ "$required" -eq 1 ]; then
        echo "missing: $label ($cmd)" >&2
        return 1
    fi

    echo "warn: $label not found ($cmd)" >&2
    return 0
}

doctor_check_files() {
    local ok=1

    if [ -f "$TODO_FILE" ]; then
        if jq -e . "$TODO_FILE" >/dev/null 2>&1; then
            echo "ok: $TODO_FILE is valid JSON"
        else
            echo "missing/invalid: $TODO_FILE is not valid JSON" >&2
            ok=0
        fi
    else
        echo "warn: $TODO_FILE not found (will bootstrap on run)" >&2
    fi

    if [ -f "$SCHEMA_FILE" ]; then
        echo "ok: $SCHEMA_FILE present"
    else
        echo "warn: $SCHEMA_FILE not found (will bootstrap on run)" >&2
    fi

    if [ -f "$TODO_FILE" ] && [ -f "$SCHEMA_FILE" ]; then
        if validate_todo; then
            echo "ok: $TODO_FILE matches schema checks"
        else
            echo "missing/invalid: $TODO_FILE failed schema checks" >&2
            ok=0
        fi
    fi

    return "$ok"
}

run_doctor() {
    local ok=1

    echo "Looper doctor"
    echo "Workdir: $WORKDIR"
    echo "Task file: $TODO_FILE"
    echo "Schema file: $SCHEMA_FILE"

    if ! doctor_check_cmd "$CODEX_BIN" "codex" 1; then
        ok=0
    fi
    if ! doctor_check_cmd jq "jq" 1; then
        ok=0
    fi

    if should_use_claude; then
        if ! doctor_check_cmd "$CLAUDE_BIN" "claude" 1; then
            ok=0
        fi
    else
        doctor_check_cmd "$CLAUDE_BIN" "claude" 0 || true
    fi

    doctor_check_cmd jsonschema "jsonschema (optional)" 0 || true

    if [ "$LOOPER_GIT_INIT" -eq 1 ]; then
        doctor_check_cmd git "git (optional)" 0 || true
    fi

    if ! doctor_check_files; then
        ok=0
    fi

    if [ "$ok" -eq 1 ]; then
        echo "Doctor: ok"
        return 0
    fi

    echo "Doctor: issues found" >&2
    return 1
}

set_todo_file() {
    local todo_file="${1:-to-do.json}"
    TODO_FILE="$todo_file"
    SCHEMA_FILE="${TODO_FILE%.json}.schema.json"
}

lowercase() {
    printf "%s" "$1" | tr '[:upper:]' '[:lower:]'
}

normalize_agent() {
    local agent
    agent=$(lowercase "$1")
    case "$agent" in
        claude)
            echo "claude"
            ;;
        codex)
            echo "codex"
            ;;
        *)
            echo "$agent"
            ;;
    esac
}

normalize_agent_list() {
    local list="$1"
    local result=""
    local IFS=','
    local items=()
    read -r -a items <<< "$list"
    for item in "${items[@]}"; do
        local trimmed
        trimmed=$(printf "%s" "$item" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
        trimmed=$(normalize_agent "$trimmed")
        if [ -n "$trimmed" ]; then
            if [ -n "$result" ]; then
                result+=",${trimmed}"
            else
                result="$trimmed"
            fi
        fi
    done
    printf "%s" "$result"
}

validate_agent() {
    case "$1" in
        codex|claude)
            return 0
            ;;
        *)
            echo "Error: invalid agent '$1' (codex|claude)." >&2
            exit 1
            ;;
    esac
}

normalize_schedule() {
    local schedule
    schedule=$(lowercase "$1")
    case "$schedule" in
        odd_even|odd-even|oddeven)
            echo "odd-even"
            ;;
        round_robin|round-robin|roundrobin|rr)
            echo "round-robin"
            ;;
        codex|claude|odd-even|round-robin)
            echo "$schedule"
            ;;
        *)
            echo "$schedule"
            ;;
    esac
}

validate_iter_schedule() {
    case "$LOOPER_ITER_SCHEDULE" in
        codex|claude)
            ;;
        odd-even)
            validate_agent "$LOOPER_ITER_ODD_AGENT"
            validate_agent "$LOOPER_ITER_EVEN_AGENT"
            ;;
        round-robin)
            local agents
            agents=$(normalize_agent_list "$LOOPER_ITER_RR_AGENTS")
            if [ -z "$agents" ]; then
                echo "Error: LOOPER_ITER_RR_AGENTS cannot be empty for round-robin schedule." >&2
                exit 1
            fi
            local IFS=','
            local items=()
            read -r -a items <<< "$agents"
            local item
            for item in "${items[@]}"; do
                validate_agent "$item"
            done
            LOOPER_ITER_RR_AGENTS="$agents"
            ;;
        *)
            echo "Error: invalid iter schedule '$LOOPER_ITER_SCHEDULE' (codex|claude|odd-even|round-robin)." >&2
            exit 1
            ;;
    esac
}

select_iter_agent() {
    local iteration="${1:-1}"
    case "$LOOPER_ITER_SCHEDULE" in
        codex|claude)
            echo "$LOOPER_ITER_SCHEDULE"
            ;;
        odd-even)
            if [ $((iteration % 2)) -eq 1 ]; then
                echo "$LOOPER_ITER_ODD_AGENT"
            else
                echo "$LOOPER_ITER_EVEN_AGENT"
            fi
            ;;
        round-robin)
            local IFS=','
            local agents=()
            read -r -a agents <<< "$LOOPER_ITER_RR_AGENTS"
            local count=${#agents[@]}
            if [ "$count" -le 0 ]; then
                echo "codex"
                return 0
            fi
            local idx=$(( (iteration - 1) % count ))
            echo "${agents[$idx]}"
            ;;
        *)
            echo "codex"
            ;;
    esac
}

schedule_uses_claude() {
    case "$LOOPER_ITER_SCHEDULE" in
        claude)
            return 0
            ;;
        odd-even)
            if [ "$LOOPER_ITER_ODD_AGENT" = "claude" ] || [ "$LOOPER_ITER_EVEN_AGENT" = "claude" ]; then
                return 0
            fi
            ;;
        round-robin)
            case ",$LOOPER_ITER_RR_AGENTS," in
                *,claude,*)
                    return 0
                    ;;
            esac
            ;;
    esac
    return 1
}

should_use_claude() {
    if [ "$LOOPER_REPAIR_AGENT" = "claude" ]; then
        return 0
    fi
    schedule_uses_claude
}

parse_args() {
    local positional=()
    while [ $# -gt 0 ]; do
        case "$1" in
            --interleave)
                LOOPER_INTERLEAVE=1
                shift
                ;;
            --iter-schedule)
                if [ -z "${2:-}" ]; then
                    echo "Error: --iter-schedule requires a value." >&2
                    usage
                    exit 1
                fi
                LOOPER_ITER_SCHEDULE="$2"
                ITER_SCHEDULE_SET=1
                shift 2
                ;;
            --odd-agent)
                if [ -z "${2:-}" ]; then
                    echo "Error: --odd-agent requires a value." >&2
                    usage
                    exit 1
                fi
                LOOPER_ITER_ODD_AGENT="$2"
                shift 2
                ;;
            --even-agent)
                if [ -z "${2:-}" ]; then
                    echo "Error: --even-agent requires a value." >&2
                    usage
                    exit 1
                fi
                LOOPER_ITER_EVEN_AGENT="$2"
                shift 2
                ;;
            --rr-agents)
                if [ -z "${2:-}" ]; then
                    echo "Error: --rr-agents requires a value." >&2
                    usage
                    exit 1
                fi
                LOOPER_ITER_RR_AGENTS="$2"
                shift 2
                ;;
            --repair-agent)
                if [ -z "${2:-}" ]; then
                    echo "Error: --repair-agent requires a value." >&2
                    usage
                    exit 1
                fi
                LOOPER_REPAIR_AGENT="$2"
                REPAIR_AGENT_SET=1
                shift 2
                ;;
            --claude-bin)
                if [ -z "${2:-}" ]; then
                    echo "Error: --claude-bin requires a value." >&2
                    usage
                    exit 1
                fi
                CLAUDE_BIN="$2"
                shift 2
                ;;
            --claude-model)
                if [ -z "${2:-}" ]; then
                    echo "Error: --claude-model requires a value." >&2
                    usage
                    exit 1
                fi
                CLAUDE_MODEL="$2"
                shift 2
                ;;
            --)
                shift
                break
                ;;
            -*)
                echo "Error: unknown option '$1'." >&2
                usage
                exit 1
                ;;
            *)
                positional+=("$1")
                shift
                ;;
        esac
    done

    for arg in "$@"; do
        positional+=("$arg")
    done

    if [ ${#positional[@]} -gt 0 ]; then
        set_todo_file "${positional[0]}"
    fi
}

apply_interleave_defaults() {
    if [ "$LOOPER_INTERLEAVE" = "1" ]; then
        if [ "$ITER_SCHEDULE_SET" -eq 0 ]; then
            LOOPER_ITER_SCHEDULE="claude"
        fi
        if [ "$REPAIR_AGENT_SET" -eq 0 ]; then
            LOOPER_REPAIR_AGENT="claude"
        fi
    fi
}

shorten() {
    local input="$1"
    local max="${2:-120}"
    if [ ${#input} -gt "$max" ]; then
        printf "%s..." "${input:0:max}"
    else
        printf "%s" "$input"
    fi
}

get_project_root() {
    if command -v git >/dev/null 2>&1; then
        local root
        root=$(git -C "$WORKDIR" rev-parse --show-toplevel 2>/dev/null) || true
        if [ -n "$root" ]; then
            echo "$root"
            return 0
        fi
    fi

    echo "$WORKDIR"
}

slugify() {
    local input="$1"
    if [ -z "$input" ]; then
        echo "project"
        return 0
    fi

    echo "$input" | tr -cs 'A-Za-z0-9._-' '_' | sed 's/^_//;s/_$//'
}

hash_path() {
    local input="$1"

    if command -v sha1sum >/dev/null 2>&1; then
        printf "%s" "$input" | sha1sum | awk '{print substr($1,1,8)}'
        return 0
    fi

    if command -v shasum >/dev/null 2>&1; then
        printf "%s" "$input" | shasum | awk '{print substr($1,1,8)}'
        return 0
    fi

    if command -v md5sum >/dev/null 2>&1; then
        printf "%s" "$input" | md5sum | awk '{print substr($1,1,8)}'
        return 0
    fi

    printf "%s" "$input" | cksum | awk '{print $1}'
}

resolve_log_dir() {
    local root
    root=$(get_project_root)
    local name
    name=$(basename "$root")
    local slug
    slug=$(slugify "$name")
    local hash
    hash=$(hash_path "$root")

    LOOPER_LOG_DIR="${LOOPER_BASE_DIR}/${slug}-${hash}"
    SUMMARY_SCHEMA_FILE="${LOOPER_LOG_DIR}/summary.schema.json"
}

find_todo_root() {
    local dir="$WORKDIR"
    while [ -n "$dir" ]; do
        if [ -f "$dir/to-do.json" ]; then
            echo "$dir"
            return 0
        fi
        local parent
        parent=$(dirname "$dir")
        if [ "$parent" = "$dir" ]; then
            break
        fi
        dir="$parent"
    done
    return 1
}

set_log_dir_for_root() {
    local root="$1"
    local name
    name=$(basename "$root")
    local slug
    slug=$(slugify "$name")
    local hash
    hash=$(hash_path "$root")

    LOOPER_LOG_DIR="${LOOPER_BASE_DIR}/${slug}-${hash}"
    SUMMARY_SCHEMA_FILE="${LOOPER_LOG_DIR}/summary.schema.json"
}

init_run_log() {
    if [ "$CODEX_JSON_LOG" -ne 1 ]; then
        return 0
    fi

    ensure_log_dir
    if [ -n "$RUN_ID" ] && [ -n "$LOG_FILE" ]; then
        return 0
    fi

    local ts
    ts=$(date +%Y%m%d-%H%M%S)
    RUN_ID="${ts}-$$"
    LOG_FILE="$LOOPER_LOG_DIR/${RUN_ID}.jsonl"
    : > "$LOG_FILE"
}

ensure_log_dir() {
    if [ "$CODEX_JSON_LOG" -eq 1 ]; then
        if [ -z "$LOOPER_LOG_DIR" ]; then
            resolve_log_dir
        fi
        mkdir -p "$LOOPER_LOG_DIR"
    fi
}

write_summary_schema_if_missing() {
    if [ "$CODEX_JSON_LOG" -ne 1 ]; then
        return 0
    fi

    ensure_log_dir
    if [ -f "$SUMMARY_SCHEMA_FILE" ]; then
        return 0
    fi

    cat > "$SUMMARY_SCHEMA_FILE" <<'EOF'
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "Codex RALF Iteration Summary",
  "type": "object",
  "additionalProperties": false,
  "required": ["task_id", "status"],
  "properties": {
    "task_id": { "type": ["string", "null"] },
    "status": { "type": "string", "enum": ["done", "blocked", "skipped"] },
    "summary": { "type": "string" },
    "files": { "type": "array", "items": { "type": "string" } },
    "blockers": { "type": "array", "items": { "type": "string" } }
  }
}
EOF
}

should_capture_last_message() {
    if [ "$CODEX_JSON_LOG" -eq 1 ]; then
        return 0
    fi
    if [ "$LOOPER_APPLY_SUMMARY" -eq 1 ]; then
        return 0
    fi
    if [ -n "$LOOPER_HOOK" ]; then
        return 0
    fi
    return 1
}

prepare_run_files() {
    local label="${1:-run}"
    local capture_last="${2:-0}"

    if [ "$CODEX_JSON_LOG" -ne 1 ]; then
        RUN_ID=""
        if [ "$capture_last" -eq 1 ]; then
            LAST_MESSAGE_FILE=$(mktemp)
            LAST_MESSAGE_TEMP=1
        else
            LAST_MESSAGE_FILE=""
            LAST_MESSAGE_TEMP=0
        fi
        return 0
    fi

    LAST_MESSAGE_TEMP=0
    init_run_log
    local safe_label="${label//[^a-zA-Z0-9_-]/_}"
    LAST_MESSAGE_FILE="$LOOPER_LOG_DIR/${RUN_ID}-${safe_label}.last.json"
}

cleanup_last_message_file() {
    if [ "$LAST_MESSAGE_TEMP" -eq 1 ] && [ -n "$LAST_MESSAGE_FILE" ]; then
        rm -f "$LAST_MESSAGE_FILE"
    fi
    LAST_MESSAGE_TEMP=0
}

progress_line() {
    local line="$1"
    [ -z "$line" ] && return 0

    local msg_type
    msg_type=$(echo "$line" | jq -r '.type // .event // empty' 2>/dev/null)
    [ -z "$msg_type" ] && return 0

    case "$msg_type" in
        assistant|assistant_message|message|assistant_response)
            local content
            content=$(echo "$line" | jq -r '.message.content[0].text // .content[0].text // .content // .text // .output_text // empty' 2>/dev/null)
            if [ -n "$content" ]; then
                echo "AI: $(shorten "$content" 120)"
            fi
            ;;
        tool_use|tool|tool_call|tool_request)
            local tool_name
            tool_name=$(echo "$line" | jq -r '.tool_name // .name // empty' 2>/dev/null)
            if [ -n "$tool_name" ]; then
                echo "Tool: $tool_name"
            fi
            ;;
        tool_result)
            local is_error
            is_error=$(echo "$line" | jq -r '.is_error // false' 2>/dev/null)
            if [ "$is_error" = "true" ]; then
                echo "Tool: error"
            else
                echo "Tool: ok"
            fi
            ;;
        result|final|done)
            echo "Result: done"
            ;;
    esac
}

stream_progress() {
    while IFS= read -r line; do
        progress_line "$line"
    done
}

stream_discard() {
    cat >/dev/null
}

annotate_line() {
    local line="$1"
    local label="$2"
    local iteration="$3"

    local annotated
    if annotated=$(echo "$line" | jq -c --arg label "$label" --arg run_id "$RUN_ID" --argjson iter "$iteration" '. + {looper_run_id:$run_id, looper_label:$label, looper_iteration:$iter}' 2>/dev/null); then
        echo "$annotated"
        return 0
    fi

    jq -c -n --arg label "$label" --arg run_id "$RUN_ID" --argjson iter "$iteration" --arg raw "$line" \
        '{type:"looper.raw", looper_run_id:$run_id, looper_label:$label, looper_iteration:$iter, raw:$raw}'
}

stream_with_annotation() {
    local label="$1"
    local iteration="$2"
    local progress="${3:-$CODEX_PROGRESS}"

    while IFS= read -r line; do
        [ -z "$line" ] && continue
        local annotated
        annotated=$(annotate_line "$line" "$label" "$iteration")
        # Ensure log directory exists before writing (defensive check)
        mkdir -p "$(dirname "$LOG_FILE")"
        echo "$annotated" >> "$LOG_FILE"
        if [ "$progress" -eq 1 ]; then
            progress_line "$annotated"
        fi
    done
}

write_schema_if_missing() {
    if [ -f "$SCHEMA_FILE" ]; then
        ensure_schema_has_source_files
        return 0
    fi

    cat > "$SCHEMA_FILE" <<'EOF'
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "Codex RALF Todo",
  "type": "object",
  "additionalProperties": false,
  "required": ["schema_version", "source_files", "tasks"],
  "properties": {
    "schema_version": { "type": "integer", "const": 1 },
    "project": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "name": { "type": "string" },
        "root": { "type": "string" }
      }
    },
    "source_files": {
      "type": "array",
      "items": { "type": "string" }
    },
    "tasks": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["id", "title", "priority", "status"],
        "properties": {
          "id": { "type": "string" },
          "title": { "type": "string", "minLength": 1 },
          "priority": { "type": "integer", "minimum": 1, "maximum": 5 },
          "status": { "type": "string", "enum": ["todo", "doing", "blocked", "done"] },
          "details": { "type": "string" },
          "steps": { "type": "array", "items": { "type": "string" } },
          "blockers": { "type": "array", "items": { "type": "string" } },
          "tags": { "type": "array", "items": { "type": "string" } },
          "files": { "type": "array", "items": { "type": "string" } },
          "depends_on": { "type": "array", "items": { "type": "string" } },
          "created_at": { "type": "string", "format": "date-time" },
          "updated_at": { "type": "string", "format": "date-time" }
        }
      }
    }
  }
}
EOF

    ensure_schema_has_source_files
}

ensure_schema_has_source_files() {
    if [ ! -f "$SCHEMA_FILE" ]; then
        return 0
    fi

    if jq -e '.properties.source_files and (.required | index("source_files"))' "$SCHEMA_FILE" >/dev/null 2>&1; then
        return 0
    fi

    local tmp
    tmp=$(mktemp)
    jq '
        .properties = (.properties // {})
        | .properties.source_files = (.properties.source_files // {"type":"array","items":{"type":"string"}})
        | .required = ((.required // []) + ["source_files"] | unique)
    ' "$SCHEMA_FILE" > "$tmp" && mv "$tmp" "$SCHEMA_FILE"
}

validate_todo() {
    if [ ! -f "$SCHEMA_FILE" ]; then
        return 1
    fi

    if command -v jsonschema >/dev/null 2>&1; then
        jsonschema -i "$TODO_FILE" "$SCHEMA_FILE" >/dev/null 2>&1
        return $?
    fi

    jq -e '
        .schema_version == 1
        and (.source_files | type == "array")
        and (.tasks | type == "array")
        and (
            [.tasks[] | select(
                (type == "object")
                and (.id | type == "string" and length > 0)
                and (.title | type == "string" and length > 0)
                and (.priority | type == "number" and . >= 1 and . <= 5)
                and (.status | type == "string" and (["todo","doing","blocked","done"] | index(.)))
            )] | length == (.tasks | length)
        )
    ' "$TODO_FILE" >/dev/null 2>&1
}

has_open_tasks() {
    jq -e '.tasks[]? | select(.status != "done")' "$TODO_FILE" >/dev/null 2>&1
}

last_task_is_project_done() {
    jq -e '
        (.tasks | length) > 0
        and (.tasks[-1].status == "done")
        and ((.tasks[-1].tags // []) | index("project-done"))
    ' "$TODO_FILE" >/dev/null 2>&1
}

list_tasks_by_status() {
    local status="$1"
    jq --arg status "$status" '.tasks[] | select(.status == $status)' "$TODO_FILE"
}

set_task_status() {
    local task_id="$1"
    local status="$2"

    if [ -z "$task_id" ] || [ -z "$status" ]; then
        return 1
    fi

    local now tmp
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

recover_task_states() {
    local doing_tasks doing_count tmp now
    doing_tasks=$(jq -r '.tasks[] | select(.status == "doing") | .id' "$TODO_FILE" 2>/dev/null)
    doing_count=$(echo "$doing_tasks" | grep -c . 2>/dev/null || echo "0")

    if [ "$doing_count" -gt 0 ]; then
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

        echo "Recovered $doing_count tasks from doing to todo state"
        if [ "$doing_count" -le 5 ]; then
            echo "$doing_tasks" | while read -r task_id; do
                [ -n "$task_id" ] && echo "  - $task_id"
            done
        fi
    fi

    return "$doing_count"
}

current_task_line() {
    jq -r '
        def first_or_null($arr):
            if ($arr | length) > 0 then $arr[0] else null end;
        def id_sort_key($id):
            if ($id | test("^[A-Za-z_-]*[0-9]+$")) then
                ($id | capture("^(?<prefix>[A-Za-z_-]*)(?<num>[0-9]+)$")
                | [.prefix, (.num | tonumber), $id])
            else
                [$id, null, $id]
            end;
        .tasks as $t
        | ([$t[] | select(.status == "doing")] | sort_by(id_sort_key(.id)) | first_or_null(.)) as $doing
        | if $doing then $doing
          else
            ([$t[] | select(.status == "todo")] | sort_by(.priority, id_sort_key(.id)) | first_or_null(.)) as $todo
            | if $todo then $todo
              else
                ([$t[] | select(.status == "blocked")] | sort_by(.priority, id_sort_key(.id)) | first_or_null(.)) as $blocked
                | if $blocked then $blocked else empty end
              end
          end
        | "\(.id)\t\(.status)\t\(.title)"
    ' "$TODO_FILE" 2>/dev/null
}

print_iteration_task() {
    local line
    line=$(current_task_line)
    if [ -n "$line" ]; then
        local task_id status title
        IFS=$'\t' read -r task_id status title <<< "$line"
        echo "Task: $task_id ($status) - $title"
    else
        echo "Task: none"
    fi
}

current_task_id() {
    local line
    line=$(current_task_line)
    if [ -n "$line" ]; then
        local task_id status title
        IFS=$'\t' read -r task_id status title <<< "$line"
        echo "$task_id"
    fi
}

current_task_status() {
    local line
    line=$(current_task_line)
    if [ -n "$line" ]; then
        local task_id status title
        IFS=$'\t' read -r task_id status title <<< "$line"
        echo "$status"
    fi
}

task_status_by_id() {
    local task_id="$1"

    if [ -z "$task_id" ]; then
        return 1
    fi

    jq -r --arg id "$task_id" '.tasks[] | select(.id == $id) | .status' "$TODO_FILE" 2>/dev/null | head -n 1
}

latest_log_file() {
    local root
    root=$(find_todo_root)
    if [ -n "$root" ]; then
        set_log_dir_for_root "$root"
    else
        resolve_log_dir
    fi
    if [ ! -d "$LOOPER_LOG_DIR" ]; then
        echo ""
        return 1
    fi
    ls -1t "$LOOPER_LOG_DIR"/*.jsonl 2>/dev/null | head -n 1
}

extract_last_agent_message() {
    local log_file="$1"
    if [ -z "$log_file" ] || [ ! -f "$log_file" ]; then
        return 1
    fi

    jq -r '
        def clean($s):
            ($s // "")
            | gsub("[\r\n\t]+"; " ")
            | sub("^ +"; "")
            | sub(" +$"; "");
        def join_text($arr):
            if ($arr | type) == "array" then
              ($arr | map(select(.type == "text") | .text) | join(""))
            else
              ""
            end;
        def clean_cmd($s):
            clean($s)
            | sub("^/bin/bash -lc "; "")
            | sub("^\""; "")
            | sub("\"$"; "")
            | sub("^'\''"; "")
            | sub("'\''$"; "");
        if (.type == "item.completed" and .item.type == "agent_message") then
            "agent_message\t" + ((.looper_iteration // -1) | tostring) + "\t" + clean(.item.text)
        elif (.type == "assistant_message" or .type == "assistant_response" or .type == "assistant") then
            "assistant_message\t" + ((.looper_iteration // -1) | tostring) + "\t" + clean(.message.content[0].text // .content[0].text // .text // .output_text)
        elif (.type == "message" and .message and .message.content) then
            "assistant_message\t" + ((.looper_iteration // -1) | tostring) + "\t" + clean(join_text(.message.content))
        elif (.type == "content_block_delta" and (.delta.text // "") != "") then
            "assistant_message\t" + ((.looper_iteration // -1) | tostring) + "\t" + clean(.delta.text)
        elif (.type == "content_block_start" and .content_block and (.content_block.text // "") != "") then
            "assistant_message\t" + ((.looper_iteration // -1) | tostring) + "\t" + clean(.content_block.text)
        elif (.type == "item.completed" and .item.type == "reasoning") then
            "reasoning\t" + ((.looper_iteration // -1) | tostring) + "\t" + clean(.item.text)
        elif (.type == "item.started" and .item.type == "command_execution") then
            "command_started\t" + ((.looper_iteration // -1) | tostring) + "\t" + clean_cmd(.item.command)
        elif (.type == "item.completed" and .item.type == "command_execution") then
            "command_completed\t" + ((.looper_iteration // -1) | tostring) + "\t" + clean_cmd(.item.command)
        else
            empty
        end
    ' "$log_file" | tail -n 1
}

tail_prefix() {
    local iteration="$1"
    local task_id="${2:-}"
    local task_status="${3:-}"

    if [ -z "$iteration" ] || [ "$iteration" = "-1" ] || [ "$iteration" = "null" ]; then
        iteration="?"
    fi

    if [ -z "$task_id" ]; then
        task_id=$(current_task_id)
    fi
    if [ -z "$task_status" ]; then
        task_status=$(current_task_status)
    fi

    local prefix="Iter $iteration"
    if [ -n "$task_id" ]; then
        if [ -n "$task_status" ]; then
            prefix="$prefix | Task $task_id ($task_status)"
        else
            prefix="$prefix | Task $task_id"
        fi
    fi

    echo "$prefix"
}

format_tail_message() {
    local tagged="$1"

    local label rest iteration message
    label=${tagged%%$'\t'*}
    rest=${tagged#*$'\t'}
    iteration=${rest%%$'\t'*}
    message=${rest#*$'\t'}

    local task_id=""
    local task_status=""
    if [ "$label" = "agent_message" ] || [ "$label" = "assistant_message" ]; then
        if echo "$message" | jq -e . >/dev/null 2>&1; then
            task_id=$(echo "$message" | jq -r '.task_id // empty' 2>/dev/null)
            task_status=$(echo "$message" | jq -r '.status // empty' 2>/dev/null)
        fi
    fi

    local prefix
    prefix=$(tail_prefix "$iteration" "$task_id" "$task_status")

    case "$label" in
        agent_message|assistant_message)
            message=$(shorten "$message" 240)
            echo "$prefix: $message"
            ;;
        reasoning)
            message=$(shorten "$message" 200)
            echo "$prefix | Reasoning: $message"
            ;;
        command_started)
            message=$(shorten "$message" 160)
            echo "$prefix | Command (start): $message"
            ;;
        command_completed)
            message=$(shorten "$message" 160)
            echo "$prefix | Command (done): $message"
            ;;
        *)
            message=$(shorten "$message" 200)
            echo "$prefix | $message"
            ;;
    esac
}

print_last_agent_message() {
    local log_file="$1"
    if [ -z "$log_file" ] || [ ! -f "$log_file" ]; then
        echo "No log file found." >&2
        return 1
    fi

    local tagged
    tagged=$(extract_last_agent_message "$log_file")

    if [ -z "$tagged" ]; then
        echo "No agent activity found in $log_file." >&2
        return 1
    fi

    format_tail_message "$tagged"
}

follow_last_agent_message() {
    local last_message=""
    local last_file=""

    while true; do
        local log_file
        log_file=$(latest_log_file)
        if [ -n "$log_file" ] && [ -f "$log_file" ]; then
            local message
            message=$(extract_last_agent_message "$log_file")
            if [ -n "$message" ] && { [ "$message" != "$last_message" ] || [ "$log_file" != "$last_file" ]; }; then
                format_tail_message "$message"
                last_message="$message"
                last_file="$log_file"
            fi
        fi
        sleep 1
    done
}

print_run_info() {
    local mode="default"
    if [ "$CODEX_YOLO" -eq 1 ]; then
        mode="yolo"
    elif [ "$CODEX_FULL_AUTO" -eq 1 ]; then
        mode="full-auto"
    fi

    local flags_string
    flags_string=$(printf "%s " "${CODEX_FLAGS[@]}")
    flags_string=${flags_string% }

    echo "Iteration schedule: $LOOPER_ITER_SCHEDULE"
    case "$LOOPER_ITER_SCHEDULE" in
        odd-even)
            echo "Iter agents: odd=$LOOPER_ITER_ODD_AGENT even=$LOOPER_ITER_EVEN_AGENT"
            ;;
        round-robin)
            echo "Iter agents: $LOOPER_ITER_RR_AGENTS"
            ;;
    esac
    echo "Review agent: codex"
    echo "Repair agent: $LOOPER_REPAIR_AGENT"
    echo "Codex model: $CODEX_MODEL (reasoning: $CODEX_REASONING_EFFORT)"
    if [ -n "$CODEX_PROFILE" ]; then
        echo "Codex mode: $mode | profile: $CODEX_PROFILE"
    else
        echo "Codex mode: $mode"
    fi
    echo "Codex flags: $CODEX_BIN $flags_string -"
    if should_use_claude; then
        local claude_model="${CLAUDE_MODEL:-default}"
        local claude_flags_string
        claude_flags_string=$(printf "%s " "${CLAUDE_FLAGS[@]}")
        claude_flags_string=${claude_flags_string% }
        echo "claude model: $claude_model"
        echo "claude flags: $CLAUDE_BIN -p <prompt> $claude_flags_string"
    fi
    echo "Schema file: $SCHEMA_FILE"
    if [ "$CODEX_JSON_LOG" -eq 1 ]; then
        echo "Log dir: $LOOPER_LOG_DIR"
        if [ -n "$LOG_FILE" ]; then
            echo "Log file: $LOG_FILE"
        fi
    else
        echo "Log dir: disabled"
    fi
    echo "Summary apply: $([ "$LOOPER_APPLY_SUMMARY" -eq 1 ] && echo on || echo off)"
    echo "Git init: $([ "$LOOPER_GIT_INIT" -eq 1 ] && echo on || echo off)"
    echo "Output schema: $([ "$CODEX_ENFORCE_OUTPUT_SCHEMA" -eq 1 ] && echo on || echo off)"
}

strip_json_fence() {
    local text="$1"
    local first_line last_line
    first_line=$(printf "%s" "$text" | sed -n '1p')
    last_line=$(printf "%s" "$text" | sed -n '$p')
    if printf "%s" "$first_line" | sed -n '/^```/p' >/dev/null 2>&1 && \
       printf "%s" "$last_line" | sed -n '/^```/p' >/dev/null 2>&1; then
        text=$(printf "%s" "$text" | sed '1d;$d')
    fi
    printf "%s" "$text"
}

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

    candidate=$(printf "%s" "$text" | sed -n '1h;1!H;${g;s/^[^{]*//;s/[^}]*$//;p}')
    if [ -n "$candidate" ] && printf "%s" "$candidate" | jq -e . >/dev/null 2>&1; then
        printf "%s" "$candidate"
        return 0
    fi

    return 1
}

extract_claude_output_json() {
    local output_file="$1"
    local json=""

    if json=$(jq -c . "$output_file" 2>/dev/null); then
        printf "%s" "$json"
        return 0
    fi

    local line
    while IFS= read -r line; do
        if printf "%s" "$line" | jq -e . >/dev/null 2>&1; then
            json="$line"
        fi
    done < "$output_file"

    if [ -n "$json" ]; then
        printf "%s" "$json"
        return 0
    fi

    return 1
}

extract_claude_text() {
    local json="$1"
    printf "%s" "$json" | jq -r '
        def join_text($arr):
          if ($arr | type) == "array" then
            ($arr | map(select(.type == "text") | .text) | join("\n"))
          else
            ""
          end;
        if .content then
          join_text(.content)
        elif .message and .message.content then
          join_text(.message.content)
        elif .completion then
          .completion
        elif .output_text then
          .output_text
        elif .text then
          .text
        elif .result then
          .result
        else
          ""
        end
    '
}

extract_claude_stream_text() {
    local output_file="$1"
    local text=""
    local line
    local saw_full=0

    while IFS= read -r line; do
        [ -z "$line" ] && continue
        local message_text
        message_text=$(printf "%s" "$line" | jq -r '
            def join_text($arr):
              if ($arr | type) == "array" then
                ($arr | map(select(.type == "text") | .text) | join(""))
              else
                ""
              end;
            if .type == "message" and .message and .message.content then
              join_text(.message.content)
            elif .message and .message.content then
              join_text(.message.content)
            else
              empty
            end
        ' 2>/dev/null)
        if [ -n "$message_text" ] && [ "$message_text" != "null" ]; then
            text="$message_text"
            saw_full=1
            continue
        fi

        if [ "$saw_full" -eq 1 ]; then
            continue
        fi

        local chunk
        chunk=$(printf "%s" "$line" | jq -r '
            if .type == "content_block_delta" and (.delta.text // "") != "" then
              .delta.text
            elif .type == "content_block_start" and .content_block and (.content_block.text // "") != "" then
              .content_block.text
            else
              empty
            end
        ' 2>/dev/null)
        if [ -n "$chunk" ] && [ "$chunk" != "null" ]; then
            text+="$chunk"
        fi
    done < "$output_file"

    printf "%s" "$text"
}

append_claude_message_log() {
    local label="$1"
    local iteration="$2"
    local text="$3"

    if [ "$CODEX_JSON_LOG" -ne 1 ] || [ -z "${LOG_FILE:-}" ] || [ -z "$label" ]; then
        return 0
    fi

    if [ -z "$text" ]; then
        return 0
    fi

    if [ -z "$iteration" ]; then
        iteration=0
    fi

    jq -c -n --arg text "$text" \
        --arg label "$label" \
        --arg run_id "$RUN_ID" \
        --argjson iter "$iteration" \
        '{type:"assistant_message", message:{content:[{type:"text", text:$text}]}, looper_run_id:$run_id, looper_label:$label, looper_iteration:$iter}' \
        >> "$LOG_FILE"
}

write_last_message_from_claude_output() {
    local output_file="$1"
    local label="${2:-}"
    local iteration="${3:-0}"

    if [ -z "$LAST_MESSAGE_FILE" ]; then
        return 0
    fi

    local output_json text normalized log_text raw_output summary_json
    text=$(extract_claude_stream_text "$output_file")
    if [ -n "$text" ]; then
        normalized=$(strip_json_fence "$text")
        if summary_json=$(extract_json_from_text "$normalized"); then
            printf "%s\n" "$summary_json" > "$LAST_MESSAGE_FILE"
        else
            jq -n --arg raw "$normalized" '{raw:$raw}' > "$LAST_MESSAGE_FILE"
        fi
        log_text="$normalized"
    elif output_json=$(extract_claude_output_json "$output_file"); then
        text=$(extract_claude_text "$output_json")
        if [ -n "$text" ]; then
            normalized=$(strip_json_fence "$text")
            if summary_json=$(extract_json_from_text "$normalized"); then
                printf "%s\n" "$summary_json" > "$LAST_MESSAGE_FILE"
            else
                jq -n --arg raw "$normalized" '{raw:$raw}' > "$LAST_MESSAGE_FILE"
            fi
            log_text="$normalized"
        else
            jq -n --arg raw "$output_json" '{raw:$raw}' > "$LAST_MESSAGE_FILE"
            log_text="$output_json"
        fi
    else
        raw_output=$(cat "$output_file")
        jq -n --arg raw "$raw_output" '{raw:$raw}' > "$LAST_MESSAGE_FILE"
        log_text="$raw_output"
    fi

    append_claude_message_log "$label" "$iteration" "$log_text"
}

run_claude() {
    local label="${1:-run}"
    local expect_summary="${2:-0}"
    local iteration="${3:-0}"
    local capture_last="${4:-$CAPTURE_LAST_MESSAGE}"
    local prompt
    prompt=$(cat)

    prepare_run_files "$label" "$capture_last"
    local cmd=("$CLAUDE_BIN" -p "$prompt" "${CLAUDE_FLAGS[@]}")
    local output_file
    output_file=$(mktemp)
    local exit_status

    if [ "$CODEX_JSON_LOG" -eq 1 ]; then
        "${cmd[@]}" 2>&1 | tee "$output_file" | stream_with_annotation "$label" "$iteration" 0
        exit_status=${PIPESTATUS[0]}
        write_last_message_from_claude_output "$output_file" "$label" "$iteration"
        rm -f "$output_file"
        return "$exit_status"
    fi

    "${cmd[@]}" >"$output_file" 2>&1
    exit_status=$?
    write_last_message_from_claude_output "$output_file" "$label" "$iteration"
    rm -f "$output_file"
    return "$exit_status"
}

run_with_agent() {
    local agent="$1"
    shift

    case "$agent" in
        claude)
            run_claude "$@"
            ;;
        codex|*)
            run_codex "$@"
            ;;
    esac
}

run_codex() {
    local label="${1:-run}"
    local expect_summary="${2:-0}"
    local iteration="${3:-0}"
    local capture_last="${4:-$CAPTURE_LAST_MESSAGE}"
    local cmd=("$CODEX_BIN" "${CODEX_FLAGS[@]}")

    prepare_run_files "$label" "$capture_last"

    if [ "$CODEX_JSON_LOG" -eq 1 ]; then
        cmd+=(--json --output-last-message "$LAST_MESSAGE_FILE")
        if [ "$expect_summary" -eq 1 ] && [ "$CODEX_ENFORCE_OUTPUT_SCHEMA" -eq 1 ]; then
            write_summary_schema_if_missing
            cmd+=(--output-schema "$SUMMARY_SCHEMA_FILE")
        fi
    elif [ "$capture_last" -eq 1 ]; then
        cmd+=(--json --output-last-message "$LAST_MESSAGE_FILE")
    fi

    cmd+=(-)

    if [ "$CODEX_JSON_LOG" -eq 1 ]; then
        "${cmd[@]}" 2>&1 | stream_with_annotation "$label" "$iteration"
        return ${PIPESTATUS[0]}
    fi

    if [ "$capture_last" -eq 1 ]; then
        if [ "$CODEX_PROGRESS" -eq 1 ]; then
            "${cmd[@]}" 2>&1 | stream_progress
        else
            "${cmd[@]}" >/dev/null 2>&1
        fi
        return ${PIPESTATUS[0]}
    fi

    "${cmd[@]}"
}

handle_last_message() {
    local label="${1:-run}"

    if [ -z "$LAST_MESSAGE_FILE" ] || [ ! -f "$LAST_MESSAGE_FILE" ]; then
        return 0
    fi

    if ! jq -e . "$LAST_MESSAGE_FILE" >/dev/null 2>&1; then
        echo "Warning: last message is not valid JSON: $LAST_MESSAGE_FILE"
        return 0
    fi

    local task_id status summary
    task_id=$(jq -r '.task_id // empty' "$LAST_MESSAGE_FILE")
    status=$(jq -r '.status // empty' "$LAST_MESSAGE_FILE")
    summary=$(jq -r '.summary // empty' "$LAST_MESSAGE_FILE")

    if [ -n "$task_id" ] && [ -n "$status" ]; then
        echo "Summary: $task_id -> $status"
    elif [ -n "$summary" ]; then
        echo "Summary: $(shorten "$summary" 120)"
    fi

    if [ -n "$LOOPER_HOOK" ]; then
        "$LOOPER_HOOK" "$task_id" "$status" "$LAST_MESSAGE_FILE" "$label" || true
    fi
}

summary_matches_selected() {
    local expected_id="$1"

    if [ -z "$expected_id" ]; then
        return 1
    fi
    if [ -z "$LAST_MESSAGE_FILE" ] || [ ! -f "$LAST_MESSAGE_FILE" ]; then
        return 1
    fi

    local summary_id summary_status
    summary_id=$(jq -r '.task_id // empty' "$LAST_MESSAGE_FILE" 2>/dev/null)
    summary_status=$(jq -r '.status // empty' "$LAST_MESSAGE_FILE" 2>/dev/null)

    if [ -z "$summary_id" ] || [ -z "$summary_status" ] || [ "$summary_status" = "skipped" ]; then
        return 1
    fi

    if [ "$summary_id" != "$expected_id" ]; then
        echo "Warning: summary task_id '$summary_id' does not match selected '$expected_id'." >&2
        return 1
    fi

    return 0
}

apply_summary_to_todo() {
    if [ "$LOOPER_APPLY_SUMMARY" -ne 1 ]; then
        return 0
    fi

    if [ -z "$LAST_MESSAGE_FILE" ] || [ ! -f "$LAST_MESSAGE_FILE" ]; then
        return 0
    fi

    local task_id status
    task_id=$(jq -r '.task_id // empty' "$LAST_MESSAGE_FILE")
    status=$(jq -r '.status // empty' "$LAST_MESSAGE_FILE")

    if [ -z "$task_id" ] || [ "$status" = "skipped" ] || [ -z "$status" ]; then
        return 0
    fi

    local now tmp
    now=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    tmp=$(mktemp)

    local files_json blockers_json
    files_json=$(jq -c '.files // []' "$LAST_MESSAGE_FILE")
    blockers_json=$(jq -c '.blockers // []' "$LAST_MESSAGE_FILE")

    jq --arg id "$task_id" \
       --arg status "$status" \
       --arg now "$now" \
       --argjson files "$files_json" \
       --argjson blockers "$blockers_json" \
       '.tasks |= map(
            if .id == $id then
              .status = $status
              | .updated_at = $now
              | (if ($files | length) > 0 then .files = ((.files // []) + $files | unique) else . end)
              | (if ($blockers | length) > 0 then .blockers = ((.blockers // []) + $blockers | unique) else . end)
            else
              .
            end
       )' "$TODO_FILE" > "$tmp" && mv "$tmp" "$TODO_FILE"
}

run_review_pass() {
    local iteration="${1:-0}"

    run_codex "review-$iteration" 0 "$iteration" "$CAPTURE_LAST_MESSAGE" <<EOF
You are running a final review pass after all tasks are complete.

Goal: review the codebase file-by-file as a senior developer reviewing a junior's work,
then update "$TODO_FILE" if new tasks are needed.

Rules:
- Read "$TODO_FILE" and follow the schema in "$SCHEMA_FILE".
- Read every file listed in source_files.
- Review the repo file-by-file, focusing on tracked, non-generated files.
  Skip .git, node_modules, dist, build, .venv, __pycache__, and other generated dirs.
- Do not modify code or other files. Only update "$TODO_FILE".
- If you find new tasks, append them to .tasks with status "todo" and priority (1 highest).
- Follow the existing id style and ensure ids are unique.
- Avoid duplicates by intent/title.
- If you add tasks, do NOT add the project-done marker.
- If no new tasks are needed, append a final task as the last item in .tasks:
  - id: a unique id (use "PROJECT-DONE" if available)
  - title: "Project done: no new tasks"
  - status: "done"
  - priority: 5
  - tags: ["project-done"]
  - details: brief note that the review found nothing to add
- Keep JSON formatted with 2-space indentation.
- Use jq for edits when practical.
- Do not ask for confirmation.
- If you conclude no new tasks are needed, you MUST add the project-done marker.
  Looper will keep running otherwise.

Return only a JSON object:
{"status":"reviewed","summary":"...","added_tasks":0,"files":["..."]}
EOF

    local exit_status=$?
    if [ "$exit_status" -ne 0 ]; then
        echo "Review failed with exit code $exit_status."
    fi

    handle_last_message "review-$iteration"
    cleanup_last_message_file
}

ensure_git_repo() {
    if ! command -v git >/dev/null 2>&1; then
        echo "Warning: git is not available. Commits may fail."
        return 1
    fi

    if git -C "$WORKDIR" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        return 0
    fi

    if [ "$LOOPER_GIT_INIT" -eq 1 ]; then
        if git -C "$WORKDIR" init >/dev/null 2>&1; then
            echo "Initialized git repository in $WORKDIR"
            return 0
        fi
        echo "Warning: failed to initialize git repository in $WORKDIR"
        return 1
    fi

    echo "Warning: not inside a git repository. Commits may fail."
    return 1
}

repair_todo_schema() {
    write_schema_if_missing

    local repair_agent="$LOOPER_REPAIR_AGENT"
    local repair_bin="$CODEX_BIN"
    if [ "$repair_agent" = "claude" ]; then
        repair_bin="$CLAUDE_BIN"
    fi

    echo "Repairing $TODO_FILE with $repair_bin..."
    run_with_agent "$repair_agent" "repair" 0 0 0 <<EOF
Fix "$TODO_FILE" to match the schema in "$SCHEMA_FILE".

Rules:
- Preserve existing tasks and their intent.
- Ensure source_files exists; if missing, add relevant source docs (PROJECT.md, PROJECT_SPEC.md, SPECS.md, SPECIFICATION.md, README.md, DESIGN.md, IDEA.md). Use relative paths and [] if none.
- Do not change code or other files.
- Use jq if helpful.
- Keep JSON formatted with 2-space indentation.
- Do not ask for confirmation.

Return a brief summary of what you changed.
EOF
}

ensure_valid_todo() {
    write_schema_if_missing
    if validate_todo; then
        return 0
    fi

    echo "Warning: $TODO_FILE does not match the expected schema structure. Attempting repair..."
    repair_todo_schema

    if ! validate_todo; then
        echo "Error: $TODO_FILE still does not match the expected schema." >&2
        exit 1
    fi
}

bootstrap_todo() {
    if [ -f "$TODO_FILE" ]; then
        return 0
    fi

    write_schema_if_missing

    echo "Bootstrapping $TODO_FILE with $CODEX_BIN..."
    run_codex "bootstrap" 0 0 0 <<EOF
Initialize a task backlog for this project.

Rules:
- Read the current directory to understand the project.
- Search for markdown source docs like PROJECT.md, PROJECT_SPEC.md, SPECS.md, SPECIFICATION.md, README.md, DESIGN.md, IDEA.md, etc.
- Create "$TODO_FILE" using the schema in "$SCHEMA_FILE".
- Populate source_files with the relative paths (from project root) of the source docs you found. If none, set source_files to [].
- Add as many actionable tasks that are needed to fully implement the project.
- Assign each task priority (1 is highest).
- Set all task statuses to "todo".
- Do not modify code or other files.
- Use jq if helpful.
- Do not ask for confirmation.

Return a brief summary of what you created.
EOF

    if [ ! -f "$TODO_FILE" ]; then
        echo "Error: $TODO_FILE was not created." >&2
        exit 1
    fi

    if ! jq -e . "$TODO_FILE" >/dev/null 2>&1; then
        echo "Error: $TODO_FILE is not valid JSON." >&2
        exit 1
    fi

    ensure_valid_todo
}

main() {
    if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
        usage
        exit 0
    fi

    if [ "${1:-}" = "--tail" ]; then
        require_cmd jq
        local log_file
        if [ "${2:-}" = "--follow" ] || [ "${2:-}" = "-f" ] || [ "${2:-}" = "--folow" ]; then
            follow_last_agent_message
            exit 0
        fi
        log_file=$(latest_log_file)
        print_last_agent_message "$log_file"
        exit $?
    fi

    if [ "${1:-}" = "--doctor" ] || [ "${1:-}" = "--check" ]; then
        shift
        parse_args "$@"
        apply_interleave_defaults

        LOOPER_ITER_SCHEDULE=$(normalize_schedule "$LOOPER_ITER_SCHEDULE")
        LOOPER_ITER_ODD_AGENT=$(normalize_agent "$LOOPER_ITER_ODD_AGENT")
        LOOPER_ITER_EVEN_AGENT=$(normalize_agent "$LOOPER_ITER_EVEN_AGENT")
        LOOPER_REPAIR_AGENT=$(normalize_agent "$LOOPER_REPAIR_AGENT")
        validate_agent "$LOOPER_REPAIR_AGENT"
        validate_iter_schedule

        run_doctor
        exit $?
    fi

    if [ "${1:-}" = "--ls" ]; then
        local status="${2:-}"
        if [ -z "$status" ]; then
            echo "Error: --ls requires a status (todo|doing|blocked|done)." >&2
            usage
            exit 1
        fi
        case "$status" in
            todo|doing|blocked|done) ;;
            *)
                echo "Error: invalid status '$status' (todo|doing|blocked|done)." >&2
                exit 1
                ;;
        esac
        TODO_FILE="${3:-to-do.json}"
        if [ ! -f "$TODO_FILE" ]; then
            echo "Error: $TODO_FILE not found." >&2
            exit 1
        fi
        list_tasks_by_status "$status"
        exit 0
    fi

    parse_args "$@"
    apply_interleave_defaults

    LOOPER_ITER_SCHEDULE=$(normalize_schedule "$LOOPER_ITER_SCHEDULE")
    LOOPER_ITER_ODD_AGENT=$(normalize_agent "$LOOPER_ITER_ODD_AGENT")
    LOOPER_ITER_EVEN_AGENT=$(normalize_agent "$LOOPER_ITER_EVEN_AGENT")
    LOOPER_REPAIR_AGENT=$(normalize_agent "$LOOPER_REPAIR_AGENT")
    validate_agent "$LOOPER_REPAIR_AGENT"
    validate_iter_schedule

    require_cmd "$CODEX_BIN"
    require_cmd jq
    if should_use_claude; then
        require_cmd "$CLAUDE_BIN"
    fi

    CODEX_FLAGS=(
        exec
        -m "$CODEX_MODEL"
        -c "model_reasoning_effort=$CODEX_REASONING_EFFORT"
        --cd "$WORKDIR"
    )

    if [ "$CODEX_YOLO" -eq 1 ]; then
        CODEX_FLAGS+=(--yolo)
    elif [ "$CODEX_FULL_AUTO" -eq 1 ]; then
        CODEX_FLAGS+=(--full-auto)
    fi

    if [ -n "$CODEX_PROFILE" ]; then
        CODEX_FLAGS+=(--profile "$CODEX_PROFILE")
    fi

    if ! ensure_git_repo; then
        CODEX_FLAGS+=(--skip-git-repo-check)
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

    if should_capture_last_message; then
        CAPTURE_LAST_MESSAGE=1
    else
        CAPTURE_LAST_MESSAGE=0
    fi

    ensure_log_dir
    init_run_log
    write_summary_schema_if_missing
    bootstrap_todo
    ensure_valid_todo

    echo "Starting Codex RALF loop"
    print_run_info
    echo "Project: $WORKDIR"
    echo "Task file: $TODO_FILE"
    echo "Max iterations: $MAX_ITERATIONS"

    iteration=0
    trap 'echo "Interrupted. Exiting."; exit 130' INT TERM

    while true; do
        iteration=$((iteration + 1))

        if [ "$iteration" -gt "$MAX_ITERATIONS" ]; then
            echo "Reached max iterations ($MAX_ITERATIONS)."
            recover_task_states

            if has_open_tasks; then
                echo "Open tasks remain. Running final review pass..."
                run_review_pass "$iteration"
                ensure_valid_todo

                local todo_count blocked_count done_count
                todo_count=$(jq '[.tasks[] | select(.status == "todo")] | length' "$TODO_FILE")
                blocked_count=$(jq '[.tasks[] | select(.status == "blocked")] | length' "$TODO_FILE")
                done_count=$(jq '[.tasks[] | select(.status == "done")] | length' "$TODO_FILE")

                echo "--- Final State Summary ---"
                echo "Tasks remaining: $((todo_count + blocked_count)) (todo: $todo_count, blocked: $blocked_count)"
                echo "Tasks completed: $done_count"
                echo "Use './bin/looper.sh run' to continue working on remaining tasks."
            else
                if last_task_is_project_done; then
                    echo "All tasks complete and project is marked done."
                else
                    echo "All tasks complete but project-done marker not found."
                fi
            fi
            break
        fi

        ensure_valid_todo

        if ! has_open_tasks; then
            if last_task_is_project_done; then
                echo "No open tasks remain and project is marked done. Exiting."
                break
            fi

            echo "No open tasks remain. Running final review..."
            run_review_pass "$iteration"
            ensure_valid_todo

            if ! has_open_tasks; then
                if last_task_is_project_done; then
                    echo "Final review complete; project marked done. Exiting."
                    break
                else
                    echo "No open tasks remain after review and no project-done marker was added."
                    echo "Re-running review until a project-done marker is appended or max iterations is reached."
                fi
                continue
            fi

            continue
        fi

        echo "Iteration $iteration/$MAX_ITERATIONS"

        local task_line task_id task_status task_title
        task_line=$(current_task_line)
        if [ -n "$task_line" ]; then
            IFS=$'\t' read -r task_id task_status task_title <<< "$task_line"
            echo "Task: $task_id ($task_status) - $task_title"
        else
            echo "Task: none"
            continue
        fi

        local selected_task_id selected_task_status selected_task_title
        selected_task_id="$task_id"
        selected_task_status="$task_status"
        selected_task_title="$task_title"

        local status_before
        local status_changed=0
        status_before="$selected_task_status"
        if [ "$selected_task_status" != "doing" ]; then
            set_task_status "$selected_task_id" "doing"
            selected_task_status="doing"
            status_changed=1
        fi

        local iter_agent
        iter_agent=$(select_iter_agent "$iteration")
        echo "Iteration agent: $iter_agent"

        run_with_agent "$iter_agent" "iter-$iteration" 1 "$iteration" "$CAPTURE_LAST_MESSAGE" <<EOF
You are running in a deterministic RALF loop with fresh context each run.
Just for fun we are naming you Ralf (in honour of Ralph Wiggum German cousin Ralf).

Goal: complete exactly one task from "$TODO_FILE" per iteration.
Selected task for this iteration:
- id: $selected_task_id
- title: $selected_task_title
- status: $selected_task_status
You must work on this exact task id. Do not switch tasks.
If the task was not already "doing", it has been set to "doing" for you.

Rules:
- Read "$TODO_FILE" and follow the schema in "$SCHEMA_FILE".
- Read every file listed in source_files and treat them as ground truth for task selection and implementation.
- Implement the task fully and keep scope tight.
- If blocked, set status to "blocked" and add clear blocker notes. Do not commit partial work.
- If completed, set status to "done", update updated_at, and record relevant files in files[] if helpful.
- Use jq for task file edits when practical.
- Commit completed work with Conventional Commits (type(scope): summary). One commit per task.
- Do not amend or rewrite history.
- If no code changes are needed, skip commit and note the reason in the task details.
- Do not ask for confirmation.

Return only a JSON object:
{"task_id":"T123","status":"done","summary":"...","files":["..."],"blockers":[]}
If no task was executed, use status "skipped" and task_id null.
EOF

        exit_status=$?
        if [ "$exit_status" -ne 0 ]; then
            echo "Iteration failed with exit code $exit_status."
        fi

        handle_last_message "iter-$iteration"
        local summary_ok=1
        if ! summary_matches_selected "$selected_task_id"; then
            summary_ok=0
            # Best-effort: check if the returned task_id exists and apply to it
            local summary_task_id
            summary_task_id=$(jq -r '.task_id // empty' "$LAST_MESSAGE_FILE" 2>/dev/null)
            if [ -n "$summary_task_id" ] && [ "$summary_task_id" != "null" ]; then
                local task_exists
                task_exists=$(jq -r --arg id "$summary_task_id" '.tasks[] | select(.id == $id) | .id' "$TODO_FILE" 2>/dev/null | head -n 1)
                if [ -n "$task_exists" ]; then
                    echo "Warning: summary task_id '$summary_task_id' does not match selected '$selected_task_id'. Applying summary to returned task_id '$summary_task_id'." >&2
                    apply_summary_to_todo
                    summary_ok=2  # Mark as applied to different task
                else
                    # Task doesn't exist, revert status
                    if [ "$status_changed" -eq 1 ]; then
                        local current_status
                        current_status=$(task_status_by_id "$selected_task_id")
                        if [ "$current_status" = "doing" ] || [ "$current_status" = "done" ]; then
                            set_task_status "$selected_task_id" "$status_before"
                            echo "Warning: reverted task '$selected_task_id' from $current_status to $status_before due to task_id mismatch." >&2
                        fi
                    fi
                    echo "Warning: skipping summary apply due to task_id mismatch and invalid task_id '$summary_task_id'." >&2
                fi
            else
                # No valid task_id in summary, revert status
                if [ "$status_changed" -eq 1 ]; then
                    local current_status
                    current_status=$(task_status_by_id "$selected_task_id")
                    if [ "$current_status" = "doing" ] || [ "$current_status" = "done" ]; then
                        set_task_status "$selected_task_id" "$status_before"
                        echo "Warning: reverted task '$selected_task_id' from $current_status to $status_before due to task_id mismatch." >&2
                    fi
                fi
                echo "Warning: skipping summary apply due to task_id mismatch." >&2
            fi
        fi
        if [ "$summary_ok" -eq 1 ]; then
            apply_summary_to_todo
        fi
        cleanup_last_message_file
        ensure_valid_todo

        if [ "$LOOP_DELAY_SECONDS" -gt 0 ]; then
            sleep "$LOOP_DELAY_SECONDS"
        fi
    done
}

main "$@"
