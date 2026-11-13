// Package ui renders Looper's interactive surfaces. It stays independent of
// the engine package: forms and dashboards accept plain values and return
// results without importing engine types.
package ui

import (
	"fmt"
	"strconv"

	"github.com/charmbracelet/huh"
)

// Settings is the editable slice of the saved run profile. A zero Jobs means
// automatic mode; an empty ReviewAgent follows the coding agent; empty model
// fields use each agent's own configuration.
type Settings struct {
	AgentChoices  *[]string
	Agent         string
	ReviewAgent   string
	Jobs          int
	Model         string
	ClaudeModel   string
	Effort        string
	VerifyCommand string
	SetupCommand  string
	ReviewEvery   int
}

const (
	autoSessions = "auto"
	sameAsAgent  = "same as coding agent"
)

// ErrAborted reports a form cancelled before submission. It carries no side
// effects: nothing is saved when it is returned.
var ErrAborted = huh.ErrUserAborted

// Field definitions are shared by the first-run form and the full editor so
// both present and validate the same choices.

func agentChoices(s *Settings) []string {
	if s.AgentChoices != nil && len(*s.AgentChoices) > 0 {
		return *s.AgentChoices
	}
	return []string{"codex", "claude", "zcode"}
}

func agentField(s *Settings) *huh.Select[string] {
	return huh.NewSelect[string]().
		Title("Worker agent profile").
		Options(huh.NewOptions(agentChoices(s)...)...).
		Value(&s.Agent)
}

func reviewAgentField(s *Settings, selection *string) *huh.Select[string] {
	return huh.NewSelect[string]().
		Title("Reviewer agent profile").
		Description("Also used for planning and diagnosis.").
		Options(huh.NewOptions(append([]string{sameAsAgent}, agentChoices(s)...)...)...).
		Value(selection)
}

func sessionsSelect() *huh.Select[int] {
	return huh.NewSelect[int]().
		Title("Parallel sessions").
		Options(
			huh.NewOption(autoSessions, 0),
			huh.NewOption("1", 1), huh.NewOption("2", 2),
			huh.NewOption("4", 4), huh.NewOption("8", 8),
		)
}

// sessionsInput accepts any positive session count, including values outside
// the first-run list, plus auto mode.
func sessionsInput(s *Settings, value *string) *huh.Input {
	return huh.NewInput().
		Title("Parallel sessions (auto or a positive number)").
		Value(value).
		Validate(func(v string) error {
			if v == autoSessions {
				return nil
			}
			if n, err := strconv.Atoi(v); err != nil || n < 1 {
				return fmt.Errorf("enter %s or a positive number", autoSessions)
			}
			return nil
		})
}

func numberField(title, description string, value *string) *huh.Input {
	return huh.NewInput().
		Title(title).
		Value(value).
		Description(description).
		Validate(func(v string) error {
			if n, err := strconv.Atoi(v); err != nil || n < 0 {
				return fmt.Errorf("enter a nonnegative number")
			}
			return nil
		})
}

// RunFirstRunForm asks the three startup questions. Enter accepts the
// highlighted choice. An empty ReviewAgent answer follows the coding agent.
func RunFirstRunForm(s *Settings) error {
	review := sameAsAgent
	if s.ReviewAgent != "" {
		review = s.ReviewAgent
	}
	sessions := sessionsSelect().Value(&s.Jobs)
	form := huh.NewForm(
		huh.NewGroup(
			agentField(s),
			reviewAgentField(s, &review),
			sessions,
		),
	)
	if err := form.Run(); err != nil {
		return err
	}
	s.ReviewAgent = ""
	if review != sameAsAgent {
		s.ReviewAgent = review
	}
	return nil
}

// RunEditorForm edits every profile field: both agents, any positive session
// count, both model overrides, effort, the verify and setup commands, and
// the review frequency.
func RunEditorForm(s *Settings) error {
	review := sameAsAgent
	if s.ReviewAgent != "" {
		review = s.ReviewAgent
	}
	sessions := autoSessions
	if s.Jobs > 0 {
		sessions = strconv.Itoa(s.Jobs)
	}
	frequency := strconv.Itoa(s.ReviewEvery)
	groups := []*huh.Group{huh.NewGroup(
		agentField(s), reviewAgentField(s, &review), sessionsInput(s, &sessions),
	)}
	// Adapter-wide values exist only for migration. New configurations set
	// model and effort on each named command so roles stay independent.
	if s.AgentChoices == nil || s.Model != "" || s.ClaudeModel != "" || s.Effort != "" {
		groups = append(groups, huh.NewGroup(
			huh.NewInput().Title("Codex model (legacy shared override)").Description("Clear this to use each agent profile's model.").Value(&s.Model),
			huh.NewInput().Title("Claude model (legacy shared override)").Description("Clear this to use each agent profile's model.").Value(&s.ClaudeModel),
			huh.NewInput().Title("Codex reasoning effort (legacy shared override)").Value(&s.Effort),
		))
	}
	groups = append(groups, huh.NewGroup(numberField("Review frequency", "integrations between milestone reviews; 0 disables them", &frequency)), huh.NewGroup(
		huh.NewInput().Title("Verify command (empty lets planning choose)").Value(&s.VerifyCommand),
		huh.NewInput().Title("Setup command (empty lets planning choose)").Value(&s.SetupCommand),
	))
	form := huh.NewForm(groups...)
	if err := form.Run(); err != nil {
		return err
	}
	s.ReviewAgent = ""
	if review != sameAsAgent {
		s.ReviewAgent = review
	}
	if sessions == autoSessions {
		s.Jobs = 0
	} else if n, err := strconv.Atoi(sessions); err == nil && n > 0 {
		s.Jobs = n
	}
	if n, err := strconv.Atoi(frequency); err == nil && n >= 0 {
		s.ReviewEvery = n
	}
	return nil
}
