#!/usr/bin/env python3
"""Worktree scheduling and integration. Uses only the Python standard library.

The shell owns agent-specific arguments. This module owns claims, processes,
verification, and publication. A task is done only after its verified tree is
published. The original branch and index are never used for worker commits.
"""

import concurrent.futures
import contextlib
import copy
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import signal
import subprocess
import sys
import tempfile
import threading
import time
import uuid


class RunError(Exception):
    pass


def read_json(path):
    return json.loads(Path(path).read_text())


def atomic_json(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix=path.name + ".", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as stream:
            json.dump(data, stream, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def git(cwd, *args, input=None, env=None, check=True):
    # Git environment overrides from an enclosing tool must not redirect a
    # worktree command to another repository or to the user's index.
    clean_env = dict(os.environ if env is None else env)
    for key in ("GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE"):
        if env is None or key != "GIT_INDEX_FILE":
            clean_env.pop(key, None)
    command = ["git", "-C", str(cwd), *args]
    try:
        result = subprocess.run(command, input=input, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, env=clean_env)
    except FileNotFoundError:
        result = subprocess.CompletedProcess(command, 127, b"", b"Git is unavailable.")
    if check and result.returncode:
        raise RunError(result.stderr.decode(errors="replace").strip())
    return result


def git_text(cwd, *args, **kwargs):
    return git(cwd, *args, **kwargs).stdout.decode().strip()


def guard(script, args):
    """An inherited advisory lock also protects the legacy sequential runner."""
    key = hashlib.sha256(str(Path.cwd()).encode()).hexdigest()[:16]
    directory = Path(tempfile.gettempdir()) / ("looper-lock-" + str(os.getuid()) + "-" + key)
    directory.mkdir(mode=0o700, exist_ok=True)
    directories = [directory]
    result = git(Path.cwd(), "rev-parse", "--git-common-dir", check=False)
    if result.returncode == 0:
        directories.append(Path(result.stdout.decode().strip()).resolve())
    descriptors = []
    for directory in directories:
        fd = os.open(directory / "looper.lock", os.O_CREAT | os.O_RDWR, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            os.close(fd)
            raise RunError("Looper is already running in this repository.")
        os.set_inheritable(fd, True)
        descriptors.append(str(fd))
    env = dict(os.environ, LOOPER_LOCK_HELD=",".join(descriptors))
    os.execvpe("bash", ["bash", str(Path(script).resolve()), *args], env)


def ensure_repository_lock():
    # Bootstrap may have created the repository after the outer guard started.
    common = Path(git_text(Path.cwd(), "rev-parse", "--git-common-dir")).resolve()
    fd = os.open(common / "looper.lock", os.O_CREAT | os.O_RDWR, 0o600)
    identity = os.fstat(fd)
    for inherited in os.environ.get("LOOPER_LOCK_HELD", "").split(","):
        with contextlib.suppress(ValueError, OSError):
            existing = os.fstat(int(inherited))
            if (identity.st_dev, identity.st_ino) == (existing.st_dev, existing.st_ino):
                os.close(fd)
                return
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        os.close(fd)
        raise RunError("Looper is already running in this repository.")
    # Keep the descriptor open until this coordinator exits.


def validate_backlog(data):
    if not isinstance(data, dict) or data.get("schema_version") != 1:
        raise RunError("Invalid backlog version.")
    if not isinstance(data.get("source_files"), list) or not all(isinstance(x, str) for x in data["source_files"]):
        raise RunError("Invalid source_files.")
    tasks = data.get("tasks")
    if not isinstance(tasks, list):
        raise RunError("Invalid tasks.")
    ids = set()
    for task in tasks:
        if (not isinstance(task, dict) or not isinstance(task.get("id"), str) or not task["id"]
                or task["id"] in ids or not isinstance(task.get("title"), str) or not task["title"]
                or type(task.get("priority")) is not int or not 1 <= task["priority"] <= 5
                or task.get("status") not in ("todo", "doing", "done", "blocked")):
            raise RunError("Invalid or duplicate task.")
        ids.add(task["id"])
        for key in ("depends_on", "files", "blockers", "tags", "steps"):
            if key in task and (not isinstance(task[key], list) or not all(isinstance(x, str) for x in task[key])):
                raise RunError("Invalid task field: " + key)
    by_id = {t["id"]: t for t in tasks}
    visiting, visited = set(), set()

    def visit(task_id):
        if task_id in visiting:
            raise RunError("Task dependencies contain a cycle.")
        if task_id in visited:
            return
        visiting.add(task_id)
        for dep in by_id[task_id].get("depends_on", []):
            if dep not in ids:
                raise RunError("Unknown dependency: " + dep)
            visit(dep)
        visiting.remove(task_id)
        visited.add(task_id)

    for task_id in ids:
        visit(task_id)
    return data


def valid_summary(data, task_id):
    return (isinstance(data, dict) and data.get("task_id") == task_id
            and data.get("status") in ("done", "blocked")
            and isinstance(data.get("summary"), str)
            and all(isinstance(data.get(k), list) and all(isinstance(x, str) for x in data[k])
                    for k in ("files", "blockers"))
            and (data["status"] != "blocked" or bool(data["blockers"])))


def id_key(value):
    match = re.fullmatch(r"([A-Za-z_-]*)([0-9]+)", value)
    return (match[1], int(match[2]), value) if match else (value, -1, value)


def ranked_ready(tasks):
    """Respect priority first, then favor the longest remaining dependency chain."""
    by_id = {t["id"]: t for t in tasks}
    successors = {t["id"]: [] for t in tasks}
    for task in tasks:
        if task["status"] != "done":
            for dep in task.get("depends_on", []):
                successors[dep].append(task["id"])
    lengths = {}

    def length(task_id):
        if task_id not in lengths:
            lengths[task_id] = 1 + max((length(x) for x in successors[task_id]), default=0)
        return lengths[task_id]

    ready = [t for t in tasks if t["status"] in ("todo", "doing")
             and all(by_id[d]["status"] == "done" for d in t.get("depends_on", []))]
    return sorted(ready, key=lambda t: (t["status"] != "doing", t["priority"], -length(t["id"]), id_key(t["id"])))


def scopes_overlap(left, right):
    # The planner supplies path prefixes, not an arbitrary glob language.
    return any(a == "." or b == "." or a == b or a.startswith(b + "/") or b.startswith(a + "/")
               for a in left for b in right)


def available_memory():
    """Use a conservative bound when Linux reports memory or container limits."""
    bounds = []
    with contextlib.suppress(OSError, ValueError):
        lines = Path("/proc/meminfo").read_text().splitlines()
        bounds.append(next(int(line.split()[1]) * 1024 for line in lines if line.startswith("MemAvailable:")))
    with contextlib.suppress(OSError, ValueError):
        maximum = int(Path("/sys/fs/cgroup/memory.max").read_text())
        current = int(Path("/sys/fs/cgroup/memory.current").read_text())
        bounds.append(max(0, maximum - current))
    return min(bounds) if bounds else None


def normalize_plan(data, tasks, verify_override=""):
    """Unplanned tasks remain exclusive. Never invent executable checks."""
    if not isinstance(data, dict) or not isinstance(data.get("tasks"), list):
        raise RunError("Planner did not return a task plan.")
    for key in ("setup", "verify"):
        if not isinstance(data.get(key), str):
            raise RunError("Planner omitted " + key)
    result = {"setup": data["setup"], "verify": verify_override or data["verify"], "tasks": {}}
    if not result["verify"].strip():
        raise RunError("Planner did not identify an integration check.")
    ids = {t["id"] for t in tasks}
    for entry in data["tasks"]:
        if (not isinstance(entry, dict) or not isinstance(entry.get("task_id"), str)
                or entry["task_id"] not in ids or entry["task_id"] in result["tasks"]):
            raise RunError("Planner returned an unknown or duplicate task.")
        paths = entry.get("write_paths", [])
        resources = entry.get("resources", [])
        if not isinstance(paths, list) or not isinstance(resources, list) or not all(isinstance(x, str) for x in paths + resources):
            raise RunError("Invalid plan scope.")
        for path in paths:
            if not path or PurePosixPath(path).is_absolute() or ".." in PurePosixPath(path).parts or any(x in path for x in "*?["):
                raise RunError("Plan paths must be relative file or directory names.")
        result["tasks"][entry["task_id"]] = {
            "write_paths": sorted({str(PurePosixPath(p)) for p in paths}) or ["."],
            "resources": sorted(set(resources)),
        }
    return result


def process_identity(pid):
    """Check start time as well as PID to avoid signaling a reused PID."""
    result = subprocess.run(["ps", "-o", "lstart=", "-p", str(pid)], capture_output=True, text=True)
    return result.stdout.strip() if result.returncode == 0 else ""


def reap_processes(directory):
    for path in directory.rglob("*.process.json"):
        record = read_json(path)
        pid = record["pid"]
        if record["identity"] and process_identity(pid) == record["identity"]:
            with contextlib.suppress(ProcessLookupError):
                if os.getpgid(pid) == pid:
                    os.killpg(pid, signal.SIGTERM)
                    deadline = time.monotonic() + 2
                    while time.monotonic() < deadline and process_identity(pid) == record["identity"]:
                        time.sleep(.05)
                    if process_identity(pid) == record["identity"]:
                        os.killpg(pid, signal.SIGKILL)
        path.unlink()


class Processes:
    def __init__(self):
        self.stop = threading.Event()
        self.lock = threading.RLock()
        self.children = set()

    def run(self, command, cwd, logfile, env=None, input=None):
        if self.stop.is_set():
            raise RunError("Run interrupted.")
        with open(logfile, "ab") as output:
            env = dict(os.environ if env is None else env)
            for key in ("GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE"):
                env.pop(key, None)
            with self.lock:
                if self.stop.is_set():
                    raise RunError("Run interrupted.")
                # The child records its own identity before executing any project
                # command. Recovery can stop it even after coordinator SIGKILL.
                lease = str(logfile) + ".process.json"
                wrapped = [sys.executable, str(Path(__file__).resolve()), "child", lease, str(os.getpid()), *command]
                process = subprocess.Popen(wrapped, cwd=cwd, env=env, stdin=subprocess.PIPE,
                                           stdout=output, stderr=output, start_new_session=True)
                self.children.add(process)
            if self.stop.is_set():
                self.terminate()
            try:
                process.communicate(input)
                return process.returncode
            finally:
                # Agents and checks must not leave servers running after an attempt.
                with contextlib.suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGTERM)
                with contextlib.suppress(ProcessLookupError):
                    os.killpg(process.pid, signal.SIGKILL)
                with self.lock:
                    self.children.discard(process)
                with contextlib.suppress(FileNotFoundError):
                    os.unlink(lease)

    def terminate(self):
        self.stop.set()
        with self.lock:
            children = list(self.children)
        for child in children:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(child.pid, signal.SIGTERM)
        deadline = time.monotonic() + 3
        for child in children:
            with contextlib.suppress(subprocess.TimeoutExpired):
                child.wait(timeout=max(0, deadline - time.monotonic()))
            with contextlib.suppress(ProcessLookupError):
                os.killpg(child.pid, signal.SIGKILL)


class Coordinator:
    def __init__(self, script):
        self.script = str(Path(script).resolve())
        self.root = Path.cwd().resolve()
        self.todo = Path(os.environ.get("TODO_FILE", "to-do.json")).resolve()
        self.schema = Path(os.environ.get("SCHEMA_FILE", "to-do.schema.json")).resolve()
        self.controls = [str(self.todo.relative_to(self.root)), str(self.schema.relative_to(self.root))]
        base_dir = Path(os.environ.get("LOOPER_BASE_DIR", str(Path.home() / ".looper"))).resolve()
        key = hashlib.sha1(str(self.root).encode()).hexdigest()[:8]
        slug = re.sub(r"[^A-Za-z0-9._-]+", "_", self.root.name).strip("_")
        self.logs = base_dir / (slug + "-" + key)
        self.directory = self.logs / "parallel"
        self.directory.mkdir(parents=True, exist_ok=True)
        self.state_file = self.directory / "state.json"
        self.run_id = os.environ.get("LOOPER_RUN_ID") or time.strftime("%Y%m%d-%H%M%S") + "-" + str(os.getpid())
        self.logfile = Path(os.environ.get("LOOPER_RUN_LOG") or self.logs / (self.run_id + ".jsonl"))
        self.excluded = list(self.controls)
        if self.logs.is_relative_to(self.root):
            self.excluded.append(str(self.logs.relative_to(self.root)))
        self.processes = Processes()
        self.limit = int(os.environ.get("MAX_ITERATIONS", "50"))
        if self.limit < 1:
            raise RunError("MAX_ITERATIONS must be positive.")
        cpus = os.cpu_count() or 2
        if hasattr(os, "sched_getaffinity"):
            cpus = len(os.sched_getaffinity(0))
        self.cpus = cpus
        requested = os.environ.get("LOOPER_JOBS", "auto")
        self.capacity = max(2, min(4, cpus // 2)) if requested == "auto" else int(requested)
        memory = available_memory()
        if requested == "auto" and memory is not None:
            self.capacity = min(self.capacity, max(1, memory // (2 * 1024 ** 3)))
        self.capacity = min(self.capacity, self.limit)
        self.maximum_capacity = self.capacity
        self.success_streak = 0
        self.test_capacity = max(1, min(self.capacity, cpus // 4))
        self.test_slots = threading.Semaphore(self.test_capacity)
        self.attempted = self.completed = self.blocked = 0
        self.todo_bytes = self.todo.read_bytes()
        self.backlog = validate_backlog(json.loads(self.todo_bytes))
        self.state = None
        self.started = time.monotonic()
        self.interrupted = False
        self.active = {}
        self.pending = []
        self.last_progress = (None, 0.0)

    def dispatch_capacity(self):
        # Keep agents moving while avoiding an ever-growing verification queue.
        waiting = sum(a["phase"] in ("ready", "verifying") for a in self.state["attempts"])
        limit = max(1, self.capacity - max(0, waiting - self.test_capacity))
        with contextlib.suppress(OSError):
            if os.getloadavg()[0] > self.cpus * 1.5:
                limit = max(1, limit // 2)
        memory = available_memory()
        if memory is not None and memory < 1024 ** 3:
            limit = 1
        return limit

    def event(self, text, **fields):
        print(text, flush=True)
        if os.environ.get("CODEX_JSON_LOG", "1") == "1":
            with self.logfile.open("a") as stream:
                stream.write(json.dumps({"type": "assistant_message", "looper_run_id": self.run_id, "looper_parallel": True,
                                         "message": {"content": [{"type": "text", "text": text}]},
                                         "time": time.time(), **fields}) + "\n")

    def progress(self, checks):
        tasks = [t for t in self.backlog["tasks"] if "project-done" not in t.get("tags", [])]
        message = (f"{len(self.active)} tasks running | {len(checks)} verifying | "
                   f"{sum(t['status'] == 'done' for t in tasks)}/{len(tasks)} complete")
        previous, when = self.last_progress
        if message != previous or time.monotonic() - when >= 30:
            self.event(message)
            self.last_progress = message, time.monotonic()

    def save(self):
        atomic_json(self.state_file, self.state)

    def write_backlog(self):
        if self.todo.read_bytes() != self.todo_bytes:
            raise RunError("The backlog changed outside Looper. Work is retained; reconcile it before resuming.")
        validate_backlog(self.backlog)
        atomic_json(self.todo, self.backlog)
        self.todo_bytes = self.todo.read_bytes()

    def task(self, task_id):
        return next(t for t in self.backlog["tasks"] if t["id"] == task_id)

    def set_status(self, task_id, status):
        task = self.task(task_id)
        task["status"] = status
        task["updated_at"] = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        self.write_backlog()

    def snapshot(self, cwd):
        """Snapshot without reading or writing the user's real index."""
        fd, name = tempfile.mkstemp(prefix="index-", dir=self.directory)
        os.close(fd)
        os.unlink(name)
        env = dict(os.environ, GIT_INDEX_FILE=name)
        try:
            if git(cwd, "rev-parse", "--verify", "HEAD", check=False).returncode:
                git(cwd, "read-tree", "--empty", env=env)
            else:
                git(cwd, "read-tree", "HEAD", env=env)
            git(cwd, "add", "-A", "--", ".", *(":(exclude)" + p for p in self.excluded), env=env)
            # A newly staged ignored file is tracked by the real index but not
            # by HEAD yet. Include its working bytes without copying the index.
            ignored = git(cwd, "ls-files", "--cached", "--ignored", "--exclude-standard", "-z").stdout
            staged = [os.fsdecode(p) for p in ignored.split(b"\0") if p]
            staged = [p for p in staged if os.path.lexists(Path(cwd) / p)
                      and not any(p == x or p.startswith(x + "/") for x in self.excluded)]
            for offset in range(0, len(staged), 100):
                git(cwd, "add", "-f", "--", *staged[offset:offset + 100], env=env)
            tracked = git(cwd, "ls-files", "-z", env=env).stdout.split(b"\0")
            remove = [p for p in tracked if p and any(p.decode() == x or p.decode().startswith(x + "/") for x in self.excluded)]
            if remove:
                git(cwd, "update-index", "--force-remove", "-z", "--stdin", input=b"\0".join(remove) + b"\0", env=env)
            return git_text(cwd, "write-tree", env=env)
        finally:
            with contextlib.suppress(FileNotFoundError):
                os.unlink(name)

    def commit(self, tree, parents, message):
        env = dict(os.environ, GIT_AUTHOR_NAME="Looper", GIT_AUTHOR_EMAIL="looper@localhost",
                   GIT_COMMITTER_NAME="Looper", GIT_COMMITTER_EMAIL="looper@localhost")
        args = ["commit-tree", tree]
        for parent in parents:
            args += ["-p", parent]
        return git_text(self.root, *args, input=(message + "\n").encode(), env=env)

    def workspace(self, name, commit):
        path = self.directory / "worktrees" / name
        path.parent.mkdir(parents=True, exist_ok=True)
        git(self.root, "worktree", "add", "--detach", str(path), commit)
        for source, relative in zip((self.todo, self.schema), self.controls):
            target = path / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(source.read_bytes())
        return path

    def remove_workspace(self, path):
        # Only coordinator-owned workspaces with durable commits are removed.
        git(self.root, "worktree", "remove", "--force", str(path), check=False)

    def check_original(self):
        if git_text(self.root, "rev-parse", "--verify", "HEAD", check=False) != self.state["head"]:
            raise RunError("The project branch moved during this run. Work is retained.")

    def check_task_contract(self, attempt):
        ignored = {"status", "updated_at", "files", "blockers"}
        saved = {k: v for k, v in attempt["task"].items() if k not in ignored}
        current = {k: v for k, v in self.task(attempt["task"]["id"]).items() if k not in ignored}
        if saved != current:
            raise RunError("The task changed while work was retained: " + attempt["task"]["id"])

    def initialize(self):
        reap_processes(self.directory)
        transient = self.directory / "worktrees"
        if transient.exists():
            for path in transient.iterdir():
                if path.name.startswith(("plan-", "review-", "check-")):
                    self.remove_workspace(path)
        if self.state_file.exists():
            state = read_json(self.state_file)
            if state.get("version") != 1 or state.get("root") != str(self.root):
                raise RunError("The saved run belongs to another project or runtime version.")
            retained = state.get("publication") or any(a["phase"] not in ("done", "abandoned") for a in state["attempts"])
            if not state.get("finished") and retained:
                if state.get("todo") != str(self.todo):
                    raise RunError("Resume the saved run with its original task file: " + state["todo"])
                self.state = state
                self.check_original()
                for attempt in state["attempts"]:
                    if attempt["phase"] not in ("done", "abandoned"):
                        self.check_task_contract(attempt)
                if state.get("publication"):
                    self.publish(state["publication"])
                if self.snapshot(self.root) != state["published"]:
                    raise RunError("Project files changed since the interrupted run. Saved work is in " + str(self.directory))
                for attempt in state["attempts"]:
                    if attempt["phase"] == "integrated":
                        self.apply_result(attempt)
                    elif attempt["phase"] == "blocked":
                        self.apply_result(attempt)
                    elif attempt["phase"] not in ("done", "abandoned", "resume"):
                        if attempt.get("result_commit"):
                            attempt["phase"] = "ready"
                            self.pending.append(attempt)
                        else:
                            # Retain partial edits and let a fresh session continue them.
                            attempt["phase"] = "resume"
                self.save()
                self.event("Resuming retained work.")
                return
            atomic_json(self.directory / "history" / (uuid.uuid4().hex + ".json"), state)
        head = git_text(self.root, "rev-parse", "--verify", "HEAD", check=False)
        tree = self.snapshot(self.root)
        seed = self.commit(tree, [head] if head else [], "Snapshot project for Looper")
        ref = "refs/looper/" + self.run_id
        git(self.root, "update-ref", ref, seed)
        self.state = {"version": 1, "root": str(self.root), "todo": str(self.todo), "head": head,
                      "ref": ref, "tip": seed, "published": tree, "attempts": [], "plan": None,
                      "finished": False}
        self.save()

    def agent_for(self, number):
        schedule = os.environ.get("LOOPER_ITER_SCHEDULE", "codex")
        if schedule == "odd-even":
            return os.environ.get("LOOPER_ITER_ODD_AGENT", "codex") if number % 2 else os.environ.get("LOOPER_ITER_EVEN_AGENT", "claude")
        if schedule == "round-robin":
            agents = os.environ.get("LOOPER_ITER_RR_AGENTS", "claude,codex").split(",")
            return agents[(number - 1) % len(agents)]
        return schedule

    def agent(self, cwd, directory, prompt, agent, summary):
        directory.mkdir(parents=True, exist_ok=True)
        prompt_path = directory / "prompt.txt"
        prompt_path.write_text(prompt)
        result_path = directory / "result.json"
        request = directory / "request.json"
        atomic_json(request, {"cwd": str(cwd), "directory": str(directory), "prompt": str(prompt_path),
                              "result": str(result_path), "agent": agent, "summary": int(summary)})
        env = dict(os.environ, LOOPER_AGENT_REQUEST=str(request))
        for key in ("GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE"):
            env.pop(key, None)
        maximum = int(os.environ.get("LOOPER_RATE_LIMIT_MAX", "12"))
        for retry in range(maximum):
            with contextlib.suppress(FileNotFoundError):
                result_path.unlink()
            code = self.processes.run(["bash", self.script], cwd, directory / "process.log", env=env)
            if code == 0:
                try:
                    return read_json(result_path)
                except (OSError, ValueError) as error:
                    raise RunError("Agent returned no valid JSON summary. See " + str(directory)) from error
            events = directory / "events.jsonl"
            output = events.read_text(errors="replace")[-12000:] if events.exists() else ""
            limited = re.search(r"(?i)(rate.?limit|429|too many requests)", output)
            if not limited or os.environ.get("LOOPER_RATE_LIMIT_RETRY", "1") != "1" or retry + 1 == maximum:
                raise RunError(f"Agent failed (exit {code}). See {directory}")
            delay = min(int(os.environ.get("LOOPER_RATE_LIMIT_CAP", "600")),
                        int(os.environ.get("LOOPER_RATE_LIMIT_DELAY", "60")) * 2 ** retry)
            if self.processes.stop.wait(delay):
                raise RunError("Run interrupted.")
        raise RunError("Rate-limit retry count must be positive.")

    def plan(self):
        signature = hashlib.sha256(json.dumps([{k: v for k, v in t.items() if k not in ("status", "updated_at", "files", "blockers")}
                                             for t in self.backlog["tasks"]], sort_keys=True).encode()).hexdigest()
        if self.state.get("plan_signature") == signature and self.state.get("plan"):
            return
        self.event("Planning independent tasks and project checks...")
        name = "plan-" + uuid.uuid4().hex[:10]
        work = self.workspace(name, self.state["tip"])
        prompt = f"""Plan execution for Looper. Read {self.controls[0]}, its source_files, repository instructions,
and the code. Do not change any files. Identify independent tasks, shared interfaces,
and executable project checks. You are planning for an arbitrary project.
Return only JSON with this shape:
{{"setup":"shell command or empty string", "verify":"shell command", "tasks":[
{{"task_id":"existing ID", "write_paths":["relative/file", "relative/directory"], "resources":["shared-resource"]}}]}}
Include every unfinished task. Paths are file or directory prefixes, without globs.
Use ["."] for uncertain or project-wide changes. Include files that need wiring or generated updates.
Use resources to serialize changes to the same public interface, database migration series,
or other shared state, even when files differ. No user configuration is required.
Setup runs in each fresh worktree. Recreate ignored dependencies there; never link mutable
build output or dependencies to another checkout. Use existing project tools and lockfiles.
Verify must check the combined project after integration, including required repository checks.
For a project without a test suite, give concrete executable acceptance checks appropriate to it.
Do not use a no-op command as verification. Do not create tasks or change their acceptance criteria.
Worktrees share Git refs and configuration. Do not edit repository configuration or other worktrees.
"""
        try:
            data = self.agent(work, self.directory / name, prompt, os.environ.get("LOOPER_REVIEW_AGENT", "codex"), False)
            self.state["plan"] = normalize_plan(data, self.backlog["tasks"], os.environ.get("LOOPER_VERIFY_COMMAND", ""))
            self.state["plan_signature"] = signature
            self.save()
        finally:
            self.remove_workspace(work)

    def command(self, command, cwd, directory, name):
        if not command.strip():
            return 0
        with self.test_slots:
            return self.processes.run(["bash", "-lc", command], cwd, directory / (name + ".log"))

    def execute(self, attempt):
        work = Path(attempt["worktree"])
        directory = Path(attempt["directory"])
        plan = self.state["plan"]
        if self.command(plan["setup"], work, directory, "setup"):
            raise RunError("Workspace setup failed. See " + str(directory / "setup.log"))
        task = attempt["task"]
        prompt = f"""Complete task {task['id']}: {task['title']}.
Read its details and acceptance criterion in "{self.controls[0]}". Read the listed source_files
and repository instructions. Your task description is also provided below.
Keep changes within this task. Do not edit "{self.controls[0]}"; Looper owns task status.
Work only in this worktree. Git refs and configuration are shared: do not switch branches,
merge, rebase, amend, push, or change shared Git configuration. Looper captures your changes.
Run the required checks. Return JSON with task_id, status (done or blocked), summary, files,
and blockers. Include a reason for blocked. Do not declare done with failing checks.
Task: {json.dumps(task)}
"""
        if attempt.get("feedback"):
            prompt += "\nRepair the task using this integration feedback:\n" + attempt["feedback"]
        result = self.agent(work, directory / ("agent-" + str(attempt.get("repairs", 0))), prompt, attempt["agent"], True)
        if not valid_summary(result, task["id"]):
            raise RunError("Invalid summary for " + task["id"])
        if result["status"] == "blocked":
            return {"summary": result, "phase": "blocked"}
        tree = self.snapshot(work)
        commit = self.commit(tree, [attempt["base"]], "Complete " + task["id"] + "\n\n" + result["summary"])
        git(self.root, "update-ref", attempt["ref"], commit)
        # Use observed files for later review, rather than trusting a file list
        # that can omit changes or include paths outside the project.
        result["files"] = [p.decode() for p in git(self.root, "diff", "--name-only", "-z", attempt["base"], commit).stdout.split(b"\0") if p]
        return {"summary": result, "result_commit": commit, "phase": "ready"}

    def scope(self, task_id):
        return self.state["plan"]["tasks"].get(task_id, {"write_paths": ["."], "resources": []})

    def observe_scope(self, attempt):
        scope = self.scope(attempt["task"]["id"])
        actual = attempt["summary"]["files"]
        extra = [p for p in actual if not scopes_overlap([p], scope["write_paths"])]
        if extra:
            scope["write_paths"] += extra
            attempt["scope_expanded"] = extra

    def compatible(self, task_id):
        scope = self.scope(task_id)
        # Keep scopes claimed until integration, not merely until the agent exits.
        for attempt in self.state["attempts"]:
            if attempt["phase"] in ("done", "blocked", "abandoned") or attempt["task"]["id"] == task_id:
                continue
            other = self.scope(attempt["task"]["id"])
            if scopes_overlap(scope["write_paths"], other["write_paths"]) or set(scope["resources"]) & set(other["resources"]):
                return False
        return True

    def dispatch(self, task, pool, resume=None):
        self.attempted += 1
        if resume:
            attempt = resume
            attempt["repairs"] = 0
        else:
            name = "task-" + uuid.uuid4().hex[:12]
            directory = self.directory / name
            directory.mkdir()
            base = self.state["tip"]
            attempt = {"id": name, "task": copy.deepcopy(task), "base": base,
                       "agent": self.agent_for(self.attempted), "directory": str(directory),
                       "worktree": str(self.workspace(name, base)),
                       "phase": "running", "repairs": 0}
            # refs cannot be both a file and a directory.
            attempt["ref"] = "refs/looper-tasks/" + self.run_id + "/" + name
            self.state["attempts"].append(attempt)
            attempt["worktrees"] = [attempt["worktree"]]
        attempt["phase"] = "running"
        self.save()
        self.set_status(task["id"], "doing")
        self.event(f"{task['id']}  {task['title']}", task_id=task["id"], attempt_id=attempt["id"])
        self.active[pool.submit(self.execute, attempt)] = attempt

    def prepare_candidate(self, attempt, base):
        """Build a candidate quickly so later checks can include its changes."""
        work = self.workspace("check-" + uuid.uuid4().hex[:12], base)
        try:
            result = git(work, "-c", "user.name=Looper", "-c", "user.email=looper@localhost",
                         "-c", "core.hooksPath=/dev/null", "merge", "--no-commit", "--no-ff", attempt["result_commit"], check=False)
            if result.returncode:
                self.remove_workspace(work)
                return {"ok": False, "feedback": "The task conflicts with the integrated project.\n" + result.stdout.decode(errors="replace") + result.stderr.decode(errors="replace"), "base": base}
            tree = git_text(work, "write-tree")
            commit = self.commit(tree, [base, attempt["result_commit"]], "Integrate " + attempt["task"]["id"])
            git(self.root, "update-ref", attempt["ref"] + "-candidate", commit)
            return {"ok": True, "commit": commit, "tree": tree, "base": base, "worktree": str(work)}
        except BaseException:
            self.remove_workspace(work)
            raise

    def verify_candidate(self, attempt, candidate):
        directory = Path(attempt["directory"])
        work = Path(candidate["worktree"])
        base = candidate["base"]
        try:
            if self.command(self.state["plan"]["setup"], work, directory, "integration-setup"):
                return {"ok": False, "feedback": "Integration setup failed:\n" + (directory / "integration-setup.log").read_text(errors="replace")[-16000:], "base": base}
            code = self.command(self.state["plan"]["verify"], work, directory, "verify")
            if code or self.snapshot(work) != candidate["tree"]:
                return {"ok": False, "feedback": "Combined verification failed or changed tracked files:\n" + (directory / "verify.log").read_text(errors="replace")[-16000:], "base": base}
            return {k: v for k, v in candidate.items() if k != "worktree"}
        finally:
            self.remove_workspace(work)

    def repair(self, attempt, feedback, pool):
        if attempt["repairs"] >= 2:
            attempt["phase"] = "resume"
            self.save()
            raise RunError("Integration still fails for " + attempt["task"]["id"] + ". Work and checks are retained in " + attempt["directory"])
        attempt["repairs"] += 1
        self.success_streak = 0
        self.capacity = max(1, self.capacity - 1)
        # Preserve every previous result; repair starts from current integrated code
        # with this task merged (including conflict markers when necessary).
        old = attempt["result_commit"]
        work = self.workspace("repair-" + uuid.uuid4().hex[:12], self.state["tip"])
        git(work, "-c", "user.name=Looper", "-c", "user.email=looper@localhost", "-c", "core.hooksPath=/dev/null",
            "merge", "--no-commit", "--no-ff", old, check=False)
        attempt.update(worktree=str(work), base=self.state["tip"], phase="running", feedback=feedback)
        attempt.setdefault("worktrees", []).append(str(work))
        attempt.pop("result_commit", None)
        self.save()
        self.event("Repairing " + attempt["task"]["id"] + " after integration checks.")
        self.active[pool.submit(self.execute, attempt)] = attempt

    def publish(self, publication):
        """Journal before apply; recovery recognizes either complete tree."""
        self.check_original()
        attempt = next(a for a in self.state["attempts"] if a["id"] == publication["attempt_id"])
        self.check_task_contract(attempt)
        if self.todo.read_bytes() != self.todo_bytes:
            raise RunError("The backlog changed outside Looper. Verified work is retained.")
        before, after = publication["before"], publication["tree"]
        observed = self.snapshot(self.root)
        if observed == before and before != after:
            patch = git(self.root, "diff", "--binary", before, after, "--").stdout
            git(self.root, "apply", "--check", "--binary", input=patch)
            git(self.root, "apply", "--binary", input=patch)
            observed = self.snapshot(self.root)
        if observed != after:
            raise RunError("Project files changed during publication. Verified work is retained; no task was marked done.")
        git(self.root, "update-ref", self.state["ref"], publication["commit"])
        self.state.update(tip=publication["commit"], published=after)
        attempt["phase"] = "integrated"
        attempt["verification"] = {k: publication[k] for k in ("base", "commit", "tree")}
        self.state.pop("publication", None)
        self.save()

    def apply_result(self, attempt):
        self.check_task_contract(attempt)
        summary = attempt["summary"]
        task = self.task(summary["task_id"])
        task["files"] = sorted(set(task.get("files", []) + summary["files"]))
        task["blockers"] = sorted(set(task.get("blockers", []) + summary["blockers"]))
        self.set_status(task["id"], summary["status"])
        attempt["phase"] = "done"
        self.save()
        if summary["status"] == "done":
            self.completed += 1
            self.success_streak += 1
            if self.success_streak >= 3:
                self.capacity = min(self.maximum_capacity, self.capacity + 1)
                self.success_streak = 0
            for work in attempt.get("worktrees", [attempt["worktree"]]):
                self.remove_workspace(work)
        else:
            self.blocked += 1
        self.event(f"{task['id']}  {summary['status']} | {summary['summary']}", task_id=task["id"])
        for blocker in summary["blockers"]:
            self.event("  Blocker: " + blocker)
        hook = os.environ.get("LOOPER_HOOK")
        if hook:
            path = Path(attempt["directory"]) / "summary.json"
            atomic_json(path, summary)
            self.processes.run([hook, task["id"], summary["status"], str(path), attempt["id"]], self.root,
                               Path(attempt["directory"]) / "hook.log")

    def review(self):
        name = "review-" + uuid.uuid4().hex[:10]
        work = self.workspace(name, self.state["tip"])
        self.event("Reviewing completed work...")
        try:
            self.agent(work, self.directory / name, f"""You are running a final review pass after all tasks are complete.
Read {self.controls[0]}, its source_files, and the repository instructions.
Review each completed task against its acceptance criteria and affected files.
Only edit {self.controls[0]}. Preserve existing tasks. Add a todo task for each verified gap,
with priority, dependencies, acceptance criteria, and observed evidence. Avoid duplicate tasks.
If there are no gaps, append a unique done task tagged project-done as the last task.
Do not add that marker when work remains. Follow {self.controls[1]}.
Return JSON with status, summary, added_tasks, and files.
""", os.environ.get("LOOPER_REVIEW_AGENT", "codex"), False)
            data = validate_backlog(read_json(work / self.controls[0]))
            schema_check = subprocess.run(["bash", self.script, "--validate-backlog", self.controls[0]],
                                          cwd=work, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            if schema_check.returncode:
                raise RunError("Final review produced an invalid backlog.")
            if self.snapshot(work) != self.state["published"]:
                raise RunError("Final review changed project files outside the backlog.")
            old = {t["id"]: t for t in self.backlog["tasks"]}
            new = {t["id"]: t for t in data["tasks"]}
            if any(new.get(key) != value for key, value in old.items()):
                raise RunError("Final review changed an existing task.")
            self.backlog = data
            self.write_backlog()
            self.state["plan_signature"] = None
            self.save()
        finally:
            self.remove_workspace(work)

    def run(self):
        self.initialize()
        self.event(f"Looper | up to {self.capacity} tasks running | {self.test_capacity} concurrent checks")
        if os.environ.get("CODEX_JSON_LOG", "1") == "1":
            self.event("Log: " + str(self.logfile))
        reviews = 0
        checks = {}
        with concurrent.futures.ThreadPoolExecutor(max_workers=self.capacity) as workers, \
                concurrent.futures.ThreadPoolExecutor(max_workers=self.capacity) as verifiers:
            try:
                while not self.processes.stop.is_set():
                    # Observe worker completions, then immediately free slots.
                    for future, attempt in list(self.active.items()):
                        if future.done():
                            del self.active[future]
                            attempt.update(future.result())
                            if attempt["phase"] == "ready":
                                self.observe_scope(attempt)
                            self.save()
                            if attempt["phase"] == "blocked":
                                self.apply_result(attempt)
                            else:
                                self.pending.append(attempt)
                    # Candidates form a speculative chain. Checks run concurrently;
                    # publication follows chain order. A failed predecessor causes
                    # successors to be rebuilt, never accepted on a different base.
                    for future, entry in list(checks.items()):
                        if not future.done():
                            break
                        attempt = entry["attempt"]
                        del checks[future]
                        result = future.result()
                        # Never publish a check against an obsolete base.
                        if result["base"] != self.state["tip"]:
                            self.pending.append(attempt)
                            continue
                        if not result["ok"]:
                            self.repair(attempt, result["feedback"], workers)
                            continue
                        publication = dict(result, before=self.state["published"], attempt_id=attempt["id"])
                        self.state["publication"] = publication
                        self.save()
                        self.publish(publication)
                        self.apply_result(attempt)
                    while self.pending and len(checks) < self.test_capacity:
                        attempt = self.pending.pop(0)
                        base = list(checks.values())[-1]["candidate"]["commit"] if checks else self.state["tip"]
                        candidate = self.prepare_candidate(attempt, base)
                        if not candidate["ok"]:
                            if checks:
                                self.pending.insert(0, attempt)
                                break
                            self.repair(attempt, candidate["feedback"], workers)
                            continue
                        attempt["phase"] = "verifying"
                        self.save()
                        self.event("Verifying " + attempt["task"]["id"] + " with integrated work...")
                        checks[verifiers.submit(self.verify_candidate, attempt, candidate)] = {"attempt": attempt, "candidate": candidate}
                    outstanding = self.active or checks or self.pending
                    open_tasks = [t for t in self.backlog["tasks"] if t["status"] != "done"]
                    if not outstanding and not open_tasks:
                        tasks = self.backlog["tasks"]
                        if tasks and "project-done" in tasks[-1].get("tags", []):
                            self.state["finished"] = True
                            self.save()
                            return 0, "Project marked done."
                        if reviews >= 3:
                            raise RunError("Final review did not complete after three passes.")
                        reviews += 1
                        self.review()
                        continue
                    if self.attempted < self.limit:
                        ready = ranked_ready(self.backlog["tasks"])
                        if ready:
                            self.plan()
                        busy = {a["task"]["id"] for a in self.state["attempts"] if a["phase"] not in ("done", "resume", "abandoned")}
                        for task in ready:
                            if len(self.active) >= self.dispatch_capacity() or self.attempted >= self.limit:
                                break
                            if task["id"] in busy or not self.compatible(task["id"]):
                                continue
                            resume = next((a for a in self.state["attempts"] if a["task"]["id"] == task["id"] and a["phase"] == "resume"), None)
                            self.dispatch(task, workers, resume)
                    if not self.active and not checks and not self.pending:
                        if self.attempted >= self.limit:
                            return 2, f"Iteration limit reached ({self.limit})."
                        return 2, "No runnable tasks remain."
                    self.progress(checks)
                    self.processes.stop.wait(0.1)
                return 130, "Interrupted. Work is retained for the next run."
            except BaseException:
                self.processes.terminate()
                raise

    def finish(self, code, reason):
        self.event(reason)
        self.event(f"This run  {self.completed} done | {self.blocked} blocked | {self.attempted} attempted | {int(time.monotonic() - self.started)}s")
        done = sum(t["status"] == "done" and "project-done" not in t.get("tags", []) for t in self.backlog["tasks"])
        total = sum("project-done" not in t.get("tags", []) for t in self.backlog["tasks"])
        self.event(f"Backlog   {done}/{total} done")
        return code


def eligibility():
    root = Path.cwd().resolve()
    result = git(root, "rev-parse", "--show-toplevel", check=False)
    reason = None
    if result.returncode or Path(result.stdout.decode().strip()).resolve() != root:
        reason = "Worktree execution needs a Git repository root."
    elif b"160000 " in git(root, "ls-files", "--stage").stdout:
        reason = "This repository uses submodules."
    elif git(root, "ls-files", "--unmerged").stdout:
        reason = "The repository has unresolved conflicts."
    todo = Path(os.environ.get("TODO_FILE", "to-do.json")).absolute()
    schema = Path(os.environ.get("SCHEMA_FILE", "to-do.schema.json")).absolute()
    if any(not path.resolve().is_relative_to(root) or path.is_symlink() for path in (todo, schema)):
        reason = "The task file and schema must be regular files inside the repository."
    if reason:
        if os.environ.get("LOOPER_JOBS", "auto") != "auto":
            raise RunError(reason + " Use --jobs 1 for sequential execution.")
        print(reason + " Running tasks sequentially.", flush=True)
        return 75
    return 0


def run(script):
    ensure_repository_lock()
    coordinator = Coordinator(script)

    def interrupt(signum, frame):
        coordinator.interrupted = True
        coordinator.processes.terminate()

    signal.signal(signal.SIGINT, interrupt)
    signal.signal(signal.SIGTERM, interrupt)
    try:
        code, reason = coordinator.run()
    except (RunError, OSError, ValueError, KeyError) as error:
        code, reason = (130, "Interrupted. Work is retained.") if coordinator.interrupted else (1, str(error))
    finally:
        coordinator.processes.terminate()
    return coordinator.finish(code, reason)


if __name__ == "__main__":
    try:
        if sys.argv[1] == "guard":
            guard(sys.argv[2], sys.argv[3:])
        elif sys.argv[1] == "eligible":
            sys.exit(eligibility())
        elif sys.argv[1] == "child":
            lease, owner = sys.argv[2], int(sys.argv[3])
            atomic_json(lease, {"pid": os.getpid(), "identity": process_identity(os.getpid())})
            # Do not launch commands if the coordinator died during spawn.
            if os.getppid() != owner:
                sys.exit(1)
            os.execvpe(sys.argv[4], sys.argv[4:], os.environ)
        else:
            sys.exit(run(sys.argv[2]))
    except (RunError, OSError, ValueError) as error:
        print("Error: " + str(error), file=sys.stderr)
        sys.exit(1)
