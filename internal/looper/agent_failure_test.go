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
