package looper

import (
	"encoding/json"
	"errors"
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
	Version        int        `json:"version"`
	Session        string     `json:"session"`
	PID            int        `json:"pid"`
	Root           string     `json:"root"`
	Mode           string     `json:"mode"`
	Started        string     `json:"started"`
	Build          *BuildInfo `json:"build,omitempty"`
	BootstrapAgent string     `json:"bootstrap_agent,omitempty"`
	// Capabilities advertises the contract versions and request types this
	// controller accepts. The record is optional so metadata from older
	// controllers keeps parsing; readers apply the compatibility policy in
	// docs/AGENT-CONTRACT-MIGRATION.md when it is absent.
	Capabilities *ControllerCapabilities `json:"capabilities,omitempty"`
}

// ControllerCapabilities is the capability record published with live
// controller identity. RequestTypes uses the public request type names of
// the capabilities contract, not the internal envelope type names.
type ControllerCapabilities struct {
	ContractVersions []int          `json:"contract_versions"`
	RequestTypes     []string       `json:"request_types"`
	Limits           map[string]int `json:"limits,omitempty"`
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
func publishController(dir, mode, bootstrapAgent string) (*ControllerInfo, error) {
	build := currentBuild()
	info := &ControllerInfo{
		Version: ControllerVersion, Session: identifier("run"),
		PID: os.Getpid(), Root: mustWD(), Mode: mode,
		Started:        time.Now().UTC().Format(time.RFC3339Nano),
		Build:          &build,
		BootstrapAgent: bootstrapAgent,
		Capabilities:   newControllerCapabilities(),
	}
	if err := writeJSON(controllerPath(dir), info); err != nil {
		return nil, err
	}
	// Optional compatibility record for tooling that only reads a pidfile;
	// the lock and controller.json stay authoritative.
	_ = atomicWrite(filepath.Join(dir, "looper.pid"), []byte(strconv.Itoa(info.PID)+"\n"), 0600)
	return info, nil
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
			fields := strings.Fields(text[i+2:])
			if len(fields) > 19 && fields[0] != "Z" && fields[0] != "X" {
				return fields[19], nil
			}
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

// stopRequestVersion is the schema version of a scoped drain request.
const stopRequestVersion = 1

// stopRequest binds a drain request to the controller it asks to stop.
// Session scopes the request to one controller session, the identity `looper
// stop` reads from controller metadata. A request written before controller
// identity is published carries the process identity instead: the PID and,
// where the platform provides it, the process start identity. A request with
// neither binding is the legacy unscoped form written by older versions.
type stopRequest struct {
	Version  int    `json:"version,omitempty"`
	Session  string `json:"session,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Identity string `json:"identity,omitempty"`
}

// processStartedAt stamps this process's start during package
// initialization, before any code can write or read a request. It anchors
// the legacy rule below: an unscoped request that arrived after this moment
// was deliverable to this controller, an earlier one predates it.
var processStartedAt = time.Now()

func stopRequestPath(dir string) string { return filepath.Join(dir, "stop") }

// writeStopRequest publishes a drain request. All writers bind the request to
// the controller they want stopped; an empty request is the legacy unscoped
// form, honored only by a controller that is alive when it arrives.
func writeStopRequest(dir string, req stopRequest) error {
	req.Version = stopRequestVersion
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return atomicWrite(stopRequestPath(dir), append(data, '\n'), 0600)
}

// readStopRequest returns the published drain request and its file
// information. A missing request returns nil without an error. Bytes that do
// not parse, including the legacy "drain" line and unreadable entries such as
// a directory, count as unscoped requests so old files stay observable.
func readStopRequest(dir string) (*stopRequest, os.FileInfo, error) {
	path := stopRequestPath(dir)
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(path)
	var req stopRequest
	if err == nil {
		err = json.Unmarshal(data, &req)
	}
	if err != nil {
		return &stopRequest{}, info, nil
	}
	return &req, info, nil
}

// targets reports whether a drain request asks this controller to stop.
//
//   - A session-scoped request matches only its exact session. A request for
//     any other controller is never adopted, whatever process IDs coincide.
//   - A process-scoped request matches the process that wrote it during its
//     own startup, before controller identity existed. The recorded process
//     start identity, when both sides have one, rules out a reused ID.
//   - A legacy unscoped request cannot name its owner. It counts only when it
//     arrived after this process started, the earliest moment a request for
//     this controller could have been written; one that predates the process
//     was left for a controller that is already gone.
func (r *stopRequest) targets(session string, info os.FileInfo) bool {
	switch {
	case r == nil:
		return false
	case r.Session != "":
		return r.Session == session
	case r.PID != 0:
		if r.PID != os.Getpid() {
			return false
		}
		if r.Identity == "" {
			return true
		}
		identity, err := runIdentityCommand(r.PID)
		return err == nil && identity == r.Identity
	default:
		return info != nil && !info.ModTime().Before(processStartedAt)
	}
}

// requestTargets reports whether the published request binds to this engine's
// controller.
func (e *Engine) requestTargets(req *stopRequest, info os.FileInfo) bool {
	session := ""
	if e.controller != nil {
		session = e.controller.Session
	}
	return req.targets(session, info)
}

// drainingNow reports whether a published stop request asks this controller
// to drain. Requests bound to another controller never drain this one.
func (e *Engine) drainingNow() bool {
	req, info, err := readStopRequest(e.Dir)
	if err != nil {
		return false
	}
	return e.requestTargets(req, info)
}

// stopRequestID derives the durable receipt identity of one stop request
// from its binding. The stop transport has no envelope identifier; the
// binding names the controller the request targeted, which is unique per
// outstanding request because one file holds one request (AX-009).
func stopRequestID(req *stopRequest) string {
	if req == nil {
		return "stop"
	}
	switch {
	case req.Session != "":
		return "stop-" + req.Session
	case req.PID != 0:
		return fmt.Sprintf("stop-pid-%d", req.PID)
	default:
		return "stop-unscoped"
	}
}

// sweepStopRequests discards a request no live controller can answer. It runs
// under the repository lock at startup, where only this controller can be
// live: a request that does not target it was left by a controller that is
// gone, and honoring it would silently drain this run instead. Requests for
// this controller survive and drain at the first boundary. Every discarded
// request records a rejected receipt, so the discard stays a queryable
// outcome instead of a silent deletion.
func (e *Engine) sweepStopRequests() {
	req, info, err := readStopRequest(e.Dir)
	if err != nil || req == nil || e.requestTargets(req, info) {
		// No request, an unreadable one, or a request this controller must
		// still honor at its first boundary.
		return
	}
	if err = os.Remove(stopRequestPath(e.Dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		// The request cannot drain this run either way; the next sweep
		// retries the removal.
		return
	}
	detail := ""
	switch {
	case req.Session != "":
		detail = fmt.Sprintf("the stop request targeted controller session %s, which is gone; it was never delivered and cannot affect this run", req.Session)
		e.event("recovery", fmt.Sprintf("Discarded a stop request for controller session %s; that controller is gone and the request cannot affect this run.", req.Session))
	case req.PID != 0:
		detail = fmt.Sprintf("the stop request targeted controller process %d, which is gone; it was never delivered and cannot affect this run", req.PID)
		e.event("recovery", fmt.Sprintf("Discarded a stop request for process %d; that controller is gone and the request cannot affect this run.", req.PID))
	default:
		detail = "the stop request predates this controller; it was never delivered and cannot affect this run"
		e.event("recovery", "Discarded a stop request that predates this controller; it cannot affect this run.")
	}
	e.recordStopRequest(req, RequestRejected, "", detail)
	if err = e.save(); err != nil {
		e.err = err
	}
}

// releaseStopRequests removes requests this controller could still have
// answered. It runs during shutdown while the repository lock is still held:
// a request that arrives after the final boundary can no longer be honored,
// and leaving it behind would drain the next run instead. The removal
// records a rejected receipt, so the undelivered request keeps a queryable
// outcome.
func (e *Engine) releaseStopRequests() {
	req, info, err := readStopRequest(e.Dir)
	if err != nil || !e.requestTargets(req, info) {
		return
	}
	// Best effort: a surviving request is scoped to this dead session, so
	// the next startup sweep still discards it.
	if os.Remove(stopRequestPath(e.Dir)) == nil {
		e.recordStopRequest(req, RequestRejected, "", "the controller ended before it could honor this stop request; the work stays retained for the next run")
		if saveErr := e.persistRequestReceipts(); saveErr != nil && e.err == nil {
			e.err = saveErr
		}
	}
}
