package looper

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ControllerInfo identifies the process that owns a repository. The
// repository lock stays authoritative: metadata locates the controller, and a
// process ID alone is never trusted because IDs get reused.
type ControllerInfo struct {
	Version int    `json:"version"`
	Session string `json:"session"`
	PID     int    `json:"pid"`
	Root    string `json:"root"`
	Mode    string `json:"mode"`
	Started string `json:"started"`
}

// ControllerModeForeground marks an interactive run; detached marks a daemon.
const (
	ControllerVersion        = 1
	ControllerModeForeground = "foreground"
	ControllerModeDetached   = "detached"
)

func controllerPath(dir string) string { return filepath.Join(dir, "controller.json") }

// publishController writes controller metadata. It runs while the repository
// lock is held; only the current session may remove its own record.
func publishController(dir, mode string) (*ControllerInfo, error) {
	info := &ControllerInfo{
		Version: ControllerVersion, Session: identifier("run"),
		PID: os.Getpid(), Root: mustWD(), Mode: mode,
		Started: time.Now().UTC().Format(time.RFC3339),
	}
	return info, writeJSON(controllerPath(dir), info)
}

func mustWD() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// removeController removes only the metadata belonging to the given session.
// A record from another session is never touched, even on shared machines.
func removeController(dir, session string) {
	info, err := readController(dir)
	if err != nil || info == nil || info.Session != session {
		return
	}
	os.Remove(controllerPath(dir))
	os.Remove(filepath.Join(dir, "looper.pid"))
}

// readController loads controller metadata. A missing, unreadable, or
// inconsistent record returns an error that callers report as unknown
// ownership instead of guessing.
func readController(dir string) (*ControllerInfo, error) {
	var info ControllerInfo
	err := readJSON(controllerPath(dir), &info)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no controller metadata in %s", dir)
	}
	if err != nil {
		return nil, fmt.Errorf("controller metadata is unreadable: %w", err)
	}
	if info.Version != ControllerVersion || info.Session == "" || info.PID <= 1 {
		return nil, fmt.Errorf("controller metadata is inconsistent")
	}
	return &info, nil
}

// controllerAlive checks the recorded process against the recorded start
// time, so a reused process ID does not impersonate a dead controller.
func controllerAlive(info *ControllerInfo) bool {
	if info == nil || info.PID <= 1 {
		return false
	}
	out, err := runIdentityCommand(info.PID)
	if err != nil || strings.TrimSpace(out) == "" {
		return false
	}
	return true
}

func runIdentityCommand(pid int) (string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err == nil {
		// The field after the comm parenthetical holds the start time.
		text := string(b)
		if i := strings.LastIndex(text, ")"); i >= 0 && i+2 <= len(text) {
			return strings.Fields(text[i+2:])[19], nil
		}
	}
	return "", fmt.Errorf("process %d is not running", pid)
}

// signalController sends a signal to the recorded process group only when
// the recorded session still matches and the process is alive.
func signalController(dir string, sig syscall.Signal) error {
	info, err := readController(dir)
	if err != nil {
		return err
	}
	if !controllerAlive(info) {
		return fmt.Errorf("recorded controller session %s is not running", info.Session)
	}
	if err = syscall.Kill(info.PID, sig); err != nil {
		return fmt.Errorf("cannot signal controller session %s: %w", info.Session, err)
	}
	return nil
}
