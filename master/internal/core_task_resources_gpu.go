package internal

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"

	"github.com/determined-ai/determined/master/internal/db"
)

// More recorded GPU lists than this leaves every GPU without an in-container index.
const taskResourceMaxGPUSets = 4096

// taskResourceGPUSet is the GPUs one container recorded at start (prep_container --resources),
// in nvidia-smi index order inside the container, with the slots the master assigned to the
// whole allocation.
type taskResourceGPUSet struct {
	AllocationID string   `bun:"allocation_id"`
	ContainerID  string   `bun:"container_id"`
	UUIDs        []string `bun:"accelerator_uuids,array"`
	Slots        int      `bun:"slots"`
}

type taskResourceGPUKey struct {
	allocationID string
	gpuUUID      string
}

// queryTaskResourceGPUSets reads the GPU lists the task's containers recorded for the given
// allocations, with each allocation's slots. Only trials, notebooks and shells record them.
func queryTaskResourceGPUSets(
	ctx context.Context, taskID string, allocationIDs []string,
) ([]taskResourceGPUSet, error) {
	sets := []taskResourceGPUSet{}
	err := db.Bun().NewRaw(`
SELECT aa.allocation_id, aa.container_id, aa.accelerator_uuids, a.slots
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

// setTaskResourceGPUIndexes sets gpu_index on the GPU series that taskResourceGPUIndexes can
// number. A failed read never fails the response.
func setTaskResourceGPUIndexes(ctx context.Context, taskID string, series []taskResourceSeries,
	deps taskResourceDependencies,
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
	indexes := taskResourceGPUIndexes(series, sets)
	for i := range series {
		labels := &series[i].Labels
		if index, ok := indexes[taskResourceGPUKey{labels.AllocationID, labels.GPUUUID}]; ok {
			labels.GPUIndex = &index
		}
	}
}

// taskResourceGPUIndexes numbers GPUs as nvidia-smi inside the task's container does: by their
// position in the list that container recorded. A list can be partial (nvidia-smi lines that
// fail to parse are skipped), so an allocation's GPUs are numbered only when its containers'
// lists add up to the allocation's slots, no GPU is named twice in them (in one list or by two
// containers), and no container has rows with different lists. An allocation's GPUs on one node
// are also left unnumbered when a GPU series is not in a list, when the series come from more
// than one list, or when a GPU of that list is reported on another node.
func taskResourceGPUIndexes(series []taskResourceSeries, sets []taskResourceGPUSet,
) map[taskResourceGPUKey]int {
	type container struct{ allocationID, containerID string }
	lists := map[container][]string{}
	slots := map[string]int{}
	// A broken recording leaves its allocation's count unreliable, so it voids the allocation.
	broken := map[string]bool{}
	for _, set := range sets {
		c := container{set.AllocationID, set.ContainerID}
		slots[set.AllocationID] = set.Slots
		if prev, ok := lists[c]; ok {
			broken[c.allocationID] = broken[c.allocationID] || !slices.Equal(prev, set.UUIDs)
			continue
		}
		lists[c] = set.UUIDs
	}

	// The count, and the list and position of each GPU, of an allocation.
	recorded := map[string]int{}
	owner := map[taskResourceGPUKey]container{}
	position := map[taskResourceGPUKey]int{}
	for c, uuids := range lists {
		recorded[c.allocationID] += len(uuids)
		for i, uuid := range uuids {
			key := taskResourceGPUKey{c.allocationID, uuid}
			if _, named := owner[key]; named {
				broken[c.allocationID] = true
			}
			owner[key], position[key] = c, i
		}
	}

	// A GPU UUID is one device, so its series must all name one node.
	nodes := map[string]string{}
	conflicts := map[string]bool{}
	for _, s := range series {
		if strings.HasPrefix(s.Metric, "gpu_") && s.Labels.GPUUUID != "" {
			uuid := s.Labels.GPUUUID
			if node, ok := nodes[uuid]; ok && node != s.Labels.Node {
				conflicts[uuid] = true
			}
			nodes[uuid] = s.Labels.Node
		}
	}

	type group struct{ allocationID, node string }
	type verdict struct {
		list container
		ok   bool
	}
	groups := map[group]verdict{}
	for _, s := range series {
		if !strings.HasPrefix(s.Metric, "gpu_") || s.Labels.GPUUUID == "" {
			continue
		}
		g := group{s.Labels.AllocationID, s.Labels.Node}
		key := taskResourceGPUKey{g.allocationID, s.Labels.GPUUUID}
		c, listed := owner[key]
		ok := listed && !broken[g.allocationID] && recorded[g.allocationID] == slots[g.allocationID]
		for _, uuid := range lists[c] {
			if node, seen := nodes[uuid]; seen && (node != g.node || conflicts[uuid]) {
				ok = false
			}
		}
		if prev, seen := groups[g]; seen {
			ok = ok && prev.ok && prev.list == c
		}
		groups[g] = verdict{list: c, ok: ok}
	}

	indexes := map[taskResourceGPUKey]int{}
	for _, s := range series {
		if strings.HasPrefix(s.Metric, "gpu_") && s.Labels.GPUUUID != "" &&
			groups[group{s.Labels.AllocationID, s.Labels.Node}].ok {
			key := taskResourceGPUKey{s.Labels.AllocationID, s.Labels.GPUUUID}
			indexes[key] = position[key]
		}
	}
	return indexes
}
