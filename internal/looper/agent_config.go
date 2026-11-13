package looper

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/nibzard/looper/internal/ui"
)

// configureAgentCommands does not write either configuration scope. A cancelled
// form leaves machine definitions and project preferences untouched.
func configureAgentCommands(c Config, s *ui.Settings) (*AgentCatalog, error) {
	catalog, err := loadAgentCatalog(c.AgentCatalogPath)
	if err != nil {
		return nil, err
	}
	c.AgentProfiles = catalog.Agents
	for name, key := range map[string]string{"claude": "CLAUDE_BIN", "codex": "CODEX_BIN", "zcode": "ZCODE_BIN"} {
		if command, ok := os.LookupEnv(key); ok {
			if p, exists := c.AgentProfiles[name]; exists {
				p.Command = command
				c.AgentProfiles[name] = p
			}
		}
	}
	candidates := map[string]AgentProfile{}
	for _, name := range agentProfileNames(c) {
		p, err := c.agentProfile(name)
		if err != nil {
			return nil, err
		}
		candidates[name] = p
	}
	// Discover installed provider wrappers without changing their files or
	// assuming an arbitrary command implements a known protocol.
	for _, name := range []string{"claude-zai"} {
		if _, ok := candidates[name]; !ok {
			if _, err := exec.LookPath(name); err == nil {
				candidates[name] = AgentProfile{Adapter: "claude", Command: name}
			}
		}
	}
	commands := []ui.AgentCommand{}
	for name, p := range candidates {
		commands = append(commands, ui.AgentCommand{Name: name, Command: p.Command, Adapter: p.Adapter, Model: p.Model, Effort: p.Effort})
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].Name < commands[j].Name })
	interfaces := []ui.AgentInterface{}
	for name, adapter := range agentAdapters {
		interfaces = append(interfaces, ui.AgentInterface{Name: name, Label: orDefault(adapter.Label, name), Model: adapter.Model, Effort: adapter.Effort})
	}
	sort.Slice(interfaces, func(i, j int) bool { return interfaces[i].Name < interfaces[j].Name })
	if err := ui.RunAgentCommands(&commands, &catalog.Worker, &catalog.Reviewer, interfaces); err != nil {
		return nil, err
	}
	catalog.Agents = map[string]AgentProfile{}
	choices := []string{}
	for _, p := range commands {
		catalog.Agents[p.Name] = AgentProfile{Adapter: p.Adapter, Command: p.Command, Model: p.Model, Effort: p.Effort}
		choices = append(choices, p.Name)
	}
	sort.Strings(choices)
	// A missing machine catalog must not lock the user out of repairing
	// project roles through the editor.
	if _, ok := catalog.Agents[s.Agent]; !ok {
		s.Agent = orDefault(catalog.Worker, defaultAgent)
	}
	if s.ReviewAgent != "" {
		if _, ok := catalog.Agents[s.ReviewAgent]; !ok {
			s.ReviewAgent = ""
		}
	}
	s.AgentChoices = &choices
	return catalog, catalog.validate()
}

func confirmAgentConfiguration(c Config, s ui.Settings, catalog *AgentCatalog) error {
	candidate := c
	candidate.AgentProfiles = catalog.Agents
	summary := []string{"Machine commands: " + c.AgentCatalogPath, "Project choices: " + c.ProfilePath}
	review := s.ReviewAgent
	if review == "" {
		review = s.Agent
	}
	candidate.Model, candidate.ClaudeModel, candidate.Effort = s.Model, s.ClaudeModel, s.Effort
	for _, role := range []struct{ name, profile string }{{"Worker", s.Agent}, {"Reviewer", review}} {
		p, err := candidate.effectiveAgent(role.profile)
		if err != nil {
			return err
		}
		path, err := resolvedAgentCommand(p)
		if err != nil {
			return fmt.Errorf("%s agent %s: %w", role.name, role.profile, err)
		}
		sandbox, note := agentSandbox(p.Adapter)
		summary = append(summary, fmt.Sprintf("%s: %s → %s (%s); model: %s; effort: %s; permissions: %s. %s", role.name, role.profile, path, p.Adapter, orDefault(p.Model, "agent settings"), orDefault(p.Effort, "agent settings"), sandbox, note))
	}
	summary = append(summary, "Saving runs no agents. Machine command changes apply after controller restart.")
	ok, err := ui.ConfirmAgentSettings(strings.Join(summary, "\n"))
	if err != nil {
		return err
	}
	if !ok {
		return ui.ErrAborted
	}
	return nil
}

func agentConfigurationError(c Config) error {
	p := usageFailure("configuration_required", "Choose an agent before starting this project.")
	p.Paths = []string{c.ProfilePath, c.AgentCatalogPath}
	p.Actions = []ErrorAction{{Description: "Configure agent commands and project roles interactively.", Command: "looper config"}, {Description: "Select an existing agent explicitly for a non-interactive run.", Command: "looper --agent claude"}}
	return p
}

// requireAgentConfiguration protects fresh unattended projects. An existing
// backlog retains the legacy default until its operator edits configuration.
func requireAgentConfiguration(c Config) error {
	if !controllerStart(c.Command) || repositoryOwned(c) || c.Yes {
		return nil
	}
	if c.Sources.Agent != SourceDefault {
		return nil
	}
	if fileExists(c.ProfilePath) || fileExists(c.Todo) {
		return nil
	}
	return agentConfigurationError(c)
}

// fileExists is deliberately local: a missing project file is not setup.
func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }
