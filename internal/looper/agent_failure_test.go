package looper

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A lapsed provider account or unsupported model makes every invocation fail
// before any work runs. The recorded error must name the agent's own failure
// text so blockers, review errors, and shutdown output self-diagnose.
func TestAgentFailureSurfacesStreamMessage(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	script := `#!/bin/bash
printf '%s\n' '{"type":"error","message":"The '\''stub-astra'\'' model is not supported when using Codex with a ChatGPT account."}'
printf '%s\n' '{"type":"turn.failed","error":{"message":"The '\''stub-astra'\'' model is not supported when using Codex with a ChatGPT account."}}'
exit 1
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	a := Agent{Config: Config{CodexBin: bin, RetryLimit: 2, RetryDelay: time.Millisecond}}
	_, err := a.Run(context.Background(), "codex", dir, dir, "prompt", false)
	var agent *AgentError
	if !errors.As(err, &agent) {
		t.Fatalf("expected AgentError, got %v", err)
	}
	if agent.Kind != "configuration" {
		t.Fatalf("classification: %s", agent.Kind)
	}
	if !strings.Contains(agent.Error(), "agent reported: The 'stub-astra' model is not supported when using Codex with a ChatGPT account.") {
		t.Fatalf("agent message missing: %s", agent.Error())
	}
	if !strings.Contains(agent.Error(), filepath.Join(dir, "events-00.jsonl")) {
		t.Fatalf("log path missing: %s", agent.Error())
	}
}

// T021: classification reads the process error and the agent's own error
// text, never the raw session log. The log echoes tool output and project
// files, so configuration-like project text or a printed status number in
// the log tail must leave an execution failure, and the echoed text must
// stay out of the returned detail.
func TestAgentFailureIgnoresEchoedLogText(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	script := `#!/bin/bash
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"README says billing, quota, and the usage limit apply after the trial; see pricing page 401."}]}}'
printf '%s\n' '{"type":"user","message":{"content":[{"type":"tool_result","content":"health check responses: 502 failed, 1024 passed"}]}}'
exit 1
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	a := Agent{Config: Config{ClaudeBin: bin, RetryLimit: 1, RetryDelay: time.Millisecond}}
	_, err := a.Run(context.Background(), "claude", dir, dir, "prompt", true)
	var agent *AgentError
	if !errors.As(err, &agent) {
		t.Fatalf("expected AgentError, got %v", err)
	}
	if agent.Kind != "execution" {
		t.Fatalf("echoed log text classified %s", agent.Kind)
	}
	if strings.Contains(agent.Error(), "billing") || strings.Contains(agent.Error(), "502 failed") {
		t.Fatalf("echoed project text entered the failure detail: %s", agent.Error())
	}
}

// T021: the agent's own error text keeps the trusted classifications. A
// synthetic account error classifies as configuration; rate-limit text
// classifies as transient.
func TestAgentFailureClassifiesStreamError(t *testing.T) {
	for _, tc := range []struct {
		name, message, want string
	}{
		{"account error", "Credit balance is too low. Add funds to continue.", "configuration"},
		{"rate limit", "stub upstream reports 429 rate limit; retry later", "transient"},
		{"bare status without context", "stub agent printed 502", "execution"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "agent.sh")
			script := "#!/bin/bash\nprintf '%s\\n' '{\"type\":\"error\",\"message\":\"" + tc.message + "\"}'\nexit 1\n"
			if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			a := Agent{Config: Config{ClaudeBin: bin, RetryLimit: 1, RetryDelay: time.Millisecond}}
			_, err := a.Run(context.Background(), "claude", dir, dir, "prompt", true)
			var agent *AgentError
			if !errors.As(err, &agent) {
				t.Fatalf("expected AgentError, got %v", err)
			}
			if agent.Kind != tc.want {
				t.Fatalf("stream error %q classified %s", tc.message, agent.Kind)
			}
			if !strings.Contains(agent.Error(), tc.message) {
				t.Fatalf("stream error missing from detail: %s", agent.Error())
			}
		})
	}
}
