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

## Workflow

1. Read `to-do.schema.json` if present and follow it strictly.
2. Read all files listed in `source_files` and treat them as ground truth.
3. Use `jq` to inspect tasks and identify the next task.
   - Choose the lowest priority number among tasks with status "todo".
   - Break ties by lexicographic `id`.
   - If no "todo" tasks exist, select the highest priority "blocked" task to attempt unblocking.
4. Set the chosen task status to "doing" before starting work.
5. On completion, set status to "done", update `updated_at`, and add relevant `files` or `details`.
6. If blocked, set status to "blocked" and add clear `blockers` entries.
7. Keep `to-do.json` formatted with 2-space indentation.

## jq Tips

```bash
jq '.tasks |= map(if .id=="T001" then .status="doing" else . end)' to-do.json > /tmp/to-do.json && mv /tmp/to-do.json to-do.json
```
