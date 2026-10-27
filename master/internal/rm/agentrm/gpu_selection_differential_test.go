package agentrm

import (
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// gpuSelectionGolden holds what the fit and the reservation chose for requests without gpu_groups
// over random pools, as recorded with the scheduler before gpu_groups existed. Set
// DET_UPDATE_GOLDEN to record it again.
const gpuSelectionGolden = "testdata/gpu_selection_without_groups.golden"

// selectionOutcomes places random requests without gpu_groups on random pools and describes each
// fit and reservation: the agents and slots of the fits, then for a fit on one agent the reserved
// devices with the rule, or the reservation's error. A reservation in map order is random, so only
// its count and reason are described.
func selectionOutcomes(t *testing.T) []string {
	rng := rand.New(rand.NewSource(42))
	fixtures := []topologyFixture{node02, node01, node07, node05, node01Widths, g292(p2pOK), g292(p2pNotOK)}
	var out []string
	for trial := 0; trial < 600; trial++ {
		agents := map[aproto.ID]*agentState{}
		for i := 0; i < 1+rng.Intn(3); i++ {
			f := fixtures[rng.Intn(len(fixtures))]
			state := topologyAgentState(t, f)
			state.id = aproto.ID(fmt.Sprintf("agent-%d", i))
			if rng.Intn(6) == 0 {
				state.gpuTopology = nil
			}
			idle := rng.Intn(2) == 0
			for _, id := range f.ids {
				d := gpuDevice(id)
				if idle {
					continue
				}
				switch rng.Intn(5) {
				case 0:
					cid := cproto.NewID()
					state.Devices[d] = &cid
					state.containerState[cid] = &cproto.Container{ID: cid, Devices: []device.Device{d}}
				case 1:
					state.slotStates[d.ID].enabled.userEnabled = false
					state.updateSlotDeviceView(d.ID)
				}
			}
			agents[state.id] = state
		}
		var xids map[string]bool
		if rng.Intn(3) == 0 {
			xids = map[string]bool{gpuDevice(rng.Intn(8)).UUID: true}
		}
		policy := gpuPolicy{packNUMA: rng.Intn(3) > 0, xids: xids}
		fitting := []SoftConstraint{BestFit, WorstFit}[rng.Intn(2)]
		req := &sproto.AllocateRequest{
			AllocationID: model.AllocationID(fmt.Sprintf("req-%d", trial)),
			SlotsNeeded:  []int{rng.Intn(9), rng.Intn(9), 14 + rng.Intn(3)}[rng.Intn(3)],
			FittingRequirements: sproto.FittingRequirements{
				SingleAgent: rng.Intn(2) == 0,
				GPUTopology: []expconf.GPUTopologyPreference{
					"", expconf.GPUTopologyOff, expconf.GPUTopologySoft, expconf.GPUTopologyStrong,
				}[rng.Intn(4)],
			},
		}
		line := fmt.Sprintf("%d slots=%d single=%v pref=%q pack=%v:", trial, req.SlotsNeeded,
			req.FittingRequirements.SingleAgent, req.FittingRequirements.GPUTopology, policy.packNUMA)
		fits := findFits(req, agents, fitting, rng.Intn(2) == 0, policy.packNUMA)
		for _, fit := range fits {
			line += fmt.Sprintf(" fit %s/%d one-node=%v", fit.Agent.id, fit.Slots, fit.OneNUMANode)
		}
		if len(fits) == 1 && req.SlotsNeeded > 0 {
			res, err := fits[0].Agent.allocateFreeDevices(fits[0].Slots, cproto.NewID(), policy.selection(req, fits))
			switch {
			case err != nil:
				line += " error " + err.Error()
			case res.choice.devices == nil:
				line += fmt.Sprintf(" map order %d (%s) failure %q", len(res.devices),
					res.choice.noSelectionReason, res.failure)
			default:
				line += fmt.Sprintf(" devices %v (%s) worst %q unranked %q", deviceIDs(res.devices),
					res.choice.rule, res.choice.worstPair, res.choice.unranked)
			}
		}
		out = append(out, line)
	}
	return out
}

func TestGPUSelectionWithoutGroupsUnchanged(t *testing.T) {
	got := selectionOutcomes(t)
	if os.Getenv("DET_UPDATE_GOLDEN") != "" {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(gpuSelectionGolden, []byte(strings.Join(got, "\n")+"\n"), 0o600))
	}
	want, err := os.ReadFile(gpuSelectionGolden)
	require.NoError(t, err)
	require.Equal(t, strings.Split(strings.TrimSuffix(string(want), "\n"), "\n"), got)
}
