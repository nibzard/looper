# Use the release helper

Resolve `scripts/release.sh` relative to this skill's `SKILL.md`.
Do not assume the target repository contains the skill directory.
Run the helper from the root of the repository to release.

For example, after resolving the helper path:

```bash
"$release_helper" --version 0.2.0 --test-cmd 'make test' --dry-run
```

A dry run prints the planned commands. It does not run tests, change files,
contact remote services, or prove that the release will succeed.

For an authorized release:

```bash
"$release_helper" --version 0.2.0 --test-cmd 'make test'
```

## Important options

- `--test-cmd`: The required checks. They run after the version changes.
  Omit this option only with an explicitly authorized `--skip-tests`.
- `--bump-cmd`: The project's version update command. Use a command that
  updates files and leaves commits and tags to the helper.
- `--version-file`: Overwrite a plain version file. Repeat for each file.
  Do not use this option on JSON manifests or Homebrew formulas.
- `--notes-file`: Use a file for release notes. `--notes` accepts short text.
- `--stage-path`: Restrict release staging to a path. Repeat as needed.
  Include every version file and manifest changed by the version update.
- `--allow-dirty`: Requires explicit `--stage-path` values and an empty
  index. Existing edits outside those paths remain outside the commit.
- `--skip-release`: Omit GitHub release creation. Branch and tag pushes
  still happen.
- `--skip-formula`: Omit the formula update.
- `--formula`: Choose a formula. If several exist, select one explicitly.
  The helper supports one `url` and one `sha256` per formula. Update
  formulas with resources or multiple archives manually.
- `--repo`: Set the GitHub repository for the archive URL.
- `--commit-msg`: Override the release message to match local rules.

The helper stops if a requested dependency, test, download, or publish
operation fails. It does not roll back commits, tags, or pushes.
Inspect the repository and remote state before recovering from a partial
release. Resume only the remaining authorized actions.
