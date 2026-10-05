-- Resource pool access (Pool ACL v1). A pool without a restriction row is public. A restricted pool
-- may be used by administrators and by the users granted access to it. Rows are keyed by pool name
-- and have no foreign key to a pool, so a pool can be restricted before it exists and a re-created
-- pool gets its records back. A grant on a public pool is stored and applies once it is restricted.
CREATE TABLE resource_pool_restrictions (
    pool_name     TEXT PRIMARY KEY CHECK (pool_name <> ''),
    restricted_by INT REFERENCES users(id) ON DELETE SET NULL,
    restricted_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE resource_pool_grants (
    pool_name  TEXT NOT NULL CHECK (pool_name <> ''),
    user_id    INT  NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    granted_by INT  REFERENCES users(id) ON DELETE SET NULL,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (pool_name, user_id)
);
