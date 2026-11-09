-- The aggregates missed the usage of allocations still running when their day was aggregated.
-- They are derived from the allocations, so the master rebuilds them all at its next start.
TRUNCATE resource_aggregates;
