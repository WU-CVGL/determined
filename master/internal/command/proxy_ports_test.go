package command

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

// testTensorBoardToken is the tensorboard service's token of the tests.
const testTensorBoardToken = "tb-token"

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

// Specs without service records, as every spec was before the master recorded them, get the
// proxies they had: a shell's sshd as its default TCP port, and plain other ports.
func TestProxyPortConfigsWithoutRecords(t *testing.T) {
	shell := legacySpec(model.TaskTypeShell, sshdProxyPort(3210))
	require.Equal(t, []*sproto.ProxyPortConfig{
		{ServiceID: "task-1", Port: 3210, ProxyTCP: true, Unauthenticated: true},
		{ServiceID: "task-1:8080", Port: 8080},
	}, proxyPortConfigs(shell, "task-1"))

	command := legacySpec(model.TaskTypeCommand)
	require.Equal(t, []*sproto.ProxyPortConfig{{ServiceID: "task-1:8080", Port: 8080}},
		proxyPortConfigs(command, "task-1"))
}

// A TensorBoard's own port gets the spec's token; the config's ports stay plain, and so does a
// TensorBoard saved without a token. A shell without records gets no token.
func TestProxyPortConfigsOfTensorBoard(t *testing.T) {
	tensorBoardPort := expconf.ProxyPort{RawProxyPort: 2700, RawDefaultServiceID: ptrs.Ptr(true)}
	tensorBoard := legacySpec(model.TaskTypeTensorboard, tensorBoardPort)
	token := testTensorBoardToken
	tensorBoard.Metadata.TensorBoardToken = &token
	require.Equal(t, []*sproto.ProxyPortConfig{
		{ServiceID: "task-1", Port: 2700, ServiceToken: token},
		{ServiceID: "task-1:8080", Port: 8080},
	}, proxyPortConfigs(tensorBoard, "task-1"))

	tensorBoard.Metadata.TensorBoardToken = nil
	require.Equal(t, []*sproto.ProxyPortConfig{
		{ServiceID: "task-1", Port: 2700},
		{ServiceID: "task-1:8080", Port: 8080},
	}, proxyPortConfigs(tensorBoard, "task-1"))

	shell := legacySpec(model.TaskTypeShell, sshdProxyPort(3210))
	shell.Metadata.TensorBoardToken = &token
	for _, port := range proxyPortConfigs(shell, "task-1") {
		require.Empty(t, port.ServiceToken)
	}
}

// A spec with service records gets one proxy policy per container port: each service's from its
// record, the tensorboard service's with the spec's token, and the config's ports and the launch's
// own extra ports plain. The services' ports are in the records only.
func TestProxyPortConfigsFromRecords(t *testing.T) {
	var spec tasks.GenericCommandSpec
	spec.TaskType = model.TaskTypeShell
	spec.Services = []tasks.InteractiveService{
		{
			Name: model.ServiceSSH, Port: 3210, DefaultServiceID: true, ProxyTCP: true,
			Unauthenticated: true,
		},
		{Name: model.ServiceJupyter, Port: 2950, TLS: true, Exec: true},
		{Name: "editor", Port: 2700, StripPrefix: true},
		{Name: model.ServiceTensorBoard, Port: 3000, Exec: true},
	}
	spec.Base.ExtraProxyPorts = expconf.ProxyPortsConfig{{RawProxyPort: 2600}}
	spec.Config.Environment.ProxyPorts = model.ProxyPortsConfig{userProxyPort}
	token := testTensorBoardToken
	spec.Metadata.TensorBoardToken = &token

	require.Equal(t, []*sproto.ProxyPortConfig{
		{ServiceID: "task-1", Port: 3210, ProxyTCP: true, Unauthenticated: true},
		{ServiceID: "task-1:2950", Port: 2950, TLS: true, Exec: true},
		{ServiceID: "task-1:2700", Port: 2700, StripPrefix: true},
		{ServiceID: "task-1:3000", Port: 3000, Exec: true, ServiceToken: token},
		{ServiceID: "task-1:2600", Port: 2600},
		{ServiceID: "task-1:8080", Port: 8080},
	}, proxyPortConfigs(spec, "task-1"))

	// A spec without a token, as one saved before the tokens, proxies its TensorBoard without one.
	spec.Metadata.TensorBoardToken = nil
	require.Empty(t, proxyPortConfigs(spec, "task-1")[3].ServiceToken)
}

// The token is in no encoding of a proxy config, such as the task list's.
func TestProxyPortConfigEncodingHidesToken(t *testing.T) {
	config := &sproto.ProxyPortConfig{ServiceID: "task-1:3000", Port: 3000, ServiceToken: testTensorBoardToken}
	encoded, err := json.Marshal(sproto.AllocationSummary{ProxyPorts: []*sproto.ProxyPortConfig{config}})
	require.NoError(t, err)
	require.NotContains(t, string(encoded), testTensorBoardToken)
	require.NotContains(t, config.Proto().String(), testTensorBoardToken)
}

// A spec with records may also hold copies of the services' ports in its extra proxy ports, as
// specs saved before the records alone held them do. Each such port still gets only the policy
// of its record.
func TestProxyPortConfigsSkipCopiesOfRecords(t *testing.T) {
	var spec tasks.GenericCommandSpec
	spec.TaskType = model.TaskTypeShell
	spec.Services = []tasks.InteractiveService{
		{
			Name: model.ServiceSSH, Port: 3210, DefaultServiceID: true, ProxyTCP: true,
			Unauthenticated: true,
		},
		{Name: model.ServiceJupyter, Port: 2950, TLS: true, Exec: true},
	}
	spec.Base.ExtraProxyPorts = expconf.ProxyPortsConfig{
		sshdProxyPort(3210), {RawProxyPort: 2950},
	}
	spec.Config.Environment.ProxyPorts = model.ProxyPortsConfig{userProxyPort}

	require.Equal(t, []*sproto.ProxyPortConfig{
		{ServiceID: "task-1", Port: 3210, ProxyTCP: true, Unauthenticated: true},
		{ServiceID: "task-1:2950", Port: 2950, TLS: true, Exec: true},
		{ServiceID: "task-1:8080", Port: 8080},
	}, proxyPortConfigs(spec, "task-1"))
}

// A tensorboard record that reads experiments and trials gets the sources check beside Exec and
// the token; a logdir one, and a legacy TensorBoard, get none.
func TestProxyPortConfigsSourcesCheck(t *testing.T) {
	var spec tasks.GenericCommandSpec
	spec.TaskType = model.TaskTypeShell
	spec.Services = []tasks.InteractiveService{
		{Name: model.ServiceTensorBoard, Port: 3000, Exec: true, SourcesCheck: true},
		{Name: model.ServiceJupyter, Port: 2950, TLS: true, Exec: true},
	}
	token := testTensorBoardToken
	spec.Metadata.TensorBoardToken = &token
	require.Equal(t, []*sproto.ProxyPortConfig{
		{ServiceID: "task-1:3000", Port: 3000, Exec: true, SourcesCheck: true, ServiceToken: token},
		{ServiceID: "task-1:2950", Port: 2950, TLS: true, Exec: true},
	}, proxyPortConfigs(spec, "task-1"))

	spec.Services[0].SourcesCheck = false
	require.Equal(t, &sproto.ProxyPortConfig{
		ServiceID: "task-1:3000", Port: 3000, Exec: true, ServiceToken: token,
	}, proxyPortConfigs(spec, "task-1")[0])

	tensorBoard := legacySpec(model.TaskTypeTensorboard,
		expconf.ProxyPort{RawProxyPort: 2700, RawDefaultServiceID: ptrs.Ptr(true)})
	tensorBoard.Metadata.TensorBoardToken = &token
	tensorBoard.Metadata.ExperimentIDs = []int32{1}
	for _, port := range proxyPortConfigs(tensorBoard, "task-1") {
		require.False(t, port.SourcesCheck)
	}
}
