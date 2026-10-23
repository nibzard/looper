package looper

import (
	"github.com/nibzard/looper/internal/ui"
	"os"
	"testing"
)

func TestAttachmentFollowsReplacementAndGuardsActions(t *testing.T) {
	e := protocolEngine(t)
	a := &dashboardAttachment{observer: &observer{store: e.Store}, dir: e.Dir, session: "old"}
	publish := func(session string, count int) {
		t.Helper()
		if err := writeJSON(controllerPath(e.Dir), &ControllerInfo{Version: ControllerVersion, Session: session, PID: os.Getpid()}); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(viewPath(e.Dir), &ui.ViewSnapshot{Version: ui.ViewVersion, Session: session, Counters: ui.ViewCounters{Done: count}}); err != nil {
			t.Fatal(err)
		}
	}
	publish("old", 22)
	if v := a.poll(); v == nil || v.Counters.Done != 22 {
		t.Fatal("old snapshot missing")
	}
	publish("new", 24)
	if _, err := a.currentSession(); err == nil {
		t.Fatal("action targeted an unseen controller")
	}
	if _, err := a.inspect("T001"); err == nil {
		t.Fatal("inspector crossed sessions before overview")
	}
	if v := a.poll(); v == nil || v.Session != "new" || v.Counters.Done != 24 {
		t.Fatal("viewer failed to reconnect")
	}
	if session, err := a.currentSession(); err != nil || session != "new" {
		t.Fatalf("action session: %s %v", session, err)
	}
	if err := writeJSON(controllerPath(e.Dir), &ControllerInfo{Version: ControllerVersion, Session: "gone", PID: 0}); err != nil {
		t.Fatal(err)
	}
	if a.poll() != nil {
		t.Fatal("dead controller appeared connected")
	}
	if _, err := a.currentSession(); err == nil {
		t.Fatal("action accepted disconnected controller")
	}
}
