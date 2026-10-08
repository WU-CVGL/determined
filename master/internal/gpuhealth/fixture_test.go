package gpuhealth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/determined-ai/determined/proto/pkg/agentv1"
)

// The CLI and WebUI tests share harness/tests/fixtures/gpu_topology_cases.json and trust its
// per-GPU health. Classify must give the same health from the rest of each topology, so the
// fixture cannot drift from the master.
func TestClassifyMatchesSharedFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "harness", "tests", "fixtures",
		"gpu_topology_cases.json"))
	require.NoError(t, err)
	var fixture struct {
		Cases []struct {
			Name        string          `json:"name"`
			GpuTopology json.RawMessage `json:"gpuTopology"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.NotEmpty(t, fixture.Cases)

	withXIDs := 0
	for _, c := range fixture.Cases {
		if string(c.GpuTopology) == "null" {
			continue
		}
		var topo agentv1.GpuTopology
		require.NoError(t, protojson.Unmarshal(c.GpuTopology, &topo), c.Name)
		var want []agentv1.GpuHealth
		for _, g := range topo.Gpus {
			want = append(want, g.Health)
			g.Health = agentv1.GpuHealth_GPU_HEALTH_UNSPECIFIED
			if len(g.RecentXids) > 0 {
				withXIDs++
			}
		}
		Classify(&topo)
		var got []agentv1.GpuHealth
		for _, g := range topo.Gpus {
			got = append(got, g.Health)
		}
		require.Equal(t, want, got, c.Name)
	}
	require.Positive(t, withXIDs, "the fixture has GPUs with recent XIDs")
}
