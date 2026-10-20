package config

import (
	"testing"

	"github.com/ghodss/yaml"
	"github.com/stretchr/testify/require"
)

func TestResourcePoolAccessConfig(t *testing.T) {
	c := DefaultConfig()
	require.False(t, c.ResourcePoolAccess.HideUnusablePools)

	raw := `
resource_pool_access:
  hide_unusable_pools: true
`
	require.NoError(t, yaml.Unmarshal([]byte(raw), c, yaml.DisallowUnknownFields))
	require.True(t, c.ResourcePoolAccess.HideUnusablePools)

	for _, bad := range []string{
		"resource_pool_access:\n  hide_unusable_pools: sometimes\n",
		"resource_pool_access:\n  hide_restricted_pools: true\n",
		"resource_pool_access: true\n",
	} {
		require.Error(t, yaml.Unmarshal([]byte(bad), DefaultConfig(), yaml.DisallowUnknownFields),
			bad)
	}
}
