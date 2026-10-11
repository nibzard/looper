#!/usr/bin/env bash
set -euo pipefail
LOOPER_SOURCE=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
python3 "$LOOPER_SOURCE/scripts/runtime-test.py" ParallelTests.test_parallel_and_exact_publication ParallelTests.test_claude_bridge
