---
name: todo-json-manager
description: Manage project task lists stored in to-do.json and to-do.schema.json, including bootstrapping a backlog, selecting the highest priority task with jq, and updating task statuses. Use when Codex needs to read or update a JSON task list.
---

# Todo JSON Manager

## Overview

Maintain the project task list in `to-do.json` using the schema in `to-do.schema.json`.

## Top-level Fields

Required: schema_version, source_files, tasks.

source_files: array of relative paths to ground-truth docs (PROJECT.md, SPECS.md, etc.).

## Task Fields

Required: id, title, priority (1-5), status (todo|doing|blocked|done).

Optional: details, steps, blockers, tags, files, depends_on, created_at, updated_at.

## Looper iterations

Looper selects the task and owns task status. During a Looper iteration, read
the selected task and its `source_files`. Do not edit `to-do.json`. Report a
structured summary with the task ID, `done` or `blocked` status, changed files,
and blocker reasons. Looper applies the status after validation and optional
verification. It does not retry blocked tasks automatically.

## Manual task file maintenance

1. Read `to-do.schema.json` if present and follow it strictly.
2. Keep task IDs unique. Each `depends_on` ID must name another task.
3. Select an existing `doing` task first. Otherwise, select the highest
   priority `todo` task whose dependencies are done.
4. Leave `blocked` tasks blocked until their cause is resolved. Then set them
   to `todo` for a new attempt.
5. Use `jq` for edits. Keep `to-do.json` formatted with 2-space indentation.

## jq Tips

```bash
jq '.tasks |= map(if .id=="T001" then .status="doing" else . end)' to-do.json > /tmp/to-do.json && mv /tmp/to-do.json to-do.json
```
