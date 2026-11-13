package looper

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The harness-controlled Git identity and initial branch. Fixtures run plain
// `git init` and `git commit` with no overrides, so these values only need to
// stay stable and to differ from any real host configuration.
const (
	harnessGitUserName  = "Looper Test Harness"
	harnessGitUserEmail = "looper-tests@localhost"
	harnessInitBranch   = "main"
)

// isolateHostGitConfig points Git's configuration at files this harness
// controls, and returns the function that removes them again. Fixtures commit
// with the ambient identity otherwise, so any host gitconfig change — a
// missing user identity, a different init.defaultBranch, credential helpers —
// flips suites that pass elsewhere (T110: ~/.gitconfig was rewritten mid-suite
// on 2026-11-06 and broke a fixture until it read its branch back). The
// harness file supplies the identity fixture commits need and pins the branch
// plain `git init` creates, so no host file decides either. GIT_CONFIG_GLOBAL
// replaces ~/.gitconfig and its XDG neighbour; GIT_CONFIG_SYSTEM replaces
// /etc/gitconfig, which carries the same settings on some hosts. Both
// variables are set in this process, so every helper inherits them: gitCommand
// passes the process environment through, as do RunChild and the tests that
// spawn this binary again.
func isolateHostGitConfig() func() {
	dir, err := os.MkdirTemp("", "looper-gitconfig-")
	if err != nil {
		panic("the test harness cannot create its Git configuration: " + err.Error())
	}
	global := filepath.Join(dir, "gitconfig")
	body := "[user]\n\tname = " + harnessGitUserName + "\n\temail = " + harnessGitUserEmail +
		"\n[init]\n\tdefaultBranch = " + harnessInitBranch + "\n"
	if err := os.WriteFile(global, []byte(body), 0600); err != nil {
		panic("the test harness cannot write its Git configuration: " + err.Error())
	}
	os.Setenv("GIT_CONFIG_GLOBAL", global)
	os.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	return func() { os.RemoveAll(dir) }
}

// TestMain dispatches helper invocations that re-execute the current
// executable. runProcess spawns os.Executable() with the __exec argument as a
// process-group supervisor; under go test that executable is this test binary,
// whose generated main would otherwise run the whole suite again.
func TestMain(m *testing.M) {
	restore := isolateHostGitConfig()
	if len(os.Args) > 1 && os.Args[1] == "__exec" {
		if os.Getenv("LOOPER_TEST_LAUNCH_REPORT") == "missing" {
			restore()
			os.Exit(0)
		}
		if os.Getenv("LOOPER_TEST_LAUNCH_REPORT") == "hang" {
			time.Sleep(time.Minute)
			restore()
			os.Exit(0)
		}
		code := RunChild(os.Args[2:])
		restore()
		os.Exit(code)
	}
	// Never read or save the operator's machine agent catalog in tests.
	// Helper subprocesses inherit this location, while individual tests can
	// override it with their own fixture catalogs.
	agentRoot, err := os.MkdirTemp("", "looper-test-agents-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		restore()
		os.Exit(1)
	}
	os.Setenv("LOOPER_AGENTS_FILE", filepath.Join(agentRoot, "agents.json"))
	// If a fixture forgets its stub, fail locally instead of using a live
	// subscription or provider account through an installed command.
	guardDir := filepath.Join(agentRoot, "bin")
	if err := os.MkdirAll(guardDir, 0700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		restore()
		os.Exit(1)
	}
	for _, name := range []string{"claude", "codex", "zcode"} {
		if err := os.WriteFile(filepath.Join(guardDir, name), []byte("#!/bin/sh\necho 'Test fixture did not configure an agent stub' >&2\nexit 125\n"), 0700); err != nil {
			fmt.Fprintln(os.Stderr, err)
			restore()
			os.Exit(1)
		}
	}
	os.Setenv("PATH", guardDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	os.RemoveAll(agentRoot)
	restore()
	os.Exit(code)
}
