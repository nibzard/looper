package looper

import (
	"bytes"
	"context"
	"encoding/json"
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
	// GitDir is this checkout's own git directory (`rev-parse
	// --absolute-git-dir`). It differs from Common inside a linked worktree,
	// and the per-worktree files — the index, its lock, and the sequencer
	// state — live here, never in the shared common directory.
	GitDir   string
	Excluded []string
	// Runtime is the repository-relative, slash-separated path of the
	// controller's runtime tree, or empty when it lives outside the
	// repository. Concurrent atomic writes under it create and rename
	// .looper-write-* temporaries, which a root scan must never stat.
	Runtime string
	// SchemaReference is the absolute path of the declared tracked reference
	// schema, or empty without a declaration. The reference is versioned
	// content the controller respects whenever it seeds the schema control
	// file: the run root's copy and every assembled tree take its bytes, so
	// the control file cannot drift from the shipped content (T117).
	SchemaReference string
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
	gitDir, err := gitText(c.Root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	if gitDir, err = filepath.EvalSymlinks(gitDir); err != nil {
		return nil, err
	}
	r := &Repository{Root: c.Root, Common: common, GitDir: gitDir, Dir: dir, Excluded: []string{filepath.ToSlash(rel), filepath.ToSlash(relSchema)}}
	if rel, e := filepath.Rel(c.Root, c.BaseDir); e == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		if rel == "." {
			return nil, fmt.Errorf("LOOPER_BASE_DIR must not be the project root")
		}
		r.Excluded = append(r.Excluded, filepath.ToSlash(rel))
		r.Runtime = filepath.ToSlash(rel)
	}
	if err = r.setSchemaReference(c); err != nil {
		return nil, err
	}
	return r, nil
}

// setSchemaReference resolves a declared reference schema onto the repository
// (T117). The declaration names tracked content — a schema versioned with the
// project — and every later seed takes the reference bytes, so the control
// file cannot drift from the shipped content. The path must name one readable
// regular file inside the repository, outside the excluded control and
// runtime paths, and hold one JSON document: a declaration that names
// anything else fails here, before a seed can honor it halfway.
func (r *Repository) setSchemaReference(c Config) error {
	if c.SchemaReference == "" {
		return nil
	}
	if r.excluded(c.SchemaReference) {
		return fmt.Errorf("LOOPER_SCHEMA_REFERENCE %s names a control or runtime path; point it at tracked content such as docs/to-do.schema.json", c.SchemaReference)
	}
	p := filepath.Join(r.Root, filepath.FromSlash(c.SchemaReference))
	if st, err := os.Lstat(p); err != nil {
		return fmt.Errorf("the declared reference schema %s is unreadable: %w", p, err)
	} else if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("reference schema must not be a symlink: %s", p)
	} else if st.IsDir() {
		return fmt.Errorf("reference schema must be one file, not a directory: %s", p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("the declared reference schema %s is unreadable: %w", p, err)
	}
	if !json.Valid(b) {
		return fmt.Errorf("the declared reference schema %s is not valid JSON", p)
	}
	r.SchemaReference = p
	return nil
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

// HeadDetail reports the resolved HEAD commit, the symbolic branch, and
// whether HEAD is unborn: a branch checked out with no commits. An unborn
// HEAD is a distinct observed state, so a guard reports it as such instead of
// presenting an empty revision as a branch that moved. A detached or
// unreadable HEAD is not unborn; it keeps today's empty branch value.
func (r *Repository) HeadDetail() (head, branch string, unborn bool) {
	return headDetail(r.Root)
}

func headDetail(root string) (head, branch string, unborn bool) {
	h, headErr := gitText(root, "rev-parse", "--verify", "HEAD")
	b, branchErr := gitText(root, "symbolic-ref", "-q", "HEAD")
	if branchErr != nil {
		return h, "", false
	}
	if headErr == nil {
		return h, b, false
	}
	// The symbolic branch resolves while HEAD does not: unborn only when the
	// branch ref itself holds no commit. A branch that resolves while HEAD
	// fails is a broken repository, not an unborn HEAD.
	if _, refErr := gitText(root, "rev-parse", "--verify", "--quiet", b); refErr != nil {
		return "", b, true
	}
	return h, b, false
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

// commitIdentity resolves the identity controller commits carry. The
// repository's configured Git identity wins — user.name and user.email as
// Git resolves them across the local, global, and system configuration —
// because the configuration is where a project records its chosen commit
// identity (the history-scrub decision lives there too). The deterministic
// Looper identity is the fallback only when no complete identity is
// configured: Git refuses an empty ident, and a controller must integrate
// work without an operator present to answer that refusal.
func (r *Repository) commitIdentity() (name, email string) {
	name, _ = gitText(r.Root, "config", "--get", "user.name")
	email, _ = gitText(r.Root, "config", "--get", "user.email")
	if name == "" || email == "" {
		// A partial identity is not a usable one; the commit takes the
		// fallback whole rather than mixing a configured half with a
		// synthetic one.
		return "Looper", "looper@localhost"
	}
	return name, email
}

// identityEnv maps one identity onto the Git environment variables that
// supply the author and the committer.
func identityEnv(name, email string) map[string]string {
	return map[string]string{
		"GIT_AUTHOR_NAME": name, "GIT_AUTHOR_EMAIL": email,
		"GIT_COMMITTER_NAME": name, "GIT_COMMITTER_EMAIL": email,
	}
}

func (r *Repository) Commit(tree string, parents []string, message string) (string, error) {
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		if p != "" {
			args = append(args, "-p", p)
		}
	}
	name, email := r.commitIdentity()
	b, err := gitCommand(r.Root, []byte(message+"\n"), identityEnv(name, email), args...)
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
	// missing-file error. A declared reference supplies the bytes directly,
	// so an assembled tree cannot carry a control-file copy that drifted
	// from the shipped content (T117). A project without one yet (first
	// bootstrap) skips the copy.
	schema, err := r.schemaSeed()
	if err != nil {
		return p, err
	}
	if len(schema) > 0 {
		if err = atomicWrite(filepath.Join(p, r.Excluded[1]), schema, 0600); err != nil {
			return p, err
		}
	}
	return p, nil
}

// schemaSeed reads the bytes a newly assembled tree carries at the schema
// control-file path. A declared reference supplies them, so the seed cannot
// drift from the shipped content even when the run root's copy already has;
// a reference that became unreadable fails the assembly instead of seeding
// stale bytes. Without a declaration the run root's copy seeds as before,
// and a project without one yet seeds nothing.
func (r *Repository) schemaSeed() ([]byte, error) {
	if r.SchemaReference != "" {
		b, err := os.ReadFile(r.SchemaReference)
		if err != nil {
			return nil, fmt.Errorf("the declared reference schema %s is unreadable: %w", r.SchemaReference, err)
		}
		return b, nil
	}
	if b, err := os.ReadFile(filepath.Join(r.Root, r.Excluded[1])); err == nil {
		return b, nil
	}
	return nil, nil
}
func (r *Repository) Remove(p string) {
	if rel, err := filepath.Rel(filepath.Join(r.Dir, "worktrees"), p); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, "../") {
		gitText(r.Root, "worktree", "remove", "--force", p)
	}
}

// MergeError carries one failed merge with its verified classification. Only
// repository evidence establishes a content conflict: unmerged entries that
// the worktree index still holds after the failed merge. A timeout kill, a
// disk failure, a missing object, and every other Git failure keep Unmerged
// empty, so no caller directs the operator to resolve conflicts that the
// repository does not show. The original error is always preserved.
type MergeError struct {
	// Unmerged lists the paths the worktree index holds in a conflicted
	// state. Empty means no conflict was verified.
	Unmerged []string
	// Checked reports whether the index evidence query itself succeeded.
	// False means the conflict status is unknown, not absent.
	Checked bool
	Err     error
}

func (m *MergeError) Error() string {
	if len(m.Unmerged) > 0 {
		return fmt.Sprintf("content conflicts in: %s\n%v", strings.Join(m.Unmerged, ", "), m.Err)
	}
	return m.Err.Error()
}
func (m *MergeError) Unwrap() error { return m.Err }

func (r *Repository) Merge(work, commit string) error {
	// The merge stops before committing, so no commit records this
	// identity, but Git refuses to start a merge without one; the resolved
	// identity rides along for that requirement alone.
	name, email := r.commitIdentity()
	out, err := gitCommand(work, nil, identityEnv(name, email), "-c", "core.hooksPath=/dev/null", "merge", "--no-commit", "--no-ff", commit)
	if err == nil {
		return nil
	}
	if output := strings.TrimSpace(string(out)); output != "" {
		err = fmt.Errorf("%w\n%s", err, output)
	}
	unmerged, checked := unmergedPaths(work)
	return &MergeError{Unmerged: unmerged, Checked: checked, Err: err}
}

// unmergedPaths reads the paths the worktree index holds in a conflicted
// state. A failed query returns nil paths with ok false, so a caller reports
// an unverified failure instead of asserting a conflict or its absence.
func unmergedPaths(work string) ([]string, bool) {
	out, err := gitText(work, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, false
	}
	var paths []string
	for _, p := range strings.Split(out, "\n") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, true
}

// mergeFailureFeedback explains one failed merge for task feedback. A
// verified content conflict names its unmerged paths. Every other failure
// names the merge operation and keeps the underlying error, without
// directing the operator to resolve conflicts that no repository evidence
// shows.
func mergeFailureFeedback(subject string, err error) string {
	var m *MergeError
	if !errors.As(err, &m) {
		return fmt.Sprintf("Merging the %s into the integrated project failed.\n%v", subject, err)
	}
	if len(m.Unmerged) > 0 {
		return fmt.Sprintf("The %s conflicts with the integrated project. Resolve the content conflicts in: %s.\n%v", subject, strings.Join(m.Unmerged, ", "), m.Err)
	}
	if !m.Checked {
		return fmt.Sprintf("Merging the %s into the integrated project failed, and the repository could not be checked for content conflicts. Fix the underlying Git failure, then retry.\n%v", subject, m.Err)
	}
	return fmt.Sprintf("Merging the %s into the integrated project failed without a content conflict: the repository reports no unmerged paths. Fix the underlying Git failure; conflict resolution is not required.\n%v", subject, m.Err)
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
	if before == after {
		return nil
	}
	// External diff drivers, textconv filters, and prefix-rewriting settings
	// (diff.noprefix, diff.mnemonicPrefix) can produce a patch `git apply`
	// rejects, so the diff runs with all of them pinned — the same hardening
	// the verification diffs use.
	p, err := gitCommand(r.Root, nil, nil, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", before, after, "--")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(p)) == 0 {
		// Identical trees (an empty commit between the revisions) produce an
		// empty patch; `git apply` rejects empty input as unrecognized.
		return nil
	}
	if _, err = gitCommand(r.Root, p, nil, "apply", "--check", "--binary"); err != nil {
		return err
	}
	_, err = gitCommand(r.Root, p, nil, "apply", "--binary")
	return err
}
