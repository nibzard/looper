# Release runbook

This file records the project's release rules and lessons. The
`github-release` skill drives an agent session through the same steps, and
`skills/github-release/scripts/release.sh` publishes when authorized.
Preparation alone never authorizes publication.

## Versioning

- `VERSION` is the single source. It holds the version and nothing else.
  `make build` injects it into the binary with ldflags.
- Versions follow semantic versioning: patch for fixes, minor for features,
  major for incompatible changes.
- Land all release content on `main` first. The version bump is the last
  commit of a release, so no commit carries a version it does not contain.
- The bump is its own commit: `Release <description>`, changing only
  `VERSION`.
- Between releases, every build self-identifies: with no explicit version,
  `build-ldflags.sh` counts the commits since the last `VERSION` bump and
  stamps `<next patch>-dev.<N>`. A build four commits after `Release
  0.9.0` reports `0.9.1-dev.4`. The bump commit itself reports the plain
  version, and an explicit version argument pins the stamp.
- Shallow clones can fake the boundary. The shallow boundary commit has
  no visible parent, so it seems to add every file. A grafted HEAD can
  therefore pretend to be the last `VERSION` change and stamp a later
  build with the plain release version. The script trusts the boundary
  only when its parent commit is present. Otherwise it stamps
  `<next patch>-dev.g<abbreviated HEAD>`. A depth-one clone after
  `Release 0.9.0` reports `0.9.1-dev.g1a2b3c4`, never the plain `0.9.0`,
  and it does not invent a commit distance. A shallow checkout of the
  bump commit itself takes the same fallback, because it cannot prove
  the boundary. The exact count returns once the clone window contains
  the bump commit with its parent. Explicit version overrides, the
  revision and modified fields, and source archives (revision `unknown`)
  keep their existing behavior.

## Executable identity

A looper executable describes itself. It never describes the repository it
runs against. The rules:

- Identity comes from the explicit stamps `build-ldflags.sh` writes:
  `Version`, `BuildRevision`, and `BuildModified`. A direct `go build`
  without stamps falls back to the VCS metadata Go embeds from the
  repository that holds the source being built. With neither source, the
  revision stays `unknown`.
- An explicit `unknown` stamp is a statement, not a gap. It wins over any
  embedded VCS metadata, so a source archive extracted inside an unrelated
  repository still reports `unknown`. The stamp script refuses an enclosing
  repository for the same reason.
- The shallow-clone rules above apply to the built executable the same way.
  A shallow build self-identifies with `<next patch>-dev.g<abbreviated
  HEAD>`, never with the plain release version.
- Identity is fixed at build time. `looper version`, `looper status`, and
  controller metadata report the executable's own build. They never read
  the project repository the executable operates on.
- Client and controller builds are reported side by side, in
  `looper version --contract v2` and in compact status. A difference is
  expected, not an error: the running controller keeps its original build
  until it restarts.
- `install.sh` warns when the destination binary is in use by a running
  process. Installing replaces the file on disk only. Running controllers
  keep their original build, as the 2026-10-11 lesson below records.

`scripts/build-test.py` builds fixtures in each identity state and reads
them back from an unrelated repository, so a binary that borrows another
repository's revision fails the check.

## Prepare a release

1. Inspect the tree: `git status`, recent tags, and the diff since the last
   tag. Other sessions may hold work in the tree; ask before sweeping it in.
2. Pick the version from what actually landed since the last tag.
3. Run `make test` on the final tree. It builds, runs the Go tests,
   `go vet`, the runtime suite, package smoke, and skill regression.
   Record the results.
4. Write the new version into `VERSION`.
5. Commit the bump alone.
6. Build and install: `make install`, then check `looper --version`.
7. Report what was and was not done. Tags and pushes are publication.

## Agent experience release checks

Run these checks before any release that ships the version 2 agent
contracts, and again before any release that flips the default. The
protocol behind them is `docs/research/AGENT-EXPERIENCE-EVALUATION.md`;
the migration rules are `docs/AGENT-CONTRACT-MIGRATION.md`.

### Compatibility opt-in and default transition

The default contract stays `legacy` until the migration guide's three
conditions hold: the plan's stages are complete and AX-026 documents
every command; the AX-027 gates pass; and the release notes name the
change. The flip itself is a release decision, prepared as follows:

1. Re-run the deterministic gates on the release tree:
   `make test`, then `make race`.
2. Confirm the byte guards are part of that run:
   `TestLegacyContractBytesAreFrozen`, `TestLegacyContractShapesAreFrozen`,
   `TestVersionTwoFixturesMatchSchemas`.
3. Write the release-notes paragraph that names the new default and the
   one-line opt-out (`--contract legacy`, or `LOOPER_CONTRACT=legacy`).
4. Append the release name to `legacy_default_until` in the
   capabilities fixture and update `legacyDefaultUntil` in
   `internal/looper/capabilities.go` to name the release.
5. Add a dated change-log entry to
   `docs/AGENT-CONTRACT-MIGRATION.md`.
6. Flip `DefaultContractVersion` in the same commit as the fixture and
   note, and re-run `make test` on the flipped tree.

A release that does not flip the default states that version 2 stays
opt-in. Until the notes name a new default, `legacy_default_until`
keeps its current sentence.

### Schema and command documentation

1. Every served document has a schema under `docs/contracts/schemas/`
   and a fixture under `docs/contracts/fixtures/`.
2. `docs/contracts/commands.json` lists every command; its
   `implemented` flags match what the binary serves.
3. `looper help <command>` prints each command's block, and the help
   guards (`TestCommandHelp*`, `TestSkill*`) pass.
4. The legacy-invocation table in the migration guide still holds: each
   legacy form either works unchanged or names its documented
   replacement.
5. The skills under `skills/` teach only invocations that parse.

### Platform gaps

State the platform support honestly in the release notes:

- Process identity checks on darwin read `/proc` only, so worker
  statistics and liveness detection are unreliable on macOS (T019,
  blocked on a macOS validation host).
- The PTY test coverage (first-run form, animation, TUI signals) runs
  on Linux only.
- The macOS ARM64 cross-compile is a compile check; no runtime behavior
  was exercised on macOS.

A release notes these gaps instead of claiming untested support. Fixing
them is separate work, not a release blocker unless the release claims
macOS support.

### Installed controllers keep their build

Installing a binary replaces the file on disk only. A running
controller keeps the build it started with until it restarts.

1. Confirm `looper version --contract v2` and compact status report
   client and controller builds side by side.
2. Confirm `install.sh` warns when the destination binary belongs to a
   running process (the identity fixtures in `scripts/build-test.py`).
3. Never claim a live controller received a fix because installation
   succeeded; name the restart as the operator action.
4. Record in the release notes that controllers started before the
   upgrade keep the old contract surface and request types until they
   restart, and that older metadata falls back to the absent-record
   compatibility set.

### Fresh-agent evaluation status

Record the evaluation's status in the release notes as it stands:
prepared and not run, or run with its results file
(`docs/research/AGENT-EXPERIENCE-EVALUATION-RESULTS.md`) cited by name.
The evaluation is not a precondition for the default transition; the
migration guide's three conditions are. Do not describe fixture success
as evidence about real agents.

## Publish (only when asked)

1. Tag the bump commit `vX.Y.Z` and push the branch and the tag.
2. Create the GitHub release. Keep multiline notes in a file.
3. Download the tag archive and verify it before hashing.
4. Put the archive checksum into `Formula/looper.rb`.
5. Commit the formula separately: `Chore(formula): Update vX.Y.Z`, then
   push.

## Lessons

Recorded 2026-10-11, from the 0.6.0 and 0.6.1 releases.

- Bump last. In 0.6.1 the version was bumped and installed before the last
  fix landed, so the installed 0.6.1 binary lacks commits that the
  repository also calls 0.6.1. Land content, then bump, then build.
- Installing does not update running controllers. A live run keeps its old
  binary; only later user-started runs pick up the new one. Never claim a
  live run received a fix because installation succeeded.
- The formula needs the tag first. Its URL points at the tag archive, so
  its checksum exists only after the tag does. Update it in its own commit.
- Prove a regression test before trusting it. Run it against the unfixed
  code (`git stash push -- <file>`), watch it fail with the real symptom,
  then restore and watch it pass.
- A shared tree moves under you. Re-run `git status` before each commit,
  run the checks on the combined tree, and keep each commit to one logical
  change.
