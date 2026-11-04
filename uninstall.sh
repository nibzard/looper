#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'USAGE'
Usage: ./uninstall.sh [options]

Removes looper and the Codex skills installed by this project.

Options:
  --prefix <path>      Uninstall from prefix (bin -> <prefix>/bin,
                       skills -> <prefix>/share/looper/skills)
  --bin-dir <path>     Override bin dir (default: <prefix>/bin)
  --codex-home <path>  Override CODEX_HOME (default: ~/.codex)
  --skills-dir <path>  Override skills dir (default: <CODEX_HOME>/skills)
  --skip-bin           Skip removing the binary
  --skip-skills        Skip removing skills
  --dry-run            Print actions without making changes
  -h, --help           Show help
USAGE
}

DEFAULT_PREFIX="$HOME/.local"
PREFIX="${PREFIX:-$DEFAULT_PREFIX}"
TARGET_CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"

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
    TARGET_CODEX_HOME="$CODEX_HOME_OVERRIDE"
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
        SKILLS_DIR="$TARGET_CODEX_HOME/skills"
    fi
fi

run() {
    if [ "$DRY_RUN" -eq 1 ]; then
        printf 'dry-run: %s\n' "$*"
    else
        "$@"
    fi
}

SKILLS_RECEIPT="$SKILLS_DIR/.looper-installed-skills"
if [ "$REMOVE_SKILLS" -eq 1 ] && { [ -e "$SKILLS_RECEIPT" ] || [ -L "$SKILLS_RECEIPT" ]; }; then
    if [ ! -f "$SKILLS_RECEIPT" ] || [ -L "$SKILLS_RECEIPT" ]; then
        echo "Error: invalid skill receipt: $SKILLS_RECEIPT" >&2
        exit 1
    fi
    # Validate the complete receipt before removing anything.
    while IFS= read -r name; do
        case "$name" in
            looper-diagnose|looper-backlog|git-commit|github-release) ;;
            *) echo "Error: invalid skill receipt: $SKILLS_RECEIPT" >&2; exit 1 ;;
        esac
    done < "$SKILLS_RECEIPT"
    if [ -n "$name" ]; then
        echo "Error: incomplete skill receipt: $SKILLS_RECEIPT" >&2
        exit 1
    fi
fi

if [ "$REMOVE_BIN" -eq 1 ]; then
    if [ -f "$BIN_DIR/looper" ]; then
        run rm -f "$BIN_DIR/looper"
    fi
    # Legacy files from the retired Bash implementation.
    if [ -f "$BIN_DIR/looper.sh" ]; then
        run rm -f "$BIN_DIR/looper.sh"
    fi
    if [ -f "$BIN_DIR/looper_parallel.py" ]; then
        run rm -f "$BIN_DIR/looper_parallel.py"
    fi
fi

if [ "$REMOVE_SKILLS" -eq 1 ]; then
    if [ -f "$SKILLS_RECEIPT" ]; then
        while IFS= read -r name || [ -n "$name" ]; do
            run rm -rf -- "$SKILLS_DIR/$name"
        done < "$SKILLS_RECEIPT"
        run rm -f -- "$SKILLS_RECEIPT"
    else
        echo "No skill receipt found; keeping existing skills: $SKILLS_DIR" >&2
    fi
fi

echo "Uninstall complete."
