#!/usr/bin/env bash
set -euo pipefail

build_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
build_version=${1:-$(cat "$build_root/VERSION")}
build_revision=unknown
build_modified=false
# Do not identify an archive by an enclosing repository's revision.
if [ "$(git -C "$build_root" rev-parse --show-toplevel 2>/dev/null || true)" = "$build_root" ]; then
    build_revision=$(git -C "$build_root" rev-parse --verify HEAD)
    if [ -n "$(git -C "$build_root" status --porcelain)" ]; then
        build_modified=true
    fi
    # Without an explicit version, a build after the last VERSION bump
    # self-identifies with the next patch and the commits since the bump.
    if [ -z "${1:-}" ]; then
        shallow=false
        if [ "$(git -C "$build_root" rev-parse --is-shallow-repository 2>/dev/null || true)" = "true" ]; then
            shallow=true
        fi
        bump=$(git -C "$build_root" log -1 --format=%H -- VERSION 2>/dev/null || true)
        # A shallow boundary commit has no visible parent, so it seems to add
        # every file and can fake the last VERSION change. The boundary counts
        # as established only when its parent commit is present.
        boundary=false
        if [ -n "$bump" ]; then
            if [ "$shallow" = false ] || git -C "$build_root" rev-parse -q --verify "$bump^" >/dev/null 2>&1; then
                boundary=true
            fi
        fi
        if [[ $build_version =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
            patch=$(( 10#${BASH_REMATCH[3]} + 1 ))
            next_version="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.$patch"
            count=""
            if [ "$boundary" = true ] && [ "$bump" != "$build_revision" ]; then
                count=$(git -C "$build_root" rev-list --count "$bump"..HEAD 2>/dev/null || true)
            fi
            if [[ $count =~ ^[1-9][0-9]*$ ]]; then
                # Complete history: the commits since the bump are countable.
                build_version="$next_version-dev.$count"
            elif [ "$shallow" = true ] && [ "$boundary" = false ]; then
                # Incomplete history: name the revision instead of inventing
                # a commit distance. See docs/release.md for the format.
                build_version="$next_version-dev.g$(git -C "$build_root" rev-parse --short HEAD)"
            fi
        fi
    fi
fi
printf '%s\n' "-X github.com/nibzard/looper/internal/looper.Version=$build_version -X github.com/nibzard/looper/internal/looper.BuildRevision=$build_revision -X github.com/nibzard/looper/internal/looper.BuildModified=$build_modified"
