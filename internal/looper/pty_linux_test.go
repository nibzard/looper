//go:build linux

package looper

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/mattn/go-isatty"
	"golang.org/x/sys/unix"
)

// openPTY allocates one pseudoterminal pair sized 80x24. Tests that need a
// real terminal on a standard stream cannot fake one with a pipe, because
// terminal detection is the behavior under test.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudoterminal available: %v", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		t.Fatalf("query pty number: %v", err)
	}
	if err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		t.Fatalf("unlock pty: %v", err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR, 0)
	if err != nil {
		master.Close()
		t.Fatalf("open pty slave: %v", err)
	}
	var ws struct{ row, col, x, y uint16 }
	ws.row, ws.col = 24, 80
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(), unix.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		t.Logf("pty size not set: %v", errno)
	}
	t.Cleanup(func() { master.Close(); slave.Close() })
	return master, slave
}

// swapStdio replaces the process standard input and output with terminals for
// one test and restores them afterwards.
func swapStdio(t *testing.T, in, out *os.File) {
	t.Helper()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = in, out
	t.Cleanup(func() { os.Stdin, os.Stdout = oldIn, oldOut })
}

// A piped writer must select plain output even when input is a real terminal.
func TestDetectModePipedWriterIsPlain(t *testing.T) {
	_, in := openPTY(t)
	t.Setenv("TERM", "xterm-256color")
	if !isatty.IsTerminal(in.Fd()) {
		t.Fatal("fixture input is not a terminal")
	}
	if got := DetectMode(in, &bytes.Buffer{}, false); got != ModePlain {
		t.Fatalf("a piped writer must select plain output, got %v", got)
	}
	// A file that is not a terminal behaves the same as an in-memory writer.
	if got := DetectMode(in, mustFile(t), false); got != ModePlain {
		t.Fatalf("a redirected writer must select plain output, got %v", got)
	}
}

func mustFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// TERM=dumb and an unset TERM select plain output on a full terminal pair;
// a usable TERM keeps the interactive layout.
func TestDetectModeTermDumbIsPlain(t *testing.T) {
	master, in := openPTY(t)
	for _, term := range []string{"dumb", ""} {
		t.Run("TERM="+term, func(t *testing.T) {
			t.Setenv("TERM", term)
			if got := DetectMode(in, master, false); got != ModePlain {
				t.Fatalf("TERM=%q must select plain output, got %v", term, got)
			}
		})
	}
	t.Setenv("TERM", "xterm-256color")
	if got := DetectMode(in, master, false); got != ModeInteractive {
		t.Fatalf("a usable TERM must keep the interactive layout, got %v", got)
	}
	// --plain wins even on a fully interactive terminal.
	if got := DetectMode(in, master, true); got != ModePlain {
		t.Fatalf("--plain must win over a terminal, got %v", got)
	}
}

// NO_COLOR keeps the interactive layout and drops color only; pipe, TERM=dumb,
// and --plain are the modes that select plain text.
func TestNoColorKeepsInteractiveLayoutWithoutColor(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	out := &bytes.Buffer{}
	p := NewPresenter(ModeInteractive, out, func() int { return 80 })
	if p.Color {
		t.Fatal("NO_COLOR must disable color")
	}
	p.Event("result", "T003  done | Add SQLite store")
	got := out.String()
	if strings.Contains(got, "\x1b") {
		t.Fatalf("NO_COLOR output must not contain escapes: %q", got)
	}
	if !strings.Contains(got, "✓") || !strings.Contains(got, "T003") {
		t.Fatalf("NO_COLOR must keep the interactive layout: %q", got)
	}
	// An empty NO_COLOR is not a request to disable color.
	t.Setenv("NO_COLOR", "")
	if !NewPresenter(ModeInteractive, out, func() int { return 80 }).Color {
		t.Fatal("an empty NO_COLOR must keep color")
	}
	// Color never decides the output mode.
	master, in := openPTY(t)
	t.Setenv("NO_COLOR", "1")
	if got := DetectMode(in, master, false); got != ModeInteractive {
		t.Fatalf("NO_COLOR must not force plain output, got %v", got)
	}
}

// A live controller owns the repository; the tui command must attach to it
// instead of starting a second controller, and must detach on q.
func TestTUIAttachesToOwnedRepository(t *testing.T) {
	c := preparedRepo(t)
	_, dir := projectPaths(c)
	info, err := publishController(dir, ControllerModeDetached, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeController(dir, info.Session) })
	common, err := gitText(c.Root, "rev-parse", "--git-common-dir")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(c.Root, common)
	}
	lock, err := lockRepository(common)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	// Two terminals avoid feeding the dashboard's own output back as input.
	masterIn, in := openPTY(t)
	masterOut, out := openPTY(t)
	go func() { io.Copy(io.Discard, masterOut) }()
	swapStdio(t, in, out)
	t.Setenv("TERM", "xterm-256color")

	var stderr strings.Builder
	done := make(chan int, 1)
	go func() { done <- runTUI(c, masterOut, &stderr) }()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("attached dashboard exited %d: %s", code, stderr.String())
			}
			if strings.Contains(stderr.String(), "already running") {
				t.Fatalf("tui started a second controller instead of attaching: %s", stderr.String())
			}
			if strings.Contains(stderr.String(), "interactive terminal") {
				t.Fatalf("tui fell back to plain output on a terminal: %s", stderr.String())
			}
			return
		default:
			masterIn.WriteString("q")
			time.Sleep(100 * time.Millisecond)
		}
	}
	t.Fatalf("the attached dashboard never detached: %s", stderr.String())
}

// The same dispatch must reach the attach path itself: with the repository
// owned but its metadata unreadable, tui reports the attach failure instead of
// starting or following a controller.
func TestTUIAttachReportsUnverifiableMetadata(t *testing.T) {
	c := preparedRepo(t)
	_, dir := projectPaths(c)
	if err := os.WriteFile(controllerPath(dir), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(controllerPath(dir)) })
	common, err := gitText(c.Root, "rev-parse", "--git-common-dir")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(c.Root, common)
	}
	lock, err := lockRepository(common)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	masterIn, in := openPTY(t)
	masterOut, out := openPTY(t)
	swapStdio(t, in, out)
	t.Setenv("TERM", "xterm-256color")
	var stderr strings.Builder
	done := make(chan int, 1)
	go func() { done <- runTUI(c, masterOut, &stderr) }()
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("attach must fail on unreadable metadata, exit %d: %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runTUI never returned")
	}
	if !strings.Contains(stderr.String(), "metadata cannot be verified") {
		t.Fatalf("expected the attach-path diagnostic, got %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "already running") {
		t.Fatalf("tui started a second controller: %s", stderr.String())
	}
	_ = masterIn
}

// removeController removes only the metadata of its own session.
func TestRemoveControllerKeepsForeignSessionRecord(t *testing.T) {
	dir := t.TempDir()
	other := &ControllerInfo{Version: ControllerVersion, Session: "run-other", PID: os.Getpid(), Root: "/repo", Mode: ControllerModeDetached}
	if err := writeJSON(controllerPath(dir), other); err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(dir, "looper.pid")
	if err := os.WriteFile(pidPath, []byte("123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	removeController(dir, "run-mine")
	if _, err := os.Stat(controllerPath(dir)); err != nil {
		t.Fatalf("a foreign session record must survive: %v", err)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("a foreign pid record must survive: %v", err)
	}
	removeController(dir, "run-other")
	if _, err := os.Stat(controllerPath(dir)); !os.IsNotExist(err) {
		t.Fatal("the owning session must remove its own record")
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatal("the owning session must remove its own pid record")
	}
}
