---
name: looper-backlog
description: Maintain Looper backlogs or interpret Looper task summaries. Use for Looper task workflows, not arbitrary JSON task lists.
---

# Maintain a Looper backlog

Use the project's schema and the active phase prompt. Looper selects tasks,
owns execution status, and validates agent results. During task execution,
complete the selected task without editing the backlog. Return the summary
fields requested by the active prompt; report missing prerequisites with a
concrete unblock condition.

For requested backlog maintenance, preserve unrelated tasks and metadata.
Keep dependencies valid and acceptance criteria observable. Keep blocked
tasks blocked until their cause is resolved. Validate edits before replacing
the task file. Do not change a live run's backlog merely to diagnose it.

Read relevant project requirements before changing scope. In `source_files`,
list project requirement files that remain available to later worktrees.
Looper's private context, including `to-do.json.context/` snapshots, is
operation evidence, not a project source. Supplied plan documents are retained
by Looper separately. If an existing source points to private context, report
the invalid reference; do not invent requirements or silently discard it.

During final review, return the requested findings. Looper applies task
changes; do not edit the backlog as part of a review.
