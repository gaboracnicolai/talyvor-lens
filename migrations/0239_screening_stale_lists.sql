-- B37.4 — a payment screened while the sanctions lists are older than LENS_SCREENING_MAX_AGE_HOURS is held, as a close
-- match is: its case's outcome is stale, for an operator to release or refuse.

ALTER TABLE compliance_cases DROP CONSTRAINT IF EXISTS compliance_cases_outcome_check;
ALTER TABLE compliance_cases ADD CONSTRAINT compliance_cases_outcome_check CHECK (outcome IN ('hit', 'review', 'alert', 'stale'));
