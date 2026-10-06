ALTER TABLE dynamic_resource_pools
    DROP CONSTRAINT dynamic_resource_pools_spec_complete,
    DROP COLUMN revision,
    DROP COLUMN spec_hash,
    DROP COLUMN spec_version,
    DROP COLUMN spec;
