-- The user usage read selects a user's jobs by owner.
CREATE INDEX ix_jobs_owner_id ON jobs (owner_id);
