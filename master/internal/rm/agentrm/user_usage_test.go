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

// TestUserUsageCountMatchesPoolSummary checks that the user pages' counts of the containers that
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

	// The pool reserves the devices and makes the resources as allocateResources does.
	counts := map[string]map[string]userusage.Counts{}
	for taskID, tk := range tasks {
		req := &sproto.AllocateRequest{TaskID: taskID, AllocationID: model.AllocationID(taskID + ".1")}
		for _, c := range tk.containers {
			agent := pools[c.pool][aproto.ID(c.agent)]
			cid := cproto.NewID()
			res, err := agent.allocateFreeDevices(c.slots, cid, deviceSelection{})
			require.NoError(t, err)
			r := containerResources{req: req, agent: agent, containerID: cid, devices: res.devices}

			rc := userusage.Count(r.Summary(), false, c.pool == "cpu")
			if counts[tk.owner] == nil {
				counts[tk.owner] = map[string]userusage.Counts{}
			}
			sum := counts[tk.owner][c.pool]
			sum.GPUs += rc.GPUs
			sum.CPUSlots += rc.CPUSlots
			sum.Aux += rc.Aux
			counts[tk.owner][c.pool] = sum
		}
	}

	rp := &resourcePool{syslog: logrus.WithField("component", "test")}
	for name, agents := range pools {
		var got userusage.Counts
		for _, owned := range counts {
			got.GPUs += owned[name].GPUs
			got.CPUSlots += owned[name].CPUSlots
			got.Aux += owned[name].Aux
		}
		numUsedZeroSlots := 0
		for _, a := range agents {
			numUsedZeroSlots += a.numUsedZeroSlots()
		}
		summary := rp.resourceSummaryFromAgentStates(agents)
		require.Equal(t, summary.numActiveAuxContainers, got.Aux, name)
		require.Equal(t, numUsedZeroSlots, got.Aux, name)
		require.Equal(t, summary.numActiveSlots, got.GPUs+got.CPUSlots, name)
	}
	require.Equal(t, userusage.Counts{GPUs: 2, Aux: 1}, counts["a"]["gpu"])
	require.Equal(t, userusage.Counts{GPUs: 3, Aux: 2}, counts["b"]["gpu"])
	require.Equal(t, userusage.Counts{CPUSlots: 4, Aux: 1}, counts["a"]["cpu"])
	require.Equal(t, userusage.Counts{Aux: 1}, counts["b"]["cpu"])
}
