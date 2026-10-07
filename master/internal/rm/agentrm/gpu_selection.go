package agentrm

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
)

// GPU selection chooses which free devices of the agent the scheduler picked a reservation gets.
// It never changes how many devices a task gets or on which agent: the fit and the agent choice
// are made before it, from counts only.
//
// Two rules rank the free devices; without either, a reservation takes them in map order, as before
// GPU selection existed:
//   - NUMA packing (packingKey), for every task in a pool with fitting_policy best and numa_packing
//     not false;
//   - prefer_gpu_topology "soft" (topology set key), for a task on one agent with 2 or more slots.
//     Under NUMA packing, packing breaks its ties; otherwise the lowest IDs do.
//
// Both rank GPUs in error last: an NVML health call of the GPU failed at agent start, or the GPU has
// a recent critical XID (gpuhealth.IsCriticalXID: the application codes 13, 31, 43 and 45 never
// count). This supersedes PR B's earlier rule "No NVML error affects ranking by itself": an NVML
// error ranks a GPU last, and otherwise the keys read only reported values.
//
// Every function here is pure and never logs: the scheduler's copies, which have no syslog, run
// them too.

const (
	// noKnownNUMANode describes an agent without a free healthy GPU with a known NUMA node.
	noKnownNUMANode = "no free healthy GPU with a known NUMA node"
	// maxTopologySets bounds the sets of free GPUs that prefer_gpu_topology compares in one
	// reservation; above it, the reservation takes the pool's default (C(16,8) = 12870 fits).
	maxTopologySets = 20000
	// maxPackingNUMANodes bounds the NUMA nodes that NUMA packing compares; above it, packing takes
	// the lowest free IDs. Two sockets at NPS4 give 8.
	maxPackingNUMANodes = 8
)

// deviceSelection is how one reservation chooses its devices. The zero value takes the free devices
// in map order.
type deviceSelection struct {
	// packNUMA packs GPUs by NUMA node: the pool's fitting_policy is best and numa_packing is on.
	packNUMA bool
	// preferTopology ranks GPU sets by the topology: the task asks for prefer_gpu_topology "soft"
	// and the fit is on one agent.
	preferTopology bool
	// xids holds the UUIDs of GPUs with a recent critical XID, read once per scheduling pass.
	xids map[string]bool
}

func (s deviceSelection) ranks() bool {
	return s.packNUMA || s.preferTopology
}

// gpuSelectionInput is what a selection reads of an agent.
type gpuSelectionInput struct {
	// free holds the free devices (no container), sorted by ID. With draining slots kept out of
	// Devices once free, these are exactly the allocatable free slots.
	free []device.Device
	// allocatable holds the devices of slots that take new work, free or in use, sorted by ID. A
	// device without a slot state counts.
	allocatable []device.Device
	topology    *gpuTopology
}

// gpuChoice is the result of a selection.
type gpuChoice struct {
	// devices are the chosen devices sorted by ID, or nil for map order.
	devices []device.Device
	// rule says how the devices were chosen, for the pool's Debug line.
	rule string
	// worstPair describes the worst pair of a set chosen by prefer_gpu_topology, for the task log.
	worstPair string
	// unranked is why prefer_gpu_topology did not choose the set, for the task log.
	unranked string
}

// rankedGPU is a free device with what the keys read of it.
type rankedGPU struct {
	device device.Device
	// numa is the device's NUMA node, or -1 when it is unknown.
	numa int
	// faulty is set for a GPU in error (see the top of the file).
	faulty bool
}

// numaNodeOf returns the device's NUMA node, or -1 when the agent did not report one: no topology
// (an agent restored from the snapshot before its AgentStarted), a topology with an unknown reason,
// a device that is not a CUDA slot, a GPU without an entry, or a nil or negative NUMA node.
func numaNodeOf(g *gpuTopology, d device.Device) int {
	if g == nil || g.unknownReason != "" || d.Type != device.CUDA {
		return -1
	}
	info, ok := g.gpus[d.ID]
	if !ok || info.NUMANode == nil || *info.NUMANode < 0 {
		return -1
	}
	return *info.NUMANode
}

// gpuInError reports whether a device is a GPU in error: an NVML health call of it failed at agent
// start, or it has a recent critical XID. It matches the error row of gpuhealth.Classify.
func gpuInError(g *gpuTopology, d device.Device, xids map[string]bool) bool {
	if xids[d.UUID] {
		return true
	}
	if g == nil {
		return false
	}
	info, ok := g.gpus[d.ID]
	return ok && info.NVMLError != ""
}

func rankGPUs(devices []device.Device, g *gpuTopology, xids map[string]bool) []rankedGPU {
	out := make([]rankedGPU, 0, len(devices))
	for _, d := range devices {
		out = append(out, rankedGPU{device: d, numa: numaNodeOf(g, d), faulty: gpuInError(g, d, xids)})
	}
	return out
}

// selectFreeDevices chooses n of the free devices, or returns no devices for map order: n is 0,
// fewer than n are free, or neither rule applies. It is the one selection of both the live
// reservation and the scheduler's copies.
func selectFreeDevices(in gpuSelectionInput, n int, sel deviceSelection) gpuChoice {
	if n <= 0 || len(in.free) < n || !sel.ranks() {
		return gpuChoice{}
	}
	free := rankGPUs(in.free, in.topology, sel.xids)
	var layout *numaLayout
	if sel.packNUMA {
		layout = newNUMALayout(free, rankGPUs(in.allocatable, in.topology, sel.xids))
	}

	var out gpuChoice
	if sel.preferTopology && n >= 2 {
		set, worst, unranked := selectByTopology(free, in.topology, n, layout)
		if set != nil {
			return gpuChoice{
				devices:   set,
				rule:      "GPU topology preference; " + worst,
				worstPair: worst,
			}
		}
		out.unranked = unranked
	}
	if layout == nil {
		return out
	}
	out.devices, out.rule = packByNUMA(free, *layout, n, topologyUnknownReason(in.topology))
	return out
}

// topologyUnknownReason says why an agent's topology is unknown, or is "" when it is known.
func topologyUnknownReason(g *gpuTopology) string {
	if g == nil {
		return reasonNotReportedSinceMasterStart
	}
	return g.unknownReason
}

// numaLayout is what NUMA packing reads of an agent: the free healthy GPUs with a known NUMA node,
// by node, and the healthy allocatable slots with a known NUMA node, by node. GPUs in error and GPUs
// without a known NUMA node take no part in rows 2 to 4 of the key.
type numaLayout struct {
	// nodes holds the NUMA nodes with a free healthy GPU, in ascending order.
	nodes []int
	// index holds the position of each node in nodes.
	index map[int]int
	// free holds the free healthy GPUs of each node, by ID.
	free map[int][]rankedGPU
	// capacity holds the number of healthy allocatable slots of each node.
	capacity map[int]int
}

func newNUMALayout(free, allocatable []rankedGPU) *numaLayout {
	l := &numaLayout{free: map[int][]rankedGPU{}, capacity: map[int]int{}}
	for _, g := range free {
		if g.faulty || g.numa < 0 {
			continue
		}
		if len(l.free[g.numa]) == 0 {
			l.nodes = append(l.nodes, g.numa)
		}
		l.free[g.numa] = append(l.free[g.numa], g)
	}
	sort.Ints(l.nodes)
	l.index = make(map[int]int, len(l.nodes))
	for i, node := range l.nodes {
		l.index[node] = i
	}
	for _, g := range allocatable {
		if !g.faulty && g.numa >= 0 {
			l.capacity[g.numa]++
		}
	}
	return l
}

// packingKey orders the sets of n free GPUs for NUMA packing. Smaller is better, row by row:
//  0. fewer GPUs in error;
//  1. fewer GPUs with an unknown NUMA node;
//  2. fewer pairs of GPUs on different NUMA nodes;
//  3. the free GPUs left per NUMA node, sorted largest first: the lexicographically larger list,
//     which takes from the fullest node that holds the task and keeps the biggest free blocks;
//  4. for a set on one NUMA node: fewer allocatable slots on that node, so a smaller node fills
//     first;
//  5. the smaller sorted list of device IDs.
//
// Rows 2 to 4 count only healthy GPUs with a known NUMA node.
type packingKey struct {
	faulty  int
	unknown int
	cross   int
	left    []int
	oneNode int
	ids     []device.ID
}

func (l numaLayout) key(set []rankedGPU) packingKey {
	k := packingKey{ids: make([]device.ID, 0, len(set)), left: make([]int, len(l.nodes))}
	// left first counts the set's GPUs on each node: a free healthy GPU with a known NUMA node is
	// on one of l.nodes.
	known := 0
	for _, g := range set {
		k.ids = append(k.ids, g.device.ID)
		switch {
		case g.faulty:
			k.faulty++
		case g.numa < 0:
			k.unknown++
		default:
			k.left[l.index[g.numa]]++
			known++
		}
	}
	slices.Sort(k.ids)
	sumSquares, used, onNode := 0, 0, 0
	for i, share := range k.left {
		sumSquares += share * share
		if share > 0 {
			used++
			onNode = l.nodes[i]
		}
		k.left[i] = len(l.free[l.nodes[i]]) - share
	}
	k.cross = (known*known - sumSquares) / 2
	slices.SortFunc(k.left, func(a, b int) int { return cmpInt(b, a) })
	if used == 1 {
		k.oneNode = l.capacity[onNode]
	}
	return k
}

// compare returns -1 when k is better than o, 1 when it is worse, and 0 for equal keys.
func (k packingKey) compare(o packingKey) int {
	for _, c := range [][2]int{{k.faulty, o.faulty}, {k.unknown, o.unknown}, {k.cross, o.cross}} {
		if c[0] != c[1] {
			return cmpInt(c[0], c[1])
		}
	}
	for i := range k.left {
		if i < len(o.left) && k.left[i] != o.left[i] {
			// More left on the fullest node is better.
			return cmpInt(o.left[i], k.left[i])
		}
	}
	if k.oneNode != o.oneNode {
		return cmpInt(k.oneNode, o.oneNode)
	}
	return compareIDs(k.ids, o.ids)
}

// packByNUMA returns the n free GPUs with the smallest packingKey, sorted by ID, without
// enumerating GPU subsets: the healthy GPUs with a known NUMA node fill some nodes completely and
// take the lowest free IDs of at most one more node, at most (m+1)·2^m candidates for m nodes. When
// n is at least the number of those GPUs, it takes all of them, then the lowest IDs of the healthy
// GPUs without a known NUMA node, then the lowest IDs of the GPUs in error. Above
// maxPackingNUMANodes nodes it takes the lowest IDs.
func packByNUMA(free []rankedGPU, l numaLayout, n int, unknownReason string) ([]device.Device, string) {
	var known, unknown, faulty []rankedGPU
	for _, g := range free {
		switch {
		case g.faulty:
			faulty = append(faulty, g)
		case g.numa < 0:
			unknown = append(unknown, g)
		default:
			known = append(known, g)
		}
	}
	rule := "NUMA packing; " + l.describe()
	if len(faulty) > 0 {
		rule += "; in error: " + idList(gpuDevices(faulty))
	}

	if n >= len(known) {
		set := append([]rankedGPU{}, known...)
		set = append(set, unknown[:min(n-len(set), len(unknown))]...)
		set = append(set, faulty[:n-len(set)]...)
		if len(l.nodes) == 0 {
			rule = "lowest free IDs: " + noPackingReason(unknownReason, len(unknown), len(faulty))
			if len(faulty) > 0 {
				rule += "; in error last: " + idList(gpuDevices(faulty))
			}
		}
		return sortedDevices(set), rule
	}
	if len(l.nodes) > maxPackingNUMANodes {
		return sortedDevices(known[:n]), fmt.Sprintf(
			"lowest free IDs: more than %d NUMA nodes", maxPackingNUMANodes)
	}

	var best []rankedGPU
	var bestKey packingKey
	consider := func(set []rankedGPU) {
		k := l.key(set)
		if best == nil || k.compare(bestKey) < 0 {
			best, bestKey = set, k
		}
	}
	m := len(l.nodes)
	for mask := 0; mask < 1<<m; mask++ {
		var base []rankedGPU
		for i, node := range l.nodes {
			if mask&(1<<i) != 0 {
				base = append(base, l.free[node]...)
			}
		}
		if len(base) > n {
			continue
		}
		if len(base) == n {
			consider(base)
			continue
		}
		for i, node := range l.nodes {
			if mask&(1<<i) != 0 || len(l.free[node]) < n-len(base) {
				continue
			}
			set := append(append([]rankedGPU{}, base...), l.free[node][:n-len(base)]...)
			consider(set)
		}
	}
	return sortedDevices(best), rule
}

// noPackingReason says why an agent has no free healthy GPU with a known NUMA node: its topology
// is unknown, every free GPU is in error, or no free healthy GPU reports a NUMA node (CPU slots
// included).
func noPackingReason(unknownReason string, unknown, faulty int) string {
	switch {
	case unknownReason != "":
		return unknownReason
	case faulty > 0 && unknown == 0:
		return "every free GPU in error"
	default:
		return noKnownNUMANode
	}
}

// describe lists the free healthy GPUs per NUMA node, for example "free per NUMA node 0:4 1:3".
func (l numaLayout) describe() string {
	parts := make([]string, 0, len(l.nodes))
	for _, node := range l.nodes {
		parts = append(parts, fmt.Sprintf("%d:%d", node, len(l.free[node])))
	}
	if len(parts) == 0 {
		return noKnownNUMANode
	}
	return "free per NUMA node " + strings.Join(parts, " ")
}

// pairRank is the rank of a pair of GPUs for prefer_gpu_topology; smaller is better, compared
// element by element. The first element is the band:
//   - 0: P2P usable with NVLinks, more NVLinks first;
//   - 1: P2P usable, by level: INTERNAL, PIX, PXB, PHB, NODE, SYS, unknown level;
//   - 2: P2P not usable or unknown: by NUMA class (one NUMA node, INTERNAL to NODE; SYS; unknown
//     level), then not usable before unknown P2P, then a pair on different PCIe switches before a
//     pair behind one switch (PIX), whose GPUs share one uplink to host memory.
//
// A pair missing from the report is unknown in every field, the last row. NVLinks count only with
// usable P2P, and link width is not used.
type pairRank [4]int

// unknownPairRank is the last row of band 2: unknown level and unknown P2P.
var unknownPairRank = pairRank{2, 2, 1, 0}

func comparePairRanks(a, b pairRank) int {
	for i := range a {
		if a[i] != b[i] {
			return cmpInt(a[i], b[i])
		}
	}
	return 0
}

var usableLevelOrder = map[aproto.GPULinkLevel]int{
	aproto.GPULinkLevelInternal: 0,
	aproto.GPULinkLevelPIX:      1,
	aproto.GPULinkLevelPXB:      2,
	aproto.GPULinkLevelPHB:      3,
	aproto.GPULinkLevelNode:     4,
	aproto.GPULinkLevelSys:      5,
}

// pair returns what the agent reported for the pair of a and b, and whether it reported it.
func (g *gpuTopology) pair(a, b device.ID) (gpuPair, bool) {
	if b < a {
		a, b = b, a
		p, ok := g.pairs[gpuPairKey{a: a, b: b}]
		p.p2pAToB, p.p2pBToA = p.p2pBToA, p.p2pAToB
		return p, ok
	}
	p, ok := g.pairs[gpuPairKey{a: a, b: b}]
	return p, ok
}

func (p gpuPair) p2p() aproto.GPUP2PUsability {
	return aproto.P2PUsability(aproto.GPULink{P2PAToB: p.p2pAToB, P2PBToA: p.p2pBToA})
}

func (g *gpuTopology) pairRank(a, b device.ID) pairRank {
	p, ok := g.pair(a, b)
	if !ok {
		return unknownPairRank
	}
	switch p.p2p() {
	case aproto.GPUP2PUsable:
		if p.nvlinks > 0 {
			return pairRank{0, -p.nvlinks}
		}
		if order, known := usableLevelOrder[p.level]; known {
			return pairRank{1, order}
		}
		return pairRank{1, len(usableLevelOrder)}
	case aproto.GPUP2PNotUsable:
		return pairRank{2, numaClass(p.level), 0, pixRank(p.level)}
	default:
		return pairRank{2, numaClass(p.level), 1, pixRank(p.level)}
	}
}

// numaClass is 0 for a pair on one NUMA node, 1 for SYS and 2 for an unknown level. NVML's NODE and
// SYS are NUMA-based: they equal one socket and two sockets only with NPS1.
func numaClass(level aproto.GPULinkLevel) int {
	switch level {
	case aproto.GPULinkLevelInternal, aproto.GPULinkLevelPIX, aproto.GPULinkLevelPXB,
		aproto.GPULinkLevelPHB, aproto.GPULinkLevelNode:
		return 0
	case aproto.GPULinkLevelSys:
		return 1
	default:
		return 2
	}
}

func pixRank(level aproto.GPULinkLevel) int {
	if level == aproto.GPULinkLevelPIX {
		return 1
	}
	return 0
}

// describePair describes a pair for the task log, for example "NODE, P2P usable".
func (g *gpuTopology) describePair(a, b device.ID) string {
	p, ok := g.pair(a, b)
	if !ok {
		return "not reported"
	}
	level := string(p.level)
	if !p.level.Known() {
		level = "level unknown"
	}
	switch p.p2p() {
	case aproto.GPUP2PUsable:
		if p.nvlinks > 0 {
			return fmt.Sprintf("%s, %d NVLinks, P2P usable", level, p.nvlinks)
		}
		return level + ", P2P usable"
	case aproto.GPUP2PNotUsable:
		return level + ", P2P not usable"
	default:
		return level + ", P2P unknown"
	}
}

// selectByTopology returns the set of n free GPUs that prefer_gpu_topology ranks first, sorted by
// ID, with its worst pair described. A set's key is the number of its GPUs in error, then its
// C(n,2) pair ranks sorted worst first, compared lexicographically. Exact ties go to the packing
// key when tie is set (NUMA packing), then to the smallest sorted list of IDs.
//
// It returns no set, with the reason, when the topology is unknown, when every pair of free GPUs is
// unknown in every field (the report holds nothing to rank), or above maxTopologySets sets.
func selectByTopology(
	free []rankedGPU, g *gpuTopology, n int, tie *numaLayout,
) (set []device.Device, worstPair string, unranked string) {
	switch {
	case topologyUnknownReason(g) != "":
		return nil, "", "topology unknown: " + topologyUnknownReason(g)
	case n < 2 || len(free) < n:
		return nil, "", "fewer than 2 slots"
	}

	f := len(free)
	ranks := make([][]pairRank, f)
	rankable := false
	for i := range ranks {
		ranks[i] = make([]pairRank, f)
		for j := range ranks[i] {
			if i == j {
				continue
			}
			ranks[i][j] = g.pairRank(free[i].device.ID, free[j].device.ID)
			if ranks[i][j] != unknownPairRank {
				rankable = true
			}
		}
	}
	if !rankable {
		return nil, "", "every pair of free GPUs unknown"
	}
	if binomial(f, n) > maxTopologySets {
		return nil, "", fmt.Sprintf("more than %d sets of free GPUs", maxTopologySets)
	}

	type setKey struct {
		faulty int
		pairs  []pairRank
	}
	// keyOf fills k with the key of the set idx, reusing its slice.
	keyOf := func(k *setKey, idx []int) {
		k.faulty, k.pairs = 0, k.pairs[:0]
		for a, i := range idx {
			if free[i].faulty {
				k.faulty++
			}
			for _, j := range idx[a+1:] {
				k.pairs = append(k.pairs, ranks[i][j])
			}
		}
		slices.SortFunc(k.pairs, func(x, y pairRank) int { return comparePairRanks(y, x) })
	}
	compareKeys := func(a, b setKey) int {
		if a.faulty != b.faulty {
			return cmpInt(a.faulty, b.faulty)
		}
		for x := range a.pairs {
			if c := comparePairRanks(a.pairs[x], b.pairs[x]); c != 0 {
				return c
			}
		}
		return 0
	}
	gpus := make([]rankedGPU, n)
	gpusOf := func(idx []int) []rankedGPU {
		for x, i := range idx {
			gpus[x] = free[i]
		}
		return gpus
	}

	// Combinations in lexicographic order of sorted IDs: keeping only a strictly better set makes
	// the smallest IDs win exact ties. The best set's packing key is computed once, at its first
	// tie.
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	best := make([]int, n)
	found := false
	pairs := n * (n - 1) / 2
	candKey := setKey{pairs: make([]pairRank, 0, pairs)}
	bestKey := setKey{pairs: make([]pairRank, 0, pairs)}
	var bestPack *packingKey
	for {
		keyOf(&candKey, idx)
		c := -1
		var candPack *packingKey
		if found {
			c = compareKeys(candKey, bestKey)
			if c == 0 && tie != nil {
				if bestPack == nil {
					k := tie.key(gpusOf(best))
					bestPack = &k
				}
				k := tie.key(gpusOf(idx))
				candPack = &k
				c = candPack.compare(*bestPack)
			}
		}
		if c < 0 {
			found = true
			copy(best, idx)
			candKey, bestKey = bestKey, candKey
			bestPack = candPack
		}
		i := n - 1
		for i >= 0 && idx[i] == f-n+i {
			i--
		}
		if i < 0 {
			break
		}
		idx[i]++
		for j := i + 1; j < n; j++ {
			idx[j] = idx[j-1] + 1
		}
	}

	chosen := append([]rankedGPU(nil), gpusOf(best)...)
	worstA, worstB := chosen[0].device.ID, chosen[1].device.ID
	worst := ranks[best[0]][best[1]]
	for a := range best {
		for _, j := range best[a+1:] {
			if comparePairRanks(ranks[best[a]][j], worst) > 0 {
				worst = ranks[best[a]][j]
				worstA, worstB = free[best[a]].device.ID, free[j].device.ID
			}
		}
	}
	return sortedDevices(chosen), "worst pair " + g.describePair(worstA, worstB), ""
}

func binomial(n, k int) int {
	if k < 0 || k > n {
		return 0
	}
	k = min(k, n-k)
	result := 1
	for i := 1; i <= k; i++ {
		result = result * (n - k + i) / i
		if result > maxTopologySets {
			return result
		}
	}
	return result
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareIDs(a, b []device.ID) int {
	for i := range a {
		if i >= len(b) {
			return 1
		}
		if a[i] != b[i] {
			return cmpInt(int(a[i]), int(b[i]))
		}
	}
	return cmpInt(len(a), len(b))
}

func gpuDevices(set []rankedGPU) []device.Device {
	out := make([]device.Device, len(set))
	for i, g := range set {
		out[i] = g.device
	}
	return out
}

func sortedDevices(set []rankedGPU) []device.Device {
	out := gpuDevices(set)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// idList lists device IDs, for example "4,5,6,7".
func idList(devices []device.Device) string {
	ids := make([]string, len(devices))
	for i, d := range devices {
		ids[i] = strconv.Itoa(int(d.ID))
	}
	return strings.Join(ids, ",")
}
