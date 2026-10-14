#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'USAGE'
Usage: release.sh --version <X.Y.Z|vX.Y.Z> [options]

Options:
  --version <vX.Y.Z>       Required release version (tag will be vX.Y.Z)
  --notes <text>           Release notes (default: "Release vX.Y.Z")
  --test-cmd <cmd>         Required unless --skip-tests is specified
  --bump-cmd <cmd>         Command to bump version (optional)
  --version-file <path>    File to overwrite with version (repeatable)
  --commit-msg <msg>       Commit message for release changes
  --notes-file <path>      File containing release notes
  --stage-path <path>      Release path to stage (repeatable)
  --formula <path>         Homebrew formula file to update (optional)
  --repo <owner/repo>      GitHub repo slug for tarball URL (optional)
  --skip-tests             Skip running tests
  --skip-release           Skip GitHub release creation
  --skip-formula           Skip formula update
  --allow-dirty            Require --stage-path and an empty index
  --dry-run                Print commands without executing
  -h, --help               Show this help
USAGE
}

VERSION=""
NOTES=""
NOTES_FILE=""
TEST_CMD=""
BUMP_CMD=""
COMMIT_MSG=""
FORMULA_PATH=""
REPO_SLUG=""
SKIP_TESTS=0
SKIP_RELEASE=0
SKIP_FORMULA=0
ALLOW_DIRTY=0
DRY_RUN=0
VERSION_FILES=()
STAGE_PATHS=()
TEMP_FILES=()
RELEASE_PHASE='preflight'

fail() {
    echo "Error: $*" >&2
    exit 1
}

require_value() {
    [ -n "$2" ] || fail "$1 requires a value."
}

cleanup() {
    local status=$?
    if [ "${#TEMP_FILES[@]}" -gt 0 ]; then
        rm -f -- "${TEMP_FILES[@]}"
    fi
    if [ "$status" -ne 0 ]; then
        printf 'Release stopped during %s (exit %s). Inspect local and remote state before retrying.\n' "$RELEASE_PHASE" "$status" >&2
    fi
    return "$status"
}
trap cleanup EXIT

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version)
            require_value "$1" "${2:-}"
            VERSION="$2"
            shift 2
            ;;
        --notes)
            require_value "$1" "${2:-}"
            NOTES="$2"
            shift 2
            ;;
        --test-cmd)
            require_value "$1" "${2:-}"
            TEST_CMD="$2"
            shift 2
            ;;
        --bump-cmd)
            require_value "$1" "${2:-}"
            BUMP_CMD="$2"
            shift 2
            ;;
        --version-file)
            require_value "$1" "${2:-}"
            VERSION_FILES+=("$2")
            shift 2
            ;;
        --commit-msg)
            require_value "$1" "${2:-}"
            COMMIT_MSG="$2"
            shift 2
            ;;
        --formula)
            require_value "$1" "${2:-}"
            FORMULA_PATH="$2"
            shift 2
            ;;
        --repo)
            require_value "$1" "${2:-}"
            REPO_SLUG="$2"
            shift 2
            ;;
        --notes-file)
            require_value "$1" "${2:-}"
            NOTES_FILE="$2"
            shift 2
            ;;
        --stage-path)
            require_value "$1" "${2:-}"
            STAGE_PATHS+=("$2")
            shift 2
            ;;
        --skip-tests)
            SKIP_TESTS=1
            shift
            ;;
        --skip-release)
            SKIP_RELEASE=1
            shift
            ;;
        --skip-formula)
            SKIP_FORMULA=1
            shift
            ;;
        --allow-dirty)
            ALLOW_DIRTY=1
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

if [ -z "$VERSION" ]; then
    echo "Error: --version is required." >&2
    usage
    exit 1
fi

TAG_VERSION="$VERSION"
if [ "${TAG_VERSION#v}" = "$TAG_VERSION" ]; then
    TAG_VERSION="v$TAG_VERSION"
fi
RAW_VERSION="${TAG_VERSION#v}"
if ! [[ "$RAW_VERSION" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]]; then
    fail "invalid release version: $VERSION"
fi

if [ "$SKIP_TESTS" -eq 0 ] && [ -z "$TEST_CMD" ]; then
    fail "provide --test-cmd or explicitly use --skip-tests."
fi
if [ -n "$NOTES_FILE" ]; then
    [ -z "$NOTES" ] || fail "use either --notes or --notes-file."
    [ -f "$NOTES_FILE" ] || fail "release notes file not found: $NOTES_FILE"
fi

run() {
    if [ "$DRY_RUN" -eq 1 ]; then
        printf 'dry-run:'
        printf ' %q' "$@"
        printf '\n'
    else
        "$@"
    fi
}

run_cmd() {
    local cmd="$1"
    if [ -z "$cmd" ]; then
        return 0
    fi
    if [ "$DRY_RUN" -eq 1 ]; then
        printf 'dry-run: %s\n' "$cmd"
    else
        bash -lc "$cmd"
    fi
}

ensure_clean() {
    if [ "$ALLOW_DIRTY" -eq 1 ]; then
        [ "${#STAGE_PATHS[@]}" -gt 0 ] || fail "--allow-dirty requires --stage-path."
        git diff --cached --quiet || fail "--allow-dirty requires an empty index."
        return 0
    fi
    if [ -n "$(git status --porcelain)" ]; then
        echo "Error: working tree is dirty (including untracked files). Commit or stash changes." >&2
        exit 1
    fi
}

resolve_branch() {
    git rev-parse --abbrev-ref HEAD
}

resolve_repo_slug() {
    if [ -n "$REPO_SLUG" ]; then
        echo "$REPO_SLUG"
        return 0
    fi

    if [ "$DRY_RUN" -ne 1 ] && command -v gh >/dev/null 2>&1; then
        local slug
        slug=$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null) || true
        if [ -n "$slug" ]; then
            echo "$slug"
            return 0
        fi
    fi

    local origin
    origin=$(git remote get-url origin 2>/dev/null || true)
    if [ -z "$origin" ]; then
        echo ""
        return 0
    fi
    echo "$origin" | sed -E 's#(git@github.com:|https://github.com/)##; s#\.git$##'
}

sha256_stream() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum | awk '{print $1}' || return 1
        return 0
    fi

    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 | awk '{print $1}' || return 1
        return 0
    fi

    if command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 | awk '{print $NF}' || return 1
        return 0
    fi

    echo "Error: no sha256 tool found (sha256sum/shasum/openssl)." >&2
    return 1
}

update_version_files() {
    if [ ${#VERSION_FILES[@]} -eq 0 ]; then
        return 0
    fi
    local file
    for file in "${VERSION_FILES[@]}"; do
        if [ ! -f "$file" ]; then
            echo "Error: version file not found: $file" >&2
            exit 1
        fi
        if [ "$DRY_RUN" -eq 1 ]; then
            printf 'dry-run: set %s to %s\n' "$file" "$RAW_VERSION"
        else
            printf '%s\n' "$RAW_VERSION" > "$file"
        fi
    done
}

resolve_formula() {
    if [ "$SKIP_FORMULA" -eq 1 ]; then
        return 0
    fi

    local formula="$FORMULA_PATH"
    if [ -z "$formula" ]; then
        if [ -d "Formula" ]; then
            local candidates=() candidate
            for candidate in Formula/*.rb; do
                [ ! -f "$candidate" ] || candidates+=("$candidate")
            done
            if [ "${#candidates[@]}" -eq 1 ]; then
                formula="${candidates[0]}"
            elif [ "${#candidates[@]}" -gt 1 ]; then
                fail "multiple formulas found; use --formula or --skip-formula."
            fi
        fi
    fi

    if [ -z "$formula" ]; then
        return 0
    fi
    [ -f "$formula" ] || fail "formula not found: $formula"
    FORMULA_PATH="$formula"
}

preflight() {
    command -v git >/dev/null 2>&1 || fail "git is required."
    git rev-parse --is-inside-work-tree >/dev/null 2>&1 || fail "run from a Git repository."
    [ "$(pwd -P)" = "$(git rev-parse --show-toplevel)" ] || fail "run from the repository root."
    local branch
    branch=$(resolve_branch)
    [ -n "$branch" ] && [ "$branch" != HEAD ] || fail "select a branch before releasing."
    git remote get-url origin >/dev/null 2>&1 || fail "origin remote is required."
    if git rev-parse --verify "refs/tags/$TAG_VERSION" >/dev/null 2>&1; then
        fail "tag $TAG_VERSION already exists; inspect the previous release before retrying."
    fi
    ensure_clean
    local file
    for file in "${VERSION_FILES[@]}"; do
        [ -f "$file" ] || fail "version file not found: $file"
    done
    if [ "$SKIP_RELEASE" -eq 0 ]; then
        command -v gh >/dev/null 2>&1 || fail "gh is required; use --skip-release to omit publication."
        if [ "$DRY_RUN" -ne 1 ]; then
            gh auth status >/dev/null 2>&1 || fail "GitHub authentication failed."
        fi
    fi
    resolve_formula
    if [ "$SKIP_FORMULA" -eq 0 ] && [ -n "$FORMULA_PATH" ]; then
        if [ -n "$REPO_SLUG" ]; then
            [[ "$REPO_SLUG" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || fail "invalid GitHub repository: $REPO_SLUG"
        fi
        git diff --quiet -- "$FORMULA_PATH" || fail "formula has existing edits; commit them or use --skip-formula."
        awk '$1 == "url" {urls++} $1 == "sha256" {hashes++}
             END {exit !(urls == 1 && hashes == 1)}' "$FORMULA_PATH" \
            || fail "the helper requires a formula with one url and one sha256; update complex formulas manually."
        if [ "$DRY_RUN" -ne 1 ]; then
            command -v curl >/dev/null 2>&1 || fail "curl is required for the formula update."
            command -v tar >/dev/null 2>&1 || fail "tar is required for archive validation."
            if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1 && ! command -v openssl >/dev/null 2>&1; then
                fail "a SHA-256 command is required for the formula update."
            fi
        fi
    fi
}

update_formula() {
    [ "$SKIP_FORMULA" -eq 0 ] && [ -n "$FORMULA_PATH" ] || return 0
    local formula="$FORMULA_PATH"
    if [ "$DRY_RUN" -eq 1 ]; then
        printf 'dry-run: download and verify archive; update %s for %s\n' "$formula" "$TAG_VERSION"
        run git add -- "$formula"
        run git commit -m "Chore(formula): Update $TAG_VERSION"
        run git push origin "$(resolve_branch)"
        return 0
    fi

    local slug sha
    slug=$(resolve_repo_slug)
    if [ -z "$slug" ]; then
        echo "Error: unable to determine repo slug for formula update." >&2
        exit 1
    fi

    [[ "$slug" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || fail "invalid GitHub repository: $slug"
    local archive
    archive=$(mktemp)
    TEMP_FILES+=("$archive")
    curl --fail --location --silent --show-error --connect-timeout 15 --max-time 180 \
        --output "$archive" "https://github.com/$slug/archive/refs/tags/$TAG_VERSION.tar.gz" \
        || fail "archive download failed for $TAG_VERSION; the release may already be published."
    tar -tzf "$archive" >/dev/null 2>&1 || fail "download is not a valid gzip tar archive."
    sha=$(sha256_stream < "$archive") || fail "archive checksum failed."
    [[ "$sha" =~ ^[0-9a-fA-F]{64}$ ]] || fail "invalid SHA-256 checksum."

    local tmp
    tmp=$(mktemp "${formula}.XXXXXX")
    TEMP_FILES+=("$tmp")
    cp -p -- "$formula" "$tmp"
    awk -v tag="$TAG_VERSION" -v slug="$slug" -v sha="$sha" '
        $1 == "url" {print "  url \"https://github.com/" slug "/archive/refs/tags/" tag ".tar.gz\""; next}
        $1 == "sha256" {print "  sha256 \"" sha "\""; next}
        {print}
    ' "$formula" > "$tmp" || fail "could not write the formula update."
    mv -- "$tmp" "$formula" || fail "could not replace the formula."

    git diff --cached --quiet || fail "the index contains unrelated changes before the formula commit."
    git add -- "$formula"
    if git diff --cached --quiet; then
        echo "Formula unchanged; skipping formula commit." >&2
        return 0
    fi

    git commit -m "Chore(formula): Update $TAG_VERSION" -m "Use the published archive and its verified checksum."
    git push origin "$(resolve_branch)"
}

preflight

RELEASE_PHASE='version update'
if [ -n "$BUMP_CMD" ]; then
    run_cmd "$BUMP_CMD"
fi

update_version_files

RELEASE_PHASE='verification'
if [ "$SKIP_TESTS" -eq 0 ]; then
    run_cmd "$TEST_CMD"
else
    echo "Tests skipped by explicit --skip-tests." >&2
fi

RELEASE_PHASE='release commit'
if [ -n "$(git status --porcelain)" ] || { [ "$DRY_RUN" -eq 1 ] && { [ -n "$BUMP_CMD" ] || [ "${#VERSION_FILES[@]}" -gt 0 ]; }; }; then
    if [ "${#STAGE_PATHS[@]}" -gt 0 ]; then
        run git add -- "${STAGE_PATHS[@]}"
    else
        run git add -A
    fi
    if [ -z "$COMMIT_MSG" ]; then
        COMMIT_MSG="Chore(release): Prepare $TAG_VERSION"
    fi
    if [ "$DRY_RUN" -eq 1 ] || ! git diff --cached --quiet; then
        run git commit -m "$COMMIT_MSG" -m "Update release files for publication."
    fi
fi

if [ "$DRY_RUN" -ne 1 ] && [ "${#VERSION_FILES[@]}" -gt 0 ]; then
    git diff --quiet HEAD -- "${VERSION_FILES[@]}" \
        || fail "version files remain uncommitted; include them in --stage-path."
fi

RELEASE_PHASE='tag and push'
run git tag -a "$TAG_VERSION" -m "$TAG_VERSION"
run git push origin "$(resolve_branch)"
run git push origin "$TAG_VERSION"

if [ "$SKIP_RELEASE" -eq 0 ]; then
    RELEASE_PHASE='GitHub release'
    if [ -z "$NOTES_FILE" ]; then
        if [ -z "$NOTES" ]; then
            NOTES="Release $TAG_VERSION"
        fi
        if [ "$DRY_RUN" -eq 1 ]; then
            printf 'dry-run: write release notes to a temporary file\n'
            NOTES_FILE='<temporary release notes>'
        else
            NOTES_FILE=$(mktemp)
            TEMP_FILES+=("$NOTES_FILE")
            printf '%s\n' "$NOTES" > "$NOTES_FILE"
        fi
    fi
    run gh release create "$TAG_VERSION" --title "$TAG_VERSION" --notes-file "$NOTES_FILE"
    if [ "$DRY_RUN" -ne 1 ]; then
        gh release view "$TAG_VERSION" >/dev/null || fail "could not verify the created release."
    fi
fi

RELEASE_PHASE='formula update'
update_formula

if [ "$DRY_RUN" -eq 1 ]; then
    echo "Dry-run plan complete for $TAG_VERSION. No release was published."
else
    echo "Release workflow complete for $TAG_VERSION."
fi
