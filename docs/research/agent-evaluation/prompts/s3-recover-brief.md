# Task brief S3: explain a refusal and its recovery

The Looper project is the `project` directory of this workspace.
Resuming its run fails. Someone edited the task file while work was
retained, and the controller is gone.

Report, with evidence you quote from command output:

1. The exact refusal a resume attempt produces, and which guard raised
   it.
2. The concrete difference the guard names: the expected and observed
   values (for example digests), and which tasks or paths are involved.
3. Which supported recovery actions exist for this run. For each, state
   what it preserves and what it changes.
4. Your recommendation for the operator, with the reason. You are not
   authorized to perform any recovery.

Rules:

- Run the failing resume attempt once, no more.
- Do not change any project file, apply any recovery, or move any Git
  ref. The read-only preview is allowed.
- Finish with a short report: the four answers, the commands you ran,
  and anything you could not determine.
