//go:build integration
// +build integration

package internal

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// storedJobAudiences reads the audiences stamped on a job.
func storedJobAudiences(ctx context.Context, t *testing.T, jobID model.JobID) model.Job {
	t.Helper()
	var j model.Job
	require.NoError(t, db.Bun().NewSelect().Model(&j).Where("job_id = ?", jobID).Scan(ctx))
	return j
}

// taskJobID returns the job of a task.
func taskJobID(ctx context.Context, t *testing.T, taskID string) model.JobID {
	t.Helper()
	tk, err := db.TaskByID(ctx, model.TaskID(taskID))
	require.NoError(t, err)
	require.NotNil(t, tk.JobID)
	return *tk.JobID
}

// experimentJobID returns the job of an experiment.
func experimentJobID(ctx context.Context, t *testing.T, id int32) model.JobID {
	t.Helper()
	var jobID model.JobID
	require.NoError(t, db.Bun().NewRaw(`SELECT job_id FROM experiments WHERE id = ?`, id).
		Scan(ctx, &jobID))
	return jobID
}

// Every way of creating a job stamps its audiences from the cluster policy and the owner's
// preferences: here the owner's new-job title audience (owner only) wins over the policy's, the
// logs follow the policy (the owner chose none) and the config takes the policy's.
func TestJobCreationStampsPrivacy(t *testing.T) {
	var aux, compute string
	api, admin, adminCtx := setupAPITest(t, nil, mockRMWithResolver(resolveOmittedPoolTo(&aux, &compute)))
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	aux = accessTestPool(t, "aux", admin, false)
	compute = accessTestPool(t, "compute", admin, false)

	owner := addGCTestOwner(t)
	ownerCtx := userContext(t, owner)
	_, err = db.Bun().NewRaw(`INSERT INTO user_job_privacy (user_id, new_job_title_audience)
		VALUES (?, '{}')`, owner.ID).Exec(adminCtx)
	require.NoError(t, err)

	requireStamped := func(name string, jobID model.JobID) {
		t.Helper()
		j := storedJobAudiences(adminCtx, t, jobID)
		require.Equal(t, owner.ID, *j.OwnerID, name)
		require.Equal(t, []string{}, j.TitleAudience, name)
		require.Equal(t, []string{"authenticated"}, j.LogsAudience, name)
		require.Equal(t, []string{"authenticated"}, j.ConfigAudience, name)
	}

	exp, err := createExperimentForAccessTest(ownerCtx, t, api,
		&apiv1.CreateExperimentRequest{Config: accessTestExperimentConfig(compute)})
	require.NoError(t, err)
	requireStamped("experiment", model.JobID(exp.Experiment.JobId))

	fork, err := createExperimentForAccessTest(ownerCtx, t, api, &apiv1.CreateExperimentRequest{
		Config: accessTestExperimentConfig(compute), ParentId: exp.Experiment.Id,
	})
	require.NoError(t, err)
	requireStamped("fork", model.JobID(fork.Experiment.JobId))

	unmanaged, err := api.PutExperiment(ownerCtx, &apiv1.PutExperimentRequest{
		CreateExperimentRequest: &apiv1.CreateExperimentRequest{
			Config: accessTestExperimentConfig(compute), Unmanaged: ptrs.Ptr(true), ProjectId: 1,
		},
		ExternalExperimentId: uuid.NewString(),
	})
	require.NoError(t, err)
	requireStamped("unmanaged experiment", experimentJobID(adminCtx, t, unmanaged.Experiment.Id))

	cmd, err := api.LaunchCommand(ownerCtx, &apiv1.LaunchCommandRequest{})
	require.NoError(t, err)
	requireStamped("command", model.JobID(cmd.Command.JobId))

	shell, err := api.LaunchShell(ownerCtx, &apiv1.LaunchShellRequest{})
	require.NoError(t, err)
	requireStamped("shell", model.JobID(shell.Shell.JobId))

	// Generic tasks start their allocations in a capturing service.
	oldService := task.DefaultService
	task.DefaultService = &captureAllocationService{}
	t.Cleanup(func() { task.DefaultService = oldService })
	const entrypoint = "entrypoint: [\"true\"]\n"
	parent, err := createGenericTaskForAccessTest(ownerCtx, t, api,
		&apiv1.CreateGenericTaskRequest{Config: entrypoint})
	require.NoError(t, err)
	requireStamped("generic task", taskJobID(adminCtx, t, parent.TaskId))

	child, err := createGenericTaskForAccessTest(ownerCtx, t, api,
		&apiv1.CreateGenericTaskRequest{Config: entrypoint, ParentId: &parent.TaskId})
	require.NoError(t, err)
	requireStamped("generic child", taskJobID(adminCtx, t, child.TaskId))

	// A checkpoint deletion is a job of the user who asks for it.
	specs := captureCheckpointGC(t)
	_, ckpt := createGCTestExperiment(adminCtx, t, api, owner)
	before := map[model.JobID]bool{}
	var gcJobs []model.JobID
	require.NoError(t, db.Bun().NewRaw(`SELECT job_id FROM jobs
		WHERE job_type = 'CHECKPOINT_GC' AND owner_id = ?`, owner.ID).Scan(adminCtx, &gcJobs))
	for _, id := range gcJobs {
		before[id] = true
	}
	_, err = api.DeleteCheckpoints(ownerCtx, &apiv1.DeleteCheckpointsRequest{
		CheckpointUuids: []string{ckpt},
	})
	require.NoError(t, err)
	nextGCSpec(t, specs)
	gcJobs = nil
	require.NoError(t, db.Bun().NewRaw(`SELECT job_id FROM jobs
		WHERE job_type = 'CHECKPOINT_GC' AND owner_id = ?`, owner.ID).Scan(adminCtx, &gcJobs))
	var added []model.JobID
	for _, id := range gcJobs {
		if !before[id] {
			added = append(added, id)
		}
	}
	require.Len(t, added, 1)
	requireStamped("checkpoint deletion", added[0])
}
