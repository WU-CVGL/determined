package tasks

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

func TestInteractiveServiceID(t *testing.T) {
	require.Equal(t, "task-1", InteractiveService{Port: 3210, DefaultServiceID: true}.ServiceID("task-1"))
	require.Equal(t, "task-1:2950", InteractiveService{Port: 2950}.ServiceID("task-1"))
}

// AddService records the service, whose port the container exposes but which is not an extra proxy
// port; the records survive the spec's JSON, which the master persists.
func TestAddService(t *testing.T) {
	userPort := expconf.ProxyPort{RawProxyPort: 8080}
	var spec GenericCommandSpec
	spec.Base.ExtraProxyPorts = expconf.ProxyPortsConfig{userPort}
	spec.Config.Environment.ProxyPorts = model.ProxyPortsConfig{{ProxyPort: 8888}}
	ssh := InteractiveService{
		Name: model.ServiceSSH, Port: 3210, DefaultServiceID: true, ProxyTCP: true,
		Unauthenticated: true,
	}
	jupyter := InteractiveService{Name: model.ServiceJupyter, Port: 2950, TLS: true, Exec: true}
	spec.AddService(ssh)
	spec.AddService(jupyter)

	require.Equal(t, []InteractiveService{ssh, jupyter}, spec.Services)
	require.Equal(t, expconf.ProxyPortsConfig{userPort}, spec.Base.ExtraProxyPorts)
	require.Equal(t, map[string]int{"8080": 8080, "8888": 8888, "3210": 3210, "2950": 2950},
		spec.ToTaskSpec().Environment.Ports())

	got, ok := spec.Service(model.ServiceJupyter)
	require.True(t, ok)
	require.Equal(t, jupyter, got)
	_, ok = spec.Service("vscode")
	require.False(t, ok)

	raw, err := json.Marshal(spec)
	require.NoError(t, err)
	var restored GenericCommandSpec
	require.NoError(t, json.Unmarshal(raw, &restored))
	require.Equal(t, spec.Services, restored.Services)
	require.Equal(t, spec.Base.ExtraProxyPorts, restored.Base.ExtraProxyPorts)
}

// The supervisor's services and JupyterLab's base URL come from the records, the primary service
// first; other variables stay. A spec without records keeps its environment.
func TestSetServicesEnv(t *testing.T) {
	var spec GenericCommandSpec
	spec.SetServicesEnv("task-1")
	require.Nil(t, spec.Base.ExtraEnvVars)

	spec.Base.ExtraEnvVars = map[string]string{ServicesEnvVar: "jupyter", "OTHER": "kept"}
	spec.SetServicesEnv("task-1")
	require.Equal(t, map[string]string{ServicesEnvVar: "jupyter", "OTHER": "kept"},
		spec.Base.ExtraEnvVars)

	spec.AddService(InteractiveService{Name: model.ServiceSSH, Port: 3210, DefaultServiceID: true})
	spec.SetServicesEnv("task-1")
	require.Equal(t, map[string]string{ServicesEnvVar: "ssh", "OTHER": "kept"},
		spec.Base.ExtraEnvVars)

	spec.AddService(InteractiveService{Name: model.ServiceJupyter, Port: 2950})
	spec.SetServicesEnv("task-1")
	require.Equal(t, map[string]string{
		ServicesEnvVar:        "ssh,jupyter",
		NotebookBaseURLEnvVar: "/proxy/task-1:2950/",
		"OTHER":               "kept",
	}, spec.Base.ExtraEnvVars)

	var fresh GenericCommandSpec
	fresh.AddService(InteractiveService{Name: model.ServiceSSH, Port: 3210, DefaultServiceID: true})
	fresh.SetServicesEnv("task-2")
	require.Equal(t, map[string]string{ServicesEnvVar: "ssh"}, fresh.Base.ExtraEnvVars)
}
