#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

BREW_DIR="$TMP_DIR/homebrew"
mkdir -p "$BREW_DIR/bin" "$BREW_DIR/share/looper"
install -m 0755 "$ROOT_DIR/install.sh" "$BREW_DIR/bin/looper-install"
install -m 0755 "$ROOT_DIR/uninstall.sh" "$BREW_DIR/bin/looper-uninstall"
install -m 0755 "$ROOT_DIR/bin/looper.sh" "$BREW_DIR/bin/looper.sh"
cp -a "$ROOT_DIR/skills" "$BREW_DIR/share/looper/skills"

SKILLS_DIR="$TMP_DIR/user-skills"
"$BREW_DIR/bin/looper-install" --skip-bin --skills-dir "$SKILLS_DIR"
test -f "$SKILLS_DIR/git-conventional-commit/SKILL.md"
test -f "$SKILLS_DIR/todo-json-manager/SKILL.md"
test -f "$SKILLS_DIR/release-runbook/SKILL.md"

"$BREW_DIR/bin/looper-uninstall" --skip-bin --skills-dir "$SKILLS_DIR"
test ! -e "$SKILLS_DIR/git-conventional-commit"
test ! -e "$SKILLS_DIR/todo-json-manager"
test ! -e "$SKILLS_DIR/release-runbook"

PREFIX_DIR="$TMP_DIR/user-prefix"
"$BREW_DIR/bin/looper-install" --prefix "$PREFIX_DIR"
test -x "$PREFIX_DIR/bin/looper.sh"
test -f "$PREFIX_DIR/share/looper/skills/release-runbook/SKILL.md"
"$BREW_DIR/bin/looper-uninstall" --prefix "$PREFIX_DIR"
test ! -e "$PREFIX_DIR/bin/looper.sh"
test ! -e "$PREFIX_DIR/share/looper/skills/release-runbook"

echo "Package smoke test passed."
