package looper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkerLogTracksRetriesAndChecks(t *testing.T) {
	e := snapshotEngine(t)
	a := e.State.Attempts[0]
	a.Directory = filepath.Join(e.Dir, "task-a")
	e.publishView(false)
	if _, text, err := workerLog(e.Dir, a.ID); err != nil || !strings.Contains(text, "Waiting for worker output") {
		t.Fatalf("empty log: %q %v", text, err)
	}
	stamp := time.Now().Add(-time.Hour)
	write := func(relative, text string) string {
		t.Helper()
		path := filepath.Join(a.Directory, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		stamp = stamp.Add(time.Second)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("work-00-session/setup.log", "setup")
	write("work-00-session/events-00.jsonl", "first attempt")
	want := write("work-00-session/events-01.jsonl", "second attempt")
	// Private temporary files are not output logs.
	write("work-00-session/tmp/events-99.jsonl", "wrong file")
	path, text, err := workerLog(e.Dir, a.ID)
	if err != nil || path != want || text != "second attempt" {
		t.Fatalf("retry log: %s %q %v", path, text, err)
	}
	a.Phase = "verifying"
	e.publishView(false)
	if _, text, err := workerLog(e.Dir, a.ID); err != nil || !strings.Contains(text, "Waiting") {
		t.Fatal("showed implementation output as a check")
	}
	want = write("checks/commit/verify.log", "tests pass\n")
	path, text, err = workerLog(e.Dir, a.ID)
	if err != nil || path != want || text != "tests pass\n" {
		t.Fatalf("check log: %s %q %v", path, text, err)
	}
	if _, _, err := workerLog(e.Dir, "../../secret"); err == nil {
		t.Fatal("accepted unknown attempt")
	}
	a.Phase = "done"
	e.publishView(false)
	if _, text, err := workerLog(e.Dir, a.ID); err != nil || text == "" {
		t.Fatal("completed task lost its retained logs")
	}
}
