package looper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Repository struct {
	Root, Common, Dir string
	Excluded          []string
	// Runtime is the repository-relative, slash-separated path of the
	// controller's runtime tree, or empty when it lives outside the
	// repository. Concurrent atomic writes under it create and rename
	// .looper-write-* temporaries, which a root scan must never stat.
	Runtime string
}

// One slow git operation on a large repository must not abort a run. The limit
// comes from LOOPER_GIT_TIMEOUT_SECONDS; 0 disables it. Parse keeps it in sync
// with the configuration.
var gitTimeout = 10 * time.Minute

func gitCommand(cwd string, input []byte, extra map[string]string, args ...string) ([]byte, error) {
	ctx := context.Background()
	if gitTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, gitTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = cleanEnv(extra)
	cmd.Stdin = bytes.NewReader(input)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return out.Bytes(), fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return out.Bytes(), nil
}
func gitText(cwd string, args ...string) (string, error) {
	b, e := gitCommand(cwd, nil, nil, args...)
	return strings.TrimSpace(string(b)), e
}
func OpenRepository(c Config, dir string) (*Repository, error) {
	top, err := gitText(c.Root, "rev-parse", "--show-toplevel")
	if err != nil && c.GitInit {
		if _, err = gitText(c.Root, "init", "-q"); err == nil {
			top, err = gitText(c.Root, "rev-parse", "--show-toplevel")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("run Looper at a Git repository root: %w", err)
	}
	top, _ = filepath.EvalSymlinks(top)
	if top != c.Root {
		return nil, fmt.Errorf("run Looper at the repository root: %s", top)
	}
	common, err := gitText(c.Root, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(c.Root, common)
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return nil, err
	}
	entries, err := gitText(c.Root, "ls-files", "--stage")
	if err != nil {
		return nil, err
	}
	for _, l := range strings.Split(entries, "\n") {
		if strings.HasPrefix(l, "160000 ") {
			return nil, fmt.Errorf("submodule worktrees are not supported")
		}
	}
	u, err := gitText(c.Root, "ls-files", "--unmerged")
	if err != nil {
		return nil, err
	}
	if u != "" {
		return nil, fmt.Errorf("resolve the repository's index conflicts before starting Looper")
	}
	rel, err := filepath.Rel(c.Root, c.Todo)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, fmt.Errorf("task file must be inside the repository")
	}
	for p := filepath.Dir(c.Todo); p != c.Root; p = filepath.Dir(p) {
		if st, e := os.Lstat(p); e == nil && st.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("task file parents must not be symlinks: %s", p)
		}
	}
	schema := c.SchemaPath()
	for _, p := range []string{c.Todo, schema} {
		if st, e := os.Lstat(p); e == nil && st.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("control file must not be a symlink: %s", p)
		}
	}
	relSchema, err := filepath.Rel(c.Root, schema)
	if err != nil {
		return nil, err
	}
	r := &Repository{Root: c.Root, Common: common, Dir: dir, Excluded: []string{filepath.ToSlash(rel), filepath.ToSlash(relSchema)}}
	if rel, e := filepath.Rel(c.Root, c.BaseDir); e == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		if rel == "." {
			return nil, fmt.Errorf("LOOPER_BASE_DIR must not be the project root")
		}
		r.Excluded = append(r.Excluded, filepath.ToSlash(rel))
		r.Runtime = filepath.ToSlash(rel)
	}
	return r, nil
}
func (r *Repository) excluded(p string) bool {
	for _, x := range r.Excluded {
		if p == x || strings.HasPrefix(p, x+"/") {
			return true
		}
	}
	return false
}

// scanExcludes keeps the repository-root scan away from the runtime tree.
// Workers, checks, and the controller write lease, context, prompt, and view
// files under it while a publication snapshots the root. Every atomic write
// creates a .looper-write-* temporary in the target directory and renames it
// into place. A traversing git add that stats a renamed-away temporary exits
// 128 and fails the publication. An exclude pathspec prunes the traversal, so
// the scan never reads those names. The pathspec is used only when the runtime
// path is not ignored: naming an ignored path in an exclude pathspec makes git
// add fail on it, and an ignored directory is already skipped without a stat.
// The check runs per snapshot because ignore rules are versioned content that
// an integrated task can change between two snapshots.
func (r *Repository) scanExcludes(cwd string) []string {
	if r.Runtime == "" || cwd != r.Root {
		return nil
	}
	_, err := gitCommand(cwd, nil, nil, "check-ignore", "-q", "--", r.Runtime)
	var exit *exec.ExitError
	if err == nil || !errors.As(err, &exit) || exit.ExitCode() != 1 {
		// Ignored (exit 0) or undetermined: keep the plain scan rather
		// than risk the ignored-pathspec failure.
		return nil
	}
	return []string{":(exclude)" + r.Runtime}
}
func (r *Repository) Head() (string, string) {
	h, _ := gitText(r.Root, "rev-parse", "--verify", "HEAD")
	b, _ := gitText(r.Root, "symbolic-ref", "-q", "HEAD")
	return h, b
}
func (r *Repository) Snapshot(cwd string) (string, error) {
	return r.SnapshotFrom(cwd, "HEAD")
}

// SnapshotFrom uses the expected tree to identify tracked files. The project
// HEAD does not advance when Looper publishes, so it cannot identify ignored
// files added by an integrated task.
func (r *Repository) SnapshotFrom(cwd, base string, knownTrees ...string) (string, error) {
	f, err := os.CreateTemp(r.Dir, "index-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	defer os.Remove(name)
	defer os.Remove(name + ".lock")
	e := map[string]string{"GIT_INDEX_FILE": name}
	var knownIgnored []byte
	for _, tree := range knownTrees {
		if _, err = gitCommand(cwd, nil, e, "read-tree", tree); err != nil {
			return "", err
		}
		paths, err := gitCommand(cwd, nil, e, "ls-files", "--cached", "--ignored", "--exclude-standard", "-z")
		if err != nil {
			return "", err
		}
		knownIgnored = append(knownIgnored, paths...)
	}
	if base == "HEAD" {
		_, err = gitText(cwd, "rev-parse", "--verify", "HEAD")
	}
	if err != nil {
		_, err = gitCommand(cwd, nil, e, "read-tree", "--empty")
	} else {
		_, err = gitCommand(cwd, nil, e, "read-tree", base)
	}
	if err != nil {
		return "", err
	}
	args := []string{"add", "-A", "--", "."}
	// The runtime tree is pruned from the scan itself: concurrent atomic
	// writes under it rename .looper-write-* temporaries away while the add
	// walks the tree, and a stat of one of those vanished names exits 128.
	// Control files instead leave the temporary index through git rm below.
	// An exclude pathspec that names an ignored file makes git add fail on
	// it, and private context is filtered out by the loop below either way.
	args = append(args, r.scanExcludes(cwd)...)
	if _, err = gitCommand(cwd, nil, e, args...); err != nil {
		return "", err
	}
	if len(r.Excluded) > 0 {
		rm := append([]string{"rm", "-r", "-f", "--cached", "--quiet", "--ignore-unmatch", "--"}, r.Excluded...)
		if _, err = gitCommand(cwd, nil, e, rm...); err != nil {
			return "", err
		}
	}
	ignored, err := gitCommand(cwd, nil, nil, "ls-files", "--cached", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	seedIgnored, err := gitCommand(cwd, nil, e, "ls-files", "--cached", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	ignored = append(ignored, seedIgnored...)
	ignored = append(ignored, knownIgnored...)
	for _, p := range strings.Split(string(ignored), "\x00") {
		if p == "" || r.excluded(p) || r.privateContext(cwd, p) {
			continue
		}
		if _, err = os.Lstat(filepath.Join(cwd, p)); err != nil {
			continue
		}
		if _, err = gitCommand(cwd, nil, e, "add", "-f", "--", p); err != nil {
			return "", err
		}
	}
	tracked, err := gitCommand(cwd, nil, e, "ls-files", "-z")
	if err != nil {
		return "", err
	}
	var remove []byte
	for _, p := range strings.Split(string(tracked), "\x00") {
		if p != "" && (r.excluded(p) || r.privateContext(cwd, p)) {
			remove = append(remove, []byte(p)...)
			remove = append(remove, 0)
		}
	}
	if len(remove) > 0 {
		if _, err = gitCommand(cwd, remove, e, "update-index", "--force-remove", "-z", "--stdin"); err != nil {
			return "", err
		}
	}
	b, err := gitCommand(cwd, nil, e, "write-tree")
	return strings.TrimSpace(string(b)), err
}
func (r *Repository) Commit(tree string, parents []string, message string) (string, error) {
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		if p != "" {
			args = append(args, "-p", p)
		}
	}
	b, err := gitCommand(r.Root, []byte(message+"\n"), map[string]string{"GIT_AUTHOR_NAME": "Looper", "GIT_AUTHOR_EMAIL": "looper@localhost", "GIT_COMMITTER_NAME": "Looper", "GIT_COMMITTER_EMAIL": "looper@localhost"}, args...)
	return strings.TrimSpace(string(b)), err
}
func (r *Repository) Ref(ref, commit string) error {
	_, err := gitText(r.Root, "update-ref", ref, commit)
	return err
}
func (r *Repository) Workspace(name, commit string, b Backlog) (string, error) {
	p := filepath.Join(r.Dir, "worktrees", name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return "", err
	}
	if _, err := gitText(r.Root, "worktree", "add", "--detach", p, commit); err != nil {
		return "", err
	}
	if err := writeJSON(filepath.Join(p, r.Excluded[0]), b); err != nil {
		return p, err
	}
	// Snapshot trees exclude control files, yet prompts name the task schema
	// as a source file, so copy it beside the backlog or every reader pays a
	// missing-file error. A project without one yet (first bootstrap) skips
	// the copy.
	if schema, err := os.ReadFile(filepath.Join(r.Root, r.Excluded[1])); err == nil {
		if err = atomicWrite(filepath.Join(p, r.Excluded[1]), schema, 0600); err != nil {
			return p, err
		}
	}
	return p, nil
}
func (r *Repository) Remove(p string) {
	if rel, err := filepath.Rel(filepath.Join(r.Dir, "worktrees"), p); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, "../") {
		gitText(r.Root, "worktree", "remove", "--force", p)
	}
}
func (r *Repository) Merge(work, commit string) error {
	output, err := gitText(work, "-c", "user.name=Looper", "-c", "user.email=looper@localhost", "-c", "core.hooksPath=/dev/null", "merge", "--no-commit", "--no-ff", commit)
	if err != nil && output != "" {
		return fmt.Errorf("%w\n%s", err, output)
	}
	return err
}
func (r *Repository) Changes(base, commit string) ([]string, error) {
	b, err := gitCommand(r.Root, nil, nil, "diff", "--name-only", "-z", base, commit, "--")
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, p := range strings.Split(string(b), "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}
func (r *Repository) Apply(before, after string) error {
	p, err := gitCommand(r.Root, nil, nil, "diff", "--binary", before, after, "--")
	if err != nil {
		return err
	}
	if _, err = gitCommand(r.Root, p, nil, "apply", "--check", "--binary"); err != nil {
		return err
	}
	_, err = gitCommand(r.Root, p, nil, "apply", "--binary")
	return err
}
