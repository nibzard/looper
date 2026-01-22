#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'USAGE'
Usage: release.sh --version <X.Y.Z|vX.Y.Z> [options]

Options:
  --version <vX.Y.Z>       Required release version (tag will be vX.Y.Z)
  --notes <text>           Release notes (default: "Release vX.Y.Z")
  --test-cmd <cmd>         Command to run tests (optional)
  --bump-cmd <cmd>         Command to bump version (optional)
  --version-file <path>    File to overwrite with version (repeatable)
  --commit-msg <msg>       Commit message for release changes
  --formula <path>         Homebrew formula file to update (optional)
  --repo <owner/repo>      GitHub repo slug for tarball URL (optional)
  --skip-tests             Skip running tests
  --skip-release           Skip GitHub release creation
  --skip-formula           Skip formula update
  --allow-dirty            Allow dirty working tree
  --dry-run                Print commands without executing
  -h, --help               Show this help
USAGE
}

VERSION=""
NOTES=""
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

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version)
            VERSION="$2"
            shift 2
            ;;
        --notes)
            NOTES="$2"
            shift 2
            ;;
        --test-cmd)
            TEST_CMD="$2"
            shift 2
            ;;
        --bump-cmd)
            BUMP_CMD="$2"
            shift 2
            ;;
        --version-file)
            VERSION_FILES+=("$2")
            shift 2
            ;;
        --commit-msg)
            COMMIT_MSG="$2"
            shift 2
            ;;
        --formula)
            FORMULA_PATH="$2"
            shift 2
            ;;
        --repo)
            REPO_SLUG="$2"
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

run() {
    if [ "$DRY_RUN" -eq 1 ]; then
        printf 'dry-run: %s\n' "$*"
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

    if command -v gh >/dev/null 2>&1; then
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
        echo "";
        return 0
    fi
    echo "$origin" | sed -E 's#(git@github.com:|https://github.com/)##; s#\.git$##'
}

sha256_stream() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum | awk '{print $1}'
        return 0
    fi

    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 | awk '{print $1}'
        return 0
    fi

    if command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 | awk '{print $NF}'
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

update_formula() {
    if [ "$SKIP_FORMULA" -eq 1 ]; then
        return 0
    fi

    local formula="$FORMULA_PATH"
    if [ -z "$formula" ]; then
        if [ -d "Formula" ]; then
            local count
            count=$(ls -1 Formula/*.rb 2>/dev/null | wc -l | tr -d ' ')
            if [ "$count" = "1" ]; then
                formula=$(ls -1 Formula/*.rb)
            fi
        fi
    fi

    if [ -z "$formula" ]; then
        echo "No formula found; skipping formula update." >&2
        return 0
    fi

    local slug sha
    slug=$(resolve_repo_slug)
    if [ -z "$slug" ]; then
        echo "Error: unable to determine repo slug for formula update." >&2
        exit 1
    fi

    sha=$(curl -L -s "https://github.com/$slug/archive/refs/tags/$TAG_VERSION.tar.gz" | sha256_stream)
    if [ -z "$sha" ]; then
        echo "Error: failed to compute sha256 for $TAG_VERSION" >&2
        exit 1
    fi

    if [ "$DRY_RUN" -eq 1 ]; then
        printf 'dry-run: update %s url+sha256 for %s\n' "$formula" "$TAG_VERSION"
        return 0
    fi

    local tmp
    tmp=$(mktemp)
    awk -v tag="$TAG_VERSION" -v slug="$slug" -v sha="$sha" '
        $1 == "url" {print "  url \"https://github.com/" slug "/archive/refs/tags/" tag ".tar.gz\""; next}
        $1 == "sha256" {print "  sha256 \"" sha "\""; next}
        {print}
    ' "$formula" > "$tmp" && mv "$tmp" "$formula"

    git add "$formula"
    if git diff --cached --quiet; then
        echo "Formula unchanged; skipping formula commit." >&2
        return 0
    fi

    git commit -m "chore(formula): bump to $TAG_VERSION"
    git push origin "$(resolve_branch)"
}

ensure_clean

if [ "$SKIP_TESTS" -eq 0 ] && [ -n "$TEST_CMD" ]; then
    run_cmd "$TEST_CMD"
fi

if [ -n "$BUMP_CMD" ]; then
    run_cmd "$BUMP_CMD"
fi

update_version_files

if ! git diff --quiet || ! git diff --cached --quiet; then
    run git add -A
    if [ -z "$COMMIT_MSG" ]; then
        COMMIT_MSG="chore(release): $TAG_VERSION"
    fi
    run git commit -m "$COMMIT_MSG"
fi

run git tag -a "$TAG_VERSION" -m "$TAG_VERSION"
run git push origin "$(resolve_branch)"
run git push origin "$TAG_VERSION"

if [ "$SKIP_RELEASE" -eq 0 ]; then
    if command -v gh >/dev/null 2>&1; then
        if [ -z "$NOTES" ]; then
            NOTES="Release $TAG_VERSION"
        fi
        run gh release create "$TAG_VERSION" --title "$TAG_VERSION" --notes "$NOTES"
    else
        echo "Warning: gh not found; skipping GitHub release." >&2
    fi
fi

update_formula

echo "Release workflow complete for $TAG_VERSION."
