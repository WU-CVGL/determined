//nolint:exhaustruct
package expconf

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas"
)

func TestGPUTopologyPreferenceJSON(t *testing.T) {
	for text, want := range map[string]GPUTopologyPreference{
		`false`: GPUTopologyOff, `"soft"`: GPUTopologySoft, `"strong"`: GPUTopologyStrong,
	} {
		var p GPUTopologyPreference
		require.NoError(t, json.Unmarshal([]byte(text), &p), text)
		require.Equal(t, want, p)
		raw, err := json.Marshal(p)
		require.NoError(t, err)
		require.Equal(t, text, string(raw))
	}
	// true must never silently mean one of the modes.
	for _, text := range []string{`true`, `"off"`, `"true"`, `1`, `{}`} {
		var p GPUTopologyPreference
		require.EqualError(t, json.Unmarshal([]byte(text), &p),
			`prefer_gpu_topology must be false, "soft" or "strong", not `+text, text)
	}
	_, err := json.Marshal(GPUTopologyPreference("bogus"))
	require.Error(t, err)
}

func TestResourcesConfigPreferGPUTopology(t *testing.T) {
	// Unset: off, and a defaulted config marshals without the key.
	var r ResourcesConfigV0
	require.NoError(t, json.Unmarshal([]byte(`{"slots_per_trial": 2}`), &r))
	require.Nil(t, r.PreferGPUTopology())
	require.Equal(t, GPUTopologyOff, r.GPUTopology())
	raw, err := json.Marshal(schemas.WithDefaults(r))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "prefer_gpu_topology")

	// null is unset too.
	require.NoError(t, json.Unmarshal([]byte(`{"prefer_gpu_topology": null}`), &r))
	require.Nil(t, r.RawPreferGPUTopology)

	require.NoError(t, json.Unmarshal([]byte(`{"prefer_gpu_topology": "soft"}`), &r))
	require.Equal(t, GPUTopologySoft, r.GPUTopology())
	require.NoError(t, json.Unmarshal([]byte(`{"prefer_gpu_topology": "strong"}`), &r))
	require.Equal(t, GPUTopologyStrong, r.GPUTopology())
	require.Error(t, json.Unmarshal([]byte(`{"prefer_gpu_topology": true}`), &r))

	// An explicit false wins over a template's soft; an invariant config policy, merged over the
	// user's config, can force soft.
	user := ResourcesConfigV0{RawPreferGPUTopology: ptrs.Ptr(GPUTopologyOff)}
	template := ResourcesConfigV0{RawPreferGPUTopology: ptrs.Ptr(GPUTopologySoft)}
	require.Equal(t, GPUTopologyOff, schemas.Merge(user, template).GPUTopology())
	require.Equal(t, GPUTopologySoft, schemas.Merge(ResourcesConfigV0{}, template).GPUTopology())
	require.Equal(t, GPUTopologySoft, schemas.Merge(template, user).GPUTopology())
	raw, err = json.Marshal(schemas.Merge(user, template))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"prefer_gpu_topology":false`)

	copied := schemas.Copy(template)
	require.Equal(t, GPUTopologySoft, copied.GPUTopology())
	require.NotSame(t, template.RawPreferGPUTopology, copied.RawPreferGPUTopology)

	// The same for a template's strong.
	strong := ResourcesConfigV0{RawPreferGPUTopology: ptrs.Ptr(GPUTopologyStrong)}
	require.Equal(t, GPUTopologyOff, schemas.Merge(user, strong).GPUTopology())
	require.Equal(t, GPUTopologyStrong, schemas.Merge(ResourcesConfigV0{}, strong).GPUTopology())
	require.Equal(t, GPUTopologySoft, schemas.Merge(template, strong).GPUTopology())
	copied = schemas.Copy(strong)
	require.Equal(t, GPUTopologyStrong, copied.GPUTopology())
	require.NotSame(t, strong.RawPreferGPUTopology, copied.RawPreferGPUTopology)
	raw, err = json.Marshal(schemas.WithDefaults(strong))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"prefer_gpu_topology":"strong"`)
}
