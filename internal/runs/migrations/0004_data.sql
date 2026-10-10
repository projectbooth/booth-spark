-- Data access (build step 5; docs/design-v0.md item 4). A run that reads or writes data has a
-- data bearer: its agents present it on the backend's internal port to fetch the run's current
-- workload token. Generated per run, delivered as a Secret in the run's own namespace, never shown
-- through the API, cleared with the run's content (sessions.resultRetention).
ALTER TABLE runs ADD COLUMN data_bearer text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX runs_data_bearer ON runs (data_bearer) WHERE data_bearer <> '';
-- The run's data access as resolved before launch (runs.DataPlan): the token's role and where each
-- location is. No credential.
ALTER TABLE runs ADD COLUMN data jsonb;
