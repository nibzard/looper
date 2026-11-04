package looper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewHeartbeatAndCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			e := snapshotEngine(t)
			bin := filepath.Join(t.TempDir(), "agent")
			delay := ".15"
			if cancel {
				delay = "5"
			}
			script := "#!/bin/sh\nsleep " + delay + "\nprintf '%s\\n' '{\"type\":\"result\",\"result\":\"{\\\"ok\\\":true}\"}'\n"
			if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			e.Config.ClaudeBin, e.Config.ReviewAgent, e.Config.RetryLimit = bin, "claude", 1
			ctx := context.Background()
			if cancel {
				// Cancel from the first heartbeat instead of a fixed offset.
				// Setup before the heartbeat loop has no bounded duration, so
				// a deadline that expires there returns before any heartbeat
				// exists and reports a silent review on a slow machine.
				var stop context.CancelFunc
				ctx, stop = context.WithCancel(ctx)
				defer stop()
				watched := make(chan struct{})
				defer close(watched)
				go func() {
					defer stop()
					for reviewHeartbeats(e) == 0 {
						select {
						case <-watched:
							return
						case <-time.After(time.Millisecond):
						}
					}
				}()
			}
			started := time.Now()
			raw, err := e.runReviewAgent(ctx, e.capture(), t.TempDir(), filepath.Join(e.Dir, "review-test"), "review", 10*time.Millisecond)
			if cancel {
				if !errors.Is(err, context.Canceled) || time.Since(started) > 3*time.Second {
					t.Fatalf("cancellation: %v", err)
				}
			} else if err != nil || string(raw) != `{"ok":true}` {
				t.Fatalf("review result: %s %v", raw, err)
			}
			if reviewHeartbeats(e) == 0 {
				t.Fatal("silent review")
			}
			if e.activityLog != "" {
				t.Fatal("review left stale log metadata")
			}
		})
	}
}

func reviewHeartbeats(e *Engine) int {
	events, err := e.Store.RecentEvents(-1, 100)
	if err != nil {
		return 0
	}
	count := 0
	for _, event := range events {
		if event.Kind == "review-heartbeat" && strings.Contains(event.Message, "Log:") {
			count++
		}
	}
	return count
}

func TestDashboardReadsReviewLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review-test", "events-00.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("review output\n"), 0600); err != nil {
		t.Fatal(err)
	}
	text, err := plannerLog(dir, path)
	if err != nil || text != "review output\n" {
		t.Fatalf("review log: %q %v", text, err)
	}
	if _, err := plannerLog(dir, filepath.Join(dir, "..", "review-test", "events-00.jsonl")); err == nil {
		t.Fatal("accepted log outside runtime directory")
	}
}
