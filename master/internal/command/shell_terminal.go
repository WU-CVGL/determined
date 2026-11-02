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

// ErrShellTerminalNotFound is returned for a task that is not in the registry or runs no sshd
// that the master can log in to.
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

// ShellTerminalTarget returns a snapshot of a task with an sshd for the browser terminal. The sshd
// address comes from the allocation's own container addresses (as reported by the agent, or the
// posted proxy address on Kubernetes and Slurm), the same addresses the master registers for its
// proxy.
func (cs *CommandService) ShellTerminalTarget(id model.TaskID) (*ShellTerminalTarget, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	c, err := cs.getSSHTask(id)
	if err != nil {
		return nil, ErrShellTerminalNotFound
	}
	return c.shellTerminalTarget()
}

// getSSHTask returns an NTSC that runs an sshd that the master can log in to with the task's key
// pair: every shell, and any other NTSC that was launched with keys and an sshd port.
func (cs *CommandService) getSSHTask(id model.TaskID) (*Command, error) {
	c, ok := cs.commands[id]
	if !ok || !c.hasSSHD() {
		return nil, fmt.Errorf("get NTSC %s with an sshd: not found", id)
	}
	return c, nil
}

// hasSSHD reports whether the master holds the task's key pair and knows its sshd port. Shells
// saved before the master recorded the port know it as their default TCP proxy port.
func (c *Command) hasSSHD() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.Metadata.PrivateKey == nil || c.Metadata.PublicKey == nil {
		return false
	}
	return c.taskType == model.TaskTypeShell || c.Keys != nil && c.SSHDPort != 0
}

func (c *Command) shellTerminalTarget() (*ShellTerminalTarget, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.Base.Owner == nil || c.Metadata.PrivateKey == nil || c.Metadata.PublicKey == nil {
		return nil, fmt.Errorf("task %s has no owner or keys", c.taskID)
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

	t.SSHAddr = shellSSHDAddr(c.SSHDPort, c.Base.ExtraProxyPorts, allo.Addresses)
	return t, nil
}

// shellSSHDAddr returns the host:port at which the master reaches a task's sshd: the address of
// the container port sshdPort, or, when that is unknown, of the port that LaunchShell registered
// as the shell's default TCP proxy port.
func shellSSHDAddr(
	sshdPort int, ports expconf.ProxyPortsConfig, addresses map[sproto.ResourcesID][]cproto.Address,
) string {
	port := sshdPort
	if port == 0 {
		port = shellDefaultTCPPort(ports)
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

// shellDefaultTCPPort returns the default TCP proxy port of a shell, which is its sshd's port.
func shellDefaultTCPPort(ports expconf.ProxyPortsConfig) int {
	for _, pp := range schemas.WithDefaults(ports) {
		if pp.DefaultServiceID() && pp.ProxyTCP() {
			return pp.ProxyPort()
		}
	}
	return 0
}
