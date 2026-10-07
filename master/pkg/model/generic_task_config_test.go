package model

import (
	"strings"
	"testing"

	"github.com/ghodss/yaml"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/check"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

func TestGenericTaskConfigNameAndDescription(t *testing.T) {
	config := DefaultConfigGenericTaskConfig(nil)
	require.NoError(t, yaml.UnmarshalStrict([]byte(`
name: eval-sweep
description: evaluates the checkpoints of run 12
entrypoint: ["python", "eval.py"]
`), &config, yaml.DisallowUnknownFields))
	require.Equal(t, "eval-sweep", config.Name)
	require.Equal(t, "evaluates the checkpoints of run 12", config.Description)
	require.NoError(t, check.Validate(config))

	config.Name = strings.Repeat("x", 256)
	require.ErrorContains(t, check.Validate(config), "name must be at most 255 characters")
}

func TestGenericTaskConfigPreferGPUTopology(t *testing.T) {
	// Generic task configs are decoded strictly and never get WithDefaults.
	config := DefaultConfigGenericTaskConfig(nil)
	text := "entrypoint: [\"true\"]\nresources:\n  slots: 2\n  prefer_gpu_topology: soft\n"
	require.NoError(t, yaml.UnmarshalStrict([]byte(text), &config, yaml.DisallowUnknownFields))
	require.Equal(t, expconf.GPUTopologySoft, config.Resources.GPUTopology())
	require.NoError(t, check.Validate(config))

	config = DefaultConfigGenericTaskConfig(nil)
	require.Equal(t, expconf.GPUTopologyOff, config.Resources.GPUTopology())
	require.Error(t, yaml.UnmarshalStrict([]byte("resources:\n  prefer_gpu_topology: true\n"), &config,
		yaml.DisallowUnknownFields))
	text = "entrypoint: [\"true\"]\nresources:\n  prefer_gpu_topology: strong\n"
	require.NoError(t, yaml.UnmarshalStrict([]byte(text), &config, yaml.DisallowUnknownFields))
	require.Equal(t, expconf.GPUTopologyStrong, config.Resources.GPUTopology())
	require.NoError(t, check.Validate(config))
}
