package internal

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
)

const (
	// More recorded GPU sets than this leaves every GPU without an in-container index.
	taskResourceMaxGPUSets = 4096
	// At most this many GPUs without a fetched series are looked up, in one extra query.
	taskResourceMaxGPULookups = 64
)

var (
	// Only plain UUIDs go into the lookup query; anything else stays without a bus ID.
	taskResourceGPUUUIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,128}$`)
	// domain:bus:device.function, as DCGM ("00000000:01:00.0") or sysfs ("0000:01:00.0") write it.
	taskResourcePCIBusIDPattern = regexp.MustCompile(
		`^([0-9a-f]{1,8}):([0-9a-f]{1,2}):([0-9a-f]{1,2})\.([0-7])$`)
)

// taskResourceGPUInfo is what DCGM reports about one GPU. A GPU reported with two different
// bus IDs or nodes in the range is a conflict and is never numbered.
type taskResourceGPUInfo struct {
	busID    string
	node     string
	conflict bool
}

type taskResourceGPUKey struct {
	allocationID string
	gpuUUID      string
}

// queryTaskResourceGPUSets reads the GPU sets the task's containers recorded for the given
// allocations. Each row is one container's GPUs as nvidia-smi inside it listed them; only
// trials, notebooks and shells record them (prep_container --resources).
func queryTaskResourceGPUSets(
	ctx context.Context, taskID string, allocationIDs []string,
) ([]model.AcceleratorData, error) {
	sets := []model.AcceleratorData{}
	err := db.Bun().NewRaw(`
SELECT aa.allocation_id, aa.container_id, aa.accelerator_uuids
FROM allocation_accelerators aa
JOIN allocations a ON a.allocation_id = aa.allocation_id
WHERE a.task_id = ? AND aa.allocation_id IN (?)
ORDER BY aa.id ASC
LIMIT ?`, taskID, bun.In(allocationIDs), taskResourceMaxGPUSets+1).Scan(ctx, &sets)
	if err != nil {
		return nil, fmt.Errorf("reading task GPU sets: %w", err)
	}
	if len(sets) > taskResourceMaxGPUSets {
		return nil, fmt.Errorf("task has more than %d GPU sets", taskResourceMaxGPUSets)
	}
	return sets, nil
}

// pciBusIDKey normalises a PCI bus ID so that string order is bus order, whatever the case
// and the width of the domain. ok is false for anything that is not a PCI bus ID.
func pciBusIDKey(id string) (key string, ok bool) {
	m := taskResourcePCIBusIDPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(id)))
	if m == nil {
		return "", false
	}
	parts := make([]uint64, 4)
	for i := range parts {
		v, err := strconv.ParseUint(m[i+1], 16, 32)
		if err != nil {
			return "", false
		}
		parts[i] = v
	}
	return fmt.Sprintf("%08x:%02x:%02x.%x", parts[0], parts[1], parts[2], parts[3]), true
}

// addTaskResourceGPUInfo records the DCGM labels of one result for its GPU.
func addTaskResourceGPUInfo(gpus map[string]taskResourceGPUInfo, metric map[string]string) {
	uuid := metric["gpu_uuid"]
	if uuid == "" {
		return
	}
	info := taskResourceGPUInfo{busID: metric["pci_bus_id"], node: metric["node"]}
	if prev, ok := gpus[uuid]; ok {
		prevKey, prevValid := pciBusIDKey(prev.busID)
		key, valid := pciBusIDKey(info.busID)
		info.conflict = prev.conflict || prevValid != valid || prevKey != key || prev.node != info.node
	}
	gpus[uuid] = info
}

// taskResourceGPUInfoQuery reads the bus ID and node of GPUs without a fetched series.
func taskResourceGPUInfoQuery(cluster string, uuids []string) string {
	quoted := make([]string, len(uuids))
	for i, uuid := range uuids {
		quoted[i] = regexp.QuoteMeta(uuid)
	}
	return `group by (det_cluster,gpu_uuid,pci_bus_id,node) (DCGM_FI_DEV_GPU_UTIL{job="dcgm",det_cluster=` +
		promLabel(cluster) + `,gpu_uuid=~` + promLabel(strings.Join(quoted, "|")) + `})`
}

// setTaskResourceGPUIndexes sets gpu_index on GPU series: the number nvidia-smi inside the
// task's container shows for the GPU. Without a complete GPU set and bus IDs for the GPU's
// container, the series keeps no index; a failed lookup never fails the response.
func setTaskResourceGPUIndexes(ctx context.Context, cluster, taskID string, r taskResourceRange,
	series []taskResourceSeries, gpus map[string]taskResourceGPUInfo, deps taskResourceDependencies,
) {
	if deps.gpuSets == nil {
		return
	}
	seen := map[string]bool{}
	allocationIDs := []string{}
	for _, s := range series {
		id := s.Labels.AllocationID
		if strings.HasPrefix(s.Metric, "gpu_") && s.Labels.GPUUUID != "" && id != "" && !seen[id] {
			seen[id] = true
			allocationIDs = append(allocationIDs, id)
		}
	}
	if len(allocationIDs) == 0 {
		return
	}
	sort.Strings(allocationIDs)
	sets, err := deps.gpuSets(ctx, taskID, allocationIDs)
	if err != nil {
		log.WithError(err).Warn("task resources: GPU sets are unavailable; GPUs keep their UUID labels")
		return
	}

	missing := []string{}
	wanted := map[string]bool{}
	for _, set := range sets {
		for _, uuid := range set.AcceleratorUuids {
			if _, ok := gpus[uuid]; !ok && !wanted[uuid] && taskResourceGPUUUIDPattern.MatchString(uuid) {
				wanted[uuid] = true
				missing = append(missing, uuid)
			}
		}
	}
	if len(missing) > 0 && len(missing) <= taskResourceMaxGPULookups {
		sort.Strings(missing)
		results, err := deps.query(ctx, taskResourceGPUInfoQuery(cluster, missing), r)
		if err == nil {
			for _, result := range results {
				if result.Metric["det_cluster"] == cluster && wanted[result.Metric["gpu_uuid"]] {
					addTaskResourceGPUInfo(gpus, result.Metric)
				}
			}
		}
	}

	indexes := taskResourceGPUIndexes(series, sets, gpus)
	for i := range series {
		labels := &series[i].Labels
		if index, ok := indexes[taskResourceGPUKey{labels.AllocationID, labels.GPUUUID}]; ok {
			labels.GPUIndex = &index
		}
	}
}

// taskResourceGPUIndexes numbers GPUs the way nvidia-smi inside the task's container does:
// by PCI bus ID among all GPUs of that container, which is one recorded GPU set. Every GPU
// series of an allocation on one node must belong to the same set, and every GPU of that set
// needs a known bus ID; otherwise none of that allocation's GPUs on that node is numbered.
func taskResourceGPUIndexes(series []taskResourceSeries, sets []model.AcceleratorData,
	gpus map[string]taskResourceGPUInfo,
) map[taskResourceGPUKey]int {
	const ambiguous = -1
	// The set each GPU belongs to; a GPU listed in two different sets of one allocation is
	// ambiguous. Identical rows of one container are the same set.
	owner := map[taskResourceGPUKey]int{}
	members := [][]string{}
	signatures := map[string]int{}
	for _, set := range sets {
		allocationID := string(set.AllocationID)
		uuids := append([]string(nil), set.AcceleratorUuids...)
		sort.Strings(uuids)
		duplicate := false
		for i := 1; i < len(uuids); i++ {
			duplicate = duplicate || uuids[i] == uuids[i-1]
		}
		if duplicate {
			for _, uuid := range uuids {
				owner[taskResourceGPUKey{allocationID, uuid}] = ambiguous
			}
			continue
		}
		signature := allocationID + "\x00" + strings.Join(uuids, "\x00")
		index, ok := signatures[signature]
		if !ok {
			index = len(members)
			signatures[signature] = index
			members = append(members, uuids)
		}
		for _, uuid := range uuids {
			key := taskResourceGPUKey{allocationID, uuid}
			if prev, seen := owner[key]; !seen {
				owner[key] = index
			} else if prev != index {
				owner[key] = ambiguous
			}
		}
	}

	// Ranks within each set, when every member has one distinct bus ID on one node.
	ranks := make([]map[string]int, len(members))
	nodes := make([]string, len(members))
	for i, uuids := range members {
		keys := make(map[string]string, len(uuids))
		complete := true
		for j, uuid := range uuids {
			info, ok := gpus[uuid]
			key, valid := pciBusIDKey(info.busID)
			if !ok || !valid || info.conflict || (j > 0 && info.node != nodes[i]) {
				complete = false
				break
			}
			nodes[i] = info.node
			keys[uuid] = key
		}
		if !complete {
			continue
		}
		ordered := append([]string(nil), uuids...)
		sort.Slice(ordered, func(a, b int) bool { return keys[ordered[a]] < keys[ordered[b]] })
		rank := make(map[string]int, len(ordered))
		for j, uuid := range ordered {
			if j > 0 && keys[uuid] == keys[ordered[j-1]] {
				rank = nil
				break
			}
			rank[uuid] = j
		}
		ranks[i] = rank
	}

	type group struct{ allocationID, node string }
	groups := map[group]int{}
	for _, s := range series {
		if !strings.HasPrefix(s.Metric, "gpu_") || s.Labels.GPUUUID == "" {
			continue
		}
		g := group{s.Labels.AllocationID, s.Labels.Node}
		set, ok := owner[taskResourceGPUKey{g.allocationID, s.Labels.GPUUUID}]
		if !ok || set == ambiguous || ranks[set] == nil || nodes[set] != g.node {
			set = ambiguous
		}
		if prev, seen := groups[g]; seen && prev != set {
			set = ambiguous
		}
		groups[g] = set
	}

	indexes := map[taskResourceGPUKey]int{}
	for _, s := range series {
		if !strings.HasPrefix(s.Metric, "gpu_") || s.Labels.GPUUUID == "" {
			continue
		}
		set := groups[group{s.Labels.AllocationID, s.Labels.Node}]
		if set != ambiguous {
			key := taskResourceGPUKey{s.Labels.AllocationID, s.Labels.GPUUUID}
			indexes[key] = ranks[set][s.Labels.GPUUUID]
		}
	}
	return indexes
}
