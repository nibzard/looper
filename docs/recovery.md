# State and recovery

The default state directory is:

```text
~/.looper/<project-name>-<path-hash>/runtime/
  state.db
  history/
  worktrees/
  task-*/
  plan-*/
  review-*/
```

SQLite stores execution state. Edit `to-do.json` to change the backlog;
Looper imports those edits. A journal tracks task-file exports and code
publication. After a restart, Looper recognizes whether an operation was
published and resumes it without marking unchecked work done. Completed worker receipts avoid repeating
implementation after a controller crash. Invalid final reports get a
separate report recovery attempt with code changes prohibited.

Plan imports keep accepted scheduling adjustments separate from unresolved
conflicts. Optional ordering before a completed task can become follow-up work
after that task. The import receipt and acknowledgement retain the adjustment.
Imports can also add prerequisites to an existing idle task through `gates`.
These additions cannot remove dependencies, create cycles, or change completed
or retained work. A follow-up report preserves an existing task's ownership.
Use a separate task for a distinct operator prerequisite.

Before waiting on blocked work, Looper can repair proven orchestration defects.
It consults the review agent once for unchanged source and command contracts.
Repairs apply after workers and checks drain. Looper retains implementation
work and runs integration checks with the corrected commands. The repair and
its retry limit survive restart. Unreadable product requirements and external
prerequisites stay blocked.

Recovery events distinguish consultation, application, integration, and successor
dispatch. Verification that changes project files reports the paths and retains
`file-changes.txt` beside the check logs. Status shows both executable and
controller builds when they differ. An installed update takes effect for a
controller after restart.

Your branch, `HEAD`, and staged index remain unchanged. Successful changes
appear as unstaged edits. Looper retains private refs for snapshots and
results. It does not automatically commit, push, or open pull requests on
your branch.

A repository lock covers all linked worktrees. Concurrent outside edits
during integration stop publication and retain the result. An external
task-file edit imports at the next boundary where no work is in flight, and
at startup when every retained attempt holds a replayable result — a result
commit or a worker receipt. Work that would rerun its worker still asks you
to restore the saved baseline and task contract before resuming. Do not delete
the runtime directory or private refs while you need recovery. Logs and refs
are retained; there is no automatic retention limit yet.

`looper recovery --preview` explains what a guard refused before you change
anything. The preview is read-only: it changes no file, database record,
process, or Git ref. It compares the task file, branch, working tree, and
any retained publication against the saved contract, reports each startup
guard verdict, and lists the supported recovery choices with what each
preserves or changes. Each preview carries a token; `looper recovery --apply
<choice> --token <token>` applies one enumerated choice and re-derives the
token first, so a stale preview is refused without changing anything. A live
controller applies the request at its boundary; a stopped run applies under
the repository lock without launching agents. Add `--contract v2 --json` for
the machine document.

`looper status --json` returns the saved state. `looper tail --json` reads
events. `looper doctor` checks local dependencies and the backlog. Exit
codes are 0 for a completed finite run or drained stop, 2 for blocked work
or a dispatch limit, 1 for a controller failure, and 130 for interruption.

`looper preflight` shows the execution configuration a start would use before
any agent launches: which agent binaries resolve where, which agents, models,
and effort apply and where each value came from (flag, environment, saved
profile, the saved plan, or a planner choice), the launch mode, once
semantics, and the effective verify and setup commands. The preview is
read-only — no controller starts, no agent runs, nothing is billed — and
credentials, environment values, and raw agent arguments stay out of it.
What Looper cannot verify without launching an agent is listed as unchecked.
A missing agent binary or core tool exits 1 like doctor. Add
`--contract v2 --json` for the machine document; it stays within 8 KiB
whatever the configured values hold. A foreground `looper` run
prints the same facts as one concise launch line before it starts; on a
resume the line's verify and setup commands name the layer that will supply
them (the configuration, a recovered command, or the saved plan), because it
reads the saved run read-only like preflight.

`looper inspect <task> --contract v2 [--json]` returns one bounded task
view: the contract and blockers, the current attempt with a stated feedback
excerpt, the newest checks, timing provenance, and the routes that page the
attempt (`--after`) and check (`--checks-after`) history without duplication
or gaps. The document stays within 8 KiB no matter how many repairs a task
accumulated; `--full` returns the complete unpaged report.

`looper runs [--limit N]` lists the saved runs under the configured base
directory (`LOOPER_BASE_DIR`, default `~/.looper`), newest first: each run's
project root, whether it still exists, its retained work, its session count
with the last recorded outcome, controller freshness (owning, stale,
stopped, or unknown), build evidence, and the exact `cd` command that
selects it. The listing is read-only — no run is started, stopped, or
changed — and reads only Looper-owned runtime directories; private agent
session directories are never scanned. `--limit` bounds the page (default
20, at most 100) and the document reports what it omitted. An empty base
directory is a bounded result with a start hint. Add `--contract v2 --json`
for the machine document. No-run errors from other commands name the
resolved project root, task file, and searched base and runtime locations,
and point at `looper runs` for discovery.

## Resume after committing published work

If you commit files that Looper has already published, restart with `looper`.
Looper accepts a descendant commit on the same branch when the committed files
and working files match its accepted tree or its verified publication candidate,
or when the commit changes no file inside a retained or published attempt's
write scope. It preserves your commit and finishes retained publication through
the usual checks.
Staged project changes, other file edits, branch switches, and changed backlog
contracts require reconciliation. `looper doctor --json` reports a moved branch
and the last recorded shutdown before you attempt recovery.

## Commit to the project while a run is live

You can commit files the run does not depend on while the controller keeps a
run live. When the commit is a descendant of the run's baseline and changes no
file inside a retained or published attempt's write scope, the next boundary
accepts it: the run folds the commit into its baseline, records a `recovery`
event that names the accepted commit and its changed paths, and continues.
A held publication re-verifies against the folded baseline before it
publishes. A commit that changes a file inside a write scope competes with
retained work; the run retains everything and the resume requires
reconciliation.

## Edit the task file during a run

In a continuous run, an invalid task file at an idle boundary pauses the
loop instead of stopping it. Looper keeps the last accepted state, leaves
your file untouched, and waits for a corrected edit. The wait reports the
file path, the problem, and the action, and clears when any valid edit
arrives. Queued plan imports wait with it; requeue requests receive a
conflict with a correction hint. Stop still drains with code 0, an interrupt
returns 130, and invalid input at startup or under `--once` exits with
code 1.

An external edit that lands while work runs pauses dispatch the same way.
The edit imports at the first boundary with nothing in flight, and the
interrupted results continue against the new baseline. A stop during that
wait drains at the same boundary: the results stay retained and your edited
file stays untouched.

## Stop a run

`looper stop` stops new dispatch and lets active work finish. A stop request
binds to the controller that owns the repository. `looper stop` writes a
request only while a controller runs, so a stop without a running controller
changes nothing and a later start runs normally. A request that arrives
during startup still drains that controller at its first boundary. A request
left by a controller that already exited is discarded when the next
controller starts. A request from an older Looper version names no owner.
The next controller honors such a request only when it arrived after that
controller started. An interrupt stops child processes and retains partial
work. Run `looper` again to resume.

`looper stop --interrupt` sends the same interrupt to a detached controller.
Looper checks the recorded session and process start time first, so it
never signals a process that reused the controller's process ID.
