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
  looper.sh [to-do.json]
  looper.sh --ls <status> [to-do.json]
  looper.sh --tail [--follow]

Core behavior:
  - Creates to-do.schema.json if missing.
  - Creates to-do.json (via Codex) if missing.
  - Validates to-do.json (jsonschema if available; jq fallback).
  - Repairs to-do.json via Codex if schema validation fails.
  - Runs Codex exec in a loop, one task per iteration.
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

set -u
set -o pipefail

MAX_ITERATIONS=${MAX_ITERATIONS:-50}
TODO_FILE=${1:-to-do.json}
SCHEMA_FILE="${TODO_FILE%.json}.schema.json"

CODEX_BIN=${CODEX_BIN:-codex}
CODEX_MODEL=${CODEX_MODEL:-gpt-5.2-codex}
CODEX_REASONING_EFFORT=${CODEX_REASONING_EFFORT:-xhigh}
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
LOOPER_APPLY_SUMMARY=${LOOPER_APPLY_SUMMARY:-1}
LOOPER_GIT_INIT=${LOOPER_GIT_INIT:-1}
LOOPER_HOOK=${LOOPER_HOOK:-}
RUN_ID=""
LOG_FILE=""
LAST_MESSAGE_FILE=""

usage() {
    echo "Usage: looper.sh [to-do.json]"
    echo "       looper.sh --ls <status> [to-do.json]"
    echo "       looper.sh --tail [--follow|-f]"
    echo "Env: MAX_ITERATIONS, CODEX_MODEL, CODEX_REASONING_EFFORT, CODEX_JSON_LOG, CODEX_PROGRESS"
    echo "Env: LOOPER_BASE_DIR, CODEX_PROFILE, CODEX_ENFORCE_OUTPUT_SCHEMA, CODEX_YOLO, CODEX_FULL_AUTO"
    echo "Env: LOOPER_APPLY_SUMMARY, LOOPER_GIT_INIT, LOOPER_HOOK, LOOP_DELAY_SECONDS"
}

require_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "Error: required command not found: $1" >&2
        exit 1
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

prepare_run_files() {
    local label="${1:-run}"

    if [ "$CODEX_JSON_LOG" -ne 1 ]; then
        RUN_ID=""
        LAST_MESSAGE_FILE=""
        return 0
    fi

    init_run_log
    local safe_label="${label//[^a-zA-Z0-9_-]/_}"
    LAST_MESSAGE_FILE="$LOOPER_LOG_DIR/${RUN_ID}-${safe_label}.last.json"
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

    while IFS= read -r line; do
        [ -z "$line" ] && continue
        local annotated
        annotated=$(annotate_line "$line" "$label" "$iteration")
        echo "$annotated" >> "$LOG_FILE"
        if [ "$CODEX_PROGRESS" -eq 1 ]; then
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

    jq -e '.schema_version == 1 and (.source_files | type == "array") and (.tasks | type == "array")' "$TODO_FILE" >/dev/null 2>&1
}

has_open_tasks() {
    jq -e '.tasks[]? | select(.status != "done")' "$TODO_FILE" >/dev/null 2>&1
}

list_tasks_by_status() {
    local status="$1"
    jq --arg status "$status" '.tasks[] | select(.status == $status)' "$TODO_FILE"
}

current_task_line() {
    jq -r '
        def first_or_null($arr):
            if ($arr | length) > 0 then $arr[0] else null end;
        .tasks as $t
        | ([$t[] | select(.status == "doing")] | sort_by(.id) | first_or_null(.)) as $doing
        | if $doing then $doing
          else
            ([$t[] | select(.status == "todo")] | sort_by(.priority, .id) | first_or_null(.)) as $todo
            | if $todo then $todo
              else
                ([$t[] | select(.status == "blocked")] | sort_by(.priority, .id) | first_or_null(.)) as $blocked
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

    echo "Codex model: $CODEX_MODEL (reasoning: $CODEX_REASONING_EFFORT)"
    if [ -n "$CODEX_PROFILE" ]; then
        echo "Codex mode: $mode | profile: $CODEX_PROFILE"
    else
        echo "Codex mode: $mode"
    fi
    echo "Codex flags: $CODEX_BIN $flags_string -"
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

run_codex() {
    local label="${1:-run}"
    local expect_summary="${2:-0}"
    local iteration="${3:-0}"
    local cmd=("$CODEX_BIN" "${CODEX_FLAGS[@]}")

    prepare_run_files "$label"

    if [ "$CODEX_JSON_LOG" -eq 1 ]; then
        cmd+=(--json --output-last-message "$LAST_MESSAGE_FILE")
        if [ "$expect_summary" -eq 1 ] && [ "$CODEX_ENFORCE_OUTPUT_SCHEMA" -eq 1 ]; then
            write_summary_schema_if_missing
            cmd+=(--output-schema "$SUMMARY_SCHEMA_FILE")
        fi
    fi

    cmd+=(-)

    if [ "$CODEX_JSON_LOG" -eq 1 ]; then
        "${cmd[@]}" 2>&1 | stream_with_annotation "$label" "$iteration"
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

    echo "Repairing $TODO_FILE with $CODEX_BIN..."
    run_codex "repair" 0 0 <<EOF
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
    run_codex "bootstrap" 0 0 <<EOF
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

    require_cmd "$CODEX_BIN"
    require_cmd jq

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
            echo "Reached max iterations ($MAX_ITERATIONS). Exiting."
            break
        fi

        ensure_valid_todo

        if ! has_open_tasks; then
            echo "No open tasks remain. Exiting."
            break
        fi

        echo "Iteration $iteration/$MAX_ITERATIONS"
        print_iteration_task

        run_codex "iter-$iteration" 1 "$iteration" <<EOF
You are running in a deterministic RALF loop with fresh context each run.
Just for fun we are naming you Ralf (in honour of Ralph Wiggum German cousin Ralf).

Goal: complete exactly one task from "$TODO_FILE" per iteration.

Rules:
- Read "$TODO_FILE" and follow the schema in "$SCHEMA_FILE".
- Read every file listed in source_files and treat them as ground truth for task selection and implementation.
- If any task has status "doing", continue that task. If multiple, pick the lowest id.
- Otherwise pick the highest priority task with status "todo". If none, pick the highest priority "blocked" task and attempt to unblock it.
- If multiple tasks share priority, pick the lowest id.
- Set the chosen task status to "doing" before making changes.
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
        apply_summary_to_todo
        ensure_valid_todo

        if [ "$LOOP_DELAY_SECONDS" -gt 0 ]; then
            sleep "$LOOP_DELAY_SECONDS"
        fi
    done
}

main "$@"
