//go:build linux

package looper

import (
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// runTUIONTerminal starts the real controlling dashboard on a pseudoterminal
// pair and returns its exit channel plus the captured screen. The dashboard's
// own output stream is the master side, so the wrapper's final messages are
// observable like on a real terminal. The controller runs in the foreground,
// inside this process, so the signals under test reach it.
func runTUIONTerminal(t *testing.T, c Config) (chan int, func() string, *strings.Builder) {
	t.Helper()
	c.Foreground = true
	// A saved profile keeps the unattached start past the first-run form, so
	// the dashboard under test is the controller this helper waits for.
	writeProfileFile(t, c.ProfilePath, `{"version":1}`)
	masterIn, in := openPTY(t)
	_ = masterIn // no keystrokes drive this dashboard; signals end it
	masterOut, out := openPTY(t)
	swapStdio(t, in, out)
	t.Setenv("TERM", "xterm-256color")

	var mu sync.Mutex
	var screen strings.Builder
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := masterOut.Read(buf)
			if n > 0 {
				mu.Lock()
				screen.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	var stderr strings.Builder
	done := make(chan int, 1)
	go func() { done <- runTUI([]string{"tui"}, c, masterOut, &stderr) }()
	// The dashboard must be a live controller with a published view before
	// the signal lands, so the run under test is past its startup.
	_, dir := projectPaths(c)
	deadline := time.Now().Add(testBudget(10 * time.Second))
	for time.Now().Before(deadline) {
		if LoadView(dir) != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if LoadView(dir) == nil {
		t.Fatalf("the tui controller never published its first view: %s", stderr.String())
	}
	return done, func() string {
		mu.Lock()
		defer mu.Unlock()
		return screen.String()
	}, &stderr
}

// waitScreen polls the captured terminal until want appears. The pseudoterminal
// reader can lag the wrapper's return under load, so one read may miss the
// final lines.
func waitScreen(t *testing.T, screen func() string, want string) {
	t.Helper()
	deadline := time.Now().Add(testBudget(5 * time.Second))
	for time.Now().Before(deadline) {
		if strings.Contains(screen(), want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the terminal never showed %q: %q", want, screen())
}

// The acceptance path itself: an external SIGTERM to a live tui controller
// drains the run, records a drained shutdown event, and leaves the UI and its
// terminal in a clean state. Before T018 the signal closed only the Bubble Tea
// view while the engine kept running and the wrapper blocked forever.
func TestTUISigTermOnTerminalDrainsRun(t *testing.T) {
	c := preparedRepo(t, "tui")
	c.Poll = time.Hour
	c.CodexBin, c.ClaudeBin = "/bin/false", "/bin/false"
	done, screen, stderr := runTUIONTerminal(t, c)
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("a drained tui controller must exit 0, got %d: %s", code, stderr.String())
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatalf("the tui controller never drained: %s", stderr.String())
	}
	waitScreen(t, screen, "Drain complete")
	if strings.Contains(stderr.String(), "Error") {
		t.Fatalf("unclean drain: %s", stderr.String())
	}
}

// An external SIGINT to a live tui controller cancels with retained work and
// exits cleanly through the same wrapper.
func TestTUISigIntOnTerminalCancelsRun(t *testing.T) {
	c := preparedRepo(t, "tui")
	c.Poll = time.Hour
	c.CodexBin, c.ClaudeBin = "/bin/false", "/bin/false"
	done, screen, stderr := runTUIONTerminal(t, c)
	syscall.Kill(os.Getpid(), syscall.SIGINT)
	select {
	case code := <-done:
		if code != 130 {
			t.Fatalf("an interrupted tui controller must exit 130, got %d: %s", code, stderr.String())
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatalf("the tui controller never cancelled: %s", stderr.String())
	}
	waitScreen(t, screen, "Interrupted. Work is retained.")
}

// A dashboard whose caller owns the signals must not quit when SIGTERM
// arrives: the controller keeps draining on the visible board and the
// dashboard exits through its Done poll. This pins the Bubble Tea capture
// opt-out itself; with capture left on, the view would quit on the first
// signal while the run continued unseen.
func TestOwnSignalDashboardKeepsRunningThroughSigTerm(t *testing.T) {
	masterIn, in := openPTY(t)
	masterOut, out := openPTY(t)
	swapStdio(t, in, out)
	t.Setenv("TERM", "xterm-256color")
	go func() { io.Copy(io.Discard, masterOut) }()

	// The caller owns the signal, like runDashboardController does.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	defer signal.Stop(sigs)

	snapshots := make(chan ui.ViewSnapshot, 1)
	snapshots <- ui.ViewSnapshot{}
	var running int32 = 1
	exited := make(chan error, 1)
	go func() {
		_, err := ui.RunDashboard(ui.Controls{
			Snapshot:   snapshots,
			Done:       func() bool { return atomic.LoadInt32(&running) == 0 },
			OwnSignals: true,
		})
		exited <- err
	}()
	time.Sleep(testBudget(1 * time.Second))
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	deadline := time.Now().Add(testBudget(1500 * time.Millisecond))
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			t.Fatalf("the dashboard quit on SIGTERM instead of the owning handler: %v", err)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	atomic.StoreInt32(&running, 0)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("the dashboard exited with an error: %v", err)
		}
	case <-time.After(testBudget(5 * time.Second)):
		t.Fatal("the dashboard never exited after its controller finished")
	}
	_ = masterIn
}
