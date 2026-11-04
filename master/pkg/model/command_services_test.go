package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/check"
	"github.com/determined-ai/determined/master/pkg/ptrs"
)

// decodeCommandConfig decodes text over the default config strictly, as the command API decodes a
// request's config.
func decodeCommandConfig(text string) (CommandConfig, error) {
	config := DefaultConfig(nil)
	config.Entrypoint = []string{"run"}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	err := dec.Decode(&config)
	return config, err
}

func TestCommandConfigServices(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		want         []string
	}{
		{name: "unset", config: `{}`},
		{name: "empty map", config: `{"services": {}}`},
		{name: "null", config: `{"services": null}`},
		{name: "jupyter", config: `{"services": {"jupyter": {}}}`, want: []string{"jupyter"}},
		{name: "null options", config: `{"services": {"jupyter": null}}`, want: []string{"jupyter"}},
		{
			name:   "ssh listed",
			config: `{"services": {"ssh": {}, "jupyter": {}}}`,
			want:   []string{"jupyter", "ssh"},
		},
		{
			name:   "vscode",
			config: `{"services": {"vscode": {}, "jupyter": {}}}`,
			want:   []string{"jupyter", "vscode"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := decodeCommandConfig(tc.config)
			require.NoError(t, err)
			require.NoError(t, check.Validate(config))
			if tc.want == nil {
				require.Empty(t, config.Services)
			} else {
				require.Equal(t, tc.want, DeclaredServices(config.Services))
			}
		})
	}

	// A service that the supervisor has no unit for.
	for _, name := range []string{"emacs", "VSCode", "SSH", ""} {
		config, err := decodeCommandConfig(`{"services": {"` + name + `": {}}}`)
		require.NoError(t, err, name)
		err = check.Validate(config)
		require.ErrorContains(t, err, `services: unknown service "`+name+`"`)
	}

	// A service's value is an object of options: "enabled", "idle_type", and no other.
	for _, value := range []string{
		`1`, `"on"`, `true`, `[]`, `{"port": 8888}`, `{"enabled": 1}`, `{"enabled": "no"}`,
		`{"idle_type": 1}`,
	} {
		_, err := decodeCommandConfig(`{"services": {"jupyter": ` + value + `}}`)
		require.Error(t, err, value)
	}
	_, err := decodeCommandConfig(`{"services": ["jupyter"]}`)
	require.Error(t, err)
}

// A service runs unless its config sets enabled to false; its entry stays in the config either
// way, and a disabled one is not declared.
func TestServiceConfigEnabled(t *testing.T) {
	require.True(t, ServiceConfig{}.Runs())
	require.True(t, ServiceConfig{Enabled: ptrs.Ptr(true)}.Runs())
	require.False(t, ServiceConfig{Enabled: ptrs.Ptr(false)}.Runs())

	config, err := decodeCommandConfig(
		`{"services": {"jupyter": {"enabled": false}, "vscode": {"enabled": true}, "ssh": {}}}`)
	require.NoError(t, err)
	require.NoError(t, check.Validate(config))
	require.Equal(t, []string{"ssh", "vscode"}, DeclaredServices(config.Services))
	require.Equal(t, []string{"jupyter", "ssh", "vscode"}, configuredServices(config.Services))
	require.Len(t, config.Services, 3)
	require.False(t, config.Services["jupyter"].Runs())

	// The entry round-trips: enabled is written only when set.
	raw, err := json.Marshal(config.Services)
	require.NoError(t, err)
	require.JSONEq(t, `{"jupyter": {"enabled": false}, "vscode": {"enabled": true}, "ssh": {}}`,
		string(raw))

	require.Empty(t, DeclaredServices(nil))
	require.Empty(t, DeclaredServices(map[string]ServiceConfig{
		"jupyter": {Enabled: ptrs.Ptr(false)},
	}))
}

// Every shell runs its SSH server: ssh cannot be disabled. An unknown service is refused whether
// it is enabled or not.
func TestCommandConfigServicesRefusals(t *testing.T) {
	config, err := decodeCommandConfig(`{"services": {"ssh": {"enabled": false}}}`)
	require.NoError(t, err)
	err = check.Validate(config)
	require.ErrorContains(t, err, "services: ssh cannot be disabled")
	config, err = decodeCommandConfig(`{"services": {"ssh": {"enabled": true}}}`)
	require.NoError(t, err)
	require.NoError(t, check.Validate(config))

	for _, value := range []string{`{}`, `{"enabled": false}`, `{"enabled": true}`} {
		config, err := decodeCommandConfig(`{"services": {"emacs": ` + value + `}}`)
		require.NoError(t, err, value)
		err = check.Validate(config)
		require.ErrorContains(t, err, `services: unknown service "emacs"`, value)
	}
}

// The jupyter service's idle_type is one of the idle types, activity when unset; no other service
// takes it. The config has no top-level notebook_idle_type.
func TestServiceConfigIdleType(t *testing.T) {
	require.Equal(t, JupyterIdleTypeActivity, ServiceConfig{}.JupyterIdleType())
	for _, idleType := range JupyterIdleTypes {
		config, err := decodeCommandConfig(`{"services": {"jupyter": {"idle_type": "` + idleType + `"}}}`)
		require.NoError(t, err, idleType)
		require.NoError(t, check.Validate(config), idleType)
		require.Equal(t, idleType, config.Services[ServiceJupyter].JupyterIdleType())
	}
	config, err := decodeCommandConfig(`{"services": {"jupyter": {"enabled": false}}}`)
	require.NoError(t, err)
	require.Equal(t, JupyterIdleTypeActivity, config.Services[ServiceJupyter].JupyterIdleType())
	raw, err := json.Marshal(config.Services)
	require.NoError(t, err)
	require.JSONEq(t, `{"jupyter": {"enabled": false}}`, string(raw), "an unset idle_type is not stored")

	for text, want := range map[string]string{
		`{"services": {"jupyter": {"idle_type": "sometimes"}}}`: `services: jupyter: unknown idle_type "sometimes"`,
		`{"services": {"jupyter": {"idle_type": "Activity"}}}`:  `services: jupyter: unknown idle_type "Activity"`,
		`{"services": {"vscode": {"idle_type": "activity"}}}`:   "services: vscode: only jupyter takes idle_type",
		`{"services": {"ssh": {"idle_type": "activity"}}}`:      "services: ssh: only jupyter takes idle_type",
	} {
		config, err := decodeCommandConfig(text)
		require.NoError(t, err, text)
		require.ErrorContains(t, check.Validate(config), want, text)
	}

	_, err = decodeCommandConfig(`{"notebook_idle_type": "activity"}`)
	require.ErrorContains(t, err, `unknown field "notebook_idle_type"`)
}

// The default services of a shell: jupyter, and vscode when the master serves code-server.
func TestDefaultShellServices(t *testing.T) {
	require.Equal(t, []string{ServiceJupyter}, DefaultShellServices(false))
	require.Equal(t, []string{ServiceJupyter, ServiceVSCode}, DefaultShellServices(true))
}

// An idle_timeout can be unset, and is positive when set.
func TestCommandConfigIdleTimeout(t *testing.T) {
	for _, text := range []string{
		`{}`, `{"idle_timeout": null}`, `{"idle_timeout": "1ns"}`, `{"idle_timeout": "45m"}`,
	} {
		config, err := decodeCommandConfig(text)
		require.NoError(t, err, text)
		require.NoError(t, check.Validate(config), text)
	}
	for _, value := range []string{"0s", "0", "-1s", "-1h30m"} {
		config, err := decodeCommandConfig(`{"idle_timeout": "` + value + `"}`)
		require.NoError(t, err, value)
		require.ErrorContains(t, check.Validate(config), "idle_timeout must be positive", value)
	}
}

// An unset or empty map is not stored, so configs without services marshal as before.
func TestCommandConfigServicesMarshal(t *testing.T) {
	for _, services := range []map[string]ServiceConfig{nil, {}} {
		config := DefaultConfig(nil)
		config.Services = services
		raw, err := json.Marshal(config)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "services")
	}
	config := DefaultConfig(nil)
	config.Services = map[string]ServiceConfig{ServiceJupyter: {}}
	raw, err := json.Marshal(config)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"services":{"jupyter":{}}`)
}

func TestInteractiveServicesSorted(t *testing.T) {
	require.IsNonDecreasing(t, InteractiveServices)
	require.Contains(t, InteractiveServices, ServiceSSH)
	require.Contains(t, InteractiveServices, ServiceJupyter)
	require.Contains(t, InteractiveServices, ServiceVSCode)
}
