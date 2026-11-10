-- User usage metering: when the resource manager assigned the allocation's resources, when they
-- were released, and what they are metered as. All NULL on an allocation that never got resources.
-- The default marks the allocations from before metering without rewriting the table; new rows get
-- NULL once it is dropped.
ALTER TABLE allocations
    ADD COLUMN assigned_time timestamp without time zone NULL,
    ADD COLUMN released_time timestamp without time zone NULL,
    ADD COLUMN metered_kind text NULL DEFAULT 'unclassified'
        CONSTRAINT allocations_metered_kind_check
        CHECK (metered_kind IN ('gpu', 'cpu', 'aux', 'none', 'unclassified')),
    ADD COLUMN metered_quantity smallint NULL;
ALTER TABLE allocations ALTER COLUMN metered_kind DROP DEFAULT;

-- Allocations from before metering that still wait for their pool's slot type.
CREATE INDEX ix_allocations_unclassified_pool ON allocations (resource_pool)
WHERE metered_kind = 'unclassified' AND assigned_time IS NULL;
