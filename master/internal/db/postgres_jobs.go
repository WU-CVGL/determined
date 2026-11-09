package db

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/determined-ai/determined/master/pkg/model"
)

// The audiences a new job is stamped with, one SQL expression per content kind of job privacy.
// The title and logs expressions take the job's owner ID as their only parameter. A kind that the
// cluster policy allows gets the owner's new-job audience (titles and logs only) or else the
// policy's; a kind that the policy forbids, or whose policy row is missing, gets {}, so a later
// allow does not publish it. An ownerless job has no preference row and takes the policy's
// audience.
const (
	stampTitleAudience = `CASE
		WHEN COALESCE((SELECT sharing_allowed FROM job_privacy_policy WHERE kind = 'title'), FALSE)
		THEN COALESCE(
			(SELECT new_job_title_audience FROM user_job_privacy WHERE user_id = ?),
			(SELECT new_job_audience FROM job_privacy_policy WHERE kind = 'title'))
		ELSE '{}'::text[] END`
	stampLogsAudience = `CASE
		WHEN COALESCE((SELECT sharing_allowed FROM job_privacy_policy WHERE kind = 'logs'), FALSE)
		THEN COALESCE(
			(SELECT new_job_logs_audience FROM user_job_privacy WHERE user_id = ?),
			(SELECT new_job_audience FROM job_privacy_policy WHERE kind = 'logs'))
		ELSE '{}'::text[] END`
	// The config has no owner preference.
	stampConfigAudience = `CASE
		WHEN COALESCE((SELECT sharing_allowed FROM job_privacy_policy WHERE kind = 'config'), FALSE)
		THEN (SELECT new_job_audience FROM job_privacy_policy WHERE kind = 'config')
		ELSE '{}'::text[] END`
)

// AddJobTx persists the existence of a job with a transaction. It is the only function that
// inserts jobs. The INSERT itself stamps the job's privacy audiences from the cluster policy and
// the owner's preferences, so a job never exists without them: if they cannot be read, no job is
// inserted. The stamped audiences are written back to j; the audience and sharing fields that the
// caller set are ignored.
func AddJobTx(ctx context.Context, idb bun.IDB, j *model.Job) error {
	if idb == nil {
		idb = Bun()
	}

	if _, err := idb.NewInsert().Model(j).
		ExcludeColumn("sharing_updated_by", "sharing_updated_at").
		Value("title_audience", stampTitleAudience, j.OwnerID).
		Value("logs_audience", stampLogsAudience, j.OwnerID).
		Value("config_audience", stampConfigAudience).
		Returning("title_audience, logs_audience, config_audience").
		Exec(ctx); err != nil {
		return fmt.Errorf("adding job: %w", err)
	}

	return nil
}

// AddJob persists the existence of a job.
func AddJob(j *model.Job) error {
	return AddJobTx(context.TODO(), Bun(), j)
}

// JobByID retrieves a job by ID.
func JobByID(ctx context.Context, jobID model.JobID) (*model.Job, error) {
	var j model.Job
	err := Bun().NewSelect().Model(&j).
		Where("job_id = ?", jobID).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("querying job: %w", err)
	}
	return &j, nil
}
