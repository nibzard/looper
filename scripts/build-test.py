#!/usr/bin/env python3
"""Verify explicit stamps for Git builds and source archives."""

from pathlib import Path
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

print("Build stamp checks passed.")
