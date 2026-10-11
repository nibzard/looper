package looper

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

func TestDashboardControllerCompletion(t *testing.T) {
	for _, mode := range []string{"complete", "error", "drain"} {
		t.Run(mode, func(t *testing.T) {
			c := preparedRepo(t, "--once")
			c.CodexBin, c.ClaudeBin = "/bin/false", "/bin/false"
			want := 0
			if mode == "error" {
				if err := os.WriteFile(c.Todo, []byte("invalid JSON"), 0600); err != nil {
					t.Fatal(err)
				}
				want = 1
			}
			if mode == "drain" {
				c.Once = false
				c.Poll = time.Hour
			}
			var stderr strings.Builder
			dashboard := func(controls ui.Controls) (ui.ExitAction, error) {
				if mode == "drain" {
					// Wait until initialization has published the first snapshot.
					select {
					case <-controls.Snapshot:
					case <-time.After(5 * time.Second):
						t.Error("no first snapshot")
						controls.Cancel()
					}
					if err := controls.Drain(); err != nil {
						t.Error(err)
						controls.Cancel()
					}
				}
				deadline := time.Now().Add(5 * time.Second)
				for !controls.Done() {
					if time.Now().After(deadline) {
						t.Error("controller did not finish")
						controls.Cancel()
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				// Completion is observable more than once without consuming the result.
				if !controls.Done() {
					t.Error("completion was consumed")
				}
				return ui.ExitAction{}, nil
			}
			finished := make(chan int, 1)
			go func() { finished <- runDashboardController(c, io.Discard, &stderr, dashboard) }()
			select {
			case code := <-finished:
				if code != want {
					t.Fatalf("exit %d, want %d: %s", code, want, stderr.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("dashboard wrapper hung after controller completion")
			}
		})
	}
}

func TestIdleControllerDoesNotPlanWithoutWork(t *testing.T) {
	c := preparedRepo(t)
	c.Poll = time.Millisecond
	c.CodexBin, c.ClaudeBin = "/bin/false", "/bin/false"
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	code, err := e.Run(ctx)
	if code != 130 || err != context.DeadlineExceeded {
		t.Fatalf("idle controller attempted work: exit %d, %v", code, err)
	}
}
