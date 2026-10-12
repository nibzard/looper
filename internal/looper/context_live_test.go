package looper

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type contextEvalResult struct {
	Case, Arm, Agent, Artifact string
	Repeat, PromptBytes        int
	Seconds                    float64
	Passed                     bool
	Error                      string `json:",omitempty"`
	Usage                      UsageReport
}

// Opt-in: real agents consume usage. Both arms use the current transport so
// this comparison isolates context selection rather than argument limits.
func TestContextLiveComparison(t *testing.T) {
	kind := os.Getenv("LOOPER_CONTEXT_EVAL_AGENT")
	if kind == "" {
		t.Skip("set LOOPER_CONTEXT_EVAL_AGENT=codex or claude to run paid agent comparisons")
	}
	if kind != "codex" && kind != "claude" {
		t.Fatal("unsupported comparison agent")
	}
	root := os.Getenv("LOOPER_CONTEXT_EVAL_DIR")
	if !filepath.IsAbs(root) {
		t.Fatal("LOOPER_CONTEXT_EVAL_DIR must be an absolute artifact path")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	repeats := 2
	if value := os.Getenv("LOOPER_CONTEXT_EVAL_REPEATS"); value != "" {
		var err error
		repeats, err = strconv.Atoi(value)
		if err != nil || repeats < 1 {
			t.Fatal("invalid repeat count")
		}
	}
	config := Config{CodexBin: env("CODEX_BIN", "codex"), ClaudeBin: env("CLAUDE_BIN", "claude"), Model: env("CODEX_MODEL", ""), ClaudeModel: env("CLAUDE_MODEL", ""), Effort: env("CODEX_REASONING_EFFORT", ""), RetryLimit: 1}
	var results []contextEvalResult
	for _, name := range []string{"plan", "repair", "review"} {
		for repeat := 1; repeat <= repeats; repeat++ {
			// Alternate order to reduce a consistent cache or ordering advantage.
			arms := []string{"inline", "files"}
			if repeat%2 == 0 {
				arms = []string{"files", "inline"}
			}
			for _, arm := range arms {
				result := runContextEval(t, root, name, arm, kind, repeat, config)
				results = append(results, result)
				if err := writeJSON(filepath.Join(root, "results.json"), results); err != nil {
					t.Fatal(err)
				}
				t.Logf("%s %s repeat %d: pass=%v prompt=%d bytes time=%.1fs error=%s", name, arm, repeat, result.Passed, result.PromptBytes, result.Seconds, result.Error)
			}
		}
	}
	for _, result := range results {
		if result.Arm == "files" && !result.Passed {
			t.Errorf("file context failed: %s repeat %d: %s", result.Case, result.Repeat, result.Error)
		}
	}
}

func runContextEval(t *testing.T, root, name, arm, kind string, repeat int, config Config) contextEvalResult {
	t.Helper()
	base := filepath.Join(root, fmt.Sprintf("%s-%s-%d", name, arm, repeat))
	project, dir := filepath.Join(base, "project"), filepath.Join(base, "attempt")
	result := contextEvalResult{Case: name, Arm: arm, Agent: kind, Repeat: repeat, Artifact: dir}
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(project, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		".gitignore": "__pycache__/\n",
		"app.py":     "def format_value(value):\n    return str(value).strip()\n",
		"check.py":   "from app import format_value\nassert format_value(None) == '(empty)'\nassert format_value('  value  ') == 'value'\n",
		"PROJECT.md": "Provide a text formatter. Applied requirements define empty input. The executable acceptance check is python3 check.py. No dependencies are required.\n",
	} {
		if err := os.WriteFile(filepath.Join(project, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitText(project, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := gitText(project, "-c", "user.name=Context eval", "-c", "user.email=eval@localhost", "commit", "-qm", "Fixture"); err != nil {
		t.Fatal(err)
	}
	commit, err := gitText(project, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	r := &Repository{Root: project, Dir: filepath.Join(base, "runtime"), Excluded: []string{"to-do.json", "to-do.schema.json"}}
	b := Backlog{Version: 1, Goal: "Provide a text formatter that meets every applied requirement.", Sources: []string{"PROJECT.md"}, Tasks: []Task{{ID: "T1", Title: "Implement text formatting", Priority: 1, Status: "todo", Details: "Meet the applied requirements. Preserve text trimming and pass python3 check.py."}}}
	for i := 0; i < 500; i++ {
		b.Tasks = append(b.Tasks, Task{ID: fmt.Sprintf("H%03d", i), Title: "Historical completed work", Priority: 3, Status: "done", Details: strings.Repeat("Historical implementation evidence. ", 20)})
	}
	requirement := strings.Repeat("Background note: formatting preserves supplied text.\n", 300) + "\nRequired acceptance: None must format as the exact text '(empty)'.\n" + strings.Repeat("Background note: formatting preserves supplied text.\n", 300)
	documents := []contextDocument{{Import: "scope-with-no-new-task", Source: "empty-input.md", Text: []byte(requirement)}}
	feedback := "Command: python3 check.py\nObserved failure: assert format_value(None) == '(empty)'\nRead the applied requirements and repair the retained implementation.\n" + strings.Repeat("Earlier check output: whitespace formatting passes.\n", 300)
	if name == "review" {
		b.Tasks[0].Status = "done"
	}
	work, err := r.Workspace("eval", commit, b)
	if err != nil {
		t.Fatal(err)
	}
	// Keep worktrees and all model evidence for inspection after the test.
	c, err := prepareAgentContext(r, work, dir, b, &b.Tasks[0], feedback, documents, nil)
	if err != nil {
		t.Fatal(err)
	}
	var prompt string
	if name == "plan" {
		prompt = planPrompt(b, c)
		if arm == "inline" {
			data, _ := json.Marshal(b)
			prompt = planPrompt(b, agentContext{}) + "\nAccepted backlog:\n" + string(data) + "\nApplied requirements:\n" + requirement
			prompt = strings.ReplaceAll(prompt, (agentContext{}).references(), "")
		}
	} else if name == "repair" {
		prompt = workerPrompt(b, b.Tasks[0], c)
		if arm == "inline" {
			prompt = workerPrompt(b, b.Tasks[0], agentContext{}) + "\nApplied requirements:\n" + requirement + "\nRepair feedback:\n" + feedback
			prompt = strings.ReplaceAll(prompt, (agentContext{}).references(), "")
		}
	} else {
		prompt = `You are running a final review pass for Looper. Read the product and exercise python3 check.py. Do not modify files. Check every applied requirement, including imports that added no tasks. Return JSON {"status":"reviewed","summary":"evidence","findings":[{"title":"observed defect","details":"acceptance criteria","evidence":"observed facts","depends_on":[],"priority":1}],"complete":false}. Set complete true only when all requirements hold.` + c.references()
		if arm == "inline" {
			data, _ := json.Marshal(b)
			prompt = `You are running a final review pass for Looper. Read the product and exercise python3 check.py. Do not modify files. Return JSON {"status":"reviewed","summary":"evidence","findings":[{"title":"observed defect","details":"acceptance criteria","evidence":"observed facts","depends_on":[],"priority":1}],"complete":false}. Set complete true only when all requirements hold. Accepted backlog: ` + string(data) + "\nApplied requirements:\n" + requirement
		}
	}
	result.PromptBytes = len(prompt)
	before, err := r.Snapshot(work)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	start := time.Now()
	var progress func(string, string)
	if name == "plan" {
		progress = func(string, string) {}
	}
	raw, runErr := (Agent{config}).run(ctx, kind, work, dir, prompt, name == "repair", progress)
	result.Seconds = time.Since(start).Seconds()
	if runErr == nil && arm == "files" {
		runErr = c.validate()
	}
	if runErr == nil && name != "repair" {
		after, err := r.Snapshot(work)
		if err != nil {
			runErr = err
		} else if before != after {
			runErr = fmt.Errorf("inspection changed project files")
		}
	}
	if runErr == nil {
		switch name {
		case "plan":
			var p Plan
			if err := json.Unmarshal(raw, &p); err != nil {
				runErr = err
			} else if err = p.Validate(b, ""); err != nil {
				runErr = err
			} else {
				found := false
				for _, scope := range p.Tasks {
					if scope.TaskID == "T1" {
						found = true
					}
				}
				if !found || !strings.Contains(p.Verify, "check.py") {
					runErr = fmt.Errorf("plan lacks task scope or independent acceptance check")
				}
			}
		case "repair":
			var summary Summary
			if err := json.Unmarshal(raw, &summary); err != nil {
				runErr = err
			} else if err = summary.Validate("T1"); err != nil {
				runErr = err
			} else if summary.Status != "done" {
				runErr = fmt.Errorf("worker did not report done")
			} else {
				runErr = runShell(ctx, work, filepath.Join(dir, "grade"), "verify", `PYTHONDONTWRITEBYTECODE=1 python3 -c 'from app import format_value; assert format_value(None) == "(empty)"; assert format_value("  value  ") == "value"'`, nil)
			}
		case "review":
			var review reviewResult
			if err := json.Unmarshal(raw, &review); err != nil {
				runErr = err
			} else if review.Status != "reviewed" || review.Complete || len(review.Findings) == 0 {
				runErr = fmt.Errorf("review missed the required empty-input behavior")
			}
		}
	}
	result.Passed = runErr == nil
	if runErr != nil {
		result.Error = runErr.Error()
	}
	_ = readJSON(filepath.Join(dir, "events-00.jsonl.usage.json"), &result.Usage)
	if result.Usage.Version == 0 {
		events, _ := os.ReadFile(filepath.Join(dir, "events-00.jsonl"))
		result.Usage = parseUsage(kind, map[string]string{"codex": config.Model, "claude": config.ClaudeModel}[kind], events, time.Now())
	}
	return result
}
