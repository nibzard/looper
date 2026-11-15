#!/usr/bin/env python3
"""Record the README figures from a real run with a stub agent.

The script builds a small project in a temporary directory, starts
`looper tui` in a private tmux server, and captures the pane. A stub
`claude` command plans, implements, and reviews five tasks. One task
reports a blocker. No real agent runs and no network is used.

Usage:
    make build
    python3 scripts/record-demo.py            # writes docs/assets/
    python3 scripts/record-demo.py --out DIR

Needs tmux and Pillow.
"""

import argparse
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tempfile
import time

from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parents[1]
COLS, ROWS = 120, 26
SOCKET = "looper-demo-%d" % os.getpid()
FONT = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"
FONT_BOLD = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono-Bold.ttf"
FONT_SIZE = 24
PAD = 16
BACKGROUND = (0x2E, 0x34, 0x40)
FOREGROUND = (0xD8, 0xDE, 0xE9)
# Nord-like ANSI colors for indexed SGR codes 0-15.
ANSI = [(0x3B, 0x42, 0x52), (0xBF, 0x61, 0x6A), (0xA3, 0xBE, 0x8C), (0xEB, 0xCB, 0x8B),
        (0x81, 0xA1, 0xC1), (0xB4, 0x8E, 0xAD), (0x88, 0xC0, 0xD0), (0xE5, 0xE9, 0xF0),
        (0x4C, 0x56, 0x6A), (0xBF, 0x61, 0x6A), (0xA3, 0xBE, 0x8C), (0xEB, 0xCB, 0x8B),
        (0x81, 0xA1, 0xC1), (0xB4, 0x8E, 0xAD), (0x8F, 0xBC, 0xBB), (0xEC, 0xEF, 0xF4)]

GOAL = "Ship jobq: a durable job queue with an HTTP API and a CLI"
TASKS = [
    ("T001", "Set up the module layout and test runner", [], "Created the package layout and the unittest runner."),
    ("T002", "Implement the durable job queue", ["T001"], "Added the SQLite-backed queue with visibility timeouts."),
    ("T003", "Add the HTTP API", ["T001"], "Added enqueue, claim, and ack endpoints with tests."),
    ("T004", "Run the retry tests against Redis", ["T002"], "Wrote the retry tests; they need a Redis server."),
    ("T005", "Add the jobq command-line client", ["T003"], "Added the jobq client with submit and status commands."),
]

STUB = r'''#!/usr/bin/env python3
import json, os, pathlib, sys, time
prompt = sys.stdin.read()
cwd = pathlib.Path.cwd()
todo = cwd / "to-do.json"
tasks = json.loads(todo.read_text())["tasks"] if todo.exists() else []
info = json.loads(os.environ["DEMO_TASKS"])
pace = float(os.environ.get("DEMO_PACE", "1"))
if "Plan execution for Looper" in prompt:
    time.sleep(2 * pace)
    result = {"setup": "", "verify": "python3 -m unittest discover -q -s tests", "tasks": [
        {"task_id": t["id"], "write_paths": ["src/" + t["id"].lower() + ".py", "tests/test_" + t["id"].lower() + ".py"],
         "resources": []} for t in tasks if t["status"] != "done"]}
elif "review pass for Looper" in prompt:
    time.sleep(2 * pace)
    result = {"status": "reviewed", "summary": "The queue, API, and CLI work end to end.",
              "findings": [], "complete": "final review pass" in prompt}
else:
    task = next(t for t in tasks if "Complete task " + t["id"] + ":" in prompt)
    tid = task["id"]
    delay, summary, blocker = info[tid]
    time.sleep(delay * pace)
    source = cwd / "src" / (tid.lower() + ".py")
    test = cwd / "tests" / ("test_" + tid.lower() + ".py")
    for path in (source, test):
        path.parent.mkdir(exist_ok=True)
    source.write_text("# " + task["title"] + "\n")
    test.write_text("import unittest\n\nclass Test(unittest.TestCase):\n    def test_ok(self):\n        pass\n")
    result = {"task_id": tid, "status": "blocked" if blocker else "done", "summary": summary,
              "files": [str(source.relative_to(cwd)), str(test.relative_to(cwd))],
              "blockers": [blocker] if blocker else []}
    if blocker:
        result["block_reason"] = {"class": "operator", "code": "operator_prerequisite", "retry": "after-change"}
print(json.dumps({"type": "result", "structured_output": result, "result": json.dumps(result)}))
'''


def tmux(*args, capture=False):
    command = ["tmux", "-L", SOCKET, *args]
    if capture:
        return subprocess.run(command, check=True, capture_output=True, text=True).stdout
    subprocess.run(command, check=True)


def prepare(work, pace):
    """Create the project, the stub agent, and the shell environment."""
    project = work / "jobq"
    project.mkdir()
    git = lambda *a: subprocess.run(["git", "-C", str(project), *a], check=True, capture_output=True)
    git("init", "-q")
    git("config", "user.name", "Demo")
    git("config", "user.email", "demo@example.invalid")
    (project / "PROJECT.md").write_text("# jobq\n\nA small job queue with an HTTP API and a command-line client.\n")
    (project / ".gitignore").write_text("to-do.json\n__pycache__/\n")
    (project / "to-do.json").write_text(json.dumps({
        "schema_version": 1, "goal": GOAL, "source_files": ["PROJECT.md"],
        "tasks": [{"id": i, "title": t, "priority": 1, "status": "todo", "depends_on": d}
                  for i, t, d, _ in TASKS]}, indent=2))
    git("add", ".")
    git("commit", "-qm", "Initial project")

    bin_dir = work / "bin"
    bin_dir.mkdir()
    (bin_dir / "looper").symlink_to(ROOT / "build" / "looper")
    stub = bin_dir / "claude"
    stub.write_text(STUB)
    stub.chmod(0o755)
    rc = work / "rc"
    rc.write_text("PS1='\\[\\e[2m\\]~/jobq\\[\\e[0m\\] $ '\ncd %s\nclear\n" % project)

    delays = {"T001": 2, "T002": 4, "T003": 3, "T004": 2, "T005": 2}
    info = {i: [delays[i], s, "Redis is not installed on this machine" if i == "T004" else ""]
            for i, _, _, s in TASKS}
    home = work / "home"
    home.mkdir()
    (home / ".sudo_as_admin_successful").touch()  # silence the Ubuntu sudo hint
    env = {
        "PATH": "%s:/usr/bin:/bin" % bin_dir, "HOME": str(home), "TERM": "xterm-256color",
        "COLORTERM": "truecolor", "LANG": "C.UTF-8", "XDG_CONFIG_HOME": str(home / ".config"),
        "LOOPER_BASE_DIR": str(home / ".looper"), "CLAUDE_BIN": str(stub),
        "LOOPER_NO_PROMPT": "1", "LOOPER_JOBS": "2", "LOOPER_REVIEW_EVERY": "0",
        "DEMO_PACE": str(pace), "DEMO_TASKS": json.dumps(info),
    }
    shell = "env -i %s bash --noprofile --rcfile %s -i" % (
        " ".join("%s=%s" % (k, shlex.quote(v)) for k, v in env.items()), rc)
    return shell


class Recorder:
    """Capture the tmux pane as ANSI text, one frame per change."""

    def __init__(self):
        self.frames = []  # [text, seconds shown]
        self.last = time.monotonic()

    def capture(self):
        text = tmux("capture-pane", "-p", "-e", "-t", "demo", capture=True)
        now = time.monotonic()
        if self.frames:
            self.frames[-1][1] += now - self.last
        self.last = now
        if not self.frames or self.frames[-1][0] != text:
            self.frames.append([text, 0.0])
        return text

    def hold(self, seconds, interval=0.2):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            time.sleep(interval)
            self.capture()

    def until(self, pattern, timeout=60):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            time.sleep(0.2)
            if re.search(pattern, strip(self.capture())):
                return
        raise SystemExit("timed out waiting for %r" % pattern)

    def keys(self, *keys, literal=False):
        tmux("send-keys", "-t", "demo", *(["-l"] if literal else []), *keys)


def strip(text):
    return re.sub(r"\x1b\[[0-9;:]*m", "", text)


def parse(text):
    """Turn one captured pane into rows of (char, fg, bg, bold) cells."""
    rows = []
    fg, bg, bold, faint, reverse = FOREGROUND, BACKGROUND, False, False, False
    for line in text.split("\n")[:ROWS]:
        cells = []
        for token in re.split(r"(\x1b\[[0-9;:]*m)", line):
            match = re.fullmatch(r"\x1b\[([0-9;:]*)m", token)
            if not match:
                for char in token:
                    f, b = (bg, fg) if reverse else (fg, bg)
                    if faint and not reverse:
                        f = tuple((x + y) // 2 for x, y in zip(f, BACKGROUND))
                    cells.append((char, f, b, bold))
                continue
            codes = [int(c) if c else 0 for c in match.group(1).replace(":", ";").split(";")]
            i = 0
            while i < len(codes):
                c = codes[i]
                if c == 0:
                    fg, bg, bold, faint, reverse = FOREGROUND, BACKGROUND, False, False, False
                elif c == 1:
                    bold = True
                elif c == 2:
                    faint = True
                elif c == 7:
                    reverse = True
                elif c == 22:
                    bold = faint = False
                elif c == 27:
                    reverse = False
                elif c in (38, 48) and i + 1 < len(codes):
                    if codes[i + 1] == 2:
                        color = tuple(codes[i + 2:i + 5])
                        i += 4
                    else:
                        color = xterm(codes[i + 2])
                        i += 2
                    if c == 38:
                        fg = color
                    else:
                        bg = color
                elif c == 39:
                    fg = FOREGROUND
                elif c == 49:
                    bg = BACKGROUND
                elif 30 <= c <= 37:
                    fg = ANSI[c - 30]
                elif 40 <= c <= 47:
                    bg = ANSI[c - 40]
                elif 90 <= c <= 97:
                    fg = ANSI[c - 82]
                elif 100 <= c <= 107:
                    bg = ANSI[c - 92]
                i += 1
        rows.append(cells)
    return rows


def xterm(n):
    if n < 16:
        return ANSI[n]
    if n < 232:
        n -= 16
        level = lambda v: 0 if v == 0 else 55 + v * 40
        return (level(n // 36), level(n // 6 % 6), level(n % 6))
    gray = 8 + (n - 232) * 10
    return (gray, gray, gray)


def render(text, fonts, size):
    regular, heavy = fonts
    width, height = size
    image = Image.new("RGB", (PAD * 2 + COLS * width, PAD * 2 + ROWS * height), BACKGROUND)
    draw = ImageDraw.Draw(image)
    for y, cells in enumerate(parse(text)):
        for x, (char, fg, bg, bold) in enumerate(cells):
            left, top = PAD + x * width, PAD + y * height
            if bg != BACKGROUND:
                draw.rectangle([left, top, left + width - 1, top + height - 1], fill=bg)
            if char != " ":
                draw.text((left, top), char, font=heavy if bold else regular, fill=fg)
    return image


def record(out, pace):
    work = Path(tempfile.mkdtemp(prefix="looper-demo-"))
    try:
        shell = prepare(work, pace)
        tmux("-f", "/dev/null", "new-session", "-d", "-s", "demo", "-x", str(COLS), "-y", str(ROWS), shell)
        size = tmux("display-message", "-p", "-t", "demo", "#{pane_width}x#{pane_height}", capture=True).strip()
        if size != "%dx%d" % (COLS, ROWS):
            raise SystemExit("pane is %s, not %dx%d" % (size, COLS, ROWS))
        rec = Recorder()
        rec.hold(1.0)
        for char in "looper tui":
            rec.keys(char, literal=True)
            rec.hold(0.09, 0.09)
        rec.hold(0.6)
        rec.keys("Enter")
        rec.until(r"Waiting for blocker resolution", timeout=120)
        rec.hold(2.5)
        still = rec.capture()
        rec.keys("u")
        rec.hold(0.6)
        rec.keys("k")
        rec.hold(1.0)
        frames = rec.frames
        rec.keys("q")
        time.sleep(0.5)
        rec.keys("c")
        time.sleep(3)
    finally:
        subprocess.run(["tmux", "-L", SOCKET, "kill-server"], capture_output=True)
        shutil.rmtree(work, ignore_errors=True)

    fonts = (ImageFont.truetype(FONT, FONT_SIZE), ImageFont.truetype(FONT_BOLD, FONT_SIZE))
    size = (round(fonts[0].getlength("M")), round(FONT_SIZE * 1.3))
    images, durations = [], []
    for text, seconds in frames:
        images.append(render(text, fonts, size))
        durations.append(max(40, int(seconds * 1000)))
    durations[-1] = 4000
    out.mkdir(parents=True, exist_ok=True)
    images[0].save(out / "loop.gif", save_all=True, append_images=images[1:],
                   duration=durations, loop=0, optimize=True)
    render(still, fonts, size).save(out / "tui.png", optimize=True)
    print("wrote %s (%d frames, %.1f s) and %s" % (
        out / "loop.gif", len(images), sum(durations) / 1000, out / "tui.png"))


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("--out", type=Path, default=ROOT / "docs" / "assets")
    parser.add_argument("--pace", type=float, default=1.0, help="multiply stub agent delays")
    args = parser.parse_args()
    if not (ROOT / "build" / "looper").exists():
        raise SystemExit("build/looper is missing; run make build first")
    record(args.out, args.pace)


if __name__ == "__main__":
    main()
