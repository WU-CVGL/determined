package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ghodss/yaml"
	"github.com/stretchr/testify/require"
)

func TestResourcePoolDefaults(t *testing.T) {
	resourcePoolDefault := dbConfig + `
resource_manager:
  type: agent
  scheduler:
    type: priority
    fitting_policy: best

resource_pools:
- pool_name: default
  scheduler:
    fitting_policy: worst
`
	unmarshaled := Config{}
	err := yaml.Unmarshal([]byte(resourcePoolDefault), &unmarshaled, yaml.DisallowUnknownFields)
	require.NoError(t, err)
	require.NoError(t, unmarshaled.Resolve())

	rm := unmarshaled.ResourceManagers()
	require.Len(t, rm, 1)
	rp := rm[0].ResourcePools
	require.Len(t, rp, 1)

	require.Equal(t, PriorityScheduling, rm[0].ResourceManager.AgentRM.Scheduler.GetType())
	require.Equal(t, PriorityScheduling, rp[0].Scheduler.GetType())
}

func TestSchedulerConfigNUMAPacking(t *testing.T) {
	for text, packs := range map[string]bool{
		"type: priority":                                            true,
		"type: priority\nnuma_packing: true":                        true,
		"type: priority\nnuma_packing: false":                       false,
		"type: priority\nfitting_policy: worst":                     false,
		"type: priority\nfitting_policy: worst\nnuma_packing: true": false,
	} {
		var s SchedulerConfig
		require.NoError(t, yaml.Unmarshal([]byte(text), &s, yaml.DisallowUnknownFields), text)
		require.Empty(t, s.Validate()[0], text)
		require.Equal(t, packs, s.PacksGPUsByNUMA(), text)

		// Unset stays unset: a config without the key marshals without it.
		raw, err := json.Marshal(s)
		require.NoError(t, err)
		require.Equal(t, strings.Contains(text, "numa_packing"), strings.Contains(string(raw), "numa_packing"),
			string(raw))
		var back SchedulerConfig
		require.NoError(t, json.Unmarshal(raw, &back))
		require.Equal(t, s, back)
	}

	var s SchedulerConfig
	require.Error(t, yaml.Unmarshal([]byte("type: priority\nnuma_packing: \"no\""), &s))
}

func TestMasterConfigNUMAPacking(t *testing.T) {
	text := dbConfig + `
resource_manager:
  type: agent
  scheduler:
    type: priority
    numa_packing: false

resource_pools:
- pool_name: default
- pool_name: packed
  scheduler:
    type: priority
    fitting_policy: best
`
	var c Config
	require.NoError(t, yaml.Unmarshal([]byte(text), &c, yaml.DisallowUnknownFields))
	require.NoError(t, c.Resolve())
	rm := c.ResourceManagers()[0]
	require.False(t, rm.ResourceManager.AgentRM.Scheduler.PacksGPUsByNUMA())
	require.Nil(t, rm.ResourcePools[0].Scheduler, "a pool without a scheduler inherits the global one")
	require.True(t, rm.ResourcePools[1].Scheduler.PacksGPUsByNUMA())
}
