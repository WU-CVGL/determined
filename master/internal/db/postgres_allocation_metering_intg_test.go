//go:build integration
// +build integration

package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/userusage"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

// addMeteringAllocation adds an allocation of a new task with the given fields.
func addMeteringAllocation(t *testing.T, pgDB *PgDB, a model.Allocation) model.AllocationID {
	tIn := RequireMockTask(t, pgDB, nil)
	a.AllocationID = model.AllocationID(fmt.Sprintf("%s.0", tIn.TaskID))
	a.TaskID = tIn.TaskID
	if a.ResourcePool == "" {
		a.ResourcePool = "default"
	}
	require.NoError(t, AddAllocation(context.Background(), &a))
	return a.AllocationID
}

func requireAllocation(t *testing.T, id model.AllocationID) *model.Allocation {
	a, err := AllocationByID(context.Background(), id)
	require.NoError(t, err)
	return a
}

func requireTimeEqual(t *testing.T, expected time.Time, actual *time.Time, msg string) {
	require.NotNil(t, actual, msg)
	require.True(t, expected.Equal(*actual), "%s: expected %s, got %s", msg, expected, *actual)
}

func TestAllocationAssignAndRelease(t *testing.T) {
	ctx := context.Background()
	pgDB, closeDB := MustResolveTestPostgres(t)
	defer closeDB()
	MustMigrateTestPostgres(t, pgDB, MigrationsFromDB)

	assigned := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	id := addMeteringAllocation(t, pgDB, model.Allocation{
		Slots: 2, State: ptrs.Ptr(model.AllocationStatePending),
	})

	// A release without an assignment records nothing.
	require.NoError(t, ReleaseAllocation(ctx, id, assigned))
	a := requireAllocation(t, id)
	require.Nil(t, a.AssignedTime)
	require.Nil(t, a.ReleasedTime)
	require.Nil(t, a.MeteredKind)
	require.Nil(t, a.MeteredQuantity)

	// Only the first assignment counts.
	require.NoError(t, AssignAllocation(ctx, id, assigned, userusage.KindGPU, 2))
	require.NoError(t, AssignAllocation(ctx, id, assigned.Add(time.Minute), userusage.KindCPU, 4))
	a = requireAllocation(t, id)
	requireTimeEqual(t, assigned, a.AssignedTime, "assigned")
	require.Equal(t, ptrs.Ptr("gpu"), a.MeteredKind)
	require.Equal(t, ptrs.Ptr(2), a.MeteredQuantity)

	// A release before the assignment is at the assignment, and only the first counts.
	require.NoError(t, ReleaseAllocation(ctx, id, assigned.Add(-time.Minute)))
	require.NoError(t, ReleaseAllocation(ctx, id, assigned.Add(time.Hour)))
	requireTimeEqual(t, assigned, requireAllocation(t, id).ReleasedTime, "released")

	// A time in another location is stored as UTC.
	other := addMeteringAllocation(t, pgDB, model.Allocation{Slots: 1})
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	require.NoError(t, AssignAllocation(ctx, other, assigned.In(shanghai), userusage.KindCPU, 1))
	require.NoError(t, ReleaseAllocation(ctx, other, assigned.Add(time.Minute).In(shanghai)))
	a = requireAllocation(t, other)
	requireTimeEqual(t, assigned, a.AssignedTime, "assigned in another location")
	requireTimeEqual(t, assigned.Add(time.Minute), a.ReleasedTime, "released in another location")

	// A cleared assignment is recorded again.
	require.NoError(t, ClearAllocationAssignment(ctx, other))
	a = requireAllocation(t, other)
	require.Nil(t, a.AssignedTime)
	require.Nil(t, a.ReleasedTime)
	require.Nil(t, a.MeteredKind)
	require.Nil(t, a.MeteredQuantity)
	require.NoError(t, AssignAllocation(ctx, other, assigned.Add(time.Hour), userusage.KindAux, 1))
	requireTimeEqual(t, assigned.Add(time.Hour), requireAllocation(t, other).AssignedTime, "again")
}

// A reused allocation ID starts without the earlier allocation's metering.
func TestAllocationReusedIDMetering(t *testing.T) {
	ctx := context.Background()
	pgDB, closeDB := MustResolveTestPostgres(t)
	defer closeDB()
	MustMigrateTestPostgres(t, pgDB, MigrationsFromDB)

	now := time.Now().UTC()
	id := addMeteringAllocation(t, pgDB, model.Allocation{Slots: 1})
	require.NoError(t, AssignAllocation(ctx, id, now, userusage.KindGPU, 1))
	require.NoError(t, ReleaseAllocation(ctx, id, now))

	a := requireAllocation(t, id)
	require.NoError(t, AddAllocation(ctx, &model.Allocation{
		AllocationID: id, TaskID: a.TaskID, ResourcePool: a.ResourcePool, Slots: 1,
	}))
	a = requireAllocation(t, id)
	require.Nil(t, a.AssignedTime)
	require.Nil(t, a.ReleasedTime)
	require.Nil(t, a.MeteredKind)
	require.Nil(t, a.MeteredQuantity)
}

// A master start releases the resources that the stopped master had assigned and not released at
// its last heartbeat, and never those of live allocations.
func TestCloseOpenAllocationsRelease(t *testing.T) {
	ctx := context.Background()
	pgDB, closeDB := MustResolveTestPostgres(t)
	defer closeDB()
	MustMigrateTestPostgres(t, pgDB, MigrationsFromDB)

	_, err := pgDB.GetOrCreateClusterID("")
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Millisecond)
	heartbeat := now.Add(-time.Hour)
	require.NoError(t, pgDB.UpdateClusterHeartBeat(heartbeat))
	boot := now.Add(-time.Minute)

	running := ptrs.Ptr(model.AllocationStateRunning)
	terminated := ptrs.Ptr(model.AllocationStateTerminated)
	add := func(a model.Allocation, assigned *time.Time) model.AllocationID {
		id := addMeteringAllocation(t, pgDB, a)
		if assigned != nil {
			require.NoError(t, AssignAllocation(ctx, id, *assigned, userusage.KindGPU, 1))
		}
		return id
	}
	assignedBefore := heartbeat.Add(-time.Hour)
	assignedAfterHeartbeat := heartbeat.Add(time.Minute / 2)
	assignedAfterBoot := boot.Add(time.Second)
	end := heartbeat.Add(-time.Minute)

	open := add(model.Allocation{State: running, StartTime: &assignedBefore}, &assignedBefore)
	// Assigned between the last heartbeat and the stop: no time is metered.
	late := add(model.Allocation{State: running}, &assignedAfterHeartbeat)
	excluded := add(model.Allocation{State: running}, &assignedBefore)
	// Assigned in this master's lifetime, after the list of live allocations was taken: one that
	// has not started yet and one that runs.
	current := add(model.Allocation{State: ptrs.Ptr(model.AllocationStateAssigned)}, &assignedAfterBoot)
	startedAfterBoot := assignedAfterBoot.Add(time.Second)
	currentRunning := add(model.Allocation{State: running, StartTime: &startedAfterBoot},
		&assignedAfterBoot)
	live := map[model.AllocationID]*model.Allocation{
		current: requireAllocation(t, current), currentRunning: requireAllocation(t, currentRunning),
	}
	never := add(model.Allocation{State: ptrs.Ptr(model.AllocationStatePending)}, nil)
	// Terminated, but its release was not written: released at its end, or without one at its
	// assignment.
	ended := add(model.Allocation{State: terminated, StartTime: &assignedBefore, EndTime: &end},
		&assignedBefore)
	unended := add(model.Allocation{State: terminated}, &assignedBefore)

	for i := 0; i < 2; i++ {
		require.NoError(t, CloseOpenAllocations(ctx, []model.AllocationID{excluded}, boot))
		requireTimeEqual(t, heartbeat, requireAllocation(t, open).ReleasedTime, "open")
		requireTimeEqual(t, assignedAfterHeartbeat, requireAllocation(t, late).ReleasedTime, "late")
		require.Nil(t, requireAllocation(t, excluded).ReleasedTime, "excluded")
		for id, before := range live {
			// The cleanup leaves an allocation assigned after boot exactly as it was, and it
			// accumulates usage until the time of the request.
			a := requireAllocation(t, id)
			require.Equal(t, before.State, a.State, "assigned after boot: state")
			require.Equal(t, before.StartTime, a.StartTime, "assigned after boot: start")
			require.Equal(t, before.EndTime, a.EndTime, "assigned after boot: end")
			require.Equal(t, before.ReleasedTime, a.ReleasedTime, "assigned after boot: release")
			require.Nil(t, a.EndTime, "assigned after boot: end")
			require.Nil(t, a.ReleasedTime, "assigned after boot: release")
			asOf := now.Add(time.Hour)
			require.Equal(t, []userusage.Interval{{
				Start: assignedAfterBoot, End: asOf, Kind: userusage.KindGPU, Quantity: 1,
			}}, userusage.Intervals([]userusage.Record{{
				Kind:     userusage.Kind(*a.MeteredKind),
				Quantity: *a.MeteredQuantity,
				Terminal: *a.State == model.AllocationStateTerminated,
				Assigned: a.AssignedTime,
				Released: a.ReleasedTime,
				Start:    a.StartTime,
				End:      a.EndTime,
			}}, asOf), "assigned after boot: usage")
		}
		require.Nil(t, requireAllocation(t, never).ReleasedTime, "never assigned")
		// The others are closed.
		for _, id := range []model.AllocationID{open, late, never} {
			a := requireAllocation(t, id)
			require.Equal(t, terminated, a.State, "closed: state")
			require.NotNil(t, a.EndTime, "closed: end")
		}
		require.Equal(t, running, requireAllocation(t, excluded).State, "excluded: state")
		require.Nil(t, requireAllocation(t, excluded).EndTime, "excluded: end")
		requireTimeEqual(t, end, requireAllocation(t, ended).ReleasedTime, "terminated with an end")
		requireTimeEqual(t, assignedBefore, requireAllocation(t, unended).ReleasedTime,
			"terminated without an end")
		// A later heartbeat does not move a release at the next start.
		require.NoError(t, pgDB.UpdateClusterHeartBeat(now))
	}
}
