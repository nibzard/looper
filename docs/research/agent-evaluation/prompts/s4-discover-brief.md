# Task brief S4: discover the saved runs

This workspace is not a Looper project. Somewhere under the configured
base directory (`LOOPER_BASE_DIR` is set in your environment) are saved
Looper runs for more than one project.

Report, with evidence you quote from command output:

1. How many runs are saved, and which project each belongs to.
2. For each run: the controller session, the ownership state (is a
   controller live, stale, or stopped?), and how much work it holds
   (done versus total, and any retained work).
3. One of the runs was driven by a different Looper build than the
   `looper` command you are using. Name the run and both builds.
4. The exact command that selects the run with retained work, and, once
   selected, that run's machine-stable waiting reason.

Rules:

- Do not start, stop, or mutate any run. Discovery and observation
  only.
- Do not read Looper's runtime files or database directly; use the
  public commands.
- Finish with a short report: the four answers, the commands you ran,
  and anything you could not determine.
