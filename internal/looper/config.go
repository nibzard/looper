package looper

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Root, Todo, BaseDir, Agent, ReviewAgent, CodexBin, ClaudeBin, Model, ClaudeModel, Effort, Verify, Setup, Assess, Hook string
	Command, ListStatus                                                                                                   string
	Jobs, Checks, Limit, ReviewEvery, MaxRepairs, RetryLimit                                                              int
	Poll, RetryDelay, Timeout                                                                                             time.Duration
	Once, Follow, JSON, Verbose, Smart, GitInit                                                                           bool
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
func integer(key string, def int) (int, error) {
	s := env(key, strconv.Itoa(def))
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%s must be a nonnegative integer", key)
	}
	return v, nil
}
func Parse(args []string) (Config, error) {
	root, err := os.Getwd()
	if err != nil {
		return Config{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Config{}, err
	}
	home, _ := os.UserHomeDir()
	c := Config{Root: root, Todo: "to-do.json", BaseDir: env("LOOPER_BASE_DIR", filepath.Join(home, ".looper")), Agent: env("LOOPER_ITER_SCHEDULE", "codex"), ReviewAgent: env("LOOPER_REVIEW_AGENT", ""), CodexBin: env("CODEX_BIN", "codex"), ClaudeBin: env("CLAUDE_BIN", "claude"), Model: env("CODEX_MODEL", ""), ClaudeModel: env("CLAUDE_MODEL", ""), Effort: env("CODEX_REASONING_EFFORT", ""), Verify: env("LOOPER_VERIFY_COMMAND", ""), Setup: env("LOOPER_SETUP_COMMAND", ""), Assess: env("LOOPER_ASSESS_COMMAND", ""), Hook: env("LOOPER_HOOK", ""), Command: "run", GitInit: env("LOOPER_GIT_INIT", "1") != "0"}
	for k, p := range map[string]*int{"MAX_ITERATIONS": &c.Limit, "LOOPER_REVIEW_EVERY": &c.ReviewEvery, "LOOPER_MAX_REPAIRS": &c.MaxRepairs, "LOOPER_RATE_LIMIT_MAX": &c.RetryLimit} {
		d := 0
		switch k {
		case "LOOPER_REVIEW_EVERY":
			d = 5
		case "LOOPER_MAX_REPAIRS":
			d = 6
		case "LOOPER_RATE_LIMIT_MAX":
			d = 12
		}
		v, e := integer(k, d)
		if e != nil {
			return c, e
		}
		*p = v
	}
	for k, p := range map[string]*time.Duration{"LOOPER_POLL_SECONDS": &c.Poll, "LOOPER_RATE_LIMIT_DELAY": &c.RetryDelay, "LOOPER_TIMEOUT_SECONDS": &c.Timeout} {
		d := 30
		switch k {
		case "LOOPER_RATE_LIMIT_DELAY":
			d = 60
		case "LOOPER_TIMEOUT_SECONDS":
			d = 0
		}
		v, e := integer(k, d)
		if e != nil {
			return c, e
		}
		*p = time.Duration(v) * time.Second
	}
	jobs := env("LOOPER_JOBS", "auto")
	checks := env("LOOPER_CHECK_JOBS", "1")
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func() (string, error) {
			i++
			if i >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			return args[i], nil
		}
		var v string
		switch a {
		case "run", "status", "stop", "doctor", "tail", "version":
			c.Command = a
		case "--help", "-h":
			c.Command = "help"
		case "--version":
			c.Command = "version"
		case "--doctor":
			c.Command = "doctor"
		case "--tail":
			c.Command = "tail"
		case "--follow":
			c.Follow = true
		case "--once":
			c.Once = true
		case "--json":
			c.JSON = true
		case "--verbose", "-v":
			c.Verbose = true
		case "--smart", "-s":
			c.Smart = true
		case "--interleave":
			c.Agent = "claude"
			if c.ReviewAgent == "" {
				c.ReviewAgent = "codex"
			}
		case "--jobs", "--check-jobs", "--agent", "--all", "--review-agent", "--model", "--effort", "--max-iterations", "--review-every", "--ls":
			v, err = value()
			if err != nil {
				return c, err
			}
			switch a {
			case "--jobs":
				jobs = v
			case "--check-jobs":
				checks = v
			case "--agent":
				c.Agent = v
			case "--all":
				c.Agent = v
				c.ReviewAgent = v
			case "--review-agent":
				c.ReviewAgent = v
			case "--model":
				c.Model = v
				c.ClaudeModel = v
			case "--effort":
				c.Effort = v
			case "--ls":
				c.Command = "status"
				c.ListStatus = v
			case "--max-iterations", "--review-every":
				n, e := strconv.Atoi(v)
				if e != nil || n < 0 {
					return c, fmt.Errorf("%s needs a nonnegative integer", a)
				}
				if a == "--max-iterations" {
					c.Limit = n
				} else {
					c.ReviewEvery = n
				}
			}
		default:
			if strings.HasPrefix(a, "-") {
				return c, fmt.Errorf("unknown option %s", a)
			}
			c.Todo = a
		}
	}
	if jobs == "auto" {
		c.Jobs = max(1, min(4, runtime.NumCPU()/2))
		if m := availableMemory(); m > 0 {
			c.Jobs = min(c.Jobs, max(1, int(m/(2<<30))))
		}
	} else {
		c.Jobs, err = strconv.Atoi(jobs)
		if err != nil || c.Jobs < 1 {
			return c, fmt.Errorf("jobs must be auto or a positive integer")
		}
	}
	c.Checks, err = strconv.Atoi(checks)
	if err != nil || c.Checks < 1 {
		return c, fmt.Errorf("check-jobs must be positive")
	}
	if c.Smart {
		c.Model = env("CODEX_SMART_MODEL", "gpt-6-astra")
		c.Effort = env("CODEX_SMART_REASONING_EFFORT", "high")
	}
	if c.ReviewAgent == "" {
		c.ReviewAgent = c.Agent
	}
	for _, a := range []string{c.Agent, c.ReviewAgent} {
		if a != "codex" && a != "claude" {
			return c, fmt.Errorf("unsupported agent %q; use codex or claude", a)
		}
	}
	if c.Poll <= 0 {
		c.Poll = time.Second
	}
	if c.RetryLimit < 1 {
		c.RetryLimit = 1
	}
	if !filepath.IsAbs(c.Todo) {
		c.Todo = filepath.Join(c.Root, c.Todo)
	}
	c.Todo = filepath.Clean(c.Todo)
	c.BaseDir, err = filepath.Abs(c.BaseDir)
	return c, err
}
func availableMemory() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		var k string
		var n int64
		if _, err := fmt.Sscan(l, &k, &n); err == nil && k == "MemAvailable:" {
			return n * 1024
		}
	}
	return 0
}
