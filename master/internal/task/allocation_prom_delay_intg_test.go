//go:build integration
// +build integration

package task

import (
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

// mappingTimer reports whether the allocation's mapping timer is pending, and returns the
// channel closed when its goroutine returns, or nil if no timer was started.
func mappingTimer(a *allocation) (bool, chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.metricsStopTimer != nil, a.metricsTimerDone
}

// requireTimerReturns waits for the goroutine of a started mapping timer to return. When it fired,
// it has exported the mappings by then.
func requireTimerReturns(t *testing.T, a *allocation) {
	t.Helper()
	_, done := mappingTimer(a)
	require.NotNil(t, done)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the mapping timer's goroutine did not return")
	}
	pending, _ := mappingTimer(a)
	require.False(t, pending)
}

func startMappedAllocation(
	t *testing.T, delay time.Duration, n int,
) (*mocks.ResourceManager, func(), *allocation, []mappedResource) {
	rm, _, closeDB, a := setup(t)
	rm.On("Release", mock.Anything, mock.Anything).Return(nil)
	a.mu.Lock()
	a.metricsDelay = delay
	a.mu.Unlock()
	list, resources := newMappedResources(a, n, false)
	a.HandleRMEvent(&sproto.ResourcesAllocated{
		ID: a.req.AllocationID, ResourcePool: a.req.ResourcePool, Resources: resources,
	})
	return rm, closeDB, a, list
}

func TestAllocationIsMappedWhenDue(t *testing.T) {
	_, closeDB, a, list := startMappedAllocation(t, time.Hour, 1)
	defer closeDB()
	defer a.detach()

	runResource(a, list[0])
	_, ok := allocationTaskValue(t, a)
	require.False(t, ok)
	list[0].requireMapped(t, false)
	pending, _ := mappingTimer(a)
	require.True(t, pending)

	a.resourceMetricsDue()
	value, ok := allocationTaskValue(t, a)
	require.True(t, ok)
	require.InDelta(t, 1, value, 0)
	list[0].requireMapped(t, true)
	requireTimerReturns(t, a)

	terminateResource(a, list[0])
	_, ok = allocationTaskValue(t, a)
	require.False(t, ok)
	list[0].requireMapped(t, false)
}

func TestAllocationIsMappedByItsTimer(t *testing.T) {
	_, closeDB, a, list := startMappedAllocation(t, 300*time.Millisecond, 1)
	defer closeDB()
	defer a.detach()

	runResource(a, list[0])
	_, ok := allocationTaskValue(t, a)
	require.False(t, ok)
	// The timer exports all mappings at once; wait for it rather than for the first of them.
	requireTimerReturns(t, a)
	_, ok = allocationTaskValue(t, a)
	require.True(t, ok)
	list[0].requireMapped(t, true)

	terminateResource(a, list[0])
	_, ok = allocationTaskValue(t, a)
	require.False(t, ok)
	list[0].requireMapped(t, false)
}

func TestAllocationStoppedBeforeTheDelayIsNeverMapped(t *testing.T) {
	_, closeDB, a, list := startMappedAllocation(t, time.Hour, 1)
	defer closeDB()
	defer a.detach()

	runResource(a, list[0])
	pending, _ := mappingTimer(a)
	require.True(t, pending)
	terminateResource(a, list[0])
	require.NotNil(t, a.exited)

	// The timer stops with the allocation, and a timer that fired as the allocation finalized
	// exports nothing.
	requireTimerReturns(t, a)
	a.resourceMetricsDue()
	_, ok := allocationTaskValue(t, a)
	require.False(t, ok)
	list[0].requireMapped(t, false)
}

func TestAllocationWithoutDelayIsMappedAtOnce(t *testing.T) {
	_, closeDB, a, list := startMappedAllocation(t, 0, 1)
	defer closeDB()
	defer a.detach()

	runResource(a, list[0])
	_, ok := allocationTaskValue(t, a)
	require.True(t, ok)
	list[0].requireMapped(t, true)
	pending, done := mappingTimer(a)
	require.False(t, pending)
	require.Nil(t, done, "no timer")

	terminateResource(a, list[0])
	_, ok = allocationTaskValue(t, a)
	require.False(t, ok)
	list[0].requireMapped(t, false)
}

// The delay counts from the allocation's start, so a container that starts once it has passed
// is mapped at once, and one that stops before it never is.
func TestMultiContainerAllocationMappings(t *testing.T) {
	_, closeDB, a, list := startMappedAllocation(t, time.Hour, 3)
	defer closeDB()
	defer a.detach()

	runResource(a, list[0])
	_, first := mappingTimer(a)
	runResource(a, list[1])
	pending, second := mappingTimer(a)
	require.True(t, pending)
	require.NotNil(t, first)
	require.True(t, first == second, "one timer per allocation")
	terminateResource(a, list[0])
	require.Nil(t, a.exited)

	a.resourceMetricsDue()
	value, ok := allocationTaskValue(t, a)
	require.True(t, ok)
	require.InDelta(t, 1, value, 0)
	list[0].requireMapped(t, false)
	list[1].requireMapped(t, true)

	runResource(a, list[2])
	value, _ = allocationTaskValue(t, a)
	require.InDelta(t, 2, value, 0)
	list[2].requireMapped(t, true)

	terminateResource(a, list[1])
	value, _ = allocationTaskValue(t, a)
	require.InDelta(t, 1, value, 0)
	list[1].requireMapped(t, false)
	terminateResource(a, list[2])
	require.NotNil(t, a.exited)
	_, ok = allocationTaskValue(t, a)
	require.False(t, ok)
	for _, r := range list {
		r.requireMapped(t, false)
	}
}

// A restored allocation without a start time waits for the whole delay.
func TestRestoredAllocationWithoutStartWaitsTheDelay(t *testing.T) {
	_, _, closeDB, a := setup(t)
	defer closeDB()
	defer a.detach()
	a.req.Restore = true
	a.model.State = ptrs.Ptr(model.AllocationStateRunning)
	a.model.StartTime = nil
	a.metricsDelay = time.Hour
	list, resources := newMappedResources(a, 1, true)
	a.mu.Lock()
	err := a.resourcesAllocated(&sproto.ResourcesAllocated{
		ID: a.req.AllocationID, ResourcePool: a.req.ResourcePool, Resources: resources,
	})
	a.mu.Unlock()
	require.NoError(t, err)
	_, ok := allocationTaskValue(t, a)
	require.False(t, ok)
	list[0].requireMapped(t, false)
}
