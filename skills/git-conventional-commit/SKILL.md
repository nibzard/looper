---
name: git-conventional-commit
description: "Create clean git commits using the Conventional Commits format (type(scope): summary). Use when Codex needs to stage changes, craft commit messages, and record completed work in a Git repository."
---

# Git Conventional Commit

## Overview

Create one conventional commit per completed task while keeping the working tree clean and history stable.

## Workflow

1. Review changes with `git status` and `git diff`.
2. Stage only the changes related to the task (use `git add -A` or selective adds).
3. Compose a Conventional Commit message: `type(scope): summary`.
   - Types: feat, fix, refactor, docs, test, chore, build, ci, perf, style.
   - Use imperative mood and keep the summary under 72 characters.
4. Commit once per task. Do not amend, rebase, or rewrite history.
5. If there are no changes to commit, skip the commit and report why.

## Notes

- Avoid touching unrelated changes in a dirty working tree.
- Prefer a specific scope (folder or subsystem) when it clarifies intent.
