# Looper manifesto

How we decide what to build and what to refuse. Written 2026-10-11, after
comparing Looper to the current orchestrator ecosystem
(`docs/research/PRACTICES-REPORT-2026-10-11.md`).

## Pass butter

The butter-passing robot in *Rick and Morty* asks about its purpose and
hears: "You pass butter." The robot is small, complete, and sure of its job.
That is the butter lesson: a tool should know its one purpose and serve it
completely.

- Looper passes butter: it keeps a local development loop running. It plans
  tasks, runs coding agents in private worktrees, checks the combined
  result, and publishes only checked work.
- Not our job: web interfaces, remote services, vendor SDKs, pull requests,
  model routing, chasing features that the agent CLIs will ship anyway.
- Note the near name: Sutton's "Bitter Lesson" says scale beats hand-built
  structure. Our answer is narrower and compatible. Do not compete with the
  big runtimes. Do the one deterministic thing they do not.

## Simple before easy

Rich Hickey separates the two. Simple means one braid, no interleaving; it
is an objective property. Easy means near at hand; it is a feeling, and it
lies. A familiar framework is easy and can still complect the code.

- Ask Cunningham's question before each feature: what is the simplest thing
  that could possibly work? Then build that.
- Prefer one obvious way (PEP 20). When a second way exists, delete one.
- Gabriel's rule beats perfectionism: it is slightly better to be simple
  than correct, and the simple thing that ships has better survival
  characteristics than the right thing that does not.

## Do not make me think

Steve Krug's first law of usability. Applied to terminals through
clig.dev: make the default the right thing for most users.

- The bare command `looper` must be the right run for most projects. Every
  added flag must argue against that default, not around it.
- Plain text when piped. Color never the only signal. Every error names its
  fix.
- Each screen answers one question: what is happening, and what happens
  next. Fewer states beat better explanations.

## Believe in the loop

The loop is the product. Plan, implement in a fresh worktree, check the
combined result, publish, review, wait for more. A fresh context per task
beats a long session with accumulated corrections.

- Keep the loop's state boring: SQLite, a journal, exit codes. Deterministic
  beats clever inside the loop.
- When a turn fails, make the next turn smaller, not smarter.
- The loop never ends by itself. It idles and waits for the next human edit.
  That patience is the feature the native tools do not have.

## Take things away

Saint-Exupéry: perfection is reached when there is nothing left to take
away. Rams: less, but better.

- Every release removes as much as it adds. Dead states, unread fields,
  compatibility aliases, and optional layers are debt.
- Remove them or wire them. Nothing ships "just in case".
- A feature that needs a paragraph to justify needs a deadline to prove
  value or die.
