package internal

import (
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
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

	// The port is the shell's ssh service, its primary one, whose record the master proxies it
	// by; the extra proxy ports stay as they were.
	require.Equal(t, []tasks.InteractiveService{{
		Name: "ssh", Port: port, DefaultServiceID: true, ProxyTCP: true, Unauthenticated: true,
	}}, spec.Services)
	require.Equal(t, expconf.ProxyPortsConfig{userPort}, spec.Base.ExtraProxyPorts)
}

// A service's port is a port of its range that no proxy port and no other service of the spec
// has; with none left, the launch is invalid and the message names the range.
func TestFreeServicePort(t *testing.T) {
	spec := &tasks.GenericCommandSpec{}
	spec.Config.Environment.ProxyPorts = model.ProxyPortsConfig{{ProxyPort: 10}}
	spec.Base.ExtraProxyPorts = expconf.ProxyPortsConfig{{RawProxyPort: 11}}
	spec.AddService(tasks.InteractiveService{Name: "ssh", Port: 12})

	for i := 0; i < 50; i++ {
		port, err := freeServicePort(spec, "jupyter", 10, 13)
		require.NoError(t, err)
		require.Equal(t, 13, port)
	}
	seen := map[int]bool{}
	for i := 0; i < 200; i++ {
		port, err := freeServicePort(spec, "jupyter", 12, 15)
		require.NoError(t, err)
		seen[port] = true
	}
	require.Equal(t, map[int]bool{13: true, 14: true, 15: true}, seen)

	_, err := freeServicePort(spec, "jupyter", 10, 12)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "services: jupyter: no free port in 10-12")
}
