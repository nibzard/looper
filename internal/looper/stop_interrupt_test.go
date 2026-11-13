package looper

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// startStandInController starts a child process and records it as the
// owning controller, so looper stop --interrupt has a process it may signal
// without signaling the test binary itself.
func startStandInController(t *testing.T, dir, session string) *exec.Cmd {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("process identity checks read /proc")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	info := controllerMetadata(dir, session, nil)
	info.PID = cmd.Process.Pid
	if err := writeJSON(controllerPath(dir), info); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestStopInterruptSignalsOwningController(t *testing.T) {
	c := preparedRepo(t, "--once")
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	child := startStandInController(t, e.Dir, e.controller.Session)

	c.Command, c.Interrupt = "stop", true
	var out, stderr strings.Builder
	if code := runStop(c, e.Dir, &out, &stderr); code != 0 {
		t.Fatalf("interrupt with a live controller must succeed: %d %s %s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), e.controller.Session) || !strings.Contains(out.String(), "retained") {
		t.Fatalf("the interrupt must name the session and retained work: %q", out.String())
	}
	err = child.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the controller process must end by signal: %v", err)
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); !ok || status.Signal() != syscall.SIGINT {
		t.Fatalf("the controller must receive SIGINT: %v", err)
	}
	if _, err = os.Stat(stopRequestPath(e.Dir)); !os.IsNotExist(err) {
		t.Fatal("an interrupt must not write a drain request")
	}
}

func TestStopInterruptRefusesVersion2(t *testing.T) {
	c := preparedRepo(t, "--once")
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	child := startStandInController(t, e.Dir, e.controller.Session)

	c.Command, c.Interrupt, c.Contract = "stop", true, ContractVersionV2
	var out, stderr strings.Builder
	if code := runStop(c, e.Dir, &out, &stderr); code == 0 {
		t.Fatal("version 2 has no interrupt document and must refuse")
	}
	if err = child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("a refused interrupt must not signal the controller: %v", err)
	}
}

func TestInterruptOptionBelongsToStop(t *testing.T) {
	for _, args := range [][]string{
		{"status", "--interrupt"},
		{"stop", "--interrupt", "--wait"},
	} {
		if _, err := Parse(args); err == nil {
			t.Errorf("Parse(%q) must refuse", args)
		}
	}
	c, err := Parse([]string{"stop", "--interrupt"})
	if err != nil || !c.Interrupt {
		t.Fatalf("stop --interrupt must parse: %+v %v", c.Interrupt, err)
	}
}

func TestDoctorNamesControllerAndWarnsOnOldBuild(t *testing.T) {
	c := preparedRepo(t, "--once")
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	c.Command, c.Plain = "doctor", true

	var out strings.Builder
	doctor(c, &out)
	if !strings.Contains(out.String(), "Controller: running | session "+e.controller.Session) {
		t.Fatalf("doctor must name the live controller:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Warning: the controller runs") {
		t.Fatalf("a controller on this build must not warn:\n%s", out.String())
	}

	// An older build owns the same session: the record keeps this PID, so
	// the controller stays live while its build differs.
	writeControllerMetadata(t, e.Dir, e.controller.Session, nil)
	out.Reset()
	doctor(c, &out)
	if !strings.Contains(out.String(), "Warning: the controller runs looper 0.10.0 (deadbeef)") || !strings.Contains(out.String(), "looper stop") {
		t.Fatalf("doctor must warn about the controller build:\n%s", out.String())
	}
}
