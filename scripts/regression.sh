#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT
mkdir -p "$TMP_DIR/bin"

cat > "$TMP_DIR/bin/codex" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
last_message=""
workdir=""
if [ -n "${STUB_CODEX_ARGS_LOG:-}" ]; then
    printf '%s\n' "$*" >> "$STUB_CODEX_ARGS_LOG"
fi
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output-last-message) last_message="$2"; shift 2 ;;
        --cd) workdir="$2"; shift 2 ;;
        *) shift ;;
    esac
done
prompt=$(cat)
printf 'call\n' >> "$STUB_CALLS_LOG"
if [[ "$prompt" == *'final review pass'* ]]; then
    if [ "${STUB_REVIEW_MARKER:-0}" -eq 1 ]; then
        jq '.tasks += [{"id":"PROJECT-DONE","title":"Project done","priority":5,"status":"done","tags":["project-done"]}]' \
            "$workdir/to-do.json" > "$workdir/review.tmp"
        mv "$workdir/review.tmp" "$workdir/to-do.json"
    fi
    if [ "${STUB_REVIEW_FAIL:-0}" -eq 1 ]; then
        exit 7
    fi
    summary='{"status":"reviewed","summary":"reviewed","added_tasks":0,"files":[]}'
else
    if [ "${STUB_WRITE_CODE:-0}" -eq 1 ]; then
        printf 'unfinished work\n' > "$workdir/agent-change.txt"
    fi
    if [ "${STUB_EDIT_TODO:-0}" -eq 1 ]; then
        jq '.tasks[0].status = "done"' "$workdir/to-do.json" > "$workdir/iteration.tmp"
        mv "$workdir/iteration.tmp" "$workdir/to-do.json"
    fi
    summary='{"task_id":"T1","status":"done","summary":"done","files":[],"blockers":[]}'
    if [ -n "${STUB_SUMMARY:-}" ]; then
        summary="$STUB_SUMMARY"
    fi
fi
printf '%s\n' "$summary" > "$last_message"
printf '{"type":"result"}\n'
exit "${STUB_EXIT_CODE:-0}"
EOF

cat > "$TMP_DIR/bin/claude" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%q ' "$@" >> "$STUB_CLAUDE_ARGS_LOG"
printf '\n' >> "$STUB_CLAUDE_ARGS_LOG"
printf '%s\n' '{"structured_output":{"task_id":"T1","status":"done","summary":"done","files":[],"blockers":[]}}'
EOF
chmod +x "$TMP_DIR/bin/codex" "$TMP_DIR/bin/claude"

make_project() {
    local name="$1"
    local status="${2:-todo}"
    mkdir -p "$TMP_DIR/$name"
    cat > "$TMP_DIR/$name/to-do.json" <<EOF
{"schema_version":1,"source_files":[],"tasks":[
  {"id":"T1","title":"First task","priority":1,"status":"$status"},
  {"id":"T2","title":"Second task","priority":2,"status":"done"}
]}
EOF
}

expect_status() {
    local expected="$1"
    local name="$2"
    shift 2
    local result=0
    (
        cd "$TMP_DIR/$name"
        CODEX_BIN="$TMP_DIR/bin/codex" \
        CLAUDE_BIN="${TEST_CLAUDE_BIN:-$TMP_DIR/bin/claude}" \
        CODEX_JSON_LOG=0 \
        LOOPER_GIT_INIT=0 \
        LOOP_DELAY_SECONDS=0 \
        MAX_ITERATIONS="${TEST_MAX_ITERATIONS:-1}" \
        STUB_CALLS_LOG="$TMP_DIR/$name/calls.log" \
        STUB_CLAUDE_ARGS_LOG="$TMP_DIR/$name/claude-args.log" \
        "$ROOT_DIR/bin/looper.sh" "$@"
    ) > "$TMP_DIR/$name/output.log" 2>&1 || result=$?
    if [ "$result" -ne "$expected" ]; then
        cat "$TMP_DIR/$name/output.log" >&2
        echo "Expected exit $expected for $name, got $result" >&2
        exit 1
    fi
}

make_project mismatch
export STUB_SUMMARY='{"task_id":"T2","status":"done","summary":"wrong task","files":[],"blockers":[]}'
expect_status 1 mismatch to-do.json
jq -e '.tasks[0].status == "todo" and .tasks[1].status == "done"' "$TMP_DIR/mismatch/to-do.json" >/dev/null
test "$(wc -l < "$TMP_DIR/mismatch/calls.log")" -eq 1
unset STUB_SUMMARY

make_project missing_summary
export STUB_SUMMARY='{}'
expect_status 1 missing_summary to-do.json
jq -e '.tasks[0].status == "todo"' "$TMP_DIR/missing_summary/to-do.json" >/dev/null
test "$(wc -l < "$TMP_DIR/missing_summary/calls.log")" -eq 1
unset STUB_SUMMARY

make_project failed_agent
export STUB_EXIT_CODE=7 STUB_EDIT_TODO=1
expect_status 7 failed_agent to-do.json
jq -e '.tasks[0].status == "todo"' "$TMP_DIR/failed_agent/to-do.json" >/dev/null
unset STUB_EXIT_CODE STUB_EDIT_TODO

make_project failed_review done
export STUB_REVIEW_FAIL=1 STUB_REVIEW_MARKER=1 TEST_MAX_ITERATIONS=5
expect_status 1 failed_review to-do.json
jq -e '(.tasks | length) == 2' "$TMP_DIR/failed_review/to-do.json" >/dev/null
if [ "$(wc -l < "$TMP_DIR/failed_review/calls.log")" -ne 3 ]; then
    cat "$TMP_DIR/failed_review/output.log" >&2
    echo "Expected three review attempts" >&2
    exit 1
fi
unset STUB_REVIEW_FAIL STUB_REVIEW_MARKER TEST_MAX_ITERATIONS

make_project good_review done
export STUB_REVIEW_MARKER=1
expect_status 0 good_review to-do.json
jq -e '.tasks[-1].tags | index("project-done")' "$TMP_DIR/good_review/to-do.json" >/dev/null
unset STUB_REVIEW_MARKER

make_project claude
export STUB_REVIEW_MARKER=1
expect_status 0 claude --interleave to-do.json
grep -q -- '--output-format json' "$TMP_DIR/claude/claude-args.log"
if grep -Eq -- 'stream-json|--include-partial-messages' "$TMP_DIR/claude/claude-args.log"; then
    echo "Claude received streaming output flags" >&2
    exit 1
fi
grep -q 'Review agent: codex' "$TMP_DIR/claude/output.log"
grep -q -- '--json-schema' "$TMP_DIR/claude/claude-args.log"
unset STUB_REVIEW_MARKER

make_project blocked_only blocked
expect_status 2 blocked_only to-do.json
test ! -e "$TMP_DIR/blocked_only/calls.log"
grep -q 'No runnable tasks remain' "$TMP_DIR/blocked_only/output.log"

make_project newly_blocked
export STUB_SUMMARY='{"task_id":"T1","status":"blocked","summary":"Missing input","files":[],"blockers":["API key is missing"]}'
expect_status 2 newly_blocked to-do.json
jq -e '.tasks[0].status == "blocked" and .tasks[0].blockers[0] == "API key is missing"' \
    "$TMP_DIR/newly_blocked/to-do.json" >/dev/null
test "$(wc -l < "$TMP_DIR/newly_blocked/calls.log")" -eq 1
unset STUB_SUMMARY

make_project dependency
jq '.tasks[0].depends_on = ["T3"] | .tasks += [{"id":"T3","title":"Prerequisite","priority":5,"status":"todo"}]' \
    "$TMP_DIR/dependency/to-do.json" > "$TMP_DIR/dependency/next.json"
mv "$TMP_DIR/dependency/next.json" "$TMP_DIR/dependency/to-do.json"
export STUB_SUMMARY='{"task_id":"T3","status":"done","summary":"done","files":[],"blockers":[]}'
expect_status 2 dependency to-do.json
jq -e '.tasks[] | select(.id == "T1" and .status == "todo")' "$TMP_DIR/dependency/to-do.json" >/dev/null
jq -e '.tasks[] | select(.id == "T3" and .status == "done")' "$TMP_DIR/dependency/to-do.json" >/dev/null
unset STUB_SUMMARY

make_project duplicate
cp "$ROOT_DIR/to-do.schema.json" "$TMP_DIR/duplicate/to-do.schema.json"
jq '.tasks[1].id = "T1"' "$TMP_DIR/duplicate/to-do.json" > "$TMP_DIR/duplicate/next.json"
mv "$TMP_DIR/duplicate/next.json" "$TMP_DIR/duplicate/to-do.json"
expect_status 1 duplicate --doctor to-do.json

make_project unknown_dependency
cp "$ROOT_DIR/to-do.schema.json" "$TMP_DIR/unknown_dependency/to-do.schema.json"
jq '.tasks[0].depends_on = ["missing"]' "$TMP_DIR/unknown_dependency/to-do.json" > "$TMP_DIR/unknown_dependency/next.json"
mv "$TMP_DIR/unknown_dependency/next.json" "$TMP_DIR/unknown_dependency/to-do.json"
expect_status 1 unknown_dependency --doctor to-do.json

make_project verify_failure
export STUB_WRITE_CODE=1 LOOPER_VERIFY_COMMAND=false
expect_status 1 verify_failure to-do.json
jq -e '.tasks[0].status == "todo"' "$TMP_DIR/verify_failure/to-do.json" >/dev/null
test -f "$TMP_DIR/verify_failure/agent-change.txt"
grep -q 'code changes and commits remain' "$TMP_DIR/verify_failure/output.log"
unset STUB_WRITE_CODE LOOPER_VERIFY_COMMAND

make_project schema_flag
export STUB_CODEX_ARGS_LOG="$TMP_DIR/schema_flag/args.log" STUB_REVIEW_MARKER=1
expect_status 0 schema_flag to-do.json
grep -q -- '--output-schema' "$STUB_CODEX_ARGS_LOG"
unset STUB_CODEX_ARGS_LOG STUB_REVIEW_MARKER

make_project agent_owns_status
export LOOPER_APPLY_SUMMARY=0
expect_status 1 agent_owns_status to-do.json
grep -q 'Looper owns task status' "$TMP_DIR/agent_owns_status/output.log"
unset LOOPER_APPLY_SUMMARY

make_project missing_claude
export TEST_CLAUDE_BIN="$TMP_DIR/bin/not-installed"
expect_status 1 missing_claude --review-agent claude to-do.json
grep -q 'required command not found' "$TMP_DIR/missing_claude/output.log"
unset TEST_CLAUDE_BIN

make_project run_alias
export STUB_REVIEW_MARKER=1
expect_status 0 run_alias run to-do.json
test ! -e "$TMP_DIR/run_alias/run"
unset STUB_REVIEW_MARKER

sed '$d' "$ROOT_DIR/bin/looper.sh" > "$TMP_DIR/functions.sh"
mkdir -p "$TMP_DIR/fallback-bin"
ln -s "$(command -v jq)" "$TMP_DIR/fallback-bin/jq"
fallback_check() {
    local file="$1"
    PATH="$TMP_DIR/fallback-bin" /bin/bash -c \
        'source "$1"; TODO_FILE="$2"; SCHEMA_FILE="$3"; validate_todo' \
        _ "$TMP_DIR/functions.sh" "$file" "$TMP_DIR/run_alias/to-do.schema.json"
}
fallback_check "$TMP_DIR/run_alias/to-do.json"
jq '.tasks[0].priority = 1.5' "$TMP_DIR/run_alias/to-do.json" > "$TMP_DIR/invalid.json"
if fallback_check "$TMP_DIR/invalid.json"; then
    echo "Fallback validator accepted a fractional priority" >&2
    exit 1
fi
jq '.tasks[0].status = "invalid"' "$TMP_DIR/run_alias/to-do.json" > "$TMP_DIR/invalid.json"
if fallback_check "$TMP_DIR/invalid.json"; then
    echo "Fallback validator accepted an invalid status" >&2
    exit 1
fi

source "$TMP_DIR/functions.sh"
test "$(strip_json_fence '{"task_id":"T1"}')" = '{"task_id":"T1"}'
test "$(strip_json_fence $'```json\n{"task_id":"T1"}\n```')" = '{"task_id":"T1"}'

echo "Regression tests passed."
