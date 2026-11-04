package looper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const RequestTypeAddTask = "add-task"

// decodeNewTasks accepts one task or a batch. Unknown fields are errors.
func decodeNewTasks(data []byte) ([]Task, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, fmt.Errorf("the task input is empty")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var tasks []Task
	if data[0] == '[' {
		if err := decoder.Decode(&tasks); err != nil {
			return nil, err
		}
	} else {
		var task Task
		if err := decoder.Decode(&task); err != nil {
			return nil, err
		}
		tasks = []Task{task}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON value")
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("supply at least one task")
	}
	for i := range tasks {
		if tasks[i].Status == "" {
			tasks[i].Status = "todo"
		}
		if tasks[i].Status != "todo" && tasks[i].Status != "blocked" {
			return nil, fmt.Errorf("new task %s must be todo or blocked", tasks[i].ID)
		}
	}
	return tasks, nil
}

func runAddTask(c Config, out, stderr io.Writer) int {
	if len(c.Plans) != 1 {
		fmt.Fprintln(stderr, "usage: looper add-task <file.json> [--json]")
		return 2
	}
	data, err := os.ReadFile(c.Plans[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if len(data) > 48<<10 {
		fmt.Fprintln(stderr, "task input exceeds 48 KiB")
		return 2
	}
	tasks, err := decodeNewTasks(data)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if !repositoryOwned(c) {
		fmt.Fprintln(stderr, "No active run. Start Looper before adding tasks.")
		return 1
	}
	_, dir := projectPaths(c)
	info, err := readController(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ack, err := submitSettingsRequest(dir, SettingsRequest{Version: SettingsRequestVersion, Type: RequestTypeAddTask, Session: info.Session, Tasks: tasks}, 30*time.Second)
	if c.JSON && ack != nil {
		json.NewEncoder(out).Encode(ack)
		if ack.Status == AckApplied {
			return 0
		}
		return 1
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if ack.Status != AckApplied {
		fmt.Fprintln(stderr, ack.Status+": "+ack.Message)
		return 1
	}
	fmt.Fprintln(out, ack.Message)
	return 0
}

func (e *Engine) processAddTaskRequests() {
	for {
		req, err := claimNextRequest(e.Dir, RequestTypeAddTask)
		if err != nil {
			e.event("request-error", err.Error())
			return
		}
		if req == nil {
			return
		}
		ack := e.applyAddTaskRequest(req)
		if err = writeJSON(filepath.Join(ackDir(e.Dir), req.ID+".json"), ack); err != nil {
			e.err = err
			return
		}
		if err = os.Remove(filepath.Join(processingDir(e.Dir), req.ID+".json")); err != nil {
			e.err = err
			return
		}
	}
}

func (e *Engine) applyAddTaskRequest(req *SettingsRequest) SettingsAck {
	ack := SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controller.Session, Status: AckError}
	if previous, ok := e.acks[req.ID]; ok {
		return previous
	}
	if req.Session != e.controller.Session {
		ack.Status = AckStale
		ack.Message = "The controller session changed. Resubmit the tasks."
		return ack
	}
	if e.inputPending || digestFile(e.Config.Todo) != e.State.ExportHash {
		ack.Status = AckConflict
		ack.Message = "The task file changed. Wait for Looper to import the edit."
		return ack
	}
	raw, err := json.Marshal(req.Tasks)
	if err != nil {
		ack.Message = err.Error()
		return ack
	}
	tasks, err := decodeNewTasks(raw)
	if err != nil {
		ack.Message = err.Error()
		return ack
	}
	next := e.State.Backlog
	next.Tasks = append(append([]Task{}, next.Tasks...), tasks...)
	if err = next.Validate(); err != nil {
		ack.Message = err.Error()
		return ack
	}
	e.State.Backlog = next
	// A request can apply while workers run, and the dispatch loop reads
	// scopes from the live plan on every tick. Keep it and invalidate only
	// the signature: the next planning pass rebuilds with the added tasks,
	// and a task missing from the retained plan scopes to the whole tree,
	// so parallel dispatch stays blocked until then.
	e.State.PlanSignature = ""
	e.State.Finished = false
	e.State.ReviewedTip = ""
	if err = e.export(); err != nil {
		e.err = err
		ack.Message = err.Error()
		return ack
	}
	ack.Status = AckApplied
	ack.Message = fmt.Sprintf("Added %d task(s). Active work continues.", len(tasks))
	e.acks[req.ID] = ack
	e.event("add-task", ack.Message)
	return ack
}
