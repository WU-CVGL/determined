//go:build integration
// +build integration

package task

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

// mappingTimers counts the goroutines waiting in an allocation's mapping timer.
func mappingTimers() int {
	buf := make([]byte, 8<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "(*allocation).armResourceMetrics.func")
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

	timers := mappingTimers()
	runResource(a, list[0])
	_, ok := allocationTaskValue(t, a)
	require.False(t, ok)
	list[0].requireMapped(t, false)
	require.Equal(t, timers+1, mappingTimers())

	a.resourceMetricsDue()
	value, ok := allocationTaskValue(t, a)
	require.True(t, ok)
	require.InDelta(t, 1, value, 0)
	list[0].requireMapped(t, true)
	require.True(t, waitForCondition(5*time.Second, func() bool { return mappingTimers() == timers }))

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
	require.True(t, waitForCondition(5*time.Second, func() bool {
		_, ok := allocationTaskValue(t, a)
		return ok
	}))
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

	timers := mappingTimers()
	runResource(a, list[0])
	require.Equal(t, timers+1, mappingTimers())
	terminateResource(a, list[0])
	require.NotNil(t, a.exited)

	// The timer stops with the allocation, and a timer that fired as the allocation finalized
	// exports nothing.
	require.True(t, waitForCondition(5*time.Second, func() bool { return mappingTimers() == timers }))
	a.resourceMetricsDue()
	_, ok := allocationTaskValue(t, a)
	require.False(t, ok)
	list[0].requireMapped(t, false)
}

func TestAllocationWithoutDelayIsMappedAtOnce(t *testing.T) {
	_, closeDB, a, list := startMappedAllocation(t, 0, 1)
	defer closeDB()
	defer a.detach()

	timers := mappingTimers()
	runResource(a, list[0])
	_, ok := allocationTaskValue(t, a)
	require.True(t, ok)
	list[0].requireMapped(t, true)
	require.Equal(t, timers, mappingTimers())

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

	timers := mappingTimers()
	runResource(a, list[0])
	runResource(a, list[1])
	require.Equal(t, timers+1, mappingTimers(), "one timer per allocation")
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
