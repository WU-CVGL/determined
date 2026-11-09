package command

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/ssh"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/shellv1"
)

func TestShellSSHDAddr(t *testing.T) {
	// A shell saved without its sshd port: LaunchShell's sshd port, plus a user proxy port from
	// the environment config.
	ports := expconf.ProxyPortsConfig{
		{
			RawProxyPort:        3210,
			RawProxyTCP:         ptrs.Ptr(true),
			RawUnauthenticated:  ptrs.Ptr(true),
			RawDefaultServiceID: ptrs.Ptr(true),
		},
		{RawProxyPort: 8888},
	}
	addrs := map[sproto.ResourcesID][]cproto.Address{
		"r1": {
			{ContainerIP: "172.17.0.2", ContainerPort: 8888, HostIP: "10.0.0.7", HostPort: 40001},
			{ContainerIP: "172.17.0.2", ContainerPort: 3210, HostIP: "10.0.0.7", HostPort: 40002},
		},
	}
	require.Equal(t, "10.0.0.7:40002", shellSSHDAddr(0, ports, addrs))

	// IPv6 hosts are bracketed.
	addrs["r1"][1].HostIP = "fd00::7"
	require.Equal(t, "[fd00::7]:40002", shellSSHDAddr(0, ports, addrs))

	// No address for the sshd port yet, or no sshd port at all.
	require.Empty(t, shellSSHDAddr(0, ports, map[sproto.ResourcesID][]cproto.Address{
		"r1": {{ContainerPort: 8888, HostIP: "10.0.0.7", HostPort: 40001}},
	}))
	require.Empty(t, shellSSHDAddr(0, ports, nil))
	require.Empty(t, shellSSHDAddr(0, expconf.ProxyPortsConfig{{RawProxyPort: 3210}}, addrs))
	require.Empty(t, shellSSHDAddr(0, ports, map[sproto.ResourcesID][]cproto.Address{
		"r1": {{ContainerPort: 3210, HostIP: "", HostPort: 40002}},
	}))

	// The recorded sshd port wins over the default TCP proxy port, and need not be one.
	addrs["r1"] = append(addrs["r1"],
		cproto.Address{ContainerIP: "172.17.0.2", ContainerPort: 2222, HostIP: "10.0.0.7", HostPort: 40003})
	require.Equal(t, "10.0.0.7:40003", shellSSHDAddr(2222, ports, addrs))
	require.Equal(t, "10.0.0.7:40003", shellSSHDAddr(2222, nil, addrs))
	require.Equal(t, "10.0.0.7:40001", shellSSHDAddr(8888, ports, addrs))
	require.Empty(t, shellSSHDAddr(2223, ports, addrs))
}

func TestHasSSHD(t *testing.T) {
	key := "key"
	sshdPort := expconf.ProxyPortsConfig{{
		RawProxyPort: 3210, RawProxyTCP: ptrs.Ptr(true), RawDefaultServiceID: ptrs.Ptr(true),
	}}
	cmd := func(taskType model.TaskType, f func(*tasks.GenericCommandSpec)) *Command {
		spec := tasks.GenericCommandSpec{Keys: &ssh.PrivateAndPublicKeys{}}
		spec.Metadata.PrivateKey = &key
		spec.Metadata.PublicKey = &key
		if f != nil {
			f(&spec)
		}
		return &Command{GenericCommandSpec: spec, taskType: taskType}
	}

	// A shell, with or without a recorded sshd port.
	require.True(t, cmd(model.TaskTypeShell, nil).hasSSHD())
	require.True(t, cmd(model.TaskTypeShell, func(s *tasks.GenericCommandSpec) {
		s.Base.ExtraProxyPorts = sshdPort
	}).hasSSHD())
	require.True(t, cmd(model.TaskTypeShell, func(s *tasks.GenericCommandSpec) { s.SSHDPort = 2222 }).hasSSHD())
	require.False(t, cmd(model.TaskTypeShell, func(s *tasks.GenericCommandSpec) {
		s.Metadata.PrivateKey = nil
	}).hasSSHD())

	// Another task needs keys and a recorded sshd port; a default TCP proxy port is not one.
	withPort := func(s *tasks.GenericCommandSpec) { s.SSHDPort = 2222 }
	require.True(t, cmd(model.TaskTypeCommand, withPort).hasSSHD())
	require.True(t, cmd(model.TaskTypeTensorboard, withPort).hasSSHD())
	require.False(t, cmd(model.TaskTypeCommand, nil).hasSSHD())
	require.False(t, cmd(model.TaskTypeCommand, func(s *tasks.GenericCommandSpec) {
		s.Base.ExtraProxyPorts = sshdPort
	}).hasSSHD())
	require.False(t, cmd(model.TaskTypeCommand, func(s *tasks.GenericCommandSpec) {
		withPort(s)
		s.Keys = nil
	}).hasSSHD())
	require.False(t, cmd(model.TaskTypeCommand, func(s *tasks.GenericCommandSpec) {
		withPort(s)
		s.Metadata.PublicKey = nil
	}).hasSSHD())
}

// A shell whose primary service is tensorboard has no SSH keys: it has no sshd, and its shell
// view has an empty public key and the experiments and trials it reads.
func TestKeylessShell(t *testing.T) {
	var spec tasks.GenericCommandSpec
	spec.Base.Owner = &model.User{ID: 1, Username: "owner"}
	spec.Metadata.ExperimentIDs = []int32{12}
	spec.Metadata.TrialIDs = []int32{40}
	spec.AddService(tasks.InteractiveService{
		Name: model.ServiceTensorBoard, Port: 3000, Exec: true, SourcesCheck: true,
	})
	c := servicesCommand(spec, model.TaskTypeShell)
	c.exitStatus = &task.AllocationExited{}

	require.False(t, c.hasSSHD())
	var shell *shellv1.Shell
	require.NotPanics(t, func() { shell = c.ToV1Shell() })
	require.Empty(t, shell.PublicKey)
	require.Equal(t, []int32{12}, shell.ExperimentIds)
	require.Equal(t, []int32{40}, shell.TrialIds)
}
