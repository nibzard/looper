#!/usr/bin/env bash
set -euo pipefail

TMP_DIR=$(mktemp -d)
PROJECT_DIR="$TMP_DIR/project"
RUN_LOG="$TMP_DIR/run.log"

resolve_looper_bin() {
    if [ -n "${LOOPER_BIN:-}" ]; then
        case "$LOOPER_BIN" in
            /*) printf "%s" "$LOOPER_BIN" ;;
            *) printf "%s/%s" "$(pwd -P)" "$LOOPER_BIN" ;;
        esac
        return 0
    fi

    if [ -x "./build/looper" ]; then
        printf "%s" "$(pwd -P)/build/looper"
        return 0
    fi

    if [ -n "${LOOPER_REPO:-}" ] && [ -x "$LOOPER_REPO/build/looper" ]; then
        printf "%s" "$(cd "$LOOPER_REPO" && pwd -P)/build/looper"
        return 0
    fi

    local checkout
    checkout=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd -P)
    if [ -x "$checkout/build/looper" ]; then
        printf '%s' "$checkout/build/looper"
        return 0
    fi

    if command -v looper >/dev/null 2>&1; then
        command -v looper
        return 0
    fi

    echo "Error: looper not found. Set LOOPER_BIN or LOOPER_REPO, build with make build, or add looper to PATH." >&2
    return 1
}

log_contains() {
    local pattern="$1"
    local file="$2"
    if command -v rg >/dev/null 2>&1; then
        rg -m 1 "$pattern" "$file" || true
    else
        grep -E -m 1 "$pattern" "$file" || true
    fi
}

LOOPER_BIN=$(resolve_looper_bin)

mkdir -p "$PROJECT_DIR"
git init -q "$PROJECT_DIR"

cat > "$PROJECT_DIR/PROJECT.md" <<'EOF'
# Smoke Test Project

Goal: verify looper runs one iteration and updates to-do.json.
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

run_status=0
(
    cd "$PROJECT_DIR"
    CODEX_JSON_LOG=0 \
        LOOPER_BASE_DIR="$TMP_DIR/logs" \
        LOOPER_GIT_INIT=0 \
        LOOPER_VERIFY_COMMAND="test -s README.md" \
        LOOP_DELAY_SECONDS=0 \
        MAX_ITERATIONS=1 \
        "$LOOPER_BIN" --once --jobs 1 to-do.json 2>&1 | tee "$RUN_LOG"
) || run_status=$?

if [ "$run_status" -ne 0 ] && [ "$run_status" -ne 2 ]; then
    echo "Looper failed with exit code $run_status." >&2
    exit "$run_status"
fi

require_outcome() {
    if ! jq -e '
        any(.tasks[]; .id == "T2" and .status == "done")
        and any(.tasks[]; .id == "T10" and .status == "todo")
    ' "$PROJECT_DIR/to-do.json" >/dev/null || [ ! -s "$PROJECT_DIR/README.md" ]; then
        echo "Error: expected T2 done, T10 todo, and a nonempty README.md." >&2
        echo "Inspect $PROJECT_DIR and $RUN_LOG." >&2
        exit 1
    fi
}
require_outcome

echo "Looper executable: $LOOPER_BIN"
echo "Temp project: $PROJECT_DIR"
echo "Run log: $RUN_LOG"
echo "Selected task: $(log_contains '^T2  ' "$RUN_LOG")"
echo "Result: $(log_contains '^T2  done' "$RUN_LOG")"

echo "Task statuses:"
jq -r '.tasks[] | "\(.id)\t\(.status)\t\(.title)"' "$PROJECT_DIR/to-do.json"
echo "Live test outcome verified."
