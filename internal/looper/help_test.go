package looper

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The contextual help block of one command (AX-026): the parser accepts the
// command, the block names the documents the command actually serves, and
// every example is an invocation the parser accepts as written. The checks
// are structural — a block cannot pass by wording alone.
func TestCommandHelpCoversTheSurface(t *testing.T) {
	if len(commandDocs) != len(clientCommands) {
		for _, name := range clientCommands {
			if _, ok := commandDocs[name]; !ok {
				t.Errorf("clientCommands lists %q with no help block", name)
			}
		}
		for name := range commandDocs {
			var listed bool
			for _, command := range clientCommands {
				if command == name {
					listed = true
				}
			}
			if !listed {
				t.Errorf("help block %q names a command the client does not list", name)
			}
		}
		t.Fatalf("help covers %d blocks for %d commands", len(commandDocs), len(clientCommands))
	}
	for name, doc := range commandDocs {
		if doc.usage == "" || doc.summary == "" || len(doc.examples) == 0 {
			t.Errorf("%s help lacks a usage line, a summary, or examples", name)
		}
		for _, ref := range []struct {
			path string
			kind string
		}{{doc.input, "input"}, {doc.legacy, "legacy"}, {doc.v2, "v2"}} {
			if ref.path != "" && !strings.HasPrefix(ref.path, "docs/") {
				t.Errorf("%s %s reference %q must name a repository path under docs/", name, ref.kind, ref.path)
			}
		}
		// Every referenced contract document exists in the repository.
		for _, ref := range []struct {
			path string
			kind string
		}{{doc.input, "input"}, {doc.legacy, "legacy"}, {doc.v2, "v2"}} {
			if ref.path == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join("../..", ref.path)); err != nil {
				t.Errorf("%s references missing %s document %s", name, ref.kind, ref.path)
			}
		}
		for _, example := range doc.examples {
			args, ok := exampleArgs(example)
			if !ok {
				t.Errorf("%s example %q is not a looper invocation", name, example)
				continue
			}
			if _, err := Parse(args); err != nil {
				t.Errorf("%s example %q does not parse: %v", name, example, err)
			}
		}
	}
}

// Every block renders, carries its usage line, and wraps without splitting a
// backtick span. The renderer is on the serving path of `looper help`, so a
// block that cannot render is a defect, not a formatting nit.
func TestCommandHelpRenders(t *testing.T) {
	for name := range commandDocs {
		rendered := renderCommandHelp(name)
		if !strings.Contains(rendered, "Usage: looper ") {
			t.Errorf("%s renders without a usage line:\n%s", name, rendered)
		}
		for _, line := range strings.Split(rendered, "\n") {
			if opens := strings.Count(line, "`"); opens%2 != 0 {
				t.Errorf("%s renders a line with an unterminated code span: %q", name, line)
			}
		}
	}
}

// The wrapper keeps a code span whole and stays inside the width it is given.
func TestHelpWrapKeepsCodeSpansWhole(t *testing.T) {
	text := "The receipt's `wait` object names `looper wait --contract v2 --request <id> --timeout 30s` for this request."
	wrapped := wrapHelp(text, 40)
	if strings.Count(wrapped, "`")%2 != 0 || strings.Count(wrapped, "\n") == 0 {
		t.Fatalf("wrapping split a code span or did not wrap:\n%s", wrapped)
	}
	for _, line := range strings.Split(wrapped, "\n") {
		if len(line) > 40+len("`looper wait --contract v2 --request <id> --timeout 30s`") {
			t.Errorf("line exceeds its bound by more than one unbreakable span: %q", line)
		}
	}
	if !strings.Contains(wrapped, "`looper wait --contract v2 --request <id> --timeout 30s`") {
		t.Errorf("wrapping broke the command span:\n%s", wrapped)
	}
}

// chdirTemp moves the test into a private directory and restores the working
// directory afterwards. The restore is absolute: later tests read repository
// files through relative paths, and a restore relative to the temporary
// directory would leave them resolving from /tmp.
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Errorf("restore the working directory: %v", err)
		}
	})
	return dir
}

// exampleArgs splits one `looper ...` example line into parser arguments.
func exampleArgs(example string) ([]string, bool) {
	fields := strings.Fields(example)
	if len(fields) == 0 || fields[0] != "looper" {
		return nil, false
	}
	return fields[1:], true
}

// The registry, the capabilities document, and the help blocks name one
// surface: every registry row has a help block, and every block's documents
// match the documents its registry row claims.
func TestCommandHelpMatchesContractRegistry(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(legacyContractDocs, "commands.json"))
	if err != nil {
		t.Skipf("command registry is absent: %v", err)
	}
	var registry struct {
		Commands []struct {
			Name       string   `json:"name"`
			Invocation []string `json:"invocation"`
			Legacy     *string  `json:"legacy"`
			V2         *string  `json:"v2"`
		} `json:"commands"`
	}
	if err = json.Unmarshal(b, &registry); err != nil {
		t.Fatalf("commands.json does not parse: %v", err)
	}
	seen := map[string]bool{}
	// A command may own several registry rows (recovery serves two
	// operations); the help block names the documents of one of them.
	legacyDocs := map[string]map[string]bool{}
	v2Docs := map[string]map[string]bool{}
	for _, command := range registry.Commands {
		seen[command.Name] = true
		if command.Legacy != nil && *command.Legacy != "" {
			if legacyDocs[command.Name] == nil {
				legacyDocs[command.Name] = map[string]bool{}
			}
			legacyDocs[command.Name]["docs/contracts/"+*command.Legacy] = true
		}
		if command.V2 != nil && *command.V2 != "" {
			if v2Docs[command.Name] == nil {
				v2Docs[command.Name] = map[string]bool{}
			}
			v2Docs[command.Name]["docs/contracts/"+*command.V2] = true
		}
	}
	for name, doc := range commandDocs {
		for _, ref := range []struct {
			listed map[string]bool
			block  string
			period string
		}{{legacyDocs[name], doc.legacy, "legacy"}, {v2Docs[name], doc.v2, "v2"}} {
			if len(ref.listed) == 0 != (ref.block == "") {
				t.Errorf("%s: the registry %s document and the help block disagree (registry %v, help %q)",
					name, ref.period, len(ref.listed) > 0, ref.block)
				continue
			}
			if ref.block != "" && !ref.listed[ref.block] {
				t.Errorf("%s: help names %s document %s, which the registry does not", name, ref.period, ref.block)
			}
		}
	}
	for _, command := range registry.Commands {
		if !seen[command.Name] {
			continue
		}
		if _, ok := commandDocs[command.Name]; !ok {
			t.Errorf("registry command %q has no help block", command.Name)
		}
	}
	for name := range commandDocs {
		if !seen[name] {
			t.Errorf("help block %q is absent from docs/contracts/commands.json", name)
		}
	}
}

// Contextual help reaches the caller through the command itself: `looper
// help <command>`, `looper <command> --help`, and the `looper <command> help`
// spelling all serve the same block, and a topic that names no command is a
// usage error rather than an empty answer.
func TestContextualHelpRoutes(t *testing.T) {
	var out bytes.Buffer
	if code := Main([]string{"help", "status"}, &out, &strings.Builder{}); code != 0 {
		t.Fatalf("help status exited %d", code)
	}
	block := out.String()
	if !strings.Contains(block, "Usage: looper status") {
		t.Fatalf("help status does not serve the status block:\n%s", block)
	}
	for _, args := range [][]string{
		{"status", "--help"},
		{"status", "help"},
		{"--help", "status"},
	} {
		out.Reset()
		if code := Main(args, &out, &strings.Builder{}); code != 0 {
			t.Fatalf("%v exited %d", args, code)
		}
		if out.String() != block {
			t.Fatalf("%v served a different block than help status:\n%s", args, out.String())
		}
	}
	// The overview keeps its own content and exit code.
	out.Reset()
	if code := Main([]string{"help"}, &out, &strings.Builder{}); code != 0 {
		t.Fatalf("help exited %d", code)
	}
	overview := out.String()
	if !strings.Contains(overview, "Usage: looper [run]") {
		t.Fatal("the overview lost its usage header")
	}
	if overview == block {
		t.Fatal("the overview and one command's block are the same document")
	}
	// An unknown topic is a usage error naming the overview route.
	out.Reset()
	code := Main([]string{"help", "not-a-command"}, &out, &strings.Builder{})
	if code != exitUsage {
		t.Fatalf("help not-a-command exited %d", code)
	}
	c, err := Parse([]string{"help", "not-a-command"})
	if err == nil {
		t.Fatal("an unknown help topic parsed")
	}
	p, ok := err.(*PublicError)
	if !ok || p.Code != "unknown_command" {
		t.Fatalf("unknown topic refusal: %+v", err)
	}
	_ = c
	// Help is text; a machine caller is routed to the capabilities document.
	if _, err = Parse([]string{"help", "--json"}); err == nil {
		t.Fatal("help --json parsed")
	}
	if p, ok = err.(*PublicError); !ok || p.Code != "option_command_mismatch" {
		t.Fatalf("help --json refusal: %+v", err)
	}
}

// An unknown command must fail as a usage error before anything starts: no
// controller session, no runtime directory, no saved state (AX-026). A bare
// word that names a task file still selects it.
func TestUnknownCommandFailsBeforeStarting(t *testing.T) {
	dir := chdirTemp(t)
	base := filepath.Join(dir, "base")
	t.Setenv("LOOPER_BASE_DIR", base)
	var out, stderr bytes.Buffer
	code := Main([]string{"bogus-command"}, &out, &stderr)
	if code != exitUsage {
		t.Fatalf("looper bogus-command exited %d with stdout %q", code, out.String())
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr does not name the unknown command: %q", stderr.String())
	}
	for _, created := range []string{base, filepath.Join(dir, "runtime")} {
		if _, err := os.Stat(created); err == nil {
			t.Fatalf("a refused command created %s", created)
		}
	}
	// The most common misspelling behaves the same way instead of starting
	// a run against a task file named "help".
	stderr.Reset()
	if code = Main([]string{"help"}, &out, &stderr); code != 0 {
		t.Fatalf("looper help exited %d: %q", code, stderr.String())
	}
	if !strings.Contains(out.String(), "Usage: looper [run]") {
		t.Fatalf("looper help did not print the overview: %q", out.String())
	}
}

// A task-file positional keeps its meaning: a path, a .json name, or an
// existing file selects the task file, while a word that names nothing is a
// command spelling Looper does not know.
func TestTaskFilePositionalSelection(t *testing.T) {
	dir := chdirTemp(t)
	bare := filepath.Join(dir, "backlog-copy")
	if err := os.WriteFile(bare, []byte(`{"schema_version":1,"goal":"g","tasks":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"sub/tasks.json":        "", // a path, absent or present
		"alternate-tasks.json":  "",
		"backlog-copy":          "", // an existing file without an extension
		"./backlog-copy":        "",
		"/abs/path/tasks.json":  "",
		"definitely-not-a-file": "unknown command",
	} {
		_, err := Parse([]string{name})
		if want == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s parsed as a task file", name)
		} else if p, ok := err.(*PublicError); !ok || !strings.Contains(p.Message, want) {
			t.Errorf("%s refusal: %+v", name, err)
		}
	}
}
