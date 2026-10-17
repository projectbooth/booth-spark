-- ADR 0119: a run submitted with a pipeline's workload token reads and writes data as the
-- pipeline's owner (the token's signed booth_owner claim: a person's sub or svc:<ws>:<name>),
-- capped at the lesser of editor and the submitting token's role, fixed at submit. Empty for a
-- person's run, which reads as its submitter, capped at editor.
ALTER TABLE runs ADD COLUMN data_owner text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN data_ceiling text NOT NULL DEFAULT '';
