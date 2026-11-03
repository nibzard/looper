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
        bump=$(git -C "$build_root" log -1 --format=%H -- VERSION 2>/dev/null || true)
        count=""
        if [ -n "$bump" ] && [ "$bump" != "$build_revision" ]; then
            count=$(git -C "$build_root" rev-list --count "$bump"..HEAD 2>/dev/null || true)
        fi
        if [[ $count =~ ^[1-9][0-9]*$ ]] && [[ $build_version =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
            patch=$(( 10#${BASH_REMATCH[3]} + 1 ))
            build_version="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.$patch-dev.$count"
        fi
    fi
fi
printf '%s\n' "-X github.com/nibzard/looper/internal/looper.Version=$build_version -X github.com/nibzard/looper/internal/looper.BuildRevision=$build_revision -X github.com/nibzard/looper/internal/looper.BuildModified=$build_modified"
