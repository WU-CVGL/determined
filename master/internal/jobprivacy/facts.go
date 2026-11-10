package jobprivacy

import (
	"context"

	"github.com/uptrace/bun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
)

// factsRow is a job joined with its owner's preferences.
type factsRow struct {
	JobID          model.JobID   `bun:"job_id"`
	OwnerID        *model.UserID `bun:"owner_id"`
	TitleAudience  []string      `bun:"title_audience,array"`
	LogsAudience   []string      `bun:"logs_audience,array"`
	ConfigAudience []string      `bun:"config_audience,array"`
	ShareTitles    bool          `bun:"share_titles"`
	ShareLogs      bool          `bun:"share_logs"`
}

// readError is the error of a job privacy read that failed.
func readError(err error) error {
	return status.Errorf(codes.Unavailable, "could not read job privacy: %s; try again", err)
}

// Facts reads the facts of the jobs in one query. Every requested ID is in the result; one without
// a jobs row has Exists false. A query error is Unavailable.
func Facts(
	ctx context.Context, idb bun.IDB, jobIDs []model.JobID,
) (map[model.JobID]JobFacts, error) {
	if idb == nil {
		idb = db.Bun()
	}
	facts := make(map[model.JobID]JobFacts, len(jobIDs))
	for _, id := range jobIDs {
		facts[id] = JobFacts{JobID: id}
	}
	if len(jobIDs) == 0 {
		return facts, nil
	}
	var rows []factsRow
	err := idb.NewSelect().
		TableExpr("jobs AS j").
		// A user without a preference row has both switches on.
		Join("LEFT JOIN user_job_privacy AS pu ON pu.user_id = j.owner_id").
		ColumnExpr("j.job_id, j.owner_id, j.title_audience, j.logs_audience, j.config_audience").
		ColumnExpr("COALESCE(pu.share_titles, TRUE) AS share_titles").
		ColumnExpr("COALESCE(pu.share_logs, TRUE) AS share_logs").
		Where("j.job_id IN (?)", bun.In(jobIDs)).
		Scan(ctx, &rows)
	if err != nil {
		return nil, readError(err)
	}
	for _, r := range rows {
		facts[r.JobID] = JobFacts{
			JobID:       r.JobID,
			OwnerID:     r.OwnerID,
			Exists:      true,
			Title:       r.TitleAudience,
			Logs:        r.LogsAudience,
			Config:      r.ConfigAudience,
			ShareTitles: r.ShareTitles,
			ShareLogs:   r.ShareLogs,
		}
	}
	return facts, nil
}

// ForJobs returns the viewer's capabilities on each job: the policy, the facts and Effective.
// Every requested ID is in the result. An admin's capabilities are computed without a query.
func ForJobs(ctx context.Context, v Viewer, jobIDs []model.JobID) (map[model.JobID]Caps, error) {
	caps := make(map[model.JobID]Caps, len(jobIDs))
	if v.Admin {
		for _, id := range jobIDs {
			caps[id] = AllCaps
		}
		return caps, nil
	}
	if len(jobIDs) == 0 {
		return caps, nil
	}
	p, err := ReadPolicy(ctx, db.Bun())
	if err != nil {
		return nil, err
	}
	facts, err := Facts(ctx, db.Bun(), jobIDs)
	if err != nil {
		return nil, err
	}
	for id, f := range facts {
		caps[id] = Effective(v, f, p)
	}
	return caps, nil
}

// TaskJob is what decides the privacy of a task: its job, or, for a task without one, the owner
// from its command record.
type TaskJob struct {
	// JobID is the task's own job, or, for a trial task without one (an unmanaged trial), the job
	// of its experiment.
	JobID *model.JobID
	// LegacyOwnerID is set only for a task without a job that has a command record (rows from
	// before tasks had jobs).
	LegacyOwnerID *model.UserID
}

// trialExperimentJobExpr is the job of the experiment of trial task t.
const trialExperimentJobExpr = `(SELECT e.job_id FROM run_id_task_id AS rt
	JOIN runs AS r ON r.id = rt.run_id
	JOIN experiments AS e ON e.id = r.experiment_id
	WHERE rt.task_id = t.task_id LIMIT 1)`

// TaskJobs resolves each task to what decides its privacy, in one query: the task's own job;
// else, for a trial task, the job of its experiment; else the owner from its command record. A
// task that is not found is not in the result; one that is found and has none of them has an
// empty TaskJob. A query error is Unavailable.
func TaskJobs(
	ctx context.Context, idb bun.IDB, taskIDs []model.TaskID,
) (map[model.TaskID]TaskJob, error) {
	if idb == nil {
		idb = db.Bun()
	}
	jobs := make(map[model.TaskID]TaskJob, len(taskIDs))
	if len(taskIDs) == 0 {
		return jobs, nil
	}
	var rows []struct {
		TaskID        model.TaskID  `bun:"task_id"`
		JobID         *model.JobID  `bun:"job_id"`
		LegacyOwnerID *model.UserID `bun:"legacy_owner_id"`
	}
	err := idb.NewSelect().
		TableExpr("tasks AS t").
		Join("LEFT JOIN command_state AS cs ON cs.task_id = t.task_id AND t.job_id IS NULL").
		Column("t.task_id").
		ColumnExpr("COALESCE(t.job_id, CASE WHEN t.task_type = ? THEN "+
			trialExperimentJobExpr+" END) AS job_id", model.TaskTypeTrial).
		ColumnExpr("(cs.generic_command_spec->'Base'->'Owner'->>'id')::int AS legacy_owner_id").
		Where("t.task_id IN (?)", bun.In(taskIDs)).
		Scan(ctx, &rows)
	if err != nil {
		return nil, readError(err)
	}
	for _, r := range rows {
		tj := TaskJob{JobID: r.JobID}
		if r.JobID == nil {
			tj.LegacyOwnerID = r.LegacyOwnerID
		}
		jobs[r.TaskID] = tj
	}
	return jobs, nil
}

// ForTaskJobs returns the viewer's capabilities on each resolved task (TaskJobs). A task whose
// TaskJob is empty is hidden from non-admins.
func ForTaskJobs(
	ctx context.Context, v Viewer, tasks map[model.TaskID]TaskJob,
) (map[model.TaskID]Caps, error) {
	caps := make(map[model.TaskID]Caps, len(tasks))
	if v.Admin {
		for id := range tasks {
			caps[id] = AllCaps
		}
		return caps, nil
	}
	var jobIDs []model.JobID
	var legacyOwners []model.UserID
	for id, tj := range tasks {
		caps[id] = Caps{}
		switch {
		case tj.JobID != nil:
			jobIDs = append(jobIDs, *tj.JobID)
		case tj.LegacyOwnerID != nil:
			legacyOwners = append(legacyOwners, *tj.LegacyOwnerID)
		}
	}
	if len(jobIDs) == 0 && len(legacyOwners) == 0 {
		return caps, nil
	}
	p, err := ReadPolicy(ctx, db.Bun())
	if err != nil {
		return nil, err
	}
	facts, err := Facts(ctx, db.Bun(), jobIDs)
	if err != nil {
		return nil, err
	}
	switches, err := ownerSwitches(ctx, legacyOwners)
	if err != nil {
		return nil, err
	}
	for id, tj := range tasks {
		switch {
		case tj.JobID != nil:
			caps[id] = Effective(v, facts[*tj.JobID], p)
		case tj.LegacyOwnerID != nil:
			s := switches[*tj.LegacyOwnerID]
			caps[id] = Effective(v, JobFacts{
				OwnerID:     tj.LegacyOwnerID,
				Exists:      true,
				ShareTitles: s.ShareTitles,
				ShareLogs:   s.ShareLogs,
			}, p)
		}
	}
	return caps, nil
}

// ForTasks returns the viewer's capabilities on each task, resolved by TaskJobs. Every requested
// ID is in the result. A task with a command record and no job is a job without audiences, public
// under the default; a task with none of TaskJobs' sources, and an unknown task, are hidden from
// non-admins.
func ForTasks(
	ctx context.Context, v Viewer, taskIDs []model.TaskID,
) (map[model.TaskID]Caps, error) {
	caps := make(map[model.TaskID]Caps, len(taskIDs))
	if v.Admin {
		for _, id := range taskIDs {
			caps[id] = AllCaps
		}
		return caps, nil
	}
	tasks, err := TaskJobs(ctx, nil, taskIDs)
	if err != nil {
		return nil, err
	}
	found, err := ForTaskJobs(ctx, v, tasks)
	if err != nil {
		return nil, err
	}
	for _, id := range taskIDs {
		caps[id] = found[id]
	}
	return caps, nil
}

// ownerSwitches reads the switches of the users; a user without a row has both on.
func ownerSwitches(
	ctx context.Context, userIDs []model.UserID,
) (map[model.UserID]UserPrivacy, error) {
	switches := make(map[model.UserID]UserPrivacy, len(userIDs))
	for _, id := range userIDs {
		switches[id] = DefaultUserPrivacy
	}
	if len(userIDs) == 0 {
		return switches, nil
	}
	var rows []userPrivacyRow
	if err := db.Bun().NewSelect().Model(&rows).
		Where("user_id IN (?)", bun.In(userIDs)).Scan(ctx); err != nil {
		return nil, readError(err)
	}
	for _, r := range rows {
		switches[r.UserID] = UserPrivacy{ShareTitles: r.ShareTitles, ShareLogs: r.ShareLogs}
	}
	return switches, nil
}

// Require returns nil when the viewer may read the kind (logs or config) of the job, and
// PermissionDenied "logs not shared" or "config not shared" otherwise. It runs after the existing
// base check, never instead of it. The title has no Require: a hidden title is redacted, not
// refused.
func Require(ctx context.Context, v Viewer, jobID model.JobID, k Kind) error {
	if k != Logs && k != Config {
		return deniedError(k)
	}
	caps, err := ForJobs(ctx, v, []model.JobID{jobID})
	if err != nil {
		return err
	}
	if !caps[jobID].Allows(k) {
		return deniedError(k)
	}
	return nil
}

// RequireTask is Require for the job of a task (ForTasks).
func RequireTask(ctx context.Context, v Viewer, taskID model.TaskID, k Kind) error {
	if k != Logs && k != Config {
		return deniedError(k)
	}
	caps, err := ForTasks(ctx, v, []model.TaskID{taskID})
	if err != nil {
		return err
	}
	if !caps[taskID].Allows(k) {
		return deniedError(k)
	}
	return nil
}
