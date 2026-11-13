#!/usr/bin/env python3
"""Verify explicit stamps for Git builds, shallow clones, and archives,
then prove the built executables report their own identity end to end."""

from pathlib import Path
import json
import os
import shlex
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def git(root, *args):
    return subprocess.check_output(["git", "-C", str(root), *args], text=True).strip()


def stamps(root, *args):
    output = subprocess.check_output(
        ["bash", str(root / "scripts/build-ldflags.sh"), *args], text=True)
    words = shlex.split(output)
    assert words[::2] == ["-X"] * 3
    return dict(word.split("=", 1) for word in words[1::2])


def clone_shallow(root, depth):
    shallow = root.parent / (root.name + "-shallow%d" % depth)
    subprocess.check_call(
        ["git", "clone", "-q", "--depth", str(depth), "file://" + str(root), str(shallow)])
    return shallow


def fallback(shallow, prefix, next_version):
    """The documented stamp for a revision without an established boundary."""
    return next_version + "-dev.g" + git(shallow, "rev-parse", "--short", "HEAD")


with tempfile.TemporaryDirectory(prefix="looper build ") as directory:
    root = Path(directory)
    (root / "scripts").mkdir()
    shutil.copyfile(ROOT / "scripts/build-ldflags.sh", root / "scripts/build-ldflags.sh")
    (root / "VERSION").write_text("0.8.2\n")
    git(root, "init", "-q")
    git(root, "config", "user.name", "Build fixture")
    git(root, "config", "user.email", "build-fixture@example.invalid")
    git(root, "add", ".")
    git(root, "commit", "-qm", "Fixture")
    prefix = "github.com/nibzard/looper/internal/looper."
    clean = stamps(root)
    assert clean[prefix + "Version"] == "0.8.2"
    assert clean[prefix + "BuildRevision"] == git(root, "rev-parse", "HEAD")
    assert clean[prefix + "BuildModified"] == "false"
    assert stamps(root, "dev")[prefix + "Version"] == "dev"

    # A depth-one checkout of the bump commit cannot prove that HEAD is the
    # boundary, so it takes the documented fallback instead of the plain
    # release version.
    at_bump = clone_shallow(root, 1)
    try:
        assert stamps(at_bump)[prefix + "Version"] == fallback(at_bump, prefix, "0.8.3")
        assert stamps(at_bump)[prefix + "BuildRevision"] == git(root, "rev-parse", "HEAD")
        assert stamps(at_bump, "dev")[prefix + "Version"] == "dev"
    finally:
        shutil.rmtree(at_bump, ignore_errors=True)

    (root / "VERSION").write_text("0.8.3\n")
    assert stamps(root)[prefix + "BuildModified"] == "true"
    git(root, "restore", "VERSION")
    worktree = root.parent / (root.name + "-worktree")
    try:
        git(root, "worktree", "add", "-q", "--detach", str(worktree), "HEAD")
        assert stamps(worktree)[prefix + "BuildRevision"] == git(root, "rev-parse", "HEAD")
        assert stamps(worktree)[prefix + "BuildModified"] == "false"
    finally:
        if worktree.exists():
            git(root, "worktree", "remove", str(worktree))
    archive = root / "archive"
    (archive / "scripts").mkdir(parents=True)
    shutil.copyfile(root / "scripts/build-ldflags.sh", archive / "scripts/build-ldflags.sh")
    (archive / "VERSION").write_text("0.8.2\n")
    assert stamps(archive)[prefix + "BuildRevision"] == "unknown"

    # Commits after the bump derive a dev stamp: next patch plus the count.
    git(root, "commit", "-q", "--allow-empty", "-m", "Feature")
    assert stamps(root)[prefix + "Version"] == "0.8.3-dev.1"
    assert stamps(root, "0.8.2")[prefix + "Version"] == "0.8.2"

    # A depth-one clone of the same post-release revision grafts HEAD as a
    # root commit, so the last VERSION change cannot be established. It
    # must not claim the plain release version.
    post_release = clone_shallow(root, 1)
    try:
        shallow = stamps(post_release)
        assert shallow[prefix + "Version"] != "0.8.2"
        assert shallow[prefix + "Version"] == fallback(post_release, prefix, "0.8.3")
        assert shallow[prefix + "BuildRevision"] == git(root, "rev-parse", "HEAD")
        assert shallow[prefix + "BuildModified"] == "false"
        # An explicit version overrides the fallback, and a modified tree
        # still reports modified beside it.
        assert stamps(post_release, "dev")[prefix + "Version"] == "dev"
        (post_release / "untracked.txt").write_text("local\n")
        assert stamps(post_release)[prefix + "BuildModified"] == "true"
        assert stamps(post_release)[prefix + "Version"] == fallback(post_release, prefix, "0.8.3")
    finally:
        shutil.rmtree(post_release, ignore_errors=True)

    (root / "VERSION").write_text("0.9.9\n")
    git(root, "add", "VERSION")
    git(root, "commit", "-qm", "Release 0.9.9")
    git(root, "commit", "-q", "--allow-empty", "-m", "Fix")
    assert stamps(root)[prefix + "Version"] == "0.9.10-dev.1"
    assert stamps(root, "dev")[prefix + "Version"] == "dev"

    # A depth-two window still grafts the bump commit, so the boundary stays
    # unverifiable. A depth-three window contains the bump with its parent,
    # so the commit count is exact again.
    grafted_bump = clone_shallow(root, 2)
    counted_bump = clone_shallow(root, 3)
    try:
        assert stamps(grafted_bump)[prefix + "Version"] == fallback(grafted_bump, prefix, "0.9.10")
        assert stamps(counted_bump)[prefix + "Version"] == "0.9.10-dev.1"
    finally:
        shutil.rmtree(grafted_bump, ignore_errors=True)
        shutil.rmtree(counted_bump, ignore_errors=True)

# Executable identity, end to end. Each fixture builds the real command in
# one identity state and reads it back from inside an unrelated repository,
# so a binary that borrowed the running repository's revision fails. The
# fixtures build committed HEAD only; identity comes from Git state, which
# the candidate changes never alter.
(ROOT / "build").mkdir(exist_ok=True)
identity_prefix = os.environ.get("LOOPER_RESOURCE_PREFIX", "looper") + "-identity-"
with tempfile.TemporaryDirectory(prefix=identity_prefix, dir=ROOT / "build") as directory:
    identity = Path(directory)
    (identity / "bin").mkdir()

    def build_executable(source, dest, stamped=True):
        command = ["go", "-C", str(source), "build", "-trimpath"]
        if stamped:
            command += ["-ldflags", subprocess.check_output(
                ["bash", str(source / "scripts/build-ldflags.sh")], text=True).strip()]
        subprocess.check_call(command + ["-o", str(dest), "./cmd/looper"])

    def run_executable(binary, *args, cwd):
        # A minimal environment: identity must not depend on Looper variables.
        env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(cwd)}
        return subprocess.check_output(
            [str(binary), *args], cwd=str(cwd), env=env, text=True).strip()

    full = identity / "full"
    subprocess.check_call(["git", "clone", "-q", str(ROOT), str(full)],
                          stderr=subprocess.DEVNULL)
    # Cloning a detached worktree leaves the clone detached; put it on a
    # branch so later clones of it stay quiet.
    git(full, "checkout", "-q", "-b", "fixture")
    head = git(full, "rev-parse", "HEAD")

    # The repository every fixture runs from: unrelated content, so any leaked
    # revision is detectably wrong.
    unrelated = identity / "unrelated"
    unrelated.mkdir()
    git(unrelated, "init", "-q", "-b", "main")
    git(unrelated, "config", "user.name", "Identity fixture")
    git(unrelated, "config", "user.email", "identity-fixture@example.invalid")
    (unrelated / "project.txt").write_text("unrelated project\n")
    git(unrelated, "add", ".")
    git(unrelated, "commit", "-qm", "Unrelated project")
    other_head = git(unrelated, "rev-parse", "HEAD")
    assert other_head != head

    def check(name, binary, release, revision, modified):
        document = json.loads(run_executable(binary, "version", "--json", cwd=unrelated))
        assert document == {"release": release, "revision": revision, "modified": modified}, \
            (name, document)
        # The executable reports its own build only.
        assert document["revision"] != other_head, name
        plain = run_executable(binary, "--version", cwd=unrelated)
        assert plain.startswith("looper %s (" % release), (name, plain)
        return document

    # Stamped repository build: every explicit stamp reaches the executable.
    clean = stamps(full)
    build_executable(full, identity / "bin/stamped")
    check("stamped", identity / "bin/stamped",
          clean[prefix + "Version"], head, False)

    # Direct go build: the Go VCS fallback identifies the source repository.
    build_executable(full, identity / "bin/direct", stamped=False)
    check("direct", identity / "bin/direct", "dev", head, False)

    # Shallow clone: the development fallback stamp, never a plain release.
    shallow = clone_shallow(full, 1)
    try:
        shallow_stamps = stamps(shallow)
        assert "dev.g" in shallow_stamps[prefix + "Version"], shallow_stamps
        build_executable(shallow, identity / "bin/shallow")
        check("shallow", identity / "bin/shallow",
              shallow_stamps[prefix + "Version"], head, False)
    finally:
        shutil.rmtree(shallow, ignore_errors=True)

    # Metadata-free source inside an unrelated repository: the revision stays
    # unknown even though Go sees the enclosing repository, and the version
    # takes no development suffix.
    archive = unrelated / "looper-archive"
    shutil.copytree(full, archive, symlinks=True,
                    ignore=shutil.ignore_patterns(".git"))
    archive_stamps = stamps(archive)
    assert archive_stamps[prefix + "BuildRevision"] == "unknown", archive_stamps
    assert "-dev." not in archive_stamps[prefix + "Version"], archive_stamps
    build_executable(archive, identity / "bin/metadata-free")
    check("metadata-free", identity / "bin/metadata-free",
          archive_stamps[prefix + "Version"], "unknown", False)

    # Dirty tree: the modification marker reaches the executable.
    (full / "local-change.txt").write_text("uncommitted\n")
    dirty_stamps = stamps(full)
    assert dirty_stamps[prefix + "BuildModified"] == "true", dirty_stamps
    build_executable(full, identity / "bin/dirty")
    check("dirty", identity / "bin/dirty",
          dirty_stamps[prefix + "Version"], head, True)

    # The version 2 capabilities document reports the same executable build.
    document = json.loads(run_executable(
        identity / "bin/stamped", "version", "--contract", "v2", "--json", cwd=unrelated))
    assert document["client"]["build"] == {
        "release": clean[prefix + "Version"], "revision": head, "modified": False}, document

print("Build stamp checks passed.")
