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
