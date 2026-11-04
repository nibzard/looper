//go:build linux

package looper

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A real terminal exercise of the opt-out: the attached dashboard boots on a
// pseudoterminal, shows the bootstrap status through the static marker, never
// draws an animated spinner frame, and still detaches cleanly on q.
func TestTUIAttachNoAnimationShowsStaticStatus(t *testing.T) {
	c := preparedRepo(t, "tui", "--no-animation")
	_, dir := projectPaths(c)
	info, err := publishController(dir, ControllerModeDetached, "claude")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeController(dir, info.Session) })
	// A live owner with no published view synthesizes the bootstrap state.
	stream := filepath.Join(dir, "bootstrap-1", "events-00.jsonl")
	if err := os.MkdirAll(filepath.Dir(stream), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stream, []byte("{\"type\":\"init\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
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
	go func() { done <- runTUI(c, masterOut, &stderr) }()
	deadline := time.Now().Add(20 * time.Second)
	detached := false
	for time.Now().Before(deadline) && !detached {
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("dashboard exited %d: %s", code, stderr.String())
			}
			detached = true
		default:
			masterIn.WriteString("q")
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !detached {
		t.Fatalf("the dashboard never detached: %s", stderr.String())
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	rendered := screen.String()
	mu.Unlock()
	for _, want := range []string{"LOOPER", "BOOTSTRAPPING", "•"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("missing %q in rendered dashboard: %q", want, rendered)
		}
	}
	for _, frame := range []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"} {
		if strings.Contains(rendered, frame) {
			t.Fatalf("an animated spinner frame %q was drawn under --no-animation: %q", frame, rendered)
		}
	}
	if strings.Contains(stderr.String(), "interactive terminal") || strings.Contains(stderr.String(), "already running") {
		t.Fatalf("attach fell back instead of opening the dashboard: %s", stderr.String())
	}
}
