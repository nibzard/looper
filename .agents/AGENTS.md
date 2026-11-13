# Looper agent instructions

## Working notes

Durable working notes live in `NOTES.md` at the repository root. Read
it at the start of a session. Record diagnosis findings, decisions,
and follow-ups there instead of only in the conversation.

## Live runs

A Looper controller can own this repository. Run `looper status` before
you edit tracked files. While a controller runs, an edit to a tracked file
can start a new review or stop integration. Use `looper add-task`,
`looper cancel`, or `looper amend` to change the backlog. Make other edits
after `looper stop` finishes, or in a separate Git worktree.

Looper publishes accepted work as unstaged edits. Uncommitted changes in
this tree can be finished work that waits for the owner's commit. Do not
discard them.
