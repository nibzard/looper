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
elif "review pass for Looper" in prompt:
    event("review")
    assert not (cwd/"forbidden.txt").exists()
    data=json.loads(todo.read_text())
    if mode=="review_fail":
        sys.exit(7)
    if mode=="review_code":
        (cwd/"seed.txt").write_text("review changed code")
    data["tasks"].append({"id":"PROJECT-DONE","title":"Project done","priority":5,"status":"done","tags":["project-done"]})
    todo.write_text(json.dumps(data))
    result={"status":"reviewed","summary":"checked","added_tasks":0,"files":[]}
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
    if mode=="resume_partial":
        assert (cwd/(task_id+".txt")).exists(), "partial work was lost"
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
            "status":"blocked" if mode=="blocked" or (mode=="mixed" and task_id=="T1") else "done", "summary":"checked "+task_id,
            "files":[path.name],"blockers":["missing input"] if mode=="blocked" or (mode=="mixed" and task_id=="T1") else []}
    if mode=="followup" and task_id=="T1":
        result["followups"]=[{"title":"Fix observed behavior","details":"Check added output","evidence":"Observed fixture failure","depends_on":["T1"],"priority":1}]
    if mode=="malformed":
        pathlib.Path(args[args.index("--output-last-message")+1]).write_text("not json")
        sys.exit(0)
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
        return [str(ROOT / "build/looper"), "--once", *args, str(self.todo)]

    def run_loop(self, expected=0, *args):
        result = subprocess.run(self.command(*args), cwd=self.project, env=self.env,
                                capture_output=True, text=True, timeout=40)
        self.assertEqual(result.returncode, expected, result.stdout + result.stderr)
        return result.stdout + result.stderr

    def recorded(self):
        return [json.loads(x) for x in self.events.read_text().splitlines()]

    def state(self):
        with sqlite3.connect(next((self.path / "logs").glob("*/runtime/state.db"))) as db:
            return json.loads(db.execute("select data from state where id=1").fetchone()[0])

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
                              capture_output=True,text=True,timeout=5)
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
                process.wait(timeout=10)
            finally:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
        self.env["TEST_MODE"]=""
        self.run_loop()
        self.assertEqual((self.project/"T1.txt").read_text(), "done")

    def test_repeated_verification_failure_keeps_code_isolated(self):
        self.tasks(count=1)
        self.env["TEST_VERIFY"]="exit 1"
        self.run_loop(2)
        self.assertFalse((self.project/"T1.txt").exists())
        self.assertEqual(self.state()["attempts"][0]["repairs"],2)
        # The repeated failure triggers one independent diagnosis.
        self.assertEqual([e["kind"] for e in self.recorded() if e["kind"]=="diagnose"],["diagnose"])

    def test_bad_plan_fails_without_starting_workers(self):
        self.env["TEST_MODE"]="bad_plan"
        self.run_loop(1)
        self.assertFalse(any(e["kind"]=="start" for e in self.recorded()))

    def test_review_failure_and_unapproved_code_are_not_published(self):
        self.tasks(count=1)
        self.env["TEST_MODE"]="review_code"
        self.run_loop(1)
        self.assertEqual((self.project/"seed.txt").read_text(),"original\n")
        self.assertFalse(any("project-done" in t.get("tags",[]) for t in json.loads(self.todo.read_text())["tasks"]))

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


    def test_external_edit_stops_publication_and_can_resume(self):
        self.tasks(count=1)
        with (self.path/"external-output").open("w") as output:
            process=subprocess.Popen(self.command(),cwd=self.project,env=self.env,stdout=output,stderr=output)
            try:
                self.wait_for(lambda:self.events.exists() and any(e["kind"]=="start" for e in self.recorded()))
                (self.project/"seed.txt").write_text("user edit\n")
                self.assertEqual(process.wait(timeout=15),1)
                self.assertEqual((self.project/"seed.txt").read_text(),"user edit\n")
                self.assertFalse((self.project/"T1.txt").exists())
                (self.project/"seed.txt").write_text("original\n")
                self.run_loop()
                self.assertEqual(len([e for e in self.recorded() if e["kind"]=="start"]),1)
            finally:
                if process.poll() is None:process.kill();process.wait()

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
        subprocess.run([str(package/"looper-install"),"--prefix",str(prefix),"--skip-skills"],check=True,capture_output=True,text=True)
        result=subprocess.run([str(prefix/"bin/looper"),"--once"],cwd=self.project,env=self.env,capture_output=True,text=True,timeout=40)
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

    def wait_for(self, predicate, timeout=15):
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
                self.assertEqual(process.wait(timeout=15),0)
            finally:
                if process.poll() is None: process.kill();process.wait()

    def test_advisory_failure_triggers_review_without_claiming_failure(self):
        self.tasks(count=1)
        self.env["LOOPER_ASSESS_COMMAND"]="echo '{\"outcome\":\"fail\"}' > \"$LOOPER_ASSESS_REPORT\""
        self.run_loop()
        self.assertEqual((self.project/"T1.txt").read_text(),"done")
        with sqlite3.connect(next((self.path/"logs").glob("*/runtime/state.db"))) as db:
            self.assertEqual(db.execute("select result from checks where kind='semantic'").fetchone()[0],"fail")

if __name__ == "__main__":
    unittest.main()
