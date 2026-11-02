//go:build integration
// +build integration

package internal

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// Submission order deliberately differs from assignment order, including a tie and an unassigned
// task submitted most recently. Every one-row page must agree with the whole-list order.
func TestAssignmentSortBeforePagination(t *testing.T) {
	api, user, ctx := setupAPITest(t, nil)
	api.m.config.RootManagerInternal.AgentRM = &config.AgentResourceManagerConfig{}
	workspaceID, projectID := createProjectAndWorkspace(ctx, t, api)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	experiments := map[int32]int{}
	generic := map[string]int{}
	for i, assigned := range []int{3, 1, 3, 0} {
		submitted := base.Add(time.Duration(i) * time.Minute)
		trial, task := createTestTrial(t, api, user)
		_, err := db.Bun().NewRaw("UPDATE experiments SET project_id = ?, start_time = ? WHERE id = ?",
			projectID, submitted, trial.ExperimentID).Exec(ctx)
		require.NoError(t, err)
		experiments[int32(trial.ExperimentID)] = i
		id := model.TaskID(fmt.Sprintf("assignment-sort-%d-%d", projectID, i))
		addJobsSortGenericTask(ctx, t, id, &user.ID, workspaceID, projectID, string(id), nil, 1,
			ptrs.Ptr(model.TaskStateActive), submitted, nil)
		generic[string(id)] = i
		allocationID := model.AllocationID(string(task.TaskID) + ".1")
		require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
			AllocationID: allocationID, TaskID: task.TaskID, Slots: 1, ResourcePool: "default",
			Ports: map[string]int{},
		}))
		if assigned > 0 {
			// Microsecond precision matters: all assignments fall in one millisecond.
			end := base.Add(time.Hour + time.Duration(assigned)*time.Microsecond)
			for _, allocation := range []model.AllocationID{allocationID, model.AllocationID(string(id) + ".0")} {
				require.NoError(t, db.RecordTaskStats(ctx, &model.TaskStats{
					AllocationID: allocation, EventType: "QUEUED", StartTime: &submitted, EndTime: &end,
				}))
			}
		}
	}
	for _, supported := range []bool{true, false} {
		if !supported {
			api.m.config.RootManagerInternal.AgentRM = nil
		}
		for _, desc := range []bool{false, true} {
			order := apiv1.OrderBy_ORDER_BY_ASC
			want := []int{1, 2, 0, 3}
			if desc {
				order = apiv1.OrderBy_ORDER_BY_DESC
				want = []int{2, 0, 1, 3}
			}
			if !supported {
				want = []int{3, 2, 1, 0}
			}
			for _, pageSize := range []int32{1, 4} {
				var gotExperiments, gotGeneric []int
				for offset := int32(0); offset < 4; offset += pageSize {
					exps, err := api.GetExperiments(ctx, &apiv1.GetExperimentsRequest{
						ProjectId: int32(projectID), SortBy: apiv1.GetExperimentsRequest_SORT_BY_ALLOCATION_START_TIME,
						OrderBy: order, Offset: offset, Limit: pageSize,
					})
					require.NoError(t, err)
					require.Equal(t, int32(4), exps.Pagination.Total)
					for _, exp := range exps.Experiments {
						gotExperiments = append(gotExperiments, experiments[exp.Id])
						if !supported {
							require.Nil(t, exp.AllocationStartTime)
						}
					}
					tasks, err := api.GetGenericTasks(ctx, &apiv1.GetGenericTasksRequest{
						ProjectId: int32(projectID), SortBy: apiv1.GetGenericTasksRequest_SORT_BY_ALLOCATION_START_TIME,
						OrderBy: order, Offset: offset, Limit: pageSize,
					})
					require.NoError(t, err)
					require.Equal(t, int32(4), tasks.Pagination.Total)
					for _, task := range tasks.Tasks {
						gotGeneric = append(gotGeneric, generic[task.TaskId])
						if !supported {
							require.Nil(t, task.AllocationStartTime)
						}
					}
				}
				require.Equal(t, want, gotExperiments)
				require.Equal(t, want, gotGeneric)
			}
		}
	}
}
