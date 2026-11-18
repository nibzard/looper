package looper

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const resourceLeaseFile = "resources.json"
const resourceCleanupFile = "cleanup-resources.sh"

// cleanupSummaryInterval bounds how long one artifact's repeated identical
// sweep warning stays silent (T138, T143): after a warning was reported in
// full, every interval-th identical repeat journals one summary event with
// the repeat count, and the session's final sweep summarizes the repeats
// that have not reached the interval yet.
const cleanupSummaryInterval = 10

// sweepWarningKind distinguishes which sweep warning a group counts, because
// a summary states what actually repeated: a cleanup run that failed (T138),
// an ownership record the sweep could not read, or one that failed validation
// (T143). The cleanup summary keeps its pinned wording.
type sweepWarningKind uint8

const (
	warnCleanupFailure sweepWarningKind = iota
	warnLeaseRead
	warnLeaseInvalid
)

// cleanupFailureGroup counts one artifact's identical sweep warnings between
// reports: the warning class the count belongs to, the error text the count
// belongs to, how often that error repeated in this controller session, and
// the count a warning or summary already announced.
type cleanupFailureGroup struct {
	kind     sweepWarningKind
	cause    string
	count    int
	reported int
}

// ResourceLease records the operation that owns registered temporary resources.
// Cleanup scripts must be idempotent and check resource ownership before removal.
type ResourceLease struct {
	Version   int    `json:"version"`
	Directory string `json:"directory"`
	Prefix    string `json:"prefix"`
	Session   string `json:"session,omitempty"`
	PID       int    `json:"pid"`
	Identity  string `json:"identity"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

type resourceContextKey struct{}
type resourceSessionKey struct{}

func beginResourceOperation(ctx context.Context, dir string) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return ctx, nil, err
	}
	if lease, ok := ctx.Value(resourceContextKey{}).(*ResourceLease); ok && lease.Directory == dir {
		return ctx, func() {}, nil
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return ctx, nil, err
	}
	var previous ResourceLease
	if err = readJSON(filepath.Join(dir, resourceLeaseFile), &previous); err == nil {
		if err = validResourceLease(previous, dir); err != nil {
			return ctx, nil, err
		}
		if previous.Status == "active" && resourceOwnerAlive(previous) {
			return ctx, nil, fmt.Errorf("resource operation is still active: %s", dir)
		}
		if previous.Status == "cleaned" {
			if err = os.Remove(filepath.Join(dir, resourceCleanupFile)); err != nil && !os.IsNotExist(err) {
				return ctx, nil, err
			}
		}
	} else if !os.IsNotExist(err) {
		return ctx, nil, err
	}
	identity, err := resourceProcessIdentity(os.Getpid())
	if err != nil {
		return ctx, nil, err
	}
	session, _ := ctx.Value(resourceSessionKey{}).(string)
	lease := &ResourceLease{Version: 1, Directory: dir, Prefix: resourceEnv(dir)["LOOPER_RESOURCE_PREFIX"], Session: session, PID: os.Getpid(), Identity: identity, Status: "active"}
	if err = writeJSON(filepath.Join(dir, resourceLeaseFile), lease); err != nil {
		return ctx, nil, err
	}
	ctx = context.WithValue(ctx, resourceContextKey{}, lease)
	finish := func() {
		if ctx.Err() != nil {
			lease.Status = "retained"
			lease.Error = ctx.Err().Error()
			_ = writeJSON(filepath.Join(dir, resourceLeaseFile), lease)
			return
		}
		_ = cleanResourceLease(lease)
	}
	return ctx, finish, nil
}

func resourceOwnerAlive(lease ResourceLease) bool {
	identity, err := resourceProcessIdentity(lease.PID)
	if err != nil {
		// An unreadable process is not proof that its owner has exited.
		return !errors.Is(syscall.Kill(lease.PID, 0), syscall.ESRCH)
	}
	return identity == lease.Identity
}

func resourceProcessIdentity(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		return runIdentityCommand(pid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("cannot identify resource owner %d", pid)
	}
	return strings.TrimSpace(string(data)), nil
}

func cleanResourceLease(lease *ResourceLease) error {
	return cleanResourceLeaseWithin(lease, 30*time.Second)
}

func validResourceLease(lease ResourceLease, dir string) error {
	if lease.Version != 1 || lease.Directory != dir || !filepath.IsAbs(dir) || lease.PID <= 1 || lease.Identity == "" || lease.Prefix != resourceEnv(dir)["LOOPER_RESOURCE_PREFIX"] {
		return fmt.Errorf("invalid resource ownership record: %s", filepath.Join(dir, resourceLeaseFile))
	}
	switch lease.Status {
	case "active", "retained", "cleanup_pending", "cleaned":
		return nil
	default:
		return fmt.Errorf("unknown resource ownership status in %s", filepath.Join(dir, resourceLeaseFile))
	}
}

func cleanResourceLeaseWithin(lease *ResourceLease, timeout time.Duration) error {
	path := filepath.Join(lease.Directory, resourceLeaseFile)
	if err := validResourceLease(*lease, lease.Directory); err != nil {
		return err
	}
	lease.Status, lease.Error = "cleanup_pending", ""
	if err := writeJSON(path, lease); err != nil {
		return err
	}
	script := filepath.Join(lease.Directory, resourceCleanupFile)
	info, err := os.Lstat(script)
	if os.IsNotExist(err) {
		err = nil
	} else if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("resource cleanup must be a regular file: %s", script)
	} else if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		err = runShell(ctx, lease.Directory, lease.Directory, "cleanup", "set -e\nsource \"$LOOPER_RESOURCE_CLEANUP_FILE\"", resourceEnv(lease.Directory))
	}
	if err != nil {
		lease.Error = err.Error()
	} else {
		lease.Status = "cleaned"
	}
	return errors.Join(err, writeJSON(path, lease))
}

// sweepResources runs on the owner goroutine at recovery and completion
// boundaries. Interrupted workers retain their resources until their retained
// implementation finishes. A live operation is never swept.
func (e *Engine) sweepResources() {
	err := filepath.WalkDir(e.Dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == "worktrees" {
			return filepath.SkipDir
		}
		if entry.Name() != resourceLeaseFile {
			return nil
		}
		var lease ResourceLease
		if err := readJSON(path, &lease); err != nil {
			if e.repeatSweepWarning(warnLeaseRead, filepath.Dir(path), err) {
				e.event("resource-cleanup-error", fmt.Sprintf("Cannot read resource ownership in %s: %v", path, err))
			}
			return nil
		}
		if err := validResourceLease(lease, filepath.Dir(path)); err != nil {
			if e.repeatSweepWarning(warnLeaseInvalid, filepath.Dir(path), err) {
				e.event("resource-cleanup-error", err.Error())
			}
			return nil
		}
		e.clearLeaseWarning(filepath.Dir(path))
		if lease.Status == "cleaned" || lease.Status == "active" && resourceOwnerAlive(lease) {
			return nil
		}
		if lease.Status == "active" || lease.Status == "retained" {
			for _, a := range e.State.Attempts {
				rel, err := filepath.Rel(a.Directory, lease.Directory)
				if err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && (a.Phase == "running" || a.Phase == "resume" || a.Phase == "verifying") {
					return nil
				}
			}
		}
		if err := cleanResourceLease(&lease); err != nil {
			if e.repeatSweepWarning(warnCleanupFailure, lease.Directory, err) {
				e.event("resource-cleanup-error", fmt.Sprintf("Temporary resources remain in %s: %v. Cleanup retries at the next completion or restart.", lease.Directory, err))
				e.cleanupFailureEvent(lease, err)
			}
		} else {
			e.clearCleanupFailure(lease.Directory)
		}
		return nil
	})
	if err != nil {
		e.event("resource-cleanup-error", err.Error())
	}
}

// repeatSweepWarning groups one artifact's repeated identical warnings from
// the resource sweep (T138 cleanup failures, T143 lease read and validate
// warnings). It returns true when this warning must be reported in full: the
// artifact's first warning of the controller session, or its first warning
// after the error changed, because a changed cause is new information.
// Identical repeats return false and only count; every cleanupSummaryInterval
// repeats, and the session's final sweep through summarizeCleanupFailures,
// journal one summary event with the repeat count instead. The sweep keeps
// retrying at every boundary either way.
func (e *Engine) repeatSweepWarning(kind sweepWarningKind, dir string, err error) bool {
	if e.cleanupFailures == nil {
		e.cleanupFailures = map[string]*cleanupFailureGroup{}
	}
	group := e.cleanupFailures[dir]
	if group == nil || group.kind != kind || group.cause != err.Error() {
		e.cleanupFailures[dir] = &cleanupFailureGroup{kind: kind, cause: err.Error(), count: 1, reported: 1}
		return true
	}
	group.count++
	if group.count-group.reported >= cleanupSummaryInterval {
		e.event("resource-cleanup-summary", cleanupSummaryMessage(kind, dir, group.count, group.cause))
		group.reported = group.count
	}
	return false
}

// clearCleanupFailure forgets one artifact's failure group after its cleanup
// succeeded: the lease is cleaned, and a later failure of the same artifact is
// a new episode that reports on its own.
func (e *Engine) clearCleanupFailure(dir string) {
	delete(e.cleanupFailures, dir)
}

// clearLeaseWarning forgets one artifact's read or validate warning group once
// its ownership record reads and validates again: the record recovered, so a
// later problem of the same artifact is a new episode that reports on its own.
// Cleanup-failure groups survive here, because only a cleanup run that
// succeeded clears them.
func (e *Engine) clearLeaseWarning(dir string) {
	if group := e.cleanupFailures[dir]; group != nil && group.kind != warnCleanupFailure {
		delete(e.cleanupFailures, dir)
	}
}

// summarizeCleanupFailures closes the session's warning groups at shutdown:
// every artifact whose warning repeated since its last report journals one
// summary with the session's repeat count, so a run states each failing
// artifact once instead of once per retry.
func (e *Engine) summarizeCleanupFailures() {
	if len(e.cleanupFailures) == 0 {
		return
	}
	dirs := make([]string, 0, len(e.cleanupFailures))
	for dir := range e.cleanupFailures {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		group := e.cleanupFailures[dir]
		if group.count <= group.reported {
			continue
		}
		e.event("resource-cleanup-summary", cleanupSummaryMessage(group.kind, dir, group.count, group.cause))
		group.reported = group.count
	}
}

// cleanupSummaryMessage states one artifact's repeat count beside the error
// that repeated. The cleanup wording is pinned by tests (T138); the lease
// read and validate wordings name the record problem the same way their
// first warning does (T143).
func cleanupSummaryMessage(kind sweepWarningKind, dir string, count int, cause string) string {
	switch kind {
	case warnLeaseRead:
		return fmt.Sprintf("Cannot read resource ownership in %s after %d identical warnings this session: %s. The sweep retries at the next completion or restart.", dir, count, cause)
	case warnLeaseInvalid:
		return fmt.Sprintf("Cannot validate resource ownership in %s after %d identical warnings this session: %s. The sweep retries at the next completion or restart.", dir, count, cause)
	default:
		return fmt.Sprintf("Temporary resources remain in %s after %d identical cleanup failures this session: %s. Cleanup retries at the next completion or restart.", dir, count, cause)
	}
}
