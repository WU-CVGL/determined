package config

import (
	"testing"
	"time"

	"github.com/ghodss/yaml"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/check"
	"github.com/determined-ai/determined/master/pkg/model"
)

func TestTaskMappingDelayConfig(t *testing.T) {
	c := DefaultConfig()
	require.Equal(t, model.Duration(5*time.Minute), c.Observability.TaskMappingDelay)
	require.NoError(t, check.Validate(c.Observability))

	// Setting only enable_prometheus keeps the default delay, and integrations.task_resources
	// plays no part in it.
	raw := `
observability:
  enable_prometheus: false
`
	require.NoError(t, yaml.Unmarshal([]byte(raw), c, yaml.DisallowUnknownFields))
	require.False(t, c.Observability.EnablePrometheus)
	require.Equal(t, model.Duration(5*time.Minute), c.Observability.TaskMappingDelay)
	require.False(t, c.Integrations.TaskResources.Enabled())

	for value, want := range map[string]time.Duration{"0s": 0, "90s": 90 * time.Second, "1h": time.Hour} {
		c := DefaultConfig()
		raw := "observability:\n  task_mapping_delay: " + value + "\n"
		require.NoError(t, yaml.Unmarshal([]byte(raw), c, yaml.DisallowUnknownFields), value)
		require.Equal(t, model.Duration(want), c.Observability.TaskMappingDelay, value)
		require.True(t, c.Observability.EnablePrometheus)
		require.NoError(t, check.Validate(c.Observability), value)
	}

	// A duration needs a unit: a bare 0 is a number, which model.Duration rejects.
	require.Error(t, yaml.Unmarshal([]byte("observability:\n  task_mapping_delay: 0\n"), DefaultConfig()))

	// check.Validate reaches the observability section from the master config.
	negative := struct{ Observability ObservabilityConfig }{
		Observability: ObservabilityConfig{TaskMappingDelay: model.Duration(-time.Second)},
	}
	err := check.Validate(negative)
	require.ErrorContains(t, err, "observability.task_mapping_delay must not be negative")
}
