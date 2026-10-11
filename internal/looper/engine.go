package looper

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Engine has one owner goroutine. Workers return values; they never mutate state.
type Engine struct {
	Config              Config
	Repo                *Repository
	Store               *Store
	State               *State
	Agent               Agent
	Dir, Logs           string
	Output              io.Writer
	checkSlots          chan struct{}
	wg                  sync.WaitGroup
	capacity, successes int
	err                 error
}

func projectPaths(c Config) (string, string) {
	h := sha1.Sum([]byte(c.Root))
	slug := strings.Trim(regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(filepath.Base(c.Root), "_"), "_")
	logs := filepath.Join(c.BaseDir, slug+"-"+hex.EncodeToString(h[:4]))
	return logs, filepath.Join(logs, "runtime")
}
func identifier(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}
func clone[T any](v T) T { b, _ := json.Marshal(v); var out T; _ = json.Unmarshal(b, &out); return out }

func NewEngine(c Config, out io.Writer) (*Engine, *os.File, error) {
	logs, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, err
	}
	r, err := OpenRepository(c, dir)
	if err != nil {
		return nil, nil, err
	}
	lock, err := lockRepository(r.Common)
	if err != nil {
		return nil, nil, err
	}
	s, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		lock.Close()
		return nil, nil, err
	}
	os.Remove(filepath.Join(dir, "stop"))
	entries, _ := os.ReadDir(filepath.Join(dir, "worktrees"))
	for _, entry := range entries {
		for _, prefix := range []string{"plan-", "review-", "check-", "bootstrap-"} {
			if strings.HasPrefix(entry.Name(), prefix) {
				r.Remove(filepath.Join(dir, "worktrees", entry.Name()))
				break
			}
		}
	}
	return &Engine{Config: c, Repo: r, Store: s, Agent: Agent{c}, Dir: dir, Logs: logs, Output: out, checkSlots: make(chan struct{}, c.Checks), capacity: c.Jobs}, lock, nil
}
func (e *Engine) event(kind, message string) {
	if e.err != nil {
		return
	}
	if err := e.Store.Event(kind, message, nil); err != nil {
		e.err = err
		return
	}
	if e.Config.JSON {
		b, _ := json.Marshal(Event{Time: time.Now().UTC().Format(time.RFC3339), Kind: kind, Message: message})
		fmt.Fprintln(e.Output, string(b))
	} else {
		fmt.Fprintln(e.Output, message)
	}
}
func (e *Engine) save() error {
	if e.err != nil {
		return e.err
	}
	return e.Store.Save(e.State)
}

// Export is journaled in SQLite first. Recovery recognizes both the old and new
// file contents. An external edit is never silently replaced by a status export.
func (e *Engine) export() error {
	if err := e.State.Backlog.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(e.State.Backlog, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	next := digest(data)
	if !e.State.ExportPending {
		e.State.ExportBefore = e.State.ExportHash
		e.State.ExportPending = true
		if err = e.save(); err != nil {
			return err
		}
	}
	current, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		return err
	}
	observed := digest(current)
	if observed != e.State.ExportBefore && observed != next {
		return fmt.Errorf("the backlog changed outside Looper; work is retained in %s", e.Dir)
	}
	if observed != next {
		if err = atomicWrite(e.Config.Todo, data, 0600); err != nil {
			return err
		}
	}
	e.State.ExportHash = next
	e.State.ExportBefore = ""
	e.State.ExportPending = false
	return e.save()
}
func (e *Engine) original() error {
	h, b := e.Repo.Head()
	if h != e.State.Head || b != e.State.Branch {
		return fmt.Errorf("the project branch moved; retained work is in %s", e.Dir)
	}
	return nil
}
func (e *Engine) contract(a *Attempt) error {
	t := e.State.Backlog.Task(a.Task.ID)
	if t == nil || t.Contract() != a.Task.Contract() {
		return fmt.Errorf("task %s changed while its work was retained", a.Task.ID)
	}
	return nil
}
func retained(s *State) bool {
	if s.Publication != nil {
		return true
	}
	for _, a := range s.Attempts {
		switch a.Phase {
		case "done", "blocked", "abandoned":
		default:
			return true
		}
	}
	return false
}
func (e *Engine) initialize(ctx context.Context) error {
	if _, err := os.Stat(e.Config.Todo); errors.Is(err, os.ErrNotExist) {
		if err = e.bootstrap(ctx); err != nil {
			return err
		}
	}
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		return err
	}
	var b Backlog
	if err = json.Unmarshal(data, &b); err != nil {
		return fmt.Errorf("invalid backlog: %w", err)
	}
	if err = b.Validate(); err != nil {
		return err
	}
	s, err := e.Store.Load()
	if err != nil {
		return err
	}
	if s == nil {
		s, err = e.importLegacy(b, data)
		if err != nil {
			return err
		}
	}
	if s != nil {
		if s.Version != 2 || s.Root != e.Config.Root || s.Todo != e.Config.Todo {
			return fmt.Errorf("saved state belongs to another project, task file, or version")
		}
		e.State = s
		if s.ExportPending {
			if err = e.export(); err != nil {
				return err
			}
			data, err = os.ReadFile(e.Config.Todo)
			if err != nil {
				return err
			}
			b = clone(s.Backlog)
		}
		if retained(s) {
			if digest(data) != s.ExportHash {
				return fmt.Errorf("the backlog changed while work was retained; restore its saved contract before resuming")
			}
			if err = e.original(); err != nil {
				return err
			}
			if s.Publication != nil {
				if err = e.publish(); err != nil {
					return err
				}
			}
			tree, err := e.Repo.Snapshot(e.Config.Root)
			if err != nil {
				return err
			}
			if tree != s.Published {
				return fmt.Errorf("project files changed since the interrupted run; work is retained in %s", e.Dir)
			}
			for _, a := range s.Attempts {
				if a.Phase == "done" || a.Phase == "blocked" || a.Phase == "abandoned" {
					continue
				}
				if err = e.contract(a); err != nil {
					return err
				}
				if a.Phase == "running" || a.Phase == "resume" {
					var receipt resultReceipt
					if readJSON(filepath.Join(a.Directory, "receipt.json"), &receipt) == nil && receipt.Base == a.Base && receipt.Contract == a.Task.Contract() && receipt.Repairs == a.Repairs {
						if err = receipt.Summary.Validate(a.Task.ID); err != nil {
							return err
						}
						a.Summary = &receipt.Summary
						a.ResultCommit = receipt.Commit
						if receipt.Summary.Status == "blocked" {
							if err = e.applyResult(a); err != nil {
								return err
							}
							continue
						}
					}
				}
				if a.Phase == "integrated" {
					if err = e.applyResult(a); err != nil {
						return err
					}
					continue
				}
				if a.ResultCommit != "" {
					a.Phase = "ready"
				} else {
					a.Phase = "resume"
				}
			}
			e.event("resume", "Resuming retained work.")
			return e.save()
		}
		// With no in-flight work the editable plan and current working tree may
		// start a new baseline. Archived state remains available for inspection.
		if err = writeJSON(filepath.Join(e.Dir, "history", identifier("state")+".json"), s); err != nil {
			return err
		}
		return e.refreshIdle()
	}
	head, branch := e.Repo.Head()
	tree, err := e.Repo.Snapshot(e.Config.Root)
	if err != nil {
		return err
	}
	seed, err := e.Repo.Commit(tree, []string{head}, "Snapshot project for Looper")
	if err != nil {
		return err
	}
	ref := "refs/looper/" + identifier("run")
	if err = e.Repo.Ref(ref, seed); err != nil {
		return err
	}
	e.State = &State{Version: 2, Root: e.Config.Root, Todo: e.Config.Todo, Head: head, Branch: branch, Ref: ref, Tip: seed, Published: tree, Backlog: b, ExportHash: digest(data), Attempts: []*Attempt{}}
	return e.save()
}

func (e *Engine) bootstrap(ctx context.Context) error {
	head, _ := e.Repo.Head()
	tree, err := e.Repo.Snapshot(e.Config.Root)
	if err != nil {
		return err
	}
	seed, err := e.Repo.Commit(tree, []string{head}, "Inspect project for Looper")
	if err != nil {
		return err
	}
	name := identifier("bootstrap")
	empty := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{}}
	work, err := e.Repo.Workspace(name, seed, empty)
	if err != nil {
		return err
	}
	defer e.Repo.Remove(work)
	e.event("bootstrap", "Reading project requirements and creating the first tasks...")
	prompt := `Bootstrap a development backlog for Looper. Read repository instructions and project specifications.
Return one JSON object: {"schema_version":1,"goal":"the existing project's intended outcome","source_files":["relative requirements files"],"tasks":[{"id":"T001","title":"outcome","priority":1,"status":"todo","details":"acceptance criteria","depends_on":[]}]}.
Only plan work grounded in the existing requirements. Include setup and executable verification where missing.
Use dependencies. Separate external prerequisites from implementable work. Do not modify any files.`
	raw, err := e.Agent.Run(ctx, e.Config.ReviewAgent, work, filepath.Join(e.Dir, name), prompt, false)
	if err != nil {
		return err
	}
	var b Backlog
	if err = json.Unmarshal(raw, &b); err != nil {
		return err
	}
	if err = b.Validate(); err != nil {
		return err
	}
	after, err := e.Repo.Snapshot(work)
	if err != nil {
		return err
	}
	if after != tree {
		return fmt.Errorf("bootstrap changed code; its result was not applied")
	}
	return writeJSON(e.Config.Todo, b)
}

func (e *Engine) plan(ctx context.Context) error {
	contracts := []string{}
	for _, t := range e.State.Backlog.Tasks {
		contracts = append(contracts, t.Contract())
	}
	signature := hashJSON(struct {
		Contracts     []string
		Setup, Verify string
	}{contracts, e.Config.Setup, e.Config.Verify})
	if e.State.Plan != nil && e.State.PlanSignature == signature {
		return nil
	}
	name := identifier("plan")
	work, err := e.Repo.Workspace(name, e.State.Tip, e.State.Backlog)
	if err != nil {
		return err
	}
	defer e.Repo.Remove(work)
	e.event("plan", "Planning independent tasks and executable checks...")
	raw, err := e.Agent.Run(ctx, e.Config.ReviewAgent, work, filepath.Join(e.Dir, name), planPrompt(e.State.Backlog), false)
	if err != nil {
		return err
	}
	var p Plan
	if err = json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if e.Config.Setup != "" {
		p.Setup = e.Config.Setup
	}
	if err = p.Validate(e.State.Backlog, e.Config.Verify); err != nil {
		return err
	}
	tree, err := e.Repo.Snapshot(work)
	if err != nil {
		return err
	}
	if tree != e.State.Published {
		return fmt.Errorf("planning changed code; its result was not applied")
	}
	e.State.Plan = &p
	e.State.PlanSignature = signature
	return e.save()
}

func (e *Engine) command(ctx context.Context, command, work, dir, name string) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	select {
	case e.checkSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-e.checkSlots }()
	return runProcess(ctx, work, filepath.Join(dir, name+".log"), []string{"bash", "-lc", command}, nil, resourceEnv(dir))
}

type workerResult struct {
	ID      string
	Summary *Summary
	Commit  string
	Err     error
}

type resultReceipt struct {
	Base     string  `json:"base"`
	Contract string  `json:"contract"`
	Repairs  int     `json:"repairs"`
	Summary  Summary `json:"summary"`
	Commit   string  `json:"commit"`
}

func (e *Engine) execute(ctx context.Context, a Attempt, b Backlog, p Plan) (r workerResult) {
	r.ID = a.ID
	defer func() {
		if r.Err == nil && r.Summary != nil {
			r.Err = writeJSON(filepath.Join(a.Directory, "receipt.json"), resultReceipt{a.Base, a.Task.Contract(), a.Repairs, *r.Summary, r.Commit})
		}
	}()
	dir := filepath.Join(a.Directory, fmt.Sprintf("work-%02d-%s", a.Repairs, identifier("session")))
	feedback := a.Feedback
	if err := e.command(ctx, p.Setup, a.Worktree, dir, "setup"); err != nil {
		if ctx.Err() != nil {
			r.Err = ctx.Err()
			return r
		}
		feedback += "\nWorkspace setup failed. Diagnose and repair the setup before completing the task. If it requires an external prerequisite, report blocked.\nCommand: " + p.Setup + "\n" + logTail(filepath.Join(dir, "setup.log"))
	}
	raw, err := e.Agent.Run(ctx, a.Agent, a.Worktree, dir, workerPrompt(b, a.Task, feedback), true)
	var summary Summary
	if err == nil {
		if err = json.Unmarshal(raw, &summary); err == nil {
			err = summary.Validate(a.Task.ID)
		}
		if err != nil {
			err = &AgentError{Kind: "invalid_result", Detail: err.Error()}
		}
	}
	var agentErr *AgentError
	if errors.As(err, &agentErr) && agentErr.Kind == "invalid_result" {
		// Recover only the report. An invalid report must not rerun implementation.
		before, snapshotErr := e.Repo.Snapshot(a.Worktree)
		if snapshotErr != nil {
			r.Err = snapshotErr
			return r
		}
		prompt := fmt.Sprintf("Recover the final task report for %s. Read the retained implementation and logs in %s. Do not change any files or repeat implementation. Return the task summary JSON contract from the original prompt. Report blocked if completion cannot be established.", a.Task.ID, dir)
		raw, err = e.Agent.Run(ctx, a.Agent, a.Worktree, filepath.Join(dir, "report-recovery"), prompt, true)
		after, snapshotErr := e.Repo.Snapshot(a.Worktree)
		if snapshotErr != nil {
			r.Err = snapshotErr
			return r
		}
		if after != before {
			r.Err = fmt.Errorf("report recovery changed code; task result is retained for inspection")
			return r
		}
	}
	if err != nil {
		r.Err = err
		return r
	}
	if err = json.Unmarshal(raw, &summary); err == nil {
		err = summary.Validate(a.Task.ID)
	}
	if err != nil {
		r.Err = err
		return r
	}
	if err = writeJSON(filepath.Join(a.Directory, "summary.json"), summary); err != nil {
		r.Err = err
		return r
	}
	r.Summary = &summary
	if summary.Status == "blocked" {
		return r
	}
	tree, err := e.Repo.Snapshot(a.Worktree)
	if err != nil {
		r.Err = err
		return r
	}
	commit, err := e.Repo.Commit(tree, []string{a.Base}, "Complete "+a.Task.ID)
	if err != nil {
		r.Err = err
		return r
	}
	if err = e.Repo.Ref(a.Ref, commit); err != nil {
		r.Err = err
		return r
	}
	r.Summary.Files, err = e.Repo.Changes(a.Base, commit)
	if err != nil {
		r.Err = err
		return r
	}
	if err = writeJSON(filepath.Join(a.Directory, "summary.json"), r.Summary); err != nil {
		r.Err = err
		return r
	}
	r.Commit = commit
	return r
}

func (e *Engine) compatible(t Task) bool {
	scope := e.State.Plan.Scope(t.ID)
	for _, a := range e.State.Attempts {
		switch a.Phase {
		case "done", "blocked", "abandoned":
			continue
		}
		if a.Task.ID == t.ID {
			continue
		}
		if overlaps(scope, e.State.Plan.Scope(a.Task.ID)) {
			return false
		}
	}
	return true
}
func (e *Engine) attempt(id string) *Attempt {
	for _, a := range e.State.Attempts {
		if a.ID == id {
			return a
		}
	}
	return nil
}
func (e *Engine) start(ctx context.Context, t Task, out chan<- workerResult) (*Attempt, error) {
	var a *Attempt
	for _, v := range e.State.Attempts {
		if v.Task.ID == t.ID && v.Phase == "resume" {
			a = v
			break
		}
	}
	if a == nil {
		for i := len(e.State.Attempts) - 1; i >= 0; i-- {
			v := e.State.Attempts[i]
			if v.Task.ID != t.ID || v.Phase != "blocked" || v.Task.Contract() != t.Contract() {
				continue
			}
			if _, err := os.Stat(v.Worktree); err != nil {
				continue
			}
			a = v
			if a.Base != e.State.Tip {
				tree, err := e.Repo.Snapshot(a.Worktree)
				if err != nil {
					return nil, err
				}
				partial, err := e.Repo.Commit(tree, []string{a.Base}, "Retain partial "+t.ID)
				if err != nil {
					return nil, err
				}
				if err = e.Repo.Ref(a.Ref, partial); err != nil {
					return nil, err
				}
				work, err := e.Repo.Workspace(identifier("resume"), e.State.Tip, e.State.Backlog)
				if err != nil {
					return nil, err
				}
				_ = e.Repo.Merge(work, partial)
				a.Worktree = work
				a.Worktrees = append(a.Worktrees, work)
				a.Base = e.State.Tip
			}
			a.Task = t
			a.Summary = nil
			a.ResultCommit = ""
			a.Verification = nil
			a.Feedback = "Continue the retained partial implementation. The task was explicitly reopened."
			a.Repairs = 0
			a.Repeats = 0
			a.Diagnosed = false
			if err := os.Remove(filepath.Join(a.Directory, "receipt.json")); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			break
		}
	}
	if a == nil {
		id := identifier("task")
		dir := filepath.Join(e.Dir, id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		work, err := e.Repo.Workspace(id, e.State.Tip, e.State.Backlog)
		if err != nil {
			return nil, err
		}
		a = &Attempt{ID: id, Task: t, Agent: e.Config.Agent, Base: e.State.Tip, Worktree: work, Directory: dir, Ref: "refs/looper-tasks/" + id, Phase: "running", Worktrees: []string{work}}
		e.State.Attempts = append(e.State.Attempts, a)
	}
	a.Phase = "running"
	e.State.Backlog.Task(t.ID).Status = "doing"
	if err := e.export(); err != nil {
		return nil, err
	}
	e.launch(ctx, a, out)
	return a, nil
}
func (e *Engine) launch(ctx context.Context, a *Attempt, out chan<- workerResult) {
	e.event("start", a.Task.ID+"  "+a.Task.Title)
	snapshot, backlog, plan := clone(*a), clone(e.State.Backlog), clone(*e.State.Plan)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		workerCtx := ctx
		cancel := func() {}
		if e.Config.Timeout > 0 {
			workerCtx, cancel = context.WithTimeout(ctx, e.Config.Timeout)
		}
		defer cancel()
		result := e.execute(workerCtx, snapshot, backlog, plan)
		select {
		case out <- result:
		case <-ctx.Done():
		}
	}()
}

func (e *Engine) hold(a *Attempt, reason string) error {
	a.Phase = "blocked"
	a.Feedback = reason
	a.Summary = &Summary{TaskID: a.Task.ID, Status: "blocked", Summary: "Work is retained for recovery.", Files: []string{}, Blockers: []string{reason}, UnblockWhen: "Resolve the recorded condition and set this task to todo."}
	return e.applyResult(a)
}
func (e *Engine) applyResult(a *Attempt) error {
	if err := e.contract(a); err != nil {
		return err
	}
	if a.Summary == nil {
		return fmt.Errorf("missing summary for %s", a.Task.ID)
	}
	t := e.State.Backlog.Task(a.Task.ID)
	s := a.Summary
	t.Status = s.Status
	t.Files = s.Files
	t.Blockers = s.Blockers
	t.UnblockWhen = s.UnblockWhen
	t.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if s.Status == "done" {
		a.Phase = "done"
		e.State.Integrated++
		e.State.Finished = false
		e.successes++
		if e.successes >= 3 {
			e.capacity = min(e.Config.Jobs, e.capacity+1)
			e.successes = 0
		}
	} else {
		a.Phase = "blocked"
	}
	for _, f := range s.Followups {
		e.addFinding(f)
	}
	if err := e.export(); err != nil {
		return err
	}
	e.event("result", fmt.Sprintf("%s  %s | %s", t.ID, s.Status, s.Summary))
	for _, b := range s.Blockers {
		e.event("blocker", "  "+b)
	}
	if e.Config.Hook != "" {
		path := filepath.Join(a.Directory, "summary.json")
		if err := writeJSON(path, s); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := runProcess(ctx, e.Config.Root, filepath.Join(a.Directory, "hook.log"), []string{e.Config.Hook, t.ID, s.Status, path, a.ID}, nil, nil)
		cancel()
		if err != nil {
			e.event("hook-error", "Result hook failed: "+err.Error())
		}
	}
	if s.Status == "done" {
		for _, w := range a.Worktrees {
			e.Repo.Remove(w)
		}
	}
	return nil
}
func (e *Engine) addFinding(f Finding) {
	if strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.Details) == "" || strings.TrimSpace(f.Evidence) == "" {
		e.event("finding", "Ignored a follow-up without a title, acceptance details, and evidence.")
		return
	}
	key := strings.ToLower(strings.Join(strings.Fields(f.Title), " "))
	for _, t := range e.State.Backlog.Tasks {
		if strings.ToLower(strings.Join(strings.Fields(t.Title), " ")) == key {
			return
		}
	}
	for _, d := range f.DependsOn {
		if e.State.Backlog.Task(d) == nil {
			e.event("finding", "Ignored follow-up with unknown dependency "+d)
			return
		}
	}
	priority := f.Priority
	if priority < 1 || priority > 5 {
		priority = 2
	}
	n := len(e.State.Backlog.Tasks) + 1
	id := fmt.Sprintf("T%03d", n)
	for e.State.Backlog.Task(id) != nil {
		n++
		id = fmt.Sprintf("T%03d", n)
	}
	e.State.Backlog.Tasks = append(e.State.Backlog.Tasks, Task{ID: id, Title: f.Title, Priority: priority, Status: "todo", Details: f.Details + "\nEvidence: " + f.Evidence, DependsOn: f.DependsOn})
	e.State.PlanSignature = ""
	e.State.Finished = false
	e.event("finding", id+"  Added follow-up: "+f.Title)
}
