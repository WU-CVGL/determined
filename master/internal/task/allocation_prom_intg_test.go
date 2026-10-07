//go:build integration
// +build integration

package task

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/prom"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

// metricValue reads only this test's unique label set from the dedicated
// DetStateMetrics registry, independently of other allocation metrics.
func allocationMetricValue(t *testing.T, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := prom.DetStateMetrics.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			found := make(map[string]string, len(metric.GetLabel()))
			for _, pair := range metric.GetLabel() {
				found[pair.GetName()] = pair.GetValue()
			}
			match := true
			for key, value := range labels {
				if found[key] != value {
					match = false
					break
				}
			}
			if match {
				return metric.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func TestRestoredAllocationRebuildsResourceMetricsOnce(t *testing.T) {
	rm, _, closeDB, a := setup(t)
	defer closeDB()
	defer a.detach()
	a.req.Restore = true
	a.model.State = ptrs.Ptr(model.AllocationStateRunning)
	// It has run for longer than the default observability.task_mapping_delay, 5m, so it is
	// exported as soon as it is restored.
	a.model.StartTime = ptrs.Ptr(time.Now().UTC().Add(-10 * time.Minute))
	a.req.Name = "trial"

	resources := make(map[sproto.ResourcesID]sproto.Resources)
	ids := make([]sproto.ResourcesID, 0, 2)
	for range 2 {
		containerID := cproto.ID(uuid.NewString())
		rID := sproto.ResourcesID(containerID)
		gpuUUID := uuid.NewString()
		started := &sproto.ResourcesStarted{NativeResourcesID: uuid.NewString()}
		summary := sproto.ResourcesSummary{
			AllocationID: a.req.AllocationID, ResourcesID: rID,
			ResourcesType: sproto.ResourcesTypeDockerContainer,
			ContainerID:   &containerID, Started: started,
			AgentDevices: map[aproto.ID][]device.Device{
				"agent": {{UUID: gpuUUID, Type: device.CUDA}},
			},
		}
		resource := &mocks.Resources{}
		resource.On("Summary").Return(summary)
		resources[rID] = resource
		ids = append(ids, rID)
	}
	// A restored allocation can also contain a resource that never started.
	// Its termination must not decrement a mapping that was never registered.
	unstartedID := sproto.ResourcesID(cproto.ID(uuid.NewString()))
	unstarted := &mocks.Resources{}
	unstarted.On("Summary").Return(sproto.ResourcesSummary{
		AllocationID: a.req.AllocationID, ResourcesID: unstartedID,
		ResourcesType: sproto.ResourcesTypeDockerContainer,
	})
	resources[unstartedID] = unstarted
	require.NoError(t, a.resourcesAllocated(&sproto.ResourcesAllocated{
		ID: a.req.AllocationID, ResourcePool: a.req.ResourcePool, Resources: resources,
	}))
	allocationLabels := map[string]string{
		"allocation_id": a.req.AllocationID.String(), "task_id": a.req.TaskID.String(),
		"task_actor": a.req.Name, "job_id": a.req.JobID.String(),
	}
	allocationMetric := "det_allocation_id_task_id_task_actor"
	value, ok := allocationMetricValue(t, allocationMetric, allocationLabels)
	require.True(t, ok)
	require.InDelta(t, 2, value, 0)

	// A resent Running notification for a restored resource has Started set and
	// must not count again. Explicit registration is also idempotent.
	a.HandleRMEvent(&sproto.ResourcesStateChanged{
		ResourcesID: ids[0], ResourcesState: sproto.Running,
		ResourcesStarted: &sproto.ResourcesStarted{},
	})
	a.registerResourceMetrics(ids[0])
	value, _ = allocationMetricValue(t, allocationMetric, allocationLabels)
	require.InDelta(t, 2, value, 0)

	rm.On("Release", mock.Anything, mock.Anything).Return(nil)
	a.HandleRMEvent(&sproto.ResourcesStateChanged{
		ResourcesID:      unstartedID,
		ResourcesState:   sproto.Terminated,
		ResourcesStopped: &sproto.ResourcesStopped{},
	})
	value, _ = allocationMetricValue(t, allocationMetric, allocationLabels)
	require.InDelta(t, 2, value, 0, "unstarted resource must not decrement mappings")
	for i, id := range ids {
		summary := resources[id].Summary()
		containerLabels := map[string]string{
			"allocation_id": a.req.AllocationID.String(), "container_id": summary.ContainerID.String(),
		}
		value, ok = allocationMetricValue(t, "det_container_id_allocation_id", containerLabels)
		require.True(t, ok)
		require.InDelta(t, 1, value, 0)
		runtimeLabels := map[string]string{
			"container_runtime_id": summary.Started.NativeResourcesID,
			"container_id":         summary.ContainerID.String(),
		}
		value, ok = allocationMetricValue(t, "det_container_id_runtime_container_id", runtimeLabels)
		require.True(t, ok)
		require.InDelta(t, 1, value, 0)
		gpuLabels := map[string]string{
			"gpu_uuid":     summary.AgentDevices["agent"][0].UUID,
			"container_id": summary.ContainerID.String(),
		}
		value, ok = allocationMetricValue(t, "det_gpu_uuid_container_id", gpuLabels)
		require.True(t, ok)
		require.InDelta(t, 1, value, 0)

		a.HandleRMEvent(&sproto.ResourcesStateChanged{
			ResourcesID:      id,
			ResourcesState:   sproto.Terminated,
			ResourcesStopped: &sproto.ResourcesStopped{},
		})
		value, ok = allocationMetricValue(t, allocationMetric, allocationLabels)
		if i == 0 {
			require.True(t, ok)
			require.InDelta(t, 1, value, 0)
		} else {
			require.False(t, ok, "an ended allocation must not stay exported as 0")
		}
		_, ok = allocationMetricValue(t, "det_container_id_allocation_id", containerLabels)
		require.False(t, ok, "a stopped container must not stay exported as 0")
		_, ok = allocationMetricValue(t, "det_container_id_runtime_container_id", runtimeLabels)
		require.False(t, ok)
		_, ok = allocationMetricValue(t, "det_gpu_uuid_container_id", gpuLabels)
		require.False(t, ok)
	}
}

// mappedResource is a container with one GPU whose mappings a test follows.
type mappedResource struct {
	id      sproto.ResourcesID
	summary sproto.ResourcesSummary
	started *sproto.ResourcesStarted
}

// newMappedResources makes n containers of the allocation. Restored containers have started.
func newMappedResources(
	a *allocation, n int, restored bool,
) ([]mappedResource, map[sproto.ResourcesID]sproto.Resources) {
	list := make([]mappedResource, 0, n)
	resources := make(map[sproto.ResourcesID]sproto.Resources, n)
	for range n {
		containerID := cproto.ID(uuid.NewString())
		r := mappedResource{
			id:      sproto.ResourcesID(containerID),
			started: &sproto.ResourcesStarted{NativeResourcesID: uuid.NewString()},
		}
		r.summary = sproto.ResourcesSummary{
			AllocationID: a.req.AllocationID, ResourcesID: r.id,
			ResourcesType: sproto.ResourcesTypeDockerContainer,
			ContainerID:   &containerID,
			AgentDevices: map[aproto.ID][]device.Device{
				"agent": {{UUID: uuid.NewString(), Type: device.CUDA}},
			},
		}
		if restored {
			r.summary.Started = r.started
		}
		resource := &mocks.Resources{}
		resource.On("Summary").Return(r.summary)
		resource.On("Start", mock.Anything, mock.Anything, mock.Anything).Return(nil)
		resource.On("Kill", mock.Anything).Return()
		resources[r.id] = resource
		list = append(list, r)
	}
	return list, resources
}

// requireMapped checks whether the container, runtime and GPU mappings of r are exported.
func (r mappedResource) requireMapped(t *testing.T, exported bool) {
	t.Helper()
	cID := r.summary.ContainerID.String()
	for name, labels := range map[string]map[string]string{
		"det_container_id_allocation_id": {
			"container_id": cID, "allocation_id": r.summary.AllocationID.String(),
		},
		"det_container_id_runtime_container_id": {
			"container_id": cID, "container_runtime_id": r.started.NativeResourcesID,
		},
		"det_gpu_uuid_container_id": {
			"container_id": cID, "gpu_uuid": r.summary.AgentDevices["agent"][0].UUID,
		},
	} {
		value, ok := allocationMetricValue(t, name, labels)
		require.Equalf(t, exported, ok, "%s exported", name)
		if ok {
			require.InDelta(t, 1, value, 0)
		}
	}
}

// allocationTaskValue reads the allocation's allocation/task mapping.
func allocationTaskValue(t *testing.T, a *allocation) (float64, bool) {
	t.Helper()
	return allocationMetricValue(t, "det_allocation_id_task_id_task_actor", map[string]string{
		"allocation_id": a.req.AllocationID.String(), "task_id": a.req.TaskID.String(),
	})
}

func runResource(a *allocation, r mappedResource) {
	a.HandleRMEvent(&sproto.ResourcesStateChanged{
		ResourcesID: r.id, ResourcesState: sproto.Pulling,
	})
	a.HandleRMEvent(&sproto.ResourcesStateChanged{
		ResourcesID: r.id, ResourcesState: sproto.Running, ResourcesStarted: r.started,
	})
}

func terminateResource(a *allocation, r mappedResource) {
	a.HandleRMEvent(&sproto.ResourcesStateChanged{
		ResourcesID: r.id, ResourcesState: sproto.Terminated,
		ResourcesStopped: &sproto.ResourcesStopped{},
	})
}

// An allocation that ends before observability.task_mapping_delay (5m by default) is never
// exported.
func TestShortAllocationIsNeverMapped(t *testing.T) {
	rm, _, closeDB, a := setup(t)
	defer closeDB()
	defer a.detach()
	rm.On("Release", mock.Anything, mock.Anything).Return(nil)
	list, resources := newMappedResources(a, 1, false)
	a.HandleRMEvent(&sproto.ResourcesAllocated{
		ID: a.req.AllocationID, ResourcePool: a.req.ResourcePool, Resources: resources,
	})

	runResource(a, list[0])
	require.NotNil(t, a.model.StartTime)
	_, ok := allocationTaskValue(t, a)
	require.False(t, ok, "a running allocation younger than the delay is mapped")
	list[0].requireMapped(t, false)

	terminateResource(a, list[0])
	require.NotNil(t, a.exited)
	_, ok = allocationTaskValue(t, a)
	require.False(t, ok)
	list[0].requireMapped(t, false)
}

// A restored allocation that is younger than the delay is exported when it has run for the
// delay, counted from its persisted start.
func TestRestoredAllocationIsMappedAfterTheRestOfTheDelay(t *testing.T) {
	rm, _, closeDB, a := setup(t)
	defer closeDB()
	defer a.detach()
	a.req.Restore = true
	a.model.State = ptrs.Ptr(model.AllocationStateRunning)
	a.model.StartTime = ptrs.Ptr(time.Now().UTC().Add(-5*time.Minute + 500*time.Millisecond))
	list, resources := newMappedResources(a, 1, true)
	a.mu.Lock()
	err := a.resourcesAllocated(&sproto.ResourcesAllocated{
		ID: a.req.AllocationID, ResourcePool: a.req.ResourcePool, Resources: resources,
	})
	a.mu.Unlock()
	require.NoError(t, err)
	_, ok := allocationTaskValue(t, a)
	require.False(t, ok, "a restored allocation younger than the delay is mapped at once")
	list[0].requireMapped(t, false)

	// The rest of the delay is 500ms; requireTimerReturns waits at most 5s.
	requireTimerReturns(t, a)
	_, ok = allocationTaskValue(t, a)
	require.True(t, ok, "the restored allocation is not mapped after the rest of the delay")
	list[0].requireMapped(t, true)

	rm.On("Release", mock.Anything, mock.Anything).Return(nil)
	terminateResource(a, list[0])
	_, ok = allocationTaskValue(t, a)
	require.False(t, ok)
	list[0].requireMapped(t, false)
}
