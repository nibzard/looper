package looper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var Version = "dev"

const help = `Looper — continuous local development

Usage: looper [run] [task-file] [options]
       looper status [--json]
       looper stop
       looper tail [--follow]
       looper doctor

Run again to resume retained work. The default run waits for new work when idle.
--once finishes after the current goal is checked or all remaining work is blocked.

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
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if c.Command == "doctor" {
		return doctor(c, out)
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
	e, lock, err := NewEngine(c, out)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	defer lock.Close()
	defer e.Store.Close()
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
	code := 0
	for _, name := range []string{"git", "bash", map[string]string{"codex": c.CodexBin, "claude": c.ClaudeBin}[c.Agent], map[string]string{"codex": c.CodexBin, "claude": c.ClaudeBin}[c.ReviewAgent]} {
		path, err := exec.LookPath(name)
		if err != nil {
			fmt.Fprintln(out, "Missing:", name)
			code = 1
		} else {
			fmt.Fprintln(out, "Found:", path)
		}
	}
	var b Backlog
	if err := readJSON(c.Todo, &b); os.IsNotExist(err) {
		fmt.Fprintln(out, "Backlog: created from project requirements on first run")
	} else if err != nil {
		fmt.Fprintln(out, "Backlog:", err)
		code = 1
	} else if err = b.Validate(); err != nil {
		fmt.Fprintln(out, "Backlog:", err)
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
				fmt.Fprintf(out, "%s  %s  %s\n", t.ID, t.Status, t.Title)
			}
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
