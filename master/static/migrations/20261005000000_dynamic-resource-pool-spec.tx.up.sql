ALTER TABLE dynamic_resource_pools
    ADD COLUMN spec JSONB,
    ADD COLUMN spec_version INTEGER,
    ADD COLUMN spec_hash TEXT,
    ADD COLUMN revision BIGINT NOT NULL DEFAULT 1,
    ADD CONSTRAINT dynamic_resource_pools_spec_complete CHECK (
        (spec IS NULL) = (spec_version IS NULL) AND (spec IS NULL) = (spec_hash IS NULL)
    );
