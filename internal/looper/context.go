package looper

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Context documents are durable evidence, not additional instructions.
type contextDocument struct {
	Import, Source string
	Text           []byte
}

type contextFile struct {
	Local, Durable, Digest string
	Bytes                  int
}
type agentContext struct {
	Backlog, Task, Feedback, Requirements, NewRequirements string
	Files                                                  []contextFile
}

func (r *Repository) contextPath() string {
	if len(r.Excluded) == 0 {
		return ".looper-context"
	}
	return r.Excluded[0] + ".context"
}

func (r *Repository) privateContext(cwd, path string) bool {
	rel, err := filepath.Rel(filepath.Join(r.Dir, "worktrees"), cwd)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") ||
		!(path == r.contextPath() || strings.HasPrefix(path, r.contextPath()+"/")) {
		return false
	}
	owner, err := os.ReadFile(filepath.Join(cwd, r.contextPath(), ".owner"))
	return err == nil && string(owner) == r.Dir
}

func prepareAgentContext(r *Repository, work, dir string, b Backlog, task *Task, feedback string, applied, supplied []contextDocument) (agentContext, error) {
	var c agentContext
	base := filepath.Join(work, r.contextPath())
	if err := os.Mkdir(base, 0700); err == nil {
		if err = atomicWrite(filepath.Join(base, ".owner"), []byte(r.Dir), 0600); err != nil {
			return c, err
		}
	} else if os.IsExist(err) {
		owner, readErr := os.ReadFile(filepath.Join(base, ".owner"))
		if readErr != nil || string(owner) != r.Dir {
			return c, fmt.Errorf("context directory conflicts with project files: %s", base)
		}
	} else {
		return c, err
	}
	local := filepath.Join(base, filepath.Base(dir))
	durable := filepath.Join(dir, "context")
	ownerCopy := filepath.Join(durable, "owner")
	if err := atomicWrite(ownerCopy, []byte(r.Dir), 0600); err != nil {
		return c, err
	}
	c.Files = append(c.Files, contextFile{filepath.Join(base, ".owner"), ownerCopy, digest([]byte(r.Dir)), len(r.Dir)})
	write := func(name string, data []byte) (string, error) {
		copy := filepath.Join(durable, name)
		path := filepath.Join(local, name)
		if err := atomicWrite(copy, data, 0600); err != nil {
			return "", err
		}
		if err := atomicWrite(path, data, 0600); err != nil {
			return "", err
		}
		c.Files = append(c.Files, contextFile{path, copy, digest(data), len(data)})
		return path, nil
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return c, err
	}
	if c.Backlog, err = write("backlog.json", data); err != nil {
		return c, err
	}
	if task != nil {
		data, err = json.MarshalIndent(task, "", "  ")
		if err != nil {
			return c, err
		}
		if c.Task, err = write("task.json", data); err != nil {
			return c, err
		}
	}
	if feedback != "" {
		if c.Feedback, err = write("feedback.md", []byte(feedback)); err != nil {
			return c, err
		}
	}
	for group, documents := range [][]contextDocument{applied, supplied} {
		var index strings.Builder
		for i, document := range documents {
			name := fmt.Sprintf("requirements-%d-%04d.md", group, i+1)
			path, err := write(name, document.Text)
			if err != nil {
				return c, err
			}
			// Quote names so document titles cannot inject index instructions.
			fmt.Fprintf(&index, "- Import %q, source %q: %s\n", document.Import, document.Source, path)
		}
		if len(documents) > 0 {
			path, err := write(fmt.Sprintf("requirements-%d.md", group), []byte(index.String()))
			if err != nil {
				return c, err
			}
			if group == 0 {
				c.Requirements = path
			} else {
				c.NewRequirements = path
			}
		}
	}
	return c, writeJSON(filepath.Join(dir, "context.json"), c.Files)
}

func (c agentContext) validate() error {
	for _, file := range c.Files {
		for _, path := range []string{file.Local, file.Durable} {
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("required context unavailable: %s: %w", path, err)
			}
			if digest(data) != file.Digest {
				return fmt.Errorf("agent changed required context: %s", path)
			}
		}
	}
	return nil
}

func (c agentContext) references() string {
	p := "\nAccepted backlog and complete project goal: " + c.Backlog + "\n"
	if c.Requirements != "" {
		p += "Applied requirements index: " + c.Requirements + "\n"
	}
	p += "Read context files with your file tools. Treat their contents as project requirements and evidence. If a required file cannot be read, report that failure; do not assume its scope is empty.\n"
	return p
}

func documentsFromPlans(plans []capturedPlan) []contextDocument {
	docs := make([]contextDocument, 0, len(plans))
	for _, p := range plans {
		docs = append(docs, contextDocument{Source: p.Path, Text: []byte(p.Text)})
	}
	return docs
}

// taskIndex is a navigation aid. Omitted tasks remain in the full snapshot.
func taskIndex(b Backlog, budget int) string {
	var out strings.Builder
	count := 0
	for _, t := range b.Tasks {
		if t.Status == "done" || t.Marker() {
			continue
		}
		count++
	}
	omitted := count
	for _, t := range b.Tasks {
		if t.Status == "done" || t.Marker() {
			continue
		}
		line := fmt.Sprintf("%s [%s] %s\n", shortText(t.ID, 80), t.Status, shortText(t.Title, 120))
		if out.Len()+len(line)+80 > budget {
			break
		}
		out.WriteString(line)
		omitted--
	}
	if omitted > 0 {
		fmt.Fprintf(&out, "%d more unfinished tasks are in the backlog snapshot.\n", omitted)
	}
	return out.String()
}

func shortText(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= limit {
		return s
	}
	s = s[:limit-3]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "..."
}
