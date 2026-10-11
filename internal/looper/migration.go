package looper

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Import copies the legacy journal. It never removes the original state or refs.
func (e *Engine) importLegacy(b Backlog, data []byte) (*State, error) {
	path := filepath.Join(e.Logs, "parallel", "state.json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var legacy struct {
		State
		Plan json.RawMessage `json:"plan"`
	}
	if err = json.Unmarshal(raw, &legacy); err != nil {
		return nil, fmt.Errorf("cannot import legacy state: %w", err)
	}
	if legacy.Version != 1 || legacy.Root != e.Config.Root || legacy.Todo != e.Config.Todo {
		return nil, fmt.Errorf("legacy state belongs to another project or task file")
	}
	hasPartial := false
	for _, a := range legacy.Attempts {
		if a.Summary != nil && a.Summary.Status == "blocked" {
			a.Phase = "blocked"
			if _, err := os.Stat(a.Worktree); err == nil {
				hasPartial = true
			}
		}
	}
	if !hasPartial && (legacy.Finished || !retained(&legacy.State)) {
		return nil, nil
	}
	if err = reapLegacy(filepath.Dir(path)); err != nil {
		return nil, err
	}
	s := legacy.State
	s.Version = 2
	s.Finished = false
	s.Backlog = b
	s.ExportHash = digest(data)
	_, s.Branch = e.Repo.Head()
	if len(legacy.Plan) > 0 && string(legacy.Plan) != "null" {
		var p struct {
			Setup  string           `json:"setup"`
			Verify string           `json:"verify"`
			Tasks  map[string]Scope `json:"tasks"`
		}
		if err = json.Unmarshal(legacy.Plan, &p); err != nil {
			return nil, err
		}
		s.Plan = &Plan{Setup: p.Setup, Verify: p.Verify}
		for id, scope := range p.Tasks {
			scope.TaskID = id
			s.Plan.Tasks = append(s.Plan.Tasks, scope)
		}
		if err = s.Plan.Validate(b, e.Config.Verify); err != nil {
			return nil, err
		}
	}
	if err = writeJSON(filepath.Join(e.Dir, "history", "imported-python-state.json"), json.RawMessage(raw)); err != nil {
		return nil, err
	}
	if err = e.Store.Save(&s); err != nil {
		return nil, err
	}
	e.event("migration", "Imported retained Python worktrees and integration journal.")
	return &s, nil
}
func reapLegacy(dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".process.json") {
			return nil
		}
		var record struct {
			PID      int    `json:"pid"`
			Identity string `json:"identity"`
		}
		if err = readJSON(path, &record); err != nil {
			return err
		}
		if record.PID <= 1 || record.Identity == "" {
			return nil
		}
		identity := func() string {
			b, _ := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(record.PID)).Output()
			return strings.TrimSpace(string(b))
		}
		if identity() == record.Identity {
			group, e := syscall.Getpgid(record.PID)
			if e == nil && group == record.PID {
				syscall.Kill(-group, syscall.SIGTERM)
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) && identity() == record.Identity {
					time.Sleep(50 * time.Millisecond)
				}
				if identity() == record.Identity {
					syscall.Kill(-group, syscall.SIGKILL)
				}
			}
		}
		return os.Remove(path)
	})
}
