# Migrate from earlier versions

**Go is the only implementation.** The Bash runner and its `looper.sh`
compatibility launcher are gone; scripts must call the `looper` binary.

Existing version 1 task files remain supported. The Go controller imports
an unfinished Python parallel journal from the same project directory. It
preserves the original journal and refs, converts the plan, and resumes
retained work. Completed legacy runs need only their existing task file.
Migration tests cover publication after files were applied but before
status was saved.

Run `make build`, then use the new launcher. Stop an old runner before
switching. Do not run an old runner against state after the Go controller
has imported it. Older sequential logs remain on disk but are not imported
into SQLite.

The default run is now continuous. Add `--once` to scripts that expect
completion. `--all`, `--interleave`, `--smart`, `--doctor`, `--tail`, and
`--ls` remain aliases. Odd/even and round-robin agent schedules are replaced
by explicit coding and review agents. Submodules and unresolved index
conflicts are not supported.

The Homebrew stable formula builds the Go release. Both stable and `--HEAD`
install the `looper` binary.
