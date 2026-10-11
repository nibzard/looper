#!/usr/bin/env bash
set -euo pipefail
LOOPER_SOURCE=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
exec python3 "$LOOPER_SOURCE/scripts/runtime-test.py" "$@"
