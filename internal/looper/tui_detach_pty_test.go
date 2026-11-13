//go:build linux

package looper

import (
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// An unattached `looper tui` starts a daemon controller and attaches to it.
// q, q detaches and the run keeps going; a second tui attaches to the same
// controller, and q, c interrupts it with retained work.
func TestTUIStartsDaemonAndDetachLeavesItRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("process-level daemon test")
	}
	bin := detachedBinary(t)
	c := preparedRepo(t, "tui")
	c.Poll = time.Hour
	c.CodexBin, c.ClaudeBin = "/bin/false", "/bin/false"
	// A saved profile keeps the start past the first-run form.
	writeProfileFile(t, c.ProfilePath, `{"version":1}`)
	orig := startExecutable
	startExecutable = func() (string, error) { return bin, nil }
	t.Cleanup(func() { startExecutable = orig })
	_, dir := projectPaths(c)
	t.Cleanup(func() {
		if info, err := readController(dir); err == nil {
			syscall.Kill(info.PID, syscall.SIGINT)
		}
		waitControllerGone(t, dir)
	})

	masterIn, in := openPTY(t)
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
	shown := func() string {
		mu.Lock()
		defer mu.Unlock()
		return screen.String()
	}
	// session runs one tui and answers the exit dialog with the given key.
	session := func(answer string) {
		t.Helper()
		var stderr strings.Builder
		done := make(chan int, 1)
		go func() { done <- runTUI([]string{"tui"}, c, masterOut, &stderr) }()
		before := strings.Count(shown(), "Detach from controller?")
		waitScreen(t, shown, "LOOPER")
		deadline := time.Now().Add(testBudget(10 * time.Second))
		for strings.Count(shown(), "Detach from controller?") == before && time.Now().Before(deadline) {
			masterIn.WriteString("q")
			time.Sleep(200 * time.Millisecond)
		}
		masterIn.WriteString(answer)
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("tui exited %d: %s", code, stderr.String())
			}
		case <-time.After(testBudget(10 * time.Second)):
			t.Fatalf("tui never exited after %q: %s", answer, stderr.String())
		}
	}

	session("q")
	info, err := readController(dir)
	if err != nil {
		t.Fatalf("the tui must leave a controller behind: %v", err)
	}
	if info.Mode != ControllerModeDetached || info.PID == os.Getpid() || !controllerAlive(info) {
		t.Fatalf("the tui controller must be a live daemon: %+v", info)
	}
	waitScreen(t, shown, "keeps running in the background")

	session("c")
	waitScreen(t, shown, "Interrupt sent to session "+info.Session)
	waitControllerGone(t, dir)
}

func waitControllerGone(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(testBudget(10 * time.Second))
	for time.Now().Before(deadline) {
		if _, err := os.Stat(controllerPath(dir)); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the detached controller did not stop")
}
