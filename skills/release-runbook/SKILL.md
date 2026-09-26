---
name: release-runbook
description: Prepare or publish a software release when requested, including version updates, final checks, tags, GitHub releases, and Homebrew formulas. Preparation alone does not authorize publication.
---

# Prepare and publish a release

Follow the user's requested scope and the project's release rules.
A version update, release review, or skill edit does not authorize publication.
When publication is already authorized, complete the workflow without asking again.

## Workflow

1. Inspect the branch, remote, working tree, release instructions, and
   existing tags. Identify unrelated edits and the intended version.
2. Update the version using the project's normal method.
   A version file must contain only the version if the helper overwrites it.
3. Run the required checks against the final version and release artifacts.
   Record actual results. Skip checks only when explicitly allowed, and
   report the skipped checks.
4. Review the release diff, notes, target branch, and version.
   Commit only release changes using the project's message rules.
   Include any required context note.
5. If publication is authorized, tag the verified commit and push the
   intended branch and tag. Verify each result before the next action.
6. Create the requested release. Use a file for multiline release notes.
   Verify that the release exists. Do not silently skip a requested step.
7. After the tag is available, update a requested Homebrew formula using
   the downloaded archive's checksum. Verify the archive before hashing.
   Commit and push the formula separately when authorized.

On failure, stop before the next external action. Report completed steps,
remaining steps, and the exact failure. Inspect existing tags and releases
before retrying. Do not delete or replace a published tag to recover.

## Helper

Use [scripts/release.sh](scripts/release.sh) from this skill's directory.
Run it with the target repository as the working directory.
The helper publishes a release; use it only within authorized scope.
For preparation alone, perform steps 1 through 4 without this helper.

Read [references/helper.md](references/helper.md) when using the helper.
It explains dry runs, test gates, scoped staging, and recovery limits.
