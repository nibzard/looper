package looper

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// AgentAdapter describes the protocol Looper implements, independently of the
// executable or provider. New protocols register here and implement invocation
// and result decoding at the agent boundary.
type AgentAdapter struct {
	protocol         agentProtocol
	Name             string `json:"name"`
	Label            string `json:"label"`
	Version          int    `json:"version"`
	Model            bool   `json:"model"`
	Effort           bool   `json:"effort"`
	StructuredOutput string `json:"structured_output"`
	Planning         string `json:"planning"`
}

var agentAdapters = map[string]AgentAdapter{
	"claude": {protocol: claudeProtocol{}, Name: "claude", Label: "Claude Code", Version: 1, Model: true, Effort: true, StructuredOutput: "schema", Planning: "restricted-tools"},
	"codex":  {protocol: codexProtocol{}, Name: "codex", Label: "Codex", Version: 1, Model: true, Effort: true, StructuredOutput: "schema", Planning: "read-only-sandbox"},
	"zcode":  {protocol: zcodeProtocol{}, Name: "zcode", Label: "Zcode", Version: 1, StructuredOutput: "validated-result", Planning: "prompt-only"},
}

// AgentProfile contains no credentials or shell expression. Command names an
// executable or wrapper; authentication stays with that executable.
type AgentProfile struct {
	Adapter string `json:"adapter"`
	Command string `json:"command"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
}

type AgentCatalog struct {
	revision string
	Version  int                     `json:"version"`
	Agents   map[string]AgentProfile `json:"agents"`
	Worker   string                  `json:"worker,omitempty"`
	Reviewer string                  `json:"reviewer,omitempty"`
}

var agentProfileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func agentCatalogPath() string {
	if path := os.Getenv("LOOPER_AGENTS_FILE"); path != "" {
		return path
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "looper", "agents.json")
}

func (p AgentProfile) validate() error {
	adapter, ok := agentAdapters[p.Adapter]
	if !ok {
		names := make([]string, 0, len(agentAdapters))
		for name := range agentAdapters {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Errorf("unsupported adapter %q; use %s", p.Adapter, strings.Join(names, ", "))
	}
	// Spaces in an absolute executable path are valid. Bare commands must be
	// single command names. No value is ever passed through a shell.
	if p.Command == "" || strings.ContainsAny(p.Command, "\x00\r\n") || (!filepath.IsAbs(p.Command) && strings.ContainsAny(p.Command, " \t;|&$`<>/\\")) {
		return fmt.Errorf("command must be an executable name or an absolute path; use a wrapper for arguments")
	}
	if !adapter.Model && p.Model != "" {
		return fmt.Errorf("adapter %s does not support a model override", p.Adapter)
	}
	if !adapter.Effort && p.Effort != "" {
		return fmt.Errorf("adapter %s does not support an effort override", p.Adapter)
	}
	return nil
}

func (c AgentCatalog) validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported agent catalog version %d", c.Version)
	}
	for name, profile := range c.Agents {
		if !agentProfileName.MatchString(name) {
			return fmt.Errorf("invalid agent profile name %q", name)
		}
		if err := profile.validate(); err != nil {
			return fmt.Errorf("agent profile %s: %w", name, err)
		}
	}
	for _, name := range []string{c.Worker, c.Reviewer} {
		if name != "" {
			if _, ok := c.Agents[name]; !ok {
				return fmt.Errorf("default agent profile %q is missing", name)
			}
		}
	}
	return nil
}

func loadAgentCatalog(path string) (*AgentCatalog, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &AgentCatalog{Version: 1, Agents: map[string]AgentProfile{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var catalog AgentCatalog
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return nil, fmt.Errorf("agent catalog %s: %w", path, err)
	}
	// Reject trailing data rather than applying only part of a configuration.
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("agent catalog %s: unexpected trailing data", path)
	}
	if err := catalog.validate(); err != nil {
		return nil, fmt.Errorf("agent catalog %s: %w", path, err)
	}
	catalog.revision = digest(data)
	if catalog.Agents == nil {
		catalog.Agents = map[string]AgentProfile{}
	}
	return &catalog, nil
}

func saveAgentCatalog(path string, catalog *AgentCatalog) error {
	if err := catalog.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another configuration editor is saving machine agents; retry")
	}
	if catalog.revision != profileDigest(path) {
		return fmt.Errorf("machine agents changed while editing; run looper config again")
	}
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(path, append(data, '\n'), 0600); err != nil {
		return err
	}
	catalog.revision = profileDigest(path)
	return nil
}

// agentProfile resolves against the controller's snapshot, never a live
// machine file. Legacy executable settings remain valid until migrated.
func (c Config) agentProfile(name string) (AgentProfile, error) {
	if p, ok := c.AgentProfiles[name]; ok {
		return p, p.validate()
	}
	p := AgentProfile{Adapter: name}
	switch name {
	case "claude":
		p.Command = c.ClaudeBin
	case "codex":
		p.Command = c.CodexBin
	case "zcode":
		p.Command = c.ZcodeBin
	default:
		return p, fmt.Errorf("unknown agent profile %q; run looper config to add it", name)
	}
	return p, nil
}

func (c Config) effectiveAgent(name string) (AgentProfile, error) {
	p, err := c.agentProfile(name)
	if err != nil {
		return p, err
	}
	// Legacy project, environment, and flag overrides remain authoritative.
	switch p.Adapter {
	case "claude":
		if c.ClaudeModel != "" || c.Sources.ClaudeModel >= SourceEnv {
			p.Model = c.ClaudeModel
		}
	case "codex":
		if c.Model != "" || c.Sources.Model >= SourceEnv {
			p.Model = c.Model
		}
	}
	if p.Adapter == "codex" && (c.Effort != "" || c.Sources.Effort >= SourceEnv) {
		p.Effort = c.Effort
	}
	return p, nil
}

func (c Config) adapterOf(name string) string {
	p, err := c.agentProfile(name)
	if err != nil {
		return name
	}
	return p.Adapter
}

func resolveAgentProfiles(c *Config) error {
	c.AgentCatalogPath = agentCatalogPath()
	if !usesProfile(c.Command) && c.Command != "doctor" {
		return nil
	}
	catalog, err := loadAgentCatalog(c.AgentCatalogPath)
	if err != nil {
		return err
	}
	c.AgentProfiles = catalog.Agents
	// Legacy executable variables affect only their legacy profile names.
	for name, key := range map[string]string{"claude": "CLAUDE_BIN", "codex": "CODEX_BIN", "zcode": "ZCODE_BIN"} {
		if command, ok := os.LookupEnv(key); ok {
			if p, exists := c.AgentProfiles[name]; exists {
				p.Command = command
				c.AgentProfiles[name] = p
			}
		}
	}
	c.DefaultWorker, c.DefaultReviewer = catalog.Worker, catalog.Reviewer
	return nil
}

func agentProfileNames(c Config) []string {
	names := map[string]bool{"claude": true, "codex": true, "zcode": true}
	for name := range c.AgentProfiles {
		names[name] = true
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func resolvedAgentCommand(p AgentProfile) (string, error) {
	return exec.LookPath(p.Command)
}

func agentInstallHint(c Config, name string) string {
	if _, named := c.AgentProfiles[name]; named {
		return "install the configured executable or run looper config to change it"
	}
	key := map[string]string{"claude": "CLAUDE_BIN", "codex": "CODEX_BIN", "zcode": "ZCODE_BIN"}[name]
	if key == "" {
		return "run looper config to select an agent command"
	}
	return "install " + name + " or set " + key
}
