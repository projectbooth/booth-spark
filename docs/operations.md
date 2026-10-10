# booth-spark operations

This is the operator's doc for booth-spark (ADR 0110). Build step 6 completes it: placement, the
second-node recipe, egress, residuals 1 to 6 of `docs/design-v0.md` item 4, and the UI. Until
then, this page holds the limits the coordinator ruled must be written down.

## Sessions: known limits in v0

- **Statements run one at a time, and there is no way to cancel one.** A session runs its
  statements in order, one at a time. v0 has no cancel endpoint and no per-statement timeout. A
  runaway statement (an endless loop, a query that never finishes) keeps running until the session
  reaches `sessions.maxLifetime` (default 12h), or until its submitter or a workspace owner deletes
  the session. Until then it holds the session, its driver and its executors, and every later
  statement waits behind it. Deleting the session ends everything in it.

  A future cancel would use Spark's `cancelJobGroup`, which the runner already sets up for each
  statement. That would stop only the statement's Spark jobs. It would not stop plain Python
  running in the driver (a loop, a `time.sleep`, driver-side computation): CPython has no safe way
  to interrupt another thread. Deleting the session would remain the only way to stop that.

- **Platform operators can't see or delete sessions.** Only a session's submitter and the
  workspace's owners can. An operator who needs a node back can't end someone's session through
  booth-spark. What bounds a session is its idle timeout (`sessions.idleTimeout`, default 20m,
  with nothing waiting or running) and its maximum lifetime (`sessions.maxLifetime`, default 12h,
  busy or not). An operator with cluster access can still delete the session's namespace
  (`bspark-<id>`); the controller then records the session as failed.

## Result retention

`sessions.resultRetention` (default `168h`, 7 days) is how long an ended run's content is kept.
After that, the controller clears:
- a session's statement code, output and error;
- the driver log tail kept when any run (session or application) ended.

The run's state, reason and timestamps stay, and `contentClearedAt` records when it was cleared. A
cleared run's log answers `410` with code `cleared`. Clearing runs with the controller's sweep,
about every 30 seconds, so content can outlive the retention by that much.

Until then, the content is in the module's own database, and it can hold workspace data: what a
query returned, or what a run printed.
