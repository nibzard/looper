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
// exists so agents can validate its export. That on-disk file is a control
// file: Repository.Excluded always lists it, snapshots strip it, and Workspace
// seeds each assembled tree from the run root's copy, so its bytes are runtime
// state a candidate cannot transport. The shipped reference copy is tracked
// content at docs/to-do.schema.json; TestShippedSchemaMatchesDefault keeps the
// two identical.
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
          "status": { "type": "string", "enum": ["todo", "doing", "blocked", "done", "cancelled"] },
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
		case "todo", "doing", "done", "blocked", "cancelled":
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
		if t.Marker() || t.OperatorOnly() || t.Deferred() || (t.Status != "todo" && t.Status != "doing") {
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

// writePathError reports the plan scope whose write path cannot scope
// repository work. The task identity lets a caller quarantine that one task
// instead of failing the whole controller.
type writePathError struct {
	TaskID string
	Path   string
}

func (e *writePathError) Error() string { return fmt.Sprintf("invalid write path %q", e.Path) }

// validWritePath normalizes one write path and reports whether it can scope
// repository work: relative to the repository root, inside it, and literal.
// Plan scopes and task files entries follow the same rule because a planner
// copies a task's files list into its write paths.
func validWritePath(path string) (string, bool) {
	p := filepath.ToSlash(filepath.Clean(path))
	return p, !(filepath.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") || strings.ContainsAny(p, "*?["))
}

// invalidFiles lists the normalized entries in t.Files that cannot scope
// repository work.
func (t Task) invalidFiles() []string {
	var bad []string
	for _, f := range t.Files {
		if p, ok := validWritePath(f); !ok {
			bad = append(bad, p)
		}
	}
	return bad
}

// reportedFiles splits a summary's reported file entries into the entries a
// plan scope can use and the normalized rejects. applyResult merges only the
// usable entries, so a worker report cannot put an unscannable path into a
// task's files list, where the next planning pass would copy it into its
// write paths.
func reportedFiles(files []string) (usable, rejected []string) {
	for _, f := range files {
		if p, ok := validWritePath(f); !ok {
			rejected = append(rejected, p)
			continue
		}
		usable = append(usable, f)
	}
	return usable, rejected
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
			normalized, ok := validWritePath(path)
			if !ok {
				return &writePathError{TaskID: s.TaskID, Path: normalized}
			}
			s.Paths[j] = normalized
		}
	}
	for _, t := range b.Tasks {
		// Cancelled work never runs again, so it takes no write scope: a
		// default whole-tree scope for it would veto every overlapping task
		// in compatible() for work that no longer exists (AX-019).
		if t.Status != "done" && t.Status != "cancelled" && !t.Marker() && !seen[t.ID] {
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

// dropScope removes one task's scope so validation can reject that task
// alone. The next Validate pass re-adds the repository default for an
// unfinished task.
func (p *Plan) dropScope(id string) {
	kept := make([]Scope, 0, len(p.Tasks))
	for _, s := range p.Tasks {
		if s.TaskID != id {
			kept = append(kept, s)
		}
	}
	p.Tasks = kept
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
	DeferUntil     string   `json:"defer_until,omitempty"`
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
// state machine: running, ready, verifying, integrated, done, blocked,
// resume, or cancelled (a resume attempt retired by task cancellation,
// AX-019). Recovery coerces any other loaded value to ready or resume.
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
	Adjustments []string     `json:"adjustments,omitempty"`
	Gates       []ImportGate `json:"gates,omitempty"`
	Error       string       `json:"error,omitempty"`
	Enqueued    string       `json:"enqueued"`
	Applied     string       `json:"applied,omitempty"`
	// IdempotencyKey and PayloadDigest carry the caller key one import was
	// submitted with, when it carried one (AX-012). Optional state fields,
	// omitted when empty, exactly like the rest of the import record.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	PayloadDigest  string `json:"payload_digest,omitempty"`
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
	// ExportSemantics is the semantic fingerprint of the content ExportHash
	// digests (AX-018). It decides whether changed task-file bytes differ
	// only in formatting while an export is pending and the in-memory
	// backlog already carries the pending mutation. States saved before it
	// existed load with it empty and fall back to the saved backlog.
	ExportSemantics string          `json:"export_semantics,omitempty"`
	ExportBefore    string          `json:"export_before,omitempty"`
	ExportPending   bool            `json:"export_pending,omitempty"`
	Attempts        []*Attempt      `json:"attempts"`
	Plan            *Plan           `json:"plan,omitempty"`
	PlanSignature   string          `json:"plan_signature,omitempty"`
	Publication     *Candidate      `json:"publication,omitempty"`
	Imports         []*ImportRecord `json:"imports,omitempty"`
	// Requests holds the durable request receipts (AX-009). Each commits in
	// the same save as the mutation it records, so a receipt and its outcome
	// never disagree across a restart.
	Requests []*RequestRecord `json:"requests,omitempty"`
	// Cancellations holds decided task cancellations whose target work has
	// not reached a safe boundary (AX-019). Each survives a restart and the
	// loop drives it at every boundary until the work drains; the receipt
	// with the same request identity stays waiting until then. Optional
	// state field: states saved before AX-019 load with none.
	Cancellations []*PendingCancellation `json:"cancellations,omitempty"`
	Finished      bool                   `json:"finished"`
	Integrated    int                    `json:"integrated"`
	ReviewedAt    int                    `json:"reviewed_at"`
	ReviewedTip   string                 `json:"reviewed_tip,omitempty"`
	ReviewPending bool                   `json:"review_pending,omitempty"`
	// StartupSeed marks a state written only so a starting controller's run
	// is publicly queryable before readiness is reported (AX-022). It is not
	// a baseline to resume: initialization treats it as absent and seeds the
	// real baseline itself, adopting the startup snapshot when the project
	// did not move under it. Only the bootstrap window keeps the marker
	// saved; every completed initialization overwrites the state without it.
	StartupSeed bool `json:"startup_seed,omitempty"`
}

// PendingCancellation is one decided cancellation waiting for its target
// work to drain (AX-019). TaskIDs is the decided set — the target and every
// live dependent a cascade took along — fixed at decision time against the
// revision the caller read. The drive applies it at the first boundary where
// no task in the set holds an active attempt; a task that completed before
// that boundary keeps its accepted result and is named in the outcome.
type PendingCancellation struct {
	RequestID string   `json:"request_id"`
	TaskIDs   []string `json:"task_ids"`
	DecidedAt string   `json:"decided_at"`
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
