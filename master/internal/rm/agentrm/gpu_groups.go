package agentrm

import (
	"fmt"
	"slices"
	"strings"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// resources.gpu_groups: a task asks for named groups of GPUs by exact model, all on one agent. A
// GPU serves at most one group, and a group takes a GPU whose model (device.Device.Brand) is one of
// its models. A set of free GPUs is valid when an assignment of exactly its GPUs to the groups
// exists (matchGPUGroups). The fit (gpuGroupsSatisfied) admits an agent exactly when its free GPUs,
// under prefer_gpu_topology "strong" those of one NUMA node, hold a valid set, and the selection
// (selectGPUGroups) then chooses among the valid sets only, never in map order.

// gpuGroupsOf returns the request's groups, or nil when it has none.
func gpuGroupsOf(req *sproto.AllocateRequest) expconf.GPUGroupRequests {
	if len(req.FittingRequirements.GPUGroups) == 0 {
		return nil
	}
	return req.FittingRequirements.GPUGroups
}

// servesGPUGroup reports whether a group can take a device: a GPU whose model is one of the
// group's.
func servesGPUGroup(group expconf.GPUGroupRequest, d device.Device) bool {
	return (d.Type == device.CUDA || d.Type == device.ROCM) && slices.Contains(group.Models, d.Brand)
}

// gpuGroupCandidates returns the devices that some group can take, in the given order.
func gpuGroupCandidates(groups expconf.GPUGroupRequests, devices []device.Device) []device.Device {
	var out []device.Device
	for _, d := range devices {
		for _, group := range groups {
			if servesGPUGroup(group, d) {
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// matchGPUGroups assigns a device to every GPU of the groups (a slot: group by group, in config
// order, each group's slots one after another), using each device at most once and each of the
// first forced devices exactly once. It returns the index in devices of each slot's device, or nil
// when no such assignment exists. With forced equal to the number of slots and as many devices, it
// tells whether exactly these devices are a valid set.
//
// It is Kuhn's augmenting paths: the forced devices are matched first, and later augmenting paths
// keep every matched device matched, so the result covers the forced devices whenever an
// assignment does (Mendelsohn–Dulmage). Slots and devices are tried in order, so the result is
// deterministic.
func matchGPUGroups(groups expconf.GPUGroupRequests, devices []device.Device, forced int) []int {
	var slotGroup []int
	for i, group := range groups {
		for c := 0; c < group.Count; c++ {
			slotGroup = append(slotGroup, i)
		}
	}
	n := len(slotGroup)
	if n == 0 || forced > n || len(devices) < n {
		return nil
	}
	serves := func(slot, dev int) bool { return servesGPUGroup(groups[slotGroup[slot]], devices[dev]) }
	slotDev := make([]int, n)
	for i := range slotDev {
		slotDev[i] = -1
	}
	devSlot := make([]int, len(devices))
	for i := range devSlot {
		devSlot[i] = -1
	}
	var seen []bool
	// augmentFromSlot finds an augmenting path from an unmatched slot.
	var augmentFromSlot func(slot int) bool
	augmentFromSlot = func(slot int) bool {
		for dev := range devices {
			if seen[dev] || !serves(slot, dev) {
				continue
			}
			seen[dev] = true
			if devSlot[dev] < 0 || augmentFromSlot(devSlot[dev]) {
				slotDev[slot], devSlot[dev] = dev, slot
				return true
			}
		}
		return false
	}
	// augmentFromDevice finds an augmenting path from an unmatched device; seenSlot marks the slots
	// on the path.
	var seenSlot []bool
	var augmentFromDevice func(dev int) bool
	augmentFromDevice = func(dev int) bool {
		for slot := 0; slot < n; slot++ {
			if seenSlot[slot] || !serves(slot, dev) {
				continue
			}
			seenSlot[slot] = true
			if slotDev[slot] < 0 || augmentFromDevice(slotDev[slot]) {
				slotDev[slot], devSlot[dev] = dev, slot
				return true
			}
		}
		return false
	}
	for dev := 0; dev < forced; dev++ {
		seenSlot = make([]bool, n)
		if !augmentFromDevice(dev) {
			return nil
		}
	}
	for slot := 0; slot < n; slot++ {
		if slotDev[slot] >= 0 {
			continue
		}
		seen = make([]bool, len(devices))
		if !augmentFromSlot(slot) {
			return nil
		}
	}
	return slotDev
}

// gpuGroupAssignment returns the devices of each group for exactly the devices of a valid set,
// each group's devices sorted by ID, or nil when the set is not valid. It is the one assignment of
// a set: the reservation records it, and a restore computes it again from the devices.
func gpuGroupAssignment(groups expconf.GPUGroupRequests, devices []device.Device) map[string][]device.Device {
	sorted := slices.Clone(devices)
	slices.SortFunc(sorted, func(a, b device.Device) int { return cmpInt(int(a.ID), int(b.ID)) })
	slotDev := matchGPUGroups(groups, sorted, len(sorted))
	if slotDev == nil {
		return nil
	}
	out := map[string][]device.Device{}
	slot := 0
	for _, group := range groups {
		for c := 0; c < group.Count; c++ {
			out[group.Name] = append(out[group.Name], sorted[slotDev[slot]])
			slot++
		}
		slices.SortFunc(out[group.Name], func(a, b device.Device) int { return cmpInt(int(a.ID), int(b.ID)) })
	}
	return out
}

// gpuGroupUUIDs returns the UUIDs of each group's devices for the container (DET_GPU_GROUPS), or
// nil when the set is not valid.
func gpuGroupUUIDs(groups expconf.GPUGroupRequests, devices []device.Device) map[string][]string {
	assignment := gpuGroupAssignment(groups, devices)
	if assignment == nil {
		return nil
	}
	out := make(map[string][]string, len(assignment))
	for name, devs := range assignment {
		for _, d := range devs {
			out[name] = append(out[name], d.UUID)
		}
	}
	return out
}

// describeGPUGroupAssignment lists each group's device IDs in config order, for example
// "simulation 3; learner 4,5".
func describeGPUGroupAssignment(groups expconf.GPUGroupRequests, assignment map[string][]device.Device) string {
	parts := make([]string, 0, len(groups))
	for _, group := range groups {
		parts = append(parts, group.Name+" "+idList(assignment[group.Name]))
	}
	return strings.Join(parts, "; ")
}

// holdsGPUGroups reports whether the devices hold a valid set.
func holdsGPUGroups(groups expconf.GPUGroupRequests, devices []device.Device) bool {
	return matchGPUGroups(groups, gpuGroupCandidates(groups, devices), 0) != nil
}

// holdsGPUGroupsOnOneNUMANode reports whether the free devices of one known NUMA node hold a valid
// set, counted as prefer_gpu_topology "strong" counts free GPUs (freeByNUMA).
func holdsGPUGroupsOnOneNUMANode(groups expconf.GPUGroupRequests, in gpuSelectionInput) bool {
	for _, devices := range freeByNUMA(in) {
		if holdsGPUGroups(groups, devices) {
			return true
		}
	}
	return false
}

// gpuGroupsSatisfied admits an agent for a request with gpu_groups when its free devices hold a
// valid set, on one known NUMA node under prefer_gpu_topology "strong". It admits every agent for
// other requests. The free devices are exactly the allocatable free slots (gpuSelectionInput).
func gpuGroupsSatisfied(req *sproto.AllocateRequest, agent *agentState) bool {
	groups := gpuGroupsOf(req)
	if groups == nil {
		return true
	}
	in := agent.gpuSelectionInput()
	if strongTopology(req) {
		return holdsGPUGroupsOnOneNUMANode(groups, in)
	}
	return holdsGPUGroups(groups, in.free)
}

// lowestValidGPUGroupSet returns the valid set of n devices with the smallest sorted list of IDs,
// without enumerating sets: it walks the candidates by ID and keeps each one that a valid set with
// the ones kept so far can still take. It returns nil when the candidates hold no valid set.
func lowestValidGPUGroupSet(groups expconf.GPUGroupRequests, candidates []rankedGPU, n int) []rankedGPU {
	devices := gpuDevices(candidates)
	var kept []int
	for i := range candidates {
		if len(kept) == n {
			break
		}
		try := make([]device.Device, 0, len(kept)+1+len(devices)-i-1)
		for _, k := range kept {
			try = append(try, devices[k])
		}
		try = append(try, devices[i])
		try = append(try, devices[i+1:]...)
		if matchGPUGroups(groups, try, len(kept)+1) != nil {
			kept = append(kept, i)
		}
	}
	if len(kept) != n {
		return nil
	}
	out := make([]rankedGPU, 0, n)
	for _, k := range kept {
		out = append(out, candidates[k])
	}
	return out
}

// forEachValidGPUGroupSet calls fn with every valid set of n candidates, in lexicographic order of
// the candidates' positions, reusing its slice.
func forEachValidGPUGroupSet(
	groups expconf.GPUGroupRequests, candidates []rankedGPU, n int, fn func([]rankedGPU),
) {
	f := len(candidates)
	if n <= 0 || f < n {
		return
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	set := make([]rankedGPU, n)
	devices := make([]device.Device, n)
	for {
		for x, i := range idx {
			set[x] = candidates[i]
			devices[x] = candidates[i].device
		}
		if matchGPUGroups(groups, devices, n) != nil {
			fn(set)
		}
		i := n - 1
		for i >= 0 && idx[i] == f-n+i {
			i--
		}
		if i < 0 {
			return
		}
		idx[i]++
		for j := i + 1; j < n; j++ {
			idx[j] = idx[j-1] + 1
		}
	}
}

// topologySetKey is prefer_gpu_topology's key of a set (selectByTopology): its GPUs in error, then
// its pair ranks sorted worst first. Smaller is better.
type topologySetKey struct {
	faulty int
	pairs  []pairRank
}

func topologySetKeyOf(set []rankedGPU, g *gpuTopology) topologySetKey {
	k := topologySetKey{pairs: make([]pairRank, 0, len(set)*(len(set)-1)/2)}
	for a, x := range set {
		if x.faulty {
			k.faulty++
		}
		for _, y := range set[a+1:] {
			k.pairs = append(k.pairs, g.pairRank(x.device.ID, y.device.ID))
		}
	}
	slices.SortFunc(k.pairs, func(x, y pairRank) int { return comparePairRanks(y, x) })
	return k
}

func (k topologySetKey) compare(o topologySetKey) int {
	if k.faulty != o.faulty {
		return cmpInt(k.faulty, o.faulty)
	}
	for i := range k.pairs {
		if c := comparePairRanks(k.pairs[i], o.pairs[i]); c != 0 {
			return c
		}
	}
	return 0
}

// gpuGroupsUnranked says why prefer_gpu_topology does not rank the valid sets of the candidates,
// or is "" when it does: as selectByTopology, the topology is unknown or every pair of the
// candidates is unknown.
func gpuGroupsUnranked(candidates []rankedGPU, g *gpuTopology, n int) string {
	if reason := topologyUnknownReason(g); reason != "" {
		return "topology unknown: " + reason
	}
	if n < 2 {
		return fewerThanTwoSlots
	}
	for i, a := range candidates {
		for _, b := range candidates[i+1:] {
			if !g.linkRank(a.device.ID, b.device.ID).linkUnknown() {
				return ""
			}
		}
	}
	return "every pair of free GPUs unknown"
}

// bestValidGPUGroupSet returns the valid set of n candidates that ranks first, or nil when there
// is none, with the rule that ranked it and, for prefer_gpu_topology, its worst pair or the reason
// it did not rank:
//   - above maxTopologySets sets, the lowest IDs (lowestValidGPUGroupSet);
//   - with topology, when prefer_gpu_topology ranks the candidates: its set key, then the packing
//     key under layout, then the lowest IDs;
//   - otherwise the packing key under layout (whose last row is the IDs), or GPUs in error last
//     with faultyLast, or the lowest IDs.
//
// The rules are those of tasks without gpu_groups, restricted to the valid sets.
func bestValidGPUGroupSet(
	groups expconf.GPUGroupRequests, candidates []rankedGPU, n int, g *gpuTopology, topology bool,
	layout *numaLayout, faultyLast bool,
) (set []rankedGPU, rule, worst, unranked string) {
	if binomial(len(candidates), n) > maxTopologySets {
		reason := fmt.Sprintf("more than %d sets of free GPUs", maxTopologySets)
		if topology {
			unranked = reason
		}
		return lowestValidGPUGroupSet(groups, candidates, n), "lowest IDs: " + reason, "", unranked
	}
	if topology {
		unranked = gpuGroupsUnranked(candidates, g, n)
		topology = unranked == ""
	}
	var best []rankedGPU
	var bestTopo topologySetKey
	var bestPack packingKey
	var bestFaulty int
	forEachValidGPUGroupSet(groups, candidates, n, func(cand []rankedGPU) {
		c := 1
		var topo topologySetKey
		var pack packingKey
		faulty := 0
		for _, x := range cand {
			if x.faulty {
				faulty++
			}
		}
		if topology {
			topo = topologySetKeyOf(cand, g)
		}
		if layout != nil {
			pack = layout.key(cand)
		}
		if best != nil {
			c = 0
			if topology {
				c = topo.compare(bestTopo)
			}
			if c == 0 && layout != nil {
				c = pack.compare(bestPack)
			}
			if c == 0 && faultyLast {
				c = cmpInt(faulty, bestFaulty)
			}
		}
		// Sets come in lexicographic order of IDs: keeping only a strictly better set makes the
		// lowest IDs win ties.
		if best == nil || c < 0 {
			best = append(best[:0], cand...)
			bestTopo, bestPack, bestFaulty = topo, pack, faulty
		}
	})
	switch {
	case best == nil:
		return nil, "", "", unranked
	case topology:
		worst = g.worstPair(sortedDevices(best))
		return best, "GPU topology preference; " + worst, worst, ""
	case layout != nil:
		return best, "NUMA packing; " + layout.describe(), "", unranked
	default:
		return best, "lowest IDs", "", unranked
	}
}

// selectGPUGroups chooses the devices of a request with gpu_groups: a valid set of the free
// devices, chosen by the rules of the request's other preferences restricted to the valid sets, or
// none with the reason. It never takes map order:
//   - prefer_gpu_topology "strong" with 2 or more slots: the one-NUMA-node rule of
//     selectOnOneNUMANode over the valid sets of each node;
//   - "soft" with 2 or more slots: its set key over the valid sets, then NUMA packing in pools that
//     pack, then the lowest IDs;
//   - otherwise NUMA packing over the valid sets in pools that pack, and the lowest IDs in others.
//
// Above maxTopologySets sets it takes the lowest IDs (of the node, under "strong").
func selectGPUGroups(in gpuSelectionInput, n int, sel deviceSelection) gpuChoice {
	groups := sel.groups
	if groups.Slots() != n {
		return gpuChoice{noSelectionReason: fmt.Sprintf("gpu_groups have %d GPUs for %d slots", groups.Slots(), n)}
	}
	if sel.strong && n >= 2 {
		return selectGPUGroupsOnOneNUMANode(in, n, sel)
	}
	free := rankGPUs(in.free, in.topology, sel.xids)
	var layout *numaLayout
	if sel.packNUMA {
		layout = newNUMALayout(free, rankGPUs(in.allocatable, in.topology, sel.xids))
	}
	candidates := rankGPUs(gpuGroupCandidates(groups, in.free), in.topology, sel.xids)
	set, rule, worst, unranked := bestValidGPUGroupSet(
		groups, candidates, n, in.topology, sel.preferTopology && n >= 2, layout, false)
	if set == nil {
		return gpuChoice{noSelectionReason: "no free GPUs for gpu_groups", unranked: unranked}
	}
	return gpuGroupsChoice(groups, sortedDevices(set), rule, worst, unranked)
}

// selectGPUGroupsOnOneNUMANode is selectGPUGroups for prefer_gpu_topology "strong": the best valid
// set of each NUMA node whose free devices hold one, as bestOnNUMANode chooses a node's best set,
// and of those, the set of the node with the smallest strongNodeKey (selectOnOneNUMANode).
func selectGPUGroupsOnOneNUMANode(in gpuSelectionInput, n int, sel deviceSelection) gpuChoice {
	groups := sel.groups
	layout := newNUMALayout(rankGPUs(in.free, in.topology, sel.xids), rankGPUs(in.allocatable, in.topology, sel.xids))
	var bestSet []device.Device
	var bestRule string
	var bestKey strongNodeKey
	for node, devices := range freeByNUMA(in) {
		candidates := rankGPUs(gpuGroupCandidates(groups, devices), in.topology, sel.xids)
		set, _, _, _ := bestValidGPUGroupSet(groups, candidates, n, in.topology, true, nil, true)
		if set == nil {
			continue
		}
		devs := sortedDevices(set)
		key := strongNodeKey{3: len(layout.free[node]), 4: layout.capacity[node], 5: node}
		var faulty []device.Device
		for _, d := range devs {
			if gpuInError(in.topology, d, sel.xids) {
				key[0]++
				faulty = append(faulty, d)
			}
			switch in.topology.gpuWidth(d.ID) {
			case widthNarrow:
				key[1]++
			case widthUnknown:
				key[2]++
			}
		}
		if bestSet == nil || compareStrongNodeKeys(key, bestKey) < 0 {
			rule := fmt.Sprintf("GPU topology preference strong; NUMA node %d; %s", node, in.topology.worstPair(devs))
			if len(faulty) > 0 {
				rule += "; in error: " + idList(faulty)
			}
			bestSet, bestRule, bestKey = devs, rule, key
		}
	}
	if bestSet == nil {
		if reason := topologyUnknownReason(in.topology); reason != "" {
			return gpuChoice{noSelectionReason: "topology unknown: " + reason}
		}
		return gpuChoice{noSelectionReason: "no NUMA node has free GPUs for gpu_groups"}
	}
	return gpuGroupsChoice(groups, bestSet, bestRule, in.topology.worstPair(bestSet), "")
}

func gpuGroupsChoice(
	groups expconf.GPUGroupRequests, set []device.Device, rule, worst, unranked string,
) gpuChoice {
	assignment := gpuGroupAssignment(groups, set)
	return gpuChoice{
		devices:   set,
		rule:      "gpu_groups " + describeGPUGroupAssignment(groups, assignment) + "; " + rule,
		worstPair: worst,
		unranked:  unranked,
	}
}

// gpuGroupsCannotFit says why no agent of the pool can ever hold the groups, or returns nil while
// one might: the pool has no agent, or an agent's devices hold a valid set with every slot free,
// disabled and draining ones included, and, under prefer_gpu_topology "strong", those of one known
// NUMA node do or the agent has not reported its topology since the master started. Restored
// agents keep their devices' models, so the models decide before an agent reports.
func gpuGroupsCannotFit(
	pool string, agents map[aproto.ID]*agentState, groups expconf.GPUGroupRequests, strong bool,
) error {
	if len(agents) == 0 {
		return nil
	}
	for _, a := range agents {
		devices := a.allDevices()
		if !holdsGPUGroups(groups, devices) {
			continue
		}
		if !strong || a.gpuTopology == nil {
			return nil
		}
		byNode := map[int][]device.Device{}
		for _, d := range devices {
			if node := numaNodeOf(a.gpuTopology, d); node >= 0 {
				byNode[node] = append(byNode[node], d)
			}
		}
		for _, nodeDevices := range byNode {
			if holdsGPUGroups(groups, nodeDevices) {
				return nil
			}
		}
	}
	return errGPUGroupsNoAgent(pool, groups)
}

// allDevices returns the agent's devices, whether they take new work or not, sorted by ID; a
// device without a slot state counts too (slotsByNUMA).
func (a *agentState) allDevices() []device.Device {
	var out []device.Device
	for _, s := range a.slotStates {
		out = append(out, s.device)
	}
	for d := range a.Devices {
		if _, ok := a.slotStates[d.ID]; !ok {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b device.Device) int { return cmpInt(int(a.ID), int(b.ID)) })
	return out
}

// errGPUGroupsNoAgent is why a task with gpu_groups is refused or fails.
func errGPUGroupsNoAgent(pool string, groups expconf.GPUGroupRequests) error {
	return fmt.Errorf("no agent in pool %s has GPUs for gpu_groups %s", pool, groups)
}
