-- The log cursor (GET /v1/applications/{id}/logs?from=<offset>, ADR 0119 amendment 3): where the
-- driver log kept when a run ended (log_tail) starts in its whole log, in bytes, so offsets read
-- while the run lived go on meaning the same after it ends. 0 for runs that ended before this.
ALTER TABLE runs ADD COLUMN log_tail_offset bigint NOT NULL DEFAULT 0;
