package looper

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

func TestBootstrapRejectsPrivateSources(t *testing.T) {
	for _, source := range []string{"to-do.json.context/bootstrap-old/backlog.json", "./to-do.json.context/bootstrap-old/backlog.json", "docs/../to-do.json.context/bootstrap-old/backlog.json"} {
		t.Run(source, func(t *testing.T) {
			b := testBacklog()
			b.Sources = []string{source}
			raw, _ := json.Marshal(b)
			bin, _ := fakeAgent(t, string(raw))
			c := repoConfig(t, "run")
			c.CodexBin, c.ReviewAgent = bin, "codex"
			e, lock, err := NewEngine(c, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			defer e.Store.Close()
			err = e.bootstrap(context.Background(), nil)
			if err == nil || !strings.Contains(err.Error(), "private context") {
				t.Fatalf("accepted transient source: %v", err)
			}
			if _, err := os.Stat(c.Todo); !os.IsNotExist(err) {
				t.Fatalf("invalid backlog persisted: %v", err)
			}
		})
	}
}

func TestBootstrapSourcePaths(t *testing.T) {
	r := &Repository{Excluded: []string{"tasks.json"}}
	for _, source := range []string{"tasks.json.context/bootstrap/backlog.json", ".looper-context/x", "/tmp/bootstrap/backlog.json", "../private/backlog.json"} {
		if err := r.validateBootstrapSources([]string{source}); err == nil {
			t.Errorf("accepted %q", source)
		}
	}
	for _, source := range []string{"PROJECT.md", "docs/context/bootstrap.json", "tasks.json.context-notes.md", "docs/../PROJECT.md"} {
		if err := r.validateBootstrapSources([]string{source}); err != nil {
			t.Errorf("rejected %q: %v", source, err)
		}
	}
}
