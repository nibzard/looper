#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'USAGE'
Usage: ./uninstall.sh [options]

Removes looper.sh and Codex skills installed by this project.

Options:
  --prefix <path>      Uninstall from prefix (bin -> <prefix>/bin,
                       skills -> <prefix>/share/looper/skills)
  --bin-dir <path>     Override bin dir (default: <prefix>/bin)
  --codex-home <path>  Override CODEX_HOME (default: ~/.codex)
  --skills-dir <path>  Override skills dir (default: <CODEX_HOME>/skills)
  --skip-bin           Skip removing looper.sh
  --skip-skills        Skip removing skills
  --dry-run            Print actions without making changes
  -h, --help           Show help
USAGE
}

DEFAULT_PREFIX="$HOME/.local"
PREFIX="${PREFIX:-$DEFAULT_PREFIX}"
CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"

PREFIX_OVERRIDE=""
BIN_DIR_OVERRIDE=""
SKILLS_DIR_OVERRIDE=""
CODEX_HOME_OVERRIDE=""
REMOVE_BIN=1
REMOVE_SKILLS=1
DRY_RUN=0
PREFIX_MODE=0

while [ "$#" -gt 0 ]; do
    case "$1" in
        --prefix)
            PREFIX_OVERRIDE="$2"
            PREFIX_MODE=1
            shift 2
            ;;
        --bin-dir)
            BIN_DIR_OVERRIDE="$2"
            shift 2
            ;;
        --skills-dir)
            SKILLS_DIR_OVERRIDE="$2"
            shift 2
            ;;
        --codex-home)
            CODEX_HOME_OVERRIDE="$2"
            shift 2
            ;;
        --skip-bin)
            REMOVE_BIN=0
            shift
            ;;
        --skip-skills)
            REMOVE_SKILLS=0
            shift
            ;;
        --dry-run)
            DRY_RUN=1
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "Unknown option: $1" >&2
            usage
            exit 1
            ;;
    esac
done

if [ -n "$PREFIX_OVERRIDE" ]; then
    PREFIX="$PREFIX_OVERRIDE"
fi

if [ -n "$CODEX_HOME_OVERRIDE" ]; then
    CODEX_HOME="$CODEX_HOME_OVERRIDE"
fi

if [ "$PREFIX" != "$DEFAULT_PREFIX" ]; then
    PREFIX_MODE=1
fi

if [ -n "$BIN_DIR_OVERRIDE" ]; then
    BIN_DIR="$BIN_DIR_OVERRIDE"
else
    BIN_DIR="$PREFIX/bin"
fi

if [ -n "$SKILLS_DIR_OVERRIDE" ]; then
    SKILLS_DIR="$SKILLS_DIR_OVERRIDE"
else
    if [ "$PREFIX_MODE" -eq 1 ] && [ -z "$CODEX_HOME_OVERRIDE" ]; then
        SKILLS_DIR="$PREFIX/share/looper/skills"
    else
        SKILLS_DIR="$CODEX_HOME/skills"
    fi
fi

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT_DIR="$SCRIPT_DIR"
SRC_SKILLS="$ROOT_DIR/skills"

run() {
    if [ "$DRY_RUN" -eq 1 ]; then
        printf 'dry-run: %s\n' "$*"
    else
        "$@"
    fi
}

if [ "$REMOVE_BIN" -eq 1 ]; then
    if [ -f "$BIN_DIR/looper.sh" ]; then
        run rm -f "$BIN_DIR/looper.sh"
    fi
fi

if [ "$REMOVE_SKILLS" -eq 1 ]; then
    if [ -d "$SRC_SKILLS" ] && [ -d "$SKILLS_DIR" ]; then
        shopt -s dotglob nullglob
        for skill in "$SRC_SKILLS"/*; do
            [ -d "$skill" ] || continue
            name=$(basename "$skill")
            target="$SKILLS_DIR/$name"
            if [ -e "$target" ]; then
                run rm -rf "$target"
            fi
        done
        shopt -u dotglob nullglob
    fi
fi

echo "Uninstall complete."
