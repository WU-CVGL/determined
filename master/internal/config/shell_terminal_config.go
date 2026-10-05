package config

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/determined-ai/determined/master/pkg/model"
)

// ShellTerminalConfig configures the browser terminal for shells. The master serves the terminal
// unless the feature switch "-shell_terminal" is set.
type ShellTerminalConfig struct {
	// AllowAdmin lets admins open terminals in other users' shells. Owners can always open
	// terminals in their own shells. No other permission grants access.
	AllowAdmin bool `json:"allow_admin"`
	// MaxSessionsPerUser caps the open terminals of one user.
	MaxSessionsPerUser int `json:"max_sessions_per_user"`
	// MaxSessions caps the open terminals of all users together.
	MaxSessions int `json:"max_sessions"`
	// IdleTimeout closes a terminal without input or output for this long. Zero disables it.
	IdleTimeout model.Duration `json:"idle_timeout"`
	// MaxSessionDuration closes a terminal after this long. A terminal also closes when the login
	// session it was opened with expires.
	MaxSessionDuration model.Duration `json:"max_session_duration"`
	// RecheckInterval is how often the master checks that the login session of an open terminal
	// is still valid and that its user may still use the shell.
	RecheckInterval model.Duration `json:"recheck_interval"`
	// TrustedProxies lists the reverse proxies (IP addresses or CIDR ranges) whose X-Real-IP
	// header the terminal's audit log uses as the client address.
	TrustedProxies []string `json:"trusted_proxies"`
}

// DefaultShellTerminalConfig returns the default browser terminal settings.
func DefaultShellTerminalConfig() ShellTerminalConfig {
	return ShellTerminalConfig{
		AllowAdmin:         true,
		MaxSessionsPerUser: 8,
		MaxSessions:        256,
		IdleTimeout:        model.Duration(time.Hour),
		MaxSessionDuration: model.Duration(24 * time.Hour),
		RecheckInterval:    model.Duration(time.Minute),
	}
}

// Validate implements the check.Validatable interface.
func (c ShellTerminalConfig) Validate() []error {
	var errs []error
	if c.MaxSessionsPerUser <= 0 {
		errs = append(errs, fmt.Errorf("shell_terminal.max_sessions_per_user must be positive"))
	}
	if c.MaxSessions <= 0 {
		errs = append(errs, fmt.Errorf("shell_terminal.max_sessions must be positive"))
	}
	if c.IdleTimeout < 0 {
		errs = append(errs, fmt.Errorf("shell_terminal.idle_timeout must not be negative"))
	}
	if c.MaxSessionDuration <= 0 {
		errs = append(errs, fmt.Errorf("shell_terminal.max_session_duration must be positive"))
	}
	if r := time.Duration(c.RecheckInterval); r < time.Second || r > 5*time.Minute {
		errs = append(errs, fmt.Errorf("shell_terminal.recheck_interval must be between 1s and 5m"))
	}
	for _, p := range c.TrustedProxies {
		if strings.Contains(p, "/") {
			if _, _, err := net.ParseCIDR(p); err != nil {
				errs = append(errs, fmt.Errorf("shell_terminal.trusted_proxies: %w", err))
			}
		} else if net.ParseIP(p) == nil {
			errs = append(errs, fmt.Errorf("shell_terminal.trusted_proxies: invalid IP address %q", p))
		}
	}
	return errs
}
