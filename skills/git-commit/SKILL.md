---
name: git-commit
description: Create a Git commit when requested or required by the active workflow, using the repository's message conventions.
---

# Create a scoped commit

Identify the requested changes in both the working tree and index. Preserve
unrelated edits, including changes staged before this task. Stage explicit
paths or selected hunks, then review the complete staged diff before committing.

Use the repository's message format. When no format is specified, use an
imperative subject and explain the reason in the body when it adds context.
Use Conventional Commits only when the project or user requests that format.

Commit a completed unit of work. Record relevant check results accurately;
a skipped check is not a passing check. Do not amend or rewrite history unless
the user requests it. If the requested changes produce no diff, report that
no commit is needed.

Verify the resulting commit and working tree. Add a context note when the
project requires one. Report the commit ID, purpose, and validation.
A commit does not authorize a push, release, or publication.
