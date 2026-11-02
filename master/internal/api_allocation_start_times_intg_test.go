//go:build integration
// +build integration

package internal

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/job/jobservice"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
)

func TestEnrichAllocationStartTimes(t *testing.T) {
	api, user, ctx := setupAPITest(t, nil)
	trial, started := createTestTrial(t, api, user)
	queuedTrial, queued := createTestTrial(t, api, user)
	_, unrelated := createTestTrial(t, api, user)
	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	at := func(minutes int) *time.Time { return ptrs.Ptr(base.Add(time.Duration(minutes) * time.Minute)) }
	add := func(taskID model.TaskID, run int, assigned *time.Time) model.AllocationID {
		id := model.AllocationID(fmt.Sprintf("%s.%d", taskID, run))
		// A synthesized accounting start must never masquerade as resource assignment.
		require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
			AllocationID: id, TaskID: taskID, Slots: 1, ResourcePool: "default",
			StartTime: at(1), State: ptrs.Ptr(model.AllocationStateRunning), Ports: map[string]int{},
		}))
		if assigned != nil {
			require.NoError(t, db.RecordTaskStats(ctx, &model.TaskStats{
				AllocationID: id, EventType: "QUEUED", StartTime: at(0), EndTime: assigned,
			}))
		}
		return id
	}
	first := add(started.TaskID, 1, at(10))
	add(started.TaskID, 2, at(30))
	add(queued.TaskID, 1, nil)
	add(unrelated.TaskID, 1, at(2))
	// The same allocation's restore records a second queue event; it must not move Started.
	require.NoError(t, db.RecordTaskStats(ctx, &model.TaskStats{
		AllocationID: first, EventType: "QUEUED", StartTime: at(60), EndTime: at(61),
	}))
	var start, pending, missing *timestamppb.Timestamp
	require.NoError(t, enrichAllocationStartTimes(ctx, []allocationStartTarget{
		{string(started.TaskID), &start}, {string(queued.TaskID), &pending}, {"unknown", &missing},
	}, allocationStartTask))
	require.Equal(t, timestamppb.New(*at(10)), start)
	require.Nil(t, pending)
	require.Nil(t, missing)

	start, pending = nil, nil
	require.NoError(t, enrichAllocationStartTimes(ctx, []allocationStartTarget{
		{strconv.Itoa(trial.ExperimentID), &start},
		{strconv.Itoa(queuedTrial.ExperimentID), &pending},
	}, allocationStartExperiment))
	require.Equal(t, timestamppb.New(*at(10)), start)
	require.Nil(t, pending)
	// Exercise the authorized experiment list, not just the helper.
	listed, err := api.GetExperiments(ctx, &apiv1.GetExperimentsRequest{Limit: 0})
	require.NoError(t, err)
	found := false
	for _, exp := range listed.Experiments {
		if int(exp.Id) == trial.ExperimentID {
			found = true
			require.Equal(t, start, exp.AllocationStartTime)
		}
		if int(exp.Id) == queuedTrial.ExperimentID {
			require.Nil(t, exp.AllocationStartTime)
		}
	}
	require.True(t, found)
}

func TestJobQueueAllocationStartTimes(t *testing.T) {
	manager := MockRM()
	api, user, ctx := setupAPITest(t, nil, manager)
	jobID := db.RequireMockJob(t, db.SingleDB(), &user.ID)
	queuedJobID := db.RequireMockJob(t, db.SingleDB(), &user.ID)
	base := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, minutes := range []int{30, 10} {
		_, task := createTestTrial(t, api, user)
		_, err := db.Bun().NewUpdate().Model((*model.Task)(nil)).
			Set("job_id = ?", jobID).Where("task_id = ?", task.TaskID).Exec(ctx)
		require.NoError(t, err)
		id := model.AllocationID(fmt.Sprintf("%s.1", task.TaskID))
		require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
			AllocationID: id, TaskID: task.TaskID, Slots: 1, ResourcePool: "default",
			State: ptrs.Ptr(model.AllocationStateRunning), Ports: map[string]int{},
		}))
		require.NoError(t, db.RecordTaskStats(ctx, &model.TaskStats{
			AllocationID: id, EventType: "QUEUED", StartTime: &base,
			EndTime: ptrs.Ptr(base.Add(time.Duration(minutes) * time.Minute)),
		}))
	}
	jobs := []*jobv1.Job{
		{JobId: string(jobID), SubmissionTime: timestamppb.New(base)},
		{JobId: string(queuedJobID), SubmissionTime: timestamppb.New(base)},
	}
	manager.On("GetJobQ", mock.Anything).Return(map[model.JobID]*sproto.RMJobInfo{}, nil)
	manager.On("GetExternalJobs", mock.Anything).Return(jobs, nil)
	jobservice.SetDefaultService(manager)

	// Both queue API versions expose the first assignment across all of the job's tasks.
	listed, err := api.GetJobs(ctx, &apiv1.GetJobsRequest{ResourcePool: "default"})
	require.NoError(t, err)
	require.Len(t, listed.Jobs, 2)
	require.Equal(t, timestamppb.New(base.Add(10*time.Minute)), listed.Jobs[0].AllocationStartTime)
	require.Equal(t, timestamppb.New(base), listed.Jobs[0].SubmissionTime)
	require.Nil(t, listed.Jobs[1].AllocationStartTime)
	listedV2, err := api.GetJobsV2(ctx, &apiv1.GetJobsV2Request{ResourcePool: "default"})
	require.NoError(t, err)
	require.Len(t, listedV2.Jobs, 2)
	require.Equal(t, listed.Jobs[0].AllocationStartTime, listedV2.Jobs[0].GetFull().AllocationStartTime)
	require.Nil(t, listedV2.Jobs[1].GetFull().AllocationStartTime)
}
