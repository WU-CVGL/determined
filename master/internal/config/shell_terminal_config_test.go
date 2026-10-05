package config

import (
	"testing"
	"time"

	"github.com/ghodss/yaml"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/check"
	"github.com/determined-ai/determined/master/pkg/model"
)

func TestShellTerminalConfig(t *testing.T) {
	c := DefaultConfig()
	require.Equal(t, DefaultShellTerminalConfig(), c.ShellTerminal)
	require.True(t, c.ShellTerminal.AllowAdmin)
	require.Empty(t, c.ShellTerminal.Validate())

	raw := `
shell_terminal:
  allow_admin: false
  max_sessions_per_user: 2
  idle_timeout: 30m
  trusted_proxies: ["192.168.233.6", "10.0.0.0/8"]
`
	require.NoError(t, yaml.Unmarshal([]byte(raw), c, yaml.DisallowUnknownFields))
	require.False(t, c.ShellTerminal.AllowAdmin)
	require.Equal(t, 2, c.ShellTerminal.MaxSessionsPerUser)
	require.Equal(t, 256, c.ShellTerminal.MaxSessions)
	require.Equal(t, model.Duration(30*time.Minute), c.ShellTerminal.IdleTimeout)
	require.Equal(t, model.Duration(24*time.Hour), c.ShellTerminal.MaxSessionDuration)
	require.NoError(t, check.Validate(c.ShellTerminal))

	bad := DefaultShellTerminalConfig()
	bad.MaxSessionsPerUser = 0
	bad.MaxSessions = -1
	bad.IdleTimeout = model.Duration(-time.Second)
	bad.MaxSessionDuration = 0
	bad.RecheckInterval = model.Duration(time.Hour)
	bad.TrustedProxies = []string{"nginx", "10.0.0.0/40"}
	require.Len(t, bad.Validate(), 7)
	require.Error(t, check.Validate(bad))
}
