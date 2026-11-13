package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/nibzard/looper/internal/contractschema"
)

func profileCatalogForTest(t *testing.T, catalog *AgentCatalog) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.json")
	t.Setenv("LOOPER_AGENTS_FILE", path)
	if err := saveAgentCatalog(path, catalog); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAgentProfilesResolveRoleDefaultsAndOverrides(t *testing.T) {
	c := repoConfig(t, "run")
	path := profileCatalogForTest(t, &AgentCatalog{Version: 1, Worker: "glm", Reviewer: "anthropic", Agents: map[string]AgentProfile{
		"glm":       {Adapter: "claude", Command: "claude-zai", Model: "glm-model", Effort: "high"},
		"anthropic": {Adapter: "claude", Command: "claude", Model: "anthropic-model"},
	}})
	c, err := Parse([]string{"run"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "glm" || c.ReviewAgent != "anthropic" || c.Sources.Agent != SourceMachine {
		t.Fatalf("machine defaults: %+v", c)
	}
	worker, _ := c.effectiveAgent(c.Agent)
	review, _ := c.effectiveAgent(c.ReviewAgent)
	if worker.Command != "claude-zai" || review.Command != "claude" || worker.Model == review.Model {
		t.Fatalf("roles collapsed: %+v %+v", worker, review)
	}
	// Each controller holds its own catalog snapshot.
	catalog, _ := loadAgentCatalog(path)
	changed := catalog.Agents["glm"]
	changed.Command = "changed-wrapper"
	catalog.Agents["glm"] = changed
	if err := saveAgentCatalog(path, catalog); err != nil {
		t.Fatal(err)
	}
	worker, _ = c.effectiveAgent(c.Agent)
	if worker.Command != "claude-zai" {
		t.Fatal("machine edit changed a running configuration")
	}
	if err := SaveProfile(c.ProfilePath, &Profile{Version: 1, Agent: "anthropic", ReviewAgent: "glm"}); err != nil {
		t.Fatal(err)
	}
	c, err = Parse([]string{"run"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "anthropic" || c.ReviewAgent != "glm" || c.Sources.Agent != SourceProfile {
		t.Fatalf("project must override machine: %+v", c)
	}
	t.Setenv("LOOPER_ITER_SCHEDULE", "glm")
	c, err = Parse([]string{"run", "--agent", "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "anthropic" || c.Sources.Agent != SourceFlag {
		t.Fatal("flag precedence lost")
	}
	t.Setenv("CLAUDE_MODEL", "")
	c, err = Parse([]string{"run"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.effectiveAgent("glm")
	if p.Model != "" {
		t.Fatal("an explicit empty override must select agent defaults")
	}
}

func TestAgentProfilesCatalogValidationAndStaticResolution(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "executed")
	binary := filepath.Join(root, "custom-agent")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	path := profileCatalogForTest(t, &AgentCatalog{Version: 1, Agents: map[string]AgentProfile{"custom": {Adapter: "claude", Command: binary}}})
	c, err := loadAgentCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolvedAgentCommand(c.Agents["custom"]); err != nil {
		t.Fatal(err)
	}
	if fileExists(marker) {
		t.Fatal("static validation launched the command")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("catalog permissions: %v", info.Mode())
	}
	for _, p := range []AgentProfile{
		{Adapter: "pi", Command: "pi"},
		{Adapter: "claude", Command: "claude --model opus"},
		{Adapter: "claude", Command: "$(touch injected)"},
		{Adapter: "zcode", Command: "zcode", Model: "unsupported"},
	} {
		if err := p.validate(); err == nil {
			t.Fatalf("accepted invalid definition: %+v", p)
		}
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"agents":{}} {}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAgentCatalog(path); err == nil {
		t.Fatal("accepted trailing document")
	}
}

func TestAgentProfilesSameAdapterLaunchIndependently(t *testing.T) {
	root := t.TempDir()
	profiles := map[string]AgentProfile{}
	for _, name := range []string{"glm", "anthropic"} {
		capture := filepath.Join(root, name+".args")
		bin := filepath.Join(root, name)
		script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + capture + "\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"result\",\"structured_output\":{\"ok\":true}}'\n"
		if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		profiles[name] = AgentProfile{Adapter: "claude", Command: bin, Model: name + "-model", Effort: "high"}
	}
	c := Config{AgentProfiles: profiles, RetryLimit: 1}
	var wg sync.WaitGroup
	for name := range profiles {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir := filepath.Join(root, name+"-run")
			if _, err := (Agent{Config: c}).Run(context.Background(), name, root, dir, "prompt", false); err != nil {
				t.Error(err)
				return
			}
			var invocation map[string]any
			if err := readJSON(filepath.Join(dir, "invocation.json"), &invocation); err != nil {
				t.Error(err)
				return
			}
			if invocation["agent_profile"] != name || invocation["adapter"] != "claude" || invocation["executable"] != profiles[name].Command {
				t.Errorf("wrong invocation identity: %+v", invocation)
			}
		}()
	}
	wg.Wait()
	for name := range profiles {
		data, err := os.ReadFile(filepath.Join(root, name+".args"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), name+"-model") || !strings.Contains(string(data), "--effort\nhigh") || !strings.Contains(string(data), "--dangerously-skip-permissions") {
			t.Fatalf("wrong args for %s: %s", name, data)
		}
	}
	if profiles["glm"].Command == profiles["anthropic"].Command {
		t.Fatal("shared state mutated")
	}
}

func TestAgentProfilesMissingConfigurationAndInspection(t *testing.T) {
	c := repoConfig(t, "run")
	if err := requireAgentConfiguration(c); err == nil {
		t.Fatal("fresh project silently chose a provider")
	}
	c.Sources.Agent = SourceFlag
	if err := requireAgentConfiguration(c); err != nil {
		t.Fatal(err)
	}
	c.Sources.Agent = SourceDefault
	c.Yes = true
	if err := requireAgentConfiguration(c); err != nil {
		t.Fatal(err)
	}
	c.Yes = false
	writeProfileFile(t, c.Todo, `{"tasks":[]}`)
	if err := requireAgentConfiguration(c); err != nil {
		t.Fatal("legacy backlog must retain its configuration", err)
	}
	t.Setenv("LOOPER_AGENTS_FILE", filepath.Join(t.TempDir(), "broken.json"))
	if err := os.WriteFile(agentCatalogPath(), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"help", "version", "status", "stop", "runs"} {
		if _, err := Parse([]string{command}); err != nil {
			t.Fatalf("inspection %s blocked by agent setup: %v", command, err)
		}
	}
	if _, err := Parse([]string{"not-a-command"}); err == nil {
		t.Fatal("unknown command accepted")
	}
}

func TestAgentProfilesPreflightReportsPerRoleSettings(t *testing.T) {
	c := repoConfig(t, "preflight")
	c.Agent, c.ReviewAgent = "glm", "anthropic"
	c.AgentProfiles = map[string]AgentProfile{
		"glm":       {Adapter: "claude", Command: "claude-zai", Model: "glm-5.3"},
		"anthropic": {Adapter: "claude", Command: "claude", Model: "opus"},
	}
	doc := buildPreflight(c)
	if doc.Agents.Coding.Adapter != "claude" || doc.Agents.Coding.Model != "glm-5.3" || doc.Agents.Review.Model != "opus" {
		t.Fatalf("profile details lost: %+v", doc.Agents)
	}
	var b bytes.Buffer
	if err := json.NewEncoder(&b).Encode(doc); err != nil {
		t.Fatal(err)
	}
	if b.Len() > preflightBytesBudget {
		t.Fatal("preflight exceeded its budget")
	}
}

func TestAgentProfilesConcurrentCatalogEditIsRejected(t *testing.T) {
	path := profileCatalogForTest(t, &AgentCatalog{Version: 1, Agents: map[string]AgentProfile{}})
	first, _ := loadAgentCatalog(path)
	second, _ := loadAgentCatalog(path)
	first.Agents["one"] = AgentProfile{Adapter: "claude", Command: "claude"}
	if err := saveAgentCatalog(path, first); err != nil {
		t.Fatal(err)
	}
	second.Agents["two"] = AgentProfile{Adapter: "codex", Command: "codex"}
	if err := saveAgentCatalog(path, second); err == nil {
		t.Fatal("stale editor overwrote another editor")
	}
	kept, _ := loadAgentCatalog(path)
	if _, ok := kept.Agents["one"]; !ok {
		t.Fatal("first edit lost")
	}
}

func TestAgentProfilesLegacyBinOnlyOverridesItsLegacyName(t *testing.T) {
	repoConfig(t, "run")
	profileCatalogForTest(t, &AgentCatalog{Version: 1, Agents: map[string]AgentProfile{
		"claude": {Adapter: "claude", Command: "saved-claude"},
		"glm":    {Adapter: "claude", Command: "claude-zai"},
	}})
	t.Setenv("CLAUDE_BIN", "legacy-wrapper")
	c, err := Parse([]string{"run", "--agent", "glm"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := c.agentProfile("glm")
	legacy, _ := c.agentProfile("claude")
	if p.Command != "claude-zai" || legacy.Command != "legacy-wrapper" {
		t.Fatalf("legacy executable leaked into named profile: %+v %+v", p, legacy)
	}
}

func TestAgentProfilesLiveProbeRequiresStructuredOutputAndTool(t *testing.T) {
	root := t.TempDir()
	for _, tool := range []bool{false, true} {
		bin := filepath.Join(root, "agent")
		script := "#!/bin/sh\ncat >/dev/null\n"
		if tool {
			script += "printf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"tool_use\",\"name\":\"Read\"}]}}'\n"
		}
		script += "printf '%s\\n' '{\"type\":\"result\",\"structured_output\":{\"setup\":\"\",\"verify\":\"\",\"tasks\":[]}}'\n"
		if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		c := Config{TestAgent: "custom", AgentProfiles: map[string]AgentProfile{"custom": {Adapter: "claude", Command: bin}}}
		if tool {
			c.JSON = true
			c.Contract = ContractVersionV2
		}
		var out, stderr bytes.Buffer
		code := testAgentProfile(c, &out, &stderr)
		if tool && code != 0 {
			t.Fatalf("tool test failed: %s", stderr.String())
		}
		if !tool && code == 0 {
			t.Fatal("test accepted a result without a tool call")
		}
		if tool {
			_, file, _, _ := runtime.Caller(0)
			schema, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../docs/contracts/schemas/v2/agent-test.schema.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := contractschema.ValidateJSON(schema, out.Bytes()); err != nil {
				t.Fatalf("live probe result violates its contract: %v", err)
			}
		}
	}
}

func TestAgentProfilesExplicitEmptySelectionNeverChoosesProvider(t *testing.T) {
	repoConfig(t, "run")
	profileCatalogForTest(t, &AgentCatalog{Version: 1, Worker: "glm", Agents: map[string]AgentProfile{"glm": {Adapter: "claude", Command: "claude-zai"}}})
	if _, err := Parse([]string{"run", "--agent", ""}); err == nil {
		t.Fatal("empty explicit worker silently selected a provider")
	}
}

func TestAgentProfilesLiveRoleChangesRespectControllerSnapshot(t *testing.T) {
	c := repoConfig(t, "run")
	c.AgentProfiles = map[string]AgentProfile{"glm": {Adapter: "claude", Command: "claude-zai"}}
	e := &Engine{Config: c}
	p := &Profile{Version: 1, Agent: "glm"}
	fields := map[string]json.RawMessage{"agent": json.RawMessage(`"glm"`)}
	outcomes := e.applyLiveSettings(p, fields)
	if e.Config.Agent != "glm" || outcomes["agent"] != "saved; applied" {
		t.Fatalf("known profile not applied: %+v", outcomes)
	}
	p.Agent = "new-profile"
	outcomes = e.applyLiveSettings(p, fields)
	if e.Config.Agent != "glm" || !strings.Contains(outcomes["agent"], "restart") {
		t.Fatalf("unknown profile changed a live controller: %+v", outcomes)
	}
}

func TestAgentProfilesMissingDefinitionCanBeRepairedInConfig(t *testing.T) {
	c := repoConfig(t, "run")
	profileCatalogForTest(t, &AgentCatalog{Version: 1, Agents: map[string]AgentProfile{}})
	if err := SaveProfile(c.ProfilePath, &Profile{Version: 1, Agent: "missing-machine-profile"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]string{"run"}); err == nil {
		t.Fatal("run accepted a missing command definition")
	}
	if _, err := Parse([]string{"config"}); err != nil {
		t.Fatalf("configuration editor cannot repair the missing definition: %v", err)
	}
	if _, err := Parse([]string{"config", "--reset"}); err != nil {
		t.Fatalf("reset cannot repair the missing definition: %v", err)
	}
}

func TestAgentProfilesMissingRetainedAgentIsConfigurationFailure(t *testing.T) {
	dir := t.TempDir()
	_, err := (Agent{Config: Config{RetryLimit: 1}}).Run(context.Background(), "removed-profile", dir, dir, "prompt", true)
	var agent *AgentError
	if !errors.As(err, &agent) || agent.Kind != "configuration" || agent.Summary == nil {
		t.Fatalf("missing definition consumed an execution repair: %v", err)
	}
	if !fileExists(filepath.Join(dir, "invocation.json")) {
		t.Fatal("missing configuration evidence")
	}
	if fileExists(filepath.Join(dir, "events-00.jsonl")) {
		t.Fatal("a missing profile launched a process")
	}
}

func TestAgentProfilesSavedFollowingReviewerOverridesMachineDefault(t *testing.T) {
	c := repoConfig(t, "run")
	profileCatalogForTest(t, &AgentCatalog{Version: 1, Worker: "glm", Reviewer: "anthropic", Agents: map[string]AgentProfile{
		"glm": {Adapter: "claude", Command: "claude-zai"}, "anthropic": {Adapter: "claude", Command: "claude"},
	}})
	if err := SaveProfile(c.ProfilePath, &Profile{Version: 1, Agent: "glm", ReviewAgent: ""}); err != nil {
		t.Fatal(err)
	}
	got, err := Parse([]string{"run"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewAgent != "glm" || got.Sources.ReviewAgent != SourceDefault {
		t.Fatalf("following reviewer replaced by machine default: %+v", got)
	}
}

func TestAgentProfilesNewDefinitionCanBeSavedForRestart(t *testing.T) {
	e := protocolEngine(t)
	path := profileCatalogForTest(t, &AgentCatalog{Version: 1, Agents: map[string]AgentProfile{"new-profile": {Adapter: "claude", Command: "claude-zai"}}})
	e.Config.AgentCatalogPath = path
	if err := SaveProfile(e.Config.ProfilePath, &Profile{Version: 1}); err != nil {
		t.Fatal(err)
	}
	before := e.Config.Agent
	ack := e.applySettingsRequest(request(e, map[string]any{"agent": "new-profile"}, profileDigest(e.Config.ProfilePath)))
	if ack.Status != AckApplied || !strings.Contains(ack.Outcomes["agent"], "restart") || e.Config.Agent != before {
		t.Fatalf("new profile applied before restart or failed to save: %+v", ack)
	}
	saved, err := LoadProfile(e.Config.ProfilePath)
	if err != nil || saved.Agent != "new-profile" {
		t.Fatalf("new role not retained: %+v %v", saved, err)
	}
}
