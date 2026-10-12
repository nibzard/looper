package looper

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func lastShutdown(t *testing.T, s *Store) sessionExit {
	t.Helper()
	var raw string
	if err := s.db.QueryRow("SELECT data FROM events WHERE kind='shutdown' ORDER BY id DESC LIMIT 1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var exit sessionExit
	if err := json.Unmarshal([]byte(raw), &exit); err != nil {
		t.Fatal(err)
	}
	return exit
}

func TestControllerRecordsCompletedAndFailedExits(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			e, c, _ := assessRunEngine(t, "", 0)
			if fail {
				if err := os.WriteFile(c.Todo, []byte(malformedBacklog), 0600); err != nil {
					t.Fatal(err)
				}
			}
			code, err := e.Run(context.Background())
			exit := lastShutdown(t, e.Store)
			reason, outcome := "complete", "stopped"
			if fail {
				reason, outcome = "error", "failed"
				if code != 1 || err == nil {
					t.Fatalf("bad input exit: %d %v", code, err)
				}
			} else if code != 0 || err != nil {
				t.Fatalf("completed exit: %d %v", code, err)
			}
			if exit.Session != e.controller.Session || exit.Reason != reason || exit.ExitCode != code || (fail && exit.Error == "") {
				t.Fatalf("exit record: %+v", exit)
			}
			var got string
			var ended sql.NullString
			if err := e.Store.db.QueryRow("SELECT outcome,ended FROM sessions WHERE id=?", exit.Session).Scan(&got, &ended); err != nil || got != outcome || !ended.Valid {
				t.Fatalf("session exit: %s %v %v", got, ended, err)
			}
		})
	}
}

type panicOutput struct{}

func (panicOutput) Write([]byte) (int, error) { panic("test controller panic") }

func TestControllerRecordsPanicAndRethrows(t *testing.T) {
	e, _, _ := assessRunEngine(t, "", 0)
	e.Output, e.Render = panicOutput{}, nil
	var crash any
	func() { defer func() { crash = recover() }(); _, _ = e.Run(context.Background()) }()
	if crash != "test controller panic" {
		t.Fatalf("panic was lost: %v", crash)
	}
	exit := lastShutdown(t, e.Store)
	if exit.Reason != "panic" || exit.ExitCode != 2 || !strings.Contains(exit.Error, "test controller panic") {
		t.Fatalf("panic record: %+v", exit)
	}
}

func TestExitRecordAndSessionEndAreAtomic(t *testing.T) {
	s := timingStore(t)
	info := &ControllerInfo{Session: "exit-test", PID: os.Getpid(), Started: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := s.beginSession(info); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("CREATE TRIGGER reject_shutdown BEFORE INSERT ON events WHEN NEW.kind='shutdown' BEGIN SELECT RAISE(ABORT,'test write failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.endSession(sessionExit{Reason: "error", ExitCode: 1}); err == nil {
		t.Fatal("exit event write succeeded unexpectedly")
	}
	path := filepath.Join(s.dir, "state.db")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	read, err := OpenStoreReadonly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	var end sql.NullString
	if err := read.db.QueryRow("SELECT ended FROM sessions WHERE id=?", info.Session).Scan(&end); err != nil || end.Valid {
		t.Fatalf("failed recording invented a clean end: %v %v", end, err)
	}
}

func TestRestartIdentifiesPreviousExit(t *testing.T) {
	for _, clean := range []bool{false, true} {
		t.Run(fmt.Sprint(clean), func(t *testing.T) {
			s := timingStore(t)
			info := &ControllerInfo{Session: "old", PID: os.Getpid(), Started: timingAt(0)}
			if err := s.beginSession(info); err != nil {
				t.Fatal(err)
			}
			id, err := s.Event("start", "last activity", nil)
			if err != nil {
				t.Fatal(err)
			}
			if clean {
				if err := s.endSession(sessionExit{Reason: "cancelled", ExitCode: 130}); err != nil {
					t.Fatal(err)
				}
			}
			info.Session = "new"
			if err := s.beginSession(info); err != nil {
				t.Fatal(err)
			}
			e := &Engine{Store: s}
			message := e.resumeMessage()
			if !strings.Contains(message, "old") {
				t.Fatal(message)
			}
			if clean {
				if !strings.Contains(message, "cancelled") || !strings.Contains(message, "Exit record: event") {
					t.Fatal(message)
				}
			} else if !strings.Contains(message, "uncleanly") || s.previous.LastEventID != id {
				t.Fatal(message)
			}
		})
	}
}

func TestRejectedFindingsIdentifyTheirSubject(t *testing.T) {
	e := snapshotEngine(t)
	for _, finding := range []Finding{
		{Title: "Fix output", Details: "criteria", Evidence: "failure", DependsOn: []string{"missing"}},
		{Title: "Fix input", Details: "criteria", Evidence: "failure", DependsOn: []string{"missing"}},
		{Evidence: "first untitled report"},
		{Evidence: "second untitled report"},
	} {
		e.addFinding(finding)
	}
	events, err := e.Store.RecentEvents(-1, 10)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	count := 0
	for _, event := range events {
		if event.Kind == "finding" {
			if seen[event.Message] {
				t.Fatal("different findings have identical messages")
			}
			seen[event.Message] = true
			count++
		}
	}
	if count != 4 {
		t.Fatalf("finding count %d", count)
	}
}
