package looper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The controller's Git identity contract (T151): a commit the controller
// creates carries the repository's configured identity — the place a project
// records its chosen identity, including a history-scrub decision — and only
// a repository with no complete configuration falls back to the deterministic
// Looper identity.

// commitIdentities reads the author and the committer one commit recorded.
func commitIdentities(t *testing.T, root, commit string) (author, committer string) {
	t.Helper()
	out, err := gitText(root, "show", "-s", "--format=%an <%ae>%n%cn <%ce>", commit)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(out, "\n")
	if len(parts) != 2 {
		t.Fatalf("unexpected identity output for %s: %q", commit, out)
	}
	return parts[0], parts[1]
}

// identityFixture opens a repository with one committed file and returns the
// repository and the commit's revision.
func identityFixture(t *testing.T, configure func(*testing.T, Config)) (*Repository, Config, string) {
	t.Helper()
	c := repoConfig(t)
	configure(t, c)
	r, err := OpenRepository(c, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(c.Root, "work.txt"), []byte("content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tree, err := r.Snapshot(c.Root)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := r.Commit(tree, nil, "Fixture work")
	if err != nil {
		t.Fatal(err)
	}
	return r, c, commit
}

// A complete configured identity reaches the commit as author and committer.
func TestControllerCommitUsesTheConfiguredGitIdentity(t *testing.T) {
	_, c, commit := identityFixture(t, func(t *testing.T, c Config) {
		for _, kv := range [][2]string{{"user.name", "Project Owner"}, {"user.email", "owner@example.test"}} {
			if _, err := gitText(c.Root, "config", kv[0], kv[1]); err != nil {
				t.Fatal(err)
			}
		}
	})
	author, committer := commitIdentities(t, c.Root, commit)
	if author != "Project Owner <owner@example.test>" || committer != author {
		t.Fatalf("the commit recorded author %q and committer %q; both must be the configured identity", author, committer)
	}
}

// The configuration is read through Git's scopes: a repository with no local
// identity resolves the harness's global one.
func TestControllerCommitReadsTheGlobalGitIdentity(t *testing.T) {
	_, c, commit := identityFixture(t, func(t *testing.T, c Config) {})
	author, _ := commitIdentities(t, c.Root, commit)
	if want := harnessGitUserName + " <" + harnessGitUserEmail + ">"; author != want {
		t.Fatalf("the commit recorded author %q; the harness global identity %q must be honored", author, want)
	}
}

// Without any configured identity the commit still succeeds and carries the
// deterministic fallback, never an empty ident.
func TestControllerCommitFallsBackToTheDeterministicIdentity(t *testing.T) {
	_, c, commit := identityFixture(t, func(t *testing.T, c Config) {
		t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
		t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	})
	author, committer := commitIdentities(t, c.Root, commit)
	if author != "Looper <looper@localhost>" || committer != author {
		t.Fatalf("the commit recorded author %q and committer %q; both must be the deterministic fallback", author, committer)
	}
}

// A partial identity is not a usable one: the commit takes the fallback
// whole rather than mixing a configured half with a synthetic one.
func TestControllerCommitIgnoresAPartialGitIdentity(t *testing.T) {
	_, c, commit := identityFixture(t, func(t *testing.T, c Config) {
		if _, err := gitText(c.Root, "config", "user.name", "Half Configured"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
		t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	})
	author, committer := commitIdentities(t, c.Root, commit)
	if author != "Looper <looper@localhost>" || committer != author {
		t.Fatalf("the commit recorded author %q and committer %q; a partial configuration must take the deterministic fallback", author, committer)
	}
}

// Git refuses to start a merge without an identity, even with --no-commit,
// so the merge path must carry the resolved identity itself: the configured
// one when it exists, the fallback otherwise. No commit records it.
func TestMergeRunsOnARepositoryWithoutConfiguredIdentity(t *testing.T) {
	r, c, base := identityFixture(t, func(t *testing.T, c Config) {
		t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
		t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	})
	if err := os.WriteFile(filepath.Join(c.Root, "more.txt"), []byte("more\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tree, err := r.Snapshot(c.Root)
	if err != nil {
		t.Fatal(err)
	}
	other, err := r.Commit(tree, []string{base}, "Divergent work")
	if err != nil {
		t.Fatal(err)
	}
	work, err := r.Workspace("merge-identity", base, Backlog{Version: 1, Sources: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Remove(work)
	if err = r.Merge(work, other); err != nil {
		t.Fatalf("the merge must run without a configured identity: %v", err)
	}
	head, err := gitText(work, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if head != base {
		t.Fatalf("the merge stopped at %s; it must not create a commit", head)
	}
	if _, err = os.Stat(filepath.Join(work, "more.txt")); err != nil {
		t.Fatalf("the merged content must reach the worktree: %v", err)
	}
}
