package looper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlannerPublishesWhileWaitingAndCancels(t *testing.T) {
	e := snapshotEngine(t)
	bin := filepath.Join(t.TempDir(), "agent.sh")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho planning-output\nprintf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"name\":\"Read\",\"input\":{\"file_path\":\"Cargo.toml\"}}]}}'\nexec sleep 30\n"), 0755); err != nil {
		t.Fatal(err)
	}
	e.Config.ClaudeBin, e.Config.ReviewAgent, e.Config.RetryLimit = bin, "claude", 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := e.runPlanner(ctx, e.capture(), t.TempDir(), filepath.Join(e.Dir, "plan-test"), "plan")
		done <- err
	}()
	deadline := time.After(5 * time.Second)
	var first string
	updates := 0
	for updates < 2 {
		select {
		case v := <-e.viewNotify:
			if v.State != "planning" || v.ActivityLog == "" {
				continue
			}
			if first == "" {
				first = v.Now
				continue
			}
			if v.Now != first && v.ActivityDetail == "Read Cargo.toml" && v.ActivityUpdated != "" {
				updates++
			}
		case <-deadline:
			t.Fatal("planner did not publish live snapshots")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel succeeded unexpectedly")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("planner did not stop")
	}
	v := LoadView(e.Dir)
	if v.State != "" || v.ActivityLog != "" {
		t.Fatalf("planning activity retained: %+v", v)
	}
	log, err := plannerLog(e.Dir, filepath.Join(e.Dir, "plan-test", "events-00.jsonl"))
	if err != nil || !strings.Contains(log, "Read Cargo.toml") {
		t.Fatalf("log: %q, %v", log, err)
	}
}

func TestPlannerLogBoundsAndSanitizes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan-test", "events-00.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 20000)+"\x1b[31mend"), 0600); err != nil {
		t.Fatal(err)
	}
	text, err := plannerLog(dir, path)
	if err != nil || len(text) > 16000 || strings.Contains(text, "\x1b") || !strings.HasSuffix(text, "end") {
		t.Fatalf("invalid log tail: %d, %v", len(text), err)
	}
	if _, err := plannerLog(dir, filepath.Join(dir, "..", "secret")); err == nil {
		t.Fatal("accepted outside log")
	}
}

func TestPlannerReportsRetryAndCompletes(t *testing.T) {
	e := snapshotEngine(t)
	bin := filepath.Join(t.TempDir(), "agent.sh")
	script := `#!/bin/sh
if [ ! -f attempted ]; then
 touch attempted
 echo '429 rate limit'
 exit 1
fi
printf '%s\n' '{"result":"{\"tasks\":[]}"}'
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	e.Config.ClaudeBin, e.Config.ReviewAgent, e.Config.RetryLimit = bin, "claude", 2
	e.Config.RetryDelay = time.Millisecond
	raw, err := e.runPlanner(context.Background(), e.capture(), t.TempDir(), filepath.Join(e.Dir, "plan-retry"), "plan")
	if err != nil || string(raw) != `{"tasks":[]}` {
		t.Fatalf("result %s: %v", raw, err)
	}
	events, err := e.Store.RecentEvents(-1, 20)
	if err != nil {
		t.Fatal(err)
	}
	var messages string
	for _, event := range events {
		messages += event.Message + "\n"
	}
	for _, want := range []string{"attempt 1/2", "retry in", "attempt 2/2", "events-01.jsonl"} {
		if !strings.Contains(messages, want) {
			t.Fatalf("missing %q: %s", want, messages)
		}
	}
}
