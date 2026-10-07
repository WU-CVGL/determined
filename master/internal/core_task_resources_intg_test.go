//go:build integration
// +build integration

package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	taskPkg "github.com/determined-ai/determined/master/internal/task"
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
	// No QUEUED row (it never got resources, or data from before task stats existed): no
	// container start, even with an allocation start.
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
	requireTaskResourceAllocations(t, got, []taskResourceSpan{
		{string(id(taskID, 1)), at(10), at(60)},
		{string(id(taskID, 3)), at(90), nil},
		{string(id(taskID, 2)), nil, at(80)},
		{string(id(taskID, 4)), nil, nil},
	})

	empty, err := queryTaskResourceAllocations(ctx, "no-such-task")
	require.NoError(t, err)
	require.NotNil(t, empty)
	require.Empty(t, empty)
}

type taskResourceSpan struct {
	id         string
	start, end *time.Time
}

func requireTaskResourceAllocations(t *testing.T, got []taskResourceAllocation, want []taskResourceSpan) {
	t.Helper()
	sameTime := func(want, got *time.Time) {
		if want == nil {
			require.Nil(t, got)
			return
		}
		require.NotNil(t, got)
		require.True(t, want.Equal(*got), "want %s, got %s", want, got)
	}
	require.Len(t, got, len(want))
	for i, w := range want {
		require.Equal(t, w.id, got[i].AllocationID)
		sameTime(w.start, got[i].ContainerStart)
		sameTime(w.end, got[i].End)
	}
}

// A master restart closes the allocations no task restores with the last cluster heartbeat as their
// start and end; one that never got resources gets no container start from it. An allocation that
// is still queued keeps no start time.
func TestTaskResourceAllocationsIgnoreRestartHeartbeat(t *testing.T) {
	api, curUser, ctx := setupAPITest(t, nil)
	_, trialTask := createTestTrial(t, api, curUser)
	taskID := trialTask.TaskID
	base := time.Now().UTC().Truncate(time.Hour).Add(-24 * time.Hour)
	at := func(minutes int) *time.Time { return ptrs.Ptr(base.Add(time.Duration(minutes) * time.Minute)) }
	queued := model.AllocationStatePending
	add := func(run int) model.AllocationID {
		aID := model.AllocationID(fmt.Sprintf("%s.%d", taskID, run))
		require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
			AllocationID: aID, TaskID: taskID, Slots: 1, ResourcePool: "default",
			State: &queued, Ports: map[string]int{},
		}))
		return aID
	}
	// .1 was queued when the master stopped. On restart the trial does not reattach it (it never
	// started) and requests .2, which is still queued when the open allocations are closed.
	closed, pending := add(1), add(2)
	pgDB := api.m.db
	_, err := pgDB.GetOrCreateClusterID("")
	require.NoError(t, err)
	require.NoError(t, pgDB.UpdateClusterHeartBeat(*at(30)))
	require.NoError(t, db.CloseOpenAllocations(ctx, []model.AllocationID{pending}))

	closedRow, err := db.AllocationByID(ctx, closed)
	require.NoError(t, err)
	require.True(t, at(30).Equal(*closedRow.StartTime), "start %s", closedRow.StartTime)
	require.True(t, at(30).Equal(*closedRow.EndTime), "end %s", closedRow.EndTime)
	pendingRow, err := db.AllocationByID(ctx, pending)
	require.NoError(t, err)
	require.Nil(t, pendingRow.StartTime)
	require.Nil(t, pendingRow.EndTime)

	got, err := queryTaskResourceAllocations(ctx, string(taskID))
	require.NoError(t, err)
	requireTaskResourceAllocations(t, got, []taskResourceSpan{
		{string(closed), nil, at(30)},
		{string(pending), nil, nil},
	})

	// .2 gets its resources at minute 55 and becomes the earliest container start.
	require.NoError(t, db.RecordTaskStats(ctx, &model.TaskStats{
		AllocationID: pending, EventType: "QUEUED", StartTime: at(0), EndTime: at(55),
	}))
	got, err = queryTaskResourceAllocations(ctx, string(taskID))
	require.NoError(t, err)
	requireTaskResourceAllocations(t, got, []taskResourceSpan{
		{string(pending), at(55), nil},
		{string(closed), nil, at(30)},
	})
}

func TestTaskResourceAllocationsUseTaskRBACBeforeReading(t *testing.T) {
	api, authZExp, _, curUser, ctx := setupExpAuthTest(t, nil)
	_, trialTask := createTestTrial(t, api, curUser)
	taskID := trialTask.TaskID.String()
	aID := model.AllocationID(taskID + ".1")
	started := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
		AllocationID: aID, TaskID: trialTask.TaskID, Slots: 1, ResourcePool: "default",
		Ports: map[string]int{}, StartTime: ptrs.Ptr(started),
	}))
	require.NoError(t, db.RecordTaskStats(ctx, &model.TaskStats{
		AllocationID: aID, EventType: "QUEUED", StartTime: ptrs.Ptr(started.Add(-time.Minute)),
		EndTime: ptrs.Ptr(started),
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
	require.True(t, started.Equal(*resp.Allocations[0].ContainerStart))
	require.Nil(t, resp.Allocations[0].End)
}

func TestTaskResourceGPUSets(t *testing.T) {
	api, curUser, ctx := setupAPITest(t, nil)
	_, trialTask := createTestTrial(t, api, curUser)
	_, otherTask := createTestTrial(t, api, curUser)
	taskID := trialTask.TaskID
	add := func(task model.TaskID, run, slots int) string {
		aID := model.AllocationID(fmt.Sprintf("%s.%d", task, run))
		require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
			AllocationID: aID, TaskID: task, Slots: slots, ResourcePool: "default",
			Ports: map[string]int{},
		}))
		return string(aID)
	}
	record := func(aID, container, agent string, uuids ...string) {
		require.NoError(t, taskPkg.AddAllocationAcceleratorData(ctx, model.AcceleratorData{
			ContainerID: container, AllocationID: model.AllocationID(aID), NodeName: agent,
			AcceleratorType: "cuda", AcceleratorUuids: uuids,
		}))
	}
	first, second, cpuOnly := add(taskID, 1, 3), add(taskID, 2, 1), add(taskID, 3, 0)
	partial := add(taskID, 4, 2)
	other := add(otherTask.TaskID, 1, 1)
	// A two-node allocation records one row per container, in nvidia-smi order inside it.
	record(first, "c1", "agent-a", "GPU-b", "GPU-a")
	record(first, "c2", "agent-b", "GPU-c")
	record(second, "c3", "agent-a", "GPU-a")
	record(cpuOnly, "c4", "agent-a")
	record(other, "c5", "agent-a", "GPU-z")
	// The container of this 2-slot allocation recorded only one of its GPUs.
	record(partial, "c6", "agent-a", "GPU-q")

	// Only the requested allocations of this task, with their slots; another task's allocation
	// is never read.
	got, err := queryTaskResourceGPUSets(ctx, string(taskID), []string{first, cpuOnly, other})
	require.NoError(t, err)
	require.Equal(t, []taskResourceGPUSet{
		{AllocationID: first, ContainerID: "c1", UUIDs: []string{"GPU-b", "GPU-a"}, Slots: 3},
		{AllocationID: first, ContainerID: "c2", UUIDs: []string{"GPU-c"}, Slots: 3},
		{AllocationID: cpuOnly, ContainerID: "c4", UUIDs: nil, Slots: 0},
	}, got)

	none, err := queryTaskResourceGPUSets(ctx, string(otherTask.TaskID), []string{first})
	require.NoError(t, err)
	require.Empty(t, none)

	// The endpoint numbers each container's GPUs from the recorded sets.
	now := time.Now().Unix()
	e := echo.New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/ui/task-resources/%s?start=%d&end=%d&step=15", taskID, now-60, now), nil)
	c := &detcontext.DetContext{Context: e.NewContext(req, rec)}
	c.SetUser(curUser)
	c.SetParamNames("task_id")
	c.SetParamValues(string(taskID))
	gpu := func(aID, node, uuid, busID string) prometheusTaskSeries {
		return prometheusTaskSeries{Metric: map[string]string{
			"det_cluster": "cvgl", "task_id": string(taskID), "allocation_id": aID, "node": node,
			"gpu_uuid": uuid, "pci_bus_id": busID,
		}, Samples: [][2]interface{}{{float64(now), float64(1)}}}
	}
	err = serveTaskResources(c, config.TaskResourcesConfig{DetCluster: "cvgl"}, taskResourceDependencies{
		authorize:         func(context.Context, model.User, string) error { return nil },
		allocationBelongs: func(context.Context, string, string) (bool, error) { return true, nil },
		query: func(_ context.Context, expr string, _ taskResourceRange) ([]prometheusTaskSeries, error) {
			if !strings.Contains(expr, "DCGM_FI_DEV_GPU_UTIL") {
				return nil, nil
			}
			return []prometheusTaskSeries{
				gpu(first, "node-a", "GPU-a", "00000000:81:00.0"),
				gpu(first, "node-a", "GPU-b", "00000000:25:00.0"),
				gpu(first, "node-b", "GPU-c", "00000000:C1:00.0"),
				gpu(second, "node-a", "GPU-a", "00000000:81:00.0"),
				gpu(partial, "node-a", "GPU-q", "00000000:01:00.0"),
			}, nil
		},
		gpuSets: queryTaskResourceGPUSets,
	})
	require.NoError(t, err)
	var resp taskResourceResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	indexes := map[string]int{}
	for _, s := range resp.Series {
		if s.Labels.GPUIndex != nil {
			indexes[s.Labels.AllocationID+"/"+s.Labels.GPUUUID] = *s.Labels.GPUIndex
		}
	}
	// The partial list is not numbered.
	require.Len(t, resp.Series, 5)
	require.Equal(t, map[string]int{
		first + "/GPU-b": 0, first + "/GPU-a": 1, first + "/GPU-c": 0, second + "/GPU-a": 0,
	}, indexes)
}
