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
// pipe. Success means controller ownership is established and the saved run
// state is publicly queryable (AX-022); planning may still be pending.
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
	// Startup plan input is rejected in the parent: a spawned child that
	// dies at initialization would otherwise report a detached start that
	// never runs any work.
	if err := validatePlanFiles(c.Plans); err != nil {
		return emitError(c, err, out, stderr)
	}
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	configPath := filepath.Join(dir, "daemon-config.json")
	data, err := json.Marshal(c)
	if err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	if err = atomicWrite(configPath, data, 0600); err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	exe, err := startExecutable()
	if err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	logFile, err := os.OpenFile(daemonLogPath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	defer logFile.Close()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		readyRead.Close()
		readyWrite.Close()
		return emitError(c, persistenceError(err.Error()), out, stderr)
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
		return emitError(c, persistenceError(err.Error()), out, stderr)
	}
	readyWrite.Close()
	defer readyRead.Close()
	ready := make(chan daemonReady, 1)
	go func() {
		var msg daemonReady
		if err := json.NewDecoder(readyRead).Decode(&msg); err != nil {
			msg = daemonReady{Status: "error", Error: err.Error(), Log: daemonLogPath(dir)}
		}
		ready <- msg
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case msg := <-ready:
		if msg.Status != "ok" {
			p := startupFailure("The controller did not start: "+msg.Error, msg.Log)
			syscall.Kill(cmd.Process.Pid, syscall.SIGTERM)
			<-exited
			return emitError(c, p, out, stderr)
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
				return emitError(c, startupFailure("The controller did not start: "+msg.Error, msg.Log), out, stderr)
			}
			// A quick --once run can finish before the parent reads the
			// pipe; the controller owned the repository and reported ready.
			fmt.Fprintln(out, "Controller detached, session", msg.Session)
			fmt.Fprintln(out, "Logs:", daemonLogPath(dir))
			return 0
		case <-time.After(2 * time.Second):
			return emitError(c, startupFailure(fmt.Sprintf("The controller exited during startup: %v", err), daemonLogPath(dir)), out, stderr)
		}
	case <-time.After(daemonStartupTimeout):
		p := startupFailure("The controller did not report readiness in time; it was terminated.", daemonLogPath(dir))
		p.Retry = retryYes
		syscall.Kill(cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
			<-exited
		}
		return emitError(c, p, out, stderr)
	}
}

// RunDaemon is the detached child entry point. It reads the validated
// configuration the parent resolved, acquires the repository lock, and
// reports readiness or a structured error through the readiness writer
// before running the engine loop.
func RunDaemon(configPath string, ready io.Writer) int {
	// The parent writes the configuration inside the runtime directory, so
	// its location names the directory even before the configuration parses.
	fail := func(err error) int {
		msg, _ := json.Marshal(daemonReady{Status: "error", Error: err.Error(), Log: daemonLogPath(filepath.Dir(configPath))})
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
	// Saved run state becomes queryable before readiness is reported (AX-022):
	// a caller that sees a successful start can query the run at once. A seed
	// failure — an unreadable or invalid task file, a failing snapshot — is a
	// startup failure, so it reaches the parent as structured evidence through
	// the pipe instead of a detached controller that dies after success was
	// reported. The seeded run never started, so its ownership records go now.
	if err = e.seedStartupState(); err != nil {
		session := e.controller.Session
		clearView(dir)
		removeController(dir, session)
		e.releaseStopRequests()
		return fail(err)
	}
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
// The request binds to this process: to the controller session once
// controller metadata names it, or to the process identity while startup has
// not published a session yet, so the engine adopts it at its first boundary.
func installControllerSignals(c Config, cancel context.CancelFunc) {
	_, dir := projectPaths(c)
	identity, _ := runIdentityCommand(os.Getpid())
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for s := range sigs {
			if s == syscall.SIGTERM {
				req := stopRequest{PID: os.Getpid(), Identity: identity}
				if info, err := readController(dir); err == nil && info.PID == os.Getpid() {
					req.Session = info.Session
				}
				_ = writeStopRequest(dir, req)
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
