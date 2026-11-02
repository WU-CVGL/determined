-- List polling reads the earliest completed QUEUED event for a bounded set of allocations.
-- Include end_time so this access path can use an index-only scan on historical stats.
CREATE INDEX task_stats_queued_allocation_end_idx
ON task_stats (allocation_id, end_time)
WHERE event_type = 'QUEUED' AND end_time IS NOT NULL;
