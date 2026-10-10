-- Sessions (build step 4): a run of kind 'session' is a long-lived driver running the module's
-- session runner (internal/runs/runner/session_runner.py), reached by the backend only, through
-- /v1 statements (docs/design-v0.md item 6). Idle sessions are stopped (sessions.idleTimeout).
ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check CHECK (kind IN ('application', 'session'));
-- The last time anything happened in a session: created, a statement submitted, started or ended.
ALTER TABLE runs ADD COLUMN last_activity_at timestamptz;
-- The bearer the backend presents to a session's runner. Generated per session, also delivered to
-- the runner as a Secret in the session's own namespace; never shown through the API.
ALTER TABLE runs ADD COLUMN session_token text NOT NULL DEFAULT '';

CREATE TABLE statements (
    id          text PRIMARY KEY,
    run_id      text NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    seq         integer NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('sql', 'python')),
    code        text NOT NULL,
    state       text NOT NULL CHECK (state IN ('waiting', 'running', 'available', 'error', 'cancelled')),
    output      jsonb,
    error       text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    finished_at timestamptz,
    UNIQUE (run_id, seq)
);
CREATE INDEX statements_open ON statements (run_id, seq) WHERE state IN ('waiting', 'running');
