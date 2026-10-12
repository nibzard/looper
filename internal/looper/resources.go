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
	"strconv"
	"strings"
	"syscall"
	"time"
)

const resourceLeaseFile = "resources.json"
const resourceCleanupFile = "cleanup-resources.sh"

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
			e.event("resource-cleanup-error", fmt.Sprintf("Cannot read resource ownership in %s: %v", path, err))
			return nil
		}
		if err := validResourceLease(lease, filepath.Dir(path)); err != nil {
			e.event("resource-cleanup-error", err.Error())
			return nil
		}
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
			e.event("resource-cleanup-error", fmt.Sprintf("Temporary resources remain in %s: %v. Cleanup retries at the next completion or restart.", lease.Directory, err))
		}
		return nil
	})
	if err != nil {
		e.event("resource-cleanup-error", err.Error())
	}
}
