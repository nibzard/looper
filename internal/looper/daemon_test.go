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
	// SIGTERM drains: the stop file appears and the context stays live.
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "stop")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dir, "stop")); err != nil {
		t.Fatal("SIGTERM must write the drain request")
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
	deadline := time.Now().Add(20 * time.Second)
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
		deadline := time.Now().Add(5 * time.Second)
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
