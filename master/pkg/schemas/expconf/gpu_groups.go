package expconf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// GPUGroupsConfigV0 is resources.gpu_groups: named groups of GPUs of one task, by exact model, all
// on one agent.
//
//go:generate ../gen.sh
type GPUGroupsConfigV0 []GPUGroupV0

// UnmarshalJSON decodes into a new list, refusing unknown keys. encoding/json would decode each
// entry over the entry at the same index, so an entry that leaves out a key would keep a
// template's value for it in a command's config.
func (g *GPUGroupsConfigV0) UnmarshalJSON(data []byte) error {
	// null leaves the pointer nil.
	var out *[]GPUGroupV0
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return fmt.Errorf("gpu_groups: %w", err)
	}
	if out == nil {
		*g = nil
		return nil
	}
	*g = *out
	return nil
}

// GPUGroupV0 is one group of resources.gpu_groups: count GPUs whose model is one of models.
//
//go:generate ../gen.sh
type GPUGroupV0 struct {
	RawName   string   `json:"name"`
	RawCount  int      `json:"count"`
	RawModels []string `json:"models"`
}

// GPUGroupRequest is one group of resources.gpu_groups as the scheduler reads it.
type GPUGroupRequest struct {
	Name  string
	Count int
	// Models match a GPU's model name (device.Device.Brand) exactly.
	Models []string
}

// GPUGroupRequests are a task's resources.gpu_groups in config order, or nil when it has none.
type GPUGroupRequests []GPUGroupRequest

// Requests returns the groups as the scheduler reads them, a copy, or nil when gpu_groups is not
// set.
func (g GPUGroupsConfigV0) Requests() GPUGroupRequests {
	if g == nil {
		return nil
	}
	out := make(GPUGroupRequests, 0, len(g))
	for _, group := range g {
		out = append(out, GPUGroupRequest{
			Name:   group.RawName,
			Count:  group.RawCount,
			Models: append([]string(nil), group.RawModels...),
		})
	}
	return out
}

// GPUGroupRequests returns resources.gpu_groups, nil when it is not set.
func (r ResourcesConfigV0) GPUGroupRequests() GPUGroupRequests {
	return r.RawGPUGroups.Requests()
}

// Slots returns the number of GPUs of all groups, or -1, which no task's slots equal, when a count
// is negative or the sum does not fit in an int.
func (g GPUGroupRequests) Slots() int {
	n := 0
	for _, group := range g {
		if group.Count < 0 || group.Count > math.MaxInt-n {
			return -1
		}
		n += group.Count
	}
	return n
}

// String describes the groups, for example "simulation: 1 × NVIDIA GeForce RTX 4090; learner: 1 ×
// NVIDIA H100 80GB HBM3".
func (g GPUGroupRequests) String() string {
	parts := make([]string, 0, len(g))
	for _, group := range g {
		parts = append(parts, fmt.Sprintf("%s: %d × %s", group.Name, group.Count,
			strings.Join(group.Models, " or ")))
	}
	return strings.Join(parts, "; ")
}

var gpuGroupName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Check refuses groups that the schema refuses, which commands, notebooks, shells, TensorBoards
// and generic tasks are not checked against, and groups whose counts do not add up to the task's
// slots, given with the name of their field (slotsField). Unset groups pass.
func (g GPUGroupRequests) Check(slots int, slotsField string) error {
	if g == nil {
		return nil
	}
	if len(g) == 0 {
		return fmt.Errorf("resources.gpu_groups must not be empty")
	}
	names := map[string]bool{}
	for _, group := range g {
		switch {
		case !gpuGroupName.MatchString(group.Name):
			return fmt.Errorf("resources.gpu_groups name %q must match %s", group.Name, gpuGroupName)
		case names[group.Name]:
			return fmt.Errorf("resources.gpu_groups name %q is not unique", group.Name)
		case group.Count < 1:
			return fmt.Errorf("resources.gpu_groups %s: count must be at least 1", group.Name)
		case len(group.Models) == 0:
			return fmt.Errorf("resources.gpu_groups %s: models must not be empty", group.Name)
		}
		names[group.Name] = true
		models := map[string]bool{}
		for _, m := range group.Models {
			switch {
			case m == "":
				return fmt.Errorf("resources.gpu_groups %s: a model must not be empty", group.Name)
			case models[m]:
				return fmt.Errorf("resources.gpu_groups %s: models must be unique", group.Name)
			}
			models[m] = true
		}
	}
	if g.Slots() != slots {
		return fmt.Errorf("resources.gpu_groups counts must add up to %s (%d)", slotsField, slots)
	}
	return nil
}
