package looper

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapCorrectsInvalidBacklog(t *testing.T) {
	valid, err := json.Marshal(testBacklog())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, raw string
	}{
		{"task round", strings.Replace(string(valid), `"priority":`, `"round":1,"priority":`, 1)},
		{"top level field", strings.Replace(string(valid), `"schema_version":`, `"round":1,"schema_version":`, 1)},
		{"priority", strings.Replace(string(valid), `"priority":1`, `"priority":7`, 1)},
		{"status", strings.Replace(string(valid), `"status":"todo"`, `"status":"invalid"`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, correct := range []bool{false, true} {
				t.Run(map[bool]string{false: "rejected twice", true: "corrected"}[correct], func(t *testing.T) {
					bin, _ := fakeAgent(t, tc.raw)
					log := filepath.Join(t.TempDir(), "calls")
					t.Setenv("LOOPER_FAKE_LOG", log)
					if correct {
						t.Setenv("LOOPER_FAKE_CORRECTED", string(valid))
						script, err := os.ReadFile(bin)
						if err != nil {
							t.Fatal(err)
						}
						script = []byte(strings.Replace(string(script), `result="$LOOPER_FAKE_RESULT"`, `result="$LOOPER_FAKE_RESULT"
if printf '%s' "$p" | grep -q 'previous backlog was rejected'; then result="$LOOPER_FAKE_CORRECTED"; fi`, 1))
						if err := os.WriteFile(bin, script, 0700); err != nil {
							t.Fatal(err)
						}
					}
					c := repoConfig(t, "run")
					c.CodexBin, c.ReviewAgent = bin, "codex"
					e, lock, err := NewEngine(c, io.Discard)
					if err != nil {
						t.Fatal(err)
					}
					defer lock.Close()
					defer e.Store.Close()
					err = e.bootstrap(context.Background(), nil)
					if correct {
						if err != nil {
							t.Fatal(err)
						}
						var got Backlog
						if err := readJSON(c.Todo, &got); err != nil {
							t.Fatal(err)
						}
						gotJSON, _ := json.Marshal(got)
						if string(gotJSON) != string(valid) {
							t.Fatalf("corrected backlog changed: %s", gotJSON)
						}
					} else {
						if err == nil || !strings.Contains(err.Error(), "rejected after correction") {
							t.Fatalf("expected bounded correction failure, got %v", err)
						}
						if _, err := os.Stat(c.Todo); !os.IsNotExist(err) {
							t.Fatalf("invalid backlog persisted: %v", err)
						}
					}
					calls, err := os.ReadFile(log)
					if err != nil || strings.Count(string(calls), "\n") != 2 {
						t.Fatalf("expected two calls: %q, %v", calls, err)
					}
				})
			}
		})
	}
}
