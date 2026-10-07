package agentrm

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

// Fixtures of the cluster's layouts. Nodes 02-08 have GPUs 0-3 on NUMA node 0 and 4-7 on node 1,
// NODE within a socket and SYS across; node01 excludes GPU 4, so NUMA node 1 has slots 5-7; g292
// has one NUMA node and four PCIe switches, each with a PIX pair.

func gpuDevice(id int) device.Device {
	return device.Device{ID: device.ID(id), Brand: "nvda", UUID: fmt.Sprintf("GPU-%d", id), Type: device.CUDA}
}

func gpuDeviceList(ids ...int) []device.Device {
	out := make([]device.Device, 0, len(ids))
	for _, id := range ids {
		out = append(out, gpuDevice(id))
	}
	return out
}

func deviceIDs(devices []device.Device) []int {
	out := make([]int, 0, len(devices))
	for _, d := range devices {
		out = append(out, int(d.ID))
	}
	return out
}

func intRange(lo, hi int) []int {
	var out []int
	for i := lo; i < hi; i++ {
		out = append(out, i)
	}
	return out
}

func without(ids []int, drop ...int) []int {
	var out []int
outer:
	for _, id := range ids {
		for _, d := range drop {
			if id == d {
				continue outer
			}
		}
		out = append(out, id)
	}
	return out
}

var (
	p2pUnknown = aproto.GPUP2PCaps{}
	p2pNotOK   = aproto.GPUP2PCaps{
		Read: aproto.GPUP2PStatusChipsetNotSupported, Write: aproto.GPUP2PStatusChipsetNotSupported,
	}
)

// topologyFixture builds a known topology. numa maps a GPU to its NUMA node (absent or negative:
// unknown); level and p2p give each pair (nil level: NODE on one node, SYS across, unknown when a
// node is unknown).
type topologyFixture struct {
	ids   []int
	numa  map[int]int
	level func(a, b int) aproto.GPULinkLevel
	p2p   func(a, b int) aproto.GPUP2PCaps
	// nvmlError sets the NVML error of these GPUs.
	nvmlError map[int]bool
}

func (f topologyFixture) build() *gpuTopology {
	g := &gpuTopology{gpus: map[device.ID]aproto.GPUInfo{}, pairs: map[gpuPairKey]gpuPair{}}
	for _, id := range f.ids {
		info := aproto.GPUInfo{UUID: gpuDevice(id).UUID}
		if node, ok := f.numa[id]; ok && node >= 0 {
			node := node
			info.NUMANode = &node
		}
		if f.nvmlError[id] {
			info.NVMLError = "GetPciInfo: ERROR_GPU_IS_LOST (15)"
		}
		g.gpus[device.ID(id)] = info
	}
	for i, a := range f.ids {
		for _, b := range f.ids[i+1:] {
			lo, hi := min(a, b), max(a, b)
			pair := gpuPair{level: f.levelOf(lo, hi)}
			if f.p2p != nil {
				pair.p2pAToB = f.p2p(lo, hi)
				pair.p2pBToA = f.p2p(lo, hi)
			}
			g.pairs[gpuPairKey{a: device.ID(lo), b: device.ID(hi)}] = pair
		}
	}
	return g
}

func (f topologyFixture) levelOf(a, b int) aproto.GPULinkLevel {
	if f.level != nil {
		return f.level(a, b)
	}
	na, okA := f.numa[a]
	nb, okB := f.numa[b]
	switch {
	case !okA || !okB || na < 0 || nb < 0:
		return ""
	case na == nb:
		return aproto.GPULinkLevelNode
	default:
		return aproto.GPULinkLevelSys
	}
}

func twoSockets(ids []int) map[int]int {
	numa := map[int]int{}
	for _, id := range ids {
		numa[id] = id / 4
	}
	return numa
}

func allP2P(caps aproto.GPUP2PCaps) func(a, b int) aproto.GPUP2PCaps {
	return func(int, int) aproto.GPUP2PCaps { return caps }
}

var (
	node02IDs = intRange(0, 8)
	node01IDs = without(intRange(0, 8), 4)
	node02    = topologyFixture{ids: node02IDs, numa: twoSockets(node02IDs), p2p: allP2P(p2pNotOK)}
	node01    = topologyFixture{ids: node01IDs, numa: twoSockets(node01IDs), p2p: allP2P(p2pNotOK)}
)

// g292 has one NUMA node and PIX pairs {0,1}, {2,3}, {4,5}, {6,7}.
func g292(p2p aproto.GPUP2PCaps) topologyFixture {
	ids := intRange(0, 8)
	numa := map[int]int{}
	for _, id := range ids {
		numa[id] = 0
	}
	return topologyFixture{
		ids:  ids,
		numa: numa,
		level: func(a, b int) aproto.GPULinkLevel {
			if a/2 == b/2 {
				return aproto.GPULinkLevelPIX
			}
			return aproto.GPULinkLevelNode
		},
		p2p: allP2P(p2p),
	}
}

func selection(free, allocatable []int, g *gpuTopology) gpuSelectionInput {
	return gpuSelectionInput{
		free: gpuDeviceList(free...), allocatable: gpuDeviceList(allocatable...), topology: g,
	}
}

func packed(t *testing.T, f topologyFixture, free, allocatable []int, n int) []int {
	t.Helper()
	c := selectFreeDevices(selection(free, allocatable, f.build()), n, deviceSelection{packNUMA: true})
	require.NotNil(t, c.devices)
	return deviceIDs(c.devices)
}

// fillOrder runs 1-slot tasks one after another until the agent is full.
func fillOrder(f topologyFixture, sel deviceSelection) []int {
	free := append([]int(nil), f.ids...)
	g := f.build()
	var order []int
	for len(free) > 0 {
		c := selectFreeDevices(selection(free, f.ids, g), 1, sel)
		id := int(c.devices[0].ID)
		order = append(order, id)
		free = without(free, id)
	}
	return order
}

func TestNUMAPackingExpectedChoices(t *testing.T) {
	// The expected choices of the amendment's table, every allocatable slot free.
	for n, want := range map[int][]int{
		1: {0}, 2: {0, 1}, 3: {0, 1, 2}, 4: {0, 1, 2, 3}, 5: {0, 1, 2, 3, 4},
	} {
		require.Equal(t, want, packed(t, node02, node02IDs, node02IDs, n), "nodes 02-08, n=%d", n)
	}
	for n, want := range map[int][]int{
		1: {5}, 2: {5, 6}, 3: {5, 6, 7}, 4: {0, 1, 2, 3}, 5: {0, 1, 2, 3, 5},
	} {
		require.Equal(t, want, packed(t, node01, node01IDs, node01IDs, n), "node01, n=%d", n)
	}
	require.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7}, fillOrder(node02, deviceSelection{packNUMA: true}))
	require.Equal(t, []int{5, 6, 7, 0, 1, 2, 3}, fillOrder(node01, deviceSelection{packNUMA: true}))

	for _, c := range []struct {
		free []int
		n    int
		want []int
	}{
		{[]int{0, 1, 2, 4}, 1, []int{4}},
		{[]int{0, 1, 4, 5, 6}, 3, []int{4, 5, 6}},
		{[]int{0, 1, 2, 3, 4, 5}, 2, []int{4, 5}},
		{[]int{0, 1, 2, 4, 5, 6, 7}, 4, []int{4, 5, 6, 7}},
		{[]int{0, 1, 2, 4, 5, 6, 7}, 5, []int{0, 4, 5, 6, 7}},
		{[]int{1, 2, 3, 5, 6, 7}, 4, []int{1, 2, 3, 5}},
		{[]int{2, 3, 5, 6, 7}, 4, []int{2, 5, 6, 7}},
		// The attraction case: every 1-slot task takes GPU 3 while 4-7 stay idle.
		{[]int{3, 4, 5, 6, 7}, 1, []int{3}},
	} {
		require.Equal(t, c.want, packed(t, node02, c.free, node02IDs, c.n), "free %v, n=%d", c.free, c.n)
	}

	// node01 with slot 0 busy: the capacity tie gives slot 5, not slot 1.
	require.Equal(t, []int{5}, packed(t, node01, without(node01IDs, 0), node01IDs, 1))
	// The same with equal capacities would give slot 1.
	require.Equal(t, []int{1}, packed(t, node02, []int{1, 2, 3, 5, 6, 7}, node02IDs, 1))
	// Slot 0 draining and busy: not allocatable, so NUMA node 0 has 3 slots and 3 free GPUs.
	require.Equal(t, []int{1}, packed(t, node02, intRange(1, 8), intRange(1, 8), 1))
	// node02 with slots 0, 1 and 4 disabled.
	disabled := without(node02IDs, 0, 1, 4)
	require.Equal(t, []int{2, 3}, packed(t, node02, disabled, disabled, 2))
	require.Equal(t, []int{2, 5, 6, 7}, packed(t, node02, disabled, disabled, 4))
}

func TestNUMAPackingUnknownNUMANodes(t *testing.T) {
	// Slot 3's NUMA node is unknown: 1-slot tasks take it last.
	f := node02
	f.numa = twoSockets(node02IDs)
	delete(f.numa, 3)
	require.Equal(t, []int{0, 1, 2, 4, 5, 6, 7, 3}, fillOrder(f, deviceSelection{packNUMA: true}))
	// A busy unknown GPU does not change the choice.
	require.Equal(t, []int{0, 1}, packed(t, f, without(node02IDs, 3), node02IDs, 2))

	// A topology that holds no NUMA node gives the lowest free IDs.
	free := []int{2, 3, 5, 6}
	unknownTopologies := map[string]*gpuTopology{
		"nil":            nil,
		"unknown reason": {unknownReason: "NVML init: ERROR_LIBRARY_NOT_FOUND (12)"},
		"no GPUs":        {gpus: map[device.ID]aproto.GPUInfo{}},
		"negative node": func() *gpuTopology {
			minus := -1
			g := node02.build()
			for id, info := range g.gpus {
				info.NUMANode = &minus
				g.gpus[id] = info
			}
			return g
		}(),
		"nil node": func() *gpuTopology {
			g := node02.build()
			for id, info := range g.gpus {
				info.NUMANode = nil
				g.gpus[id] = info
			}
			return g
		}(),
	}
	for name, g := range unknownTopologies {
		c := selectFreeDevices(selection(free, node02IDs, g), 3, deviceSelection{packNUMA: true})
		require.Equal(t, []int{2, 3, 5}, deviceIDs(c.devices), name)
		require.Contains(t, c.rule, "lowest free IDs", name)
	}
	// Non-CUDA slots have no NUMA node.
	cpus := []device.Device{{ID: 1, Type: device.CPU}, {ID: 0, Type: device.CPU}}
	sort.Slice(cpus, func(i, j int) bool { return cpus[i].ID < cpus[j].ID })
	c := selectFreeDevices(gpuSelectionInput{free: cpus, allocatable: cpus, topology: node02.build()}, 1,
		deviceSelection{packNUMA: true})
	require.Equal(t, []int{0}, deviceIDs(c.devices))

	// A one-node host gives the lowest free IDs through packing.
	g292NoP2P := g292(p2pNotOK)
	require.Equal(t, []int{0, 1}, packed(t, g292NoP2P, intRange(0, 8), intRange(0, 8), 2))
	require.Equal(t, []int{1, 2}, packed(t, g292NoP2P, intRange(1, 8), intRange(0, 8), 2))
}

func TestNUMAPackingAboveEightNodesTakesLowestIDs(t *testing.T) {
	ids := intRange(0, 10)
	numa := map[int]int{}
	for _, id := range ids {
		numa[id] = 9 - id
	}
	f := topologyFixture{ids: ids, numa: numa}
	c := selectFreeDevices(selection(ids, ids, f.build()), 2, deviceSelection{packNUMA: true})
	require.Equal(t, []int{0, 1}, deviceIDs(c.devices))
	require.Equal(t, "lowest free IDs: more than 8 NUMA nodes", c.rule)
}

func TestNUMAPackingRules(t *testing.T) {
	// The rule of the pool's Debug line says how packing chose, without nested parentheses.
	rule := func(g *gpuTopology, free []int, n int, xids ...int) string {
		sel := deviceSelection{packNUMA: true, xids: map[string]bool{}}
		for _, id := range xids {
			sel.xids[gpuDevice(id).UUID] = true
		}
		return selectFreeDevices(selection(free, node02IDs, g), n, sel).rule
	}
	require.Equal(t, "NUMA packing; free per NUMA node 0:4 1:4", rule(node02.build(), node02IDs, 2))
	require.Equal(t, "NUMA packing; free per NUMA node 0:3 1:3; in error: 0,5",
		rule(node02.build(), node02IDs, 2, 0, 5))
	require.Equal(t, "lowest free IDs: not reported since the master started; in error last: 0",
		rule(nil, []int{0, 1, 2}, 2, 0))
	// Every free GPU in error: GPUs in error take no part in the NUMA rows, so the lowest IDs.
	require.Equal(t, "lowest free IDs: every free GPU in error; in error last: 1,2,3",
		rule(node02.build(), []int{1, 2, 3}, 2, 1, 2, 3))

	cpus := []device.Device{{ID: 0, Type: device.CPU}, {ID: 1, Type: device.CPU}}
	c := selectFreeDevices(gpuSelectionInput{free: cpus, allocatable: cpus, topology: node02.build()}, 1,
		deviceSelection{packNUMA: true})
	require.Equal(t, "lowest free IDs: no free healthy GPU with a known NUMA node", c.rule)
}

func TestGPUsInErrorRankLast(t *testing.T) {
	// GPU 0 had an NVML error at agent start: it is used only when nothing else is free.
	f := node02
	f.nvmlError = map[int]bool{0: true}
	require.Equal(t, []int{1, 2, 3, 4, 5, 6, 7, 0}, fillOrder(f, deviceSelection{packNUMA: true}))
	require.Equal(t, []int{4, 5, 6, 7}, packed(t, f, node02IDs, node02IDs, 4))
	require.Equal(t, []int{0, 1, 2, 3, 4, 5, 6, 7}, packed(t, f, node02IDs, node02IDs, 8))

	// A recent critical XID does the same, also when the topology is unknown.
	xids := map[string]bool{gpuDevice(5).UUID: true}
	order := fillOrder(node01, deviceSelection{packNUMA: true, xids: xids})
	require.Equal(t, []int{6, 7, 0, 1, 2, 3, 5}, order)
	c := selectFreeDevices(selection([]int{3, 5, 6}, node01IDs, nil), 3, deviceSelection{packNUMA: true, xids: xids})
	require.Equal(t, []int{3, 5, 6}, deviceIDs(c.devices))
	c = selectFreeDevices(selection([]int{3, 5, 6}, node01IDs, nil), 2, deviceSelection{packNUMA: true, xids: xids})
	require.Equal(t, []int{3, 6}, deviceIDs(c.devices))

	// prefer_gpu_topology ranks a GPU in error last too, before the set key: {0,1} is the only
	// NVLink pair, but GPU 0 is in error.
	nv := node02
	nv.nvmlError = map[int]bool{0: true}
	nv.p2p = allP2P(p2pOK)
	g := nv.build()
	p := g.pairs[gpuPairKey{a: 0, b: 1}]
	p.nvlinks = 4
	g.pairs[gpuPairKey{a: 0, b: 1}] = p
	c = selectFreeDevices(selection(node02IDs, node02IDs, g), 2, deviceSelection{preferTopology: true})
	require.Equal(t, []int{1, 2}, deviceIDs(c.devices))
	c = selectFreeDevices(selection([]int{0, 1, 5}, node02IDs, g), 2, deviceSelection{
		preferTopology: true, xids: map[string]bool{gpuDevice(5).UUID: true},
	})
	require.Equal(t, []int{0, 1}, deviceIDs(c.devices), "one GPU in error either way: the NVLink pair")
}

// bruteForcePacking returns the n-subset of free with the smallest packingKey.
func bruteForcePacking(free []rankedGPU, l numaLayout, n int) []int {
	var best []rankedGPU
	var bestKey packingKey
	forEachSubset(len(free), n, func(idx []int) {
		set := make([]rankedGPU, n)
		for i, x := range idx {
			set[i] = free[x]
		}
		if k := l.key(set); best == nil || k.compare(bestKey) < 0 {
			best, bestKey = set, k
		}
	})
	return deviceIDs(sortedDevices(best))
}

func forEachSubset(f, n int, fn func(idx []int)) {
	idx := make([]int, n)
	var rec func(pos, start int)
	rec = func(pos, start int) {
		if pos == n {
			fn(idx)
			return
		}
		for i := start; i <= f-(n-pos); i++ {
			idx[pos] = i
			rec(pos+1, i+1)
		}
	}
	rec(0, 0)
}

func TestNUMAPackingConstructionEqualsBruteForce(t *testing.T) {
	// As numa/merge/merged.py check_random: 1-4 NUMA nodes, IDs interleaved across nodes, unknown
	// NUMA nodes, GPUs in error, random allocatable and free slots. merged.py matched in
	// 27,272/27,272 cases without GPUs in error.
	rng := rand.New(rand.NewSource(1)) //nolint:gosec
	cases := 0
	for trial := 0; trial < 4000; trial++ {
		m := 2 + rng.Intn(9)
		nodes := 1 + rng.Intn(4)
		f := topologyFixture{ids: intRange(0, m), numa: map[int]int{}, nvmlError: map[int]bool{}}
		var free, allocatable []int
		for id := 0; id < m; id++ {
			if rng.Float64() >= 0.15 {
				f.numa[id] = rng.Intn(nodes)
			}
			if rng.Float64() < 0.1 {
				f.nvmlError[id] = true
			}
			isFree := rng.Float64() < 0.75
			if isFree {
				free = append(free, id)
			}
			if isFree || rng.Float64() < 0.8 {
				allocatable = append(allocatable, id)
			}
		}
		if len(free) == 0 {
			free, allocatable = []int{0}, append([]int{0}, without(allocatable, 0)...)
			sort.Ints(allocatable)
		}
		g := f.build()
		in := selection(free, allocatable, g)
		ranked := rankGPUs(in.free, g, nil)
		layout := newNUMALayout(ranked, rankGPUs(in.allocatable, g, nil))
		for n := 1; n <= len(free); n++ {
			cases++
			c := selectFreeDevices(in, n, deviceSelection{packNUMA: true})
			got := deviceIDs(c.devices)
			require.Equal(t, bruteForcePacking(ranked, *layout, n), got,
				"numa %v, error %v, free %v, allocatable %v, n=%d", f.numa, f.nvmlError, free, allocatable, n)
			require.Len(t, got, n)
			require.Subset(t, free, got)
			again := selectFreeDevices(in, n, deviceSelection{packNUMA: true})
			require.Equal(t, got, deviceIDs(again.devices))
		}
	}
	t.Logf("construction equals the brute-force argmin in %d cases", cases)
}

func TestNUMAPackingTouchesFewestNUMANodes(t *testing.T) {
	// On two nodes with every GPU known and healthy, a set that fits one node stays on one node.
	for mask := 1; mask < 1<<8; mask++ {
		var free []int
		perNode := [2]int{}
		for id := 0; id < 8; id++ {
			if mask&(1<<id) != 0 {
				free = append(free, id)
				perNode[id/4]++
			}
		}
		for n := 1; n <= len(free); n++ {
			got := packed(t, node02, free, node02IDs, n)
			nodes := map[int]bool{}
			for _, id := range got {
				nodes[id/4] = true
			}
			if n <= max(perNode[0], perNode[1]) {
				require.Len(t, nodes, 1, "free %v, n=%d", free, n)
			}
		}
	}
}

func TestPairRank(t *testing.T) {
	levels := []aproto.GPULinkLevel{
		aproto.GPULinkLevelInternal, aproto.GPULinkLevelPIX, aproto.GPULinkLevelPXB,
		aproto.GPULinkLevelPHB, aproto.GPULinkLevelNode, aproto.GPULinkLevelSys, "",
	}
	const usable, notUsable, unknown = "usable", "not usable", "unknown"
	states := map[string]aproto.GPUP2PCaps{usable: p2pOK, notUsable: p2pNotOK, unknown: p2pUnknown}
	rankOf := func(level aproto.GPULinkLevel, caps aproto.GPUP2PCaps, nvlinks int) pairRank {
		g := &gpuTopology{pairs: map[gpuPairKey]gpuPair{
			{a: 0, b: 1}: {level: level, nvlinks: nvlinks, p2pAToB: caps, p2pBToA: caps},
		}, gpus: map[device.ID]aproto.GPUInfo{
			0: {UUID: "a", NVMLError: "GetPciInfo: ERROR_UNKNOWN (999)", PCIeLinkWidth: 8},
			1: {UUID: "b", PCIeLinkWidth: 16},
		}}
		return g.pairRank(0, 1)
	}

	// A usable pair with unknown level and no NVLink beats a not-usable PIX pair; a same-NUMA pair
	// with unknown P2P beats a not-usable SYS pair.
	require.Negative(t, comparePairRanks(rankOf("", p2pOK, 0), rankOf(aproto.GPULinkLevelPIX, p2pNotOK, 0)))
	require.Negative(t, comparePairRanks(
		rankOf(aproto.GPULinkLevelNode, p2pUnknown, 0), rankOf(aproto.GPULinkLevelSys, p2pNotOK, 0)))
	// Without P2P, two switches beat one switch.
	require.Negative(t, comparePairRanks(
		rankOf(aproto.GPULinkLevelNode, p2pNotOK, 0), rankOf(aproto.GPULinkLevelPIX, p2pNotOK, 0)))
	// With P2P, PIX beats NODE.
	require.Negative(t, comparePairRanks(
		rankOf(aproto.GPULinkLevelPIX, p2pOK, 0), rankOf(aproto.GPULinkLevelNode, p2pOK, 0)))
	// More NVLinks first.
	require.Negative(t, comparePairRanks(rankOf("", p2pOK, 4), rankOf(aproto.GPULinkLevelInternal, p2pOK, 2)))

	numaClassOf := map[aproto.GPULinkLevel]int{aproto.GPULinkLevelSys: 1, "": 2}
	for _, la := range levels {
		for na, ca := range states {
			for _, nvA := range []int{0, 2} {
				a := rankOf(la, ca, nvA)
				// NVLinks never change a key without usable P2P.
				if na != usable {
					require.Equal(t, rankOf(la, ca, 0), a)
				}
				for _, lb := range levels {
					for nb, cb := range states {
						b := rankOf(lb, cb, 0)
						switch {
						case na == usable && nb != usable:
							require.Negative(t, comparePairRanks(a, b), "%s %s vs %s %s", la, na, lb, nb)
						case na != usable && nb != usable && numaClassOf[la] < numaClassOf[lb]:
							require.Negative(t, comparePairRanks(a, b), "%s %s vs %s %s", la, na, lb, nb)
						case na == notUsable && nb == unknown && la == lb:
							require.Negative(t, comparePairRanks(a, b), "%s %s vs %s %s", la, na, lb, nb)
						}
					}
				}
			}
		}
	}
	// Link width and the NVML error field never change a key.
	plain := &gpuTopology{pairs: map[gpuPairKey]gpuPair{
		{a: 0, b: 1}: {level: aproto.GPULinkLevelNode, p2pAToB: p2pOK, p2pBToA: p2pOK},
	}}
	require.Equal(t, plain.pairRank(0, 1), rankOf(aproto.GPULinkLevelNode, p2pOK, 0))
	// A missing pair, as for a GPU whose handle lookup failed, is in the last row.
	require.Equal(t, unknownPairRank, (&gpuTopology{}).pairRank(0, 1))
	require.Equal(t, unknownPairRank, rankOf("", p2pUnknown, 0))
}

func topologySelect(
	t *testing.T, f topologyFixture, free []int, n int, packNUMA bool,
) gpuChoice {
	t.Helper()
	return selectFreeDevices(selection(free, f.ids, f.build()), n,
		deviceSelection{preferTopology: true, packNUMA: packNUMA})
}

func TestTopologyPreferenceExpectedChoices(t *testing.T) {
	// g292 with the stock driver: no P2P, so GPUs on different switches.
	stock := g292(p2pNotOK)
	require.Equal(t, []int{0, 2}, deviceIDs(topologySelect(t, stock, intRange(0, 8), 2, true).devices))
	require.Equal(t, []int{0, 2, 4, 6}, deviceIDs(topologySelect(t, stock, intRange(0, 8), 4, true).devices))
	// With usable P2P, PIX pairs first, also over packing's lowest IDs.
	patched := g292(p2pOK)
	require.Equal(t, []int{0, 1}, deviceIDs(topologySelect(t, patched, intRange(0, 8), 2, true).devices))
	require.Equal(t, []int{2, 3}, deviceIDs(topologySelect(t, patched, intRange(1, 8), 2, true).devices))
	require.Equal(t, []int{0, 1, 2, 3}, deviceIDs(topologySelect(t, patched, intRange(0, 8), 4, true).devices))
	// Plain tasks there get the lowest free IDs.
	require.Equal(t, []int{1, 2}, packed(t, patched, intRange(1, 8), intRange(0, 8), 2))

	// node02 with free {0,1,4,5,6}: one socket.
	c := topologySelect(t, node02, []int{0, 1, 4, 5, 6}, 3, true)
	require.Equal(t, []int{4, 5, 6}, deviceIDs(c.devices))
	require.Equal(t, "worst pair NODE, P2P not usable", c.worstPair)
	// Leximax: the worst pair decides first, then the second worst.
	c = topologySelect(t, node02, []int{0, 1, 2, 4, 5, 6}, 4, false)
	require.Equal(t, []int{0, 1, 2, 4}, deviceIDs(c.devices), "IDs break the tie between 3+1 sets")
	require.Equal(t, "worst pair SYS, P2P not usable", c.worstPair)
	// A 4+1 split has fewer cross-socket pairs than 3+2.
	c = topologySelect(t, node02, []int{0, 1, 4, 5, 6, 7}, 5, true)
	require.Equal(t, []int{0, 4, 5, 6, 7}, deviceIDs(c.devices))
}

func TestTopologyPreferenceComposition(t *testing.T) {
	// On the 4+4 and 4+3 layouts, with P2P usable on every pair and on none, opted-in and plain
	// tasks get the same set for every free set and n >= 2 (merged.py: 769/769 and 321/321).
	for name, f := range map[string]topologyFixture{"4+4": node02, "4+3": node01} {
		for _, caps := range []aproto.GPUP2PCaps{p2pOK, p2pNotOK} {
			f.p2p = allP2P(caps)
			g := f.build()
			cases := 0
			forEachNonEmptySubset(f.ids, func(free []int) {
				for n := 2; n <= len(free); n++ {
					cases++
					in := selection(free, f.ids, g)
					plain := selectFreeDevices(in, n, deviceSelection{packNUMA: true})
					opted := selectFreeDevices(in, n, deviceSelection{packNUMA: true, preferTopology: true})
					require.Equal(t, deviceIDs(plain.devices), deviceIDs(opted.devices),
						"%s, free %v, n=%d", name, free, n)
				}
			})
			require.Equal(t, map[string]int{"4+4": 769, "4+3": 321}[name], cases)
		}
	}

	// GPU 6's P2P is not usable: the tied usable pairs go to the packing key, {4,7} on the fuller
	// node. With IDs as the tie-break (worst, or numa_packing off), {0,1}.
	mixed := node02
	mixed.p2p = func(a, b int) aproto.GPUP2PCaps {
		if a == 6 || b == 6 {
			return p2pNotOK
		}
		return p2pOK
	}
	free := []int{0, 1, 2, 3, 4, 6, 7}
	require.Equal(t, []int{4, 7}, deviceIDs(topologySelect(t, mixed, free, 2, true).devices))
	require.Equal(t, []int{0, 1}, deviceIDs(topologySelect(t, mixed, free, 2, false).devices))
	require.Equal(t, []int{4, 6}, packed(t, mixed, free, node02IDs, 2))
}

func forEachNonEmptySubset(ids []int, fn func([]int)) {
	for mask := 1; mask < 1<<len(ids); mask++ {
		var set []int
		for i, id := range ids {
			if mask&(1<<i) != 0 {
				set = append(set, id)
			}
		}
		fn(set)
	}
}

func TestTopologyPreferenceGate(t *testing.T) {
	everyPairUnknown := topologyFixture{
		ids: intRange(0, 4), numa: twoSockets(intRange(0, 4)),
		level: func(int, int) aproto.GPULinkLevel { return "" },
	}
	c := topologySelect(t, everyPairUnknown, intRange(0, 4), 2, false)
	require.Nil(t, c.devices, "nothing to rank: map order")
	require.Equal(t, "every pair of free GPUs unknown", c.unranked)
	c = topologySelect(t, everyPairUnknown, intRange(0, 4), 2, true)
	require.Equal(t, []int{0, 1}, deviceIDs(c.devices), "nothing to rank: packing")
	require.Equal(t, "every pair of free GPUs unknown", c.unranked)

	// One pair with a known P2P state is enough to rank.
	onePair := everyPairUnknown
	onePair.p2p = func(a, b int) aproto.GPUP2PCaps {
		if a == 2 && b == 3 {
			return p2pOK
		}
		return p2pUnknown
	}
	c = topologySelect(t, onePair, intRange(0, 4), 2, false)
	require.Equal(t, []int{2, 3}, deviceIDs(c.devices))
	require.Empty(t, c.unranked)

	c = selectFreeDevices(selection(intRange(0, 4), intRange(0, 4), nil), 2, deviceSelection{preferTopology: true})
	require.Nil(t, c.devices)
	require.Equal(t, "topology unknown: "+reasonNotReportedSinceMasterStart, c.unranked)

	// Fewer than 2 slots: as without the preference.
	c = topologySelect(t, node02, node02IDs, 1, false)
	require.Nil(t, c.devices)
	require.Empty(t, c.unranked)
}

func TestTopologyPreferenceSubsetCap(t *testing.T) {
	ids := intRange(0, 17)
	f := topologyFixture{ids: ids, numa: twoSockets(ids), p2p: allP2P(p2pOK)}
	require.Equal(t, 12870, binomial(16, 8))
	require.Greater(t, binomial(17, 8), maxTopologySets)
	c := topologySelect(t, f, ids, 8, false)
	require.Nil(t, c.devices)
	require.Equal(t, "more than 20000 sets of free GPUs", c.unranked)
	c = topologySelect(t, f, ids[:16], 8, false)
	require.Equal(t, intRange(0, 8), deviceIDs(c.devices))
}

func TestTopologyPreferenceEqualsBruteForce(t *testing.T) {
	// Over random topologies (levels, P2P, NVLinks, NUMA nodes, GPUs in error), "soft" gives the
	// argmin of: GPUs in error, the pair ranks worst first, then the packing key or the IDs.
	rng := rand.New(rand.NewSource(3)) //nolint:gosec
	levels := []aproto.GPULinkLevel{
		aproto.GPULinkLevelPIX, aproto.GPULinkLevelPXB, aproto.GPULinkLevelNode, aproto.GPULinkLevelSys, "",
	}
	caps := []aproto.GPUP2PCaps{p2pOK, p2pNotOK, p2pUnknown}
	cases := 0
	for trial := 0; trial < 1500; trial++ {
		m := 2 + rng.Intn(7)
		f := topologyFixture{ids: intRange(0, m), numa: map[int]int{}, nvmlError: map[int]bool{}}
		for id := 0; id < m; id++ {
			if rng.Float64() >= 0.15 {
				f.numa[id] = rng.Intn(3)
			}
			f.nvmlError[id] = rng.Float64() < 0.15
		}
		g := f.build()
		for k := range g.pairs {
			p2p := caps[rng.Intn(len(caps))]
			pair := gpuPair{level: levels[rng.Intn(len(levels))], p2pAToB: p2p, p2pBToA: p2p}
			if rng.Float64() < 0.2 {
				pair.nvlinks = 1 + rng.Intn(2)
			}
			g.pairs[k] = pair
		}
		var free []int
		for id := 0; id < m; id++ {
			if rng.Float64() < 0.8 {
				free = append(free, id)
			}
		}
		in := selection(free, f.ids, g)
		ranked := rankGPUs(in.free, g, nil)
		layout := newNUMALayout(ranked, rankGPUs(in.allocatable, g, nil))
		for n := 2; n <= len(free); n++ {
			for _, packNUMA := range []bool{false, true} {
				c := selectFreeDevices(in, n, deviceSelection{preferTopology: true, packNUMA: packNUMA})
				if c.unranked != "" {
					continue
				}
				cases++
				var best []rankedGPU
				var bestFaulty int
				var bestPairs []pairRank
				forEachSubset(len(ranked), n, func(idx []int) {
					set := make([]rankedGPU, n)
					faulty := 0
					var pairs []pairRank
					for a, i := range idx {
						set[a] = ranked[i]
						if ranked[i].faulty {
							faulty++
						}
						for _, j := range idx[a+1:] {
							pairs = append(pairs, g.pairRank(ranked[i].device.ID, ranked[j].device.ID))
						}
					}
					sort.Slice(pairs, func(x, y int) bool { return comparePairRanks(pairs[x], pairs[y]) > 0 })
					cmp := -1
					if best != nil {
						cmp = cmpInt(faulty, bestFaulty)
						for x := 0; cmp == 0 && x < len(pairs); x++ {
							cmp = comparePairRanks(pairs[x], bestPairs[x])
						}
						if cmp == 0 && packNUMA {
							cmp = layout.key(set).compare(layout.key(best))
						}
					}
					if cmp < 0 {
						best, bestFaulty, bestPairs = set, faulty, pairs
					}
				})
				require.Equal(t, deviceIDs(sortedDevices(best)), deviceIDs(c.devices),
					"trial %d, free %v, n=%d, packing %v", trial, free, n, packNUMA)
			}
		}
	}
	t.Logf("soft equals the brute-force argmin in %d cases", cases)
}

func BenchmarkTopologyPreferenceEightChooseFour(b *testing.B) {
	g := node02.build()
	in := selection(node02IDs, node02IDs, g)
	sel := deviceSelection{preferTopology: true, packNUMA: true}
	for i := 0; i < b.N; i++ {
		selectFreeDevices(in, 4, sel)
	}
}

func BenchmarkTopologyPreferenceSixteenChooseEightAllTied(b *testing.B) {
	// The worst case under the set cap: every pair equal, so every set ties and goes to packing.
	ids := intRange(0, 16)
	numa := map[int]int{}
	for _, id := range ids {
		numa[id] = 0
	}
	f := topologyFixture{ids: ids, numa: numa, p2p: allP2P(p2pOK)}
	in := selection(ids, ids, f.build())
	sel := deviceSelection{preferTopology: true, packNUMA: true}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		selectFreeDevices(in, 8, sel)
	}
}
