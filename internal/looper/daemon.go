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
	"strings"
	"syscall"
	"time"
)

// startExecutable resolves the daemon child binary; tests substitute it.
var startExecutable = os.Executable

// daemonReady is the structured readiness message sent through the startup
// pipe. Success means controller ownership is established; planning may still
// be pending.
type daemonReady struct {
	Status  string `json:"status"`
	Session string `json:"session,omitempty"`
	Error   string `json:"error,omitempty"`
	Log     string `json:"log,omitempty"`
}

const daemonStartupTimeout = 30 * time.Second

func daemonLogPath(dir string) string { return filepath.Join(dir, "daemon.log") }

// StartDetached spawns a daemon child in a new session, redirects its output
// to runtime/daemon.log, and waits for readiness through a pipe that is not a
// lifetime pipe: the child keeps running after the parent exits. The parent
// terminates and reaps only the child it created.
func StartDetached(c Config, out, stderr io.Writer) int {
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	configPath := filepath.Join(dir, "daemon-config.json")
	data, err := json.Marshal(c)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	if err = atomicWrite(configPath, data, 0600); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	exe, err := startExecutable()
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	logFile, err := os.OpenFile(daemonLogPath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	defer logFile.Close()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		readyRead.Close()
		readyWrite.Close()
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	defer devNull.Close()
	cmd := exec.Command(exe, "__daemon", configPath)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// fd 3 carries the readiness message. It is deliberately not a lifetime
	// pipe: closing it never kills the controller.
	cmd.ExtraFiles = []*os.File{readyWrite}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		readyRead.Close()
		readyWrite.Close()
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	readyWrite.Close()
	defer readyRead.Close()
	ready := make(chan daemonReady, 1)
	go func() {
		raw, _ := io.ReadAll(readyRead)
		var msg daemonReady
		json.Unmarshal(raw, &msg)
		if msg.Status == "" && len(raw) > 0 {
			msg = daemonReady{Status: "error", Error: strings.TrimSpace(string(raw)), Log: daemonLogPath(dir)}
		}
		ready <- msg
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case msg := <-ready:
		if msg.Status != "ok" {
			fmt.Fprintf(stderr, "The controller did not start: %s\n", msg.Error)
			if msg.Log != "" {
				fmt.Fprintln(stderr, "Log:", msg.Log)
			}
			syscall.Kill(cmd.Process.Pid, syscall.SIGTERM)
			<-exited
			return 1
		}
		fmt.Fprintln(out, "Controller detached, session", msg.Session)
		fmt.Fprintln(out, "Logs:", daemonLogPath(dir))
		fmt.Fprintln(out, "Attach with: looper tui")
		return 0
	case err := <-exited:
		// The child may have delivered its readiness message just before
		// exiting; prefer the structured report over the bare exit status.
		select {
		case msg := <-ready:
			if msg.Status != "ok" {
				fmt.Fprintf(stderr, "The controller did not start: %s\n", msg.Error)
				if msg.Log != "" {
					fmt.Fprintln(stderr, "Log:", msg.Log)
				}
				return 1
			}
			// A quick --once run can finish before the parent reads the
			// pipe; the controller owned the repository and reported ready.
			fmt.Fprintln(out, "Controller detached, session", msg.Session)
			fmt.Fprintln(out, "Logs:", daemonLogPath(dir))
			return 0
		case <-time.After(2 * time.Second):
			fmt.Fprintf(stderr, "The controller exited during startup: %v\n", err)
			fmt.Fprintln(stderr, "Log:", daemonLogPath(dir))
			return 1
		}
	case <-time.After(daemonStartupTimeout):
		fmt.Fprintln(stderr, "The controller did not report readiness in time; terminating it.")
		syscall.Kill(cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
			<-exited
		}
		fmt.Fprintln(stderr, "Log:", daemonLogPath(dir))
		return 1
	}
}

// RunDaemon is the detached child entry point. It reads the validated
// configuration the parent resolved, acquires the repository lock, and
// reports readiness or a structured error through the readiness writer
// before running the engine loop.
func RunDaemon(configPath string, ready io.Writer) int {
	fail := func(err error) int {
		msg, _ := json.Marshal(daemonReady{Status: "error", Error: err.Error(), Log: "runtime/daemon.log"})
		fmt.Fprintln(ready, string(msg))
		return 1
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fail(err)
	}
	os.Remove(configPath)
	var c Config
	if err = json.Unmarshal(data, &c); err != nil {
		return fail(err)
	}
	c.Detached = true
	gitTimeout = c.GitTimeout
	_, dir := projectPaths(c)
	// Signals install before the lock so an early drain request stays
	// pending until the loop reaches its first boundary.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	installControllerSignals(c, cancel)
	logFile, err := os.OpenFile(daemonLogPath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fail(err)
	}
	defer logFile.Close()
	e, lock, err := NewEngine(c, logFile)
	if err != nil {
		return fail(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	msg, _ := json.Marshal(daemonReady{Status: "ok", Session: e.controller.Session})
	fmt.Fprintln(ready, string(msg))
	code, err := e.Run(ctx)
	if err != nil {
		fmt.Fprintln(logFile, "Error:", err)
	}
	return code
}

// installControllerSignals routes SIGTERM to the drain request used by
// `looper stop` without cancelling the execution context; SIGINT stays an
// explicit cancellation with retained work. The two are not interchangeable.
func installControllerSignals(c Config, cancel context.CancelFunc) {
	_, dir := projectPaths(c)
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for s := range sigs {
			if s == syscall.SIGTERM {
				_ = atomicWrite(filepath.Join(dir, "stop"), []byte("drain\n"), 0600)
				continue
			}
			cancel()
		}
	}()
}

// daemonTail returns the last lines of the daemon log for diagnostics.
func daemonTail(dir string, lines int) string {
	b, err := os.ReadFile(daemonLogPath(dir))
	if err != nil {
		return ""
	}
	content := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(content) > lines {
		content = content[len(content)-lines:]
	}
	return strings.Join(content, "\n")
}
