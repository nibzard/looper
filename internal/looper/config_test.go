package looper

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// parseAt runs Parse inside a temporary repository root so projectPaths and
// the task-file resolution have predictable inputs.
func parseAt(t *testing.T, args ...string) (Config, error) {
	t.Helper()
	root := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	t.Setenv("LOOPER_BASE_DIR", filepath.Join(root, "base"))
	c, err := Parse(args)
	if err == nil {
		if c.Root != root {
			t.Fatalf("root %s != %s", c.Root, root)
		}
	}
	return c, err
}

func clearSettingEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"LOOPER_ITER_SCHEDULE", "LOOPER_REVIEW_AGENT", "LOOPER_JOBS", "LOOPER_CHECK_JOBS",
		"CODEX_MODEL", "CLAUDE_MODEL", "CODEX_REASONING_EFFORT", "CODEX_SMART_MODEL",
		"CODEX_SMART_REASONING_EFFORT", "LOOPER_VERIFY_COMMAND", "LOOPER_SETUP_COMMAND",
		"LOOPER_REVIEW_EVERY", "LOOPER_MAX_REPAIRS", "LOOPER_RATE_LIMIT_MAX",
		"LOOPER_POLL_SECONDS", "LOOPER_RATE_LIMIT_DELAY", "LOOPER_TIMEOUT_SECONDS",
		"LOOPER_ASSESS_COMMAND", "LOOPER_HOOK", "CODEX_BIN", "CLAUDE_BIN", "LOOPER_NO_PROMPT",
	} {
		val, ok := os.LookupEnv(k)
		os.Unsetenv(k)
		t.Cleanup(func() {
			if ok {
				os.Setenv(k, val)
			} else {
				os.Unsetenv(k)
			}
		})
	}
}

func TestParseDefaults(t *testing.T) {
	clearSettingEnv(t)
	c, err := parseAt(t)
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "codex" || c.ReviewAgent != "codex" {
		t.Fatalf("agents %s/%s", c.Agent, c.ReviewAgent)
	}
	if c.Model != "" || c.ClaudeModel != "" || c.Effort != "" {
		t.Fatalf("models not empty: %s %s %s", c.Model, c.ClaudeModel, c.Effort)
	}
	if c.Verify != "" || c.Setup != "" {
		t.Fatalf("commands not empty: %s %s", c.Verify, c.Setup)
	}
	if c.ReviewEvery != 5 || c.MaxRepairs != 6 || c.RetryLimit != 12 {
		t.Fatalf("counts %d %d %d", c.ReviewEvery, c.MaxRepairs, c.RetryLimit)
	}
	if c.Poll != 30*time.Second || c.RetryDelay != 60*time.Second {
		t.Fatalf("durations %s %s", c.Poll, c.RetryDelay)
	}
	if c.Jobs < 1 {
		t.Fatalf("auto jobs %d", c.Jobs)
	}
	if c.Checks != 1 {
		t.Fatalf("checks %d", c.Checks)
	}
	if c.Todo != filepath.Join(c.Root, "to-do.json") {
		t.Fatalf("todo %s", c.Todo)
	}
}

func TestParseAgentSelection(t *testing.T) {
	clearSettingEnv(t)
	c, err := parseAt(t, "--agent", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "claude" || c.ReviewAgent != "claude" {
		t.Fatalf("agent fallback broken: %s/%s", c.Agent, c.ReviewAgent)
	}
	c, err = parseAt(t, "--agent", "claude", "--review-agent", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "claude" || c.ReviewAgent != "codex" {
		t.Fatalf("explicit review agent broken: %s/%s", c.Agent, c.ReviewAgent)
	}
	c, err = parseAt(t, "--all", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "claude" || c.ReviewAgent != "claude" {
		t.Fatalf("--all broken: %s/%s", c.Agent, c.ReviewAgent)
	}
	if _, err = parseAt(t, "--agent", "gemini"); err == nil {
		t.Fatal("unsupported agent accepted")
	}
}

func TestParseInterleave(t *testing.T) {
	clearSettingEnv(t)
	c, err := parseAt(t, "--interleave")
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "claude" || c.ReviewAgent != "codex" {
		t.Fatalf("interleave broken: %s/%s", c.Agent, c.ReviewAgent)
	}
	// An explicit review agent survives --interleave in any order.
	c, err = parseAt(t, "--interleave", "--review-agent", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if c.ReviewAgent != "claude" {
		t.Fatalf("interleave overrode explicit review agent: %s", c.ReviewAgent)
	}
	c, err = parseAt(t, "--review-agent", "claude", "--interleave")
	if err != nil {
		t.Fatal(err)
	}
	if c.ReviewAgent != "claude" {
		t.Fatalf("interleave overrode earlier review agent: %s", c.ReviewAgent)
	}
	// A later --agent or --all still wins over --interleave, in order.
	c, err = parseAt(t, "--interleave", "--agent", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "codex" || c.ReviewAgent != "codex" {
		t.Fatalf("interleave must not override a later agent flag: %s/%s", c.Agent, c.ReviewAgent)
	}
	c, err = parseAt(t, "--interleave", "--all", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "codex" || c.ReviewAgent != "codex" {
		t.Fatalf("interleave must not override a later all flag: %s/%s", c.Agent, c.ReviewAgent)
	}
}

func TestParseModels(t *testing.T) {
	clearSettingEnv(t)
	c, err := parseAt(t, "--model", "gpt-x")
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "gpt-x" || c.ClaudeModel != "gpt-x" {
		t.Fatalf("--model must set both agents: %s/%s", c.Model, c.ClaudeModel)
	}
	t.Setenv("CODEX_MODEL", "codex-env")
	t.Setenv("CLAUDE_MODEL", "claude-env")
	c, err = parseAt(t)
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "codex-env" || c.ClaudeModel != "claude-env" {
		t.Fatalf("model envs broken: %s/%s", c.Model, c.ClaudeModel)
	}
	// Flags override the environment per setting.
	c, err = parseAt(t, "--model", "flag-model")
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "flag-model" || c.ClaudeModel != "flag-model" {
		t.Fatalf("model flag precedence broken: %s/%s", c.Model, c.ClaudeModel)
	}
}

func TestParseSmart(t *testing.T) {
	clearSettingEnv(t)
	c, err := parseAt(t, "--smart")
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "gpt-6-astra" || c.Effort != "high" {
		t.Fatalf("smart defaults broken: %s/%s", c.Model, c.Effort)
	}
	// Smart overrides an explicit --model, as it always has.
	c, err = parseAt(t, "--model", "gpt-x", "--smart")
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "gpt-6-astra" {
		t.Fatalf("smart must override model: %s", c.Model)
	}
	t.Setenv("CODEX_SMART_MODEL", "smart-env")
	t.Setenv("CODEX_SMART_REASONING_EFFORT", "minimal")
	c, err = parseAt(t, "--smart")
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "smart-env" || c.Effort != "minimal" {
		t.Fatalf("smart envs broken: %s/%s", c.Model, c.Effort)
	}
}

func TestParseJobs(t *testing.T) {
	clearSettingEnv(t)
	c, err := parseAt(t, "--jobs", "3")
	if err != nil {
		t.Fatal(err)
	}
	if c.Jobs != 3 {
		t.Fatalf("jobs flag broken: %d", c.Jobs)
	}
	t.Setenv("LOOPER_JOBS", "2")
	c, err = parseAt(t)
	if err != nil {
		t.Fatal(err)
	}
	if c.Jobs != 2 {
		t.Fatalf("jobs env broken: %d", c.Jobs)
	}
	c, err = parseAt(t, "--jobs", "4")
	if err != nil {
		t.Fatal(err)
	}
	if c.Jobs != 4 {
		t.Fatalf("jobs precedence broken: %d", c.Jobs)
	}
	auto := max(1, min(4, runtime.NumCPU()/2))
	c, err = parseAt(t, "--jobs", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if c.Jobs != auto {
		t.Fatalf("auto jobs %d != %d", c.Jobs, auto)
	}
	for _, bad := range []string{"0", "-1", "x"} {
		if _, err = parseAt(t, "--jobs", bad); err == nil {
			t.Fatalf("jobs %q accepted", bad)
		}
	}
	c, err = parseAt(t, "--check-jobs", "2")
	if err != nil {
		t.Fatal(err)
	}
	if c.Checks != 2 {
		t.Fatalf("check-jobs broken: %d", c.Checks)
	}
	if _, err = parseAt(t, "--check-jobs", "0"); err == nil {
		t.Fatal("check-jobs 0 accepted")
	}
}

func TestParseEnvironmentAgent(t *testing.T) {
	clearSettingEnv(t)
	t.Setenv("LOOPER_ITER_SCHEDULE", "claude")
	t.Setenv("LOOPER_REVIEW_AGENT", "codex")
	c, err := parseAt(t)
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "claude" || c.ReviewAgent != "codex" {
		t.Fatalf("agent envs broken: %s/%s", c.Agent, c.ReviewAgent)
	}
	c, err = parseAt(t, "--agent", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "codex" || c.ReviewAgent != "codex" {
		t.Fatalf("agent flag precedence broken: %s/%s", c.Agent, c.ReviewAgent)
	}
}

func TestParseCommandsAndErrors(t *testing.T) {
	clearSettingEnv(t)
	for _, tc := range []struct {
		args    []string
		command string
	}{
		{nil, "run"},
		{[]string{"run"}, "run"},
		{[]string{"status"}, "status"},
		{[]string{"--tail"}, "tail"},
		{[]string{"--ls", "blocked"}, "status"},
	} {
		c, err := parseAt(t, tc.args...)
		if err != nil {
			t.Fatal(err)
		}
		if c.Command != tc.command {
			t.Fatalf("command %q != %q", c.Command, tc.command)
		}
	}
	c, err := parseAt(t, "--ls", "blocked")
	if err != nil {
		t.Fatal(err)
	}
	if c.ListStatus != "blocked" {
		t.Fatalf("ls filter %q", c.ListStatus)
	}
	if _, err = parseAt(t, "--nonsense"); err == nil {
		t.Fatal("unknown option accepted")
	}
	if _, err = parseAt(t, "--jobs"); err == nil {
		t.Fatal("missing value accepted")
	}
	if _, err = parseAt(t, "--max-iterations", "-3"); err == nil {
		t.Fatal("negative iterations accepted")
	}
	c, err = parseAt(t, "--max-iterations", "5", "--review-every", "0")
	if err != nil {
		t.Fatal(err)
	}
	if c.Limit != 5 || c.ReviewEvery != 0 {
		t.Fatalf("iteration options broken: %d/%d", c.Limit, c.ReviewEvery)
	}
	c, err = parseAt(t, "tasks.json")
	if err != nil {
		t.Fatal(err)
	}
	if c.Todo != filepath.Join(c.Root, "tasks.json") {
		t.Fatalf("positional todo %s", c.Todo)
	}
	if !strings.HasSuffix(c.BaseDir, "base") {
		t.Fatalf("base dir %s", c.BaseDir)
	}
}

func TestParseVerifyAndSetupCommands(t *testing.T) {
	clearSettingEnv(t)
	t.Setenv("LOOPER_VERIFY_COMMAND", "go vet ./...")
	t.Setenv("LOOPER_SETUP_COMMAND", "make setup")
	c, err := parseAt(t)
	if err != nil {
		t.Fatal(err)
	}
	if c.Verify != "go vet ./..." || c.Setup != "make setup" {
		t.Fatalf("command envs broken: %s/%s", c.Verify, c.Setup)
	}
}

func TestCgroupHeadroomExcludesReclaimableCache(t *testing.T) {
	dir := t.TempDir()
	limit := filepath.Join(dir, "memory.max")
	usage := filepath.Join(dir, "memory.current")
	stat := filepath.Join(dir, "memory.stat")
	writeFile := func(p, content string) {
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(limit, "4294967296") // 4 GiB
	writeFile(usage, "3019898880") // 2.8 GiB used
	writeFile(stat, "anon 314572800\ninactive_file 2415919104\n")
	// Working set is 2.8 GiB minus 2.25 GiB cache, so the headroom is
	// the limit minus that working set.
	if got := cgroupHeadroom(limit, usage, stat); got != 4294967296-(3019898880-2415919104) {
		t.Fatalf("cache not excluded: %d", got)
	}
	// A missing stat file keeps the raw usage bound.
	if got := cgroupHeadroom(limit, usage, filepath.Join(dir, "none")); got != 4294967296-3019898880 {
		t.Fatalf("missing stat changed the bound: %d", got)
	}
}
