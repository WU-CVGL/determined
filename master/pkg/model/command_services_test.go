package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/check"
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
	for _, name := range []string{"vscode", "SSH", ""} {
		config, err := decodeCommandConfig(`{"services": {"` + name + `": {}}}`)
		require.NoError(t, err, name)
		err = check.Validate(config)
		require.ErrorContains(t, err, `services: unknown service "`+name+`"`)
	}

	// A service's value is an object of options, and it has none yet.
	for _, value := range []string{`1`, `"on"`, `true`, `[]`, `{"port": 8888}`} {
		_, err := decodeCommandConfig(`{"services": {"jupyter": ` + value + `}}`)
		require.Error(t, err, value)
	}
	_, err := decodeCommandConfig(`{"services": ["jupyter"]}`)
	require.Error(t, err)
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
}
