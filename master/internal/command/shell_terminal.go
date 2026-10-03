package command

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// ErrShellTerminalNotFound is returned for a task that is not a shell in the registry.
var ErrShellTerminalNotFound = errors.New("shell not found")

// ShellTerminalTarget is what the browser terminal needs to know about a shell. It holds the
// shell's private key: never send it to a client.
type ShellTerminalTarget struct {
	TaskID       model.TaskID
	AllocationID model.AllocationID
	OwnerID      model.UserID
	WorkspaceID  model.AccessScopeID
	// LoginUser is the user the terminal logs in as: the shell's agent user, or root.
	LoginUser  string
	PrivateKey []byte
	PublicKey  []byte

	State model.AllocationState
	Ready bool
	// Ended is true once the shell's allocation has exited.
	Ended bool
	// SSHAddr is the host:port of the shell's sshd, taken from the addresses its allocation
	// reported, or empty while it has none.
	SSHAddr string
}

// ShellTerminalTarget returns a snapshot of a shell for the browser terminal. The sshd address
// comes from the allocation's own container addresses (as reported by the agent, or the posted
// proxy address on Kubernetes and Slurm), the same addresses the master registers for its proxy.
func (cs *CommandService) ShellTerminalTarget(id model.TaskID) (*ShellTerminalTarget, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	c, err := cs.getNTSC(id, model.TaskTypeShell)
	if err != nil {
		return nil, ErrShellTerminalNotFound
	}
	return c.shellTerminalTarget()
}

func (c *Command) shellTerminalTarget() (*ShellTerminalTarget, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.Base.Owner == nil || c.Metadata.PrivateKey == nil || c.Metadata.PublicKey == nil {
		return nil, fmt.Errorf("shell %s has no owner or keys", c.taskID)
	}

	t := &ShellTerminalTarget{
		TaskID:       c.taskID,
		AllocationID: c.allocationID,
		OwnerID:      c.Base.Owner.ID,
		WorkspaceID:  c.Metadata.WorkspaceID,
		// The same rule as `det shell open`.
		LoginUser:  "root",
		PrivateKey: []byte(*c.Metadata.PrivateKey),
		PublicKey:  []byte(*c.Metadata.PublicKey),
		Ended:      c.exitStatus != nil,
	}
	if c.Base.AgentUserGroup != nil && c.Base.AgentUserGroup.User != "" {
		t.LoginUser = c.Base.AgentUserGroup.User
	}

	allo := c.refreshAllocationState()
	t.State = allo.State
	t.Ready = allo.Ready
	if t.Ended || allo.State == model.AllocationStateTerminated {
		t.Ended = true
		return t, nil
	}

	t.SSHAddr = shellSSHDAddr(c.Base.ExtraProxyPorts, allo.Addresses)
	return t, nil
}

// shellSSHDAddr returns the host:port at which the master reaches a shell's sshd: the address of
// the container port that LaunchShell registered as the shell's default TCP proxy port.
func shellSSHDAddr(
	ports expconf.ProxyPortsConfig, addresses map[sproto.ResourcesID][]cproto.Address,
) string {
	port := 0
	for _, pp := range schemas.WithDefaults(ports) {
		if pp.DefaultServiceID() && pp.ProxyTCP() {
			port = pp.ProxyPort()
			break
		}
	}
	if port == 0 {
		return ""
	}
	for _, addrs := range addresses {
		for _, a := range addrs {
			if a.ContainerPort == port && a.HostIP != "" && a.HostPort > 0 {
				return net.JoinHostPort(a.HostIP, strconv.Itoa(a.HostPort))
			}
		}
	}
	return ""
}
