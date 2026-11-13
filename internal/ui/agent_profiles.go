package ui

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/huh"
)

// AgentInterface describes the capabilities shown by the configuration form.
type AgentInterface struct {
	Name, Label   string
	Model, Effort bool
}

// AgentCommand is a form value. Protocol handling stays in the engine.
type AgentCommand struct {
	Name, Command, Adapter, Model, Effort string
}

// RunAgentCommands edits a candidate catalog. The caller saves it only after
// the complete configuration, including project roles, is confirmed.
func RunAgentCommands(agents *[]AgentCommand, worker, reviewer *string, interfaces []AgentInterface) error {
	adapterOptions := []huh.Option[string]{}
	for _, adapter := range interfaces {
		adapterOptions = append(adapterOptions, huh.NewOption(adapter.Label, adapter.Name))
	}
	for {
		action := "continue"
		options := []huh.Option[string]{huh.NewOption("Continue to project settings", "continue"), huh.NewOption("Add an agent command", "add"), huh.NewOption("Edit an agent command", "edit"), huh.NewOption("Set defaults for new projects", "defaults")}
		if err := huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Agent commands on this machine").Description("Authentication stays with each agent. Changes are saved after review.").Options(options...).Value(&action))).Run(); err != nil {
			return err
		}
		if action == "continue" {
			return nil
		}
		names := []string{}
		for _, p := range *agents {
			names = append(names, p.Name)
		}
		sort.Strings(names)
		if action == "defaults" {
			w, r := *worker, *reviewer
			if w == "" {
				w = "claude"
			}
			if r == "" {
				r = sameAsAgent
			}
			if err := huh.NewForm(huh.NewGroup(
				huh.NewSelect[string]().Title("Default worker for new projects").Options(huh.NewOptions(names...)...).Value(&w),
				huh.NewSelect[string]().Title("Default reviewer for new projects").Options(huh.NewOptions(append([]string{sameAsAgent}, names...)...)...).Value(&r),
			)).Run(); err != nil {
				return err
			}
			*worker, *reviewer = w, r
			if r == sameAsAgent {
				*reviewer = ""
			}
			continue
		}
		candidate := AgentCommand{Adapter: "claude"}
		index := -1
		if action == "edit" {
			name := names[0]
			if err := huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Agent command to edit").Options(huh.NewOptions(names...)...).Value(&name))).Run(); err != nil {
				return err
			}
			for i, p := range *agents {
				if p.Name == name {
					candidate = p
					index = i
					break
				}
			}
		}
		nameValidator := func(v string) error {
			if len(v) == 0 || len(v) > 64 {
				return fmt.Errorf("enter a name with 1 to 64 characters")
			}
			for i, c := range v {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && strings.ContainsRune("._-", c)) {
					return fmt.Errorf("use letters, numbers, dots, underscores, or hyphens")
				}
			}
			for i, p := range *agents {
				if p.Name == v && i != index {
					return fmt.Errorf("that name already exists")
				}
			}
			return nil
		}
		if err := huh.NewForm(huh.NewGroup(
			huh.NewInput().Title("Agent name").Description("For example: anthropic, glm, or codex").Value(&candidate.Name).Validate(nameValidator),
			huh.NewInput().Title("Executable name or absolute path").Description("For example: claude-zai. Use a wrapper for command arguments.").Value(&candidate.Command).Validate(func(v string) error { _, err := exec.LookPath(v); return err }),
		)).Run(); err != nil {
			return err
		}
		if action == "add" {
			for _, known := range *agents {
				if candidate.Command == known.Command {
					candidate.Adapter = known.Adapter
					break
				}
			}
			for _, adapter := range interfaces {
				if filepath.Base(candidate.Command) == adapter.Name {
					candidate.Adapter = adapter.Name
					break
				}
			}
		}
		if err := huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Agent interface").Description("Confirm the command's interface. A wrapper uses its underlying agent's interface.").Options(adapterOptions...).Value(&candidate.Adapter))).Run(); err != nil {
			return err
		}
		fields := []huh.Field{}
		for _, adapter := range interfaces {
			if adapter.Name == candidate.Adapter {
				if adapter.Model {
					fields = append(fields, huh.NewInput().Title("Model (empty uses agent settings)").Value(&candidate.Model))
				} else {
					candidate.Model = ""
				}
				if adapter.Effort {
					fields = append(fields, huh.NewInput().Title("Effort (empty uses agent settings)").Value(&candidate.Effort))
				} else {
					candidate.Effort = ""
				}
			}
		}
		if len(fields) > 0 {
			if err := huh.NewForm(huh.NewGroup(fields...)).Run(); err != nil {
				return err
			}
		}
		if index < 0 {
			*agents = append(*agents, candidate)
		} else {
			(*agents)[index] = candidate
		}
	}
}

func ConfirmAgentSettings(summary string) (bool, error) {
	confirmed := false
	err := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Save this configuration?").Description(summary).Value(&confirmed))).Run()
	return confirmed, err
}
