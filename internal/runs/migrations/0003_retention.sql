-- Result retention (ADR 0110, step 4 ruling 1): once a run has been ended for
-- sessions.resultRetention, its content is cleared: a session's statement code, output and error,
-- and any run's saved log tail. Its state and timestamps are kept. content_cleared_at says when.
ALTER TABLE runs ADD COLUMN content_cleared_at timestamptz;
CREATE INDEX runs_uncleared ON runs (finished_at) WHERE finished_at IS NOT NULL AND content_cleared_at IS NULL;
