package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// testAgentProfile is an explicitly requested live check. Static configuration
// and preflight never invoke this function or spend model usage.
func testAgentProfile(c Config, out, stderr io.Writer) int {
	p, err := c.effectiveAgent(c.TestAgent)
	if err != nil {
		return emitError(c, usageFailure("agent_unsupported", err.Error()), out, stderr)
	}
	resolved, err := resolvedAgentCommand(p)
	if err != nil {
		return emitError(c, usageFailure("agent_executable_missing", err.Error()), out, stderr)
	}
	dir, err := os.MkdirTemp("", "looper-agent-test-")
	if err != nil {
		return emitError(c, err, out, stderr)
	}
	// Preserve test evidence. A caller can inspect the structured invocation and
	// private raw log without mixing test artifacts into the project state.
	fmt.Fprintf(stderr, "Testing %s using %s (%s). This uses your agent account. Evidence: %s\n", c.TestAgent, resolved, p.Adapter, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c.RetryLimit = 1
	if err := os.WriteFile(filepath.Join(dir, "probe.txt"), []byte("Looper agent interface test.\n"), 0600); err != nil {
		return emitError(c, err, out, stderr)
	}
	prompt := `Use a read-only tool to read probe.txt in the current directory. Do not change any files. Return only a JSON object with keys setup (empty string), verify (empty string), and tasks (empty array).`
	raw, err := (Agent{Config: c}).run(ctx, c.TestAgent, dir, dir, prompt, false, func(string, string) {})
	if err != nil {
		return emitError(c, err, out, stderr)
	}
	var result map[string]json.RawMessage
	if err = json.Unmarshal(raw, &result); err != nil {
		return emitError(c, err, out, stderr)
	}
	var contract struct {
		Setup  string            `json:"setup"`
		Verify string            `json:"verify"`
		Tasks  []json.RawMessage `json:"tasks"`
	}
	decodeErr := json.Unmarshal(raw, &contract)
	if decodeErr != nil || len(result) != 3 || contract.Setup != "" || contract.Verify != "" || contract.Tasks == nil || len(contract.Tasks) != 0 || string(result["setup"]) == "null" || string(result["verify"]) == "null" {
		return emitError(c, operationFailure("agent_test_failed", "The agent did not return the test contract."), out, stderr)
	}
	toolVerified := false
	if p.Adapter == "claude" || p.Adapter == "codex" {
		log, err := readAgentLog(filepath.Join(dir, "events-00.jsonl"), 64<<20)
		if err != nil {
			return emitError(c, err, out, stderr)
		}
		for _, line := range bytes.Split(log, []byte{'\n'}) {
			for _, action := range workerActivity(line) {
				if strings.HasPrefix(action, "Tool started:") || strings.HasPrefix(action, "Command completed (exit 0)") {
					toolVerified = true
				}
			}
		}
		if !toolVerified {
			return emitError(c, operationFailure("agent_test_failed", "The structured result passed, but no tool call was observed."), out, stderr)
		}
	}
	if c.JSON {
		document := map[string]any{"agent_profile": c.TestAgent, "adapter": p.Adapter, "executable": resolved, "result": "passed", "evidence": dir, "tool_call_verified": toolVerified}
		if c.Contract == ContractVersionV2 {
			document["contract_version"] = ContractVersionV2
			document["schema_version"] = 1
			document["kind"] = "looper.agent-test"
		}
		json.NewEncoder(out).Encode(document)
	} else {
		fmt.Fprintln(out, "Agent structured-output test passed:", c.TestAgent)
		if toolVerified {
			fmt.Fprintln(out, "A tool call was observed.")
		} else {
			fmt.Fprintln(out, "Tool-call verification is unavailable for this adapter.")
		}
	}
	return 0
}
