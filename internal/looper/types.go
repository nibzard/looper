package looper

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Task is an editable outcome. Attempts, not agents, own its execution state.
type Task struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Priority    int      `json:"priority"`
	Status      string   `json:"status"`
	Details     string   `json:"details,omitempty"`
	Steps       []string `json:"steps,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
	Files       []string `json:"files,omitempty"`
	Blockers    []string `json:"blockers,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	UpdatedAt   string   `json:"updated_at,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
	UnblockWhen string   `json:"unblock_when,omitempty"`
}

type Backlog struct {
	Project json.RawMessage `json:"project,omitempty"`
	Version int             `json:"schema_version"`
	Sources []string        `json:"source_files"`
	Goal    string          `json:"goal,omitempty"`
	Tasks   []Task          `json:"tasks"`
}

// backlogSchema must accept every field the engine itself writes, including
// goal and unblock_when. Looper writes it beside the task file when no schema
// exists so agents can validate its export. TestShippedSchemaMatchesDefault
// keeps this copy and the repository's to-do.schema.json identical.
const backlogSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "Looper Todo",
  "type": "object",
  "additionalProperties": false,
  "required": ["schema_version", "source_files", "tasks"],
  "properties": {
    "schema_version": { "type": "integer", "const": 1 },
    "goal": { "type": "string" },
    "project": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "name": { "type": "string" },
        "root": { "type": "string" }
      }
    },
    "source_files": {
      "type": "array",
      "items": { "type": "string" }
    },
    "tasks": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["id", "title", "priority", "status"],
        "properties": {
          "id": { "type": "string" },
          "title": { "type": "string", "minLength": 1 },
          "priority": { "type": "integer", "minimum": 1, "maximum": 5 },
          "status": { "type": "string", "enum": ["todo", "doing", "blocked", "done"] },
          "details": { "type": "string" },
          "steps": { "type": "array", "items": { "type": "string" } },
          "blockers": { "type": "array", "items": { "type": "string" } },
          "tags": { "type": "array", "items": { "type": "string" } },
          "files": { "type": "array", "items": { "type": "string" } },
          "depends_on": { "type": "array", "items": { "type": "string" } },
          "unblock_when": { "type": "string" },
          "created_at": { "type": "string", "format": "date-time" },
          "updated_at": { "type": "string", "format": "date-time" }
        }
      }
    }
  }
}
`

func (b Backlog) Validate() error {
	if b.Version != 1 {
		return fmt.Errorf("unsupported backlog version %d", b.Version)
	}
	if b.Tasks == nil || b.Sources == nil {
		return fmt.Errorf("backlog requires tasks and source_files arrays")
	}
	ids := map[string]Task{}
	for _, t := range b.Tasks {
		if t.ID == "" || t.Title == "" || t.Priority < 1 || t.Priority > 5 {
			return fmt.Errorf("invalid task %q", t.ID)
		}
		if _, ok := ids[t.ID]; ok {
			return fmt.Errorf("duplicate task %s", t.ID)
		}
		switch t.Status {
		case "todo", "doing", "done", "blocked":
		default:
			return fmt.Errorf("invalid status for %s", t.ID)
		}
		ids[t.ID] = t
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("dependency cycle at %s", id)
		}
		if visited[id] {
			return nil
		}
		t, ok := ids[id]
		if !ok {
			return fmt.Errorf("unknown dependency %s", id)
		}
		visiting[id] = true
		for _, dep := range t.DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		delete(visiting, id)
		visited[id] = true
		return nil
	}
	for id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backlog) Task(id string) *Task {
	for i := range b.Tasks {
		if b.Tasks[i].ID == id {
			return &b.Tasks[i]
		}
	}
	return nil
}
func (t Task) Marker() bool {
	for _, tag := range t.Tags {
		if tag == "project-done" {
			return true
		}
	}
	return false
}
func (t Task) Contract() string {
	t.Status = ""
	t.Files = nil
	t.Blockers = nil
	t.UpdatedAt = ""
	t.UnblockWhen = ""
	// Finding identity is controller metadata, not an acceptance criterion.
	// Keep operator ownership in the contract: changing it changes who can act.
	tags := make([]string, 0, len(t.Tags))
	for _, tag := range t.Tags {
		if !strings.HasPrefix(tag, findingKeyPrefix) {
			tags = append(tags, tag)
		}
	}
	if len(tags) == 0 {
		t.Tags = nil
	} else {
		t.Tags = tags
	}
	return hashJSON(t)
}
func hashJSON(v any) string  { data, _ := json.Marshal(v); return digest(data) }
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (b Backlog) Ready() []Task {
	byID := map[string]Task{}
	successors := map[string][]string{}
	for _, t := range b.Tasks {
		byID[t.ID] = t
		if t.Status != "done" {
			for _, d := range t.DependsOn {
				successors[d] = append(successors[d], t.ID)
			}
		}
	}
	lengths := map[string]int{}
	var length func(string) int
	length = func(id string) int {
		if v := lengths[id]; v > 0 {
			return v
		}
		v := 1
		for _, s := range successors[id] {
			if n := 1 + length(s); n > v {
				v = n
			}
		}
		lengths[id] = v
		return v
	}
	var ready []Task
	for _, t := range b.Tasks {
		if t.Marker() || t.OperatorOnly() || (t.Status != "todo" && t.Status != "doing") {
			continue
		}
		ok := true
		for _, d := range t.DependsOn {
			if byID[d].Status != "done" {
				ok = false
			}
		}
		if ok {
			ready = append(ready, t)
		}
	}
	sort.SliceStable(ready, func(i, j int) bool {
		a, c := ready[i], ready[j]
		if (a.Status == "doing") != (c.Status == "doing") {
			return a.Status == "doing"
		}
		if a.Priority != c.Priority {
			return a.Priority < c.Priority
		}
		if length(a.ID) != length(c.ID) {
			return length(a.ID) > length(c.ID)
		}
		return naturalLess(a.ID, c.ID)
	})
	return ready
}

func naturalLess(a, b string) bool { // IDs often use T2 and T10 without padding.
	for len(a) > 0 && len(b) > 0 {
		if a[0] >= '0' && a[0] <= '9' && b[0] >= '0' && b[0] <= '9' {
			i, j := 0, 0
			for i < len(a) && a[i] >= '0' && a[i] <= '9' {
				i++
			}
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			x, y := strings.TrimLeft(a[:i], "0"), strings.TrimLeft(b[:j], "0")
			if len(x) != len(y) {
				return len(x) < len(y)
			}
			if x != y {
				return x < y
			}
			a, b = a[i:], b[j:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

type Scope struct {
	TaskID    string   `json:"task_id"`
	Paths     []string `json:"write_paths"`
	Resources []string `json:"resources"`
}
type Plan struct {
	Setup  string  `json:"setup"`
	Verify string  `json:"verify"`
	Tasks  []Scope `json:"tasks"`
}

func (p *Plan) Validate(b Backlog, override string) error {
	if override != "" {
		p.Verify = override
	}
	if strings.TrimSpace(p.Verify) == "" {
		return fmt.Errorf("execution plan needs an executable verification command")
	}
	seen := map[string]bool{}
	for i := range p.Tasks {
		s := &p.Tasks[i]
		if b.Task(s.TaskID) == nil || seen[s.TaskID] {
			return fmt.Errorf("invalid plan task %q", s.TaskID)
		}
		seen[s.TaskID] = true
		if len(s.Paths) == 0 {
			s.Paths = []string{"."}
		}
		for j, path := range s.Paths {
			path = filepath.ToSlash(filepath.Clean(path))
			if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") || strings.ContainsAny(path, "*?[") {
				return fmt.Errorf("invalid write path %q", path)
			}
			s.Paths[j] = path
		}
	}
	for _, t := range b.Tasks {
		if t.Status != "done" && !t.Marker() && !seen[t.ID] {
			p.Tasks = append(p.Tasks, Scope{TaskID: t.ID, Paths: []string{"."}})
		}
	}
	return nil
}
func (p Plan) Scope(id string) Scope {
	for _, s := range p.Tasks {
		if s.TaskID == id {
			return s
		}
	}
	return Scope{TaskID: id, Paths: []string{"."}}
}
func overlaps(a, b Scope) bool {
	for _, x := range a.Paths {
		for _, y := range b.Paths {
			if x == "." || y == "." || x == y || strings.HasPrefix(x, y+"/") || strings.HasPrefix(y, x+"/") {
				return true
			}
		}
	}
	for _, x := range a.Resources {
		for _, y := range b.Resources {
			if x == y {
				return true
			}
		}
	}
	return false
}

type Finding struct {
	Owner          string   `json:"owner,omitempty"`
	Key            string   `json:"key,omitempty"`
	ExistingTaskID string   `json:"existing_task_id,omitempty"`
	Title          string   `json:"title"`
	Details        string   `json:"details"`
	Evidence       string   `json:"evidence"`
	DependsOn      []string `json:"depends_on,omitempty"`
	Priority       int      `json:"priority"`
}
type Summary struct {
	TaskID      string    `json:"task_id"`
	Status      string    `json:"status"`
	Summary     string    `json:"summary"`
	Files       []string  `json:"files"`
	Blockers    []string  `json:"blockers"`
	Followups   []Finding `json:"followups,omitempty"`
	UnblockWhen string    `json:"unblock_when,omitempty"`
}

func (s Summary) Validate(id string) error {
	if s.TaskID != id || (s.Status != "done" && s.Status != "blocked") || s.Summary == "" || s.Files == nil || s.Blockers == nil || (s.Status == "blocked" && len(s.Blockers) == 0) {
		return fmt.Errorf("invalid summary for task %s", id)
	}
	return nil
}

// Attempt is one worker run of a task. Phase is its position in the engine
// state machine: running, ready, verifying, integrated, done, blocked, or
// resume. Recovery coerces any other loaded value to ready or resume.
type Attempt struct {
	ID           string     `json:"id"`
	Task         Task       `json:"task"`
	Agent        string     `json:"agent"`
	Base         string     `json:"base"`
	Worktree     string     `json:"worktree"`
	Directory    string     `json:"directory"`
	Ref          string     `json:"ref"`
	Phase        string     `json:"phase"`
	ResultCommit string     `json:"result_commit,omitempty"`
	Summary      *Summary   `json:"summary,omitempty"`
	Verification *Candidate `json:"verification,omitempty"`
	Repairs      int        `json:"repairs"`
	Feedback     string     `json:"feedback,omitempty"`
	FailureHash  string     `json:"failure_hash,omitempty"`
	Repeats      int        `json:"repeats,omitempty"`
	Diagnosed    bool       `json:"diagnosed,omitempty"`
	Worktrees    []string   `json:"worktrees"`
}

// ImportRecord is the durable receipt for one plan import. Receipts are
// optional state fields: projects without imports serialize exactly as
// before, and older records load with empty import lists.
type ImportRecord struct {
	RequestID   string       `json:"request_id"`
	Digests     []string     `json:"digests"`
	Artifacts   []string     `json:"artifacts"`
	SourcePaths []string     `json:"source_paths,omitempty"`
	Status      string       `json:"status"`
	AddedTasks  []string     `json:"added_tasks,omitempty"`
	Covered     []CoveredRef `json:"covered,omitempty"`
	Conflicts   []string     `json:"conflicts,omitempty"`
	Error       string       `json:"error,omitempty"`
	Enqueued    string       `json:"enqueued"`
	Applied     string       `json:"applied,omitempty"`
}

// CoveredRef records a requirement already satisfied by existing work.
type CoveredRef struct {
	Requirement string `json:"requirement"`
	TaskID      string `json:"task_id,omitempty"`
	Evidence    string `json:"evidence,omitempty"`
}

// Import statuses across the durable receipt lifecycle.
const (
	ImportQueued   = "queued"
	ImportWaiting  = "waiting"
	ImportPlanning = "planning"
	ImportApplied  = "applied"
	ImportFailed   = "failed"
)

type Candidate struct {
	// Fatal launch errors stay transient; restart re-verifies retained work.
	LaunchFailure error  `json:"-"`
	Base          string `json:"base"`
	Commit        string `json:"commit"`
	Tree          string `json:"tree"`
	Worktree      string `json:"worktree,omitempty"`
	Before        string `json:"before,omitempty"`
	AttemptID     string `json:"attempt_id,omitempty"`
	OK            bool   `json:"ok"`
	Feedback      string `json:"feedback,omitempty"`
}
type State struct {
	PendingRunRepair  *pendingRunRepair `json:"pending_run_repair,omitempty"`
	Recoveries        []RunRecovery     `json:"recoveries,omitempty"`
	RecoveredCommands *RecoveryCommands `json:"recovered_commands,omitempty"`
	Version           int               `json:"version"`
	Root              string            `json:"root"`
	Todo              string            `json:"todo"`
	Head              string            `json:"head"`
	Branch            string            `json:"branch"`
	Ref               string            `json:"ref"`
	Tip               string            `json:"tip"`
	Published         string            `json:"published"`
	Backlog           Backlog           `json:"backlog"`
	ExportHash        string            `json:"export_hash"`
	ExportBefore      string            `json:"export_before,omitempty"`
	ExportPending     bool              `json:"export_pending,omitempty"`
	Attempts          []*Attempt        `json:"attempts"`
	Plan              *Plan             `json:"plan,omitempty"`
	PlanSignature     string            `json:"plan_signature,omitempty"`
	Publication       *Candidate        `json:"publication,omitempty"`
	Imports           []*ImportRecord   `json:"imports,omitempty"`
	Finished          bool              `json:"finished"`
	Integrated        int               `json:"integrated"`
	ReviewedAt        int               `json:"reviewed_at"`
	ReviewedTip       string            `json:"reviewed_tip,omitempty"`
	ReviewPending     bool              `json:"review_pending,omitempty"`
}

// Reject unknown plan fields instead of silently dropping user requirements.
func (t *Task) UnmarshalJSON(data []byte) error {
	type plain Task
	var v plain
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return err
	}
	*t = Task(v)
	return nil
}
func (b *Backlog) UnmarshalJSON(data []byte) error {
	type plain Backlog
	var v plain
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return err
	}
	*b = Backlog(v)
	return nil
}
