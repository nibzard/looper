package looper

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// staleSchemaBytes models the drifted control-file copy T117 recorded: bytes
// that differ from the shipped reference and stay wrong until a declared
// reference refreshes them.
const staleSchemaBytes = "{\n  \"stale\": \"pre-cancellation copy\"\n}\n"

// referenceSchemaFixture prepares one repository whose schema contract is
// pinned at tracked content: docs/to-do.schema.json is committed beside the
// project, and the control file at the run root carries drifted bytes. The
// controller is not started; declare selects whether the caller declares the
// reference on the configuration.
func referenceSchemaFixture(t *testing.T, declare bool) (Config, []byte) {
	t.Helper()
	c := repoConfig(t, "run")
	for _, kv := range [][2]string{{"user.name", "Fixture Owner"}, {"user.email", "owner@example.test"}} {
		if _, err := gitText(c.Root, "config", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(c.Root, "docs"), 0755); err != nil {
		t.Fatal(err)
	}
	reference := []byte(backlogSchema)
	if err := os.WriteFile(filepath.Join(c.Root, "docs", "to-do.schema.json"), reference, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "add", "--", "docs/to-do.schema.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(c.Root, "commit", "-q", "-m", "pin the schema contract"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.SchemaPath(), []byte(staleSchemaBytes), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(c.Todo, Backlog{Version: 1, Sources: []string{}, Goal: "reference schema fixture", Tasks: []Task{
		{ID: "T001", Title: "existing", Priority: 1, Status: "todo"},
	}}); err != nil {
		t.Fatal(err)
	}
	if declare {
		c.SchemaReference = "docs/to-do.schema.json"
	}
	return c, reference
}

// A declared reference refreshes a drifted control file at startup, so the
// run root cannot keep bytes that differ from the shipped content.
func TestSchemaReferenceRefreshesDriftedControlFile(t *testing.T) {
	c, reference := referenceSchemaFixture(t, true)
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	refreshed, err := os.ReadFile(c.SchemaPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(refreshed) != string(reference) {
		t.Fatalf("the drifted control file must carry the declared reference bytes; got %d bytes", len(refreshed))
	}
	events, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, ev := range events {
		if ev.Kind == "schema-refreshed" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("the refresh must be recorded as one schema-refreshed event")
	}
}

// An assembled tree carries the reference bytes at the control-file path and
// the tracked reference itself beside them: the contract transports as
// ordinary content, and the seeded copy cannot be the drifted root copy.
func TestSchemaReferenceSeedsWorktreesFromTrackedContent(t *testing.T) {
	c, reference := referenceSchemaFixture(t, true)
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	work, err := e.Repo.Workspace(identifier("task"), e.State.Tip, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(work) })
	tracked, err := os.ReadFile(filepath.Join(work, "docs", "to-do.schema.json"))
	if err != nil {
		t.Fatalf("the tracked reference must transport into the worktree: %v", err)
	}
	if string(tracked) != string(reference) {
		t.Fatal("the tracked reference must arrive unchanged")
	}
	seeded, err := os.ReadFile(filepath.Join(work, e.Repo.Excluded[1]))
	if err != nil {
		t.Fatalf("the control-file schema must exist in the workspace: %v", err)
	}
	if string(seeded) != string(reference) {
		t.Fatal("the workspace schema must carry the reference bytes, not the drifted root copy")
	}
}

// Drift reintroduced at the run root after startup cannot re-enter: seeding
// reads the declared reference every time, so a stale copy never reaches an
// assembled tree while the declaration is in effect.
func TestSchemaReferenceHoldsAgainstRootCopyDrift(t *testing.T) {
	c, reference := referenceSchemaFixture(t, true)
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	if err = os.WriteFile(c.SchemaPath(), []byte(staleSchemaBytes), 0644); err != nil {
		t.Fatal(err)
	}
	work, err := e.Repo.Workspace(identifier("check"), e.State.Tip, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(work) })
	seeded, err := os.ReadFile(filepath.Join(work, e.Repo.Excluded[1]))
	if err != nil {
		t.Fatal(err)
	}
	if string(seeded) != string(reference) {
		t.Fatal("a drifted root copy must not seed a worktree under a declared reference")
	}
}

// Without a declaration the earlier rules stand: an existing control file is
// never touched, and it stays the seed every worktree copies.
func TestUndeclaredSchemaControlFileStaysUntouched(t *testing.T) {
	c, _ := referenceSchemaFixture(t, false)
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	current, err := os.ReadFile(c.SchemaPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != staleSchemaBytes {
		t.Fatal("an undeclared control file must keep its bytes")
	}
	work, err := e.Repo.Workspace(identifier("task"), e.State.Tip, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(work) })
	seeded, err := os.ReadFile(filepath.Join(work, e.Repo.Excluded[1]))
	if err != nil {
		t.Fatal(err)
	}
	if string(seeded) != staleSchemaBytes {
		t.Fatal("an undeclared run must keep seeding from the run root's copy")
	}
}

// A declaration that cannot name transportable tracked content is refused
// before any seed honors it halfway.
func TestSchemaReferenceDeclarationRefusals(t *testing.T) {
	c, _ := referenceSchemaFixture(t, false)
	outside := t.TempDir()
	target := filepath.Join(outside, "elsewhere.json")
	if err := os.WriteFile(target, []byte(backlogSchema), 0644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		set  func()
		want string
	}{
		{"a missing file", func() {
			c.SchemaReference = "docs/absent.schema.json"
		}, "is unreadable"},
		{"the schema control file", func() {
			c.SchemaReference = "to-do.schema.json"
		}, "control or runtime path"},
		{"the task file", func() {
			c.SchemaReference = "to-do.json"
		}, "control or runtime path"},
		{"inside the runtime tree", func() {
			c.SchemaReference = "base/docs/to-do.schema.json"
		}, "control or runtime path"},
		{"a symlink", func() {
			if err := os.Symlink(target, filepath.Join(c.Root, "docs", "link.schema.json")); err != nil {
				t.Fatal(err)
			}
			c.SchemaReference = "docs/link.schema.json"
		}, "must not be a symlink"},
		{"a directory", func() {
			c.SchemaReference = "docs"
		}, "must be one file"},
		{"invalid JSON", func() {
			if err := os.WriteFile(filepath.Join(c.Root, "docs", "broken.schema.json"), []byte("not json"), 0644); err != nil {
				t.Fatal(err)
			}
			c.SchemaReference = "docs/broken.schema.json"
		}, "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.set()
			_, err := OpenRepository(c, filepath.Join(t.TempDir(), "runtime"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

// The declaration reads from the environment, keeps one normalized spelling,
// and refuses values that cannot name content inside the repository.
func TestParseSchemaReference(t *testing.T) {
	clearSettingEnv(t)
	t.Setenv("LOOPER_SCHEMA_REFERENCE", "./docs/to-do.schema.json")
	c, err := parseAt(t, "run")
	if err != nil {
		t.Fatal(err)
	}
	if c.SchemaReference != "docs/to-do.schema.json" {
		t.Fatalf("the declaration must normalize to docs/to-do.schema.json, got %q", c.SchemaReference)
	}
	for _, invalid := range []string{"/elsewhere/to-do.schema.json", "../outside.json", "."} {
		t.Setenv("LOOPER_SCHEMA_REFERENCE", invalid)
		if _, err = parseAt(t, "run"); err == nil || !strings.Contains(err.Error(), "LOOPER_SCHEMA_REFERENCE") {
			t.Fatalf("the invalid declaration %q must fail with a usage error, got %v", invalid, err)
		}
	}
}
