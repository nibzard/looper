---
name: git-conventional-commit
description: Create a scoped Git commit when the user requests a commit or the active workflow requires one. Use Conventional Commits when compatible with repository rules.
---

# Create a scoped commit

Follow user and repository rules before this skill's defaults.
Commit work only when the current request or workflow authorizes it.
During a Looper task, keep the commit within the selected task.

## Workflow

1. Inspect `git status --short`, the working diff, and the staged diff.
   Identify changes that existed before this task.
2. Run checks that verify the changed behavior. Record their results.
   Do not describe a skipped check as a passing check.
3. Stage explicit paths or selected hunks. Review `git diff --cached`.
   Keep unrelated working changes and staged changes out of the commit.
4. Write the message using the repository's format.
   When compatible, use `type(scope): Summary`.
5. Commit once for the completed unit of work. Do not amend or rewrite
   history unless the user explicitly requests it.
6. Check the resulting commit and working tree. Add any context note
   required by the project. Report the commit ID, purpose, and checks.

If there is no diff for this task, report that no commit is needed.
A commit does not authorize a push, release, or publication.

## Message defaults

- Aim for a subject of 50 characters or fewer. Use 72 as the hard limit.
- Capitalize the subject as required by local rules. Use an imperative
  summary without a final period.
- Separate the subject and body with a blank line. Wrap the body at 72
  characters. Explain the problem, the resulting behavior, and the reason.
- Put issue references at the end of the body.
- Use one logical change per commit. Do not split a working change into
  incomplete commits only to meet this rule.

For example, when local rules require a capitalized subject:

```text
Fix(parser): Reject empty inputs

Return a clear error when the input is empty. This prevents the caller
from treating an empty document as a successful parse.
```
