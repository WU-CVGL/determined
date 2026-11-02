package sproto

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// TestNewProxyPortConfigPolicy checks that ports from a proxy port config never get the internal
// policy flags, whatever the config sets.
func TestNewProxyPortConfigPolicy(t *testing.T) {
	port := func(number int, tcp, unauth, defaultID bool) expconf.ProxyPortV0 {
		return expconf.ProxyPortV0{
			RawProxyPort: number, RawProxyTCP: ptrs.Ptr(tcp),
			RawUnauthenticated: ptrs.Ptr(unauth), RawDefaultServiceID: ptrs.Ptr(defaultID),
		}
	}
	input := expconf.ProxyPortsConfig{
		port(2900, false, false, true), port(8080, false, false, false), port(22, true, true, false),
	}
	for _, tls := range []bool{false, true} {
		got := NewProxyPortConfig(input, model.TaskID("task"), tls)
		require.Equal(t, []*ProxyPortConfig{
			{ServiceID: "task", Port: 2900, TLS: tls},
			{ServiceID: "task:8080", Port: 8080, TLS: tls},
			{ServiceID: "task:22", Port: 22, ProxyTCP: true, Unauthenticated: true, TLS: tls},
		}, got)
	}
}
