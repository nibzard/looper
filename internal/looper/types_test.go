package looper

import (
	"encoding/json"
	"path/filepath"
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
