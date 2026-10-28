package expconf

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas"
)

func gpuGroupRequests(groups ...GPUGroupRequest) GPUGroupRequests { return groups }

func TestGPUGroupsCheck(t *testing.T) {
	sim := GPUGroupRequest{Name: "simulation", Count: 1, Models: []string{"NVIDIA GeForce RTX 4090"}}
	learner := GPUGroupRequest{Name: "learner", Count: 2, Models: []string{"NVIDIA RTX 6000 Ada Generation"}}
	require.NoError(t, gpuGroupRequests(sim, learner).Check(3, "resources.slots_per_trial"))
	require.NoError(t, GPUGroupRequests(nil).Check(0, "resources.slots"))

	for _, c := range []struct {
		groups GPUGroupRequests
		slots  int
		field  string
		want   string
	}{
		{
			gpuGroupRequests(sim, learner), 2, "resources.slots_per_trial",
			"resources.gpu_groups counts must add up to resources.slots_per_trial (2)",
		},
		{gpuGroupRequests(sim), 0, "resources.slots", "resources.gpu_groups counts must add up to resources.slots (0)"},
		{GPUGroupRequests{}, 0, "resources.slots", "resources.gpu_groups must not be empty"},
		{
			gpuGroupRequests(GPUGroupRequest{Name: "Sim", Count: 1, Models: []string{"x"}}), 1, "resources.slots",
			`resources.gpu_groups name "Sim" must match ^[a-z][a-z0-9_-]{0,31}$`,
		},
		{
			gpuGroupRequests(GPUGroupRequest{Count: 1, Models: []string{"x"}}), 1, "resources.slots",
			`resources.gpu_groups name "" must match ^[a-z][a-z0-9_-]{0,31}$`,
		},
		{gpuGroupRequests(sim, sim), 2, "resources.slots", `resources.gpu_groups name "simulation" is not unique`},
		{
			gpuGroupRequests(GPUGroupRequest{Name: "a", Models: []string{"x"}}), 0, "resources.slots",
			"resources.gpu_groups a: count must be at least 1",
		},
		{
			gpuGroupRequests(GPUGroupRequest{Name: "a", Count: 1}), 1, "resources.slots",
			"resources.gpu_groups a: models must not be empty",
		},
		{
			gpuGroupRequests(GPUGroupRequest{Name: "a", Count: 1, Models: []string{"x", "x"}}), 1, "resources.slots",
			"resources.gpu_groups a: models must be unique",
		},
		{
			gpuGroupRequests(GPUGroupRequest{Name: "a", Count: 1, Models: []string{""}}), 1, "resources.slots",
			"resources.gpu_groups a: a model must not be empty",
		},
		{
			// The counts' sum wraps around to 2 in an int.
			gpuGroupRequests(
				GPUGroupRequest{Name: "a", Count: 1 << 62, Models: []string{"x"}},
				GPUGroupRequest{Name: "b", Count: 1 << 62, Models: []string{"x"}},
				GPUGroupRequest{Name: "c", Count: 1 << 62, Models: []string{"x"}},
				GPUGroupRequest{Name: "d", Count: 1 << 62, Models: []string{"x"}},
				GPUGroupRequest{Name: "e", Count: 2, Models: []string{"x"}},
			), 2, "resources.slots",
			"resources.gpu_groups counts must add up to resources.slots (2)",
		},
	} {
		require.EqualError(t, c.groups.Check(c.slots, c.field), c.want)
	}
}

func TestGPUGroupsRequests(t *testing.T) {
	require.Nil(t, ResourcesConfigV0{}.GPUGroupRequests())
	config := GPUGroupsConfigV0{{RawName: "sim", RawCount: 2, RawModels: []string{"a", "b"}}}
	got := ResourcesConfigV0{RawGPUGroups: config}.GPUGroupRequests()
	require.Equal(t, GPUGroupRequests{{Name: "sim", Count: 2, Models: []string{"a", "b"}}}, got)
	require.Equal(t, 2, got.Slots())
	require.Equal(t, "sim: 2 × a or b", got.String())
	got[0].Models[0] = "changed"
	require.Equal(t, "a", config[0].RawModels[0], "a copy")
	require.Equal(t, GPUGroupRequests{}, GPUGroupsConfigV0{}.Requests())
}

func TestGPUGroupsJSON(t *testing.T) {
	// Unset leaves no key in a stored config.
	bytes0, err := json.Marshal(ResourcesConfigV0{RawSlotsPerTrial: ptrs.Ptr(1)})
	require.NoError(t, err)
	require.NotContains(t, string(bytes0), "gpu_groups")

	var r ResourcesConfigV0
	require.NoError(t, json.Unmarshal([]byte(
		`{"slots_per_trial": 2, "gpu_groups": [{"name": "sim", "count": 2, "models": ["m"]}]}`), &r))
	require.Equal(t, GPUGroupRequests{{Name: "sim", Count: 2, Models: []string{"m"}}}, r.GPUGroupRequests())
	stored, err := json.Marshal(r)
	require.NoError(t, err)
	require.Contains(t, string(stored), `"gpu_groups":[{"name":"sim","count":2,"models":["m"]}]`)

	// An unknown key is refused, also without DisallowUnknownFields.
	err = json.Unmarshal([]byte(`{"gpu_groups": [{"name": "sim", "count": 1, "models": ["m"], "x": 1}]}`), &r)
	require.ErrorContains(t, err, `gpu_groups: json: unknown field "x"`)

	// The list decodes into a new slice: an entry that leaves out a key does not keep the value of
	// the entry it replaces, as a command's config decoded over its template would.
	over := ResourcesConfigV0{RawGPUGroups: GPUGroupsConfigV0{
		{RawName: "a", RawCount: 1, RawModels: []string{"template"}},
		{RawName: "b", RawCount: 1, RawModels: []string{"template"}},
	}}
	dec := json.NewDecoder(bytes.NewReader([]byte(`{"gpu_groups": [{"name": "c", "count": 2}]}`)))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&over))
	require.Equal(t, GPUGroupsConfigV0{{RawName: "c", RawCount: 2}}, over.RawGPUGroups)

	// null clears it.
	require.NoError(t, json.Unmarshal([]byte(`{"gpu_groups": null}`), &over))
	require.Nil(t, over.RawGPUGroups)
}

func TestGPUGroupsCopyAndMerge(t *testing.T) {
	config := GPUGroupsConfigV0{{RawName: "sim", RawCount: 1, RawModels: []string{"a"}}}
	copied := schemas.Copy(config)
	copied[0].RawModels[0] = "b"
	require.Equal(t, "a", config[0].RawModels[0])
	require.Nil(t, schemas.Copy(GPUGroupsConfigV0(nil)))

	template := ResourcesConfigV0{RawGPUGroups: GPUGroupsConfigV0{
		{RawName: "t", RawCount: 1, RawModels: []string{"x"}},
		{RawName: "u", RawCount: 1, RawModels: []string{"y"}},
	}}
	merged := schemas.Merge(ResourcesConfigV0{RawGPUGroups: config}, template)
	require.Equal(t, config, merged.RawGPUGroups, "the config's list replaces the template's")
	merged = schemas.Merge(ResourcesConfigV0{}, template)
	require.Equal(t, template.RawGPUGroups, merged.RawGPUGroups)
	require.Nil(t, schemas.WithDefaults(ResourcesConfigV0{}).RawGPUGroups)
}
