#!/usr/bin/env python3
"""Exercise parallel execution with real Git repositories and local agent stubs."""

import importlib.util
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
spec = importlib.util.spec_from_file_location("parallel", ROOT / "lib/looper_parallel.py")
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)

STUB = r'''#!/usr/bin/env python3
import json, os, pathlib, sys, time
args=sys.argv[1:]
claude="-p" in args
prompt=args[args.index("-p")+1] if claude else sys.stdin.read()
cwd=pathlib.Path.cwd()
todo=cwd/os.environ.get("TEST_TODO", "to-do.json")
tasks=json.loads(todo.read_text())["tasks"]
events=pathlib.Path(os.environ["TEST_EVENTS"])
def event(kind, task=""):
    with events.open("a") as f:
        f.write(json.dumps({"kind":kind,"task":task,"time":time.time(),"cwd":str(cwd),"pid":os.getpid()})+"\n")
mode=os.environ.get("TEST_MODE", "")
if "Plan execution for Looper" in prompt:
    event("plan")
    result={"setup":"", "verify":os.environ.get("TEST_VERIFY", "python3 check.py"), "tasks":[
        {"task_id":t["id"],"write_paths":["." if mode=="exclusive" else t["id"]+".txt"],"resources":[]}
        for t in tasks if t["status"]!="done"]}
    if mode=="bad_plan":
        result={"tasks":[],"setup":"","verify":""}
elif "final review pass" in prompt:
    event("review")
    data=json.loads(todo.read_text())
    if mode=="review_fail":
        sys.exit(7)
    if mode=="review_code":
        (cwd/"seed.txt").write_text("review changed code")
    data["tasks"].append({"id":"PROJECT-DONE","title":"Project done","priority":5,"status":"done","tags":["project-done"]})
    todo.write_text(json.dumps(data))
    result={"status":"reviewed","summary":"checked","added_tasks":0,"files":[]}
else:
    task=next(t for t in tasks if "Complete task "+t["id"]+":" in prompt)
    task_id=task["id"]
    event("start",task_id)
    if mode=="interrupt":
        (cwd/(task_id+".txt")).write_text("partial")
        time.sleep(30)
    else:
        time.sleep(2 if mode=="pipeline" and task_id=="T2" else (0.2 if task_id=="T1" else 0.5))
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
            "status":"blocked" if mode=="blocked" else "done", "summary":"checked "+task_id,
            "files":[path.name],"blockers":["missing input"] if mode=="blocked" else []}
if claude:
    print(json.dumps({"structured_output":result,"result":json.dumps(result)}))
else:
    pathlib.Path(args[args.index("--output-last-message")+1]).write_text(json.dumps(result))
    print(json.dumps({"type":"result"}))
'''


class ParallelTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="looper-parallel-")
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.project = self.path / "project with spaces"
        self.project.mkdir()
        self.git("init", "-q")
        self.git("config", "user.name", "Test")
        self.git("config", "user.email", "test@example.invalid")
        (self.project / "seed.txt").write_text("original\n")
        (self.project / "check.py").write_text(
            'from pathlib import Path\n'
            'a, b = Path("T1.txt"), Path("T2.txt")\n'
            'assert not (a.exists() and b.exists()) or b.read_text() == "done"\n')
        self.git("add", ".")
        self.git("commit", "-qm", "Initial fixture")
        self.head = self.git("rev-parse", "HEAD")
        self.stub = self.path / "agent"
        self.stub.write_text(STUB)
        self.stub.chmod(0o755)
        self.events = self.path / "events.jsonl"
        self.env = dict(os.environ, CODEX_BIN=str(self.stub), CLAUDE_BIN=str(self.stub),
                        CODEX_JSON_LOG="1", CODEX_PROGRESS="0", LOOPER_BASE_DIR=str(self.path / "logs"),
                        LOOPER_JOBS="2", LOOPER_VERIFY_COMMAND="", LOOPER_HOOK="", LOOPER_GIT_INIT="0",
                        LOOP_DELAY_SECONDS="0", MAX_ITERATIONS="10", TEST_EVENTS=str(self.events),
                        LOOPER_ITER_SCHEDULE="codex", LOOPER_REVIEW_AGENT="codex", LOOPER_REPAIR_AGENT="codex")
        self.env.pop("LOOPER_LOCK_HELD", None)
        self.todo = self.project / "to-do.json"
        self.tasks()

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.project), *args], stderr=subprocess.PIPE).decode().strip()

    def tasks(self, dependencies=None, count=2):
        self.todo.write_text(json.dumps({"schema_version":1,"source_files":[],"tasks":[
            {"id":"T"+str(i),"title":"Task "+str(i),"status":"todo","priority":1,
             "depends_on": (dependencies or {}).get("T"+str(i), [])} for i in range(1,count+1)]}))

    def command(self, *args):
        return [str(ROOT / "bin/looper.sh"), *args, str(self.todo)]

    def run_loop(self, expected=0, *args):
        result = subprocess.run(self.command(*args), cwd=self.project, env=self.env,
                                capture_output=True, text=True, timeout=40)
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
        return result.stdout + result.stderr

    def recorded(self):
        return [json.loads(x) for x in self.events.read_text().splitlines()]

    def state(self):
        return runtime.read_json(next((self.path / "logs").glob("*/parallel/state.json")))

    def test_parallel_and_exact_publication(self):
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

    def test_conflict_returns_to_worker(self):
        self.env["TEST_MODE"]="conflict"
        self.run_loop()
        self.assertEqual((self.project/"shared.txt").read_text(), "T1\nT2\n")
        self.assertTrue(any(a["repairs"] for a in self.state()["attempts"]))

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
        # Exercise two verification slots even on a small CI machine. Only the
        # reported processor count changes; Git, agents, and checks are real.
        shutil.copyfile(ROOT/"to-do.schema.json",self.project/"to-do.schema.json")
        script=("import os, runpy, sys; os.sched_getaffinity=lambda _:set(range(8)); "
                f"sys.argv=['runtime','run',{str(ROOT/'bin/looper.sh')!r}]; "
                f"runpy.run_path({str(ROOT/'lib/looper_parallel.py')!r},run_name='__main__')")
        result=subprocess.run([sys.executable,"-c",script],cwd=self.project,env=self.env,
                              capture_output=True,text=True,timeout=40)
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
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
        self.run_loop(1)
        self.assertFalse((self.project/"T1.txt").exists())
        self.assertNotEqual(runtime.read_json(self.todo)["tasks"][0]["status"], "done")
        self.env["TEST_MODE"]=""
        self.run_loop()

    def test_blocked_result_does_not_publish_partial_work(self):
        self.tasks({"T2":["T1"]})
        self.env["TEST_MODE"]="blocked"
        self.run_loop(2)
        self.assertFalse((self.project/"T1.txt").exists())
        self.assertEqual(runtime.read_json(self.todo)["tasks"][0]["status"], "blocked")

    def test_limit_drains_workers_and_resumes(self):
        self.tasks(count=3)
        self.env["MAX_ITERATIONS"]="2"
        self.run_loop(2)
        self.assertEqual(sum(t["status"]=="done" for t in runtime.read_json(self.todo)["tasks"]), 2)
        self.run_loop()

    def test_claude_bridge(self):
        self.run_loop(0, "--all", "claude")

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
        result=subprocess.run([str(ROOT/"bin/looper.sh"),"--tail"],cwd=self.project,env=self.env,
                              capture_output=True,text=True,timeout=5)
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        self.assertIn("Backlog",result.stdout)
        self.assertNotIn("Iter ?",result.stdout)

    def test_installed_runner_includes_the_coordinator(self):
        prefix=self.path/"installed"
        subprocess.run([str(ROOT/"install.sh"),"--prefix",str(prefix),"--skip-skills"],
                       check=True,capture_output=True,text=True)
        result=subprocess.run([str(prefix/"bin/looper.sh")],cwd=self.project,env=self.env,
                              capture_output=True,text=True,timeout=40)
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        self.assertTrue(self.state()["finished"])

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
                os.killpg(process.pid, signal.SIGTERM)
                process.wait(timeout=10)
            finally:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
        self.env["TEST_MODE"]=""
        self.run_loop()
        self.assertEqual((self.project/"T1.txt").read_text(), "done")

    def test_recovery_after_apply_before_status(self):
        self.tasks(count=1)
        self.run_loop()
        state=self.state()
        attempt=state["attempts"][0]
        state["finished"]=False
        attempt["phase"]="integrated"
        state["plan_signature"]=None
        path=next((self.path/"logs").glob("*/parallel/state.json"))
        runtime.atomic_json(path,state)
        self.tasks(count=1)
        self.run_loop()
        self.assertEqual(len([e for e in self.recorded() if e["kind"]=="start"]),1)

    def test_publication_journal_replays_after_apply(self):
        self.tasks(count=1)
        self.run_loop()
        state=self.state()
        attempt=state["attempts"][0]
        verified=attempt["verification"]
        state["finished"]=False
        state["publication"]=dict(verified, before=self.git("rev-parse", attempt["base"]+"^{tree}"), attempt_id=attempt["id"])
        state["tip"]=verified["base"]
        state["published"]=state["publication"]["before"]
        attempt["phase"]="verifying"
        runtime.atomic_json(next((self.path/"logs").glob("*/parallel/state.json")),state)
        self.tasks(count=1)
        self.run_loop()
        self.assertEqual(len([e for e in self.recorded() if e["kind"]=="start"]),1)

    def test_repeated_verification_failure_keeps_code_isolated(self):
        self.tasks(count=1)
        self.env["TEST_VERIFY"]="exit 1"
        self.run_loop(1)
        self.assertFalse((self.project/"T1.txt").exists())
        self.assertEqual(self.state()["attempts"][0]["repairs"],2)

    def test_bad_plan_fails_without_starting_workers(self):
        self.env["TEST_MODE"]="bad_plan"
        self.run_loop(1)
        self.assertFalse(any(e["kind"]=="start" for e in self.recorded()))

    def test_review_failure_and_unapproved_code_are_not_published(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="review_code"
        self.run_loop(1)
        self.assertEqual((self.project/"seed.txt").read_text(),"original\n")
        self.assertFalse(any("project-done" in t.get("tags",[]) for t in runtime.read_json(self.todo)["tasks"]))

    def test_external_edits_are_preserved(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="invalid"
        self.run_loop(1)
        (self.project/"seed.txt").write_text("user edit\n")
        self.assertIn("changed since",self.run_loop(1))
        self.assertEqual((self.project/"seed.txt").read_text(),"user edit\n")

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
                process.wait(timeout=5)
                self.env["TEST_MODE"]=""
                self.run_loop()
                # Reaped children can briefly remain zombies under a container init.
                status=subprocess.run(["ps","-o","stat=","-p",str(pid)],capture_output=True,text=True).stdout.strip()
                self.assertTrue(not status or status.startswith("Z"),status)
            finally:
                if process.poll() is None:
                    os.killpg(process.pid,signal.SIGKILL)
                    process.wait()


class SchedulerTests(unittest.TestCase):
    def test_plan_scopes_are_normalized_and_unknown_tasks_are_rejected(self):
        tasks=[{"id":"T1"}]
        plan=runtime.normalize_plan({"setup":"","verify":"make test","tasks":[
            {"task_id":"T1","write_paths":["./src//app.rs"],"resources":[]}]},tasks)
        self.assertEqual(plan["tasks"]["T1"]["write_paths"],["src/app.rs"])
        with self.assertRaises(runtime.RunError):
            runtime.normalize_plan({"setup":"","verify":"make test","tasks":[{"task_id":[]}]},tasks)

    def test_critical_path_and_priority(self):
        tasks=[{"id":x,"title":x,"priority":1,"status":"todo","depends_on":deps}
               for x,deps in [("T1",[]),("T2",[]),("T3",["T2"]),("T4",["T3"])]]
        self.assertEqual(runtime.ranked_ready(tasks)[0]["id"],"T2")
        tasks[0]["priority"]=1
        tasks[1]["priority"]=2
        self.assertEqual(runtime.ranked_ready(tasks)[0]["id"],"T1")

    def test_cycles_rejected(self):
        with self.assertRaises(runtime.RunError):
            runtime.validate_backlog({"schema_version":1,"source_files":[],"tasks":[
                {"id":"T1","title":"one","priority":1,"status":"todo","depends_on":["T2"]},
                {"id":"T2","title":"two","priority":1,"status":"todo","depends_on":["T1"]}]})


if __name__ == "__main__":
    unittest.main()
