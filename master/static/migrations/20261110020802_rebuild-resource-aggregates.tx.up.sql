-- resource_aggregates_legacy holds the aggregates computed before this change, kept for reference;
-- nothing reads it. Under the old rule a task still running when its day was aggregated was not
-- counted for that day. It still includes the usage of tasks whose records were deleted since,
-- which the rebuild cannot recompute.
CREATE TABLE resource_aggregates_legacy (LIKE resource_aggregates INCLUDING ALL);
INSERT INTO resource_aggregates_legacy SELECT * FROM resource_aggregates;

-- The aggregates missed the usage of allocations still running when their day was aggregated.
-- They are derived from the allocations, so the master rebuilds them all at its next start.
TRUNCATE resource_aggregates;
