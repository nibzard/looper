//go:build linux

package looper

import (
	"os"
	"syscall"
)

func platformExecLimit() (int, error) {
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_STACK, &limit); err != nil {
		return 0, err
	}
	// Linux limits strings to a quarter of the soft stack limit, with a
	// floor of 32 pages and a ceiling of three quarters of an 8 MiB stack.
	total := min(limit.Cur/4, uint64(6<<20))
	total = max(total, uint64(32*os.Getpagesize()))
	return int(total), nil
}
