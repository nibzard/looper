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
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 70*time.Millisecond)
				defer stop()
			}
			started := time.Now()
			raw, err := e.runReviewAgent(ctx, e.capture(), t.TempDir(), filepath.Join(e.Dir, "review-test"), "review", 10*time.Millisecond)
			if cancel {
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
					t.Fatalf("cancellation: %v", err)
				}
			} else if err != nil || string(raw) != `{"ok":true}` {
				t.Fatalf("review result: %s %v", raw, err)
			}
			events, err := e.Store.RecentEvents(-1, 100)
			if err != nil {
				t.Fatal(err)
			}
			heartbeats := 0
			for _, event := range events {
				if event.Kind == "review-heartbeat" && strings.Contains(event.Message, "Log:") {
					heartbeats++
				}
			}
			if heartbeats == 0 {
				t.Fatal("silent review")
			}
			if e.activityLog != "" {
				t.Fatal("review left stale log metadata")
			}
		})
	}
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
