//go:build linux

package looper

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/nibzard/looper/internal/ui"
)

// swapStdioWithStderr replaces all three standard streams with terminals.
// huh renders its forms on the standard error stream, so a form test must
// capture that stream too.
func swapStdioWithStderr(t *testing.T, in, out *os.File) {
	t.Helper()
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = in, out, out
	t.Cleanup(func() { os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr })
}

// quietIntrChars stops the input terminal from raising SIGINT for Ctrl-C, so
// the byte always reaches the form. Bubble Tea owns raw mode only while the
// form is live; a stray signal outside that window would kill the test.
func quietIntrChars(t *testing.T, slave *os.File) {
	t.Helper()
	termios, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
	if err != nil {
		t.Skipf("cannot read the terminal state: %v", err)
	}
	termios.Lflag &^= unix.ISIG
	if err = unix.IoctlSetTermios(int(slave.Fd()), unix.TCSETS, termios); err != nil {
		t.Skipf("cannot clear ISIG: %v", err)
	}
}

// recordScreen reads everything a terminal master emits into one string.
func recordScreen(master *os.File) func() string {
	var mu sync.Mutex
	var screen strings.Builder
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				mu.Lock()
				screen.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return screen.String()
	}
}

// waitFormScreen polls the recorded terminal until want appears.
func waitFormScreen(t *testing.T, screen func() string, want string) {
	t.Helper()
	deadline := time.Now().Add(testBudget(10 * time.Second))
	for time.Now().Before(deadline) {
		if strings.Contains(screen(), want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the terminal never showed %q: %q", want, screen())
}

// openFormTerminal prepares the terminal pair every form test needs: real
// streams for detection, a usable TERM, and byte-wise Ctrl-C delivery.
func openFormTerminal(t *testing.T) (*os.File, func() string) {
	t.Helper()
	masterIn, in := openPTY(t)
	masterOut, out := openPTY(t)
	swapStdioWithStderr(t, in, out)
	quietIntrChars(t, in)
	t.Setenv("TERM", "xterm-256color")
	return masterIn, recordScreen(masterOut)
}

// Cancelling the first-run form has no side effects: no profile is saved, no
// controller starts, and the repository lock stays free.
func TestFirstRunFormCancelLeavesNoProfile(t *testing.T) {
	c := repoConfig(t) // the run command, no flags, no profile
	masterIn, screen := openFormTerminal(t)

	type outcome struct {
		start bool
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		_, start, err := maybeFirstRun(nil, c, os.Stdin, os.Stdout, &strings.Builder{})
		done <- outcome{start, err}
	}()
	waitFormScreen(t, screen, "Agent commands on this machine")
	if _, err := os.Stat(c.ProfilePath); !os.IsNotExist(err) {
		t.Fatal("the open form must not have saved a profile yet")
	}
	masterIn.WriteString("\x03")
	select {
	case got := <-done:
		if got.err != nil || got.start {
			t.Fatalf("cancellation must return without starting: %+v", got)
		}
	case <-time.After(testBudget(10 * time.Second)):
		t.Fatal("the cancelled form never returned")
	}
	if _, err := os.Stat(c.ProfilePath); !os.IsNotExist(err) {
		t.Fatal("cancellation must not save a profile")
	}
	common, err := gitText(c.Root, "rev-parse", "--git-common-dir")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(c.Root, common)
	}
	lock, err := lockRepository(common)
	if err != nil {
		t.Fatalf("cancellation must leave the repository lock free: %v", err)
	}
	lock.Close()
}

// The reopened contract itself: an unattached tui start asks the first-run
// form on the terminal, and cancelling it exits without saving answers or
// starting any controller.
func TestTUIAsksFirstRunFormAndCancelStartsNothing(t *testing.T) {
	c := preparedRepo(t, "tui")
	masterIn, screen := openFormTerminal(t)
	_, dir := projectPaths(c)

	var stderr strings.Builder
	done := make(chan int, 1)
	go func() { done <- runTUI([]string{"tui"}, c, os.Stdout, &stderr) }()
	waitFormScreen(t, screen, "Agent commands on this machine")
	masterIn.WriteString("\x03")
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("a cancelled first-run form must exit 0, got %d: %s", code, stderr.String())
		}
	case <-time.After(testBudget(10 * time.Second)):
		t.Fatal("the cancelled tui start never returned")
	}
	if _, err := os.Stat(c.ProfilePath); !os.IsNotExist(err) {
		t.Fatal("cancellation must not save a profile")
	}
	if _, err := os.Stat(controllerPath(dir)); !os.IsNotExist(err) {
		t.Fatal("cancellation must not start a controller")
	}
	if LoadView(dir) != nil {
		t.Fatal("cancellation must not publish a dashboard view")
	}
	if strings.Contains(stderr.String(), "Error") {
		t.Fatalf("unclean cancel: %s", stderr.String())
	}
}

// Completing the first-run form on the unattached tui path saves the answers
// as the profile, re-resolves the configuration from it, and starts the
// dashboard controller; the answers never leak one-run values.
func TestTUIFirstRunFormSavesProfileAndStartsController(t *testing.T) {
	t.Setenv("CODEX_BIN", "/bin/false")
	t.Setenv("CLAUDE_BIN", "/bin/false")
	c := preparedRepo(t, "tui")
	masterIn, screen := openFormTerminal(t)
	_, dir := projectPaths(c)

	var stderr strings.Builder
	done := make(chan int, 1)
	go func() { done <- runTUI([]string{"tui"}, c, os.Stdout, &stderr) }()
	finished := false
	t.Cleanup(func() {
		if finished {
			return
		}
		masterIn.WriteString("\x03")
		if LoadView(dir) != nil {
			syscall.Kill(os.Getpid(), syscall.SIGTERM)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
	waitFormScreen(t, screen, "Agent commands on this machine")
	masterIn.WriteString("\r")
	waitFormScreen(t, screen, "Worker agent profile")
	for i := 0; i < 3; i++ {
		masterIn.WriteString("\r")
		time.Sleep(150 * time.Millisecond)
	}
	waitFormScreen(t, screen, "Save this configuration?")
	masterIn.WriteString("y\r")
	deadline := time.Now().Add(testBudget(15 * time.Second))
	for !fileExists(c.ProfilePath) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(c.ProfilePath); err != nil {
		t.Fatalf("the completed form never saved a profile: %s", screen())
	}
	waitFormScreen(t, screen, "Saved settings to")
	profile, err := LoadProfile(c.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Agent != defaultAgent || profile.Jobs != 0 || profile.ReviewEvery == nil || *profile.ReviewEvery != 5 {
		t.Fatalf("the saved answers must carry the accepted defaults: %+v", profile)
	}
	// The dashboard controller is up; drain it through the same signal path
	// as any tui controller.
	for LoadView(dir) == nil && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if LoadView(dir) == nil {
		t.Fatalf("the tui controller never published its first view: %s", screen())
	}
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		finished = true
		if code != 0 {
			t.Fatalf("a drained tui controller must exit 0, got %d: %s", code, stderr.String())
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatalf("the tui controller never drained: %s", stderr.String())
	}
	waitFormScreen(t, screen, "Drain complete")
}

// `looper tui --plain` on a fully interactive terminal falls back to the CLI
// controller and still never asks the first-run form: the parsed flags reach
// the fallback instead of a re-parsed bare command line.
func TestTUIPlainFallbackSuppressesFirstRunForm(t *testing.T) {
	c := preparedRepo(t, "tui", "--plain", "--once")
	_, screen := openFormTerminal(t)

	var stderr strings.Builder
	done := make(chan int, 1)
	go func() { done <- runTUI([]string{"tui", "--plain", "--once"}, c, os.Stdout, &stderr) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("the plain CLI fallback must finish cleanly, got %d: %s", code, stderr.String())
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatalf("the plain CLI fallback never finished: %s", screen())
	}
	if strings.Contains(screen(), "Worker agent profile") {
		t.Fatalf("--plain must suppress the first-run form: %s", screen())
	}
	if _, err := os.Stat(c.ProfilePath); !os.IsNotExist(err) {
		t.Fatal("the suppressed form must not save a profile")
	}
	if !strings.Contains(stderr.String(), "running with CLI output") {
		t.Fatalf("expected the CLI fallback notice: %s", stderr.String())
	}
}

// The full editor exposes the complete saved-profile field set: both agents,
// any positive session count, both model fields, effort, the review
// frequency, and the verify and setup commands. Accepting every prefilled
// field leaves the settings unchanged.
func TestEditorFormFieldSet(t *testing.T) {
	before := settingsFromProfile(nil)
	s := before
	masterIn, screen := openFormTerminal(t)

	done := make(chan error, 1)
	go func() { done <- ui.RunEditorForm(&s) }()
	waitFormScreen(t, screen, "Worker agent profile")
	deadline := time.Now().Add(testBudget(20 * time.Second))
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the editor form failed: %v", err)
			}
			for _, want := range []string{
				"Worker agent profile", "Reviewer agent profile", "Parallel sessions",
				"Codex model", "Claude model", "reasoning effort", "Review frequency",
				"Verify command", "Setup command",
			} {
				if !strings.Contains(screen(), want) {
					t.Fatalf("the editor is missing the %q field: %s", want, screen())
				}
			}
			if s != before {
				t.Fatalf("accepting every prefilled field must keep the settings: %+v", s)
			}
			return
		default:
			masterIn.WriteString("\r")
			time.Sleep(150 * time.Millisecond)
		}
	}
	t.Fatalf("the editor form never completed: %s", screen())
}

// A complete setup saves both scopes only after reviewing the actual commands.
// The fixture command must never execute while configuring the project.
func TestAgentConfigurationWizardSavesAfterConfirmation(t *testing.T) {
	c := repoConfig(t)
	catalogPath := filepath.Join(t.TempDir(), "agents.json")
	t.Setenv("LOOPER_AGENTS_FILE", catalogPath)
	c.AgentCatalogPath = catalogPath
	marker := filepath.Join(t.TempDir(), "executed")
	command := filepath.Join(t.TempDir(), "custom-claude")
	if err := os.WriteFile(command, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	c.ClaudeBin = command
	masterIn, screen := openFormTerminal(t)
	type result struct {
		c     Config
		start bool
		err   error
	}
	done := make(chan result, 1)
	go func() {
		got, start, err := maybeFirstRun(nil, c, os.Stdin, os.Stdout, &strings.Builder{})
		done <- result{got, start, err}
	}()
	waitFormScreen(t, screen, "Agent commands on this machine")
	masterIn.WriteString("\r")
	waitFormScreen(t, screen, "Worker agent profile")
	// Accept the worker, following reviewer, and session count.
	for i := 0; i < 3; i++ {
		masterIn.WriteString("\r")
		time.Sleep(150 * time.Millisecond)
	}
	waitFormScreen(t, screen, "Save this configuration?")
	if fileExists(catalogPath) || fileExists(c.ProfilePath) {
		t.Fatal("configuration saved before confirmation")
	}
	masterIn.WriteString("y\r")
	select {
	case got := <-done:
		if got.err != nil || !got.start {
			t.Fatalf("setup did not complete: %+v", got)
		}
		catalog, err := loadAgentCatalog(catalogPath)
		if err != nil {
			t.Fatal(err)
		}
		if catalog.Agents["claude"].Command != command {
			t.Fatalf("command not saved: %+v", catalog)
		}
		profile, err := LoadProfile(c.ProfilePath)
		if err != nil || profile == nil {
			t.Fatalf("project not saved: %v", err)
		}
		if fileExists(marker) {
			t.Fatal("configuration launched an agent")
		}
	case <-time.After(testBudget(15 * time.Second)):
		t.Fatalf("setup did not complete: %s", screen())
	}
}

func TestAgentCommandFormAddsWrapper(t *testing.T) {
	command := filepath.Join(t.TempDir(), "provider-wrapper")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	commands := []ui.AgentCommand{{Name: "claude", Command: command, Adapter: "claude"}}
	worker, reviewer := "", ""
	masterIn, screen := openFormTerminal(t)
	done := make(chan error, 1)
	go func() {
		done <- ui.RunAgentCommands(&commands, &worker, &reviewer, []ui.AgentInterface{{Name: "claude", Label: "Claude Code", Model: true, Effort: true}})
	}()
	waitFormScreen(t, screen, "Agent commands on this machine")
	masterIn.WriteString("\x1b[B\r")
	waitFormScreen(t, screen, "Agent name")
	for _, answer := range []string{"glm\r", command + "\r", "\r"} {
		masterIn.WriteString(answer)
		time.Sleep(150 * time.Millisecond)
	}
	waitFormScreen(t, screen, "Model (empty uses agent settings)")
	for _, answer := range []string{"test-model\r", "high\r"} {
		masterIn.WriteString(answer)
		time.Sleep(150 * time.Millisecond)
	}
	masterIn.WriteString("\r")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if len(commands) != 2 || commands[1].Name != "glm" || commands[1].Command != command || commands[1].Adapter != "claude" || commands[1].Model != "test-model" || commands[1].Effort != "high" {
			t.Fatalf("custom command was not configured: %+v", commands)
		}
	case <-time.After(testBudget(10 * time.Second)):
		t.Fatalf("custom command form did not complete: %s", screen())
	}
}
