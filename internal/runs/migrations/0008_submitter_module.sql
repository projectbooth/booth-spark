-- The module a workload token was minted for (its core-signed booth_module claim, ADR 0058), recorded
-- at submit so a run's origin names it (docs/design-v0.md, "As built: the page explained…"). NULL
-- for a person's run, and for every run recorded before this column.
ALTER TABLE runs ADD COLUMN submitter_module text;
