package looper

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// runSignalDashboard starts the dashboard controller with a stub view, waits
// for its first published snapshot, delivers a signal to this process, and
// waits for the controller to finish. It reports the wrapper's exit code and
// output.
func runSignalDashboard(t *testing.T, c Config, signal func()) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	dashboard := func(controls ui.Controls) (ui.ExitAction, error) {
		select {
		case <-controls.Snapshot:
		case <-time.After(testBudget(5 * time.Second)):
			t.Error("no first snapshot")
			controls.Cancel()
		}
		signal()
		deadline := time.Now().Add(testBudget(10 * time.Second))
		for !controls.Done() {
			if time.Now().After(deadline) {
				t.Error("controller did not finish after the signal")
				controls.Cancel()
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		return ui.ExitAction{}, nil
	}
	finished := make(chan int, 1)
	go func() { finished <- runDashboardController(c, &stdout, &stderr, dashboard) }()
	select {
	case code := <-finished:
		return code, stdout.String(), stderr.String()
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatal("dashboard wrapper hung after the signal")
		return 0, "", ""
	}
}

// lastShutdownFor reports the newest shutdown event recorded in a run's store.
func lastShutdownFor(t *testing.T, dir string) sessionExit {
	t.Helper()
	s, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	return lastShutdown(t, s)
}

// An external SIGTERM to a tui controller must drain the run through the same
// stop request as `looper stop`, record a drained shutdown event, and exit
// cleanly. The tui dispatch skips Main's controller signal block, so the
// dashboard controller has to install the handlers itself; before that fix
// the signal closed only the view while the run kept going and a second
// signal killed the process.
func TestTUIControllerSigTermDrains(t *testing.T) {
	c := preparedRepo(t)
	c.Poll = time.Hour
	c.CodexBin, c.ClaudeBin = "/bin/false", "/bin/false"
	_, dir := projectPaths(c)
	code, stdout, stderr := runSignalDashboard(t, c, func() {
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
	})
	if code != 0 {
		t.Fatalf("drained tui controller must exit 0, got %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Drain complete") {
		t.Fatalf("silent drain: %q", stdout)
	}
	if exit := lastShutdownFor(t, dir); exit.Reason != "drained" {
		t.Fatalf("the drained run must record a shutdown event, got %+v", exit)
	}
	// The request that drained this run was consumed, so it cannot drain a
	// later one.
	if _, err := os.Stat(stopRequestPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("the completed drain left its request behind: %v", err)
	}
}

// A second SIGTERM must not fall back to the default disposition and kill the
// process with no shutdown event: the installed handler keeps owning the
// signal while the drain completes.
func TestTUIControllerSecondSigTermDoesNotKill(t *testing.T) {
	c := preparedRepo(t)
	c.Poll = time.Hour
	c.CodexBin, c.ClaudeBin = "/bin/false", "/bin/false"
	_, dir := projectPaths(c)
	code, stdout, stderr := runSignalDashboard(t, c, func() {
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
	})
	if code != 0 {
		t.Fatalf("drained tui controller must exit 0, got %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Drain complete") {
		t.Fatalf("silent drain: %q", stdout)
	}
	if exit := lastShutdownFor(t, dir); exit.Reason != "drained" {
		t.Fatalf("the drained run must record a shutdown event, got %+v", exit)
	}
}

// An external SIGINT to a tui controller must cancel with retained work and
// record a cancelled shutdown event.
func TestTUIControllerSigIntCancels(t *testing.T) {
	c := preparedRepo(t)
	c.Poll = time.Hour
	c.CodexBin, c.ClaudeBin = "/bin/false", "/bin/false"
	_, dir := projectPaths(c)
	code, stdout, stderr := runSignalDashboard(t, c, func() {
		syscall.Kill(os.Getpid(), syscall.SIGINT)
	})
	if code != 130 {
		t.Fatalf("interrupted tui controller must exit 130, got %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "Interrupted. Work is retained.") {
		t.Fatalf("silent cancellation: %q", stdout)
	}
	if exit := lastShutdownFor(t, dir); exit.Reason != "cancelled" {
		t.Fatalf("the cancelled run must record a shutdown event, got %+v", exit)
	}
}
