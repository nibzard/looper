---
name: todo-json-manager
description: Read or maintain JSON task backlogs, including task selection, dependencies, acceptance criteria, and blocker recovery. Use for to-do.json workflows and Looper task summaries.
---

# Manage a task backlog

Use the project's task file and schema as the source of truth.
Read the relevant `source_files` before adding or completing work.
Use `python3` if Python is needed. Prefer `jq` for small JSON edits.

## During a Looper task

Looper selects the task and owns its status. Complete the selected task.
Read its details, acceptance criteria, and relevant files.
Keep changes within that task. Do not edit the backlog during execution.
Bootstrap, repair, and final review are separate phases. Edit the backlog
only as directed by the prompt for that phase.

Verify the required outcome before reporting `done`.
Include the checks and their results in `summary`.
Report `blocked` when required input or external work is unavailable.
Name the missing input and the action needed to resolve it.
Do not invent human approval, test results, data, or completed work.

Return the summary fields that Looper accepts:

```json
{
  "task_id": "T063",
  "status": "blocked",
  "summary": "The challenge labels need human approval.",
  "files": [],
  "blockers": ["A reviewer must approve the challenge labels."]
}
```

Use the selected task ID. Include only files affected by this task.
Use `done` or `blocked`; include at least one reason for `blocked`.
Do not add extra fields unless the active output schema permits them.
Looper validates the summary and applies any configured verification gate.

## Manual maintenance and bootstrap

1. Read the actual schema. Preserve unrelated tasks and metadata.
   For a new backlog, derive tasks from project documents and list those
   documents in `source_files`.
2. Give each task a unique ID and an observable acceptance criterion.
   Keep task scope small enough to verify in one iteration.
3. Make dependencies refer to existing tasks. Reject self-dependencies
   and dependency cycles. Priority 1 is highest.
4. Select an existing `doing` task first. Otherwise, select the highest
   priority `todo` task whose dependencies are done. Looper uses task IDs
   to break ties, with numeric suffixes sorted numerically.
5. Keep blocked tasks blocked until the cause is resolved. Then set them
   to `todo`. Do not treat unresolved blockers as successful completion.
6. Write edits to a unique temporary file beside the task file.
   For example, use `mktemp ./to-do.json.XXXXXX`.
   Validate the candidate against the schema and dependency rules before
   replacing the task file. Remove the temporary file on failure.

A final review checks completed work against its acceptance criteria.
Add tasks for observed gaps. Add the `project-done` marker only when the
review finds no remaining work, as directed by Looper's review prompt.
