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
fi
printf '%s\n' "-X github.com/nibzard/looper/internal/looper.Version=$build_version -X github.com/nibzard/looper/internal/looper.BuildRevision=$build_revision -X github.com/nibzard/looper/internal/looper.BuildModified=$build_modified"
