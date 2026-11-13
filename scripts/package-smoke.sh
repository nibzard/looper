#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

# T056: installer fixtures run against a sandbox HOME and a bound user
# prefix, so no invocation shape can write to the operator's real prefix.
export HOME="$TMP_DIR/home"
export LOOPER_USER_PREFIX="$TMP_DIR/user-prefix"
unset PREFIX CODEX_HOME
mkdir -p "$HOME"

BREW_DIR="$TMP_DIR/homebrew"
mkdir -p "$BREW_DIR/bin" "$BREW_DIR/share/looper"
install -m 0755 "$ROOT_DIR/install.sh" "$BREW_DIR/bin/looper-install"
install -m 0755 "$ROOT_DIR/uninstall.sh" "$BREW_DIR/bin/looper-uninstall"
install -m 0755 "$ROOT_DIR/build/looper" "$BREW_DIR/bin/looper"
cp -a "$ROOT_DIR/skills" "$BREW_DIR/share/looper/skills"

SKILLS_DIR="$TMP_DIR/user-skills"
"$BREW_DIR/bin/looper-install" --skip-bin --skills-dir "$SKILLS_DIR"
test -f "$SKILLS_DIR/looper-diagnose/SKILL.md"
test -f "$SKILLS_DIR/looper-backlog/SKILL.md"
test ! -e "$SKILLS_DIR/git-commit"
test ! -e "$SKILLS_DIR/github-release"
"$BREW_DIR/bin/looper-install" --skip-bin --with-workflow-skills --skills-dir "$SKILLS_DIR"
test -f "$SKILLS_DIR/git-commit/SKILL.md"
test -f "$SKILLS_DIR/looper-backlog/SKILL.md"
test -f "$SKILLS_DIR/github-release/SKILL.md"
test -x "$SKILLS_DIR/github-release/scripts/release.sh"
test -f "$SKILLS_DIR/github-release/references/helper.md"
"$SKILLS_DIR/github-release/scripts/release.sh" --help >/dev/null

# Value options invoked without a value must report a usage error under
# set -u instead of crashing on an unbound $2 (T036).
expect_missing_value_error() {
    local script="$1" option="$2" output
    if output=$(bash "$script" "$option" 2>&1); then
        echo "Error: $script $option without a value must fail." >&2
        exit 1
    fi
    case "$output" in
        *"$option"*) ;;
        *)
            echo "Error: $script $option must name the option in its error:" >&2
            printf '%s\n' "$output" >&2
            exit 1
            ;;
    esac
    case "$output" in
        *value*) ;;
        *)
            echo "Error: $script $option must report the missing value:" >&2
            printf '%s\n' "$output" >&2
            exit 1
            ;;
    esac
}
for option in --prefix --bin-dir --skills-dir --codex-home; do
    expect_missing_value_error "$BREW_DIR/bin/looper-install" "$option"
    expect_missing_value_error "$BREW_DIR/bin/looper-uninstall" "$option"
done
for option in --version --bin-dir --prefix --skills-dir --codex-home; do
    expect_missing_value_error "$ROOT_DIR/scripts/install-latest.sh" "$option"
done
for option in --version --notes --test-cmd --bump-cmd --version-file \
              --commit-msg --notes-file --stage-path --formula --repo; do
    expect_missing_value_error "$SKILLS_DIR/github-release/scripts/release.sh" "$option"
done

# Empty-array expansions must keep working under set -u on Bash versions
# older than 4.4 (including macOS 3.2): every "$ARR[@]" expansion of the
# arrays below carries the ${arr[@]+"${arr[@]}"} guard (T036).
require_array_guard() {
    local file="$1" array="$2" offending
    offending=$(grep -nF '"${'"$array"'[@]}"' "$file" \
        | grep -vF '${'"$array"'[@]+"${'"$array"'[@]}"}' || true)
    if [ -n "$offending" ]; then
        echo "Error: $file expands \$$array without an empty-array guard:" >&2
        printf '%s\n' "$offending" >&2
        exit 1
    fi
}
require_array_guard "$ROOT_DIR/scripts/install-latest.sh" install_args
require_array_guard "$ROOT_DIR/skills/github-release/scripts/release.sh" VERSION_FILES

"$BREW_DIR/bin/looper-uninstall" --skip-bin --skills-dir "$SKILLS_DIR"
test ! -e "$SKILLS_DIR/looper-diagnose"
test ! -e "$SKILLS_DIR/git-commit"
test ! -e "$SKILLS_DIR/looper-backlog"
test ! -e "$SKILLS_DIR/github-release"

PREFIX_DIR="$TMP_DIR/user-prefix"
"$BREW_DIR/bin/looper-install" --prefix "$PREFIX_DIR"
test -x "$PREFIX_DIR/bin/looper"
"$PREFIX_DIR/bin/looper" --help >/dev/null
test "$("$PREFIX_DIR/bin/looper" --version)" = "$("$ROOT_DIR/build/looper" --version)"
test -f "$PREFIX_DIR/share/looper/skills/looper-backlog/SKILL.md"
mkdir -p "$PREFIX_DIR/share/looper/skills/git-commit"
printf 'User-authored skill.\n' > "$PREFIX_DIR/share/looper/skills/git-commit/SKILL.md"
"$BREW_DIR/bin/looper-uninstall" --prefix "$PREFIX_DIR"
test ! -e "$PREFIX_DIR/bin/looper"
test ! -e "$PREFIX_DIR/share/looper/skills/looper-backlog"
test ! -e "$PREFIX_DIR/share/looper/skills/looper-diagnose"
test "$(cat "$PREFIX_DIR/share/looper/skills/git-commit/SKILL.md")" = 'User-authored skill.'
test ! -e "$PREFIX_DIR/share/looper/skills/.looper-installed-skills"

# The incident shape (T056): a no-options install used to write
# ~/.local/bin/looper and ~/.codex/skills. Under the bound user prefix it
# cannot leave the sandbox, and the sandbox HOME keeps no default locations.
"$BREW_DIR/bin/looper-install"
test -x "$LOOPER_USER_PREFIX/bin/looper"
test -f "$LOOPER_USER_PREFIX/share/looper/skills/looper-backlog/SKILL.md"
test -f "$LOOPER_USER_PREFIX/share/looper/skills/looper-diagnose/SKILL.md"
test ! -e "$HOME/.local"
test ! -e "$HOME/.codex"
"$BREW_DIR/bin/looper-uninstall"
test ! -e "$LOOPER_USER_PREFIX/bin/looper"
test ! -e "$LOOPER_USER_PREFIX/share/looper/skills/looper-diagnose"
test ! -e "$LOOPER_USER_PREFIX/share/looper/skills/looper-backlog"
test ! -e "$LOOPER_USER_PREFIX/share/looper/skills/.looper-installed-skills"

echo "Package smoke test passed."
