package looper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type commandError struct {
	Path string
	Err  error
}

func (e *commandError) Error() string { return fmt.Sprintf("command script %s: %v", e.Path, e.Err) }
func (e *commandError) Unwrap() error { return e.Err }

// runShell keeps the command body out of arguments and preserves stdin for
// commands. Shift removes the script argument before loading the command.
func runShell(ctx context.Context, work, dir, name, command string, extra map[string]string) error {
	path, err := filepath.Abs(filepath.Join(dir, name+".sh"))
	if err != nil {
		return &LaunchError{"command script", err}
	}
	if err := atomicWrite(path, []byte(command+"\n"), 0600); err != nil {
		return &LaunchError{"command script", err}
	}
	err = runProcess(ctx, work, filepath.Join(dir, name+".log"),
		[]string{"bash", "-lc", `looper_command_file=$1; shift; source "$looper_command_file"`, "bash", path}, nil, extra)
	if err != nil {
		return &commandError{path, err}
	}
	return nil
}

func commandFeedback(label, command string, err error, log string) string {
	var shell *commandError
	if errors.As(err, &shell) {
		err = shell.Err
	}
	feedback := label + "\nCommand: " + command + "\nError: " + err.Error() + "\nArtifacts: " + filepath.Dir(log)
	if _, statErr := os.Stat(log); statErr == nil {
		// Bash prefixes errors with the sourced script path. Keep that path
		// in the raw log, but compare repeated repairs by the script name.
		script := strings.TrimSuffix(log, ".log") + ".sh"
		output := strings.ReplaceAll(logTail(log), script, filepath.Base(script))
		feedback += "\nObserved output:\n" + output + "\nArtifacts: full log " + log
	}
	return feedback
}
