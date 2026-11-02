package internal

import (
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

func TestSetShellSSHD(t *testing.T) {
	userPort := expconf.ProxyPort{RawProxyPort: 8080, RawProxyTCP: ptrs.Ptr(true)}
	spec := &tasks.GenericCommandSpec{}
	spec.Base.ExtraProxyPorts = expconf.ProxyPortsConfig{userPort}

	const port = 3217
	setShellSSHD(spec, port)

	// The browser terminal finds sshd at SSHDPort: the port sshd listens on.
	require.Equal(t, port, spec.SSHDPort)
	p := slices.Index(spec.Config.Entrypoint, "-p")
	require.GreaterOrEqual(t, p, 0)
	require.Less(t, p+1, len(spec.Config.Entrypoint))
	require.Equal(t, strconv.Itoa(port), spec.Config.Entrypoint[p+1])

	// The same port is the shell's only default TCP proxy port, which the master proxies and which
	// shells saved without SSHDPort fall back to; the user's ports stay.
	ports := schemas.WithDefaults(spec.Base.ExtraProxyPorts)
	require.Len(t, ports, 2)
	require.Equal(t, userPort.RawProxyPort, ports[0].ProxyPort())
	require.False(t, ports[0].DefaultServiceID())
	var defaultTCP []int
	for _, pp := range ports {
		if pp.DefaultServiceID() && pp.ProxyTCP() {
			defaultTCP = append(defaultTCP, pp.ProxyPort())
		}
	}
	require.Equal(t, []int{port}, defaultTCP)
	require.True(t, ports[1].Unauthenticated())
}
