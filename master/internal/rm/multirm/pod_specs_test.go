package multirm

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/resourcepoolv1"
)

// poolsRM is a resource manager of these pools that does not say whether it applies pod specs.
type poolsRM struct {
	rm.ResourceManager
	pools []string
}

func (r poolsRM) GetResourcePools() (*apiv1.GetResourcePoolsResponse, error) {
	var resp apiv1.GetResourcePoolsResponse
	for _, p := range r.pools {
		resp.ResourcePools = append(resp.ResourcePools, &resourcepoolv1.ResourcePool{Name: p})
	}
	return &resp, nil
}

// podSpecsRM is a resource manager of these pools that applies pod specs, or not.
type podSpecsRM struct {
	poolsRM
	applies bool
}

func (r podSpecsRM) AppliesPodSpecs(rm.ResourcePoolName) (bool, error) {
	return r.applies, nil
}

// The router asks the resource manager of the pool, the default one for no pool. It fails for a
// pool that no resource manager has and for a resource manager that does not say.
//
//nolint:exhaustruct
func TestAppliesPodSpecsRouting(t *testing.T) {
	router := New("agents", map[string]rm.ResourceManager{
		"agents": podSpecsRM{poolsRM: poolsRM{pools: []string{"default", "aux"}}},
		"k8s":    podSpecsRM{poolsRM: poolsRM{pools: []string{"gpu"}}, applies: true},
		"other":  poolsRM{pools: []string{"slurm"}},
	})
	for pool, applies := range map[rm.ResourcePoolName]bool{
		"": false, "default": false, "aux": false, "gpu": true,
	} {
		got, err := router.AppliesPodSpecs(pool)
		require.NoError(t, err, pool)
		require.Equal(t, applies, got, pool)
	}
	_, err := router.AppliesPodSpecs("gone")
	require.ErrorContains(t, err, "could not find resource pool gone")
	_, err = router.AppliesPodSpecs("slurm")
	require.ErrorContains(t, err, "resource manager other does not say whether it applies pod specs")
}
