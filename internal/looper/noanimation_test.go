package looper

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// The animation opt-out must parse everywhere a dashboard can open, stay off
// by default, and never select a different output mode.
func TestParseNoAnimationFlag(t *testing.T) {
	clearSettingEnv(t)
	for _, args := range [][]string{
		{"--no-animation"},
		{"tui", "--no-animation"},
		{"start", "--no-animation"},
		{"--no-animation", "--json"},
	} {
		c, err := parseAt(t, args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !c.NoAnimation {
			t.Fatalf("%v: the flag must set the opt-out", args)
		}
		if c.Plain {
			t.Fatalf("%v: the opt-out must not select plain output", args)
		}
		if (len(args) > 0 && args[len(args)-1] == "--json") != c.JSON {
			t.Fatalf("%v: json output must stay unchanged, got %t", args, c.JSON)
		}
		want := "run"
		if args[0] == "tui" || args[0] == "start" {
			want = args[0]
		}
		if c.Command != want {
			t.Fatalf("%v: the command changed to %s", args, c.Command)
		}
	}
	c, err := parseAt(t, "tui")
	if err != nil {
		t.Fatal(err)
	}
	if c.NoAnimation {
		t.Fatal("animation must stay on by default")
	}
	if !strings.Contains(help, "--no-animation") {
		t.Fatal("the flag must appear in help")
	}
}

// The opt-out is presentation-only: a run with the flag completes normally
// and stores the same event sequence as a run without it.
func TestNoAnimationRunKeepsEventsAndExitCode(t *testing.T) {
	kinds := func(t *testing.T, withFlag bool) (string, []string) {
		t.Helper()
		args := []string{"--once"}
		if withFlag {
			args = append(args, "--no-animation")
		}
		c := preparedRepo(t, args...)
		e, lock, err := NewEngine(c, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		code, err := e.Run(ctx)
		events := storedEvents(t, e)
		e.Store.Close()
		lock.Close()
		if code != 0 || err != nil {
			t.Fatalf("no-animation=%t: finished project must complete cleanly, got %d (%v)", withFlag, code, err)
		}
		var out []string
		for _, ev := range events {
			out = append(out, ev.Kind)
			if ev.Kind == "config" {
				t.Fatalf("no-animation=%t: the flag must not emit events, got %q", withFlag, ev.Message)
			}
		}
		return strings.Join(out, " "), out
	}
	baseline, list := kinds(t, false)
	flagged, _ := kinds(t, true)
	if baseline != flagged {
		t.Fatalf("event kinds changed under --no-animation:\n%v\n%v", list, flagged)
	}
}

// The dashboard wrapper must forward the opt-out into the controls it hands
// to the UI.
func TestDashboardControllerThreadsNoAnimation(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[on], func(t *testing.T) {
			c := preparedRepo(t, "--once", "--no-animation")
			c.CodexBin, c.ClaudeBin = "/bin/false", "/bin/false"
			c.NoAnimation = on
			seen := make(chan bool, 4)
			dashboard := func(controls ui.Controls) (ui.ExitAction, error) {
				seen <- controls.NoAnimation
				return ui.ExitAction{}, nil
			}
			finished := make(chan int, 1)
			go func() { finished <- runDashboardController(c, io.Discard, io.Discard, dashboard) }()
			select {
			case got := <-seen:
				if got != on {
					t.Fatalf("controls.NoAnimation = %t, want %t", got, on)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the dashboard never started")
			}
			select {
			case code := <-finished:
				if code != 0 {
					t.Fatalf("controller exited %d", code)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("dashboard wrapper hung after controller completion")
			}
		})
	}
}
