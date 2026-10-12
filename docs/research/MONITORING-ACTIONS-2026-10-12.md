# Actions from monitoring, 2026-10-12

## Exit 126

The retained margin snapshot has 28 blocked tasks with exit 126. Their logs
are empty. Their prompts range from 1,727 to 2,331 bytes. The old records do
not identify the underlying failure. Current executable permissions cannot
prove the launcher's condition at the time of failure.

An isolated fixture distinguishes a non-executable target from a target that
starts and returns 126. Current handling stops scheduling on a confirmed launch
failure and retains work. No shared circuit breaker is justified by this evidence.
The next occurrence needs its structured launch report and launcher identity.

## Cost provenance

A retained commonstate report has 43,181 uncached input tokens, 1,822,272 cache
read tokens, and 14,342 output tokens. The pinned snapshot uses rates of 1.4,
0.26, and 4.4 USD per million tokens respectively. The calculation produces
0.59734892 USD, exactly matching the saved estimate. The agent reports
1.485591 USD and explicitly labels its cost basis unknown.

The arithmetic is correct for the selected pinned rates. The evidence does not
establish an actual charge or the agent's rate basis. Preserve the public agent
provider and cost basis, including unknown values. Do not apply a multiplier.
Older saved reports keep their original estimates and unknown provenance.

## Review visibility

The notes record a 27.5-minute milestone review with no events after its start.
The source calls the review agent synchronously. Add supervised log metadata
and 30-second heartbeats while preserving agent tools and acceptance checks.
Regression coverage includes a quiet agent and controller cancellation.

## Project blockers

Commonstate has 339 pending file changes at inspection. Keep its existing edits
and live controller separate from Looper changes.

- Docker: retained checks fail during `pnpm db:up` with exhausted address pools.
  The daemon currently has 32 bridge networks. Use a scoped network allocation
  or an existing-network configuration for the affected Compose deployment.
  Validate setup in an isolated check workspace, then reopen only affected tasks.
  A worker-only fallback does not prove that integration setup succeeds.
- Hosts: provide the required reachable 26.2 host pair and project credentials.
  Confirm the named readiness conditions before reopening infrastructure tasks.
- Policy: C-18 needs configurable submission expiry with database time as the
  authority. The user must choose an operations setting or a bounded request field.
  Keep the seven-day default until that choice is made.

No live controller is restarted and no blocked task is requeued during diagnosis.
The Looper release supplies diagnostics; it does not satisfy missing host or
product-policy conditions.

## Verified database recovery

A fresh 18:40Z observation confirms that margin is stopped with 28 blockers.
Commonstate owns its repository, has 84 completed tasks, and retains the same
13 blockers. Its accepted setup already replaces failing Compose creation with
a uniquely named PostgreSQL container on the default bridge.

The isolated fixture runs that database portion of the accepted setup, including
`pnpm db:check`, against source from the accepted tip. The dependency lock matches
the project. PostgreSQL 18.6 passes the SQL round trip in 36 milliseconds.
The temporary container and anonymous volume are removed successfully. No
existing container, network, or project source is changed.

Submit guarded requeue requests for T057, T059, T100, T102, T104, T106, T107,
T109, and T111. Each request binds to the current session and task revision.
The controller alone changes status. Requests remain pending during synchronous
review; report acknowledgements separately from submission. Host and policy
blockers remain blocked until their conditions are resolved.
