# Install Looper

This page covers installation options. For the short path, see the [README](../README.md#quickstart).

## Install a release

Install the latest published release with:

```sh
curl -fsSL https://raw.githubusercontent.com/nibzard/looper/main/scripts/install-latest.sh | bash
```

This downloads the release source from GitHub and builds it with Go. It
requires Bash, curl, tar, Git, and Go 1.22 or later. It installs the binary
in `~/.local/bin` without sudo. Run the same command to update. Existing
controllers keep their current binary until you restart them.

To inspect the script first or select an installation directory:

```sh
curl -fsSL https://raw.githubusercontent.com/nibzard/looper/main/scripts/install-latest.sh -o /tmp/looper-install.sh
bash /tmp/looper-install.sh --bin-dir "$HOME/.local/bin"
```

Use `--version v0.8.3` to select a release. Add `--with-skills` to install
that release's bundled skills; older releases can include the old skill names.
The latest release can lag behind `main`. To build the current source:

## Build from source

```sh
git clone https://github.com/nibzard/looper.git
cd looper
./install.sh
```

This installs `looper-diagnose` and `looper-backlog`. Use `--skip-skills`
to install only the binary.
Add `--with-workflow-skills` to also install `git-commit` and
`github-release`. Skills are optional; Looper enforces task contracts
without them. The release skill prefers the project's release process.

Earlier installations used `todo-json-manager`, `git-conventional-commit`,
and `release-runbook`. Remove those old directories from your skills
directory after preserving any local edits. Installation does not remove
or rename existing user skills.

Installation skips existing skills unless a previous Looper install owns them.
It records installed skills in `.looper-installed-skills` in the skills directory.
Run `./uninstall.sh` with the same location options to remove the recorded skills.
Without a receipt, uninstall preserves all skills, including skills from older installations.

Both scripts honor `LOOPER_USER_PREFIX` as the default prefix instead of
`~/.local`. An explicit `--prefix` still wins. Test fixtures that exercise
the installers bind this variable (plus a temporary `HOME`) so they can
never write to the real user prefix.
