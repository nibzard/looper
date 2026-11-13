package looper

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
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

// TestStopDuringReviewDrainsDespiteFileChange reproduces the 2026-11-13
// incident: a stop arrived while a review ran, a tracked file changed, and
// the controller started a second review instead of draining. A stop that
// targets this controller must drain at the boundary after the review.
func TestStopDuringReviewDrainsDespiteFileChange(t *testing.T) {
	bin, _ := fakeAgent(t, "")
	t.Setenv("LOOPER_FAKE_SLEEP", "2")
	t.Setenv("LOOPER_FAKE_REVIEW", `{"status":"reviewed","summary":"checked","findings":[],"complete":false}`)
	c := repoConfig(t, "run")
	c.CodexBin, c.ClaudeBin, c.ReviewAgent, c.ReviewEvery = bin, bin, "codex", 5
	b := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{{ID: "T001", Title: "done work", Priority: 1, Status: "done"}}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	if err = e.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.State.Integrated, e.State.ReviewedAt, e.State.ReviewPending = 5, 0, true
	if err = e.save(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testBudget(60*time.Second))
	defer cancel()
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := e.Run(ctx)
		done <- result{code, err}
	}()

	// Wait until the review agent runs, then stop and edit a tracked file.
	deadline := time.Now().Add(testBudget(20 * time.Second))
	for !reviewStarted(t, e) {
		if time.Now().After(deadline) {
			t.Fatal("the review never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err = writeStopRequest(e.Dir, stopRequest{Session: e.controller.Session}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.Root, "NOTES.md"), []byte("edited during the review\n"), 0644); err != nil {
		t.Fatal(err)
	}

	r := <-done
	if r.code != 0 || r.err != nil {
		t.Fatalf("the stop must drain with code 0: %d %v", r.code, r.err)
	}
	if exit := lastShutdown(t, e.Store); exit.Reason != "drained" {
		t.Fatalf("the stop was not honored: %+v", exit)
	}
	events, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	reviews := 0
	for _, ev := range events {
		if ev.Kind == "review" && strings.HasPrefix(ev.Message, "Checking") {
			reviews++
		}
	}
	if reviews > 1 {
		t.Fatalf("a drained controller must not start another review: %d reviews", reviews)
	}
}

func reviewStarted(t *testing.T, e *Engine) bool {
	t.Helper()
	events, err := e.Store.Events(0)
	if err != nil {
		return false
	}
	for _, ev := range events {
		if ev.Kind == "review-activity" {
			return true
		}
	}
	return false
}
