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
	for _, mode := range []string{"complete", "error", "drain", "cancel"} {
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
			if mode == "drain" || mode == "cancel" {
				c.Once = false
				c.Poll = time.Hour
			}
			if mode == "cancel" {
				want = 130
			}
			var stdout, stderr strings.Builder
			dashboard := func(controls ui.Controls) (ui.ExitAction, error) {
				if mode == "cancel" {
					controls.Cancel()
				}
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
			go func() { finished <- runDashboardController(c, &stdout, &stderr, dashboard) }()
			select {
			case code := <-finished:
				if code != want {
					t.Fatalf("exit %d, want %d: %s", code, want, stderr.String())
				}
				if mode == "drain" && (!strings.Contains(stdout.String(), "Drain complete") || !strings.Contains(stdout.String(), "looper tui")) {
					t.Fatalf("silent or unexplained drain: %q", stdout.String())
				}
				if mode == "cancel" && !strings.Contains(stdout.String(), "Interrupted") {
					t.Fatalf("silent cancellation: %q", stdout.String())
				}
				if mode != "drain" && strings.Contains(stdout.String(), "Drain complete") {
					t.Fatalf("incorrect drain claim for %s: %q", mode, stdout.String())
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

func TestDashboardExitUsesOwnSessionRecord(t *testing.T) {
	store := timingStore(t)
	if _, err := store.Event("shutdown", "older drain", sessionExit{Session: "drained", Reason: "drained", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Event("shutdown", "newer completion", sessionExit{Session: "completed", Reason: "complete", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	printDashboardExit(&out, store, "drained", 0)
	if !strings.Contains(out.String(), "Drain complete") {
		t.Fatalf("wrong session: %q", out.String())
	}
	out.Reset()
	printDashboardExit(&out, store, "completed", 0)
	if !strings.Contains(out.String(), "all planned work is complete") {
		t.Fatalf("completion: %q", out.String())
	}
	out.Reset()
	printDashboardExit(&out, store, "unknown", 0)
	if strings.Contains(out.String(), "cleanly") || !strings.Contains(out.String(), "inspect") {
		t.Fatalf("invented exit reason: %q", out.String())
	}
}
