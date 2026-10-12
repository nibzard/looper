package looper

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecSizeAccountsForPointersAndTerminators(t *testing.T) {
	args, environment := []string{"agent", "argument"}, []string{"KEY=value"}
	size := (len(args) + len(environment) + 2) * (strconv.IntSize / 8)
	for _, s := range append(args, environment...) {
		size += len(s) + 1
	}
	if err := checkExecSize(args, environment, size+(16<<10)+1); err != nil {
		t.Fatal(err)
	}
	var limit *ExecLimitError
	if err := checkExecSize(args, environment, size+(16<<10)); !errors.As(err, &limit) {
		t.Fatalf("boundary accepted: %v", err)
	}
	if limit.Bytes != size {
		t.Fatalf("size %d, want %d", limit.Bytes, size)
	}
}

func TestLaunchGuardDoesNotCreateLog(t *testing.T) {
	for _, environment := range []bool{false, true} {
		t.Run(strconv.FormatBool(environment), func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "events.log")
			args := []string{"sh", "-c", "touch child-ran"}
			extra := map[string]string{}
			if environment {
				extra["LARGE_SECRET"] = strings.Repeat("private", 20000)
			} else {
				args = append(args, strings.Repeat("x", 128<<10))
			}
			err := runProcess(context.Background(), dir, log, args, nil, extra)
			var limit *ExecLimitError
			if !errors.As(err, &limit) {
				t.Fatalf("want size failure: %v", err)
			}
			if environment && (!strings.Contains(err.Error(), "LARGE_SECRET") || strings.Contains(err.Error(), "private")) {
				t.Fatal(err)
			}
			for _, path := range []string{log, filepath.Join(dir, "child-ran")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("unexpected artifact: %s", path)
				}
			}
		})
	}
}

func TestMissingTargetAndExit127AreDistinct(t *testing.T) {
	dir := t.TempDir()
	err := runProcess(context.Background(), dir, filepath.Join(dir, "missing.log"), []string{filepath.Join(dir, "missing")}, nil, nil)
	var launch *LaunchError
	if !errors.As(err, &launch) || launch.Stage != "target" {
		t.Fatalf("missing binary: %v", err)
	}
	err = runProcess(context.Background(), dir, filepath.Join(dir, "exit.log"), []string{"sh", "-c", "exit 127"}, nil, nil)
	var exit *exec.ExitError
	if errors.As(err, &launch) || !errors.As(err, &exit) || exit.ExitCode() != 127 {
		t.Fatalf("target exit: %v", err)
	}
}

func TestReducedStackExecutionLimit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux stack limit")
	}
	if os.Getenv("LOOPER_TEST_SMALL_STACK") == "1" {
		var stack syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_STACK, &stack); err != nil {
			t.Fatal(err)
		}
		stack.Cur = 512 << 10
		if err := syscall.Setrlimit(syscall.RLIMIT_STACK, &stack); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		err := runProcess(context.Background(), dir, filepath.Join(dir, "log"), []string{"sh", strings.Repeat("a", 80<<10), strings.Repeat("b", 80<<10)}, nil, nil)
		var limit *ExecLimitError
		if !errors.As(err, &limit) || limit.Entry != "arguments and environment" {
			t.Fatalf("reduced limit not enforced: %v", err)
		}
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestReducedStackExecutionLimit$")
	cmd.Env = append(os.Environ(), "LOOPER_TEST_SMALL_STACK=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("subprocess: %v\n%s", err, output)
	}
}

func TestClaudePromptUsesStdin(t *testing.T) {
	for _, planning := range []bool{false, true} {
		t.Run(strconv.FormatBool(planning), func(t *testing.T) {
			dir, work := t.TempDir(), t.TempDir()
			bin := filepath.Join(dir, "claude")
			script := `#!/bin/sh
printf '%s\n' "$@" > "$LOOPER_ARTIFACT_DIR/args.txt"
cat > "$LOOPER_ARTIFACT_DIR/stdin.txt"
printf '%s\n' '{"type":"result","result":"{\"tasks\":[]}"}'
`
			if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			prompt := strings.Repeat("prompt content\n", 20000)
			var progress func(string, string)
			if planning {
				progress = func(string, string) {}
			}
			_, err := (Agent{Config{ClaudeBin: bin, RetryLimit: 1}}).run(context.Background(), "claude", work, dir, prompt, false, progress)
			if err != nil {
				t.Fatal(err)
			}
			input, err := os.ReadFile(filepath.Join(dir, "stdin.txt"))
			if err != nil || string(input) != prompt {
				t.Fatalf("stdin differs: %v", err)
			}
			args, _ := os.ReadFile(filepath.Join(dir, "args.txt"))
			if strings.Contains(string(args), "prompt content") {
				t.Fatal("prompt appears in argv")
			}
			var record map[string]any
			if err := readJSON(filepath.Join(dir, "invocation.json"), &record); err != nil {
				t.Fatal(err)
			}
			if record["prompt_transport"] != "stdin" || record["prompt_bytes"] != float64(len(prompt)) || record["prompt_target_exceeded"] != true {
				t.Fatalf("metadata: %v", record)
			}
		})
	}
}

func TestAgentLaunchErrorPointsToInputs(t *testing.T) {
	for _, kind := range []string{"codex", "zcode"} {
		dir := t.TempDir()
		prompt := "hello"
		if kind == "zcode" {
			prompt = strings.Repeat("x", 128<<10)
		}
		_, err := (Agent{Config{CodexBin: filepath.Join(dir, "missing"), ZcodeBin: filepath.Join(dir, "missing"), RetryLimit: 2}}).Run(context.Background(), kind, dir, filepath.Join(dir, "run"), prompt, false)
		var agent *AgentError
		if !errors.As(err, &agent) || agent.Kind != "invalid_request" {
			t.Fatalf("%s: %v", kind, err)
		}
		for _, want := range []string{"never started", "prompt.txt", "invocation.json"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatal(err)
			}
		}
		if strings.Contains(err.Error(), "events-") {
			t.Fatal(err)
		}
	}
}

func TestShellFilePreservesCommandBehavior(t *testing.T) {
	dir, work := t.TempDir(), t.TempDir()
	command := `test "$0" = bash && test "$#" = 0 && test "$PWD" = "$EXPECTED_CWD" || exit 9
printf '%s\n' 'quotes: "$var" ` + "`" + `literal` + "`" + `' > result.txt
` + strings.Repeat("# padding\n", 20000) + `exit 7`
	err := runShell(context.Background(), work, dir, "verify", command, map[string]string{"EXPECTED_CWD": work})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("shell behavior: %v", err)
	}
	result, err := os.ReadFile(filepath.Join(work, "result.txt"))
	if err != nil || string(result) != "quotes: \"$var\" `literal`\n" {
		t.Fatalf("quoted result: %q %v", result, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "verify.sh"))
	if string(data) != command+"\n" {
		t.Fatal("command record changed")
	}
}

func TestLaunchReportFailureAndCancellation(t *testing.T) {
	for _, mode := range []string{"missing", "hang"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			timeout := 5 * time.Second
			if mode == "hang" {
				timeout = 200 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			err := runProcess(ctx, dir, filepath.Join(dir, "events.log"), []string{"sh", "-c", "exit 0"}, nil, map[string]string{"LOOPER_TEST_LAUNCH_REPORT": mode})
			if mode == "hang" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			} else {
				var launch *LaunchError
				if !errors.As(err, &launch) || launch.Stage != "protocol" || !strings.Contains(err.Error(), "unconfirmed") {
					t.Fatalf("missing report: %v", err)
				}
			}
		})
	}
}

func TestWorkerLaunchFailureRetainsWork(t *testing.T) {
	e, _, _ := assessRunEngine(t, "", 0)
	e.Config.Agent = "claude"
	e.Config.ReviewAgent = "codex"
	e.Config.ClaudeBin = filepath.Join(t.TempDir(), "missing-agent")
	code, err := e.Run(context.Background())
	var agent *AgentError
	if code != 1 || !errors.As(err, &agent) || agent.Kind != "invalid_request" {
		t.Fatalf("launch exit: %d %v", code, err)
	}
	if len(e.State.Attempts) != 1 || e.State.Attempts[0].Phase != "running" || e.State.Attempts[0].Repairs != 0 || e.State.Backlog.Tasks[0].Status != "doing" {
		t.Fatal("launch failure consumed repairs or blocked retained work")
	}
	var rows int
	if err := e.Store.db.QueryRow("SELECT count(*) FROM usage_reports WHERE artifact LIKE ?", e.State.Attempts[0].Directory+"/%").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("failed launch recorded usage: %d %v", rows, err)
	}
}

func TestFeedbackIgnoresOperationPathsWhenComparingRepairs(t *testing.T) {
	var evidence string
	for _, name := range []string{"first", "second"} {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(dir, "verify.log")
		if err := os.WriteFile(log, []byte("same observed failure"), 0600); err != nil {
			t.Fatal(err)
		}
		feedback := commandFeedback("Combined verification failed:", "exit 1", &commandError{filepath.Join(dir, "verify.sh"), errors.New("exit status 1")}, log)
		if evidence != "" && failureEvidence(feedback) != evidence {
			t.Fatal("operation paths changed the repeated-failure signature")
		}
		evidence = failureEvidence(feedback)
	}
}

func TestShellErrorsKeepStableRepairEvidence(t *testing.T) {
	var previous string
	for i := 0; i < 2; i++ {
		dir, work := t.TempDir(), t.TempDir()
		command := "looper_test_missing_command"
		err := runShell(context.Background(), work, dir, "verify", command, nil)
		if err == nil {
			t.Fatal("missing command succeeded")
		}
		feedback := commandFeedback("Combined verification failed:", command, err, filepath.Join(dir, "verify.log"))
		current := failureEvidence(feedback)
		if previous != "" && current != previous {
			t.Fatalf("script paths changed failure identity:\n%s\n%s", previous, current)
		}
		previous = current
	}
}
