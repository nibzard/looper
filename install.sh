#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'USAGE'
Usage: ./install.sh [options]

Installs looper.sh and Codex skills into user-appropriate locations.

Options:
  --prefix <path>      Install into a prefix (bin -> <prefix>/bin,
                       skills -> <prefix>/share/looper/skills)
  --bin-dir <path>     Override bin dir (default: <prefix>/bin)
  --codex-home <path>  Override CODEX_HOME (default: ~/.codex)
  --skills-dir <path>  Override skills dir (default: <CODEX_HOME>/skills)
  --skip-bin           Skip installing looper.sh
  --skip-skills        Skip installing skills
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
INSTALL_BIN=1
INSTALL_SKILLS=1
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
            INSTALL_BIN=0
            shift
            ;;
        --skip-skills)
            INSTALL_SKILLS=0
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
SRC_BIN="$ROOT_DIR/bin/looper.sh"
SRC_SKILLS="$ROOT_DIR/skills"

if [ ! -f "$SRC_BIN" ]; then
    echo "Error: missing $SRC_BIN" >&2
    exit 1
fi

if [ ! -d "$SRC_SKILLS" ]; then
    echo "Error: missing $SRC_SKILLS" >&2
    exit 1
fi

run() {
    if [ "$DRY_RUN" -eq 1 ]; then
        printf 'dry-run: %s\n' "$*"
    else
        "$@"
    fi
}

warn_missing() {
    local cmd="$1"
    if ! command -v "$cmd" >/dev/null 2>&1; then
        echo "Warning: '$cmd' not found on PATH." >&2
    fi
}

if [ "$INSTALL_BIN" -eq 1 ]; then
    run install -d "$BIN_DIR"
    run install -m 0755 "$SRC_BIN" "$BIN_DIR/looper.sh"
fi

if [ "$INSTALL_SKILLS" -eq 1 ]; then
    run mkdir -p "$SKILLS_DIR"
    run cp -a "$SRC_SKILLS"/. "$SKILLS_DIR"/
fi

if [ "$INSTALL_BIN" -eq 1 ]; then
    case ":$PATH:" in
        *":$BIN_DIR:"*) ;;
        *)
            echo "Note: $BIN_DIR is not on PATH. Add it to your shell profile." >&2
            ;;
    esac
fi

warn_missing jq
warn_missing codex

echo "Install complete."
echo "  looper.sh -> $BIN_DIR/looper.sh"
if [ "$INSTALL_SKILLS" -eq 1 ]; then
    echo "  skills -> $SKILLS_DIR"
fi
