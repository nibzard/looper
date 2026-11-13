#!/usr/bin/env python3
"""Exercise parallel execution with real Git repositories and local agent stubs."""

import sqlite3
import hashlib
import json
import os
from pathlib import Path
import signal
import shutil
import subprocess
import sys
import tempfile
import time
import unittest


ROOT = Path(__file__).resolve().parents[1]

def load_budget(base: float) -> float:
    """Scale a quiet-host subprocess timeout by the pressure this host reports.

    The looper binary the suite drives is fsync-bound, and sibling suites on
    the same host stretch every durable step by the time their own fsyncs sit
    queued ahead of it. Recorded failures: a 40-second budget lost two tests
    at host load 10-16, while the same suite passed 64/64 in 380 s at load
    10 and in 297 s at load 7-9, so the failure threshold sits above load 10
    and the budget must grow with pressure instead of failing healthy runs.
    Anchors mirror internal/looper's testBudget: each CPU-load above one adds
    its excess, every 25 percent of "full" IO stall (PSI, /proc/pressure/io)
    adds one more, and the growth stops at six times the base.
    """
    factor = 1.0
    try:
        cpus = os.cpu_count() or 1
        with open("/proc/loadavg") as loadavg:
            load = float(loadavg.read().split()[0])
        factor += max(0.0, load / cpus - 1)
        with open("/proc/pressure/io") as pressure:
            for line in pressure:
                if line.startswith("full "):
                    for field in line.split()[1:]:
                        if field.startswith("avg60="):
                            factor += float(field[len("avg60="):]) / 25
                            break
    except (OSError, ValueError):
        pass
    return base * min(factor, 6.0)

STUB = r'''#!/usr/bin/env python3
import json, os, pathlib, sys, time
args=sys.argv[1:]
claude="-p" in args
prompt=sys.stdin.read()
cwd=pathlib.Path.cwd()
todo=cwd/os.environ.get("TEST_TODO", "to-do.json")
# A bootstrap worktree carries no task file yet, so tasks load lazily.
tasks=json.loads(todo.read_text())["tasks"] if todo.exists() else []
events=pathlib.Path(os.environ["TEST_EVENTS"])
def event(kind, task=""):
    with events.open("a") as f:
        f.write(json.dumps({"kind":kind,"task":task,"time":time.time(),"cwd":str(cwd),"pid":os.getpid()})+"\n")
def wait_for_gate(path, label):
    deadline=time.monotonic()+120
    while not pathlib.Path(path).exists():
        if time.monotonic()>deadline:
            raise RuntimeError("the %s gate was never released" % label)
        time.sleep(.01)
mode=os.environ.get("TEST_MODE", "")
if "Plan execution for Looper" in prompt:
    event("plan")
    result={"setup":"", "verify":os.environ.get("TEST_VERIFY", "python3 check.py"), "tasks":[
        {"task_id":t["id"],"write_paths":["." if mode=="exclusive" else t["id"]+".txt"],"resources":[]}
        for t in tasks if t["status"]!="done"]}
    if mode=="bad_plan":
        result={"tasks":[],"setup":"","verify":""}
elif "Bootstrap a development backlog for Looper" in prompt:
    # AX-027 benchmark fixture: hold the bootstrap window open so a public
    # status observes a live controller that has not saved its first backlog.
    event("bootstrap")
    if os.environ.get("TEST_BOOTSTRAP_GATE"):
        wait_for_gate(os.environ["TEST_BOOTSTRAP_GATE"], "bootstrap")
    result={"schema_version":1,"goal":"Benchmark project goal with a finished procedure",
            "source_files":[],
            "tasks":[{"id":"T001","title":"Bootstrap task one","status":"todo","priority":1},
                     {"id":"T002","title":"Bootstrap task two","status":"todo","priority":1}]}
elif "Analyze additional requirements for Looper" in prompt:
    # AX-027 benchmark fixture: hold the import analysis open so the public
    # CLI observes a waiting import and the dispatch pause it causes.
    event("import-analysis")
    if os.environ.get("TEST_IMPORT_GATE"):
        wait_for_gate(os.environ["TEST_IMPORT_GATE"], "import analysis")
    result={"additions":[{"id":"P1","title":"Imported benchmark task","priority":1,"status":"todo",
             "details":"Create P1.txt containing done.","depends_on":[]}],
            "covered":[],"conflicts":[],"adjustments":[],"gates":[]}
elif "review pass for Looper" in prompt:
    event("review")
    assert not (cwd/"forbidden.txt").exists()
    if mode=="review_fail":
        sys.exit(7)
    if mode=="review_account":
        message="The 'stub-astra' model is not supported when using Codex with a ChatGPT account."
        print(json.dumps({"type":"error","message":message}))
        print(json.dumps({"type":"turn.failed","error":{"message":message}}))
        sys.exit(1)
    if mode=="review_code":
        (cwd/"seed.txt").write_text("review changed code")
    if mode in ("review_backlog","review_goal"):
        data=json.loads(todo.read_text())
        if mode=="review_backlog":
            data["tasks"].append({"id":"PROJECT-DONE","title":"Project done","priority":5,"status":"done","tags":["project-done"]})
        else:
            data["goal"]="a different goal"
        todo.write_text(json.dumps(data))
    complete="final review pass for Looper" in prompt
    findings=[]
    if mode=="review_finding" and not any(t["id"]=="T003" for t in tasks):
        findings=[{"title":"Fix observed behavior","details":"Check added output","evidence":"Observed fixture failure","depends_on":[],"priority":1}]
        complete=False
    if mode=="review_followup_hold":
        gate=pathlib.Path(os.environ["TEST_GATE"])
        deadline=time.monotonic()+30
        while not gate.exists():
            if time.monotonic()>deadline:
                raise RuntimeError("the review gate was never released")
            time.sleep(.01)
        if not any(t["title"]=="Fix observed behavior" for t in tasks):
            # The finding is returned once with the project directory
            # read-only, so the first export after the finding cannot write
            # the task file. Later reviews return the finding without the
            # lock so a recovered run can commit it.
            lock=pathlib.Path(os.environ["TEST_LOCK_ONCE"])
            if not lock.exists():
                os.chmod(os.environ["TEST_PROJECT"], 0o555)
                lock.write_text("locked")
            findings=[{"title":"Fix observed behavior","details":"Check added output","evidence":"Observed fixture failure","depends_on":[],"priority":1}]
            complete=False
    if mode=="review_incomplete":
        complete=False
    result={"status":"reviewed","summary":"checked","findings":findings,"complete":complete}
elif "Recover the final task report" in prompt:
    task=next(t for t in tasks if t["id"] in prompt)
    event("recover",task["id"])
    result={"task_id":"WRONG" if mode=="invalid" else task["id"],"status":"done","summary":"recovered","files":[task["id"]+".txt"],"blockers":[]}
elif "Diagnose the repeated failure" in prompt:
    task=next(t for t in tasks if t["id"] in prompt)
    event("diagnose",task["id"])
    result={"root_cause":"the verify command fails before the task output is checked",
            "evidence":"verify.log shows the failure",
            "fix_approach":"make the verify command pass first"}
else:
    task=next(t for t in tasks if "Complete task "+t["id"]+":" in prompt)
    task_id=task["id"]
    event("start",task_id)
    if mode=="worker_gate":
        # AX-027 benchmark fixture: the gate file names the tasks that hold
        # their worker open; rewriting or unlinking it releases them.
        gate=pathlib.Path(os.environ["TEST_WORKER_GATE"])
        deadline=time.monotonic()+120
        while gate.exists() and task_id in gate.read_text():
            if time.monotonic()>deadline:
                raise RuntimeError("the worker gate was never released for "+task_id)
            time.sleep(.01)
    if mode=="timing_parallel":
        deadline=time.monotonic()+10
        while time.monotonic()<deadline:
            try:
                started=[json.loads(line) for line in events.read_text().splitlines()]
                if sum(e["kind"]=="start" for e in started)==2:
                    break
            except json.JSONDecodeError:
                pass
            time.sleep(.01)
        else:
            raise RuntimeError("timing fixture did not start both workers")
    if mode=="resume_partial":
        assert (cwd/(task_id+".txt")).exists(), "partial work was lost"
    if mode=="interrupt":
        (cwd/(task_id+".txt")).write_text("partial")
        time.sleep(30)
    elif mode=="pipeline" and task_id=="T2":
        # Keep the unrelated worker active until its successor starts. A fixed
        # sleep tests disk speed instead of whether dispatch waits for workers.
        deadline=time.monotonic()+15
        while time.monotonic()<deadline:
            try:
                started=[json.loads(line) for line in events.read_text().splitlines()]
                if any(e["kind"]=="start" and e["task"]=="T3" for e in started):
                    break
            except json.JSONDecodeError:
                pass
            time.sleep(.01)
        else:
            raise RuntimeError("successor did not start while the unrelated worker was active")
    elif mode!="fast":
        time.sleep(0.2 if task_id=="T1" else 0.5)
    if mode=="pipeline" and task_id=="T3":
        assert (cwd/"T1.txt").read_text()=="done"
    if mode=="conflict":
        path=cwd/"shared.txt"
        content=path.read_text() if path.exists() else ""
        if "Repair the task" in prompt:
            content="T1\nT2\n"
        else:
            content=task_id+"\n"
    else:
        path=cwd/(task_id+".txt")
        content="done"
    path.write_text(content)
    if mode=="semantic" and task_id=="T2" and "Repair the task" not in prompt:
        path.write_text("incompatible")
    if mode=="baseline":
        assert (cwd/"seed.txt").read_text()=="unstaged\n"
        assert (cwd/"untracked.txt").read_text()=="untracked\n"
        assert (cwd/"ignored.txt").read_text()=="staged ignored file\n"
    event("end",task_id)
    result={"task_id":"WRONG" if mode=="invalid" else task_id,
            "status":"blocked" if mode=="blocked" or (mode=="mixed" and task_id=="T1") else "done", "summary":"checked "+task_id,
            "files":[path.name],"blockers":["missing input"] if mode=="blocked" or (mode=="mixed" and task_id=="T1") else []}
    if mode=="followup" and task_id=="T1":
        result["followups"]=[{"title":"Fix observed behavior","details":"Check added output","evidence":"Observed fixture failure","depends_on":["T1"],"priority":1}]
    if mode=="malformed":
        pathlib.Path(args[args.index("--output-last-message")+1]).write_text("not json")
        sys.exit(0)
if claude:
    output={"structured_output":result,"result":json.dumps(result)}
    if mode=="usage":
        output.update(type="result",usage={"input_tokens":100,"cache_read_input_tokens":500,"output_tokens":20},total_cost_usd=0.01)
    print(json.dumps(output))
else:
    pathlib.Path(args[args.index("--output-last-message")+1]).write_text(json.dumps(result))
    print(json.dumps({"type":"result"}))
    if mode=="usage":
        print(json.dumps({"type":"turn.completed","usage":{"input_tokens":600,"cached_input_tokens":500,"output_tokens":20}}))
'''


def project_fixture(test):
    """Give one test a temporary Git project with the deterministic stub agent.

    Shared by the parallel-execution suite and the AX-027 benchmark suite so
    both drive the same binary against the same project layout.
    """
    test.temp = tempfile.TemporaryDirectory(prefix="looper-parallel-")
    test.addCleanup(test.temp.cleanup)
    test.path = Path(test.temp.name)
    test.project = test.path / "project with spaces"
    test.project.mkdir()
    test.git("init", "-q")
    test.git("config", "user.name", "Test")
    test.git("config", "user.email", "test@example.invalid")
    (test.project / "seed.txt").write_text("original\n")
    (test.project / "check.py").write_text(
        'from pathlib import Path\n'
        'a, b = Path("T1.txt"), Path("T2.txt")\n'
        'assert not (a.exists() and b.exists()) or b.read_text() == "done"\n')
    test.git("add", ".")
    test.git("commit", "-qm", "Initial fixture")
    test.head = test.git("rev-parse", "HEAD")
    test.stub = test.path / "agent"
    test.stub.write_text(STUB)
    test.stub.chmod(0o755)
    test.events = test.path / "events.jsonl"
    test.env = dict(os.environ, CODEX_BIN=str(test.stub), CLAUDE_BIN=str(test.stub),
                    CODEX_JSON_LOG="1", CODEX_PROGRESS="0", LOOPER_BASE_DIR=str(test.path / "logs"),
                    LOOPER_JOBS="2", LOOPER_VERIFY_COMMAND="", LOOPER_HOOK="", LOOPER_GIT_INIT="0",
                    LOOP_DELAY_SECONDS="0", MAX_ITERATIONS="10", TEST_EVENTS=str(test.events),
                    LOOPER_ITER_SCHEDULE="codex", LOOPER_REVIEW_AGENT="codex", LOOPER_REPAIR_AGENT="codex")
    test.env.pop("LOOPER_LOCK_HELD", None)
    test.todo = test.project / "to-do.json"
    test.tasks()


class ParallelTests(unittest.TestCase):
    def setUp(self):
        project_fixture(self)

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.project), *args], stderr=subprocess.PIPE).decode().strip()

    def tasks(self, dependencies=None, count=2):
        self.todo.write_text(json.dumps({"schema_version":1,"source_files":[],"tasks":[
            {"id":"T"+str(i),"title":"Task "+str(i),"status":"todo","priority":1,
             "depends_on": (dependencies or {}).get("T"+str(i), [])} for i in range(1,count+1)]}))

    def command(self, *args):
        return [str(ROOT / "build/looper"), "--once", *args, str(self.todo)]

    def run_loop(self, expected=0, *args):
        result = subprocess.run(self.command(*args), cwd=self.project, env=self.env,
                                capture_output=True, text=True, timeout=load_budget(40))
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
        return result.stdout + result.stderr

    def recorded(self):
        return [json.loads(x) for x in self.events.read_text().splitlines()]

    def state(self):
        with sqlite3.connect(next((self.path / "logs").glob("*/runtime/state.db"))) as db:
            return json.loads(db.execute("select data from state where id=1").fetchone()[0])

    def controller_events(self, kind):
        with sqlite3.connect(next((self.path / "logs").glob("*/runtime/state.db"))) as db:
            rows = db.execute("select id,message,data from events where kind=? order by id", (kind,)).fetchall()
        return [{"id": row[0], "message": row[1], "data": json.loads(row[2])} for row in rows]

    def state_eventually(self):
        try:
            return self.state()
        except Exception:
            return None

    def test_parallel_and_exact_publication(self):
        self.env["TEST_MODE"] = "timing_parallel"
        self.run_loop()
        events = self.recorded()
        starts = [e["time"] for e in events if e["kind"] == "start"]
        ends = [e["time"] for e in events if e["kind"] == "end"]
        self.assertLess(max(starts), min(ends), events)
        self.assertEqual(self.git("rev-parse", "HEAD"), self.head)
        self.assertEqual((self.project / "T1.txt").read_text(), "done")
        self.assertEqual((self.project / "T2.txt").read_text(), "done")
        self.assertTrue(self.state()["finished"])
        self.assertEqual(len([e for e in events if e["kind"] == "review"]), 1)

    def test_dirty_baseline_preserves_index_and_head(self):
        (self.project / "seed.txt").write_text("staged\n")
        self.git("add", "seed.txt")
        (self.project / "seed.txt").write_text("unstaged\n")
        (self.project / "untracked.txt").write_text("untracked\n")
        (self.project / ".gitignore").write_text("ignored.txt\n")
        (self.project / "ignored.txt").write_text("staged ignored file\n")
        self.git("add", "-f", "ignored.txt")
        index = (self.project / ".git/index").read_bytes()
        self.env["TEST_MODE"] = "baseline"
        self.run_loop()
        self.assertEqual((self.project / ".git/index").read_bytes(), index)
        self.assertEqual(self.git("show", ":seed.txt"), "staged")
        self.assertEqual((self.project / "seed.txt").read_text(), "unstaged\n")

    def test_project_timing_is_durable_and_reported(self):
        self.env["TEST_MODE"]="timing_parallel"
        self.run_loop()
        path = next((self.path / "logs").glob("*/runtime/state.db"))
        with sqlite3.connect(path) as db:
            sessions = db.execute("select id,started,ended,outcome from sessions").fetchall()
            self.assertEqual(len(sessions), 1)
            self.assertIsNotNone(sessions[0][2])
            self.assertEqual(sessions[0][3], "stopped")
            processes = db.execute("select session_id,attempt_id,ended,outcome from processes").fetchall()
            self.assertGreaterEqual(len(processes), 6)
            self.assertTrue(all(p[0] == sessions[0][0] and p[2] is not None for p in processes))
            self.assertTrue(all(p[3] == "success" for p in processes))
            attempts = {a["id"] for a in self.state()["attempts"]}
            self.assertEqual({p[1] for p in processes if p[1]}, attempts)
        result = subprocess.run([str(ROOT / "build/looper"), "status", "--json", str(self.todo)],
                                cwd=self.project, env=self.env, capture_output=True, text=True, timeout=load_budget(10))
        self.assertEqual(result.returncode, 0, result.stderr)
        timing = json.loads(result.stdout)["timing"]
        self.assertTrue(timing["available"])
        self.assertTrue(timing["history_complete"])
        self.assertEqual(timing["unknown_sessions"], 0)
        self.assertEqual(timing["unknown_processes"], 0)
        self.assertGreater(timing["active_seconds"], 0)
        self.assertGreater(timing["worker_seconds"], timing["active_seconds"])
        self.assertGreaterEqual(timing["elapsed_seconds"], timing["active_seconds"])
        self.assertGreaterEqual(timing["controller_seconds"], timing["active_seconds"])

        task_times = {t["task_id"]: t for t in timing["tasks"]}
        self.assertEqual(set(task_times), {"T1", "T2"})
        for task in task_times.values():
            self.assertTrue(task["available"])
            self.assertTrue(task["history_complete"])
            self.assertGreater(task["worker_seconds"], 0)
            self.assertGreater(task["check_seconds"], 0)
            self.assertEqual(task["recorded_attempts"], 1)
        inspected = subprocess.run([str(ROOT / "build/looper"), "inspect", "T1", "--json"],
                                   cwd=self.project, env=self.env, capture_output=True, text=True, timeout=load_budget(10))
        self.assertEqual(inspected.returncode, 0, inspected.stderr)
        self.assertEqual(json.loads(inspected.stdout)["timing"], task_times["T1"])
        shutdown = self.controller_events("shutdown")[-1]["data"]
        self.assertEqual(shutdown["reason"], "complete")
        self.assertEqual(shutdown["exit_code"], 0)
        version = subprocess.run([str(ROOT / "build/looper"), "--version", "--json"],
                                 cwd=self.project, env=self.env, capture_output=True, text=True, timeout=load_budget(10))
        self.assertEqual(version.returncode, 0, version.stderr)
        build = json.loads(version.stdout)
        self.assertTrue(build["revision"])
        if (ROOT / ".git").exists():
            revision = subprocess.check_output(["git", "-C", str(ROOT), "rev-parse", "HEAD"], text=True).strip()
            self.assertEqual(build["revision"], revision)
        self.assertEqual(self.controller_events("session")[-1]["data"]["build"], build)

    def test_usage_statistics_survive_restart_without_duplicates(self):
        self.env["TEST_MODE"]="usage"
        self.run_loop(0,"--model","gpt-5.3-codex")
        def statistics():
            result=subprocess.run([str(ROOT/"build/looper"),"status","--json",str(self.todo)],
                                  cwd=self.project,env=self.env,capture_output=True,text=True,timeout=load_budget(10))
            self.assertEqual(result.returncode,0,result.stderr)
            return json.loads(result.stdout)["statistics"]
        before=statistics()
        self.assertGreater(before["usage_reports"],0)
        self.assertEqual(before["tokens"],before["usage_reports"]*620)
        self.assertEqual(before["priced_reports"],before["usage_reports"])
        self.assertGreater(before["estimated_usd"],0)
        # Startup imports completed process sidecars, including these reports.
        # Their artifact and start time must keep the old totals unchanged.
        self.run_loop(0,"--model","gpt-5.3-codex")
        after=statistics()
        self.assertEqual(after["tokens"]-before["tokens"],
                         (after["usage_reports"]-before["usage_reports"])*620)
        self.assertTrue(all(m["model"]=="gpt-5.3-codex" for m in after["models"]))

    def test_claude_usage_statistics_keep_reported_cost_separate(self):
        self.env["TEST_MODE"]="usage"
        self.env["CLAUDE_MODEL"]="claude-sonnet-4-6"
        self.run_loop(0,"--all","claude")
        result=subprocess.run([str(ROOT/"build/looper"),"status","--json",str(self.todo)],
                              cwd=self.project,env=self.env,capture_output=True,text=True,timeout=load_budget(10))
        self.assertEqual(result.returncode,0,result.stderr)
        report=json.loads(result.stdout)["statistics"]
        self.assertEqual(report["tokens"],report["usage_reports"]*620)
        self.assertGreater(report["estimated_usd"],0)
        self.assertAlmostEqual(report["reported_usd"],report["usage_reports"]*0.01)

    def test_repository_before_its_first_commit(self):
        self.git("checkout","--orphan","new-project")
        index=(self.project/".git/index").read_bytes()
        self.run_loop()
        result=subprocess.run(["git","rev-parse","--verify","HEAD"],cwd=self.project,capture_output=True)
        self.assertNotEqual(result.returncode,0)
        self.assertEqual((self.project/".git/index").read_bytes(),index)
        self.assertTrue((self.project/"T1.txt").exists())

    def test_dependency_waits_for_integration(self):
        self.tasks({"T2":["T1"]})
        self.run_loop()
        events = self.recorded()
        end = next(e for e in events if e["kind"]=="end" and e["task"]=="T1")
        start = next(e for e in events if e["kind"]=="start" and e["task"]=="T2")
        self.assertGreater(start["time"], end["time"])
        state=self.state()
        self.assertNotEqual(state["attempts"][0]["base"], state["attempts"][1]["base"])

    def test_dispatch_does_not_wait_for_unrelated_workers(self):
        self.tasks({"T3":["T1"]},count=3)
        self.env["TEST_MODE"]="pipeline"
        self.run_loop()
        events=self.recorded()
        start=next(e["time"] for e in events if e["kind"]=="start" and e["task"]=="T3")
        end=next(e["time"] for e in events if e["kind"]=="end" and e["task"]=="T2")
        self.assertLess(start,end)

    def test_overlapping_scopes_run_sequentially(self):
        self.env["TEST_MODE"]="exclusive"
        self.run_loop()
        events=self.recorded()
        self.assertEqual([e["kind"] for e in events if e["kind"] in ("start","end")], ["start","end","start","end"])

    def test_conflict_blocks_with_retained_work(self):
        self.env["TEST_MODE"]="conflict"
        self.run_loop(2)
        state = self.state()
        self.assertFalse(state["finished"])
        done = next(a for a in state["attempts"] if a["phase"] == "done")
        blocked = next(a for a in state["attempts"] if a["phase"] == "blocked")
        self.assertEqual((self.project/"shared.txt").read_text(), done["task"]["id"]+"\n")
        self.assertEqual((Path(blocked["worktree"])/"shared.txt").read_text(), blocked["task"]["id"]+"\n")
        self.assertIn("CONFLICT", blocked["feedback"])
        self.assertIn("shared.txt", blocked["feedback"])
        starts = [e for e in self.recorded() if e["kind"] == "start"]
        self.assertEqual(len(starts), 2, starts)
        self.assertEqual(len(blocked["worktrees"]), 1)
        self.assertEqual(self.controller_events("repair"), [])

    def test_combined_verification_repairs_semantic_conflict(self):
        self.env["TEST_MODE"]="semantic"
        self.run_loop()
        self.assertEqual((self.project/"T2.txt").read_text(), "done")
        self.assertTrue(any(a["repairs"] for a in self.state()["attempts"]))

    def test_checks_overlap_on_a_speculative_chain(self):
        (self.project/"check.py").write_text(
            'import json, os, time\nfrom pathlib import Path\n'
            'tasks=sorted(p.name for p in Path(".").glob("T*.txt"))\n'
            'def event(kind):\n'
            '    with open(os.environ["TEST_EVENTS"],"a") as f:\n'
            '        f.write(json.dumps({"kind":kind,"task":tasks,"time":time.time()})+"\\n")\n'
            'event("check-start")\ntime.sleep(1.5)\nevent("check-end")\n')
        self.run_loop(0, "--check-jobs", "2")
        starts=[e for e in self.recorded() if e["kind"]=="check-start"]
        ends=[e for e in self.recorded() if e["kind"]=="check-end"]
        self.assertEqual(len(starts),2,starts)
        self.assertLess(starts[1]["time"],ends[0]["time"])
        self.assertEqual(starts[1]["task"],["T1.txt","T2.txt"])
        attempts=self.state()["attempts"]
        self.assertEqual(attempts[1]["verification"]["base"],attempts[0]["verification"]["commit"])

    def test_invalid_summary_does_not_publish(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="invalid"
        self.run_loop(2)
        self.assertFalse((self.project/"T1.txt").exists())
        self.assertNotEqual(json.loads(self.todo.read_text())["tasks"][0]["status"], "done")
        self.env["TEST_MODE"]=""
        data=json.loads(self.todo.read_text()); data["tasks"][0]["status"]="todo"
        self.todo.write_text(json.dumps(data))
        self.run_loop()

    def test_blocked_result_does_not_publish_partial_work(self):
        self.tasks({"T2":["T1"]})
        self.env["TEST_MODE"]="blocked"
        self.run_loop(2)
        self.assertFalse((self.project/"T1.txt").exists())
        self.assertEqual(json.loads(self.todo.read_text())["tasks"][0]["status"], "blocked")

    def test_limit_drains_workers_and_resumes(self):
        self.tasks(count=3)
        self.env["MAX_ITERATIONS"]="2"
        self.run_loop(2)
        self.assertEqual(sum(t["status"]=="done" for t in json.loads(self.todo.read_text())["tasks"]), 2)
        self.run_loop()

    def test_claude_bridge(self):
        self.run_loop(0, "--all", "claude")

    def test_named_claude_profiles_use_independent_commands(self):
        commands = {}
        for name in ("glm", "anthropic"):
            wrapper = self.path / (name + "-wrapper")
            wrapper.write_text(
                "#!/usr/bin/env python3\n"
                "import json, os, sys, time\n"
                "from pathlib import Path\n"
                f"with Path({str(self.events)!r}).open('a') as log:\n"
                f"    log.write(json.dumps({{'kind': 'profile', 'profile': {name!r}}}) + '\\n')\n"
                f"os.execv({str(self.stub)!r}, [{str(self.stub)!r}, *sys.argv[1:]])\n")
            wrapper.chmod(0o755)
            commands[name] = str(wrapper)
        catalog = self.path / "agents.json"
        catalog.write_text(json.dumps({"version": 1, "agents": {
            name: {"adapter": "claude", "command": command, "model": name + "-model"}
            for name, command in commands.items()}}))
        self.env["LOOPER_AGENTS_FILE"] = str(catalog)
        self.env.pop("CLAUDE_MODEL", None)
        self.run_loop(0, "--agent", "glm", "--review-agent", "anthropic")
        profiles = {event["profile"] for event in [json.loads(line) for line in self.events.read_text().splitlines()]
                    if event["kind"] == "profile"}
        self.assertEqual(profiles, {"glm", "anthropic"})
        invocations = [json.loads(path.read_text()) for path in
                       (self.path / "logs").rglob("invocation.json")]
        for name, command in commands.items():
            matching = [item for item in invocations if item.get("agent_profile") == name]
            self.assertTrue(matching, name)
            self.assertTrue(all(item["executable"] == command and
                                item["model"] == name + "-model" for item in matching))

    def test_auto_requires_no_project_configuration(self):
        self.env["LOOPER_JOBS"]="auto"
        self.run_loop()

    def test_verify_override(self):
        self.env["TEST_VERIFY"]="exit 1"
        self.env["LOOPER_VERIFY_COMMAND"]="python3 check.py"
        self.run_loop()

    def test_no_activity_log_still_keeps_recovery_records(self):
        self.env["CODEX_JSON_LOG"]="0"
        self.run_loop()
        self.assertTrue(self.state()["finished"])
        self.assertFalse(list((self.path/"logs").glob("*/*.jsonl")))

    def test_tail_reads_parallel_activity(self):
        self.run_loop()
        result=subprocess.run([str(ROOT/"build/looper"),"--tail"],cwd=self.project,env=self.env,
                              capture_output=True,text=True,timeout=load_budget(5))
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        self.assertIn("Project goal satisfied",result.stdout)
        self.assertNotIn("Iter ?",result.stdout)

    def test_custom_task_filename(self):
        self.todo.rename(self.project/"tasks.json")
        self.todo=self.project/"tasks.json"
        self.env["TEST_TODO"]="tasks.json"
        self.run_loop()

    def test_lock_and_interrupt_resume(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="interrupt"
        with (self.path/"output").open("w") as output:
            process=subprocess.Popen(self.command(), cwd=self.project, env=self.env, stdout=output,
                                     stderr=output, start_new_session=True)
            try:
                deadline=time.monotonic()+10
                while time.monotonic()<deadline:
                    if self.events.exists() and any(e["kind"]=="start" for e in self.recorded()):
                        break
                    time.sleep(.05)
                else:
                    self.fail("Worker did not start")
                self.assertIn("already running", self.run_loop(1))
                # SIGINT cancels with retained work; SIGTERM drains and waits.
                os.killpg(process.pid, signal.SIGINT)
                self.assertEqual(process.wait(timeout=load_budget(10)), 130)
            finally:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
        self.env["TEST_MODE"]=""
        shutdown = self.controller_events("shutdown")[-1]["data"]
        self.assertEqual(shutdown["reason"], "cancelled")
        self.assertEqual(shutdown["exit_code"], 130)
        self.assertIn("context canceled", shutdown["error"])
        self.run_loop()
        self.assertEqual((self.project/"T1.txt").read_text(), "done")
        resume = self.controller_events("resume")[-1]["message"]
        self.assertIn(shutdown["session"], resume)
        self.assertIn("cancelled", resume)

    def test_repeated_verification_failure_keeps_code_isolated(self):
        self.tasks(count=1)
        # Every attempt checks in a fresh worktree with fresh timings. The
        # repeat detector must still recognize the identical failure.
        self.env["TEST_VERIFY"]=('pwd; '
            'echo "1/5 Test #1: probe_tests .................. Passed 0.$RANDOM sec"; '
            'echo "Total Test time (real) = 0.$RANDOM sec"; '
            'exit 1')
        self.run_loop(2)
        self.assertFalse((self.project/"T1.txt").exists())
        self.assertEqual(self.state()["attempts"][0]["repairs"],2)
        # The repeated failure triggers one independent diagnosis and the
        # controller records it; the stub records its own consultation.
        self.assertEqual([e["kind"] for e in self.recorded() if e["kind"]=="diagnose"],["diagnose"])
        self.assertEqual(len(self.controller_events("diagnosis")),1)
        # The third matching failure holds the task after the diagnosis, with
        # the raw evidence (artifact paths) retained in the saved feedback.
        self.assertIn("persists after a fresh diagnosis", self.state()["attempts"][0]["feedback"])
        self.assertIn("Artifacts:", self.state()["attempts"][0]["feedback"])
        self.assertEqual(json.loads(self.todo.read_text())["tasks"][0]["status"],"blocked")

    def test_bad_plan_fails_without_starting_workers(self):
        self.env["TEST_MODE"]="bad_plan"
        self.run_loop(1)
        self.assertFalse(any(e["kind"]=="start" for e in self.recorded()))
        shutdown = self.controller_events("shutdown")[-1]["data"]
        self.assertEqual(shutdown["reason"], "error")
        self.assertEqual(shutdown["exit_code"], 1)
        self.assertIn("verification command", shutdown["error"])

    def test_missing_startup_plan_fails_before_work(self):
        # An unreadable --plan file must reject the invocation before any
        # planning, dispatch, or publication, and preserve the backlog.
        backlog = self.todo.read_text()
        output = self.run_loop(1, "--plan", "missing-plan.md")
        self.assertIn("cannot read plan", output)
        self.assertFalse(self.events.exists(), "an agent ran despite rejected startup input")
        errors = self.controller_events("import-error")
        self.assertEqual(len(errors), 1)
        self.assertIn("cannot read plan", errors[0]["message"])
        for name in ("T1.txt", "T2.txt"):
            self.assertFalse((self.project / name).exists(), name)
        self.assertEqual(self.git("rev-parse", "HEAD"), self.head)
        self.assertEqual(self.todo.read_text(), backlog)
        self.assertEqual(self.controller_events("finish"), [])
        shutdown = self.controller_events("shutdown")[-1]["data"]
        self.assertEqual(shutdown["reason"], "error")
        self.assertEqual(shutdown["exit_code"], 1)

    def test_review_failure_and_unapproved_code_are_not_published(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="review_code"
        self.run_loop(1)
        self.assertEqual((self.project/"seed.txt").read_text(),"original\n")
        self.assertFalse(any("project-done" in t.get("tags",[]) for t in json.loads(self.todo.read_text())["tasks"]))

    def test_review_account_failure_fails_fast_with_cause(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="review_account"
        self.run_loop(1)
        errors=self.controller_events("review-error")
        self.assertEqual(len(errors), 1, errors)
        self.assertIn("agent reported", errors[0]["message"])
        self.assertIn("not supported when using Codex with a ChatGPT account", errors[0]["message"])
        shutdown=self.controller_events("shutdown")[-1]["data"]
        self.assertEqual(shutdown["reason"], "error")
        self.assertIn("not supported when using Codex with a ChatGPT account", shutdown["error"])
        self.assertEqual(json.loads(self.todo.read_text())["tasks"][0]["status"], "done")

    def test_final_review_requires_explicit_completion(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="review_incomplete"
        self.run_loop(2)
        self.assertFalse(self.state()["finished"])
        self.assertEqual(json.loads(self.todo.read_text())["tasks"][0]["status"],"done")

    def test_review_backlog_edit_rejects_the_review(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="review_backlog"
        self.run_loop(1)
        data=json.loads(self.todo.read_text())
        self.assertEqual(len(data["tasks"]),1)
        self.assertFalse(any("project-done" in t.get("tags",[]) for t in data["tasks"]))
        self.assertFalse(self.state()["finished"])

    def test_review_goal_edit_rejects_the_review(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="review_goal"
        self.run_loop(1)
        data=json.loads(self.todo.read_text())
        self.assertEqual(len(data["tasks"]),1)

    def test_review_finding_creates_and_completes_work(self):
        self.env["TEST_MODE"]="review_finding"
        self.run_loop()
        tasks=json.loads(self.todo.read_text())["tasks"]
        self.assertEqual(len(tasks),3)
        self.assertTrue(all(t["status"]=="done" for t in tasks))
        self.assertTrue(self.state()["finished"])

    def test_review_followup_export_failure_reports_no_success(self):
        if os.geteuid()==0:
            self.skipTest("read-only directories do not stop the root user")
        self.tasks(count=1)
        self.env["TEST_MODE"]="review_followup_hold"
        self.env["TEST_GATE"]=str(self.path/"release-review")
        self.env["TEST_PROJECT"]=str(self.project)
        self.env["TEST_LOCK_ONCE"]=str(self.path/"review-locked-once")
        self.addCleanup(os.chmod, self.project, 0o755)
        with (self.path/"review-hold-output").open("w") as output:
            process=subprocess.Popen(self.command(), cwd=self.project, env=self.env, stdout=output,
                                     stderr=output)
            try:
                self.wait_for(lambda: self.events.exists() and any(e["kind"]=="review" for e in self.recorded()))
                # The review agent holds its gate: it made the project
                # directory read-only and returns the finding once released,
                # so the export after the finding cannot write the task file.
                (self.path/"release-review").write_text("release")
                self.assertEqual(process.wait(timeout=load_budget(40)), 1,
                                  (self.path/"review-hold-output").read_text())
            finally:
                if process.poll() is None:
                    process.kill();process.wait()
        # The failed commits reported no finding outcome, and the pending
        # export journal records the interrupted application.
        self.assertEqual([e for e in self.controller_events("finding")
                          if "Fix observed behavior" in e["message"]], [])
        self.assertTrue(self.state()["export_pending"])
        # Recovery: make the project writable, replay the pending export, and
        # finish the work. The follow-up arrives through one committed review
        # with exactly one task and one added-follow-up outcome; nothing was
        # invented for the failed attempts.
        os.chmod(self.project, 0o755)
        self.run_loop()
        tasks=json.loads(self.todo.read_text())["tasks"]
        added=[t for t in tasks if t["title"]=="Fix observed behavior"]
        self.assertEqual(len(added),1)
        self.assertTrue(all(t["status"]=="done" for t in tasks))
        self.assertTrue(self.state()["finished"])
        self.assertEqual(len([e for e in self.controller_events("finding")
                          if "Added follow-up" in e["message"]]),1)

    def test_new_baseline_after_draining_the_previous_run(self):
        self.env["MAX_ITERATIONS"]="1"
        self.run_loop(2)
        (self.project/"seed.txt").write_text("user edit\n")
        self.run_loop()
        self.assertEqual((self.project/"seed.txt").read_text(),"user edit\n")

    def test_hard_kill_reaps_previous_agent_before_resume(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="interrupt"
        with (self.path/"hard-output").open("w") as output:
            process=subprocess.Popen(self.command(), cwd=self.project, env=self.env, stdout=output,
                                     stderr=output, start_new_session=True)
            try:
                deadline=time.monotonic()+10
                while time.monotonic()<deadline:
                    if self.events.exists() and any(e["kind"]=="start" for e in self.recorded()):
                        break
                    time.sleep(.05)
                else:
                    self.fail("Worker did not start")
                pid=next(e["pid"] for e in self.recorded() if e["kind"]=="start")
                process.kill()
                process.wait(timeout=load_budget(5))
                self.assertEqual(self.controller_events("shutdown"), [])
                db_path = next((self.path / "logs").glob("*/runtime/state.db"))
                with sqlite3.connect(db_path) as db:
                    previous = db.execute("select id from sessions order by rowid desc limit 1").fetchone()[0]
                    last_event = db.execute("select max(id) from events").fetchone()[0]
                self.env["TEST_MODE"]=""
                self.run_loop()
                recovery_event = self.controller_events("recovery")[-1]
                recovery = recovery_event["message"]
                self.assertIn(previous, recovery)
                self.assertIn("uncleanly", recovery)
                self.assertIn(str(last_event), recovery)
                self.assertEqual(recovery_event["data"]["previous_session"], previous)
                self.assertEqual(recovery_event["data"]["last_event_id"], last_event)
                self.assertTrue(recovery_event["data"]["unclean"])
                self.assertEqual(recovery_event["data"]["previous_exit_reason"], "unknown")
                self.assertIn("uncleanly", self.controller_events("resume")[-1]["message"])
                with sqlite3.connect(db_path) as db:
                    outcome, ended = db.execute("select outcome,ended from sessions where id=?", (previous,)).fetchone()
                self.assertEqual(outcome, "interrupted")
                self.assertIsNone(ended)
                # Reaped children can briefly remain zombies under a container init.
                status=subprocess.run(["ps","-o","stat=","-p",str(pid)],capture_output=True,text=True).stdout.strip()
                self.assertTrue(not status or status.startswith("Z"),status)
            finally:
                if process.poll() is None:
                    os.killpg(process.pid,signal.SIGKILL)
                    process.wait()


    def test_external_edit_stops_publication_and_can_resume(self):
        self.tasks(count=1)
        with (self.path/"external-output").open("w") as output:
            process=subprocess.Popen(self.command(),cwd=self.project,env=self.env,stdout=output,stderr=output)
            try:
                self.wait_for(lambda:self.events.exists() and any(e["kind"]=="start" for e in self.recorded()))
                (self.project/"seed.txt").write_text("user edit\n")
                self.assertEqual(process.wait(timeout=load_budget(15)),1)
                self.assertEqual((self.project/"seed.txt").read_text(),"user edit\n")
                self.assertFalse((self.project/"T1.txt").exists())
                (self.project/"seed.txt").write_text("original\n")
                self.run_loop()
                self.assertEqual(len([e for e in self.recorded() if e["kind"]=="start"]),1)
            finally:
                if process.poll() is None:process.kill();process.wait()


    # AX-015 live fixtures: startup guard refusals through the real binary
    # under contract version 2 give distinct actionable evidence.
    def stop_with_retained_work(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="interrupt"
        with (self.path/"guard-output").open("w") as output:
            process=subprocess.Popen(self.command(),cwd=self.project,env=self.env,stdout=output,
                                     stderr=output,start_new_session=True)
            try:
                self.wait_for(lambda:self.events.exists() and any(e["kind"]=="start" for e in self.recorded()))
                os.killpg(process.pid,signal.SIGKILL)
                process.wait(timeout=load_budget(10))
            finally:
                if process.poll() is None:
                    os.killpg(process.pid,signal.SIGKILL)
                    process.wait()
        self.env["TEST_MODE"]=""

    def guard_document(self):
        result=subprocess.run(self.command("--contract","v2","--json"),cwd=self.project,env=self.env,
                              capture_output=True,text=True,timeout=load_budget(40))
        self.assertEqual(result.returncode,1,result.stdout+result.stderr)
        self.assertEqual(result.stderr,"")
        # A startup the guard refuses streams no journal line (AX-022): journal
        # events begin only after a successful start, so the stream is exactly
        # the launch event and the error document, which is always last.
        lines=[line for line in result.stdout.splitlines() if line.strip()]
        self.assertTrue(lines,"the run printed nothing")
        documents=[json.loads(line) for line in lines]
        self.assertEqual([d["kind"] for d in documents],["launch","looper.error"],
                         result.stdout)
        return documents[-1]["error"]

    def test_guard_documents_report_concrete_differences(self):
        self.stop_with_retained_work()
        original=self.todo.read_text()
        saved_head=self.state()["head"]
        # An actual contract change names the changed task identity and the
        # retained work.
        backlog=json.loads(original)
        backlog["tasks"][0]["title"]="edited outside Looper"
        self.todo.write_text(json.dumps(backlog))
        error=self.guard_document()
        self.assertEqual(error["code"],"backlog_contract_changed")
        self.assertEqual(error["observed"]["task_contract_changes"],["T1"])
        self.assertEqual(error["observed"]["retained_tasks"],["T1"])
        self.assertTrue(error["observed"]["parsed_backlog"].startswith("differs"))
        self.assertTrue(error["observed"]["task_file_digest"]!=error["expected"]["task_file_digest"])
        self.assertTrue(any("inspect T1" in a.get("command","") for a in error["next_actions"]))
        self.assertTrue(error["unknown_evidence"])
        # A task-data edit without a contract change is still a refusal and
        # names the task through task_data_changes (AX-018).
        data=json.loads(original)
        data["tasks"][0]["status"]="done"
        self.todo.write_text(json.dumps(data))
        error=self.guard_document()
        self.assertEqual(error["code"],"backlog_contract_changed")
        self.assertEqual(error["observed"]["task_contract_changes"],[])
        self.assertEqual(error["observed"]["task_data_changes"],["T1"])
        self.assertTrue(error["observed"]["parsed_backlog"].startswith("differs"))
        # An unborn HEAD is reported as unborn, never as branch movement.
        self.todo.write_text(original)
        branch=self.git("symbolic-ref","--short","HEAD")
        self.git("update-ref","-d","refs/heads/"+branch)
        error=self.guard_document()
        self.assertEqual(error["code"],"branch_moved")
        self.assertEqual(error["observed"]["head"],"unborn")
        self.assertTrue(error["observed"]["unborn_head"])
        self.assertEqual(error["observed"]["committed_changes"],"unknown")
        self.assertIn("unborn",error["message"])
        self.assertNotIn("branch moved",error["message"])
        self.assertTrue(any("no commit exists" in u for u in error["unknown_evidence"]))
        self.assertEqual(self.state()["head"],saved_head)
        # Doctor names the same unborn state in its branch finding.
        result=subprocess.run([str(ROOT/"build/looper"),"doctor","--json"],cwd=self.project,
                              env=self.env,capture_output=True,text=True,timeout=load_budget(40))
        report=json.loads(result.stdout)
        finding=next(f for f in report["findings"] if f["code"]=="branch_moved")
        self.assertIn("unborn",finding["message"])
        self.assertIn(saved_head,finding["message"])

    def test_formatting_only_edit_resumes_through_adoption(self):
        # AX-018: a formatting-only edit of the task file no longer refuses.
        # The run adopts the equivalent bytes, journals one backlog-format
        # event with both digests, and resumes the retained attempt.
        self.stop_with_retained_work()
        original=self.todo.read_text()
        self.todo.write_text(json.dumps(json.loads(original),separators=(",",":")))
        self.run_loop()
        adoption=self.controller_events("backlog-format")
        self.assertTrue(adoption,"the adoption must journal one backlog-format event")
        self.assertIn("formatting",adoption[0]["message"])
        self.assertIn("equivalent content",adoption[0]["message"])
        state=self.state()
        self.assertTrue(state["finished"])

    def test_resource_cleanup_after_cancelled_worker_recovery(self):
        self.resource_cleanup_recovery(signal.SIGINT, 130, "retained")

    def test_resource_cleanup_after_killed_worker_recovery(self):
        self.resource_cleanup_recovery(signal.SIGKILL, -signal.SIGKILL, "active")

    def test_failure_summaries_bounded_queryable_and_distinguishable(self):
        # AX-021 end to end: a verification that exits 4 with a long, secret-
        # bearing log and a cleanup that fails only on an already-absent path
        # both journal bounded summaries; one focused query retrieves both
        # classes with distinct outcomes and exit statuses.
        self.tasks(count=1)
        self.env["TEST_VERIFY"] = ('for i in $(seq 1 300); do echo "filler line $i"; done; '
            'echo "Authorization: Bearer sk-pythonsecret12"; '
            'echo "tail-marker: assertion failed" >&2; exit 4')
        self.env["LOOPER_SETUP_COMMAND"] = (
            'cat > "$LOOPER_RESOURCE_CLEANUP_FILE" <<\'CLEANUP\'\n'
            'rm "$LOOPER_ARTIFACT_DIR/database"\n'
            'CLEANUP\n')
        self.run_loop(2)
        outcomes = {}
        failures = self.controller_events("failure")
        self.assertTrue(failures, "a failed verification and a failed cleanup must journal summaries")
        for e in failures:
            summary = e["data"]["failure"]
            outcomes[(summary["operation"], summary["outcome"])] = summary
            self.assertLessEqual(len(summary.get("context", "")), 2048)
            self.assertNotIn("sk-pythonsecret12", json.dumps(summary))
            self.assertTrue(summary["evidence"], "every summary names Looper-owned evidence")
            if summary["operation"] == "verify":
                self.assertEqual(e["data"]["task_id"], "T1")
                self.assertIn("tail-marker", summary["context"])
        # An exit failure of the verification logic and a cleanup that failed
        # only on already-absent paths are distinguishable by outcome and exit
        # status, not by message prose.
        self.assertIn(("verify", "exit_failure"), outcomes)
        self.assertIn(("resource-cleanup", "cleanup_already_absent"), outcomes)
        self.assertEqual(outcomes[("verify", "exit_failure")]["exit_status"], 4)
        self.assertEqual(outcomes[("resource-cleanup", "cleanup_already_absent")]["exit_status"], 1)
        self.assertIn("No such file or directory", outcomes[("resource-cleanup", "cleanup_already_absent")]["context"])
        # The cleanup notice keeps its documented shape beside the summary.
        notices = self.controller_events("resource-cleanup-error")
        self.assertTrue(notices)
        self.assertIn("Cleanup retries at the next completion or restart.", notices[0]["message"])
        # One focused query through the public CLI retrieves every summary.
        result = subprocess.run(
            [str(ROOT / "build/looper"), "events", "--contract", "v2", "--json",
             "--kind", "failure", str(self.todo)],
            cwd=self.project, env=self.env, capture_output=True, text=True, timeout=load_budget(20))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        document = json.loads(result.stdout)
        self.assertTrue(document["items"])
        self.assertEqual({i["kind"] for i in document["items"]}, {"failure"})
        verify_items = [i for i in document["items"] if i["data"]["failure"]["operation"] == "verify"]
        self.assertTrue(verify_items)
        self.assertEqual({i["task_id"] for i in verify_items}, {"T1"})
        self.assertLessEqual(len(result.stdout), 65536, "failure summaries must stay bounded")

    def resource_cleanup_recovery(self, stop_signal, exit_code, lease_status):
        self.tasks(count=1)
        self.env["LOOPER_SETUP_COMMAND"] = r'''set -e
touch "$LOOPER_ARTIFACT_DIR/database"
cat > "$LOOPER_RESOURCE_CLEANUP_FILE" <<'CLEANUP'
rm -f "$LOOPER_ARTIFACT_DIR/database"
echo cleaned > "$LOOPER_ARTIFACT_DIR/cleanup-proof"
CLEANUP
'''
        self.env["TEST_MODE"] = "interrupt"
        with (self.path / "resource-output").open("w") as output:
            process = subprocess.Popen(self.command(), cwd=self.project, env=self.env,
                                       stdout=output, stderr=output)
            try:
                self.wait_for(lambda: self.events.exists() and any(
                    e["kind"] == "start" and (Path(e["cwd"]) / "T1.txt").exists()
                    for e in self.recorded()))
                process.send_signal(stop_signal)
                self.assertEqual(process.wait(timeout=load_budget(15)), exit_code)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
        runtime = next((self.path / "logs").glob("*/runtime"))
        retained = list(runtime.glob("task-*/work-*/database"))
        self.assertEqual(len(retained), 1)
        lease = json.loads((retained[0].parent / "resources.json").read_text())
        self.assertEqual(lease["status"], lease_status)
        self.assertTrue(lease["session"])
        self.env["TEST_MODE"] = "resume_partial"
        self.env["LOOPER_SETUP_COMMAND"] += '\ntouch "$LOOPER_ARTIFACT_DIR/new-setup"\n'
        self.env["LOOPER_VERIFY_COMMAND"] = 'test -f "$LOOPER_ARTIFACT_DIR/new-setup"'
        self.run_loop()
        self.assertFalse(list(runtime.glob("task-*/work-*/database")))
        self.assertFalse(list(runtime.glob("task-*/checks/*/database")))
        self.assertTrue((retained[0].parent / "cleanup-proof").exists())
        new_proofs = [p for p in runtime.glob("task-*/**/cleanup-proof")
                      if p.parent != retained[0].parent]
        self.assertEqual(len(new_proofs), 2)
        for proof in new_proofs:
            self.assertTrue((proof.parent / "new-setup").exists())
        self.assertEqual(json.loads(self.todo.read_text())["tasks"][0]["status"], "done")

    def test_setup_failure_can_be_repaired_by_worker(self):
        self.tasks(count=1)
        self.env["LOOPER_SETUP_COMMAND"]="test -f T1.txt"
        self.run_loop()
        self.assertEqual((self.project/"T1.txt").read_text(),"done")

    def test_verification_mutation_cannot_publish(self):
        self.tasks(count=1)
        self.env["TEST_VERIFY"]="echo changed > seed.txt"
        self.run_loop(2)
        self.assertEqual((self.project/"seed.txt").read_text(),"original\n")
        self.assertFalse((self.project/"T1.txt").exists())

    def test_json_progress_is_valid_and_terminal_independent(self):
        self.tasks(count=1)
        output=self.run_loop(0,"--json")
        records=[json.loads(line) for line in output.splitlines()]
        self.assertEqual(records[-1]["kind"],"finish")
        self.assertNotIn("\x1b",output)

    def test_installed_binary_runs_without_source_tree(self):
        prefix=self.path/"installed"
        package=self.path/"package/bin";package.mkdir(parents=True)
        shutil.copy2(ROOT/"build/looper",package/"looper")
        shutil.copy2(ROOT/"install.sh",package/"looper-install")
        # T056: run the installer with a sandbox HOME and bound user prefix.
        home=self.path/"installer-home"
        install_env={k:v for k,v in os.environ.items() if k not in ("HOME","PREFIX","CODEX_HOME","LOOPER_USER_PREFIX")}
        install_env["HOME"]=str(home);install_env["LOOPER_USER_PREFIX"]=str(prefix)
        subprocess.run([str(package/"looper-install"),"--prefix",str(prefix),"--skip-skills"],check=True,capture_output=True,text=True,env=install_env)
        self.assertFalse((home/".local").exists())
        result=subprocess.run([str(prefix/"bin/looper"),"--once"],cwd=self.project,env=self.env,capture_output=True,text=True,timeout=load_budget(40))
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        self.assertTrue(self.state()["finished"])

    def save_state(self, state):
        with sqlite3.connect(next((self.path / "logs").glob("*/runtime/state.db"))) as db:
            db.execute("update state set data=? where id=1", (json.dumps(state),))

    def interrupted_publication(self):
        self.tasks(count=1)
        self.run_loop()
        state=self.state()
        a=state["attempts"][0]
        state["finished"]=False
        state["integrated"]=0
        state["reviewed_at"]=0
        state["reviewed_tip"]=""
        state["backlog"]["tasks"][0]["status"]="doing"
        self.todo.write_text(json.dumps(state["backlog"]))
        state["export_hash"]=hashlib.sha256(self.todo.read_bytes()).hexdigest()
        state["publication"]=dict(a["verification"],before=self.git("rev-parse",a["base"]+"^{tree}"),attempt_id=a["id"])
        state["tip"]=state["publication"]["base"]
        state["published"]=state["publication"]["before"]
        a["phase"]="verifying"
        return state

    def test_completed_receipt_resumes_without_implementation(self):
        state=self.interrupted_publication()
        state.pop("publication")
        attempt=state["attempts"][0]
        attempt["phase"]="running"
        attempt.pop("result_commit")
        attempt.pop("summary")
        (self.project/"T1.txt").unlink()
        self.save_state(state)
        self.run_loop()
        self.assertEqual(len([e for e in self.recorded() if e["kind"]=="start"]),1)
        self.assertTrue(self.state()["finished"])

    def test_python_blocked_partial_work_is_migrated(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="blocked"
        self.run_loop(2)
        state=self.state();state["version"]=1;state["finished"]=True
        state["attempts"][0]["phase"]="done"
        state["plan"]["tasks"]={s["task_id"]:s for s in state["plan"]["tasks"]}
        data=json.loads(self.todo.read_text());data["tasks"][0]["status"]="todo"
        self.todo.write_text(json.dumps(data))
        for k in ("branch","backlog","export_hash","integrated","reviewed_at","reviewed_tip"):
            state.pop(k,None)
        db=next((self.path/"logs").glob("*/runtime/state.db"));db.unlink()
        legacy=db.parent.parent/"parallel/state.json";legacy.parent.mkdir()
        legacy.write_text(json.dumps(state))
        self.env["TEST_MODE"]="resume_partial"
        self.run_loop()
        self.assertTrue(self.state()["finished"])

    def test_publication_journal_after_apply(self):
        self.save_state(self.interrupted_publication())
        self.run_loop()
        self.assertEqual(len([e for e in self.recorded() if e["kind"]=="start"]),1)
        self.assertTrue(self.state()["finished"])

    def test_publication_journal_before_apply(self):
        state=self.interrupted_publication()
        (self.project/"T1.txt").unlink()
        self.save_state(state)
        self.run_loop()
        self.assertEqual(len([e for e in self.recorded() if e["kind"]=="start"]),1)
        self.assertEqual((self.project/"T1.txt").read_text(),"done")

    def test_export_journal_replays(self):
        self.tasks(count=1)
        self.run_loop()
        state=self.state()
        before=json.dumps(dict(state["backlog"],goal="old serialized state")).encode()
        self.todo.write_bytes(before)
        state["export_before"]=hashlib.sha256(before).hexdigest()
        state["export_pending"]=True
        self.save_state(state)
        self.run_loop()
        self.assertEqual(json.loads(self.todo.read_text()),state["backlog"])
        self.assertEqual(len([e for e in self.recorded() if e["kind"]=="start"]),1)

    def test_python_publication_migration(self):
        state=self.interrupted_publication()
        state["version"]=1
        state["plan"]["tasks"]={s["task_id"]:s for s in state["plan"]["tasks"]}
        for k in ("branch","backlog","export_hash","integrated","reviewed_at","reviewed_tip"):
            state.pop(k,None)
        db=next((self.path/"logs").glob("*/runtime/state.db"))
        db.unlink()
        legacy=db.parent.parent/"parallel/state.json"
        legacy.parent.mkdir()
        legacy.write_text(json.dumps(state))
        self.run_loop()
        self.assertEqual(len([e for e in self.recorded() if e["kind"]=="start"]),1)
        self.assertTrue(self.state()["finished"])
        self.assertTrue(legacy.exists())

    def test_report_recovery_does_not_repeat_implementation(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="malformed"
        self.run_loop()
        self.assertEqual([e["kind"] for e in self.recorded() if e["kind"] in ("start","recover")],["start","recover"])

    def test_failed_task_does_not_stop_independent_task(self):
        self.env["TEST_MODE"]="mixed"
        self.run_loop(2)
        self.assertFalse((self.project/"T1.txt").exists())
        self.assertEqual((self.project/"T2.txt").read_text(),"done")

    def test_blocked_retry_retains_partial_work(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="blocked"
        self.run_loop(2)
        data=json.loads(self.todo.read_text());data["tasks"][0]["status"]="todo"
        self.todo.write_text(json.dumps(data))
        self.env["TEST_MODE"]="resume_partial"
        self.run_loop()
        self.assertEqual((self.project/"T1.txt").read_text(),"done")

    def test_worker_followup_is_implemented(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="followup"
        self.run_loop()
        self.assertEqual(len(self.state()["backlog"]["tasks"]),2)
        self.assertTrue(all(t["status"]=="done" for t in self.state()["backlog"]["tasks"]))

    def test_milestone_review_drains_before_new_work(self):
        self.tasks(count=3)
        self.run_loop(0,"--jobs","1","--review-every","1")
        kinds=[e["kind"] for e in self.recorded()]
        self.assertEqual(kinds.count("review"),3)
        starts=[i for i,k in enumerate(kinds) if k=="start"]
        self.assertIn("review",kinds[starts[0]:starts[1]])

    def wait_for(self, predicate, timeout=None):
        if timeout is None:
            timeout = load_budget(15)
        deadline=time.monotonic()+timeout
        while time.monotonic()<deadline:
            if predicate(): return
            time.sleep(.05)
        self.fail("Timed out waiting for Looper")

    def test_continuous_run_imports_work_and_stops(self):
        self.tasks(count=1)
        self.env["LOOPER_POLL_SECONDS"]="1"
        command=self.command();command.remove("--once")
        with (self.path/"continuous-output").open("w") as output:
            process=subprocess.Popen(command,cwd=self.project,env=self.env,stdout=output,stderr=output)
            try:
                self.wait_for(lambda: self.events.exists() and any(e["kind"]=="review" for e in self.recorded()))
                self.wait_for(lambda: self.state()["finished"])
                data=json.loads(self.todo.read_text())
                data["tasks"].append({"id":"T2","title":"New work","status":"todo","priority":1})
                self.todo.write_text(json.dumps(data))
                self.wait_for(lambda:(self.project/"T2.txt").exists())
                stop=subprocess.run([str(ROOT/"build/looper"),"stop"],cwd=self.project,env=self.env,capture_output=True,text=True)
                self.assertEqual(stop.returncode,0,stop.stderr)
                self.assertEqual(process.wait(timeout=load_budget(15)),0)
            finally:
                if process.poll() is None: process.kill();process.wait()

    def compact_status(self):
        result = subprocess.run(
            [str(ROOT / "build/looper"), "status", "--contract", "v2", "--json", str(self.todo)],
            cwd=self.project, env=self.env, capture_output=True, text=True, timeout=load_budget(20))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertLessEqual(len(result.stdout), 8192, "the compact document exceeded its byte budget")
        return json.loads(result.stdout)

    def compact_inspect(self, *args):
        # inspect takes the task ID as its positional argument; the task file
        # resolves from the project directory like every other command.
        result = subprocess.run(
            [str(ROOT / "build/looper"), "inspect", "T1", "--contract", "v2", "--json", *args],
            cwd=self.project, env=self.env, capture_output=True, text=True, timeout=load_budget(20))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertLessEqual(len(result.stdout), 8192, "the compact inspection exceeded its byte budget")
        return json.loads(result.stdout)

    def test_compact_status_observes_a_live_run(self):
        # AX-004: one bounded status document answers identity, ownership,
        # active work, the waiting reason, and the cursor, through the public
        # CLI against a real controller.
        self.tasks(count=1)
        self.env["LOOPER_POLL_SECONDS"] = "1"
        self.env["TEST_MODE"] = "interrupt"  # one worker stays in flight
        command = self.command(); command.remove("--once")
        with (self.path / "continuous-output").open("w") as output:
            process = subprocess.Popen(command, cwd=self.project, env=self.env,
                                       stdout=output, stderr=output)
            try:
                self.wait_for(lambda: self.events.exists()
                              and any(e["kind"] == "start" for e in self.recorded()))
                document = self.compact_status()
                self.assertEqual(document["kind"], "looper.status.compact")
                controller = document["controller"]
                self.assertEqual(controller["state"], "controlling")
                self.assertTrue(controller["owned"])
                self.assertEqual(controller["ownership_evidence"], "lock")
                self.assertTrue(controller["view_matches"])
                active = document["work"]["active"]
                self.assertEqual([a["task_id"] for a in active], ["T1"])
                self.assertIn(active[0]["progress"], ("unknown", "advancing", "quiet"))
                self.assertGreater(document["cursor"]["event_id"], 0)
                self.assertNotIn("statistics", document)
                self.assertNotIn("timing", document)
                self.assertTrue(document["detail"]["full"].startswith("looper status"))
                # A drain request is visible while the active work finishes.
                stop = subprocess.run([str(ROOT / "build/looper"), "stop"], cwd=self.project,
                                      env=self.env, capture_output=True, text=True)
                self.assertEqual(stop.returncode, 0, stop.stderr)
                draining = self.compact_status()
                self.assertEqual(draining["controller"]["state"], "draining")
                self.assertEqual(draining["waiting"]["reason"], "draining")
                # Piping the plain status still shows liveness and the wait.
                plain = subprocess.run(
                    [str(ROOT / "build/looper"), "status", str(self.todo)],
                    cwd=self.project, env=self.env, capture_output=True, text=True, timeout=load_budget(20))
                self.assertEqual(plain.returncode, 0, plain.stderr)
                for expected in ("Controller:", "Active:", "Waiting:", "Cursor:"):
                    self.assertIn(expected, plain.stdout, plain.stdout)
                self.assertLessEqual(len(plain.stdout), 8192, "plain piped status is unbounded")
                # A hard kill leaves stale metadata: the run is stopped and the
                # leftover record is named as the evidence, not as an owner.
                process.kill(); process.wait()
                killed = self.compact_status()
                self.assertEqual(killed["controller"]["state"], "stopped")
                self.assertFalse(killed["controller"]["owned"])
                self.assertEqual(killed["controller"]["ownership_evidence"], "metadata")
                self.assertEqual(killed["waiting"]["reason"], "controller_stopped")
            finally:
                if process.poll() is None: process.kill(); process.wait()

    def test_compact_inspect_reports_retained_work(self):
        # AX-005: one bounded inspection document answers the task contract,
        # the current attempt, active versus retained work, and the paged
        # history routes, through the public CLI against a real run.
        self.tasks(count=1)
        self.env["LOOPER_POLL_SECONDS"] = "1"
        self.env["TEST_MODE"] = "interrupt"  # one worker stays in flight
        command = self.command(); command.remove("--once")
        with (self.path / "continuous-inspect").open("w") as output:
            process = subprocess.Popen(command, cwd=self.project, env=self.env,
                                       stdout=output, stderr=output)
            try:
                self.wait_for(lambda: self.events.exists()
                              and any(e["kind"] == "start" for e in self.recorded()))
                live = self.compact_inspect()
                self.assertEqual(live["kind"], "looper.inspect.compact")
                self.assertEqual(live["task"]["id"], "T1")
                self.assertNotEqual(live["task"]["contract_digest"], "")
                current = live["attempts"]["current"]
                self.assertEqual(current["phase"], "running")
                self.assertTrue(current["active"])
                self.assertFalse(current["retained"], "a live controller owns the work")
                self.assertIn(current["output"]["progress"], ("unknown", "advancing", "quiet"))
                self.assertTrue(live["attempts"]["total"] >= 1)
                self.assertTrue(live["detail"]["events"].endswith("--task T1"))
                # A hard kill leaves the attempt retained: active work no
                # controller owns, stated instead of implied.
                process.kill(); process.wait()
                retained = self.compact_inspect()
                self.assertFalse(retained["controller"]["owned"])
                current = retained["attempts"]["current"]
                self.assertTrue(current["active"])
                self.assertTrue(current["retained"], "an active attempt without a controller is retained")
                # The complete document stays one explicit flag away and
                # carries the whole unpaged history; it is not bounded.
                full = subprocess.run(
                    [str(ROOT / "build/looper"), "inspect", "T1", "--contract", "v2",
                     "--full", "--json"],
                    cwd=self.project, env=self.env, capture_output=True, text=True, timeout=load_budget(20))
                self.assertEqual(full.returncode, 0, full.stdout + full.stderr)
                document = json.loads(full.stdout)
                self.assertNotIn("contract_version", document)
                self.assertGreaterEqual(len(document["attempts"]), 1)
                self.assertEqual(document["schema_version"], 1)
            finally:
                if process.poll() is None: process.kill(); process.wait()

    def test_stop_without_controller_does_not_drain_next_run(self):
        # A saved run with no live controller: stop must write nothing and
        # the next start must run work instead of draining on a stale request.
        self.tasks(count=1)
        self.run_loop()
        runtime = next((self.path / "logs").glob("*/runtime"))
        stop = subprocess.run([str(ROOT / "build/looper"), "stop"], cwd=self.project,
                              env=self.env, capture_output=True, text=True)
        self.assertEqual(stop.returncode, 1, stop.stdout + stop.stderr)
        self.assertIn("No controller is running", stop.stdout)
        self.assertFalse((runtime / "stop").exists(), "stop without an owner wrote a request")
        data = json.loads(self.todo.read_text())
        data["tasks"].append({"id": "T2", "title": "New work", "status": "todo", "priority": 1})
        self.todo.write_text(json.dumps(data))
        self.run_loop()
        self.assertTrue((self.project / "T2.txt").exists())
        shutdowns = self.controller_events("shutdown")
        self.assertEqual(shutdowns[-1]["data"]["reason"], "complete",
                         "a stop without an owner drained the next run")

    def test_invalid_startup_input_fails(self):
        self.tasks(count=1)
        self.todo.write_text("{not json")
        self.run_loop(1)

    def test_continuous_run_survives_invalid_task_file(self):
        self.tasks(count=1)
        self.env["LOOPER_POLL_SECONDS"]="1"
        command=self.command();command.remove("--once")
        with (self.path/"wait-output").open("w") as output:
            process=subprocess.Popen(command,cwd=self.project,env=self.env,stdout=output,stderr=output)
            try:
                self.wait_for(lambda: self.state_eventually() and self.state_eventually()["finished"])
                original=self.todo.read_text()
                starts_before=len([e for e in self.recorded() if e["kind"]=="start"])
                # Malformed JSON keeps the run alive; the file is preserved.
                self.todo.write_text("{broken")
                self.wait_for(lambda:"needs correction" in (self.path/"wait-output").read_text())
                time.sleep(2.5)
                self.assertIsNone(process.poll())
                self.assertEqual(self.todo.read_text(),"{broken")
                self.assertEqual(len([e for e in self.recorded() if e["kind"]=="start"]),starts_before)
                self.assertEqual((self.path/"wait-output").read_text().count("needs correction"),1)
                # An invalid task status is a new problem and is reported once.
                self.todo.write_text(json.dumps({"schema_version":1,"source_files":[],
                    "tasks":[{"id":"T1","title":"Task 1","status":"bogus","priority":1}]}))
                self.wait_for(lambda:(self.path/"wait-output").read_text().count("needs correction")>=2)
                # Restoring the previous valid file clears the diagnostic.
                self.todo.write_text(original)
                self.wait_for(lambda:"accepted again" in (self.path/"wait-output").read_text())
                self.assertIsNone(process.poll())
                # A corrected backlog with a new task resumes without repeats.
                data=json.loads(original)
                data["tasks"].append({"id":"T2","title":"New work","status":"todo","priority":1})
                self.todo.write_text(json.dumps(data))
                self.wait_for(lambda:(self.project/"T2.txt").exists())
                self.assertEqual(len([e for e in self.recorded() if e["kind"]=="start" and e.get("task")=="T1"]),1)
                stop=subprocess.run([str(ROOT/"build/looper"),"stop"],cwd=self.project,env=self.env,capture_output=True,text=True)
                self.assertEqual(stop.returncode,0,stop.stderr)
                self.assertEqual(process.wait(timeout=load_budget(15)),0)
            finally:
                if process.poll() is None:process.kill();process.wait()

    def test_interrupt_during_input_wait_returns_130(self):
        self.tasks(count=1)
        self.env["LOOPER_POLL_SECONDS"]="1"
        command=self.command();command.remove("--once")
        with (self.path/"interrupt-output").open("w") as output:
            process=subprocess.Popen(command,cwd=self.project,env=self.env,stdout=output,stderr=output,start_new_session=True)
            try:
                self.wait_for(lambda: self.state_eventually() and self.state_eventually()["finished"])
                self.todo.write_text("{broken")
                self.wait_for(lambda:"needs correction" in (self.path/"interrupt-output").read_text())
                os.killpg(process.pid,signal.SIGINT)
                self.assertEqual(process.wait(timeout=load_budget(10)),130)
            finally:
                if process.poll() is None:os.killpg(process.pid,signal.SIGKILL);process.wait()

    def test_advisory_failure_triggers_review_without_claiming_failure(self):
        self.tasks(count=1)
        self.env["LOOPER_ASSESS_COMMAND"]="echo '{\"outcome\":\"fail\"}' > \"$LOOPER_ASSESS_REPORT\""
        self.run_loop()
        self.assertEqual((self.project/"T1.txt").read_text(),"done")
        with sqlite3.connect(next((self.path/"logs").glob("*/runtime/state.db"))) as db:
            self.assertEqual(db.execute("select result from checks where kind='semantic'").fetchone()[0],"fail")

    def test_nonzero_assessment_exit_stays_advisory(self):
        self.tasks(count=1)
        self.env["LOOPER_ASSESS_COMMAND"]="exit 3"
        self.run_loop()
        self.assertTrue(self.state()["finished"])
        errors=list((self.path/"logs").glob("*/runtime/task-*/checks/*/assessment-error.json"))
        self.assertEqual(len(errors),1,errors)


class BenchmarkTests(unittest.TestCase):
    """AX-027: benchmark public workflows against deterministic audit fixtures.

    Each fixture reproduces one verified incident from the 2026-11-05 CLI
    interaction audit (/notes/looper-cli-interaction-audit-2026-11-05.md)
    with the stub agent and drives the recovery procedure through documented
    public commands only. Assertions check observable outcomes against the
    release gates; output bytes, command latency, and observation counts are
    recorded so docs/research/AGENT-EXPERIENCE-RESULTS.md reports measured
    numbers. Set LOOPER_BENCHMARK_RECORD=<path> to append one JSON line per
    public command the fixtures run.
    """

    def setUp(self):
        project_fixture(self)
        # The benchmark backlogs exceed the suite's shared dispatch limit, and
        # observation must see completed journals, not retained work. A one-
        # second poll keeps the fixture windows (input holds, request claims)
        # short without racing the controller.
        self.env["MAX_ITERATIONS"] = "0"
        self.env["LOOPER_POLL_SECONDS"] = "1"
        self.fixture = ""
        self.measurements = []
        record = os.environ.get("LOOPER_BENCHMARK_RECORD")
        self.record_path = Path(record) if record else None
        self.addCleanup(self.flush_measurements)

    def flush_measurements(self):
        if self.record_path and self.measurements:
            with self.record_path.open("a") as record:
                for entry in self.measurements:
                    record.write(json.dumps(entry) + "\n")

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.project), *args], stderr=subprocess.PIPE).decode().strip()

    def tasks(self, dependencies=None, count=2):
        self.todo.write_text(json.dumps({"schema_version": 1, "source_files": [], "tasks": [
            {"id": "T" + str(i), "title": "Task " + str(i), "status": "todo", "priority": 1,
             "depends_on": (dependencies or {}).get("T" + str(i), [])} for i in range(1, count + 1)]}))

    def command(self, *args):
        return [str(ROOT / "build/looper"), "--once", *args, str(self.todo)]

    def run_once(self, expected=0, *args):
        result = subprocess.run(self.command(*args), cwd=self.project, env=self.env,
                                capture_output=True, text=True, timeout=load_budget(120))
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
        return result.stdout + result.stderr

    def recorded(self):
        if not self.events.exists():
            return []
        return [json.loads(x) for x in self.events.read_text().splitlines()]

    def cli(self, *args, expect=0, procedure="observe", timeout=90):
        """Run one public command, assert its exit code, and record its cost."""
        started = time.monotonic()
        result = subprocess.run([str(ROOT / "build/looper"), *args], cwd=self.project, env=self.env,
                                capture_output=True, text=True, timeout=load_budget(timeout))
        self.measurements.append({
            "fixture": self.fixture, "procedure": procedure,
            "command": "looper " + " ".join(args), "exit": result.returncode,
            "seconds": round(time.monotonic() - started, 3),
            "stdout_bytes": len(result.stdout.encode()),
            "stderr_bytes": len(result.stderr.encode())})
        if expect is not None:
            self.assertEqual(result.returncode, expect,
                             "looper %s\n%s%s" % (" ".join(args), result.stdout, result.stderr))
        return result

    def compact(self, *args):
        """One bounded compact status document through the public CLI."""
        result = self.cli("status", "--contract", "v2", "--json", str(self.todo), *args)
        self.assertLessEqual(len(result.stdout.encode()), 8192,
                             "the compact document exceeded its byte budget")
        return json.loads(result.stdout)

    def events_page(self, *args, expect=0):
        result = self.cli("events", "--contract", "v2", "--json",
                          str(self.todo), *args, expect=expect)
        return json.loads(result.stdout)

    def wait_for(self, predicate, timeout=None):
        if timeout is None:
            timeout = load_budget(30)
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                return
            time.sleep(.05)
        self.fail("Timed out waiting for Looper")

    def test_large_history_observation_stays_bounded(self):
        # Audit patterns 1 and 3: mature journals made every status line a
        # 79k-500KB payload and every events call a whole-history render.
        # The same backlog must now answer through bounded public documents,
        # and continuation from any reported cursor misses no commit.
        self.fixture = "large-history"
        count = 30
        title = ("Large history benchmark task with a long descriptive title, markers &, <tags>, "
                 "and quoting \"pressure\" ") * 2
        self.todo.write_text(json.dumps({"schema_version": 1, "source_files": [], "tasks": [
            {"id": "T%02d" % i, "title": "%s%d" % (title, i), "status": "todo", "priority": 1}
            for i in range(1, count + 1)]}))
        self.env["TEST_MODE"] = "fast"
        self.run_once()
        self.assertTrue(all(t["status"] == "done" for t in json.loads(self.todo.read_text())["tasks"]))

        document = self.compact()
        self.assertEqual(document["kind"], "looper.status.compact")
        self.assertEqual(document["work"]["counts"]["done"], count)
        cursor = document["cursor"]["event_id"]
        self.assertGreater(cursor, 0)
        self.assertNotIn("statistics", document)
        self.assertNotIn("timing", document)

        # The default tail is bounded and chronological; a smaller page never
        # exceeds its own limit.
        tail = self.events_page()
        self.assertLessEqual(len(tail["items"]), 50)
        self.assertEqual([i["id"] for i in tail["items"]], sorted(i["id"] for i in tail["items"]))
        self.assertEqual(tail["page"]["high_water"], cursor)
        page = self.events_page("--limit", "17")
        self.assertLessEqual(len(page["items"]), 17)
        self.assertEqual(page["page"]["limit"], 17)

        # Walk the whole journal through continuation cursors: every committed
        # event appears exactly once, in order, with no gap and no duplicate.
        collected = []
        after = 0
        while True:
            page = self.events_page("--after", str(after), "--limit", "17")
            ids = [i["id"] for i in page["items"]]
            self.assertEqual(ids, sorted(ids))
            if ids:
                self.assertEqual(ids[0], after + 1, "the continuation missed a committed event")
                after = ids[-1]
                collected.extend(ids)
            if not page["continuation"]["has_more"]:
                break
            self.assertLess(len(collected), 100000)
        self.assertEqual(collected, list(range(1, cursor + 1)))

        # A restart extends the journal; the cursor saved before it still
        # continues without missing the new commits.
        data = json.loads(self.todo.read_text())
        data["tasks"].append({"id": "T-LATE", "title": "Late arrival task", "status": "todo", "priority": 1})
        self.todo.write_text(json.dumps(data))
        self.run_once()
        resumed = self.events_page("--after", str(cursor))
        new_ids = [i["id"] for i in resumed["items"]]
        self.assertTrue(new_ids, "the continuation after restart returned nothing")
        self.assertEqual(new_ids, list(range(cursor + 1, cursor + 1 + len(new_ids))))
        document = self.compact()
        self.assertEqual(document["work"]["counts"]["done"], count + 1)
        self.assertGreater(document["cursor"]["event_id"], cursor)

        # One finite wait replaces the audit's repeated polling: on a finished
        # run it times out as an outcome, not an error.
        wait = self.cli("wait", "--contract", "v2", "--json", "--cursor",
                        str(document["cursor"]["event_id"]), "--timeout", "3s", expect=5)
        self.assertEqual(json.loads(wait.stdout)["outcome"], "timeout")

        # Plain piped status stays bounded and keeps liveness and the cursor.
        plain = self.cli("status", str(self.todo))
        self.assertLessEqual(len(plain.stdout.encode()), 8192)
        for line in ("Controller:", "Backlog:", "Cursor:", "Wait for the next change"):
            self.assertIn(line, plain.stdout)

    def test_submission_receipts_survive_restart_and_apply_once(self):
        # Audit patterns 6 and 12: submissions waited silently, queued work
        # died with the controller, and success events had no durable state.
        # The procedure now returns an immediate receipt through a long check,
        # replays idempotently, and survives a hard kill.
        self.fixture = "submission-restart"
        self.tasks(count=1)
        self.env["TEST_MODE"] = "worker_gate"
        self.env["TEST_WORKER_GATE"] = str(self.path / "worker-gate")
        gate = Path(self.env["TEST_WORKER_GATE"])
        gate.write_text("NEW-2\n")
        self.env["TEST_VERIFY"] = "sleep 6; exit 0"
        command = self.command()
        command.remove("--once")
        with (self.path / "lifecycle-output").open("w") as output:
            process = subprocess.Popen(command, cwd=self.project, env=self.env, stdout=output, stderr=output)
            try:
                self.wait_for(lambda: any(e["kind"] == "end" for e in self.recorded()))
                # The receipt returns immediately while the long check holds.
                # The input file lives outside the project so the guarded
                # project tree never sees the submission as an edit.
                added = self.path / "added.json"
                added.write_text(json.dumps({"id": "NEW-1", "title": "Benchmark added task",
                                             "status": "todo", "priority": 1}))
                receipt = json.loads(self.cli(
                    "add-task", str(added), "--contract", "v2", "--json",
                    "--idempotency-key", "bench-add-first", procedure="mutate").stdout)
                self.assertEqual(receipt["kind"], "looper.request.receipt")
                first = receipt["request"]["id"]
                self.assertLess(self.measurements[-1]["seconds"], 5.0,
                                "the mutation receipt was not immediate")
                waited = json.loads(self.cli("wait", "--contract", "v2", "--json",
                                             "--request", first, "--timeout", "40s").stdout)
                self.assertEqual(waited["outcome"], "satisfied")
                record = json.loads(self.cli("requests", first, "--contract", "v2", "--json").stdout)
                self.assertEqual(record["kind"], "looper.request.record")
                self.assertEqual(record["request"]["state"], "applied")
                # The same key and content returns the original request.
                replay = json.loads(self.cli(
                    "add-task", str(added), "--contract", "v2", "--json",
                    "--idempotency-key", "bench-add-first", procedure="mutate").stdout)
                self.assertEqual(replay["request"]["id"], first)
                self.assertTrue(replay["request"]["duplicate"])
                # Changed content under the same key is a conflict, not a
                # second task.
                added.write_text(json.dumps({"id": "NEW-1X", "title": "Benchmark added task",
                                             "status": "todo", "priority": 1}))
                conflict = self.cli("add-task", str(added), "--contract", "v2", "--json",
                                    "--idempotency-key", "bench-add-first", expect=1, procedure="mutate")
                self.assertEqual(json.loads(conflict.stdout)["error"]["code"], "idempotency_key_conflict")
                # A second submission is interrupted by a hard kill before its
                # outcome; its durable envelope must not vanish with it.
                added.write_text(json.dumps({"id": "NEW-2", "title": "Benchmark crash task",
                                             "status": "todo", "priority": 1}))
                second = json.loads(self.cli(
                    "add-task", str(added), "--contract", "v2", "--json", procedure="mutate").stdout)
                second_id = second["request"]["id"]
                process.kill()
                process.wait(timeout=load_budget(10))
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()
        document = self.compact()
        self.assertEqual(document["controller"]["state"], "stopped")
        self.assertFalse(document["controller"]["owned"])
        # Restart through the public command. The accepted request keeps its
        # applied receipt across the restart; the envelope the crash caught
        # before acceptance has an explicit rejected outcome, never a silent
        # deletion.
        gate.unlink()
        self.env["TEST_MODE"] = ""
        self.run_once()
        accepted = json.loads(self.cli("requests", first, "--contract", "v2", "--json").stdout)
        self.assertEqual(accepted["request"]["state"], "applied")
        interrupted = json.loads(self.cli("requests", second_id, "--contract", "v2", "--json").stdout)
        self.assertEqual(interrupted["request"]["state"], "rejected")
        self.assertIn("never applied", interrupted["request"]["error"]["message"])
        data = json.loads(self.todo.read_text())
        self.assertEqual([t["id"] for t in data["tasks"] if t["id"] == "NEW-1"], ["NEW-1"])
        # The documented recovery for a request that never reached acceptance
        # is a resubmission under a new key against a live controller.
        gate.write_text("none\n")
        self.env["TEST_MODE"] = "worker_gate"
        command = self.command()
        command.remove("--once")
        with (self.path / "retry-output").open("w") as output:
            process = subprocess.Popen(command, cwd=self.project, env=self.env, stdout=output, stderr=output)
            try:
                self.wait_for(lambda:
                              self.compact()["controller"]["state"] == "controlling")
                retried = json.loads(self.cli(
                    "add-task", str(added), "--contract", "v2", "--json",
                    "--idempotency-key", "bench-add-crash-retry", procedure="mutate").stdout)
                retry_id = retried["request"]["id"]
                self.assertNotEqual(retry_id, second_id)
                gate.unlink(missing_ok=True)
                waited = json.loads(self.cli("wait", "--contract", "v2", "--json",
                                             "--request", retry_id, "--timeout", "60s").stdout)
                self.assertEqual(waited["outcome"], "satisfied")
                self.wait_for(lambda: (self.project / "NEW-2.txt").exists(),
                              timeout=load_budget(90))
            finally:
                gate.unlink(missing_ok=True)
                if process.poll() is None:
                    stop = self.cli("stop", "--wait", procedure="mutate", timeout=120)
                    self.assertEqual(stop.returncode, 0)
                    process.wait(timeout=load_budget(30))
        record = json.loads(self.cli("requests", retry_id, "--contract", "v2", "--json").stdout)
        self.assertEqual(record["request"]["state"], "applied")
        data = json.loads(self.todo.read_text())
        for task_id in ("NEW-1", "NEW-2"):
            matching = [t for t in data["tasks"] if t["id"] == task_id]
            self.assertEqual(len(matching), 1, "%s applied more than once" % task_id)
            self.assertEqual(matching[0]["status"], "done")
        runs = json.loads(self.cli("runs", "--contract", "v2", "--json").stdout)
        self.assertGreaterEqual(runs["run_total"], 1)
        self.assertTrue(any(entry.get("current_project") for entry in runs["runs"]))

    def test_bootstrap_is_observable_on_a_foreground_start(self):
        # The AX-004 live-bootstrap fixture: before the first backlog exists,
        # a public status must name the bootstrap phase, not an error.
        self.fixture = "bootstrap-foreground"
        self.todo.unlink()
        self.env["TEST_BOOTSTRAP_GATE"] = str(self.path / "bootstrap-gate")
        gate = Path(self.env["TEST_BOOTSTRAP_GATE"])
        gate.write_text("hold")
        log = self.path / "bootstrap-foreground.log"
        with log.open("w") as output:
            process = subprocess.Popen(self.command(), cwd=self.project, env=self.env,
                                       stdout=output, stderr=output)
            try:
                self.wait_for(lambda: any(e["kind"] == "bootstrap" for e in self.recorded()))
                document = self.compact()
                self.assertEqual(document["controller"]["state"], "starting")
                self.assertEqual(document["waiting"]["reason"], "bootstrap")
                self.assertTrue(any(p["phase"] == "bootstrap" for p in document["work"]["phases"]))
                self.assertEqual(document["work"]["counts"]["done"], 0)
                gate.unlink()
                self.assertEqual(process.wait(timeout=load_budget(90)), 0, log.read_text()[-2000:])
            finally:
                gate.unlink(missing_ok=True)
                if process.poll() is None:
                    process.kill()
                    process.wait()
        data = json.loads(self.todo.read_text())
        self.assertTrue(all(t["status"] == "done" for t in data["tasks"]))

    def test_bootstrap_is_observable_on_a_detached_start(self):
        # The same bootstrap window on the daemon path: the startup seed a
        # detached start saves before readiness must not read as an empty,
        # finished backlog (the AX-004 live fixture gap).
        self.fixture = "bootstrap-detached"
        self.todo.unlink()
        self.env["TEST_BOOTSTRAP_GATE"] = str(self.path / "bootstrap-gate")
        gate = Path(self.env["TEST_BOOTSTRAP_GATE"])
        gate.write_text("hold")
        started = self.cli("start", procedure="mutate")
        self.assertIn("Controller detached", started.stdout + started.stderr)
        try:
            self.wait_for(lambda: any(e["kind"] == "bootstrap" for e in self.recorded()))
            document = self.compact()
            self.assertEqual(document["controller"]["state"], "starting")
            self.assertEqual(document["controller"]["owned"], True)
            self.assertEqual(document["waiting"]["reason"], "bootstrap")
            self.assertTrue(any(p["phase"] == "bootstrap" for p in document["work"]["phases"]))
            for finding in document["findings"]:
                self.assertNotEqual(finding["code"], "view_unavailable",
                                    "a bootstrapping controller has no view yet")
        finally:
            gate.unlink(missing_ok=True)
            stop = self.cli("stop", "--wait", procedure="mutate", timeout=120)
            self.assertEqual(stop.returncode, 0)
        data = json.loads(self.todo.read_text())
        self.assertEqual([t["id"] for t in data["tasks"]], ["T001", "T002"])

    def test_pre_contract_controller_refuses_new_request_types(self):
        # Audit pattern 6: a newer client against an older controller created
        # one confirmed dead-letter pair. A controller whose metadata predates
        # capability advertisement must refuse the unsupported request without
        # queueing anything, and name what it does accept.
        self.fixture = "old-capabilities"
        self.tasks(count=1)
        command = self.command()
        command.remove("--once")
        with (self.path / "capabilities-output").open("w") as output:
            process = subprocess.Popen(command, cwd=self.project, env=self.env, stdout=output, stderr=output)
            try:
                self.wait_for(lambda: any(e["kind"] == "end" for e in self.recorded()))
                runtime = next((self.path / "logs").glob("*/runtime"))
                metadata = json.loads((runtime / "controller.json").read_text())
                self.assertIn("capabilities", metadata)
                # Fixture setup: make the live controller look pre-contract.
                metadata.pop("capabilities")
                (runtime / "controller.json").write_text(json.dumps(metadata))
                refusal = self.cli("cancel", "T1", "--contract", "v2", "--json",
                                   expect=None, procedure="mutate")
                self.assertNotEqual(refusal.returncode, 0)
                error = json.loads(refusal.stdout)["error"]
                self.assertEqual(error["code"], "capability_unsupported")
                self.assertIn("No request was queued", error["message"])
                self.assertIn("predates capability advertisement", error["message"])
                self.assertTrue(any("version --contract v2" in a.get("command", "")
                                    for a in error.get("next_actions", [])))
                # No dead letter: the bounded request list shows nothing new.
                listing = json.loads(self.cli("requests", "--contract", "v2", "--json").stdout)
                self.assertFalse([r for r in listing.get("requests", [])
                                  if r["type"] == "cancel-task"])
                # Capability discovery explains the derived set.
                capabilities = json.loads(self.cli("version", "--contract", "v2", "--json").stdout)
                controller = capabilities["controller"]
                self.assertTrue(controller["absent_metadata"])
                self.assertNotIn("cancel-task", controller["request_types"])
                self.assertIn("add-task", controller["request_types"])
                # The legacy base set still serves the documented stop.
            finally:
                stop = self.cli("stop", "--wait", procedure="mutate", timeout=120)
                self.assertEqual(stop.returncode, 0)
                if process.poll() is None:
                    process.wait(timeout=load_budget(30))
        self.assertEqual(process.returncode, 0, (self.path / "capabilities-output").read_text()[-2000:])

    def test_import_submission_pauses_dispatch_through_public_commands(self):
        # T044 verification through the public CLI: the add-plan receipt
        # returns during active work, the waiting import is visible in the
        # bounded status, ordinary dispatch pauses until the import applies,
        # and every task then finishes with documented commands.
        self.fixture = "import-pause"
        self.tasks(count=3)
        self.env["TEST_MODE"] = "worker_gate"
        self.env["TEST_WORKER_GATE"] = str(self.path / "worker-gate")
        gate = Path(self.env["TEST_WORKER_GATE"])
        gate.write_text("T1\nT2\n")  # both workers hold their slots open
        self.env["TEST_IMPORT_GATE"] = str(self.path / "import-gate")
        import_gate = Path(self.env["TEST_IMPORT_GATE"])
        # The plan document lives outside the project so submitting it cannot
        # read as a guarded project edit.
        plan = self.path / "extra-requirements.md"
        plan.write_text("# Extra benchmark requirements\n\nCreate the P1 marker task.\n")
        command = self.command()
        command.remove("--once")
        with (self.path / "import-output").open("w") as output:
            process = subprocess.Popen(command, cwd=self.project, env=self.env, stdout=output, stderr=output)
            try:
                self.wait_for(lambda: any(e["kind"] == "start" and e["task"] == "T2"
                                          for e in self.recorded()))
                receipt = json.loads(self.cli("add-plan", str(plan), "--contract", "v2", "--json",
                                              procedure="mutate").stdout)
                self.assertEqual(receipt["kind"], "looper.request.receipt")
                request_id = receipt["request"]["id"]
                # The import is queued durably behind the active work and the
                # bounded status reports it without reading the queue.
                self.wait_for(lambda: (document := self.compact())["requests"].get("waiting", 0) >= 1
                              or document["requests"]["pending"] >= 1)
                # Let the active work reach its boundary: the import analysis
                # then holds the import gate with T3 ready and every worker
                # slot free, which is exactly the window the dispatch pause
                # must cover.
                gate.unlink(missing_ok=True)
                self.wait_for(lambda: any(e["kind"] == "import-analysis" for e in self.recorded()),
                              timeout=load_budget(60))
                deadline = time.monotonic() + 3
                while time.monotonic() < deadline:
                    self.assertFalse(any(e["kind"] == "start" and e["task"] == "T3"
                                         for e in self.recorded()),
                                     "ordinary dispatch was not paused for the accepted import")
                    time.sleep(.1)
                import_gate.write_text("release")
                # The import applies, the pause releases, and all remaining
                # work - including the imported task - finishes. The record
                # names the task identifier the import received.
                self.wait_for(lambda: json.loads(
                    self.cli("requests", request_id, "--contract", "v2", "--json").stdout
                )["request"]["state"] == "applied", timeout=load_budget(90))
                record = json.loads(self.cli("requests", request_id, "--contract", "v2", "--json").stdout)
                added_tasks = record["request"].get("affected_tasks", []) or \
                    record["request"].get("plan_coverage", {}).get("added_tasks", [])
                self.assertEqual(len(added_tasks), 1)
                imported = added_tasks[0]
                self.wait_for(lambda: (self.project / "T3.txt").exists() and
                              (self.project / (imported + ".txt")).exists(),
                              timeout=load_budget(90))
                # The public journal proves the pause: T3's first start event
                # follows the import's applied event.
                applied = self.events_page("--kind", "import")
                applied_ids = [i for i in applied["items"] if i.get("request_id") == request_id]
                self.assertTrue(applied_ids, "the applied import journaled no event")
                starts = self.events_page("--task", "T3")
                start_ids = [i for i in starts["items"] if i["kind"] == "start"]
                self.assertTrue(start_ids, "T3 never started")
                self.assertGreater(start_ids[0]["id"], applied_ids[-1]["id"])
            finally:
                gate.unlink(missing_ok=True)
                if process.poll() is None:
                    stop = self.cli("stop", "--wait", procedure="mutate", timeout=120)
                    self.assertEqual(stop.returncode, 0)
                    process.wait(timeout=load_budget(30))
        data = json.loads(self.todo.read_text())
        self.assertTrue(all(t["status"] == "done" for t in data["tasks"]))

    def test_idle_controller_and_held_input_are_observable(self):
        # Audit pattern 2 and the input-hold incidents: piped status hid
        # liveness and in-flight work, so operators appended ps to every poll.
        # The procedure below observes the idle wait, then the input
        # correction hold, with one bounded wait per transition instead of
        # repeated status polls.
        self.fixture = "idle-held-input"
        self.tasks(count=1)
        command = self.command()
        command.remove("--once")
        with (self.path / "held-input-output").open("w") as output:
            process = subprocess.Popen(command, cwd=self.project, env=self.env,
                                       stdout=output, stderr=output)
            try:
                self.wait_for(lambda: any(e["kind"] == "end" for e in self.recorded()))
                # An idle controller is alive and states why nothing runs.
                self.wait_for(lambda: self.compact()["waiting"]["reason"] == "no_ready_task")
                document = self.compact()
                self.assertEqual(document["controller"]["state"], "controlling")
                self.assertTrue(document["controller"]["owned"])
                self.assertEqual(document["work"]["counts"].get("in_flight", 0), 0)
                # A malformed task file holds the run at an input correction
                # boundary. The journal's waiting event wakes one bounded
                # wait; the file is preserved and the run stays alive.
                original = self.todo.read_text()
                self.todo.write_text("{broken")
                changed = self.cli("wait", "--contract", "v2", "--json", "--cursor",
                                   str(document["cursor"]["event_id"]), "--timeout", "30s")
                self.assertEqual(json.loads(changed.stdout)["outcome"], "satisfied")
                # The wait can wake on the digest-hold transition just before
                # the parse records the correction; the hold settles within
                # one poll cycle.
                self.assertIn(self.compact()["waiting"]["reason"],
                              ("input_correction", "backlog_hold"))
                self.wait_for(lambda: self.compact()["waiting"]["reason"] == "input_correction")
                held = self.compact()
                self.assertEqual(held["waiting"]["reason"], "input_correction")
                self.assertEqual(held["controller"]["state"], "controlling")
                self.assertEqual(self.todo.read_text(), "{broken")
                # Restoring the file releases the hold; the accepted-again
                # waiting event wakes the second bounded wait.
                self.todo.write_text(original)
                resumed = self.cli("wait", "--contract", "v2", "--json", "--cursor",
                                   str(held["cursor"]["event_id"]), "--timeout", "30s")
                self.assertEqual(json.loads(resumed.stdout)["outcome"], "satisfied")
                self.wait_for(lambda: self.compact()["waiting"]["reason"] == "no_ready_task")
            finally:
                if process.poll() is None:
                    stop = self.cli("stop", "--wait", procedure="mutate", timeout=120)
                    self.assertEqual(stop.returncode, 0)
                    process.wait(timeout=load_budget(30))
        self.assertEqual(process.returncode, 0,
                         (self.path / "held-input-output").read_text()[-2000:])

    def test_stop_wait_binds_to_its_session_across_a_new_start(self):
        # Audit pattern 11: four groups hand-rolled drain polling and a stale
        # stop file survived into the next run. The public procedure now waits
        # finitely for the drain of the session it targeted, and a replacement
        # controller is never drained by it.
        self.fixture = "stop-start-overlap"
        self.tasks(count=1)
        self.env["TEST_MODE"] = "worker_gate"
        self.env["TEST_WORKER_GATE"] = str(self.path / "worker-gate")
        gate = Path(self.env["TEST_WORKER_GATE"])
        gate.write_text("T1\n")
        command = self.command()
        command.remove("--once")
        with (self.path / "overlap-output").open("w") as output:
            process = subprocess.Popen(command, cwd=self.project, env=self.env, stdout=output, stderr=output)
            try:
                self.wait_for(lambda: any(e["kind"] == "start" for e in self.recorded()))
                stopper = subprocess.Popen(
                    [str(ROOT / "build/looper"), "stop", "--wait", "--wait-timeout", "60",
                     "--contract", "v2", "--json"], cwd=self.project, env=self.env,
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                # The stop is observable as a drain while the worker holds.
                self.wait_for(lambda: self.compact()["controller"]["state"] == "draining")
                gate.unlink()
                stdout, stderr = stopper.communicate(timeout=load_budget(90))
                self.assertEqual(stopper.returncode, 0, stdout + stderr)
                document = json.loads(stdout)
                self.assertEqual(document["kind"], "looper.request.record")
                self.assertEqual(document["request"]["state"], "applied")
            finally:
                gate.unlink(missing_ok=True)
                if process.poll() is None:
                    process.kill()
                    process.wait()
        # A replacement controller starts cleanly: the drained session left no
        # request behind, so the new run works instead of stopping.
        data = json.loads(self.todo.read_text())
        data["tasks"].append({"id": "T2", "title": "Replacement work", "status": "todo", "priority": 1})
        self.todo.write_text(json.dumps(data))
        self.env["TEST_MODE"] = ""
        self.run_once()
        self.assertEqual((self.project / "T2.txt").read_text(), "done")
        shutdowns = self.events_page("--kind", "shutdown")
        self.assertEqual(shutdowns["items"][-1]["data"]["reason"], "complete",
                         "the replacement controller inherited the earlier stop request")


if __name__ == "__main__":
    unittest.main()
