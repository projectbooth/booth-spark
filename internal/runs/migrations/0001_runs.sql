-- One row per run (docs/design-v0.md item 6). A live run (pending, running) also has a namespace,
-- bspark-<id>, which the controller deletes when the run ends.
CREATE TABLE runs (
    id              text PRIMARY KEY,
    kind            text NOT NULL CHECK (kind IN ('application')),
    workspace       text NOT NULL,
    submitter       text NOT NULL,
    submitter_name  text NOT NULL DEFAULT '',
    workload        boolean NOT NULL DEFAULT false,
    name            text NOT NULL,
    state           text NOT NULL CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'stopped')),
    reason          text NOT NULL DEFAULT '',
    namespace       text NOT NULL,
    launched        boolean NOT NULL DEFAULT false,
    stop_requested  boolean NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    started_at      timestamptz,
    finished_at     timestamptz,
    footprint_mi    integer NOT NULL,
    spec            jsonb NOT NULL,
    -- The last part of the driver's log, kept when the run ends (its pods are gone after that).
    log_tail        text NOT NULL DEFAULT '',
    idempotency_key text,
    UNIQUE (workspace, submitter, idempotency_key)
);
CREATE INDEX runs_live ON runs (state) WHERE state IN ('pending', 'running');
CREATE INDEX runs_workspace ON runs (workspace, created_at DESC);
