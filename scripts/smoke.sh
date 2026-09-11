#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TMP_DIR=$(mktemp -d)
PROJECT_DIR="$TMP_DIR/project"
STUB_BIN="$TMP_DIR/bin"
RUN_LOG="$TMP_DIR/run.log"
SMART_PROJECT_DIR="$TMP_DIR/smart-project"
SMART_RUN_LOG="$TMP_DIR/smart-run.log"
NORMAL_ARGS_LOG="$TMP_DIR/normal-args.log"
SMART_ARGS_LOG="$TMP_DIR/smart-args.log"

cleanup() {
    rm -rf "$TMP_DIR"
}
trap cleanup EXIT

mkdir -p "$PROJECT_DIR" "$STUB_BIN"

log_contains() {
    local pattern="$1"
    local file="$2"
    if command -v rg >/dev/null 2>&1; then
        rg -q "$pattern" "$file"
    else
        grep -q "$pattern" "$file"
    fi
}

cat > "$STUB_BIN/codex" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

last_message=""
workdir=""
json_output=0

if [ -n "${ARGS_LOG:-}" ]; then
    printf '%q ' "$@" >> "$ARGS_LOG"
    printf '\n' >> "$ARGS_LOG"
fi

while [ "$#" -gt 0 ]; do
    case "$1" in
        --output-last-message)
            last_message="$2"
            shift 2
            ;;
        --cd)
            workdir="$2"
            shift 2
            ;;
        --json)
            json_output=1
            shift
            ;;
        -)
            shift
            break
            ;;
        *)
            shift
            ;;
    esac
done

cat >/dev/null || true

if [ -n "$workdir" ]; then
    mkdir -p "$workdir"
    printf "smoke test readme\n" > "$workdir/README.md"
fi

summary='{"task_id":"T2","status":"done","summary":"smoke","files":["README.md"],"blockers":[]}'
if [ -n "$last_message" ]; then
    printf '%s\n' "$summary" > "$last_message"
fi

if [ "$json_output" -eq 1 ]; then
    printf '{"type":"assistant_message","message":{"content":[{"type":"text","text":"smoke"}]}}\n'
else
    printf '%s\n' "$summary"
fi
EOF

chmod +x "$STUB_BIN/codex"

cat > "$PROJECT_DIR/PROJECT.md" <<'EOF'
# Smoke Test Project

Goal: verify the looper can run one iteration and update to-do.json.
EOF

cat > "$PROJECT_DIR/to-do.json" <<'EOF'
{
  "schema_version": 1,
  "source_files": ["PROJECT.md"],
  "tasks": [
    {
      "id": "T10",
      "title": "Create hello.txt",
      "priority": 1,
      "status": "todo"
    },
    {
      "id": "T2",
      "title": "Add README.md",
      "priority": 1,
      "status": "todo"
    }
  ]
}
EOF

cp -R "$PROJECT_DIR" "$SMART_PROJECT_DIR"

(
    cd "$PROJECT_DIR"
    PATH="$STUB_BIN:$PATH" \
        ARGS_LOG="$NORMAL_ARGS_LOG" \
        CODEX_BIN=codex \
        CODEX_JSON_LOG=0 \
        LOOPER_GIT_INIT=0 \
        LOOP_DELAY_SECONDS=0 \
        MAX_ITERATIONS=1 \
        "$ROOT_DIR/bin/looper.sh" to-do.json 2>&1 | tee "$RUN_LOG"
)

log_contains "Task: T2" "$RUN_LOG"
log_contains "exec -m gpt-5.6-terra -c model_reasoning_effort=medium" "$NORMAL_ARGS_LOG"
test -f "$PROJECT_DIR/README.md"
test -f "$PROJECT_DIR/to-do.schema.json"
jq -e '.tasks[] | select(.id == "T2" and .status == "done")' "$PROJECT_DIR/to-do.json" >/dev/null
if log_contains "integer expression expected" "$RUN_LOG"; then
    exit 1
fi
if log_contains "numeric argument required" "$RUN_LOG"; then
    exit 1
fi

(
    cd "$SMART_PROJECT_DIR"
    PATH="$STUB_BIN:$PATH" \
        ARGS_LOG="$SMART_ARGS_LOG" \
        CODEX_BIN=codex \
        CODEX_JSON_LOG=0 \
        LOOPER_GIT_INIT=0 \
        LOOP_DELAY_SECONDS=0 \
        MAX_ITERATIONS=1 \
        "$ROOT_DIR/bin/looper.sh" --smart to-do.json 2>&1 | tee "$SMART_RUN_LOG"
)

log_contains "exec -m gpt-6-astra -c model_reasoning_effort=high" "$SMART_ARGS_LOG"

(
    cd "$PROJECT_DIR"
    PATH="$STUB_BIN:$PATH" \
        CODEX_BIN=codex \
        LOOPER_GIT_INIT=0 \
        "$ROOT_DIR/bin/looper.sh" --doctor to-do.json
)

echo "Smoke test passed."
