package looper

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHarnessGitConfigurationIsControlled pins the T110 contract: every Git
// subprocess the suite spawns reads the harness-controlled configuration,
// never the host's. A hostile home directory carrying its own global
// configuration must not leak in, because fixtures commit and run plain
// `git init` with no identity or branch overrides.
func TestHarnessGitConfigurationIsControlled(t *testing.T) {
	global, ok := os.LookupEnv("GIT_CONFIG_GLOBAL")
	if !ok || global == "" {
		t.Fatal("the harness must set GIT_CONFIG_GLOBAL before the suite runs; plain fixture commits would take the host identity otherwise")
	}
	if _, err := os.Stat(global); err != nil {
		t.Fatalf("the harness global configuration must exist: %v", err)
	}
	// The hostile home supplies everything a host gitconfig could: a
	// different identity, a different default branch, and an XDG file
	// agreeing with it.
	home := t.TempDir()
	hostile := "[user]\n\tname = Host Override\n\temail = host@invalid\n" +
		"[init]\n\tdefaultBranch = host-branch\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(hostile), 0600); err != nil {
		t.Fatal(err)
	}
	xdg := filepath.Join(home, "xdg", "git")
	if err := os.MkdirAll(xdg, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "config"), []byte(hostile), 0600); err != nil {
		t.Fatal(err)
	}
	env := os.Environ()
	kept := env[:0]
	for _, kv := range env {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "HOME", "XDG_CONFIG_HOME":
		default:
			kept = append(kept, kv)
		}
	}
	env = append(kept, "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "xdg"))
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, stderr.String())
		}
		return strings.TrimSpace(out.String())
	}
	if got := git(t.TempDir(), "config", "--global", "--get", "user.name"); got != harnessGitUserName {
		t.Fatalf("the global identity resolved to %q; the hostile host configuration leaked past the harness override (%q)", got, harnessGitUserName)
	}
	// The class T110 closes: one fixture repository commits and reads its
	// branch with no overrides at all, exactly as errorProject and its
	// siblings do.
	repo := t.TempDir()
	git(repo, "init", "-q")
	if got := git(repo, "symbolic-ref", "--short", "HEAD"); got != harnessInitBranch {
		t.Fatalf("a plain git init created branch %q; the initial branch must come from the harness configuration (%q)", got, harnessInitBranch)
	}
	if err := os.WriteFile(filepath.Join(repo, "seed.md"), []byte("seed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(repo, "add", "--", "seed.md")
	git(repo, "commit", "-qm", "fixture")
	ident := git(repo, "log", "-1", "--format=%an <%ae>")
	if want := harnessGitUserName + " <" + harnessGitUserEmail + ">"; ident != want {
		t.Fatalf("the fixture commit recorded %q; the identity must come from the harness configuration (%q)", ident, want)
	}
}
