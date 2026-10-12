package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Agent struct{ Config Config }
type AgentError struct {
	Kind   string
	Detail string
}

func (e *AgentError) Error() string { return e.Kind + ": " + e.Detail }

const summarySchema = `{"type":"object","additionalProperties":false,"required":["task_id","status","summary","files","blockers","followups","unblock_when"],"properties":{"task_id":{"type":"string"},"status":{"type":"string","enum":["done","blocked"]},"summary":{"type":"string"},"files":{"type":"array","items":{"type":"string"}},"blockers":{"type":"array","items":{"type":"string"}},"unblock_when":{"type":"string"},"followups":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["title","details","evidence","depends_on","priority"],"properties":{"title":{"type":"string"},"details":{"type":"string"},"evidence":{"type":"string"},"depends_on":{"type":"array","items":{"type":"string"}},"priority":{"type":"integer"}}}}}}`

const planSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": [
    "setup",
    "verify",
    "tasks"
  ],
  "properties": {
    "setup": {
      "type": "string"
    },
    "verify": {
      "type": "string"
    },
    "tasks": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": [
          "task_id",
          "write_paths",
          "resources"
        ],
        "properties": {
          "task_id": {
            "type": "string"
          },
          "write_paths": {
            "type": "array",
            "items": {
              "type": "string"
            }
          },
          "resources": {
            "type": "array",
            "items": {
              "type": "string"
            }
          }
        }
      }
    }
  }
}`

func (a Agent) Run(ctx context.Context, kind, cwd, dir, prompt string, summary bool) (json.RawMessage, error) {
	return a.run(ctx, kind, cwd, dir, prompt, summary, nil)
}

// run reports process starts and retry delays on the caller's goroutine.
func (a Agent) run(ctx context.Context, kind, cwd, dir, prompt string, summary bool, progress func(string, string)) (json.RawMessage, error) {
	ctx, finish, err := beginResourceOperation(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer finish()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := atomicWrite(filepath.Join(dir, "prompt.txt"), []byte(prompt), 0600); err != nil {
		return nil, err
	}
	resultPath := filepath.Join(dir, "result.json")
	schema := filepath.Join(dir, "schema.json")
	outputSchema := ""
	if summary {
		outputSchema = summarySchema
	} else if progress != nil {
		outputSchema = planSchema
	}
	if outputSchema != "" {
		if err := atomicWrite(schema, []byte(outputSchema), 0600); err != nil {
			return nil, err
		}
	}
	var args []string
	if kind == "codex" {
		args = []string{a.Config.CodexBin, "exec", "--cd", cwd, "--json", "--output-last-message", resultPath}
		if a.Config.Model != "" {
			args = append(args, "--model", a.Config.Model)
		}
		if a.Config.Effort != "" {
			args = append(args, "-c", "model_reasoning_effort="+fmt.Sprintf("%q", a.Config.Effort))
		}
		if progress != nil {
			args = append(args, "--sandbox", "read-only")
		} else if env("CODEX_YOLO", "1") != "0" {
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		} else {
			args = append(args, "--sandbox", "workspace-write")
		}
		if outputSchema != "" {
			args = append(args, "--output-schema", schema)
		}
		args = append(args, "-")
	} else if kind == "zcode" {
		// zcode ships no schema flag; the prompt states the output shape and
		// the response text is parsed below. Headless already defaults to
		// full access, so the mode is explicit for the invocation record.
		// stream-json, not json: the json summary prints pretty and spans
		// lines, while the stream result terminator is one line. Planning
		// stays in yolo with the inspection-only prompt as its restriction:
		// zcode's plan mode answers with a prose summary and drops the JSON
		// (verified against a live binary).
		args = []string{a.Config.ZcodeBin, "--cwd", cwd, "--output-format", "stream-json", "--mode", "yolo", "-p", prompt}
	} else {
		args = []string{a.Config.ClaudeBin, "-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions", "--add-dir", cwd}
		if progress != nil {
			// Planning can inspect files but cannot invoke a shell or delegate work.
			args = []string{a.Config.ClaudeBin, "-p", "--output-format", "stream-json", "--verbose",
				"--dangerously-skip-permissions", "--add-dir", cwd, "--tools", "Read,Glob,Grep",
				"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--disable-slash-commands"}
		}
		if a.Config.ClaudeModel != "" {
			args = append(args, "--model", a.Config.ClaudeModel)
		}
		if outputSchema != "" {
			args = append(args, "--json-schema", outputSchema)
		}
	}
	transport := "stdin"
	if kind == "zcode" {
		transport = "argv"
	}
	role, target := promptBudget(prompt, summary, progress != nil)
	var files []contextFile
	_ = readJSON(filepath.Join(dir, "context.json"), &files)
	contextBytes := 0
	for _, file := range files {
		contextBytes += file.Bytes
	}
	if err := writeJSON(filepath.Join(dir, "invocation.json"), map[string]any{"agent": kind, "model": map[string]string{"codex": a.Config.Model, "claude": a.Config.ClaudeModel}[kind], "effort": a.Config.Effort, "cwd": cwd, "prompt_bytes": len(prompt), "prompt_transport": transport, "context_role": role, "context_bytes": contextBytes, "prompt_target_bytes": target, "prompt_target_exceeded": len(prompt) > target}); err != nil {
		return nil, err
	}
	for retry := 0; retry < a.Config.RetryLimit; retry++ {
		os.Remove(resultPath)
		log := filepath.Join(dir, fmt.Sprintf("events-%02d.jsonl", retry))
		if progress != nil {
			progress(fmt.Sprintf("%s attempt %d/%d", kind, retry+1, a.Config.RetryLimit), log)
		}
		input := strings.NewReader(prompt)
		err := runProcess(ctx, cwd, log, args, input, resourceEnv(dir))
		var launchErr *LaunchError
		if errors.As(err, &launchErr) {
			return nil, &AgentError{"invalid_request", fmt.Sprintf("%v; see %s and %s", err, filepath.Join(dir, "prompt.txt"), filepath.Join(dir, "invocation.json"))}
		}
		raw, readErr := readBounded(log, 64<<20)
		if readErr == nil {
			recordAgentUsage(ctx, log, kind, map[string]string{"codex": a.Config.Model, "claude": a.Config.ClaudeModel}[kind], raw)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if readErr != nil {
			return nil, readErr
		}
		var wrapper map[string]json.RawMessage
		if kind == "claude" || kind == "zcode" {
			// Only the zcode result terminator carries the final response.
			// Claude can return either text or structured output.
			for _, line := range bytes.Split(raw, []byte{'\n'}) {
				var o map[string]json.RawMessage
				if json.Unmarshal(line, &o) != nil {
					continue
				}
				if kind == "zcode" {
					if string(o["type"]) == `"result"` {
						wrapper = o
					}
				} else if o["result"] != nil || o["structured_output"] != nil {
					wrapper = o
				}
			}
			if string(wrapper["is_error"]) == "true" && err == nil {
				err = errors.New("agent returned an execution error")
			}
		}
		if err != nil {
			failure := classifyFailure(err.Error() + "\n" + string(tail(raw, 8<<10)))
			if failure == "transient" && retry+1 < a.Config.RetryLimit {
				delay := min(10*time.Minute, a.Config.RetryDelay*time.Duration(1<<min(retry, 8)))
				if progress != nil {
					progress(fmt.Sprintf("%s retry in %s after a transient failure", kind, delay), log)
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(delay):
					continue
				}
			}
			return nil, &AgentError{failure, fmt.Sprintf("%v; see %s", err, log)}
		}
		if kind == "codex" {
			raw, err = readBounded(resultPath, 8<<20)
			if err != nil {
				return nil, &AgentError{"invalid_result", "missing final result; see " + dir}
			}
		} else if kind == "zcode" {
			var s string
			if json.Unmarshal(wrapper["response"], &s) != nil || strings.TrimSpace(s) == "" {
				return nil, &AgentError{"invalid_result", "missing response in summary; see " + dir}
			}
			raw = []byte(s)
		} else if v := wrapper["structured_output"]; len(v) > 0 && string(v) != "null" {
			raw = v
		} else {
			var s string
			if json.Unmarshal(wrapper["result"], &s) == nil {
				raw = []byte(s)
			}
		}
		data, err := extractObject(raw)
		if err != nil {
			return nil, &AgentError{"invalid_result", fmt.Sprintf("%v; see %s", err, dir)}
		}
		if err = atomicWrite(resultPath, append(data, '\n'), 0600); err != nil {
			return nil, err
		}
		return data, nil
	}
	return nil, &AgentError{"transient", "retry limit reached"}
}

// These byte targets are observations, not model context limits. Exceeding
// one does not reject work; actual usage is recorded from agent events.
func promptBudget(prompt string, summary, planning bool) (string, int) {
	if planning {
		return "planner", 4 << 10
	}
	if summary {
		return "worker", 16 << 10
	}
	if strings.HasPrefix(prompt, "You are running a ") {
		return "review", 16 << 10
	}
	if strings.HasPrefix(prompt, "Analyze additional requirements") {
		return "import", 16 << 10
	}
	if strings.HasPrefix(prompt, "Bootstrap a development backlog") {
		return "bootstrap", 16 << 10
	}
	return "consultation", 16 << 10
}
func readBounded(path string, limit int64) ([]byte, error) {
	s, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if s.Size() > limit {
		return nil, fmt.Errorf("artifact exceeds %d bytes: %s", limit, path)
	}
	return os.ReadFile(path)
}
func extractObject(b []byte) (json.RawMessage, error) {
	b = bytes.TrimSpace(b)
	var v map[string]json.RawMessage
	if json.Unmarshal(b, &v) == nil && v != nil {
		return b, nil
	}
	// Accept one complete JSON fence surrounded by prose. Never choose
	// between multiple blocks or salvage a fragment of malformed JSON.
	var candidate []byte
	var block bytes.Buffer
	inside, blocks, validFence := false, 0, false
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		trimmed := strings.TrimSpace(string(line))
		if strings.HasPrefix(trimmed, "```") {
			if !inside {
				blocks++
				inside = true
				validFence = trimmed == "```" || strings.EqualFold(trimmed, "```json")
				block.Reset()
			} else if trimmed == "```" {
				inside = false
				if validFence {
					candidate = bytes.TrimSpace(append([]byte(nil), block.Bytes()...))
				}
			} else {
				return nil, fmt.Errorf("final output contains malformed code fences")
			}
			continue
		}
		if inside {
			block.Write(line)
			block.WriteByte('\n')
		}
	}
	if blocks == 1 && !inside && len(candidate) > 0 && json.Unmarshal(candidate, &v) == nil && v != nil {
		return candidate, nil
	}
	return nil, fmt.Errorf("final output must contain one JSON object")
}

// Classification reads only the process error and the tail of the event log.
// The whole session log cannot be trusted here: a timed-out test the agent was
// fixing, a matching file name, or a bare number in echoed output would turn a
// deterministic failure into hours of retries.
var transientFailure = regexp.MustCompile(`(?i)\b(?:429|500|502|503|504|529)\b|rate.?limit|too many requests|bad gateway|service unavailable|gateway timeout|overloaded|connection (?:reset|refused|dropped)|network (?:error|unreachable|timeout)|temporarily unavailable|econnreset|econnrefused|etimedout|socket hang up`)
var configurationFailure = regexp.MustCompile(`(?i)\b(?:401|403)\b|unauthorized|authentication|permission denied|invalid api key`)

func classifyFailure(s string) string {
	if transientFailure.MatchString(s) {
		return "transient"
	}
	if configurationFailure.MatchString(s) {
		return "configuration"
	}
	return "execution"
}
func tail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}
func resourceEnv(dir string) map[string]string {
	id := digest([]byte(dir))[:20]
	return map[string]string{"LOOPER_ATTEMPT_ID": id, "LOOPER_RESOURCE_PREFIX": "looper_" + id, "LOOPER_ARTIFACT_DIR": dir, "LOOPER_RESOURCE_CLEANUP_FILE": filepath.Join(dir, resourceCleanupFile)}
}

func workerPrompt(b Backlog, t Task, c agentContext) string {
	data, _ := json.MarshalIndent(t, "", "  ")
	sources, _ := json.Marshal(b.Sources)
	contract := string(data)
	if len(data) > 8<<10 {
		contract = "Read the complete task contract and acceptance criteria in " + c.Task
	}
	if len(sources) > 1024 {
		sources = []byte(`(listed in the accepted backlog snapshot)`)
	}
	p := fmt.Sprintf(`Complete task %s: %s.
You work autonomously in a private Git worktree. Read repository instructions and source files %s.
Project goal: %s
Task contract:
%s
Implement the outcome and exercise its acceptance criteria. Repair problems you can resolve locally.
Setup and verification must leave tracked and unignored project files unchanged.
Include ignore rules for generated output in your implementation. Remove tracked build artifacts when necessary.
Do not edit the backlog, other worktrees, shared Git refs, or repository configuration. Do not push.
Use LOOPER_RESOURCE_PREFIX for private databases and temporary resources. Bind servers to free ports.
Register cleanup for external resources in LOOPER_RESOURCE_CLEANUP_FILE before creating them. Make cleanup idempotent and check ownership before removal. Looper runs cleanup after this operation finishes; interrupted work retains resources until recovery completes.
Preserve cleanup already registered by setup. For Docker containers, set labels io.looper.resource-prefix=$LOOPER_RESOURCE_PREFIX and io.looper.artifact=$LOOPER_ARTIFACT_DIR. The registered Docker cleanup can then find each owned container. Keep shared services outside this cleanup.
Do not weaken acceptance criteria or remove failing tests to claim success.
If an external prerequisite is missing, report blocked with an explicit unblock condition.
Return JSON: task_id, status (done or blocked), summary, files (array), blockers (array),
followups (array of title, details, evidence, depends_on, priority), and unblock_when (string).
Capture concrete missing work in followups; do not invent unrelated product goals.
`, shortText(t.ID, 80), shortText(t.Title, 160), sources, shortText(b.Goal, 512), contract)
	p += c.references()
	p += "Read requirements relevant to this task. Use the complete index to find constraints; unrelated tasks are context only.\n"
	if c.Feedback != "" {
		p += "\nRepair the task using the retained code and this evidence. Inspect existing edits before changing them.\nRead repair feedback: " + c.Feedback
	}
	return p
}

func planPrompt(b Backlog, c agentContext) string {
	p := `Plan execution for Looper. Read repository instructions and the source_files in the accepted backlog snapshot.
This is an inspection-only planning step. Read files and search existing code and configuration.
Do not run builds, tests, benchmarks, dataset or corpus generators, package installation, or application commands.
Do not delegate work. Identify setup and verification commands from existing documentation, manifests, and scripts;
return those commands for later execution by workers and checks. Do not execute them to validate this plan.
Focus on unfinished tasks; completed tasks provide context only. Do not change any files.
Return JSON with setup (shell command or empty string), verify (shell command),
and tasks [{task_id, write_paths: [relative file or directory prefixes], resources: [shared interface names]}].
Include each unfinished task. Use ["."] when the write scope is uncertain. Include generated files and wiring.
Setup must recreate ignored dependencies in each worktree. Never share mutable build output between worktrees.
Setup and verification must leave tracked and unignored project files unchanged; workers must implement any needed ignore rules.
Checks must execute meaningful acceptance checks for the combined product; a no-op is invalid.
Checks run in a fresh integrated worktree. Verify product behavior without depending on Looper-private refs or worker commit history.
Commands receive LOOPER_RESOURCE_PREFIX unique to the operation and TMPDIR private to each process. Isolate databases and bind free ports.
Tasks sharing public interfaces or migration ordering need the same resource name, even if files differ.
Do not create tasks or change requirements. Use existing commands and lockfiles where available.`
	p += c.references()
	p += "\nUnfinished task index (read the snapshot for all tasks and dependencies):\n" + taskIndex(b, 1024)
	return p
}
