#!/usr/bin/env python3
"""Check terminal rendering and timer cleanup with a local stub agent."""

import errno
import json
import os
from pathlib import Path
import pty
import re
import select
import signal
import subprocess
import tempfile
import time


LOOPER = Path(__file__).resolve().parents[1] / "bin" / "looper.sh"
STUB = """#!/usr/bin/env bash
sleep "${STUB_DELAY:-0}"
task_id=T1
if [[ "$*" == *'Complete task T2:'* ]]; then task_id=T2; fi
jq -cn --arg id "$task_id" '{structured_output:{task_id:$id,status:"blocked",
  summary:"The public labels need human review before the next step can start.",
  files:[],blockers:["A reviewer must approve the challenge labels."]}}'
"""


def run_case(overrides=None, *, terminal=True, interrupt=False):
    with tempfile.TemporaryDirectory(prefix="looper-terminal-") as temp:
        work = Path(temp)
        tasks = [
            {"id": task_id, "title": "Obtain human review of public challenge labels",
             "status": "todo", "priority": 1}
            for task_id in ("T1", "T2")
        ]
        (work / "to-do.json").write_text(json.dumps({
            "schema_version": 1, "source_files": [], "tasks": tasks,
        }))
        stub = work / "claude"
        stub.write_text(STUB)
        stub.chmod(0o755)
        env = dict(os.environ, CLAUDE_BIN=str(stub), CODEX_JSON_LOG="0",
                   CODEX_PROGRESS="1", LOOPER_VERBOSE="0", NO_COLOR="",
                   LOOPER_APPLY_SUMMARY="1", LOOPER_VERIFY_COMMAND="",
                   LOOPER_BASE_DIR=str(work / "logs"), LOOPER_HOOK="",
                   LOOP_DELAY_SECONDS="0", MAX_ITERATIONS="3",
                   TERM="xterm-256color", COLUMNS="80", STUB_DELAY="0")
        env.update(overrides or {})
        command = [str(LOOPER), "--all", "claude"]
        if not terminal:
            result = subprocess.run(command, cwd=work, env=env, capture_output=True,
                                    text=True, timeout=15)
            assert result.returncode == 2, result.stdout + result.stderr
            return result.stdout + result.stderr

        master, slave = pty.openpty()
        process = subprocess.Popen(command, cwd=work, env=env, stdout=slave,
                                   stderr=slave, start_new_session=True)
        os.close(slave)
        output = bytearray()
        deadline = time.monotonic() + 15
        interrupted = False
        try:
            while True:
                assert time.monotonic() < deadline, "A timer or agent kept the terminal open"
                readable, _, _ = select.select([master], [], [], 0.1)
                if not readable:
                    continue
                try:
                    chunk = os.read(master, 65536)
                except OSError as error:
                    if error.errno != errno.EIO:
                        raise
                    break
                if not chunk:
                    break
                output.extend(chunk)
                if interrupt and not interrupted and b"1s elapsed" in output:
                    os.killpg(process.pid, signal.SIGINT)
                    interrupted = True
            status = process.wait(timeout=3)
        finally:
            # Also clean up descendants if an assertion fails.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait(timeout=3)
            os.close(master)
        text = output.decode()
        assert status == (130 if interrupt else 2), text
        assert text.count("This run") == 1, text
        return text


plain = run_case(terminal=False)
assert "\x1b" not in plain and "\r" not in plain
assert "No runnable tasks remain" in plain

live = run_case({"STUB_DELAY": "2", "LOOP_DELAY_SECONDS": "1"})
assert "\x1b[1mLooper\x1b[0m" in live
assert "claude running | 1s elapsed" in live
assert "Next task in 1s" in live
assert "Next task in" not in live.split("[2/3]", 1)[1]
assert "This run  0 done | 2 blocked | 2 attempted" in live

no_color = run_case({"NO_COLOR": "1"})
assert not re.search(r"\x1b\[[0-9;]*m", no_color)
assert "\x1b[2K" in no_color  # NO_COLOR preserves the live line.

dumb = run_case({"TERM": "dumb"})
assert "\x1b" not in dumb

quiet = run_case({"CODEX_PROGRESS": "0"})
assert "\x1b[2K" not in quiet
assert "Blocked" in quiet and "A reviewer must approve" in quiet

narrow = run_case({"COLUMNS": "40", "NO_COLOR": "1", "CODEX_PROGRESS": "0"})
for line in narrow.splitlines():
    if line.startswith("    "):
        assert len(line) <= 40, line
assert "A reviewer must approve the" in narrow
assert "challenge labels." in narrow

interrupted = run_case({"STUB_DELAY": "10"}, interrupt=True)
assert "Interrupted" in interrupted
assert "0 done | 0 blocked | 1 attempted" in interrupted
assert "claude running" not in interrupted.split("Interrupted", 1)[1]

print("Terminal output checks passed.")
