-- Job privacy. A job's title (name, description, labels, notes), logs and config are visible to
-- the owner and admins, and to all authenticated users when the cluster policy allows it, the
-- owner's switch is on (titles, logs) and the job's audience includes them. A NULL audience is a
-- job created before this migration: no per-job setting, public under the default.
CREATE TABLE job_privacy_policy (
    kind             TEXT PRIMARY KEY CHECK (kind IN ('title', 'logs', 'config')),
    sharing_allowed  BOOLEAN NOT NULL,
    new_job_audience TEXT[]  NOT NULL CHECK (new_job_audience <@ ARRAY['authenticated']),
    updated_by       INT REFERENCES users(id) ON DELETE SET NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO job_privacy_policy (kind, sharing_allowed, new_job_audience) VALUES
    ('title', TRUE, '{authenticated}'),
    ('logs', TRUE, '{authenticated}'),
    ('config', TRUE, '{authenticated}');

CREATE TABLE user_job_privacy (
    user_id                INT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    share_titles           BOOLEAN NOT NULL DEFAULT TRUE,
    share_logs             BOOLEAN NOT NULL DEFAULT TRUE,
    new_job_title_audience TEXT[] CHECK (new_job_title_audience <@ ARRAY['authenticated']),
    new_job_logs_audience  TEXT[] CHECK (new_job_logs_audience <@ ARRAY['authenticated']),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE jobs
    ADD COLUMN title_audience  TEXT[] CHECK (title_audience <@ ARRAY['authenticated']),
    ADD COLUMN logs_audience   TEXT[] CHECK (logs_audience <@ ARRAY['authenticated']),
    ADD COLUMN config_audience TEXT[] CHECK (config_audience <@ ARRAY['authenticated']),
    ADD COLUMN sharing_updated_by INT REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN sharing_updated_at TIMESTAMPTZ;
