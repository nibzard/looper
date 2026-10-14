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

func TestBootstrapAcceptsOneCorrectedResponse(t *testing.T) {
	b := testBacklog()
	b.Sources = []string{"to-do.json.context/bootstrap-old/backlog.json"}
	raw, _ := json.Marshal(b)
	bin, _ := fakeAgent(t, string(raw))
	script, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	b.Sources = []string{}
	corrected, _ := json.Marshal(b)
	t.Setenv("LOOPER_FAKE_CORRECTED", string(corrected))
	script = []byte(strings.Replace(string(script), `result="$LOOPER_FAKE_RESULT"`, `result="$LOOPER_FAKE_RESULT"
if printf '%s' "$p" | grep -q 'previous source list was rejected'; then result="$LOOPER_FAKE_CORRECTED"; fi`, 1))
	if err = os.WriteFile(bin, script, 0700); err != nil {
		t.Fatal(err)
	}
	c := repoConfig(t, "run")
	c.CodexBin = bin
	c.ReviewAgent = "codex"
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	if err = e.bootstrap(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	var accepted Backlog
	if err = readJSON(c.Todo, &accepted); err != nil || len(accepted.Sources) != 0 {
		t.Fatalf("correction not accepted: %+v %v", accepted, err)
	}
}
