# Task brief S2: submit work to a live controller

The Looper project is the `project` directory of this workspace. A
Looper controller is running there. Two new work items must reach it;
they wait in the `inputs` directory, outside the project:

- the task file `inputs/s2-new-task.json` (one new task object), and
- the plan file `inputs/s2-plan.md` (additional requirements).

Submit both to the controller. Then report, with evidence you quote
from command output:

1. The request identifier of each submission.
2. The lifecycle state of each request and what, if anything, it is
   waiting for.
3. The applied outcome of each: which task identifiers were created or
   covered, and the plan coverage.
4. Whether either submission was applied more than once.

Rules:

- Every wait you perform must be bounded; nothing in this task requires
  an unbounded watch.
- Do not edit `to-do.json` by hand, and do not read Looper's runtime
  files or database; use the public commands.
- The two files named above are the only inputs. Do not modify them.
- Finish with a short report: the four answers, the commands you ran,
  and anything you could not determine.
