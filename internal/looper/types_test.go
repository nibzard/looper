package looper

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testBacklog() Backlog {
	return Backlog{Version: 1, Sources: []string{}, Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
		{ID: "T2", Title: "two", Priority: 1, Status: "todo"},
		{ID: "T3", Title: "three", Priority: 1, Status: "todo", DependsOn: []string{"T2"}},
		{ID: "T4", Title: "four", Priority: 1, Status: "todo", DependsOn: []string{"T3"}},
	}}
}
func TestTaskDependencies(t *testing.T) {
	b := testBacklog()
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := b.Ready()[0].ID; got != "T2" {
		t.Fatal(got)
	}
	b.Tasks[1].Priority = 2
	if got := b.Ready()[0].ID; got != "T1" {
		t.Fatal(got)
	}
	b.Tasks[1].DependsOn = []string{"T4"}
	if b.Validate() == nil {
		t.Fatal("cycle accepted")
	}
	b.Tasks[1].DependsOn = []string{"unknown"}
	if b.Validate() == nil {
		t.Fatal("unknown dependency accepted")
	}
}
func TestContractIgnoresExecutionOnly(t *testing.T) {
	a := testBacklog().Tasks[0]
	b := a
	b.Status = "blocked"
	b.Blockers = []string{"offline"}
	b.UnblockWhen = "restore network"
	if a.Contract() != b.Contract() {
		t.Fatal("execution changed contract")
	}
	b.Details = "different acceptance"
	if a.Contract() == b.Contract() {
		t.Fatal("acceptance not covered")
	}
}
func TestPlanScopes(t *testing.T) {
	b := testBacklog()
	p := Plan{Verify: "make test", Tasks: []Scope{{TaskID: "T1", Paths: []string{"./src//main.go"}}}}
	if err := p.Validate(b, ""); err != nil {
		t.Fatal(err)
	}
	if p.Tasks[0].Paths[0] != "src/main.go" {
		t.Fatal(p)
	}
	if !overlaps(p.Scope("T1"), p.Scope("T2")) {
		t.Fatal("missing scope must be exclusive")
	}
	if overlaps(Scope{Paths: []string{"src"}}, Scope{Paths: []string{"src2"}}) {
		t.Fatal("prefix collision")
	}
	if !overlaps(Scope{Resources: []string{"db"}}, Scope{Resources: []string{"db"}}) {
		t.Fatal("resource collision missed")
	}
	for _, path := range []string{"../outside", "/absolute", "*.go"} {
		p.Tasks[0].Paths = []string{path}
		if p.Validate(b, "") == nil {
			t.Fatal(path)
		}
	}
}
func TestStoreDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	want := &State{Version: 2, Backlog: testBacklog(), ExportPending: true, ExportBefore: "old"}
	if err = s.Save(want); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if hashJSON(got) != hashJSON(want) {
		t.Fatalf("%+v", got)
	}
}
func TestAgentSchemaAndResourceIsolation(t *testing.T) {
	var schema any
	if err := json.Unmarshal([]byte(summarySchema), &schema); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	a := resourceEnv(filepath.Join(root, "task-a", "work-00"))
	b := resourceEnv(filepath.Join(root, "task-b", "work-00"))
	if a["LOOPER_RESOURCE_PREFIX"] == b["LOOPER_RESOURCE_PREFIX"] {
		t.Fatal("workers share resource prefix")
	}
}
func TestClassifyFailure(t *testing.T) {
	transient := []string{
		"API Error: 429 {\"type\":\"error\"}",
		"rate limit exceeded; retry later",
		"502 Bad Gateway",
		"error code: 503",
		"connection reset by peer",
		"the server is temporarily unavailable",
	}
	for _, in := range transient {
		if got := classifyFailure("exit status 1\n" + in); got != "transient" {
			t.Fatalf("%q classified %s", in, got)
		}
	}
	// Session output that only mentions a timeout or a bare number must not
	// turn a deterministic failure into hours of retries.
	execution := []string{
		"FAIL: TestServerTimeout timed out after 10s",
		"edited net_timeout.go and ran checks; elapsed 502ms",
		"exit status 1",
	}
	for _, in := range execution {
		if got := classifyFailure("exit status 1\n" + in); got != "execution" {
			t.Fatalf("%q classified %s", in, got)
		}
	}
	if got := classifyFailure("exit status 1\n401 unauthorized: invalid api key"); got != "configuration" {
		t.Fatal(got)
	}
}
func TestTail(t *testing.T) {
	if string(tail([]byte("abcdef"), 3)) != "def" {
		t.Fatal("tail kept the wrong end")
	}
	if string(tail([]byte("ab"), 3)) != "ab" {
		t.Fatal("tail truncated a short input")
	}
}
func TestSchemaPath(t *testing.T) {
	for todo, want := range map[string]string{
		"/repo/to-do.json":          "/repo/to-do.schema.json",
		"/repo/dir/my-backlog.json": "/repo/dir/my-backlog.schema.json",
		"/repo/backlog":             "/repo/backlog.schema.json",
	} {
		if got := (Config{Todo: todo}).SchemaPath(); got != want {
			t.Fatalf("%s: %s", todo, got)
		}
	}
}
func TestParseGitTimeout(t *testing.T) {
	t.Setenv("LOOPER_GIT_TIMEOUT_SECONDS", "0")
	prev := gitTimeout
	defer func() { gitTimeout = prev }()
	if _, err := Parse(nil); err != nil {
		t.Fatal(err)
	}
	if gitTimeout != 0 {
		t.Fatal("git timeout setting ignored")
	}
}
func TestMergeUnique(t *testing.T) {
	got := mergeUnique([]string{"b", "a"}, []string{"a", "c", "a"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatal(got)
	}
	if got := mergeUnique(nil, nil); len(got) != 0 {
		t.Fatal(got)
	}
}
func TestApplyResultAccumulates(t *testing.T) {
	root := t.TempDir()
	todo := filepath.Join(root, "to-do.json")
	b := testBacklog()
	if err := writeJSON(todo, b); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data, err := os.ReadFile(todo)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Config: Config{Root: root, Todo: todo}, Repo: &Repository{Root: root}, Store: s, State: &State{Version: 2, Root: root, Todo: todo, Backlog: b, ExportHash: digest(data)}, Output: io.Discard}
	a := &Attempt{ID: "task-1", Task: b.Tasks[0], Directory: root}
	e.State.Attempts = append(e.State.Attempts, a)
	a.Summary = &Summary{TaskID: "T1", Status: "blocked", Summary: "missing key", Files: []string{"src/a.go"}, Blockers: []string{"missing API key"}}
	if err = e.applyResult(a); err != nil {
		t.Fatal(err)
	}
	a.Summary = &Summary{TaskID: "T1", Status: "done", Summary: "unblocked and complete", Files: []string{"src/b.go"}, Blockers: []string{"missing API key"}}
	if err = e.applyResult(a); err != nil {
		t.Fatal(err)
	}
	if err = e.hold(a, "worker failed"); err != nil {
		t.Fatal(err)
	}
	task := e.State.Backlog.Task("T1")
	if strings.Join(task.Files, ",") != "src/a.go,src/b.go" {
		t.Fatal(task.Files)
	}
	if strings.Join(task.Blockers, ",") != "missing API key,worker failed" {
		t.Fatal(task.Blockers)
	}
}
func TestObserveScope(t *testing.T) {
	b := testBacklog()
	plan := &Plan{Verify: "make test", Tasks: []Scope{{TaskID: "T1", Paths: []string{"src/auth"}}}}
	s, err := OpenStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := &Engine{Repo: &Repository{Root: "/repo", Excluded: []string{"to-do.json"}}, Store: s, State: &State{Backlog: b, Plan: plan}, Output: io.Discard}
	a := &Attempt{Task: b.Tasks[0], Phase: "running", Summary: &Summary{Files: []string{"src/auth/a.go", "src/db/config.go", "src/db/config.go", "/etc/abs", "../up", "to-do.json"}}}
	e.State.Attempts = append(e.State.Attempts, a)
	e.observeScope(a)
	if strings.Join(plan.Tasks[0].Paths, ",") != "src/auth,src/db/config.go" {
		t.Fatal(plan.Tasks[0].Paths)
	}
	if e.compatible(b.Tasks[1]) {
		t.Fatal("observed overlap did not block parallel dispatch")
	}
}
func TestShippedSchemaMatchesDefault(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "to-do.schema.json"))
	if err != nil {
		t.Skip(err)
	}
	if string(b) != backlogSchema {
		t.Fatal("to-do.schema.json differs from the default Looper writes")
	}
	var schema map[string]any
	if err = json.Unmarshal([]byte(backlogSchema), &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	if _, ok := properties["goal"]; !ok {
		t.Fatal("schema rejects the engine's goal field")
	}
	tasks := properties["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if _, ok := tasks["unblock_when"]; !ok {
		t.Fatal("schema rejects the engine's unblock_when field")
	}
}
