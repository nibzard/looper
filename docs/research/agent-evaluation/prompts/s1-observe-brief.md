# Task brief S1: observe a saved run

The Looper project is the `project` directory of this workspace. A
Looper run has finished there. Your job is to report its state and its
most useful evidence, then leave the project exactly as you found it.

Answer these questions with evidence you quote from command output:

1. Which run and controller session does this project hold, and in what
   state is the controller?
2. How many tasks are done, ready, and blocked? If anything is blocked,
   name the task and its blocker.
3. What was the last durable transition (event id, kind, and what it
   says)?
4. From the position you just reported, what is the exact next command
   that reads only events you have not seen yet?
5. Which task failed verification most recently? Report the failing
   operation, the stable outcome, the exit status, the task, and where
   the retained evidence for that failure lives.

Rules:

- Do not change any file, run any controller, or apply any recovery.
- Finish with a short report: the five answers, the commands you ran,
  and anything you could not determine.
