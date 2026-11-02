package command

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

// sshdProxyPort is the extra proxy port that the master gives a shell's sshd.
func sshdProxyPort(port int) expconf.ProxyPort {
	return expconf.ProxyPort{
		RawProxyPort:        port,
		RawProxyTCP:         ptrs.Ptr(true),
		RawUnauthenticated:  ptrs.Ptr(true),
		RawDefaultServiceID: ptrs.Ptr(true),
	}
}

// userProxyPort is a proxy port of a config's environment.
var userProxyPort = model.ProxyPort{ProxyPort: 8080}

func legacySpec(taskType model.TaskType, ports ...expconf.ProxyPort) tasks.GenericCommandSpec {
	var spec tasks.GenericCommandSpec
	spec.TaskType = taskType
	spec.Base.ExtraProxyPorts = ports
	spec.Config.Environment.ProxyPorts = model.ProxyPortsConfig{userProxyPort}
	return spec
}

// Specs without service records, as every spec was before the master recorded them and as
// notebooks still are, get the proxies they had: a shell's sshd as its default TCP port, plain
// other ports, and every port of a notebook over HTTPS.
func TestProxyPortConfigsWithoutRecords(t *testing.T) {
	shell := legacySpec(model.TaskTypeShell, sshdProxyPort(3210))
	require.Equal(t, []*sproto.ProxyPortConfig{
		{ServiceID: "task-1", Port: 3210, ProxyTCP: true, Unauthenticated: true},
		{ServiceID: "task-1:8080", Port: 8080},
	}, proxyPortConfigs(shell, "task-1"))

	notebook := legacySpec(model.TaskTypeNotebook,
		expconf.ProxyPort{RawProxyPort: 2950, RawDefaultServiceID: ptrs.Ptr(true)})
	require.Equal(t, []*sproto.ProxyPortConfig{
		{ServiceID: "task-1", Port: 2950, TLS: true},
		{ServiceID: "task-1:8080", Port: 8080, TLS: true},
	}, proxyPortConfigs(notebook, "task-1"))

	command := legacySpec(model.TaskTypeCommand)
	require.Equal(t, []*sproto.ProxyPortConfig{{ServiceID: "task-1:8080", Port: 8080}},
		proxyPortConfigs(command, "task-1"))
}

// A shell's services get their recorded policies on their own ports, and the config's ports stay
// plain, also on a port that a service has.
func TestProxyPortConfigsFromRecords(t *testing.T) {
	var spec tasks.GenericCommandSpec
	spec.TaskType = model.TaskTypeShell
	spec.AddService(tasks.InteractiveService{
		Name: model.ServiceSSH, Port: 3210, DefaultServiceID: true, ProxyTCP: true,
		Unauthenticated: true,
	})
	spec.AddService(tasks.InteractiveService{
		Name: model.ServiceJupyter, Port: 2950, TLS: true, Exec: true,
	})
	spec.AddService(tasks.InteractiveService{Name: "editor", Port: 2700, StripPrefix: true})
	spec.Config.Environment.ProxyPorts = model.ProxyPortsConfig{userProxyPort, {ProxyPort: 2950}}

	require.Equal(t, []*sproto.ProxyPortConfig{
		{ServiceID: "task-1", Port: 3210, ProxyTCP: true, Unauthenticated: true},
		{ServiceID: "task-1:2950", Port: 2950, TLS: true, Exec: true},
		{ServiceID: "task-1:2700", Port: 2700, StripPrefix: true},
		{ServiceID: "task-1:8080", Port: 8080},
		{ServiceID: "task-1:2950", Port: 2950},
	}, proxyPortConfigs(spec, "task-1"))
}
