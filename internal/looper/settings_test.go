package looper

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// repoConfig builds a Config inside a temporary git repository.
func repoConfig(t *testing.T, args ...string) Config {
	t.Helper()
	clearSettingEnv(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "base"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_BASE_DIR", filepath.Join(root, "base"))
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	if _, err = gitText(root, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	c, err := Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDefaultAgentNotice(t *testing.T) {
	c := repoConfig(t, "run")
	var out bytes.Buffer
	defaultAgentNotice(c, &out)
	if !strings.Contains(out.String(), defaultAgent) {
		t.Fatalf("a defaulted agent with no profile must print the notice, got %q", out.String())
	}
	flagged := repoConfig(t, "run", "--agent", "codex")
	out.Reset()
	defaultAgentNotice(flagged, &out)
	if out.Len() != 0 {
		t.Fatalf("an explicit agent must not print a notice, got %q", out.String())
	}
	asJSON := repoConfig(t, "run", "--json")
	out.Reset()
	defaultAgentNotice(asJSON, &out)
	if out.Len() != 0 {
		t.Fatalf("json output must stay unchanged, got %q", out.String())
	}
}

func TestFirstRunDecisionGates(t *testing.T) {
	c := repoConfig(t)
	// Tests never have terminal streams, so the terminal gate must reject.
	d := firstRunDecision(c, os.Stdin, &bytes.Buffer{})
	if d.Ask {
		t.Fatal("non-terminal output must never ask")
	}
	if !strings.Contains(d.Reason, "terminals") {
		t.Fatalf("unexpected reason %q", d.Reason)
	}
	// --json suppresses forms even on a terminal (checked before streams).
	jsonConfig := repoConfig(t, "--json")
	jsonConfig.Command = "run"
	d = firstRunDecision(jsonConfig, os.Stdin, os.Stdout)
	if d.Ask || !strings.Contains(d.Reason, "--json") {
		t.Fatalf("json gate broken: %+v", d)
	}
	plainConfig := repoConfig(t, "--plain")
	plainConfig.Command = "run"
	d = firstRunDecision(plainConfig, os.Stdin, os.Stdout)
	if d.Ask || !strings.Contains(d.Reason, "--plain") {
		t.Fatalf("plain gate broken: %+v", d)
	}
	yesConfig := repoConfig(t, "--yes")
	yesConfig.Command = "run"
	d = firstRunDecision(yesConfig, os.Stdin, os.Stdout)
	if d.Ask || !strings.Contains(d.Reason, "--yes") {
		t.Fatalf("yes gate broken: %+v", d)
	}
	t.Setenv("LOOPER_NO_PROMPT", "1")
	d = firstRunDecision(c, os.Stdin, os.Stdout)
	if d.Ask || !strings.Contains(d.Reason, "LOOPER_NO_PROMPT") {
		t.Fatalf("no-prompt gate broken: %+v", d)
	}
	t.Setenv("LOOPER_NO_PROMPT", "")
	os.Unsetenv("LOOPER_NO_PROMPT")
	// Explicit settings prefill setup; they do not bypass interactive setup.
	flagged := repoConfig(t, "--agent", "claude")
	flagged.Command = "run"
	d = firstRunDecision(flagged, os.Stdin, os.Stdout)
	if d.Ask || !strings.Contains(d.Reason, "terminals") {
		t.Fatalf("flag gate broken: %+v", d)
	}
	envConfig := repoConfig(t)
	envConfig.Command = "run"
	t.Setenv("CODEX_MODEL", "x")
	d = firstRunDecision(envConfig, os.Stdin, os.Stdout)
	if d.Ask {
		t.Fatal("env gate broken")
	}
	t.Setenv("CODEX_MODEL", "")
	os.Unsetenv("CODEX_MODEL")
	// A saved profile skips the form.
	saved := repoConfig(t)
	saved.Command = "run"
	writeProfileFile(t, saved.ProfilePath, `{"version":1}`)
	d = firstRunDecision(saved, os.Stdin, os.Stdout)
	if d.Ask || !strings.Contains(d.Reason, "profile") {
		t.Fatalf("profile gate broken: %+v", d)
	}
	// Read-only commands never ask.
	other := repoConfig(t, "status")
	d = firstRunDecision(other, os.Stdin, os.Stdout)
	if d.Ask || !strings.Contains(d.Reason, "does not start") {
		t.Fatalf("command gate broken: %+v", d)
	}
}

// The suppression flags apply to every eligible controller start, including
// the unattached tui path (T011): --plain, --json, --yes, and
// LOOPER_NO_PROMPT=1 keep the automatic form from appearing there too.
func TestFirstRunDecisionSuppressesOnTUI(t *testing.T) {
	for _, args := range [][]string{{"tui", "--plain"}, {"tui", "--json"}, {"tui", "--yes"}} {
		c := repoConfig(t, args...)
		d := firstRunDecision(c, os.Stdin, os.Stdout)
		if d.Ask {
			t.Fatalf("%v must suppress the first-run form: %+v", args, d)
		}
	}
	c := repoConfig(t, "tui")
	t.Setenv("LOOPER_NO_PROMPT", "1")
	d := firstRunDecision(c, os.Stdin, os.Stdout)
	if d.Ask || !strings.Contains(d.Reason, "LOOPER_NO_PROMPT") {
		t.Fatalf("LOOPER_NO_PROMPT must suppress the tui form: %+v", d)
	}
}

func TestMaybeFirstRunSkipsWithoutTerminal(t *testing.T) {
	c := repoConfig(t)
	args := []string{}
	got, start, err := maybeFirstRun(args, c, os.Stdin, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || start || !strings.Contains(err.Error(), "Choose an agent") {
		t.Fatalf("a fresh unattended project must require an agent: %v %v", start, err)
	}
	if got.Agent != c.Agent {
		t.Fatal("config replaced without asking")
	}
	if _, err = os.Stat(c.ProfilePath); !os.IsNotExist(err) {
		t.Fatal("skipped form must not create a profile")
	}
}

func TestConfigResetOffline(t *testing.T) {
	c := repoConfig(t, "config")
	c.Reset = true
	writeProfileFile(t, c.ProfilePath, `{"version":1}`)
	var out, stderr bytes.Buffer
	if code := runConfig(c, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("reset failed: %d %s", code, stderr.String())
	}
	if _, err := os.Stat(c.ProfilePath); !os.IsNotExist(err) {
		t.Fatal("reset must remove the profile")
	}
	if !strings.Contains(out.String(), "Removed") {
		t.Fatalf("unexpected output %q", out.String())
	}
	// Resetting again reports that no profile exists.
	out.Reset()
	if code := runConfig(c, os.Stdin, &out, &stderr); code != 1 {
		t.Fatalf("second reset must fail: %d", code)
	}
}

func TestConfigEditorNeedsTerminal(t *testing.T) {
	c := repoConfig(t, "config")
	var out, stderr bytes.Buffer
	if code := runConfig(c, os.Stdin, &out, &stderr); code != 1 {
		t.Fatalf("editor must require a terminal: %d", code)
	}
	if !strings.Contains(stderr.String(), "interactive terminal") {
		t.Fatalf("unexpected error %q", stderr.String())
	}
}

func TestConfigRejectedWhileOwned(t *testing.T) {
	c := repoConfig(t, "config")
	common := filepath.Join(c.Root, ".git")
	f, err := os.OpenFile(filepath.Join(common, "looper.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }()
	if !repositoryOwned(c) {
		t.Fatal("ownership probe missed a held lock")
	}
	var out, stderr bytes.Buffer
	if code := runConfig(c, os.Stdin, &out, &stderr); code != 1 {
		t.Fatalf("config must refuse while a controller owns the repository: %d", code)
	}
	if !strings.Contains(stderr.String(), "owns this repository") {
		t.Fatalf("unexpected error %q", stderr.String())
	}
}

func TestProfileSettingsRoundTrip(t *testing.T) {
	p := &Profile{Version: 1, Agent: "claude", Jobs: 4}
	s := settingsFromProfile(p)
	if s.Agent != "claude" || s.Jobs != 4 || s.ReviewEvery != 5 {
		t.Fatalf("defaults not applied: %+v", s)
	}
	s.ReviewAgent = ""
	s.VerifyCommand = "go test ./..."
	back := ProfileFromSettings(s)
	if back.Agent != "claude" || *back.ReviewEvery != 5 || back.VerifyCommand != "go test ./..." {
		t.Fatalf("conversion lost values: %+v", back)
	}
	if err := back.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The form default for the coding agent is the shared defaultAgent, so a
// missing or partial profile never prefills a different agent than a
// defaulted run. Explicit saved agents stay untouched.
func TestSettingsFromProfileAgentDefault(t *testing.T) {
	if got := settingsFromProfile(nil).Agent; got != defaultAgent {
		t.Fatalf("missing profile must default to %s, got %q", defaultAgent, got)
	}
	partial := settingsFromProfile(&Profile{Version: 1, Jobs: 2})
	if partial.Agent != defaultAgent || partial.Jobs != 2 {
		t.Fatalf("absent agent field must default to %s: %+v", defaultAgent, partial)
	}
	for _, agent := range []string{"codex", "claude", "zcode"} {
		s := settingsFromProfile(&Profile{Version: 1, Agent: agent, ReviewAgent: agent})
		if s.Agent != agent || s.ReviewAgent != agent {
			t.Fatalf("explicit agent %s changed: %+v", agent, s)
		}
	}
}

// A fresh form prefilled from the default and edited only for parallel
// sessions saves defaultAgent, not a hardcoded agent. The value survives a
// profile save and reload.
func TestFreshFormSavesDefaultAgent(t *testing.T) {
	before := settingsFromProfile(nil)
	after := before
	after.Jobs = 3
	saved := ProfileFromSettings(after)
	if saved.Agent != defaultAgent {
		t.Fatalf("saving a fresh form must pin %s, got %q", defaultAgent, saved.Agent)
	}
	if err := saved.Validate(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := SaveProfile(path, saved); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Agent != defaultAgent {
		t.Fatalf("reload lost the agent: %+v", reloaded)
	}
	if s := settingsFromProfile(reloaded); s.Agent != defaultAgent || s.Jobs != 3 {
		t.Fatalf("reloaded form values wrong: %+v", s)
	}
}
