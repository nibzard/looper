package looper

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// Both spellings of the verbose flag must parse, and it must stay off by
// default.
func TestParseVerboseFlag(t *testing.T) {
	clearSettingEnv(t)
	for _, flag := range []string{"--verbose", "-v"} {
		c, err := parseAt(t, flag)
		if err != nil {
			t.Fatal(err)
		}
		if !c.Verbose {
			t.Fatalf("%s must set verbose mode", flag)
		}
		if c.Command != "run" {
			t.Fatalf("%s must not change the command: %s", flag, c.Command)
		}
	}
	c, err := parseAt(t)
	if err != nil {
		t.Fatal(err)
	}
	if c.Verbose {
		t.Fatal("verbose must default to off")
	}
	if !strings.Contains(help, "--verbose") {
		t.Fatal("the flag must appear in help")
	}
}

// A verbose run prints the run configuration exactly once, as a config event.
func TestVerboseEmitsExactlyOneConfigEvent(t *testing.T) {
	for _, verbose := range []bool{true, false} {
		c := preparedRepo(t, "--once")
		c.Verbose = verbose
		e, lock, err := NewEngine(c, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		code, err := e.Run(ctx)
		events := storedEvents(t, e)
		e.Store.Close()
		lock.Close()
		if code != 0 || err != nil {
			t.Fatalf("verbose=%t: finished project must complete cleanly, got %d (%v)", verbose, code, err)
		}
		var configEvents []Event
		for _, ev := range events {
			if ev.Kind == "config" {
				configEvents = append(configEvents, ev)
			}
		}
		if verbose {
			if len(configEvents) != 1 {
				t.Fatalf("verbose must emit exactly one config event, got %d", len(configEvents))
			}
			for _, want := range []string{"agents", "jobs", "task file"} {
				if !strings.Contains(configEvents[0].Message, want) {
					t.Fatalf("config event must report %q: %q", want, configEvents[0].Message)
				}
			}
		} else if len(configEvents) != 0 {
			t.Fatalf("a plain run must not emit config events, got %d", len(configEvents))
		}
	}
}

// runningEngine builds a controller over two ready tasks whose workers finish
// at once, so a run can be driven to a chosen boundary.
func runningEngine(t *testing.T, result string) (*Engine, Config) {
	t.Helper()
	bin, _ := fakeAgent(t, result)
	c := repoConfig(t, "run")
	c.CodexBin, c.ClaudeBin = bin, bin
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{
		{ID: "T001", Title: "first", Priority: 1, Status: "todo"},
		{ID: "T002", Title: "second", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	e.State.Plan = &Plan{Verify: "true", Tasks: []Scope{
		{TaskID: "T001", Paths: []string{"a"}},
		{TaskID: "T002", Paths: []string{"b"}},
	}}
	contracts := []string{}
	for _, task := range e.State.Backlog.Tasks {
		contracts = append(contracts, task.Contract())
	}
	e.State.PlanSignature = hashJSON(struct {
		Contracts     []string
		Setup, Verify string
	}{contracts, e.Config.Setup, e.Config.Verify})
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e, c
}

// Reaching the dispatch limit exits with code 2 and records the
// dispatch_limit reason in the session's own shutdown event.
func TestDispatchLimitExitReason(t *testing.T) {
	clearSettingEnv(t)
	result := `{"task_id":"T001","status":"done","summary":"finished","files":["a.txt"],"blockers":[],"followups":[],"unblock_when":""}`
	e, _ := runningEngine(t, result)
	e.Config.Limit = 1
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code, err := e.Run(ctx)
	if code != 2 || err != nil {
		t.Fatalf("a reached dispatch limit must exit 2 without an error, got %d (%v)", code, err)
	}
	events := storedEvents(t, e)
	var limited bool
	for _, ev := range events {
		if ev.Kind == "limit" && strings.Contains(ev.Message, "Dispatch limit reached") {
			limited = true
		}
	}
	if !limited {
		t.Fatal("the dispatch limit must be reported as an event")
	}
	var exit sessionExit
	found := false
	for _, ev := range events {
		if ev.Kind != "shutdown" {
			continue
		}
		var candidate sessionExit
		if json.Unmarshal(ev.Data, &candidate) == nil && candidate.Session == e.Store.session {
			exit, found = candidate, true
		}
	}
	if !found {
		t.Fatal("the session shutdown record is missing")
	}
	if exit.Reason != "dispatch_limit" || exit.ExitCode != 2 {
		t.Fatalf("shutdown record = %+v; want reason dispatch_limit at exit 2", exit)
	}
	// The second task stays runnable: the limit retains work, it does not fail it.
	if task := e.State.Backlog.Task("T002"); task.Status != "todo" {
		t.Fatalf("undispatched work must stay todo, got %s", task.Status)
	}
}
