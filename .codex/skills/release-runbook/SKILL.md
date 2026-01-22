---
name: release-runbook
description: "Release preparation and publish workflow: run tests, bump version, tag, push, and create a GitHub release (and update Homebrew formula if present). Use when asked to cut a release, bump version, create tags, or publish a release."
---

# Release Runbook

## Overview
Execute a clean release workflow: verify repo state, run tests, bump versions, tag, push, publish a GitHub release, and update downstream artifacts after the tag exists.

## Workflow

### 1) Preflight
- Check repo state: `git status -s` and `git diff` should be clean.
- Confirm remote: `git remote -v` and current branch.
- Verify GitHub auth: `gh auth status`.
- Ensure required tools are available (`git`, `gh`, `jq`, language runtimes).

### 2) Decide the version bump
- Choose SemVer bump (major/minor/patch) based on changes.
- Locate version references and update them before tagging:
  - Common files: `VERSION`, `package.json`, `pyproject.toml`, `Cargo.toml`, `go.mod`, `setup.cfg`, `setup.py`, `Formula/*.rb` (url only), `README.md` badges.
  - Use `rg -n "version|VERSION|__version__"` to find references.

### 3) Run tests (or a documented smoke test)
- Prefer project-defined tests (README/Makefile/CI):
  - `make test`, `npm test`, `pytest`, `go test ./...`, etc.
- If no tests exist, run a minimal smoke check and record it in the release notes.
- Looper-specific smoke check (if repo contains `bin/looper.sh`):
  - Create a temp project, set `MAX_ITERATIONS` low, run the loop, and verify it exits cleanly.

### 4) Commit release changes
- Stage and commit all changes required for the release.
- Keep commit messages Conventional Commits unless the repo specifies otherwise.

### 5) Tag and push
- Create an annotated tag on the release commit:
  - `git tag -a vX.Y.Z -m "vX.Y.Z"`
- Push code and tag:
  - `git push origin <branch>`
  - `git push origin vX.Y.Z`

### 6) Publish GitHub release
- Create a release from the tag:
  - `gh release create vX.Y.Z --title "vX.Y.Z" --notes "<summary>"`

### 7) Update Homebrew formula (if present)
- Only after the release tag exists (tarball is published).
- Compute the new sha:
  - `curl -L -s https://github.com/<org>/<repo>/archive/refs/tags/vX.Y.Z.tar.gz | sha256sum | awk '{print $1}'`
- Update `Formula/*.rb` with the new `url` and `sha256`.
- Commit and push the formula update.

## Notes
- Tag should point to the release commit; formula updates are separate and can land after the tag.
- If tests fail, stop and fix before tagging.
- Keep release notes short and factual (highlights + testing performed).

## Helper Script
Use `scripts/release.sh` to automate the end-to-end release flow.

Examples:
```bash
# Tag, push, release, and update Formula/*.rb
scripts/release.sh --version 0.2.0 --test-cmd "make test"

# Use a custom bump command and a VERSION file
scripts/release.sh --version 1.4.0 --bump-cmd "npm version minor --no-git-tag-version" --version-file VERSION
```

Notes:
- `--version-file` overwrites files with the raw version string (no leading `v`).
- Formula updates are performed after the tag exists; set `--skip-formula` to skip.
- Use `--dry-run` to preview commands without executing.
