//go:build linux || darwin

package looper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

func lockRepository(common string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(common, "looper.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("Looper is already running in this repository")
	}
	return f, nil
}

// ownsRepository reports whether another process holds the repository lock,
// without holding it. An unopenable lock file counts as owned: something is
// wrong with the repository and starting a controller is unsafe.
func ownsRepository(common string) bool {
	f, err := os.OpenFile(filepath.Join(common, "looper.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return true
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

func cleanEnv(extra map[string]string) []string {
	skip := map[string]bool{"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_COMMON_DIR": true, "GIT_INDEX_FILE": true, "CLAUDECODE": true}
	var out []string
	for _, v := range os.Environ() {
		k, _, _ := strings.Cut(v, "=")
		if !skip[k] {
			if _, ok := extra[k]; !ok {
				out = append(out, v)
			}
		}
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// RunChild watches a pipe owned only by the coordinator. A killed coordinator
// closes the pipe, so the helper terminates its whole process group.
func RunChild(args []string) int {
	if len(args) == 0 {
		return 2
	}
	life := os.NewFile(3, "coordinator-lifetime")
	if life == nil {
		return 2
	}
	go func() { io.Copy(io.Discard, life); syscall.Kill(-syscall.Getpgrp(), syscall.SIGKILL) }()
	report := os.NewFile(4, "target-launch")
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	defer report.Close()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		_ = json.NewEncoder(report).Encode(launchReport{Error: err.Error()})
		return 127
	}
	if err := json.NewEncoder(report).Encode(launchReport{Started: true}); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return 126
	}
	report.Close()
	if err := cmd.Wait(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			return e.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 127
	}
	return 0
}

type launchReport struct {
	Started bool   `json:"started"`
	Error   string `json:"error,omitempty"`
}

func runProcess(ctx context.Context, cwd, logfile string, args []string, input io.Reader, extra map[string]string) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if extra["LOOPER_ATTEMPT_ID"] != "" {
		// Use a short root even when the inherited TMPDIR or runtime path is
		// too long for Unix sockets. MkdirTemp creates a private directory.
		tmp, err := os.MkdirTemp("/tmp", "looper-")
		if err != nil {
			return &LaunchError{"helper", fmt.Errorf("create private TMPDIR: %w", err)}
		}
		defer os.RemoveAll(tmp)
		environment := make(map[string]string, len(extra)+1)
		for k, v := range extra {
			environment[k] = v
		}
		environment["TMPDIR"] = tmp
		extra = environment
	}
	exe, err := os.Executable()
	if err != nil {
		return &LaunchError{"helper", err}
	}
	cmd := exec.Command(exe, append([]string{"__exec"}, args...)...)
	cmd.Dir = cwd
	cmd.Env = cleanEnv(extra)
	// Materialize the environment used for execution before measuring it.
	cmd.Env = cmd.Environ()
	limit, err := platformExecLimit()
	if err != nil {
		return &LaunchError{"limits", err}
	}
	for _, vector := range [][]string{cmd.Args, args} {
		if err = checkExecSize(vector, cmd.Env, limit); err != nil {
			return &LaunchError{"limits", err}
		}
	}
	if err := os.MkdirAll(filepath.Dir(logfile), 0700); err != nil {
		return &LaunchError{"helper", err}
	}
	out, err := os.OpenFile(logfile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return &LaunchError{"helper", err}
	}
	defer out.Close()
	read, write, err := os.Pipe()
	if err != nil {
		return &LaunchError{"helper", err}
	}
	defer read.Close()
	defer write.Close()
	launchRead, launchWrite, err := os.Pipe()
	if err != nil {
		return &LaunchError{"helper", err}
	}
	defer launchRead.Close()
	defer launchWrite.Close()
	cmd.Stdin = input
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.ExtraFiles = []*os.File{read, launchWrite}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		return &LaunchError{"helper", err}
	}
	launchWrite.Close()
	read.Close()
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	launched := make(chan error, 1)
	go func() {
		var report launchReport
		err := json.NewDecoder(io.LimitReader(launchRead, 16<<10)).Decode(&report)
		if err != nil {
			launched <- &LaunchError{"protocol", err}
		} else if !report.Started && report.Error == "" {
			launched <- &LaunchError{"protocol", errors.New("missing launch status")}
		} else if !report.Started {
			launched <- &LaunchError{"target", errors.New(report.Error)}
		} else {
			launched <- nil
		}
	}()
	select {
	case err = <-launched:
		if err != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			cmd.Wait()
			return err
		}
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
		return ctx.Err()
	}
	identity, _ := runIdentityCommand(cmd.Process.Pid)
	record := ProcessObservation{PID: cmd.Process.Pid, Identity: identity, Started: time.Now().UTC().Format(time.RFC3339Nano), Running: true}
	var processID string
	store, _ := ctx.Value(timingContextKey{}).(*Store)
	if store != nil {
		processID, err = store.beginProcess(logfile, record)
		if err != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			cmd.Wait()
			return fmt.Errorf("record process start: %w", err)
		}
	}
	_ = writeJSON(logfile+".process.json", record)
	defer func() {
		record.Running = false
		record.Ended = time.Now().UTC().Format(time.RFC3339Nano)
		_ = writeJSON(logfile+".process.json", record)
		if store != nil {
			outcome := "success"
			if result != nil {
				outcome = "failed"
				if ctx.Err() != nil {
					outcome = "cancelled"
				}
			}
			result = errors.Join(result, store.endProcess(processID, record.Ended, outcome))
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
		return err
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return ctx.Err()
	}
}

type winsize struct{ rows, cols, x, y uint16 }

// terminalWidth reports the terminal width in cells, detected per call so
// resize events apply to the next rendered line. Zero means unknown.
func terminalWidth(f *os.File) int {
	if f == nil {
		return 0
	}
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.cols == 0 {
		return 0
	}
	return int(ws.cols)
}
