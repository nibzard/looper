//go:build linux || darwin

package looper

import (
	"fmt"
	"strconv"
	"strings"
)

// ExecLimitError identifies an entry without exposing its contents.
type ExecLimitError struct {
	Entry        string
	Bytes, Limit int
}

func (e *ExecLimitError) Error() string {
	return fmt.Sprintf("%s uses %d bytes; execution limit is %d bytes", e.Entry, e.Bytes, e.Limit)
}

// LaunchError is separate from a target that started and exited unsuccessfully.
type LaunchError struct {
	Stage string
	Err   error
}

func (e *LaunchError) Error() string {
	if e.Stage == "protocol" {
		return "helper launch report failed; target start is unconfirmed: " + e.Err.Error()
	}
	return e.Stage + " launch failed; target never started: " + e.Err.Error()
}
func (e *LaunchError) Unwrap() error { return e.Err }

func checkExecSize(args, environment []string, limit int) error {
	total := (len(args) + len(environment) + 2) * (strconv.IntSize / 8)
	for group, entries := range [][]string{args, environment} {
		for i, value := range entries {
			name := fmt.Sprintf("argument %d", i)
			if group == 1 {
				key, _, _ := strings.Cut(value, "=")
				name = "environment key " + key
			}
			size := len(value) + 1
			if size >= 128<<10 {
				return &ExecLimitError{name, size, 128 << 10}
			}
			total += size
		}
	}
	// Leave space for interpreter arguments and platform execution overhead.
	limit -= 16 << 10
	if total >= limit {
		return &ExecLimitError{"arguments and environment", total, limit}
	}
	return nil
}
