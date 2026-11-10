-- User usage metering: when the resource manager assigned the allocation's resources, when they
-- were released, and what they are metered as. All NULL on an allocation that never got resources.
-- The default marks the allocations from before metering (Postgres 10 rewrites the table for it,
-- 11 and later do not); new rows get NULL once it is dropped.
ALTER TABLE allocations
    ADD COLUMN assigned_time timestamp without time zone NULL,
    ADD COLUMN released_time timestamp without time zone NULL,
    ADD COLUMN metered_kind text NULL DEFAULT 'unclassified'
        CONSTRAINT allocations_metered_kind_check
        CHECK (metered_kind IN ('gpu', 'cpu', 'aux', 'none', 'unclassified')),
    ADD COLUMN metered_quantity smallint NULL;
ALTER TABLE allocations ALTER COLUMN metered_kind DROP DEFAULT;

-- An allocation from before metering with slots whose task container recorded GPUs held GPUs, one
-- per slot. The kind of every other one is not known.
UPDATE allocations a SET metered_kind = 'gpu', metered_quantity = a.slots
WHERE a.slots > 0
  AND EXISTS (
      SELECT 1 FROM allocation_accelerators x
      WHERE x.allocation_id = a.allocation_id
        AND x.accelerator_type IN ('cuda', 'rocm')
        AND cardinality(x.accelerator_uuids) > 0
  );
