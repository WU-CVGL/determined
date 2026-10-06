//go:build integration
// +build integration

package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/config"
	detcontext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	expauth "github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

func TestTaskResourcesUsesGetTaskRBACBeforePrometheus(t *testing.T) {
	api, authZExp, _, curUser, grpcCtx := setupExpAuthTest(t, nil)
	_, task := createTestTrial(t, api, curUser)
	taskID := task.TaskID.String()

	// The same artifact permission blocks the existing GetTask API and this Echo route.
	authZExp.On("CanGetExperiment", mock.Anything, curUser, mock.Anything).Return(nil).Twice()
	authZExp.On("CanGetExperimentArtifacts", mock.Anything, curUser, mock.Anything).
		Return(fmt.Errorf("artifact access denied")).Twice()
	_, err := api.GetTask(grpcCtx, &apiv1.GetTaskRequest{TaskId: taskID})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	now := time.Now().Unix()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/ui/task-resources/%s?start=%d&end=%d&step=15", taskID, now-60, now), nil)
	c := &detcontext.DetContext{Context: e.NewContext(req, httptest.NewRecorder())}
	c.SetUser(curUser)
	c.SetParamNames("task_id")
	c.SetParamValues(taskID)
	queried := false
	err = serveTaskResources(c, config.TaskResourcesConfig{DetCluster: "cvgl"}, taskResourceDependencies{
		authorize: func(ctx context.Context, user model.User, id string) error {
			_, _, err := api.canDoActionsOnTaskForUser(ctx, model.TaskID(id), user,
				expauth.AuthZProvider.Get().CanGetExperimentArtifacts)
			return err
		},
		allocationBelongs: func(context.Context, string, string) (bool, error) {
			queried = true
			return true, nil
		},
		query: func(context.Context, string, taskResourceRange) ([]prometheusTaskSeries, error) {
			queried = true
			return nil, nil
		},
	})
	require.False(t, queried)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code)
}

func TestTaskResourceAllocationsContainerStart(t *testing.T) {
	api, curUser, ctx := setupAPITest(t, nil)
	_, trialTask := createTestTrial(t, api, curUser)
	_, otherTask := createTestTrial(t, api, curUser)
	taskID := trialTask.TaskID
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	at := func(minutes int) *time.Time { return ptrs.Ptr(base.Add(time.Duration(minutes) * time.Minute)) }
	id := func(task model.TaskID, run int) model.AllocationID {
		return model.AllocationID(fmt.Sprintf("%s.%d", task, run))
	}
	addAllocation := func(aID model.AllocationID, task model.TaskID, state model.AllocationState,
		start, end *time.Time,
	) {
		a := model.Allocation{
			AllocationID: aID, TaskID: task, Slots: 1, ResourcePool: "default",
			StartTime: start, State: &state, Ports: map[string]int{},
		}
		require.NoError(t, db.AddAllocation(ctx, &a))
		if end != nil {
			a.EndTime = end
			require.NoError(t, db.CompleteAllocation(ctx, &a))
		}
	}
	record := func(aID model.AllocationID, eventType string, start, end *time.Time) {
		require.NoError(t, db.RecordTaskStats(ctx, &model.TaskStats{
			AllocationID: aID, EventType: eventType, StartTime: start, EndTime: end,
		}))
	}

	// Resources assigned at minute 10; the pull starts at 12 (allocations.start_time) and ends at 20.
	addAllocation(id(taskID, 1), taskID, model.AllocationStateTerminated, at(12), at(60))
	record(id(taskID, 1), "QUEUED", at(0), at(10))
	record(id(taskID, 1), "IMAGEPULL", at(12), at(20))
	// No QUEUED row (data from before task stats existed): the allocation start is used.
	addAllocation(id(taskID, 2), taskID, model.AllocationStateTerminated, at(70), at(80))
	// Restored after a master restart: a second QUEUED row ends at the restore; the first one wins.
	addAllocation(id(taskID, 3), taskID, model.AllocationStateRunning, at(92), nil)
	record(id(taskID, 3), "QUEUED", at(85), at(90))
	record(id(taskID, 3), "QUEUED", at(200), at(200))
	// Still queued: no QUEUED row and no start yet.
	addAllocation(id(taskID, 4), taskID, model.AllocationStatePending, nil, nil)
	// Another task's allocation is never returned.
	addAllocation(id(otherTask.TaskID, 1), otherTask.TaskID, model.AllocationStateTerminated, at(1), at(2))
	record(id(otherTask.TaskID, 1), "QUEUED", at(0), at(1))

	got, err := queryTaskResourceAllocations(ctx, string(taskID))
	require.NoError(t, err)
	type span struct {
		id         string
		start, end *time.Time
	}
	want := []span{
		{string(id(taskID, 1)), at(10), at(60)},
		{string(id(taskID, 2)), at(70), at(80)},
		{string(id(taskID, 3)), at(90), nil},
		{string(id(taskID, 4)), nil, nil},
	}
	require.Len(t, got, len(want))
	sameTime := func(want, got *time.Time) {
		if want == nil {
			require.Nil(t, got)
			return
		}
		require.NotNil(t, got)
		require.True(t, want.Equal(*got), "want %s, got %s", want, got)
	}
	for i, w := range want {
		require.Equal(t, w.id, got[i].AllocationID)
		sameTime(w.start, got[i].ContainerStart)
		sameTime(w.end, got[i].End)
	}

	empty, err := queryTaskResourceAllocations(ctx, "no-such-task")
	require.NoError(t, err)
	require.NotNil(t, empty)
	require.Empty(t, empty)
}

func TestTaskResourceAllocationsUseTaskRBACBeforeReading(t *testing.T) {
	api, authZExp, _, curUser, ctx := setupExpAuthTest(t, nil)
	_, trialTask := createTestTrial(t, api, curUser)
	taskID := trialTask.TaskID.String()
	aID := model.AllocationID(taskID + ".1")
	require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
		AllocationID: aID, TaskID: trialTask.TaskID, Slots: 1, ResourcePool: "default",
		Ports: map[string]int{}, StartTime: ptrs.Ptr(time.Now().UTC().Truncate(time.Second)),
	}))

	serve := func() (*httptest.ResponseRecorder, bool, error) {
		e := echo.New()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/ui/task-resources/"+taskID+"/allocations", nil)
		c := &detcontext.DetContext{Context: e.NewContext(req, rec)}
		c.SetUser(curUser)
		c.SetParamNames("task_id")
		c.SetParamValues(taskID)
		read := false
		err := serveTaskResourceAllocations(c, taskResourceDependencies{
			authorize: func(ctx context.Context, user model.User, id string) error {
				_, _, err := api.canDoActionsOnTaskForUser(ctx, model.TaskID(id), user,
					expauth.AuthZProvider.Get().CanGetExperimentArtifacts)
				return err
			},
			allocations: func(ctx context.Context, id string) ([]taskResourceAllocation, error) {
				read = true
				return queryTaskResourceAllocations(ctx, id)
			},
		})
		return rec, read, err
	}

	authZExp.On("CanGetExperiment", mock.Anything, curUser, mock.Anything).Return(nil).Once()
	authZExp.On("CanGetExperimentArtifacts", mock.Anything, curUser, mock.Anything).
		Return(fmt.Errorf("artifact access denied")).Once()
	_, read, err := serve()
	require.False(t, read)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, he.Code)
	require.NotContains(t, fmt.Sprint(he.Message), "artifact access denied")

	authZExp.On("CanGetExperiment", mock.Anything, curUser, mock.Anything).Return(nil).Once()
	authZExp.On("CanGetExperimentArtifacts", mock.Anything, curUser, mock.Anything).Return(nil).Once()
	rec, read, err := serve()
	require.NoError(t, err)
	require.True(t, read)
	var resp taskResourceAllocationsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Allocations, 1)
	require.Equal(t, string(aID), resp.Allocations[0].AllocationID)
	require.NotNil(t, resp.Allocations[0].ContainerStart)
	require.Nil(t, resp.Allocations[0].End)
}
