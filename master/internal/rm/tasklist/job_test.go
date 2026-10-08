package tasklist

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/logger"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

// placedResources are the resources of one container: the devices it holds on one agent.
type placedResources struct {
	agentID aproto.ID
	ids     []device.ID
}

func (r placedResources) Summary() sproto.ResourcesSummary {
	devices := make([]device.Device, 0, len(r.ids))
	for _, id := range r.ids {
		devices = append(devices, device.Device{ID: id, Type: device.CUDA})
	}
	return sproto.ResourcesSummary{
		ResourcesID:  sproto.ResourcesID(string(r.agentID) + "-container"),
		AgentDevices: map[aproto.ID][]device.Device{r.agentID: devices},
	}
}

func (placedResources) Start(logger.Context, tasks.TaskSpec, sproto.ResourcesRuntimeInfo) error {
	return nil
}

func (placedResources) Kill(logger.Context) {}

// addRequest adds a request of the job to the task list, allocated to the given containers unless
// there are none.
func addRequest(
	l *TaskList, id model.AllocationID, jobID model.JobID, visible bool, containers ...placedResources,
) {
	l.AddTask(&sproto.AllocateRequest{
		AllocationID: id, JobID: jobID, IsUserVisible: visible, JobSubmissionTime: time.Now(),
	})
	if len(containers) == 0 {
		return
	}
	resources := sproto.ResourceList{}
	for i, c := range containers {
		resources[sproto.ResourcesID(string(id)+string(rune('a'+i)))] = c
	}
	l.AddAllocation(id, &sproto.ResourcesAllocated{
		ID: id, Resources: resources, JobSubmissionTime: time.Now(),
	})
}

func TestAddPlacement(t *testing.T) {
	l := New()
	// One agent: two trials of an experiment, and a queued third one.
	addRequest(l, "exp.1", "exp", true, placedResources{"node01", []device.ID{5, 0}})
	addRequest(l, "exp.2", "exp", true, placedResources{"node01", []device.ID{6, 1}})
	addRequest(l, "exp.3", "exp", true)
	// Two agents: a distributed trial takes whole nodes.
	addRequest(l, "dist.1", "dist", true,
		placedResources{"node03", []device.ID{0, 1}}, placedResources{"node04", []device.ID{0, 1}})
	// A zero-slot container holds no device.
	addRequest(l, "tb.1", "tb", true, placedResources{"node01", nil})
	// A queued job.
	addRequest(l, "queued.1", "queued", true)
	// A request that is not user-visible, of a job that is.
	addRequest(l, "gc.1", "exp", false, placedResources{"node02", []device.ID{3}})
	// A job missing from the queue.
	addRequest(l, "gone.1", "gone", true, placedResources{"node02", []device.ID{2}})

	jobQ := sproto.AQueue{"exp": {}, "dist": {}, "tb": {}, "queued": {}}
	AddPlacement(jobQ, l)

	exp := jobQ["exp"].Placement
	require.Len(t, exp, 1)
	require.ElementsMatch(t, []device.ID{0, 1, 5, 6}, exp["node01"])
	require.Equal(t, map[aproto.ID][]device.ID{"node03": {0, 1}, "node04": {0, 1}},
		jobQ["dist"].Placement)
	require.Nil(t, jobQ["tb"].Placement)
	require.Nil(t, jobQ["queued"].Placement)
	require.NotContains(t, jobQ, model.JobID("gone"))
}
