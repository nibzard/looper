package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestObservationCommandsAndReplay(t *testing.T) {
	c := Config{Root: t.TempDir(), BaseDir: t.TempDir(), Command: "status", JSON: true}
	_, dir := projectPaths(c)
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &State{Version: 1, Backlog: Backlog{Tasks: []Task{{ID: "T1", Title: "A task", Status: "blocked", Blockers: []string{"decision needed"}}}},
		Attempts: []*Attempt{{ID: "a1", Task: Task{ID: "T1"}, Phase: "blocked"}}}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	if err = store.Check("a1", "revision", "verify", "failed", "verify.log"); err != nil {
		t.Fatal(err)
	}
	first, err := store.Event("start", "started", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Event("worker_activity", "T1 Tool started", map[string]string{"task_id": "T1", "attempt_id": "a1"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = inspect(context.Background(), c, &out); err != nil {
		t.Fatal(err)
	}
	var status struct {
		Backlog     Backlog
		Observation Observation
	}
	if err = json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Backlog.Tasks) != 1 || status.Observation.Version != 1 || status.Observation.ControllerOwned {
		t.Fatalf("bad status: %s", out.String())
	}
	c.Command = "inspect"
	c.InspectTask = "T1"
	out.Reset()
	if err = inspect(context.Background(), c, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"artifact":"verify.log"`) || !strings.Contains(out.String(), "decision needed") {
		t.Fatal(out.String())
	}
	c.InspectTask = "missing"
	if err = inspect(context.Background(), c, &out); err == nil {
		t.Fatal("unknown task accepted")
	}
	c.Command = "events"
	c.After = first
	out.Reset()
	if err = inspect(context.Background(), c, &out); err != nil {
		t.Fatal(err)
	}
	var event Event
	if err = json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.ID <= first || !strings.Contains(string(event.Data), `"task_id":"T1"`) {
		t.Fatal(out.String())
	}
	out.Reset()
	if code := doctorJSON(c, &out); code != 0 {
		t.Fatalf("doctor code %d: %s", code, out.String())
	}
	if !json.Valid(out.Bytes()) || !strings.Contains(out.String(), "task_blocked") {
		t.Fatal(out.String())
	}
}

func TestWorkerActivityJournalAndPrivacy(t *testing.T) {
	e := snapshotEngine(t)
	a := e.State.Attempts[0]
	a.Directory = t.TempDir()
	log := filepath.Join(a.Directory, "work-00", "events-00.jsonl")
	data := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"SECRET=value do-work"}}]}}` + "\n"
	if err := atomicWrite(log, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	e.pollWorkerActivity()
	events, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "worker_activity" {
		t.Fatalf("%+v", events)
	}
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), "SECRET") || !strings.Contains(string(raw), `"task_id":"T2"`) {
		t.Fatal(string(raw))
	}
	e.lastObservation = time.Time{}
	e.pollWorkerActivity()
	events, _ = e.Store.Events(0)
	if len(events) != 1 {
		t.Fatal("replayed duplicate activity")
	}
	w := observeWorker(a)
	if w.Log != log || w.LastOutput == "" || w.Progress != "unknown" {
		t.Fatalf("%+v", w)
	}
}

func TestQuietOutputDoesNotMeanFailed(t *testing.T) {
	age := int64(600)
	findings := diagnose(Observation{ControllerOwned: true, ViewMatchesController: true, Workers: []WorkerObservation{{TaskID: "T1", OutputAge: &age}}}, nil)
	if len(findings) != 1 || findings[0].Code != "output_quiet" || findings[0].Severity != "info" {
		t.Fatalf("%+v", findings)
	}
}

func TestObserveArgumentParsing(t *testing.T) {
	for _, args := range [][]string{{"events", "--json", "--follow", "--after", "42"}, {"inspect", "T054", "--json"}, {"doctor", "--json"}} {
		c, err := Parse(args)
		if err != nil {
			t.Fatal(err)
		}
		if c.Command != args[0] {
			t.Fatalf("%+v", c)
		}
		if c.Command == "inspect" && c.InspectTask != "T054" {
			t.Fatalf("%+v", c)
		}
		if c.Command == "events" && c.After != 42 {
			t.Fatalf("%+v", c)
		}
	}
	for _, args := range [][]string{{"events", "--after", "-1"}, {"inspect", "T1", "T2"}} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestWorkerProcessIdentity(t *testing.T) {
	a := &Attempt{Directory: t.TempDir()}
	log := filepath.Join(a.Directory, "work-00", "events-00.jsonl")
	if err := atomicWrite(log, []byte("output"), 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := runIdentityCommand(os.Getpid())
	if err != nil {
		t.Skip("process identity unavailable")
	}
	record := ProcessObservation{PID: os.Getpid(), Identity: identity}
	if err = writeJSON(log+".process.json", record); err != nil {
		t.Fatal(err)
	}
	if !observeWorker(a).Process.Running {
		t.Fatal("live process not recognized")
	}
	record.Identity = "different"
	if err = writeJSON(log+".process.json", record); err != nil {
		t.Fatal(err)
	}
	if observeWorker(a).Process.Running {
		t.Fatal("reused PID accepted")
	}
}

func TestWorkerStreamKeepsStructuredResult(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agent.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > args.txt\nprintf '%s\\n' " +
		`'{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read"}]}}'` + "\n" +
		"printf '%s\\n' " + `'{"type":"result","structured_output":{"task_id":"T1","status":"done","summary":"checked","files":[],"blockers":[],"followups":[],"unblock_when":""}}'` + "\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	work, dir := t.TempDir(), t.TempDir()
	a := Agent{Config: Config{ClaudeBin: bin, RetryLimit: 1}}
	result, err := a.Run(context.Background(), "claude", work, dir, "work", true)
	if err != nil || !strings.Contains(string(result), `"status":"done"`) {
		t.Fatalf("%s: %v", result, err)
	}
	args, err := os.ReadFile(filepath.Join(work, "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--output-format\nstream-json\n--verbose") {
		t.Fatal(string(args))
	}
	var process ProcessObservation
	if err = readJSON(filepath.Join(dir, "events-00.jsonl.process.json"), &process); err != nil {
		t.Fatal(err)
	}
	if process.Ended == "" || process.Running {
		t.Fatalf("missing process completion: %+v", process)
	}
}
