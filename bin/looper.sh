#!/usr/bin/env bash
# Compatibility entry point. All execution belongs to the Go binary.
set -euo pipefail
LOOPER_BIN_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
if [ -x "$LOOPER_BIN_DIR/looper" ]; then
    exec "$LOOPER_BIN_DIR/looper" "$@"
fi
if [ -x "$LOOPER_BIN_DIR/../build/looper" ]; then
    exec "$LOOPER_BIN_DIR/../build/looper" "$@"
fi
echo 'Looper is not built. Run make build or ./install.sh.' >&2
exit 1
