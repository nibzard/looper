package looper

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDecodeNewTasks(t *testing.T) {
	for _, input := range []string{`{}`, `[]`, `null`, `{"id":"T2","typo":true}`, `{} {}`, `{"status":"done"}`} {
		tasks, err := decodeNewTasks([]byte(input))
		if err == nil {
			b := Backlog{Version: 1, Sources: []string{}, Tasks: tasks}
			err = b.Validate()
		}
		if err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	tasks, err := decodeNewTasks([]byte(`{"id":"T2","title":"New work","priority":2}`))
	if err != nil || len(tasks) != 1 || tasks[0].Status != "todo" {
		t.Fatalf("decode: %+v %v", tasks, err)
	}
}

func TestAddTaskTransportPreservesActiveWork(t *testing.T) {
	e := requeueEngine(t)
	e.State.Attempts[0].Phase = "implementing"
	before := hashJSON(e.State.Attempts)
	original := e.State.Backlog.Tasks[0]
	req := &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("add"), Type: RequestTypeAddTask, Session: e.controller.Session, Tasks: []Task{
		{ID: "T2", Title: "New work", Priority: 2},
		{ID: "T3", Title: "Dependent work", Priority: 3, DependsOn: []string{"T2"}},
	}}
	if err := writeJSON(filepath.Join(requestDir(e.Dir), req.ID+".json"), req); err != nil {
		t.Fatal(err)
	}
	e.processAddTaskRequests()
	var ack SettingsAck
	if err := readJSON(filepath.Join(ackDir(e.Dir), req.ID+".json"), &ack); err != nil || ack.Status != AckApplied {
		t.Fatalf("ack: %+v %v", ack, err)
	}
	if len(e.State.Backlog.Tasks) != 3 || !reflect.DeepEqual(original, e.State.Backlog.Tasks[0]) || before != hashJSON(e.State.Attempts) {
		t.Fatal("existing work changed")
	}
	var exported Backlog
	if err := readJSON(e.Config.Todo, &exported); err != nil || len(exported.Tasks) != 3 {
		t.Fatalf("export: %v", err)
	}
	if ack = e.applyAddTaskRequest(req); ack.Status != AckApplied || len(e.State.Backlog.Tasks) != 3 {
		t.Fatal("replay duplicated tasks")
	}
}

func TestAddTaskRejectsWholeBatch(t *testing.T) {
	for _, mode := range []string{"duplicate", "dependency", "cycle", "stale", "external", "status"} {
		t.Run(mode, func(t *testing.T) {
			e := requeueEngine(t)
			before := hashJSON(e.State.Backlog)
			req := &SettingsRequest{Version: 1, ID: identifier("add"), Session: e.controller.Session, Tasks: []Task{{ID: "T2", Title: "Work", Priority: 2}, {ID: "T3", Title: "Other", Priority: 2}}}
			switch mode {
			case "duplicate":
				req.Tasks[1].ID = "T1"
			case "dependency":
				req.Tasks[1].DependsOn = []string{"missing"}
			case "cycle":
				req.Tasks[0].DependsOn = []string{"T3"}
				req.Tasks[1].DependsOn = []string{"T2"}
			case "stale":
				req.Session = "old"
			case "external":
				if err := os.WriteFile(e.Config.Todo, []byte("external edit"), 0600); err != nil {
					t.Fatal(err)
				}
			case "status":
				req.Tasks[1].Status = "doing"
			}
			if ack := e.applyAddTaskRequest(req); ack.Status == AckApplied {
				t.Fatal("accepted invalid batch")
			}
			if hashJSON(e.State.Backlog) != before {
				t.Fatal("partially applied batch")
			}
		})
	}
}

func TestParseAddTask(t *testing.T) {
	c, err := Parse([]string{"add-task", "tasks.json", "--json"})
	if err != nil || c.Command != "add-task" || len(c.Plans) != 1 || !c.JSON {
		t.Fatalf("parse: %+v %v", c, err)
	}
}
