//go:build darwin

package looper

import "golang.org/x/sys/unix"

func platformExecLimit() (int, error) {
	limit, err := unix.SysctlUint32("kern.argmax")
	return int(limit), err
}
