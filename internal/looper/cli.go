package looper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

var Version = "dev"

const help = `Looper — continuous local development

Usage: looper [run] [task-file] [options]
       looper status [--json]
       looper stop
       looper tail [--follow]
       looper doctor
       looper config [--reset]

Run again to resume retained work. The default run waits for new work when idle.
--once finishes after the current goal is checked or all remaining work is blocked.
The first interactive run in a project asks three setup questions and saves the
answers to runtime/profile.json; looper config edits that profile.

Options:
  --jobs auto|N          Maximum concurrent workers (default: auto)
  --check-jobs N         Concurrent setup and checks (default: 1)
  --agent codex|claude   Coding agent (default: codex)
  --review-agent NAME   Planning and review agent (default: coding agent)
  --model NAME          Explicit model; otherwise use the agent's configuration
  --effort NAME         Codex reasoning effort
  --max-iterations N    Dispatch limit; 0 is unlimited (default: 0)
  --review-every N      Check product behavior after N integrations (default: 5)
  --once                Exit when no runnable work remains after review
  --plain               Plain text only; no forms, color, or glyphs
  --yes                 Skip the first-run form and use defaults
  --json                Emit machine-readable progress
  --verbose             Show the run configuration at start
  --help                Show help

Project commands can be supplied through LOOPER_SETUP_COMMAND,
LOOPER_VERIFY_COMMAND, and optional LOOPER_ASSESS_COMMAND.
Runtime records live in LOOPER_BASE_DIR (default: ~/.looper).
Git commands time out after LOOPER_GIT_TIMEOUT_SECONDS (default: 600; 0 waits).
`

func Main(args []string, out, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "__exec" {
		return RunChild(args[1:])
	}
	if len(args) > 0 && args[0] == "__daemon" {
		if len(args) < 2 {
			fmt.Fprintln(stderr, "the daemon entry point needs a configuration path")
			return 2
		}
		ready := io.Writer(os.Stderr)
		if f := os.NewFile(3, "daemon-readiness"); f != nil {
			ready = f
		}
		return RunDaemon(args[1], ready)
	}

	c, err := Parse(args)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 2
	}
	if c.Command == "help" {
		fmt.Fprint(out, help)
		return 0
	}
	if c.Command == "version" {
		fmt.Fprintln(out, "looper", Version)
		return 0
	}
	switch c.Command {
	case "add-plan":
		fmt.Fprintln(stderr, c.Command+" is not available in this build yet.")
		return 2
	case "tui":
		return runTUI(c, out, stderr)
	case "attach":
		return runAttach(c, out, stderr, false)
	}
	// SIGTERM drains like `looper stop`; SIGINT cancels with retained work.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	installControllerSignals(c, cancel)
	if c.Command == "doctor" {
		return doctor(c, out)
	}
	if c.Command == "config" {
		return runConfig(c, os.Stdin, out, stderr)
	}
	_, dir := projectPaths(c)
	if c.Command == "stop" {
		if _, err = os.Stat(filepath.Join(dir, "state.db")); err != nil {
			fmt.Fprintln(stderr, "No saved Looper run exists in this project.")
			return 1
		}
		if err = atomicWrite(filepath.Join(dir, "stop"), []byte("drain\n"), 0600); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(out, "Stop requested. Active work will drain.")
		return 0
	}
	if c.Command == "status" || c.Command == "tail" {
		if err = inspect(ctx, c, out); err != nil {
			fmt.Fprintln(stderr, "Error:", err)
			return 1
		}
		return 0
	}
	if controllerStart(c.Command) {
		var start bool
		c, start, err = maybeFirstRun(args, c, os.Stdin, out, stderr)
		if err != nil {
			fmt.Fprintln(stderr, "Error:", err)
			return 2
		}
		if !start {
			return 0
		}
	}
	if c.Command == "start" {
		return StartDetached(c, out, stderr)
	}
	profileAtStart := profileDigest(c.ProfilePath)
	e, lock, err := NewEngine(c, out)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	defer lock.Close()
	defer e.Store.Close()
	// Recheck after acquiring the lock: a profile written while the form or
	// startup ran is reported, never silently replaced.
	if profileAtStart != "" && profileDigest(c.ProfilePath) != profileAtStart && !c.JSON {
		fmt.Fprintln(out, "Note: the saved profile changed while this run was starting; the new values apply through looper config or the next run.")
	}
	code, err := e.Run(ctx)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "Interrupted. Work is retained; run looper to resume.")
		} else {
			fmt.Fprintln(stderr, "Error:", err)
		}
	}
	return code
}
func doctor(c Config, out io.Writer) int {
	mode := DetectMode(os.Stdin, out, c.Plain)
	if c.JSON {
		mode = ModePlain
	}
	p := NewPresenter(mode, out, func() int {
		if f, ok := out.(*os.File); ok {
			return terminalWidth(f)
		}
		return 0
	})
	code := 0
	agentBin := map[string]string{"codex": c.CodexBin, "claude": c.ClaudeBin}
	checks := []struct{ name, hint string }{
		{"git", "install Git and make sure git is on PATH"},
		{"bash", "install Bash or point LOOPER_* commands at another shell"},
		{agentBin[c.Agent], "install " + c.Agent + " or set " + map[string]string{"codex": "CODEX_BIN", "claude": "CLAUDE_BIN"}[c.Agent]},
	}
	if c.ReviewAgent != c.Agent {
		checks = append(checks, struct{ name, hint string }{agentBin[c.ReviewAgent], "install " + c.ReviewAgent + " or set " + map[string]string{"codex": "CODEX_BIN", "claude": "CLAUDE_BIN"}[c.ReviewAgent]})
	}
	for _, check := range checks {
		path, err := exec.LookPath(check.name)
		if err != nil {
			p.DoctorLine(false, check.name, check.hint)
			code = 1
		} else {
			p.DoctorLine(true, path, check.hint)
		}
	}
	var b Backlog
	if err := readJSON(c.Todo, &b); os.IsNotExist(err) {
		fmt.Fprintln(out, "Backlog: created from project requirements on first run")
	} else if err != nil {
		fmt.Fprintln(out, "Backlog:", err)
		fmt.Fprintln(out, "  Fix: repair the task file or remove it to plan from requirements")
		code = 1
	} else if err = b.Validate(); err != nil {
		fmt.Fprintln(out, "Backlog:", err)
		fmt.Fprintln(out, "  Fix: repair the reported task or dependency in", c.Todo)
		code = 1
	} else {
		fmt.Fprintf(out, "Backlog: %d tasks, %d ready\n", len(b.Tasks), len(b.Ready()))
	}
	return code
}
func inspect(ctx context.Context, c Config, out io.Writer) error {
	_, dir := projectPaths(c)
	path := filepath.Join(dir, "state.db")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no saved run; start looper first")
	}
	store, err := OpenStore(path)
	if err != nil {
		return err
	}
	defer store.Close()
	if c.Command == "status" {
		s, err := store.Load()
		if err != nil {
			return err
		}
		if s == nil {
			return fmt.Errorf("no saved run")
		}
		if c.JSON {
			return json.NewEncoder(out).Encode(s)
		}
		done, blocked, total := 0, 0, 0
		for _, t := range s.Backlog.Tasks {
			if t.Marker() {
				continue
			}
			total++
			if t.Status == "done" {
				done++
			}
			if t.Status == "blocked" {
				blocked++
			}
			if c.ListStatus != "" && t.Status == c.ListStatus {
				fmt.Fprintf(out, "%s  %s  %s\n", sanitize(t.ID), t.Status, sanitize(t.Title))
			}
		}
		if DetectMode(os.Stdin, out, c.Plain) == ModeInteractive {
			NewPresenter(ModeInteractive, out, func() int { return terminalWidth(fileOf(out)) }).StatusBoard(s, path)
			return nil
		}
		fmt.Fprintf(out, "Backlog: %d/%d complete | %d ready | %d blocked\n", done, total, len(s.Backlog.Ready()), blocked)
		for _, t := range s.Backlog.Tasks {
			if t.Status == "blocked" {
				fmt.Fprintf(out, "%s  %s\n", t.ID, t.Title)
				for _, b := range t.Blockers {
					fmt.Fprintln(out, "  "+b)
				}
			}
		}
		fmt.Fprintln(out, "State:", path)
		return nil
	}
	var after int64
	follow := NewPresenter(DetectMode(os.Stdin, out, c.Plain), out, func() int { return terminalWidth(fileOf(out)) })
	for {
		events, err := store.Events(after)
		if err != nil {
			return err
		}
		for _, e := range events {
			if c.JSON {
				if err = json.NewEncoder(out).Encode(e); err != nil {
					return err
				}
			} else if follow.Mode == ModeInteractive {
				follow.StoredEvent(e)
			} else {
				fmt.Fprintln(out, e.Message)
			}
			after = e.ID
		}
		if !c.Follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}
