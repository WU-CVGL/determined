package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ghodss/yaml"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

var gpuGroupsJSON = `[{"name":"simulation","count":1,"models":["NVIDIA GeForce RTX 4090"]},` +
	`{"name":"learner","count":1,"models":["NVIDIA RTX 6000 Ada Generation"]}]`

var wantGPUGroups = expconf.GPUGroupRequests{
	{Name: "simulation", Count: 1, Models: []string{"NVIDIA GeForce RTX 4090"}},
	{Name: "learner", Count: 1, Models: []string{"NVIDIA RTX 6000 Ada Generation"}},
}

func TestCommandConfigGPUGroups(t *testing.T) {
	decode := func(config *CommandConfig, text string) error {
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		return dec.Decode(config)
	}

	// The merged command config is decoded strictly, as in the command API.
	config := DefaultConfig(nil)
	require.Nil(t, config.Resources.GPUGroupRequests())
	raw, err := json.Marshal(config)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "gpu_groups", "an unset key is not stored")
	require.NoError(t, decode(&config, `{"resources": {"slots": 2, "gpu_groups": `+gpuGroupsJSON+`}}`))
	require.Equal(t, wantGPUGroups, config.Resources.GPUGroupRequests())
	require.Equal(t, wantGPUGroups, config.Resources.ToExpconf().GPUGroupRequests())

	// A stored command snapshot keeps the groups.
	raw, err = json.Marshal(config)
	require.NoError(t, err)
	restored := DefaultConfig(nil)
	require.NoError(t, json.Unmarshal(raw, &restored))
	require.Equal(t, wantGPUGroups, restored.Resources.GPUGroupRequests())

	// A user's list replaces a template's, entry by entry too, and null clears it.
	require.NoError(t, decode(&config, `{"resources": {"gpu_groups": [{"name": "a", "count": 2}]}}`))
	require.Equal(t, expconf.GPUGroupRequests{{Name: "a", Count: 2}}, config.Resources.GPUGroupRequests())
	require.EqualError(t, config.Resources.GPUGroupRequests().Check(2, "resources.slots"),
		"resources.gpu_groups a: models must not be empty")
	require.NoError(t, decode(&config, `{"resources": {"gpu_groups": null}}`))
	require.Nil(t, config.Resources.GPUGroupRequests())

	require.ErrorContains(t, decode(&config,
		`{"resources": {"gpu_groups": [{"name": "a", "count": 1, "models": ["m"], "memory": 8}]}}`),
		`unknown field "memory"`)
	require.Error(t, decode(&config, `{"resources": {"gpu_groups": [{"name": "a", "count": "one"}]}}`))
}

func TestGenericTaskConfigGPUGroups(t *testing.T) {
	config := DefaultConfigGenericTaskConfig(nil)
	text := "entrypoint: [\"true\"]\nresources:\n  slots: 2\n  gpu_groups:\n" +
		"    - {name: simulation, count: 1, models: [NVIDIA GeForce RTX 4090]}\n" +
		"    - {name: learner, count: 1, models: [NVIDIA RTX 6000 Ada Generation]}\n"
	require.NoError(t, yaml.UnmarshalStrict([]byte(text), &config, yaml.DisallowUnknownFields))
	require.Equal(t, wantGPUGroups, config.Resources.GPUGroupRequests())
	config = DefaultConfigGenericTaskConfig(nil)
	require.Error(t, yaml.UnmarshalStrict([]byte(
		"resources:\n  gpu_groups:\n    - {name: a, count: 1, models: [m], memory: 8}\n"), &config,
		yaml.DisallowUnknownFields))
}

func TestParseJustResourcesSkipsGPUGroups(t *testing.T) {
	// A bad entry must not hide the pool and the slots, which sort after gpu_groups.
	for _, value := range []string{gpuGroupsJSON, `[{"name": 1}]`, `"x"`, `[{"name": "a", "extra": 1}]`} {
		r := ParseJustResources([]byte(`{"resources":{"gpu_groups":` + value + `,"resource_pool":"gpu","slots":4}}`))
		require.Equal(t, "gpu", r.ResourcePool, value)
		require.Equal(t, 4, r.Slots, value)
		require.Nil(t, r.GPUGroups, value)
	}
}
