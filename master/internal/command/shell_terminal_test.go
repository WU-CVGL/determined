package command

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

func TestShellSSHDAddr(t *testing.T) {
	// LaunchShell's sshd port, plus a user proxy port from the environment config.
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
	require.Equal(t, "10.0.0.7:40002", shellSSHDAddr(ports, addrs))

	// IPv6 hosts are bracketed.
	addrs["r1"][1].HostIP = "fd00::7"
	require.Equal(t, "[fd00::7]:40002", shellSSHDAddr(ports, addrs))

	// No address for the sshd port yet, or no sshd port at all.
	require.Empty(t, shellSSHDAddr(ports, map[sproto.ResourcesID][]cproto.Address{
		"r1": {{ContainerPort: 8888, HostIP: "10.0.0.7", HostPort: 40001}},
	}))
	require.Empty(t, shellSSHDAddr(ports, nil))
	require.Empty(t, shellSSHDAddr(expconf.ProxyPortsConfig{{RawProxyPort: 3210}}, addrs))
	require.Empty(t, shellSSHDAddr(ports, map[sproto.ResourcesID][]cproto.Address{
		"r1": {{ContainerPort: 3210, HostIP: "", HostPort: 40002}},
	}))
}
