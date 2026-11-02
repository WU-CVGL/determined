//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/experimentv1"
)

type experimentPageReadKey struct{}

type experimentPageRead struct {
	once sync.Once
	run  func()
}

// The callback is scoped to one request context. Bun's hook runs after the SELECT rows have
// been scanned, so an assignment here commits exactly in the former read/enrichment window.
type experimentPageReadHook struct{}

func (experimentPageReadHook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (experimentPageReadHook) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	read, ok := ctx.Value(experimentPageReadKey{}).(*experimentPageRead)
	if !ok || event.Err != nil || event.Model == nil {
		return
	}
	if _, ok := event.IQuery.(*bun.SelectQuery); !ok { // Excludes the pagination count.
		return
	}
	if _, ok := event.Model.Value().(*[]*experimentv1.Experiment); ok {
		read.once.Do(read.run)
	}
}

var addExperimentPageReadHook sync.Once

func TestAssignmentSortUsesPageSnapshot(t *testing.T) {
	api, user, ctx := setupAPITest(t, nil)
	api.m.config.RootManagerInternal.AgentRM = &config.AgentResourceManagerConfig{}
	addExperimentPageReadHook.Do(func() { db.Bun().AddQueryHook(experimentPageReadHook{}) })
	for _, direction := range []apiv1.OrderBy{apiv1.OrderBy_ORDER_BY_ASC, apiv1.OrderBy_ORDER_BY_DESC} {
		t.Run(direction.String(), func(t *testing.T) {
			_, projectID := createProjectAndWorkspace(ctx, t, api)
			older, task := createTestTrial(t, api, user)
			newer, _ := createTestTrial(t, api, user)
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for i, id := range []int{older.ExperimentID, newer.ExperimentID} {
				_, err := db.Bun().NewRaw("UPDATE experiments SET project_id = ?, start_time = ? WHERE id = ?",
					projectID, base.Add(time.Duration(i)*time.Minute), id).Exec(ctx)
				require.NoError(t, err)
			}
			allocation := model.AllocationID(string(task.TaskID) + ".1")
			require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
				AllocationID: allocation, TaskID: task.TaskID, Slots: 1, ResourcePool: "default",
				Ports: map[string]int{},
			}))
			assigned := base.Add(time.Hour + time.Microsecond)
			fired := false
			read := &experimentPageRead{run: func() {
				fired = true
				require.NoError(t, db.RecordTaskStats(ctx, &model.TaskStats{
					AllocationID: allocation, EventType: "QUEUED", StartTime: &base, EndTime: &assigned,
				}))
			}}
			req := &apiv1.GetExperimentsRequest{
				ProjectId: int32(projectID), Limit: 2, OrderBy: direction,
				SortBy: apiv1.GetExperimentsRequest_SORT_BY_ALLOCATION_START_TIME,
			}
			page, err := api.GetExperiments(context.WithValue(ctx, experimentPageReadKey{}, read), req)
			require.NoError(t, err)
			require.True(t, fired, "assignment must commit after the page SELECT")
			require.Len(t, page.Experiments, 2)
			require.Equal(t, int32(newer.ExperimentID), page.Experiments[0].Id)
			require.Equal(t, int32(older.ExperimentID), page.Experiments[1].Id)
			for _, exp := range page.Experiments {
				require.Nil(t, exp.AllocationStartTime, "returned value must use the sorting snapshot")
			}
			// The next request sees the newly committed assignment and puts it before missing values.
			page, err = api.GetExperiments(ctx, req)
			require.NoError(t, err)
			require.Equal(t, int32(older.ExperimentID), page.Experiments[0].Id)
			require.Equal(t, timestamppb.New(assigned), page.Experiments[0].AllocationStartTime)
			require.Nil(t, page.Experiments[1].AllocationStartTime)
		})
	}
}

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
