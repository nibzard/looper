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
	Poll, RetryDelay, Timeout, GitTimeout                                                                                 time.Duration
	Once, Follow, JSON, Verbose, Smart, GitInit, JobsAuto                                                                 bool
	Plain, Yes, Reset                                                                                                     bool
	Sources                                                                                                               Sources
	ProfilePath                                                                                                           string
}

// Source records where a setting's effective value came from. Higher values
// win during resolution; sources are tracked separately from values so an
// explicit value equal to a default still overrides a saved profile.
type Source int

const (
	SourceDefault Source = iota
	SourceProfile
	SourceEnv
	SourceFlag
)

func (s Source) String() string {
	switch s {
	case SourceProfile:
		return "profile"
	case SourceEnv:
		return "environment"
	case SourceFlag:
		return "flag"
	default:
		return "default"
	}
}

// Sources is the per-setting provenance of the effective configuration.
type Sources struct {
	Agent, ReviewAgent, Jobs, Model, ClaudeModel, Effort, Verify, Setup, ReviewEvery Source
}

func (s Sources) any(atLeast Source) bool {
	for _, v := range []Source{s.Agent, s.ReviewAgent, s.Jobs, s.Model, s.ClaudeModel, s.Effort, s.Verify, s.Setup, s.ReviewEvery} {
		if v >= atLeast {
			return true
		}
	}
	return false
}

// AnyFlag reports whether any profile-managed setting came from a flag.
func (s Sources) AnyFlag() bool { return s.any(SourceFlag) }

// AnyEnv reports whether any profile-managed setting came from the environment.
func (s Sources) AnyEnv() bool { return s.any(SourceEnv) }

type inputValue struct {
	value string
	from  Source
}
type intValue struct {
	value int
	from  Source
}

// settingSources layers one value per profile-managed setting: defaults, then
// the saved profile, then present environment variables, then explicit flags.
type settingSources struct {
	agent, reviewAgent, jobs, model, claudeModel, effort, verify, setup inputValue
	reviewEvery                                                         intValue
}

// flagOptions records setting flags in command-line order so replay applies
// the same last-flag-wins semantics the original parser had. An explicit
// empty value ("--model \"\"") is a real override, not an absent flag.
type flagOptions struct {
	apps []flagApplication
}

const (
	flagAgent = iota
	flagReviewAgent
	flagAll
	flagModel
	flagEffort
	flagJobs
	flagReviewEvery
	flagInterleave
)

type flagApplication struct {
	field int
	value string
}

func (f *flagOptions) apply(field int, value string) {
	f.apps = append(f.apps, flagApplication{field, value})
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

// usesProfile reports whether a command reads saved settings. Read-only
// commands never fail on an invalid profile.
func usesProfile(command string) bool {
	switch command {
	case "run", "start", "tui", "config":
		return true
	}
	return false
}

// Parse splits parsing from resolution: flags are collected first, the saved
// profile loads as the lowest non-default layer, present environment
// variables follow, and explicit flags win. Final resolution (automatic jobs,
// the review-agent fallback, smart mode) happens once after the merge.
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
	c := Config{Root: root, Todo: "to-do.json", BaseDir: env("LOOPER_BASE_DIR", filepath.Join(home, ".looper")), CodexBin: env("CODEX_BIN", "codex"), ClaudeBin: env("CLAUDE_BIN", "claude"), Assess: env("LOOPER_ASSESS_COMMAND", ""), Hook: env("LOOPER_HOOK", ""), Command: "run", GitInit: env("LOOPER_GIT_INIT", "1") != "0"}
	for k, p := range map[string]*int{"MAX_ITERATIONS": &c.Limit, "LOOPER_MAX_REPAIRS": &c.MaxRepairs, "LOOPER_RATE_LIMIT_MAX": &c.RetryLimit} {
		d := 0
		switch k {
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
	for k, p := range map[string]*time.Duration{"LOOPER_POLL_SECONDS": &c.Poll, "LOOPER_RATE_LIMIT_DELAY": &c.RetryDelay, "LOOPER_TIMEOUT_SECONDS": &c.Timeout, "LOOPER_GIT_TIMEOUT_SECONDS": &c.GitTimeout} {
		d := 30
		switch k {
		case "LOOPER_RATE_LIMIT_DELAY":
			d = 60
		case "LOOPER_TIMEOUT_SECONDS":
			d = 0
		case "LOOPER_GIT_TIMEOUT_SECONDS":
			d = 600
		}
		v, e := integer(k, d)
		if e != nil {
			return c, e
		}
		*p = time.Duration(v) * time.Second
	}
	// Environment validation keeps its original precedence: an invalid
	// numeric environment value fails before any argument is examined.
	if v, ok := os.LookupEnv("LOOPER_REVIEW_EVERY"); ok {
		if n, e := strconv.Atoi(v); e != nil || n < 0 {
			return c, fmt.Errorf("LOOPER_REVIEW_EVERY must be a nonnegative integer")
		}
	}
	// Parsing: collect every option before touching the setting layers.
	flags := flagOptions{}
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
		case "run", "status", "stop", "doctor", "tail", "version", "start", "tui", "attach", "config", "add-plan":
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
		case "--plain":
			c.Plain = true
		case "--yes", "-y":
			c.Yes = true
		case "--reset":
			c.Reset = true
		case "--verbose", "-v":
			c.Verbose = true
		case "--smart", "-s":
			c.Smart = true
		case "--interleave":
			flags.apply(flagInterleave, "")
		case "--jobs", "--check-jobs", "--agent", "--all", "--review-agent", "--model", "--effort", "--max-iterations", "--review-every", "--ls":
			v, err = value()
			if err != nil {
				return c, err
			}
			switch a {
			case "--jobs":
				flags.apply(flagJobs, v)
			case "--check-jobs":
				checks = v
			case "--agent":
				flags.apply(flagAgent, v)
			case "--all":
				flags.apply(flagAll, v)
			case "--review-agent":
				flags.apply(flagReviewAgent, v)
			case "--model":
				flags.apply(flagModel, v)
			case "--effort":
				flags.apply(flagEffort, v)
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
					flags.apply(flagReviewEvery, v)
				}
			}
		default:
			if strings.HasPrefix(a, "-") {
				return c, fmt.Errorf("unknown option %s", a)
			}
			c.Todo = a
		}
	}
	// Resolution starts here. Layer 1 is the built-in defaults; automatic
	// jobs resolve at controller startup, not during parsing.
	settings := settingSources{jobs: inputValue{"auto", SourceDefault}, reviewEvery: intValue{5, SourceDefault}}
	c.BaseDir, err = filepath.Abs(c.BaseDir)
	if err != nil {
		return c, err
	}
	// Layer 2: the saved profile, below any environment variable or flag.
	c.ProfilePath = ProfilePath(c)
	var profile *Profile
	// A corrupt profile blocks every command that applies settings, but
	// `config --reset` must still be able to remove it.
	if usesProfile(c.Command) && !(c.Command == "config" && c.Reset) {
		if profile, err = LoadProfile(c.ProfilePath); err != nil {
			return c, err
		}
	}
	profile.mergeInto(&settings)
	// Layer 3: present environment variables. An explicitly empty variable
	// counts as present and overrides the profile with an empty value.
	for k, into := range map[string]*inputValue{
		"LOOPER_ITER_SCHEDULE": &settings.agent, "LOOPER_REVIEW_AGENT": &settings.reviewAgent,
		"CODEX_MODEL": &settings.model, "CLAUDE_MODEL": &settings.claudeModel,
		"CODEX_REASONING_EFFORT": &settings.effort, "LOOPER_VERIFY_COMMAND": &settings.verify,
		"LOOPER_SETUP_COMMAND": &settings.setup,
	} {
		if v, ok := os.LookupEnv(k); ok {
			*into = inputValue{v, SourceEnv}
		}
	}
	if v, ok := os.LookupEnv("LOOPER_JOBS"); ok {
		settings.jobs = inputValue{v, SourceEnv}
	}
	if v, ok := os.LookupEnv("LOOPER_REVIEW_EVERY"); ok {
		n, _ := strconv.Atoi(v) // validated before parsing
		settings.reviewEvery = intValue{n, SourceEnv}
	}
	// Layer 4: explicit flags, replayed in command-line order so the last
	// flag for each field wins, exactly as the original parser did.
	for _, app := range flags.apps {
		switch app.field {
		case flagJobs:
			settings.jobs = inputValue{app.value, SourceFlag}
		case flagAgent:
			settings.agent = inputValue{app.value, SourceFlag}
		case flagAll:
			settings.agent = inputValue{app.value, SourceFlag}
			settings.reviewAgent = inputValue{app.value, SourceFlag}
		case flagReviewAgent:
			settings.reviewAgent = inputValue{app.value, SourceFlag}
		case flagModel:
			settings.model = inputValue{app.value, SourceFlag}
			settings.claudeModel = inputValue{app.value, SourceFlag}
		case flagEffort:
			settings.effort = inputValue{app.value, SourceFlag}
		case flagReviewEvery:
			n, _ := strconv.Atoi(app.value) // validated during parsing
			settings.reviewEvery = intValue{n, SourceFlag}
		case flagInterleave:
			// At its command-line position: a later --agent or --all still
			// wins, exactly as the original parser behaved. An explicit
			// review agent from a flag, the environment, or a saved profile
			// beats the codex preset; a present-but-empty environment value
			// follows the coding agent.
			settings.agent = inputValue{"claude", SourceFlag}
			if settings.reviewAgent.from == SourceDefault {
				settings.reviewAgent = inputValue{"codex", SourceFlag}
			}
		}
	}
	if settings.jobs.value == "auto" {
		c.JobsAuto = true
		c.Jobs = max(1, min(4, runtime.NumCPU()/2))
		if m := availableMemory(); m > 0 {
			c.Jobs = min(c.Jobs, max(1, int(m/(2<<30))))
		}
	} else {
		c.Jobs, err = strconv.Atoi(settings.jobs.value)
		if err != nil || c.Jobs < 1 {
			return c, fmt.Errorf("jobs must be auto or a positive integer")
		}
	}
	c.Checks, err = strconv.Atoi(checks)
	if err != nil || c.Checks < 1 {
		return c, fmt.Errorf("check-jobs must be positive")
	}
	if c.Smart {
		settings.model = inputValue{env("CODEX_SMART_MODEL", "gpt-6-astra"), SourceFlag}
		settings.effort = inputValue{env("CODEX_SMART_REASONING_EFFORT", "high"), SourceFlag}
	}
	c.Agent = settings.agent.value
	if c.Agent == "" && settings.agent.from == SourceDefault {
		c.Agent = "codex"
	}
	c.ReviewAgent = settings.reviewAgent.value
	if c.ReviewAgent == "" {
		c.ReviewAgent = c.Agent
	}
	c.Model, c.ClaudeModel, c.Effort = settings.model.value, settings.claudeModel.value, settings.effort.value
	c.Verify, c.Setup = settings.verify.value, settings.setup.value
	c.ReviewEvery = settings.reviewEvery.value
	c.Sources = Sources{
		Agent: settings.agent.from, ReviewAgent: settings.reviewAgent.from,
		Jobs: settings.jobs.from, Model: settings.model.from, ClaudeModel: settings.claudeModel.from,
		Effort: settings.effort.from, Verify: settings.verify.from, Setup: settings.setup.from,
		ReviewEvery: settings.reviewEvery.from,
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
	gitTimeout = c.GitTimeout
	return c, nil
}

// The schema control file is named after the task file: a trailing .json is
// replaced by .schema.json, as in the earlier shell controller.
func (c Config) SchemaPath() string {
	base := filepath.Base(c.Todo)
	if strings.HasSuffix(base, ".json") {
		base = strings.TrimSuffix(base, ".json")
	}
	return filepath.Join(filepath.Dir(c.Todo), base+".schema.json")
}

// Use a conservative bound when Linux reports memory or container limits:
// inside a container, /proc/meminfo shows the host, not the cgroup limit.
func availableMemory() int64 {
	bounds := []int64{}
	if n := memAvailable(); n > 0 {
		bounds = append(bounds, n)
	}
	for _, n := range []int64{cgroupHeadroom("/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory.current"), cgroupHeadroom("/sys/fs/cgroup/memory/memory.limit_in_bytes", "/sys/fs/cgroup/memory/memory.usage_in_bytes")} {
		if n > 0 {
			bounds = append(bounds, n)
		}
	}
	if len(bounds) == 0 {
		return 0
	}
	m := bounds[0]
	for _, b := range bounds[1:] {
		m = min(m, b)
	}
	return m
}
func memAvailable() int64 {
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
func cgroupHeadroom(limitPath, usagePath string) int64 {
	limit, err := readCount(limitPath)
	if err != nil || limit <= 0 || limit > 1<<40 { // "max" or a legacy unlimited value
		return 0
	}
	used, err := readCount(usagePath)
	if err != nil {
		return 0
	}
	return max(0, limit-used)
}
func readCount(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}
