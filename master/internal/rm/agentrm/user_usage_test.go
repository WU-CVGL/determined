package agentrm

import (
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/userusage"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
)

// TestUserUsageCountMatchesPoolSummary checks that the user pages' counts of the allocations that
// the pool holds, summed over their owners, equal the pool summary: the Aux containers equal
// numActiveAuxContainers (the agents' numUsedZeroSlots), and the GPUs or CPU slots equal
// numActiveSlots.
func TestUserUsageCountMatchesPoolSummary(t *testing.T) {
	newAgent := func(id string, typ device.Type, n int) *agentState {
		state := newAgentState(aproto.ID(id), 100)
		for i := 0; i < n; i++ {
			state.Devices[device.Device{ID: device.ID(i), Type: typ}] = nil
		}
		return state
	}
	pools := map[string]map[aproto.ID]*agentState{
		"gpu": {"g1": newAgent("g1", device.CUDA, 4), "g2": newAgent("g2", device.CUDA, 2)},
		"cpu": {"p1": newAgent("p1", device.CPU, 4)},
	}

	type container struct {
		pool, agent string
		slots       int
	}
	type task struct {
		owner      string
		containers []container
	}
	tasks := map[model.TaskID]task{
		"t1": {"a", []container{{"gpu", "g1", 2}}},
		"t2": {"b", []container{{"gpu", "g1", 0}}},
		"t3": {"a", []container{{"gpu", "g1", 0}}},
		"t4": {"b", []container{{"gpu", "g1", 2}, {"gpu", "g2", 1}}},
		"t5": {"b", []container{{"gpu", "g2", 0}}},
		"t6": {"a", []container{{"cpu", "p1", 4}}},
		"t7": {"a", []container{{"cpu", "p1", 0}}},
		"t8": {"b", []container{{"cpu", "p1", 0}}},
	}

	// The pool reserves the devices and makes the resources as allocateResources does; each
	// allocation is metered as the allocation records its assignment.
	type counts struct{ gpus, cpuSlots, aux int }
	owned := map[string]map[string]counts{}
	for taskID, tk := range tasks {
		req := &sproto.AllocateRequest{TaskID: taskID, AllocationID: model.AllocationID(taskID + ".1")}
		var summaries []sproto.ResourcesSummary
		slots := 0
		for _, c := range tk.containers {
			agent := pools[c.pool][aproto.ID(c.agent)]
			cid := cproto.NewID()
			res, err := agent.allocateFreeDevices(c.slots, cid, deviceSelection{})
			require.NoError(t, err)
			r := containerResources{req: req, agent: agent, containerID: cid, devices: res.devices}
			summaries = append(summaries, r.Summary())
			slots += c.slots
		}
		pool := tk.containers[0].pool
		if owned[tk.owner] == nil {
			owned[tk.owner] = map[string]counts{}
		}
		sum := owned[tk.owner][pool]
		switch kind, n := userusage.Assigned(summaries, slots); kind {
		case userusage.KindGPU:
			sum.gpus += n
		case userusage.KindCPU:
			sum.cpuSlots += n
		case userusage.KindAux:
			sum.aux += n
		default:
			t.Fatalf("task %s is metered as %s", taskID, kind)
		}
		owned[tk.owner][pool] = sum
	}

	rp := &resourcePool{syslog: logrus.WithField("component", "test")}
	for name, agents := range pools {
		var got counts
		for _, o := range owned {
			got.gpus += o[name].gpus
			got.cpuSlots += o[name].cpuSlots
			got.aux += o[name].aux
		}
		numUsedZeroSlots := 0
		for _, a := range agents {
			numUsedZeroSlots += a.numUsedZeroSlots()
		}
		summary := rp.resourceSummaryFromAgentStates(agents)
		require.Equal(t, summary.numActiveAuxContainers, got.aux, name)
		require.Equal(t, numUsedZeroSlots, got.aux, name)
		require.Equal(t, summary.numActiveSlots, got.gpus+got.cpuSlots, name)
	}
	require.Equal(t, counts{gpus: 2, aux: 1}, owned["a"]["gpu"])
	require.Equal(t, counts{gpus: 3, aux: 2}, owned["b"]["gpu"])
	require.Equal(t, counts{cpuSlots: 4, aux: 1}, owned["a"]["cpu"])
	require.Equal(t, counts{aux: 1}, owned["b"]["cpu"])
}
