package looper

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nibzard/looper/internal/ui"
)

// controllerStart reports whether a command starts a new controller. The
// first-run form runs only for these commands.
func controllerStart(command string) bool {
	switch command {
	case "run", "start", "tui":
		return true
	}
	return false
}

// FormDecision explains whether the first-run form may run.
type FormDecision struct {
	Ask    bool
	Reason string
}

// firstRunDecision applies the gating conditions in plan order: a controller
// start, no quiet mode, no profile and no owner, no setting flags or
// environment variables, and finally interactive terminal streams.
func firstRunDecision(c Config, in *os.File, out io.Writer) FormDecision {
	reject := func(reason string) FormDecision { return FormDecision{false, reason} }
	if !controllerStart(c.Command) {
		return reject("command does not start a controller")
	}
	if c.JSON {
		return reject("--json suppresses forms")
	}
	if c.Plain {
		return reject("--plain suppresses forms")
	}
	if c.Yes {
		return reject("--yes skips the first-run form")
	}
	if env("LOOPER_NO_PROMPT", "") == "1" {
		return reject("LOOPER_NO_PROMPT=1")
	}
	if _, err := os.Stat(c.ProfilePath); err == nil {
		return reject("a saved profile already exists")
	}
	if repositoryOwned(c) {
		return reject("a controller already owns this repository")
	}
	if c.Sources.AnyFlag() || c.Sources.AnyEnv() {
		return reject("setting flags or environment variables were supplied")
	}
	if !isTTY(in) || !writerTTY(out) {
		return reject("input and output must be terminals")
	}
	if !terminalInteractive() {
		return reject("the terminal type does not support interaction")
	}
	return FormDecision{true, ""}
}

// repositoryOwned probes the repository lock without holding it.
func repositoryOwned(c Config) bool {
	common, err := gitText(c.Root, "rev-parse", "--git-common-dir")
	if err != nil {
		return false
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(c.Root, common)
	}
	return ownsRepository(common)
}

// maybeFirstRun asks the startup form when eligible, saves the answers, and
// re-resolves the configuration. Cancellation exits without starting work or
// saving answers. A profile created while the form was open is never replaced.
func maybeFirstRun(args []string, c Config, in *os.File, out, stderr io.Writer) (Config, bool, error) {
	decision := firstRunDecision(c, in, out)
	if !decision.Ask {
		return c, true, nil
	}
	s := ui.Settings{Agent: c.Agent, Jobs: c.Jobs, ReviewEvery: c.ReviewEvery}
	if c.JobsAuto {
		s.Jobs = 0
	}
	detection := DetectVerifyCommand(c.Root)
	s.VerifyCommand = detection.Command
	if err := ui.RunFirstRunForm(&s); err != nil {
		if errors.Is(err, ui.ErrAborted) {
			return c, false, nil
		}
		return c, false, err
	}
	profile := ProfileFromSettings(s)
	if _, err := os.Stat(c.ProfilePath); err == nil {
		fmt.Fprintln(out, "A profile was created while the form was open; using it.")
	} else if err = SaveProfile(c.ProfilePath, profile); err != nil {
		return c, false, err
	} else if !c.JSON {
		fmt.Fprintln(out, "Saved settings to", c.ProfilePath)
		noticeVerifyCommand(out, profile.VerifyCommand, detection, false)
	}
	// Re-resolve: the saved profile now supplies the settings for this run.
	merged, err := Parse(args)
	if err != nil {
		return c, false, err
	}
	return merged, true, nil
}

// noticeVerifyCommand reports the selected verify command and its source.
// An empty command keeps planner selection, which still requires executable
// verification before work proceeds.
func noticeVerifyCommand(out io.Writer, command string, detection Detection, manual bool) {
	switch {
	case command == "":
		fmt.Fprintln(out, "Verify command: chosen by the planner at first run")
	case manual:
		fmt.Fprintf(out, "Verify command: %s (edited)\n", command)
	default:
		fmt.Fprintf(out, "Verify command: %s (detected from %s)\n", command, detection.Source)
	}
}

// ProfileFromSettings converts form output into a saved profile.
func ProfileFromSettings(s ui.Settings) *Profile {
	return &Profile{
		Version: ProfileVersion, Agent: s.Agent, ReviewAgent: s.ReviewAgent,
		Jobs: s.Jobs, Model: s.Model, ClaudeModel: s.ClaudeModel, Effort: s.Effort,
		VerifyCommand: s.VerifyCommand, SetupCommand: s.SetupCommand,
		ReviewEvery: &s.ReviewEvery,
	}
}

// settingsFromProfile fills a form from a saved profile, applying defaults
// for absent fields.
func settingsFromProfile(p *Profile) ui.Settings {
	s := ui.Settings{Agent: "codex", ReviewEvery: 5}
	if p == nil {
		return s
	}
	s.Agent = p.Agent
	if s.Agent == "" {
		s.Agent = "codex"
	}
	s.ReviewAgent, s.Jobs = p.ReviewAgent, p.Jobs
	s.Model, s.ClaudeModel, s.Effort = p.Model, p.ClaudeModel, p.Effort
	s.VerifyCommand, s.SetupCommand = p.VerifyCommand, p.SetupCommand
	if p.ReviewEvery != nil {
		s.ReviewEvery = *p.ReviewEvery
	}
	return s
}

// runConfig edits or resets the saved profile. Without a controller it saves
// changes under the repository lock and releases the lock without starting
// the engine. A controller-owned repository answers through the controller
// request protocol instead of direct file writes.
func runConfig(c Config, in *os.File, out, stderr io.Writer) int {
	if repositoryOwned(c) {
		fmt.Fprintln(stderr, "A controller owns this repository. Settings changes go through the controller; stop it to edit offline.")
		return 1
	}
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	repo, err := OpenRepository(c, dir)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	lock, err := lockRepository(repo.Common)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	defer lock.Close()
	if c.Reset {
		if _, err := os.Stat(c.ProfilePath); err != nil {
			fmt.Fprintln(stderr, "No saved profile exists.")
			return 1
		}
		if err := os.Remove(c.ProfilePath); err != nil {
			fmt.Fprintln(stderr, "Error:", err)
			return 1
		}
		fmt.Fprintln(out, "Removed", c.ProfilePath)
		fmt.Fprintln(out, "Effective settings stay active until the controller restarts.")
		return 0
	}
	profile, err := LoadProfile(c.ProfilePath)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	if !isTTY(in) || !writerTTY(out) || !terminalInteractive() {
		fmt.Fprintln(stderr, "looper config needs an interactive terminal; --reset works without one.")
		return 1
	}
	s := settingsFromProfile(profile)
	detection := DetectVerifyCommand(c.Root)
	edited := s.VerifyCommand != ""
	if err = ui.RunEditorForm(&s); err != nil {
		if errors.Is(err, ui.ErrAborted) {
			fmt.Fprintln(out, "Cancelled. Nothing was saved.")
			return 0
		}
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	if s.VerifyCommand == "" && detection.Command != "" && !edited {
		// Detection supplies a candidate only when nothing explicit exists.
		s.VerifyCommand = detection.Command
	}
	saved := ProfileFromSettings(s)
	if profile != nil {
		saved.Unknown = profile.Unknown
	}
	if err = SaveProfile(c.ProfilePath, saved); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	fmt.Fprintln(out, "Saved settings to", c.ProfilePath)
	noticeVerifyCommand(out, saved.VerifyCommand, detection, edited || s.VerifyCommand != detection.Command)
	return 0
}
