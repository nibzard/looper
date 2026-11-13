package looper

import (
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestForegroundOptionBelongsToTUI(t *testing.T) {
	for _, args := range [][]string{{"--foreground"}, {"start", "--foreground"}, {"attach", "--foreground"}} {
		if _, err := Parse(args); err == nil {
			t.Errorf("Parse(%q) must refuse", args)
		}
	}
	c, err := Parse([]string{"tui", "--foreground"})
	if err != nil || !c.Foreground {
		t.Fatalf("tui --foreground must parse: %+v %v", c.Foreground, err)
	}
}

// The attached interrupt signals only the session the dashboard displays, so
// a replacement controller is never stopped by a stale dashboard.
func TestAttachmentInterruptSignalsOnlyItsSession(t *testing.T) {
	c := preparedRepo(t, "--once")
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	child := startStandInController(t, e.Dir, e.controller.Session)

	stale := &dashboardAttachment{observer: &observer{store: e.Store}, dir: e.Dir, session: "run-earlier"}
	if err = stale.interrupt(); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("a stale session must refuse: %v", err)
	}
	if err = child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("a refused interrupt signaled the controller: %v", err)
	}

	current := &dashboardAttachment{observer: &observer{store: e.Store}, dir: e.Dir, session: e.controller.Session}
	if err = current.interrupt(); err != nil {
		t.Fatalf("interrupt of the attached session: %v", err)
	}
	err = child.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the controller process must end by signal: %v", err)
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); !ok || status.Signal() != syscall.SIGINT {
		t.Fatalf("the controller must receive SIGINT: %v", err)
	}
}

func TestPrintDetachNamesWhatContinues(t *testing.T) {
	c := preparedRepo(t, "--once")
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	session := e.controller.Session
	for _, tt := range []struct{ kind, session, want string }{
		{"", session, "keeps running in the background"},
		{"", "run-earlier", ""},
		{"drain", session, "Drain requested"},
		{"interrupt", session, "Interrupt sent to session " + session},
	} {
		var out strings.Builder
		printDetach(&out, e.Dir, tt.session, tt.kind)
		if tt.want == "" && out.Len() != 0 || !strings.Contains(out.String(), tt.want) {
			t.Errorf("printDetach(%q, %q) = %q, want %q", tt.kind, tt.session, out.String(), tt.want)
		}
	}
}
