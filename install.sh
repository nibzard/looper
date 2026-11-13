#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'USAGE'
Usage: ./install.sh [options]

Installs looper and the Codex skills into user-appropriate locations.

Options:
  --prefix <path>      Install into a prefix (bin -> <prefix>/bin,
                       skills -> <prefix>/share/looper/skills)
  --bin-dir <path>     Override bin dir (default: <prefix>/bin)
  --codex-home <path>  Override CODEX_HOME (default: ~/.codex)
  --skills-dir <path>  Override skills dir (default: <CODEX_HOME>/skills)
  --skip-bin           Skip installing the binary
  --skip-skills        Skip installing skills
  --with-workflow-skills  Also install git-commit and github-release
  --dry-run            Print actions without making changes
  -h, --help           Show help

Environment:
  LOOPER_USER_PREFIX   Default prefix instead of ~/.local; an explicit
                       --prefix still wins. Test fixtures bind this so
                       they can never write to the user's real prefix.
USAGE
}

require_value() {
    [ -n "$2" ] || {
        echo "Error: $1 requires a value." >&2
        usage >&2
        return 1
    }
}

DEFAULT_PREFIX="$HOME/.local"
# Precedence: --prefix, then PREFIX, then LOOPER_USER_PREFIX, then ~/.local.
# A LOOPER_USER_PREFIX different from ~/.local selects prefix mode below, so
# skills also land inside it.
PREFIX="${PREFIX:-${LOOPER_USER_PREFIX:-$DEFAULT_PREFIX}}"
TARGET_CODEX_HOME="${CODEX_HOME:-$HOME/.codex}"

PREFIX_OVERRIDE=""
BIN_DIR_OVERRIDE=""
SKILLS_DIR_OVERRIDE=""
CODEX_HOME_OVERRIDE=""
INSTALL_BIN=1
INSTALL_SKILLS=1
WORKFLOW_SKILLS=0
DRY_RUN=0
PREFIX_MODE=0

while [ "$#" -gt 0 ]; do
    case "$1" in
        --prefix)
            require_value "$1" "${2:-}" || exit 1
            PREFIX_OVERRIDE="$2"
            PREFIX_MODE=1
            shift 2
            ;;
        --bin-dir)
            require_value "$1" "${2:-}" || exit 1
            BIN_DIR_OVERRIDE="$2"
            shift 2
            ;;
        --skills-dir)
            require_value "$1" "${2:-}" || exit 1
            SKILLS_DIR_OVERRIDE="$2"
            shift 2
            ;;
        --codex-home)
            require_value "$1" "${2:-}" || exit 1
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
        --with-workflow-skills)
            WORKFLOW_SKILLS=1
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

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT_DIR="$SCRIPT_DIR"
SRC_PROGRAM="$ROOT_DIR/build/looper"
SRC_SKILLS="$ROOT_DIR/skills"

# Homebrew installs the commands in bin and the skills in share/looper.
if [ ! -f "$SRC_PROGRAM" ] && [ -f "$SCRIPT_DIR/looper" ]; then
    SRC_PROGRAM="$SCRIPT_DIR/looper"
fi
if [ ! -d "$SRC_SKILLS" ] && [ -d "$SCRIPT_DIR/../share/looper/skills" ]; then
    SRC_SKILLS="$SCRIPT_DIR/../share/looper/skills"
fi

if [ "$INSTALL_BIN" -eq 1 ] && [ ! -f "$SRC_PROGRAM" ] && [ ! -f "$ROOT_DIR/go.mod" ]; then
    echo "Error: missing $SRC_PROGRAM" >&2
    exit 1
fi

if [ "$INSTALL_SKILLS" -eq 1 ] && [ ! -d "$SRC_SKILLS" ]; then
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

# Replacing a binary cannot reach processes that already run it: they keep
# their mapped executable. Warn before an install replaces a destination in
# live use, so an operator knows a running controller keeps its own build.
warn_live_destination() {
    local dest="$1" target pid exe pids proc noun
    [ -f "$dest" ] || return 0
    [ -d /proc ] || return 0
    target=$(readlink -f -- "$dest" 2>/dev/null) || target="$dest"
    pids=""
    for proc in /proc/[0-9]*; do
        exe=$(readlink -- "$proc/exe" 2>/dev/null) || continue
        exe=${exe% (deleted)}
        if [ "$exe" = "$target" ]; then
            pid=${proc#/proc/}
            pids="${pids:+$pids }$pid"
        fi
    done
    if [ -n "$pids" ]; then
        noun="processes"
        set -- $pids
        if [ "$#" -eq 1 ]; then
            noun="process"
        fi
        echo "Warning: $target is in use by running $noun $pids." >&2
        echo "Installing replaces the file on disk; running controllers keep their original build until they restart." >&2
    fi
    return 0
}

SKILLS_RECEIPT="$SKILLS_DIR/.looper-installed-skills"
if [ "$INSTALL_SKILLS" -eq 1 ] && { [ -e "$SKILLS_RECEIPT" ] || [ -L "$SKILLS_RECEIPT" ]; }; then
    if [ ! -f "$SKILLS_RECEIPT" ] || [ -L "$SKILLS_RECEIPT" ]; then
        echo "Error: invalid skill receipt: $SKILLS_RECEIPT" >&2
        exit 1
    fi
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

record_skill() {
    # Record ownership before copying so an interrupted copy can be removed.
    printf '%s\n' "$1" >> "$SKILLS_RECEIPT"
}

if [ "$INSTALL_BIN" -eq 1 ]; then
    if [ -f "$ROOT_DIR/go.mod" ]; then
        run mkdir -p "$ROOT_DIR/build"
        BUILD_FLAGS=$(bash "$ROOT_DIR/scripts/build-ldflags.sh")
        run go -C "$ROOT_DIR" build -trimpath -ldflags "$BUILD_FLAGS" -o "$SRC_PROGRAM" ./cmd/looper
    fi
    warn_live_destination "$BIN_DIR/looper"
    run install -d "$BIN_DIR"
    run install -m 0755 "$SRC_PROGRAM" "$BIN_DIR/looper"
fi

if [ "$INSTALL_SKILLS" -eq 1 ]; then
    run mkdir -p "$SKILLS_DIR"
    selected_skills=(looper-diagnose looper-backlog)
    if [ "$WORKFLOW_SKILLS" -eq 1 ]; then
        selected_skills+=(git-commit github-release)
    fi
    for skill in "${selected_skills[@]}"; do
        if [ ! -d "$SRC_SKILLS/$skill" ]; then
            echo "Error: missing $SRC_SKILLS/$skill" >&2
            exit 1
        fi
        owned=0
        if [ -f "$SKILLS_RECEIPT" ] && grep -Fxq -- "$skill" "$SKILLS_RECEIPT"; then
            owned=1
        fi
        target="$SKILLS_DIR/$skill"
        if [ -L "$target" ] || { [ -e "$target" ] && { [ "$owned" -eq 0 ] || [ ! -d "$target" ]; }; }; then
            echo "Keeping existing skill: $target" >&2
            continue
        fi
        if [ "$owned" -eq 0 ]; then
            run record_skill "$skill"
        fi
        run cp -a "$SRC_SKILLS/$skill" "$SKILLS_DIR/"
    done
fi

if [ "$INSTALL_BIN" -eq 1 ]; then
    case ":$PATH:" in
        *":$BIN_DIR:"*) ;;
        *)
            echo "Note: $BIN_DIR is not on PATH. Add it to your shell profile." >&2
            ;;
    esac
fi

warn_missing git
warn_missing bash
warn_missing codex

echo "Install complete."
if [ "$INSTALL_BIN" -eq 1 ]; then
    echo "  looper -> $BIN_DIR/looper"
fi
if [ "$INSTALL_SKILLS" -eq 1 ]; then
    echo "  skills -> $SKILLS_DIR"
fi
