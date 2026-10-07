package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/check"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

func TestConfigValidate(t *testing.T) {
	type fields struct {
		Description      string
		BindMounts       []BindMount
		Environment      Environment
		Resources        ResourcesConfig
		Entrypoint       []string
		NotebookIdleType string
	}
	type testCase struct {
		name    string
		fields  fields
		wantErr bool
	}
	var environment Environment
	resources := ResourcesConfig{
		Slots:  1,
		Weight: 1,
	}

	tests := []testCase{
		{
			name: "valid",
			fields: fields{
				Resources:   resources,
				Environment: environment,
				Entrypoint: []string{
					"test",
				},
				NotebookIdleType: NotebookIdleTypeActivity,
			},
		},
		{
			name: "invalid",
			fields: fields{
				Resources:   resources,
				Environment: environment,
			},
			wantErr: true,
		},
		{
			name: "invalid-notebook-idle-type",
			fields: fields{
				Resources:   resources,
				Environment: environment,
				Entrypoint: []string{
					"test",
				},
				NotebookIdleType: NotebookIdleTypeActivity + "x",
			},
			wantErr: true,
		},
	}
	runTestCase := func(t *testing.T, tc testCase) {
		t.Run(tc.name, func(t *testing.T) {
			c := &CommandConfig{
				Description:      tc.fields.Description,
				BindMounts:       tc.fields.BindMounts,
				Environment:      tc.fields.Environment,
				Resources:        tc.fields.Resources,
				Entrypoint:       tc.fields.Entrypoint,
				NotebookIdleType: tc.fields.NotebookIdleType,
			}
			if err := check.Validate(c); (err != nil) != tc.wantErr {
				t.Errorf("config.Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}

	for _, tc := range tests {
		runTestCase(t, tc)
	}
}

func TestCommandConfigPreferGPUTopology(t *testing.T) {
	decode := func(config *CommandConfig, text string) error {
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		return dec.Decode(config)
	}

	// The merged command config is decoded strictly, as in the command API.
	config := DefaultConfig(nil)
	require.NoError(t, decode(&config, `{"resources": {"prefer_gpu_topology": "soft"}}`))
	require.Equal(t, expconf.GPUTopologySoft, config.Resources.GPUTopology())
	require.Equal(t, expconf.GPUTopologySoft, config.Resources.ToExpconf().GPUTopology())
	require.NoError(t, check.Validate(config.Resources))

	// A user's false over a template's soft.
	require.NoError(t, decode(&config, `{"resources": {"prefer_gpu_topology": false}}`))
	require.Equal(t, expconf.GPUTopologyOff, config.Resources.GPUTopology())
	raw, err := json.Marshal(config.Resources)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"prefer_gpu_topology":false`)

	config = DefaultConfig(nil)
	require.Equal(t, expconf.GPUTopologyOff, config.Resources.GPUTopology())
	raw, err = json.Marshal(config)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "prefer_gpu_topology", "an unset key is not stored")

	require.EqualError(t, decode(&config, `{"resources": {"prefer_gpu_topology": true}}`),
		`prefer_gpu_topology must be false, "soft" or "strong", not true`)
	require.NoError(t, decode(&config, `{"resources": {"prefer_gpu_topology": "strong"}}`))
	require.Equal(t, expconf.GPUTopologyStrong, config.Resources.GPUTopology())
	require.Equal(t, expconf.GPUTopologyStrong, config.Resources.ToExpconf().GPUTopology())
	require.NoError(t, check.Validate(config.Resources))
}

func TestParseJustResourcesSkipsPreferGPUTopology(t *testing.T) {
	// The command API resolves the pool from these resources before the strict decode reports an
	// invalid prefer_gpu_topology: an invalid value must not hide the pool and the slots.
	for _, value := range []string{`true`, `"soft"`, `"on"`, `false`} {
		r := ParseJustResources([]byte(
			`{"resources":{"prefer_gpu_topology":` + value + `,"resource_pool":"gpu","slots":4}}`))
		require.Equal(t, "gpu", r.ResourcePool, value)
		require.Equal(t, 4, r.Slots, value)
		require.Nil(t, r.PreferGPUTopology, value)
	}
	require.Equal(t, 1, ParseJustResources(nil).Slots)
}
