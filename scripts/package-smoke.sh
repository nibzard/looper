#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

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
"$BREW_DIR/bin/looper-uninstall" --prefix "$PREFIX_DIR"
test ! -e "$PREFIX_DIR/bin/looper"
test ! -e "$PREFIX_DIR/share/looper/skills/looper-backlog"
test ! -e "$PREFIX_DIR/share/looper/skills/looper-diagnose"

echo "Package smoke test passed."
