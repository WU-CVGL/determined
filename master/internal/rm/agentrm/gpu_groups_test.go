package agentrm

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

const (
	rtx4090 = "NVIDIA GeForce RTX 4090"
	ada6000 = "NVIDIA RTX 6000 Ada Generation"
)

// modelDevice is GPU id of a model.
func modelDevice(id int, brand string) device.Device {
	d := gpuDevice(id)
	d.Brand = brand
	return d
}

// modelDevices returns GPUs 0 to len(brands)-1 with these models.
func modelDevices(brands ...string) []device.Device {
	out := make([]device.Device, len(brands))
	for i, b := range brands {
		out[i] = modelDevice(i, b)
	}
	return out
}

func group(name string, count int, models ...string) expconf.GPUGroupRequest {
	return expconf.GPUGroupRequest{Name: name, Count: count, Models: models}
}

// simAndLearner is the config example: one RTX 4090 for simulation, one RTX 6000 Ada for learning.
var simAndLearner = expconf.GPUGroupRequests{group("simulation", 1, rtx4090), group("learner", 1, ada6000)}

// modelAgentState returns an agent whose slots are the devices, with the topology of f (nil: not
// reported since the master started).
func modelAgentState(t *testing.T, devices []device.Device, f *topologyFixture) *agentState {
	t.Helper()
	state, _ := newSlotsAgentState(t, 0)
	for _, d := range devices {
		state.slotStates[d.ID] = &slot{device: d, enabled: slotEnabled{agentEnabled: true, userEnabled: true}}
		state.updateSlotDeviceView(d.ID)
	}
	if f != nil {
		state.setGPUTopology(f.build())
	}
	return state
}

// occupy puts a container on the agent's devices with these IDs.
func occupy(t *testing.T, state *agentState, ids ...int) cproto.ID {
	t.Helper()
	cid := cproto.NewID()
	var devices []device.Device
	for d := range state.Devices {
		if slices.Contains(ids, int(d.ID)) {
			require.Nil(t, state.Devices[d], "device %d", d.ID)
			state.Devices[d] = &cid
			devices = append(devices, d)
		}
	}
	require.Len(t, devices, len(ids))
	state.containerState[cid] = &cproto.Container{ID: cid, Devices: devices}
	return cid
}

func groupsRequest(groups expconf.GPUGroupRequests, pref expconf.GPUTopologyPreference) *sproto.AllocateRequest {
	return &sproto.AllocateRequest{
		AllocationID: "groups", SlotsNeeded: groups.Slots(),
		FittingRequirements: sproto.FittingRequirements{GPUTopology: pref, GPUGroups: groups},
	}
}

func assignmentIDs(a map[string][]device.Device) map[string][]int {
	if a == nil {
		return nil
	}
	out := map[string][]int{}
	for name, devices := range a {
		out[name] = deviceIDs(devices)
	}
	return out
}

func TestGPUGroupAssignment(t *testing.T) {
	// Overlapping model lists: the matching moves a GPU to the group that needs it.
	overlap := expconf.GPUGroupRequests{group("any", 1, rtx4090, ada6000), group("ada", 1, ada6000)}
	require.Equal(t, map[string][]int{"any": {0}, "ada": {1}},
		assignmentIDs(gpuGroupAssignment(overlap, modelDevices(rtx4090, ada6000))))
	require.Equal(t, map[string][]int{"any": {1}, "ada": {0}},
		assignmentIDs(gpuGroupAssignment(overlap, modelDevices(ada6000, rtx4090))))
	require.Equal(t, map[string][]int{"any": {0}, "ada": {1}},
		assignmentIDs(gpuGroupAssignment(overlap, modelDevices(ada6000, ada6000))))
	require.Nil(t, gpuGroupAssignment(overlap, modelDevices(rtx4090, rtx4090)))

	// Groups listed first take the lowest IDs that leave GPUs for the rest.
	same := expconf.GPUGroupRequests{group("a", 2, rtx4090), group("b", 2, rtx4090)}
	require.Equal(t, map[string][]int{"a": {0, 1}, "b": {2, 3}},
		assignmentIDs(gpuGroupAssignment(same, modelDevices(rtx4090, rtx4090, rtx4090, rtx4090))))
	three := expconf.GPUGroupRequests{group("a", 1, rtx4090), group("b", 1, rtx4090), group("c", 1, rtx4090)}
	require.Equal(t, map[string][]int{"a": {0}, "b": {1}, "c": {2}},
		assignmentIDs(gpuGroupAssignment(three, modelDevices(rtx4090, rtx4090, rtx4090))))
	wide := expconf.GPUGroupRequests{group("sim", 2, rtx4090, ada6000), group("learn", 2, rtx4090)}
	require.Equal(t, map[string][]int{"sim": {0, 1}, "learn": {2, 3}},
		assignmentIDs(gpuGroupAssignment(wide, modelDevices(rtx4090, rtx4090, rtx4090, rtx4090))))
	require.Equal(t, map[string][]int{"sim": {1, 3}, "learn": {0, 2}},
		assignmentIDs(gpuGroupAssignment(wide, modelDevices(rtx4090, ada6000, rtx4090, ada6000))))

	// A GPU serves at most one group.
	twice := expconf.GPUGroupRequests{group("a", 1, rtx4090), group("b", 1, rtx4090)}
	require.Nil(t, gpuGroupAssignment(twice, modelDevices(rtx4090)))
	require.Nil(t, gpuGroupAssignment(twice, modelDevices(rtx4090, ada6000)))
	require.False(t, holdsGPUGroups(twice, modelDevices(rtx4090, ada6000)))
	require.True(t, holdsGPUGroups(twice, modelDevices(ada6000, rtx4090, rtx4090)))

	// Exactly the devices: one more or one fewer is no valid set.
	require.Nil(t, gpuGroupAssignment(simAndLearner, modelDevices(rtx4090, ada6000, rtx4090)))
	require.Nil(t, gpuGroupAssignment(simAndLearner, modelDevices(rtx4090)))
	require.Equal(t, map[string][]string{"simulation": {"GPU-0"}, "learner": {"GPU-1"}},
		gpuGroupUUIDs(simAndLearner, modelDevices(rtx4090, ada6000)))

	// Models match exactly; CPU slots never serve a group.
	require.False(t, holdsGPUGroups(expconf.GPUGroupRequests{group("a", 1, "nvidia geforce rtx 4090")},
		modelDevices(rtx4090)))
	cpu := device.Device{ID: 0, Brand: rtx4090, Type: device.CPU}
	require.False(t, holdsGPUGroups(expconf.GPUGroupRequests{group("a", 1, rtx4090)}, []device.Device{cpu}))

	// Devices are listed in ID order within a group.
	pair := expconf.GPUGroupRequests{group("sim", 2, rtx4090)}
	devices := []device.Device{modelDevice(5, rtx4090), modelDevice(2, rtx4090)}
	require.Equal(t, map[string][]int{"sim": {2, 5}}, assignmentIDs(gpuGroupAssignment(pair, devices)))

	require.Equal(t, "simulation: 1 × NVIDIA GeForce RTX 4090; learner: 1 × NVIDIA RTX 6000 Ada Generation",
		simAndLearner.String())
	require.Equal(t, "any: 1 × NVIDIA GeForce RTX 4090 or NVIDIA RTX 6000 Ada Generation; "+
		"ada: 1 × NVIDIA RTX 6000 Ada Generation", overlap.String())
}

// TestGPUGroupAssignmentLowestFirst checks the assignment against every assignment of small sets:
// it is valid and, read GPU by GPU in config order, has the smallest IDs.
func TestGPUGroupAssignmentLowestFirst(t *testing.T) {
	brands := []string{rtx4090, ada6000, "NVIDIA A100"}
	rng := rand.New(rand.NewSource(7))
	for iter := 0; iter < 3000; iter++ {
		var groups expconf.GPUGroupRequests
		n := 0
		for g := 0; g < 1+rng.Intn(3); g++ {
			var models []string
			for _, b := range brands {
				if rng.Intn(2) == 0 {
					models = append(models, b)
				}
			}
			if len(models) == 0 {
				models = []string{brands[rng.Intn(len(brands))]}
			}
			count := 1 + rng.Intn(2)
			n += count
			groups = append(groups, group(fmt.Sprintf("g%d", g), count, models...))
		}
		devices := make([]device.Device, n)
		for i := range devices {
			devices[i] = modelDevice(i, brands[rng.Intn(len(brands))])
		}
		// Slots in config order; the smallest list of IDs, slot by slot, of every assignment.
		var slotGroup []int
		for i, g := range groups {
			for c := 0; c < g.Count; c++ {
				slotGroup = append(slotGroup, i)
			}
		}
		var best []int
		perm := make([]int, n)
		used := make([]bool, n)
		var walk func(slot int)
		walk = func(slot int) {
			if slot == n {
				if best == nil || slices.Compare(perm, best) < 0 {
					best = slices.Clone(perm)
				}
				return
			}
			for d := 0; d < n; d++ {
				if !used[d] && servesGPUGroup(groups[slotGroup[slot]], devices[d]) {
					used[d], perm[slot] = true, d
					walk(slot + 1)
					used[d] = false
				}
			}
		}
		walk(0)
		got := assignmentIDs(gpuGroupAssignment(groups, devices))
		if best == nil {
			require.Nil(t, got, "%v %v", groups, devices)
			continue
		}
		want := map[string][]int{}
		for slot, d := range best {
			want[groups[slotGroup[slot]].Name] = append(want[groups[slotGroup[slot]].Name], d)
		}
		require.Equal(t, want, got, "%v %v", groups, devices)
	}
}

// gpuGroupsOracle says whether exactly the devices are a valid set, by Hall's condition on their
// models: every subset of groups needs at most as many devices as serve one of its groups.
func gpuGroupsOracle(groups expconf.GPUGroupRequests, devices []device.Device) bool {
	if len(devices) != groups.Slots() {
		return false
	}
	for mask := 1; mask < 1<<len(groups); mask++ {
		need, serve := 0, 0
		for i, g := range groups {
			if mask&(1<<i) != 0 {
				need += g.Count
			}
		}
		for _, d := range devices {
			for i, g := range groups {
				if mask&(1<<i) != 0 && servesGPUGroup(g, d) {
					serve++
					break
				}
			}
		}
		if need > serve {
			return false
		}
	}
	return true
}

// oracleValidSets returns every valid set of the free devices, sorted by ID, in lexicographic
// order of IDs; under strong, only sets on one known NUMA node.
func oracleValidSets(groups expconf.GPUGroupRequests, in gpuSelectionInput, strong bool) [][]device.Device {
	var out [][]device.Device
	n := groups.Slots()
	forEachSubset(len(in.free), n, func(idx []int) {
		set := make([]device.Device, 0, n)
		for _, i := range idx {
			set = append(set, in.free[i])
		}
		if !gpuGroupsOracle(groups, set) {
			return
		}
		if strong {
			node := numaNodeOf(in.topology, set[0])
			for _, d := range set {
				if n := numaNodeOf(in.topology, d); n < 0 || n != node {
					return
				}
			}
		}
		out = append(out, set)
	})
	return out
}

// randomGPUGroups returns 1 to 3 groups of 1 to 3 GPUs over the models, sometimes naming a model
// that no GPU has.
func randomGPUGroups(rng *rand.Rand, models []string) expconf.GPUGroupRequests {
	var groups expconf.GPUGroupRequests
	for i := 0; i < 1+rng.Intn(3); i++ {
		var ms []string
		for _, m := range append(models, "absent") {
			if rng.Intn(2) == 0 {
				ms = append(ms, m)
			}
		}
		if len(ms) == 0 {
			ms = []string{models[rng.Intn(len(models))]}
		}
		groups = append(groups, group(fmt.Sprintf("g%d", i), 1+rng.Intn(3), ms...))
	}
	return groups
}

// randomGroupsAgent returns an agent of 1 to 8 GPUs of 1 to 3 models with some GPUs busy,
// disabled or draining, an unknown NUMA node on some, no topology at all on some agents, and
// sometimes the whole agent disabled or draining.
func randomGroupsAgent(t *testing.T, rng *rand.Rand, models []string) *agentState {
	gpus := 1 + rng.Intn(8)
	brands := make([]string, gpus)
	for i := range brands {
		brands[i] = models[rng.Intn(len(models))]
	}
	f := topologyFixture{ids: intRange(0, gpus), numa: map[int]int{}, p2p: allP2P(p2pOK)}
	for _, id := range f.ids {
		if rng.Intn(5) > 0 {
			f.numa[id] = rng.Intn(2)
		}
		if rng.Intn(6) == 0 {
			f.nvmlError = map[int]bool{id: true}
		}
	}
	var topology *topologyFixture
	if rng.Intn(5) > 0 {
		topology = &f
	}
	state := modelAgentState(t, modelDevices(brands...), topology)
	for _, id := range f.ids {
		switch rng.Intn(6) {
		case 0:
			occupy(t, state, id)
		case 1:
			state.slotStates[device.ID(id)].enabled.userEnabled = false
			state.updateSlotDeviceView(device.ID(id))
		case 2:
			if rng.Intn(2) == 0 {
				occupy(t, state, id)
			}
			state.slotStates[device.ID(id)].enabled.draining = true
			state.updateSlotDeviceView(device.ID(id))
		}
	}
	switch rng.Intn(12) {
	case 0:
		state.enabled = false
	case 1:
		state.enabled, state.draining = false, true
	}
	return state
}

func TestGPUGroupsEqualsOracle(t *testing.T) {
	// Over random agents, the fit holds exactly when the oracle has a valid set of the allocatable
	// free GPUs (on one known NUMA node under "strong"), the reservation takes a valid set with the
	// assignment it records, the lowest-ID valid set without a rule, and a failed reservation
	// changes nothing.
	rng := rand.New(rand.NewSource(7)) //nolint:gosec
	allModels := []string{rtx4090, ada6000, "NVIDIA A100-SXM4-80GB"}
	fitted := map[string]int{}
	for trial := 0; trial < 3000; trial++ {
		models := allModels[:1+rng.Intn(3)]
		state := randomGroupsAgent(t, rng, models)
		groups := randomGPUGroups(rng, models)
		pref := []expconf.GPUTopologyPreference{
			expconf.GPUTopologyOff, expconf.GPUTopologySoft, expconf.GPUTopologyStrong,
		}[rng.Intn(3)]
		packNUMA := rng.Intn(2) == 0
		req := groupsRequest(groups, pref)
		strong := strongTopology(req)
		desc := fmt.Sprintf("trial %d: groups %s, %s, packing %v", trial, groups, pref, packNUMA)

		in := state.gpuSelectionInput()
		valid := oracleValidSets(groups, in, strong)
		fits := findFits(req, map[aproto.ID]*agentState{state.id: state}, BestFit, false, packNUMA)
		// A disabled or draining agent takes no new work; the reservation, which runs only after
		// a fit, reads only the devices.
		require.Equal(t, state.enabled && len(valid) > 0, len(fits) > 0, desc)

		policy := gpuPolicy{packNUMA: packNUMA}
		sel := policy.selection(req, []*fittingState{{Agent: state}})
		before := snapshotOf(state)
		res, err := state.allocateFreeDevices(req.SlotsNeeded, cproto.NewID(), sel)
		if len(valid) == 0 {
			require.Error(t, err, desc)
			require.Equal(t, before, snapshotOf(state), desc)
			fitted["none"]++
			continue
		}
		require.NoError(t, err, desc)
		require.True(t, gpuGroupsOracle(groups, res.devices), desc)
		require.Contains(t, valid, res.devices, desc)
		if strong {
			require.NoError(t, state.checkOneNUMANode(res.devices), desc)
		}
		assignment := gpuGroupAssignment(groups, res.devices)
		require.NotNil(t, assignment, desc)
		require.Equal(t, gpuGroupUUIDs(groups, res.devices), res.choice.gpuGroups, desc)
		for _, g := range groups {
			require.Len(t, assignment[g.Name], g.Count, desc)
			for _, d := range assignment[g.Name] {
				require.True(t, servesGPUGroup(g, d), desc)
			}
		}
		if !strong && !packNUMA && pref == expconf.GPUTopologyOff {
			require.Equal(t, valid[0], res.devices, desc)
		}
		if !strong {
			// The walk by ID that serves above the set cap finds the lowest-ID valid set.
			lowest := lowestValidGPUGroupSet(groups, rankGPUs(gpuGroupCandidates(groups, in.free), nil, nil),
				req.SlotsNeeded)
			require.Equal(t, valid[0], sortedDevices(lowest), desc)
		}
		fitted[string(pref)]++
	}
	for _, k := range []string{"none", "off", "soft", "strong"} {
		require.Greater(t, fitted[k], 100, "%s: %v", k, fitted)
	}
}

func TestGPUGroupsLowestValidSetAboveTheCap(t *testing.T) {
	// Above maxTopologySets sets (C(20,10) of 20 free GPUs), the set is the lowest-ID valid set,
	// found without enumerating sets.
	var brands []string
	for i := 0; i < 20; i++ {
		brands = append(brands, []string{rtx4090, ada6000}[i%3/2])
	}
	groups := expconf.GPUGroupRequests{group("sim", 3, rtx4090), group("learner", 7, ada6000, rtx4090)}
	in := gpuSelectionInput{free: modelDevices(brands...)}
	in.allocatable = in.free
	require.Greater(t, binomial(20, 10), maxTopologySets)
	for _, sel := range []deviceSelection{{groups: groups}, {groups: groups, packNUMA: true}, {
		groups: groups, preferTopology: true,
	}} {
		c := selectFreeDevices(in, 10, sel)
		require.Equal(t, intRange(0, 10), deviceIDs(c.devices))
		require.Contains(t, c.rule, "lowest IDs: more than 20000 sets of free GPUs")
	}
	// The Ada are GPUs 2, 5, 8, 11, 14 and 17: eight learners on them have no valid set, and six
	// take every Ada with the two lowest RTX 4090s.
	groups = expconf.GPUGroupRequests{group("sim", 2, rtx4090), group("learner", 8, ada6000)}
	c := selectFreeDevices(in, 10, deviceSelection{groups: groups})
	require.Nil(t, c.devices)
	groups = expconf.GPUGroupRequests{group("sim", 2, rtx4090), group("learner", 6, ada6000)}
	got := lowestValidGPUGroupSet(groups, rankGPUs(in.free, nil, nil), 8)
	require.Equal(t, []int{0, 1, 2, 5, 8, 11, 14, 17}, deviceIDs(gpuDevices(got)))
}

func TestGPUGroupsExpectedChoices(t *testing.T) {
	// node02's layout: RTX 4090s on NUMA node 0 (GPUs 0-3) and RTX 6000 Ada on node 1 (4-7).
	brands := []string{rtx4090, rtx4090, rtx4090, rtx4090, ada6000, ada6000, ada6000, ada6000}
	split := modelAgentState(t, modelDevices(brands...), &node02)
	occupy(t, split, 0)
	for _, packNUMA := range []bool{true, false} {
		for _, pref := range []expconf.GPUTopologyPreference{expconf.GPUTopologyOff, expconf.GPUTopologySoft} {
			req := groupsRequest(simAndLearner, pref)
			fits := findFits(req, map[aproto.ID]*agentState{split.id: split}, BestFit, false, packNUMA)
			require.Len(t, fits, 1)
			c := selectFreeDevices(split.gpuSelectionInput(), 2, gpuPolicy{packNUMA: packNUMA}.selection(req, fits))
			require.Equal(t, []int{1, 4}, deviceIDs(c.devices), "%s, packing %v", pref, packNUMA)
		}
	}
	// No NUMA node has both models: "strong" never fits.
	strong := groupsRequest(simAndLearner, expconf.GPUTopologyStrong)
	require.Empty(t, findFits(strong, map[aproto.ID]*agentState{split.id: split}, BestFit, false, true))
	require.ErrorContains(t, gpuGroupsCannotFit("gpus", map[aproto.ID]*agentState{split.id: split},
		simAndLearner, true), "no agent in pool gpus has GPUs for gpu_groups simulation: 1 × "+rtx4090)
	require.NoError(t, gpuGroupsCannotFit("gpus", map[aproto.ID]*agentState{split.id: split},
		simAndLearner, false))

	// g292: one NUMA node, PIX pairs {0,1}, {2,3}, {4,5}, {6,7}, RTX 4090 on even IDs and Ada on odd
	// ones, GPU 1 busy. Packing and the lowest IDs take 0 and 3; "soft" takes the PIX pair 2 and 3,
	// and so does "strong", on the one node.
	alternating := make([]string, 8)
	for i := range alternating {
		alternating[i] = []string{rtx4090, ada6000}[i%2]
	}
	f := g292(p2pOK)
	pix := modelAgentState(t, modelDevices(alternating...), &f)
	occupy(t, pix, 1)
	for _, c := range []struct {
		pref     expconf.GPUTopologyPreference
		packNUMA bool
		want     []int
		rule     string
	}{
		{expconf.GPUTopologyOff, false, []int{0, 3}, "gpu_groups simulation 0; learner 3; lowest IDs"},
		{expconf.GPUTopologyOff, true, []int{0, 3}, "gpu_groups simulation 0; learner 3; NUMA packing; " +
			"free per NUMA node 0:7"},
		{expconf.GPUTopologySoft, false, []int{2, 3}, "gpu_groups simulation 2; learner 3; " +
			"GPU topology preference; worst pair PIX, P2P usable"},
		{expconf.GPUTopologyStrong, false, []int{2, 3}, "gpu_groups simulation 2; learner 3; " +
			"GPU topology preference strong; NUMA node 0; worst pair PIX, P2P usable"},
	} {
		req := groupsRequest(simAndLearner, c.pref)
		fits := findFits(req, map[aproto.ID]*agentState{pix.id: pix}, BestFit, false, c.packNUMA)
		require.Len(t, fits, 1)
		got := selectFreeDevices(pix.gpuSelectionInput(), 2, gpuPolicy{packNUMA: c.packNUMA}.selection(req, fits))
		require.Equal(t, c.want, deviceIDs(got.devices), "%s, packing %v", c.pref, c.packNUMA)
		require.Equal(t, c.rule, got.rule)
	}
}

func TestGPUGroupsNeverTakeMapOrder(t *testing.T) {
	// A selection that panics, chooses nothing, or chooses a set that is not valid makes the
	// reservation fail and change nothing; it never takes map order.
	state := modelAgentState(t, modelDevices(rtx4090, ada6000, rtx4090, ada6000), nil)
	occupy(t, state, 3)
	sel := deviceSelection{groups: simAndLearner, packNUMA: true}
	for name, selector := range map[string]func(gpuSelectionInput, int, deviceSelection) gpuChoice{
		"panic": func(gpuSelectionInput, int, deviceSelection) gpuChoice { panic("selector") },
		"none":  func(gpuSelectionInput, int, deviceSelection) gpuChoice { return gpuChoice{} },
		"reason": func(gpuSelectionInput, int, deviceSelection) gpuChoice {
			return gpuChoice{noSelectionReason: "stub"}
		},
		"two of one model": func(gpuSelectionInput, int, deviceSelection) gpuChoice {
			return gpuChoice{devices: []device.Device{modelDevice(0, rtx4090), modelDevice(2, rtx4090)}}
		},
		"busy": func(gpuSelectionInput, int, deviceSelection) gpuChoice {
			return gpuChoice{devices: []device.Device{modelDevice(0, rtx4090), modelDevice(3, ada6000)}}
		},
		"twice": func(gpuSelectionInput, int, deviceSelection) gpuChoice {
			return gpuChoice{devices: []device.Device{modelDevice(1, ada6000), modelDevice(1, ada6000)}}
		},
		"not the agent's": func(gpuSelectionInput, int, deviceSelection) gpuChoice {
			return gpuChoice{devices: []device.Device{modelDevice(0, rtx4090), modelDevice(9, ada6000)}}
		},
	} {
		before := snapshotOf(state)
		res, err := state.chooseFreeDevices(2, sel, selector)
		require.Error(t, err, name)
		require.Contains(t, err.Error(), "gpu_groups", name)
		require.Nil(t, res.devices, name)
		require.Equal(t, before, snapshotOf(state), name)
	}
	// The real selection takes the valid set.
	res, err := state.allocateFreeDevices(2, cproto.NewID(), sel)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, deviceIDs(res.devices))
	require.Equal(t, map[string][]string{"simulation": {"GPU-0"}, "learner": {"GPU-1"}}, res.choice.gpuGroups)

	// Under "strong", a valid set on two NUMA nodes fails too.
	brands := []string{rtx4090, rtx4090, rtx4090, rtx4090, ada6000, ada6000, ada6000, ada6000}
	split := modelAgentState(t, modelDevices(brands...), &node02)
	before := snapshotOf(split)
	res, err = split.chooseFreeDevices(2, deviceSelection{groups: simAndLearner, strong: true},
		func(gpuSelectionInput, int, deviceSelection) gpuChoice {
			return gpuChoice{devices: []device.Device{modelDevice(1, rtx4090), modelDevice(4, ada6000)}}
		})
	require.ErrorContains(t, err, "gpu_groups")
	require.Nil(t, res.devices)
	require.Equal(t, before, snapshotOf(split))

	// The simulation counts a failed reservation as a miss; it does not panic.
	full :=modelAgentState(t, modelDevices(rtx4090, rtx4090), nil)
	req := groupsRequest(simAndLearner, expconf.GPUTopologyOff)
	require.False(t, priorityScheduler{}.addTaskToAgents(req, []*fittingState{{Agent: full, Slots: 2}}))
}

func TestGPUGroupsUseOneAgent(t *testing.T) {
	// Two agents each with one model: no multi-agent fit, whatever is_single_node says.
	a := modelAgentState(t, modelDevices(rtx4090, rtx4090), nil)
	a.id = "a"
	b := modelAgentState(t, modelDevices(ada6000, ada6000), nil)
	b.id = "b"
	agents := map[aproto.ID]*agentState{"a": a, "b": b}
	req := groupsRequest(expconf.GPUGroupRequests{group("sim", 2, rtx4090), group("learner", 2, ada6000)},
		expconf.GPUTopologyOff)
	require.Empty(t, findFits(req, agents, BestFit, true, true))
	plain := &sproto.AllocateRequest{AllocationID: "plain", SlotsNeeded: 4}
	require.Len(t, findFits(plain, agents, BestFit, true, true), 2)
	// The agent that holds the groups is the one fit.
	c := modelAgentState(t, modelDevices(ada6000, rtx4090, ada6000, rtx4090, rtx4090), nil)
	c.id = "c"
	agents["c"] = c
	fits := findFits(req, agents, BestFit, true, true)
	require.Len(t, fits, 1)
	require.Equal(t, aproto.ID("c"), fits[0].Agent.id)
}

func TestGPUGroupsSoftAgentChoiceCountsValidSets(t *testing.T) {
	// Under NUMA packing, "soft" puts the agents where one NUMA node holds a valid set first. Agent
	// a has both models free on NUMA node 0 and 6 free GPUs; agent b, the fuller one with 5, has
	// only RTX 4090s free on node 0 and only an Ada free on node 1.
	both := []string{rtx4090, ada6000, rtx4090, rtx4090, rtx4090, rtx4090, rtx4090, rtx4090}
	a := modelAgentState(t, modelDevices(both...), &node02)
	a.id = "a"
	occupy(t, a, 2, 3)
	only := []string{rtx4090, rtx4090, rtx4090, rtx4090, ada6000, ada6000, ada6000, ada6000}
	b := modelAgentState(t, modelDevices(only...), &node02)
	b.id = "b"
	occupy(t, b, 5, 6, 7)
	agents := map[aproto.ID]*agentState{"a": a, "b": b}
	soft := findFits(groupsRequest(simAndLearner, expconf.GPUTopologySoft), agents, BestFit, false, true)
	require.Len(t, soft, 1)
	require.Equal(t, aproto.ID("a"), soft[0].Agent.id)
	require.True(t, soft[0].OneNUMANode)
	off := findFits(groupsRequest(simAndLearner, expconf.GPUTopologyOff), agents, BestFit, false, true)
	require.Equal(t, aproto.ID("b"), off[0].Agent.id)
	require.False(t, off[0].OneNUMANode)
}

// groupsPool is a strongPool on agents built by the test, with requests that carry gpu_groups.
func groupsPool(t *testing.T, policy gpuPolicy, agents map[aproto.ID]*agentState, tasks ...strongTask) *strongPool {
	t.Helper()
	s := newStrongPool(t, false, policy, nil, tasks...)
	for id, a := range agents {
		a.id = id
		s.live[id] = a
	}
	return s
}

// addGroups adds a pending task with gpu_groups and subscribes to its events.
func (s *strongPool) addGroups(
	t *testing.T, id string, groups expconf.GPUGroupRequests, pref expconf.GPUTopologyPreference,
) *sproto.ResourcesSubscription {
	t.Helper()
	sub := s.addStrong(t, id, groups.Slots())
	s.rp.mu.Lock()
	defer s.rp.mu.Unlock()
	req, ok := s.rp.taskList.TaskByID(model.AllocationID(id))
	require.True(t, ok)
	req.FittingRequirements.GPUTopology = pref
	req.FittingRequirements.GPUGroups = groups
	return sub
}

func TestGPUGroupsWaitAllOrNothing(t *testing.T) {
	// Every RTX 6000 Ada is busy: the task gets nothing, not even its RTX 4090, and the task log
	// says once that it waits. When an Ada is free, it gets both GPUs in one pass.
	brands := []string{rtx4090, rtx4090, ada6000, ada6000}
	state := modelAgentState(t, modelDevices(brands...), nil)
	s := groupsPool(t, gpuPolicy{packNUMA: true}, map[aproto.ID]*agentState{"a": state})
	busy := occupy(t, state, 2, 3)
	sub := s.addGroups(t, "groups", simAndLearner, expconf.GPUTopologyOff)
	before := snapshotOf(state)
	for pass := 0; pass < 3; pass++ {
		allocated, _, again := s.pass(t)
		require.Empty(t, allocated)
		require.False(t, again)
		require.Equal(t, before, snapshotOf(state))
	}
	event := awaitEvent(t, sub, time.Second)
	require.IsType(t, &sproto.ContainerLog{}, event)
	require.Equal(t, "gpu_groups: waiting until one agent in pool gpus has free GPUs for "+
		"simulation: 1 × NVIDIA GeForce RTX 4090; learner: 1 × NVIDIA RTX 6000 Ada Generation",
		event.(*sproto.ContainerLog).Message())
	require.Nil(t, awaitEvent(t, sub, 100*time.Millisecond), "one waiting line")

	state.deallocateContainer(busy)
	allocated, _, _ := s.pass(t)
	require.Equal(t, map[string][]int{"groups": {0, 2}}, allocated)
}

func TestGPUGroupsImpossibleFailsOnce(t *testing.T) {
	// No agent of the pool has the model, counting disabled and busy slots: the task fails once.
	// A disabled Ada counts: the task with it waits instead.
	state := modelAgentState(t, modelDevices(rtx4090, rtx4090, ada6000), nil)
	state.slotStates[2].enabled.userEnabled = false
	state.updateSlotDeviceView(2)
	s := groupsPool(t, gpuPolicy{packNUMA: true}, map[aproto.ID]*agentState{"a": state})
	impossible := expconf.GPUGroupRequests{group("sim", 1, rtx4090), group("learner", 1, "NVIDIA H100")}
	sub := s.addGroups(t, "impossible", impossible, expconf.GPUTopologyOff)
	waits := s.addGroups(t, "waits", simAndLearner, expconf.GPUTopologyOff)
	for pass := 0; pass < 3; pass++ {
		allocated, _, _ := s.pass(t)
		require.Empty(t, allocated)
	}
	event := awaitEvent(t, sub, time.Second)
	require.IsType(t, &sproto.InvalidResourcesRequestError{}, event)
	cause := event.(*sproto.InvalidResourcesRequestError).Cause
	require.True(t, sproto.IsUnrecoverableSystemError(cause), "%T", cause)
	require.EqualError(t, cause, "invalid resources request: no agent in pool gpus has GPUs for gpu_groups "+
		"sim: 1 × NVIDIA GeForce RTX 4090; learner: 1 × NVIDIA H100")
	require.Nil(t, awaitEvent(t, sub, 100*time.Millisecond), "fails once")
	require.IsType(t, &sproto.ContainerLog{}, awaitEvent(t, waits, time.Second))

	// A restored agent has not reported its topology: its models decide; with "strong", a task
	// that they admit waits for the topology.
	restored := modelAgentState(t, modelDevices(rtx4090, ada6000), nil)
	agents := map[aproto.ID]*agentState{"r": restored}
	require.NoError(t, gpuGroupsCannotFit("gpus", agents, simAndLearner, true))
	require.NoError(t, gpuGroupsCannotFit("gpus", agents, simAndLearner, false))
	require.Error(t, gpuGroupsCannotFit("gpus", agents, impossible, false))
	require.Error(t, gpuGroupsCannotFit("gpus", agents, impossible, true))
	require.NoError(t, gpuGroupsCannotFit("gpus", map[aproto.ID]*agentState{}, impossible, false))
}

func TestGPUGroupsPreferenceLine(t *testing.T) {
	// Unranked, a task with gpu_groups gets its set by the rule its debug line names.
	fit := &fittingState{Agent: &agentState{id: "node02"}}
	require.Equal(t, "GPU topology preference: agent node02 not ranked (more than 20000 sets of free GPUs)",
		gpuTopologyPreferenceLine([]gpuReservation{{fit: fit, resp: allocateFreeDevicesResponse{
			devices: gpuDeviceList(0, 1),
			choice: gpuChoice{
				unranked:  "more than 20000 sets of free GPUs",
				gpuGroups: map[string][]string{"simulation": {"GPU-0"}, "learner": {"GPU-1"}},
			},
		}}}))
}

func TestGPUGroupsCountMismatchFailsOnce(t *testing.T) {
	// A request whose groups do not add up to its slots never fits; it fails once with the count.
	state := modelAgentState(t, modelDevices(rtx4090, ada6000, rtx4090), nil)
	s := groupsPool(t, gpuPolicy{packNUMA: true}, map[aproto.ID]*agentState{"a": state})
	sub := s.addGroups(t, "mismatch", simAndLearner, expconf.GPUTopologyOff)
	s.rp.mu.Lock()
	req, ok := s.rp.taskList.TaskByID("mismatch")
	require.True(t, ok)
	req.SlotsNeeded = 3
	require.Empty(t, findFits(req, s.live, BestFit, false, true))
	s.rp.mu.Unlock()
	before := snapshotOf(state)
	for pass := 0; pass < 3; pass++ {
		allocated, _, again := s.pass(t)
		require.Empty(t, allocated)
		require.False(t, again)
	}
	require.Equal(t, before, snapshotOf(state))
	event := awaitEvent(t, sub, time.Second)
	require.IsType(t, &sproto.InvalidResourcesRequestError{}, event)
	require.EqualError(t, event.(*sproto.InvalidResourcesRequestError).Cause,
		"invalid resources request: gpu_groups have 2 GPUs for 3 slots")
	require.Nil(t, awaitEvent(t, sub, 100*time.Millisecond), "fails once")
}

func TestGPUGroupsPlanMatchesTheReservations(t *testing.T) {
	// The simulation and the live reservations choose the same GPUs for requests with gpu_groups
	// and plain ones in one pass.
	brands := []string{rtx4090, ada6000, rtx4090, ada6000, rtx4090, ada6000, rtx4090, ada6000}
	state := modelAgentState(t, modelDevices(brands...), &node02)
	s := groupsPool(t, gpuPolicy{packNUMA: true}, map[aproto.ID]*agentState{"a": state},
		strongTask{id: "plain", slots: 1, priority: 42})
	s.addGroups(t, "first", simAndLearner, expconf.GPUTopologyStrong)
	s.addGroups(t, "second", expconf.GPUGroupRequests{group("sim", 2, rtx4090), group("learner", 1, ada6000)},
		expconf.GPUTopologySoft)

	s.rp.mu.Lock()
	s.rp.agentStatesCache = s.live
	s.rp.gpuPolicy = s.policy
	toAllocate, _ := s.rp.scheduler.Schedule(s.rp)
	s.rp.mu.Unlock()
	require.Len(t, toAllocate, 3)
	copies := deepCopyAgents(s.live)
	simulation := priorityScheduler{gpus: s.policy}
	for _, req := range toAllocate {
		fits := findFits(req, s.live, s.rp.fittingMethod, false, true)
		copyFits := findFits(req, copies, s.rp.fittingMethod, false, true)
		before := freeDeviceIDs(copyFits[0].Agent)
		require.True(t, simulation.addTaskToAgents(req, copyFits))
		planned := idsMinus(before, freeDeviceIDs(copyFits[0].Agent))
		res, err := fits[0].Agent.allocateFreeDevices(fits[0].Slots, cproto.NewID(), s.policy.selection(req, fits))
		require.NoError(t, err)
		require.Equal(t, planned, deviceIDs(res.devices), "request %s", req.AllocationID)
	}
}
