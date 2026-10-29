-- Why a pending run is still waiting at launch (its data access being resolved and retried, or its
-- driver not yet scheduled), shown on the page and in /v1 as waitingReason while the run is pending.
-- NULL once it starts or ends, and for every run recorded before this column.
ALTER TABLE runs ADD COLUMN waiting_reason text;
