package looper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// agentProtocol is the extension point for additional agents, such as Pi.
// Profiles and project roles never construct protocol-specific arguments or
// interpret a provider's result stream.
type agentProtocol interface {
	arguments(agentInvocation) []string
	envelope([]byte) map[string]json.RawMessage
	result(agentInvocation, map[string]json.RawMessage) ([]byte, error)
	transport() string
	permissions() (string, string)
}

type agentInvocation struct {
	profile                                                 AgentProfile
	executable, cwd, resultPath, schemaPath, schema, prompt string
	planning                                                bool
}

type claudeProtocol struct{}
type codexProtocol struct{}
type zcodeProtocol struct{}

func (claudeProtocol) transport() string { return "stdin" }
func (codexProtocol) transport() string  { return "stdin" }
func (zcodeProtocol) transport() string  { return "argv" }

func (claudeProtocol) arguments(v agentInvocation) []string {
	args := []string{v.executable, "-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions", "--add-dir", v.cwd}
	if v.planning {
		args = append(args, "--tools", "Read,Glob,Grep", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--disable-slash-commands")
	}
	if v.profile.Model != "" {
		args = append(args, "--model", v.profile.Model)
	}
	if v.profile.Effort != "" {
		args = append(args, "--effort", v.profile.Effort)
	}
	if v.schema != "" {
		args = append(args, "--json-schema", v.schema)
	}
	return args
}

func (codexProtocol) arguments(v agentInvocation) []string {
	args := []string{v.executable, "exec", "--cd", v.cwd, "--json", "--output-last-message", v.resultPath}
	if v.profile.Model != "" {
		args = append(args, "--model", v.profile.Model)
	}
	if v.profile.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+fmt.Sprintf("%q", v.profile.Effort))
	}
	if v.planning {
		args = append(args, "--sandbox", "read-only")
	} else if env("CODEX_YOLO", "1") != "0" {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	} else {
		args = append(args, "--sandbox", "workspace-write")
	}
	if v.schema != "" {
		args = append(args, "--output-schema", v.schemaPath)
	}
	return append(args, "-")
}

func (zcodeProtocol) arguments(v agentInvocation) []string {
	// Zcode's planning mode produces prose. Its JSON mode therefore uses full
	// access with an inspection-only prompt, as the capabilities declare.
	return []string{v.executable, "--cwd", v.cwd, "--output-format", "stream-json", "--mode", "yolo", "-p", v.prompt}
}

func streamEnvelope(raw []byte, accept func(map[string]json.RawMessage) bool) map[string]json.RawMessage {
	var result map[string]json.RawMessage
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		var event map[string]json.RawMessage
		if json.Unmarshal(line, &event) == nil && accept(event) {
			result = event
		}
	}
	return result
}

func (claudeProtocol) envelope(raw []byte) map[string]json.RawMessage {
	return streamEnvelope(raw, func(event map[string]json.RawMessage) bool {
		return event["result"] != nil || event["structured_output"] != nil
	})
}
func (zcodeProtocol) envelope(raw []byte) map[string]json.RawMessage {
	return streamEnvelope(raw, func(event map[string]json.RawMessage) bool { return string(event["type"]) == `"result"` })
}
func (codexProtocol) envelope([]byte) map[string]json.RawMessage { return nil }

func (claudeProtocol) result(_ agentInvocation, event map[string]json.RawMessage) ([]byte, error) {
	if value := event["structured_output"]; len(value) > 0 && string(value) != "null" {
		return value, nil
	}
	// A log truncation marker must never become a valid result.
	if event["result"] == nil {
		return nil, fmt.Errorf("missing result envelope")
	}
	var text string
	if json.Unmarshal(event["result"], &text) != nil || strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("malformed result envelope")
	}
	return []byte(text), nil
}
func (zcodeProtocol) result(_ agentInvocation, event map[string]json.RawMessage) ([]byte, error) {
	var text string
	if json.Unmarshal(event["response"], &text) != nil || strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("missing response in summary")
	}
	return []byte(text), nil
}
func (codexProtocol) result(v agentInvocation, _ map[string]json.RawMessage) ([]byte, error) {
	data, err := readBounded(v.resultPath, 8<<20)
	if err != nil {
		return nil, fmt.Errorf("missing final result")
	}
	return data, nil
}

func (claudeProtocol) permissions() (string, string) {
	return "permissions-skipped", "planning uses Read, Glob, and Grep only; review and diagnosis use worker permissions"
}
func (codexProtocol) permissions() (string, string) {
	if env("CODEX_YOLO", "1") == "0" {
		return "workspace-write", "CODEX_YOLO=0 keeps the workspace-write sandbox; planning uses read-only mode"
	}
	return "approvals-and-sandbox-bypassed", "planning uses a read-only sandbox; review and diagnosis use worker permissions"
}
func (zcodeProtocol) permissions() (string, string) {
	return "yolo-full-access", "planning relies on an inspection-only prompt; tools retain full access"
}
