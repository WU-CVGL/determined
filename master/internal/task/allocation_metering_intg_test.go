//go:build integration

package task

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/cluster"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task/taskmodel"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/syncx/queue"
)

// requireAssignedDevices assigns the allocation one container per entry of devices, on one agent
// each, with those device types.
func requireAssignedDevices(
	t *testing.T,
	id model.AllocationID,
	q *queue.Queue[sproto.ResourcesEvent],
	devices ...[]device.Type,
) []sproto.ResourcesID {
	var ids []sproto.ResourcesID
	assigned := map[sproto.ResourcesID]sproto.Resources{}
	for i, types := range devices {
		rID := sproto.ResourcesID(cproto.NewID())
		var ds []device.Device
		for j, typ := range types {
			ds = append(ds, device.Device{ID: device.ID(j), Type: typ})
		}
		var r mocks.Resources
		r.On("Start", mock.Anything, mock.Anything, mock.Anything).Return(nil).Times(1)
		r.On("Summary").Return(sproto.ResourcesSummary{
			AllocationID:  id,
			ResourcesID:   rID,
			ResourcesType: sproto.ResourcesTypeDockerContainer,
			AgentDevices:  map[aproto.ID][]device.Device{aproto.ID(fmt.Sprintf("agent%d", i)): ds},
		})
		r.On("Kill", mock.Anything).Return().Run(func(_ mock.Arguments) {
			q.Put(&sproto.ResourcesStateChanged{
				ResourcesID:      rID,
				ResourcesState:   sproto.Terminated,
				ResourcesStopped: &sproto.ResourcesStopped{},
			})
		})
		assigned[rID] = &r
		ids = append(ids, rID)
	}
	q.Put(&sproto.ResourcesAllocated{ID: id, ResourcePool: stubResourcePoolName, Resources: assigned})
	requireState(t, id, model.AllocationStateAssigned)
	return ids
}

func requireNoMetering(t *testing.T, a *model.Allocation) {
	require.Nil(t, a.AssignedTime)
	require.Nil(t, a.ReleasedTime)
	require.Nil(t, a.MeteredKind)
	require.Nil(t, a.MeteredQuantity)
}

// An allocation's resources are metered by the devices it was assigned, from the assignment, which
// is also the end of its queued time, to the release, also when it never started.
func TestAllocationMeteredByAssignedDevices(t *testing.T) {
	cuda, rocm, cpu := device.CUDA, device.ROCM, device.CPU
	for _, tc := range []struct {
		name     string
		devices  [][]device.Type
		kind     string
		quantity int
	}{
		{"cuda", [][]device.Type{{cuda, cuda}}, "gpu", 2},
		{"rocm", [][]device.Type{{rocm}}, "gpu", 1},
		{"cpu", [][]device.Type{{cpu, cpu, cpu, cpu}}, "cpu", 4},
		{"aux", [][]device.Type{{}}, "aux", 1},
		{"gpu and cpu", [][]device.Type{{cuda, cpu, cpu}}, "gpu", 1},
		{"two agents", [][]device.Type{{cuda, cuda}, {cuda}}, "gpu", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closeDB, _, id, q, exitFuture := requireStarted(t)
			defer closeDB()

			before := time.Now().UTC().Add(-time.Second)
			requireAssignedDevices(t, id, q, tc.devices...)
			row, err := db.AllocationByID(context.TODO(), id)
			require.NoError(t, err)
			require.NotNil(t, row.AssignedTime)
			require.True(t, row.AssignedTime.After(before), "assigned %s", row.AssignedTime)
			require.Equal(t, &tc.kind, row.MeteredKind)
			require.Equal(t, &tc.quantity, row.MeteredQuantity)
			require.Nil(t, row.ReleasedTime)

			var queuedEnd time.Time
			require.NoError(t, db.Bun().NewSelect().Table("task_stats").ColumnExpr("end_time").
				Where("allocation_id = ?", id).Where("event_type = 'QUEUED'").
				Scan(context.TODO(), &queuedEnd))
			require.True(t, queuedEnd.Equal(*row.AssignedTime), "queued end %s", queuedEnd)

			// Killed before it was pulling.
			requireKilled(t, id, exitFuture)
			row, err = db.AllocationByID(context.TODO(), id)
			require.NoError(t, err)
			require.NotNil(t, row.ReleasedTime)
			require.False(t, row.ReleasedTime.Before(*row.AssignedTime))
			require.Nil(t, row.StartTime)
			require.Nil(t, row.EndTime)

			// The release is recorded once.
			released := *row.ReleasedTime
			require.NoError(t, db.ReleaseAllocation(context.TODO(), id, time.Now().Add(time.Hour)))
			row, err = db.AllocationByID(context.TODO(), id)
			require.NoError(t, err)
			require.True(t, released.Equal(*row.ReleasedTime), "released %s", row.ReleasedTime)
		})
	}
}

// Resources that ran and exited are released when the allocation ends.
func TestAllocationExitedReleasedAtEnd(t *testing.T) {
	closeDB, _, id, q, exitFuture := requireStarted(t)
	defer closeDB()

	rIDs := requireAssignedDevices(t, id, q, []device.Type{device.CUDA})
	q.Put(&sproto.ResourcesStateChanged{ResourcesID: rIDs[0], ResourcesState: sproto.Pulling})
	q.Put(&sproto.ResourcesStateChanged{
		ResourcesID:      rIDs[0],
		ResourcesState:   sproto.Running,
		ResourcesStarted: &sproto.ResourcesStarted{},
	})
	requireState(t, id, model.AllocationStateRunning)
	q.Put(&sproto.ResourcesStateChanged{
		ResourcesID:      rIDs[0],
		ResourcesState:   sproto.Terminated,
		ResourcesStopped: &sproto.ResourcesStopped{},
	})
	requireTerminated(t, id, exitFuture)

	row, err := db.AllocationByID(context.TODO(), id)
	require.NoError(t, err)
	require.NotNil(t, row.StartTime)
	require.NotNil(t, row.EndTime)
	require.NotNil(t, row.ReleasedTime)
	require.True(t, row.ReleasedTime.Equal(*row.EndTime), "released %s, end %s",
		row.ReleasedTime, row.EndTime)
	require.False(t, row.ReleasedTime.Before(*row.StartTime))
}

// Resources that fail before they were pulling are released when they fail.
func TestAllocationFailedBeforePullingReleased(t *testing.T) {
	closeDB, _, id, q, exitFuture := requireStarted(t)
	defer closeDB()

	rIDs := requireAssignedDevices(t, id, q, []device.Type{device.CUDA})
	failed := time.Now().UTC()
	q.Put(&sproto.ResourcesStateChanged{
		ResourcesID:    rIDs[0],
		ResourcesState: sproto.Terminated,
		ResourcesStopped: &sproto.ResourcesStopped{
			Failure: &sproto.ResourcesFailedError{FailureType: sproto.ResourcesFailed, ErrMsg: "failed"},
		},
	})
	requireTerminated(t, id, exitFuture)

	row, err := db.AllocationByID(context.TODO(), id)
	require.NoError(t, err)
	require.NotNil(t, row.ReleasedTime)
	require.False(t, row.ReleasedTime.Before(failed.Truncate(time.Microsecond)),
		"released %s, failed %s", row.ReleasedTime, failed)
	require.Nil(t, row.StartTime)
	require.Nil(t, row.EndTime)
}

// An allocation killed while it waits for resources is not metered.
func TestAllocationKilledWhileQueuedNotMetered(t *testing.T) {
	closeDB, _, id, _, exitFuture := requireStarted(t)
	defer closeDB()

	requireKilled(t, id, exitFuture)
	row, err := db.AllocationByID(context.TODO(), id)
	require.NoError(t, err)
	requireNoMetering(t, row)
}

// addRestorable adds an allocation that a master start restores: it has persisted resources.
func addRestorable(t *testing.T, ar sproto.AllocateRequest, a model.Allocation) {
	a.AllocationID, a.TaskID = ar.AllocationID, ar.TaskID
	a.Slots, a.ResourcePool = ar.SlotsNeeded, ar.ResourcePool
	a.State = ptrs.Ptr(model.AllocationStatePending)
	require.NoError(t, db.AddAllocation(context.TODO(), &a))
	restoredResources := taskmodel.ResourcesWithState{
		ResourceID:   sproto.ResourcesID(cproto.NewID()),
		AllocationID: ar.AllocationID,
	}
	require.NoError(t, restoredResources.Persist())
}

// A restored allocation keeps its assignment, and its release is recorded when it ends.
func TestRestoreKeepsAssignment(t *testing.T) {
	pgDB, closeDB := requireDeps(t)
	defer closeDB()

	restoredTask := db.RequireMockTask(t, pgDB, nil)
	restoredAr := stubAllocateRequest(restoredTask)
	restoredAr.Restore = true
	assigned := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	addRestorable(t, restoredAr, model.Allocation{
		StartTime:       &assigned,
		AssignedTime:    &assigned,
		MeteredKind:     ptrs.Ptr("gpu"),
		MeteredQuantity: ptrs.Ptr(2),
	})

	closeDB, _, id, q, exitFuture := requireStarted(t, func(ar *sproto.AllocateRequest) {
		*ar = restoredAr
	})
	defer closeDB()

	rID, resources := requireAssigned(t, id, q)
	q.Put(&sproto.ResourcesAllocated{
		ID:           id,
		ResourcePool: restoredAr.ResourcePool,
		Resources:    map[sproto.ResourcesID]sproto.Resources{rID: resources},
		Recovered:    true,
	})
	row, err := db.AllocationByID(context.TODO(), id)
	require.NoError(t, err)
	require.True(t, assigned.Equal(*row.AssignedTime), "assigned %s", row.AssignedTime)
	require.Equal(t, ptrs.Ptr("gpu"), row.MeteredKind)
	require.Equal(t, ptrs.Ptr(2), row.MeteredQuantity)

	before := time.Now().UTC().Add(-time.Second)
	requireKilled(t, id, exitFuture)
	row, err = db.AllocationByID(context.TODO(), id)
	require.NoError(t, err)
	require.True(t, assigned.Equal(*row.AssignedTime), "assigned %s", row.AssignedTime)
	require.NotNil(t, row.ReleasedTime)
	require.True(t, row.ReleasedTime.After(before), "released %s", row.ReleasedTime)
}

// Resources that are gone when the master restores them are released at the last heartbeat of the
// master that stopped, not when the restore fails. The allocation ends when it is finalized, no
// earlier than the release.
func TestRestoreFailedReleasedAtHeartbeat(t *testing.T) {
	pgDB, closeDB := requireDeps(t)
	defer closeDB()

	_, err := pgDB.GetOrCreateClusterID("")
	require.NoError(t, err)
	cluster.InitTheLastBootClusterHeartbeat()
	heartbeat := time.Now().UTC()
	if hb := cluster.TheLastBootClusterHeartbeat(); hb != nil {
		heartbeat = hb.UTC()
	}
	t.Logf("last boot heartbeat %v", cluster.TheLastBootClusterHeartbeat())

	restoredTask := db.RequireMockTask(t, pgDB, nil)
	restoredAr := stubAllocateRequest(restoredTask)
	restoredAr.Restore = true
	assigned := heartbeat.Add(-time.Hour).Truncate(time.Millisecond)
	addRestorable(t, restoredAr, model.Allocation{
		StartTime:       &assigned,
		AssignedTime:    &assigned,
		MeteredKind:     ptrs.Ptr("gpu"),
		MeteredQuantity: ptrs.Ptr(2),
	})

	closeDB, _, id, q, exitFuture := requireStarted(t, func(ar *sproto.AllocateRequest) {
		*ar = restoredAr
	})
	defer closeDB()

	q.Put(&sproto.ResourcesFailedError{FailureType: sproto.RestoreError, ErrMsg: "gone"})
	requireTerminated(t, id, exitFuture)

	row, err := db.AllocationByID(context.TODO(), id)
	require.NoError(t, err)
	require.NotNil(t, row.ReleasedTime)
	require.NotNil(t, row.EndTime)
	if cluster.TheLastBootClusterHeartbeat() != nil {
		require.True(t, heartbeat.Truncate(time.Microsecond).Equal(*row.ReleasedTime),
			"released %s, heartbeat %s", row.ReleasedTime, heartbeat)
	} else {
		require.False(t, row.ReleasedTime.Before(heartbeat.Truncate(time.Microsecond)))
	}
	require.False(t, row.EndTime.Before(*row.ReleasedTime), "end %s, released %s",
		row.EndTime, row.ReleasedTime)
}

// An allocation that waited for resources when the master stopped has its assignment cleared, also
// one recorded just before the stop or the mark of an allocation from before metering, and the
// assignment it gets is recorded.
func TestRestoreQueuedClearsAssignment(t *testing.T) {
	stale := time.Now().UTC().Add(-48 * time.Hour)
	for _, tc := range []struct {
		name string
		a    model.Allocation
	}{
		{"assigned", model.Allocation{
			AssignedTime: &stale, MeteredKind: ptrs.Ptr("gpu"), MeteredQuantity: ptrs.Ptr(2),
		}},
		{"legacy", model.Allocation{MeteredKind: ptrs.Ptr("unclassified")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pgDB, closeDB := requireDeps(t)
			defer closeDB()

			queuedTask := db.RequireMockTask(t, pgDB, nil)
			queuedAr := stubAllocateRequest(queuedTask)
			queuedAr.Restore = true
			a := tc.a
			a.AllocationID, a.TaskID = queuedAr.AllocationID, queuedAr.TaskID
			a.Slots, a.ResourcePool = queuedAr.SlotsNeeded, queuedAr.ResourcePool
			a.State = ptrs.Ptr(model.AllocationStatePending)
			a.Ports = map[string]int{}
			require.NoError(t, db.AddAllocation(context.TODO(), &a))

			closeDB, _, id, q, exitFuture := requireStarted(t, func(ar *sproto.AllocateRequest) {
				*ar = queuedAr
			})
			defer closeDB()
			defer requireKilled(t, id, exitFuture)
			row, err := db.AllocationByID(context.TODO(), id)
			require.NoError(t, err)
			requireNoMetering(t, row)

			before := time.Now().UTC().Add(-time.Second)
			requireAssignedDevices(t, id, q, []device.Type{device.CPU})
			row, err = db.AllocationByID(context.TODO(), id)
			require.NoError(t, err)
			require.NotNil(t, row.AssignedTime)
			require.True(t, row.AssignedTime.After(before), "assigned %s", row.AssignedTime)
			require.Equal(t, ptrs.Ptr("cpu"), row.MeteredKind)
			require.Equal(t, ptrs.Ptr(1), row.MeteredQuantity)
		})
	}
}
