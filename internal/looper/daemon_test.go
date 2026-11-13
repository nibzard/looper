package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// preparedRepo creates a repository with a finished state so a controller
// starts, skips review, and exits cleanly without contacting any agent.
func preparedRepo(t *testing.T, extraArgs ...string) Config {
	t.Helper()
	c := repoConfig(t, extraArgs...)
	todo := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "done", Tags: []string{"project-done"}},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	e.State.Finished = true
	e.State.ReviewedTip = e.State.Tip
	e.State.ReviewedAt = 1
	e.State.Integrated = 1
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	dir := e.Dir
	e.Store.Close()
	lock.Close()
	os.Remove(controllerPath(dir))
	clearView(dir)
	return c
}

// daemonChild runs the daemon entry point in-process with a buffered
// readiness writer.
func daemonChild(t *testing.T, c Config) (int, daemonReady) {
	t.Helper()
	configPath := filepath.Join(filepath.Dir(c.ProfilePath), "daemon-config.json")
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	var ready bytes.Buffer
	code := RunDaemon(configPath, &ready)
	var msg daemonReady
	json.Unmarshal(ready.Bytes(), &msg)
	return code, msg
}

func TestRunDaemonReadinessAndCleanup(t *testing.T) {
	c := preparedRepo(t, "--once")
	code, msg := daemonChild(t, c)
	if code != 0 {
		t.Fatalf("daemon exit %d: %+v", code, msg)
	}
	if msg.Status != "ok" || msg.Session == "" {
		t.Fatalf("readiness broken: %+v", msg)
	}
	_, dir := projectPaths(c)
	if _, err := os.Stat(daemonLogPath(dir)); err != nil {
		t.Fatal("daemon log missing")
	}
	// The exited controller removed only its own metadata.
	if _, err := os.Stat(controllerPath(dir)); !os.IsNotExist(err) {
		t.Fatal("the exiting controller must remove its own metadata")
	}
	if _, err := os.Stat(viewPath(dir)); !os.IsNotExist(err) {
		t.Fatal("the exiting controller must clear its view")
	}
}

func TestRunDaemonReportsLockFailure(t *testing.T) {
	c := preparedRepo(t)
	f, err := os.OpenFile(filepath.Join(c.Root, ".git", "looper.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }()
	code, msg := daemonChild(t, c)
	if code != 1 || msg.Status != "error" {
		t.Fatalf("lock failure must surface through readiness: %d %+v", code, msg)
	}
	if !strings.Contains(msg.Error, "already running") {
		t.Fatalf("lock error not surfaced: %q", msg.Error)
	}
}

func TestControllerSignals(t *testing.T) {
	c := repoConfig(t)
	_, dir := projectPaths(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	installControllerSignals(c, cancel)
	// SIGTERM drains: a request bound to this process appears and the
	// context stays live.
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stopRequestPath(dir)); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	req, _, err := readStopRequest(dir)
	if err != nil || req == nil {
		t.Fatalf("SIGTERM must write the drain request: %v", err)
	}
	if req.Session != "" || req.PID != os.Getpid() {
		t.Fatalf("the startup request must bind to this process, got %+v", req)
	}
	if id, err := runIdentityCommand(os.Getpid()); err == nil && req.Identity != id {
		t.Fatalf("the startup request must carry the process identity: %q vs %q", req.Identity, id)
	}
	if !req.targets("", nil) {
		t.Fatal("the process-bound request must target this process")
	}
	select {
	case <-ctx.Done():
		t.Fatal("SIGTERM must not cancel the execution context")
	default:
	}
	// SIGINT cancels with retained work.
	syscall.Kill(os.Getpid(), syscall.SIGINT)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("SIGINT must cancel")
	}
	os.Remove(stopRequestPath(dir))
}

// TestSigTermBeforeEngineDrainsAtFirstBoundary covers the startup overlap: a
// drain request written by the signal handler before controller identity is
// published still drains that controller at its first boundary, and the
// completed drain consumes the request.
func TestSigTermBeforeEngineDrainsAtFirstBoundary(t *testing.T) {
	c := preparedRepo(t, "--once")
	_, dir := projectPaths(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	installControllerSignals(c, cancel)
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stopRequestPath(dir)); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(stopRequestPath(dir)); err != nil {
		t.Fatal("SIGTERM must write the drain request during startup")
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	code, err := e.Run(ctx)
	if code != 0 || err != nil {
		t.Fatalf("startup drain after SIGTERM: %d %v", code, err)
	}
	if exit := lastShutdown(t, e.Store); exit.Reason != "drained" {
		t.Fatalf("the startup request was lost: %+v", exit)
	}
	if _, err = os.Stat(stopRequestPath(dir)); !os.IsNotExist(err) {
		t.Fatal("the completed drain left its request behind")
	}
}

var binaryOnce struct {
	path string
	err  error
	once sync.Once
}

// detachedBinary builds the real CLI once for process-level tests.
func detachedBinary(t *testing.T) string {
	t.Helper()
	binaryOnce.once.Do(func() {
		dir, err := os.MkdirTemp("", "looper-daemon-test")
		if err != nil {
			binaryOnce.err = err
			return
		}
		binaryOnce.path = filepath.Join(dir, "looper")
		cmd := exec.Command("go", "build", "-o", binaryOnce.path, "./cmd/looper")
		wd, _ := os.Getwd()
		cmd.Dir = filepath.Clean(filepath.Join(wd, "..", ".."))
		if out, err := cmd.CombinedOutput(); err != nil {
			binaryOnce.err = fmt.Errorf("go build: %v: %s", err, out)
		}
	})
	if binaryOnce.err != nil {
		t.Skipf("cannot build the CLI: %v", binaryOnce.err)
	}
	return binaryOnce.path
}

// The full detached path: the parent spawns a setsid child, the child
// outlives the parent's pipe, finishes its run, and removes its own metadata.
func TestStartDetachedRealBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("process-level daemon test")
	}
	bin := detachedBinary(t)
	c := preparedRepo(t, "start", "--once")
	orig := startExecutable
	startExecutable = func() (string, error) { return bin, nil }
	t.Cleanup(func() { startExecutable = orig })
	var out, stderr bytes.Buffer
	code := StartDetached(c, &out, &stderr)
	if code != 0 {
		t.Fatalf("detached start failed: %d %s %s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "session") || !strings.Contains(out.String(), "daemon.log") {
		t.Fatalf("start output missing session or log path: %q", out.String())
	}
	_, dir := projectPaths(c)
	deadline := time.Now().Add(testBudget(20 * time.Second))
	for time.Now().Before(deadline) {
		if _, err := os.Stat(controllerPath(dir)); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the detached controller never finished or never removed its metadata")
}

func TestStartDetachedDoubleStart(t *testing.T) {
	if testing.Short() {
		t.Skip("process-level daemon test")
	}
	bin := detachedBinary(t)
	c := preparedRepo(t, "start")
	f, err := os.OpenFile(filepath.Join(c.Root, ".git", "looper.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }()
	orig := startExecutable
	startExecutable = func() (string, error) { return bin, nil }
	t.Cleanup(func() { startExecutable = orig })
	var out, stderr bytes.Buffer
	code := StartDetached(c, &out, &stderr)
	if code != 1 {
		t.Fatalf("double start must fail: %d %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "did not start") || !strings.Contains(stderr.String(), "already running") {
		t.Fatalf("readiness error missing: %q", stderr.String())
	}
}

// The detached child never reads terminal input (T011): the parent asks any
// form, and the child is born with /dev/null on standard input and the daemon
// log on output, so a daemon can never consume the operator's terminal.
func TestStartDetachedChildNeverReadsTerminalInput(t *testing.T) {
	if testing.Short() {
		t.Skip("process-level daemon test")
	}
	bin := detachedBinary(t)
	c := preparedRepo(t, "start")
	c.Poll = 10 * time.Millisecond
	c.CodexBin, c.ClaudeBin = "/bin/false", "/bin/false"
	orig := startExecutable
	startExecutable = func() (string, error) { return bin, nil }
	t.Cleanup(func() { startExecutable = orig })
	_, dir := projectPaths(c)
	t.Cleanup(func() {
		if info, err := readController(dir); err == nil {
			syscall.Kill(info.PID, syscall.SIGINT)
		}
		deadline := time.Now().Add(testBudget(5 * time.Second))
		for time.Now().Before(deadline) {
			if _, err := os.Stat(controllerPath(dir)); os.IsNotExist(err) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("detached controller did not stop")
	})
	var out, stderr bytes.Buffer
	// The test process reads /dev/null itself, which would make an inherited
	// standard input indistinguishable from the required one. Substitute a
	// plain file so the pin can tell the parent's stream from /dev/null.
	probe, err := os.CreateTemp("", "looper-daemon-stdin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { probe.Close(); os.Remove(probe.Name()) })
	oldStdin := os.Stdin
	os.Stdin = probe
	defer func() { os.Stdin = oldStdin }()
	if code := StartDetached(c, &out, &stderr); code != 0 {
		t.Fatalf("start failed: %s", stderr.String())
	}
	os.Stdin = oldStdin
	info, err := readController(dir)
	if err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", info.PID))
	if err != nil {
		t.Skipf("cannot inspect the child's file descriptors: %v", err)
	}
	if link != os.DevNull {
		t.Fatalf("the detached child must read %s, not %q", os.DevNull, link)
	}
	stdout, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/1", info.PID))
	if err != nil {
		t.Skipf("cannot inspect the child's file descriptors: %v", err)
	}
	if !strings.Contains(stdout, "daemon.log") {
		t.Fatalf("the detached child must write the daemon log, not %q", stdout)
	}
}

func TestStartDetachedWhileControllerStaysRunning(t *testing.T) {
	bin := detachedBinary(t)
	c := preparedRepo(t, "start")
	c.Poll = 10 * time.Millisecond
	c.CodexBin, c.ClaudeBin = "/bin/false", "/bin/false"
	orig := startExecutable
	startExecutable = func() (string, error) { return bin, nil }
	defer func() { startExecutable = orig }()
	_, dir := projectPaths(c)
	t.Cleanup(func() {
		if info, err := readController(dir); err == nil {
			syscall.Kill(info.PID, syscall.SIGINT)
		}
		deadline := time.Now().Add(testBudget(5 * time.Second))
		for time.Now().Before(deadline) {
			if _, err := os.Stat(controllerPath(dir)); os.IsNotExist(err) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("detached controller did not stop")
	})
	var out, stderr bytes.Buffer
	started := time.Now()
	if code := StartDetached(c, &out, &stderr); code != 0 {
		t.Fatalf("start failed: %s", stderr.String())
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("readiness waited for controller exit")
	}
	info, err := readController(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(info.PID, 0); err != nil {
		t.Fatalf("controller exited with its parent: %v", err)
	}
}

// A controller launched by looper start must record detached mode, so its
// session metadata distinguishes the daemon from an interactive run.
func TestStartDetachedRecordsDetachedMode(t *testing.T) {
	if testing.Short() {
		t.Skip("process-level daemon test")
	}
	bin := detachedBinary(t)
	c := preparedRepo(t, "start")
	c.Poll = 10 * time.Millisecond
	c.CodexBin, c.ClaudeBin = "/bin/false", "/bin/false"
	orig := startExecutable
	startExecutable = func() (string, error) { return bin, nil }
	t.Cleanup(func() { startExecutable = orig })
	_, dir := projectPaths(c)
	t.Cleanup(func() {
		if info, err := readController(dir); err == nil {
			syscall.Kill(info.PID, syscall.SIGINT)
		}
		deadline := time.Now().Add(testBudget(5 * time.Second))
		for time.Now().Before(deadline) {
			if _, err := os.Stat(controllerPath(dir)); os.IsNotExist(err) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("detached controller did not stop")
	})
	var out, stderr bytes.Buffer
	if code := StartDetached(c, &out, &stderr); code != 0 {
		t.Fatalf("start failed: %s", stderr.String())
	}
	info, err := readController(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode != ControllerModeDetached {
		t.Fatalf("controller started by looper start must record mode %q, got %q", ControllerModeDetached, info.Mode)
	}
}

// The durable session record keeps the detached mode after the controller
// exits and removes its controller.json.
func TestRunDaemonRecordsDetachedSessionMode(t *testing.T) {
	c := preparedRepo(t, "--once")
	code, msg := daemonChild(t, c)
	if code != 0 {
		t.Fatalf("daemon exit %d: %+v", code, msg)
	}
	if msg.Status != "ok" || msg.Session == "" {
		t.Fatalf("readiness broken: %+v", msg)
	}
	_, dir := projectPaths(c)
	s, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events, err := s.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	var recorded *ControllerInfo
	for _, ev := range events {
		if ev.Kind != "session" {
			continue
		}
		var info ControllerInfo
		if err = json.Unmarshal(ev.Data, &info); err != nil {
			t.Fatalf("session event data is not controller metadata: %v", err)
		}
		if info.Session == msg.Session {
			recorded = &info
		}
	}
	if recorded == nil {
		t.Fatalf("no durable session record for daemon session %s", msg.Session)
	}
	if recorded.Mode != ControllerModeDetached {
		t.Fatalf("daemon session %s must record mode %q, got %q", msg.Session, ControllerModeDetached, recorded.Mode)
	}
}
