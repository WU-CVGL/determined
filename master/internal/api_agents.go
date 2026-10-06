package internal

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pkg/errors"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/cluster"
	"github.com/determined-ai/determined/master/internal/grpcutil"
	"github.com/determined-ai/determined/master/internal/rm/rmerrors"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/agentv1"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

func (a *apiServer) GetAgents(
	ctx context.Context, req *apiv1.GetAgentsRequest,
) (*apiv1.GetAgentsResponse, error) {
	resp, err := a.m.rm.GetAgents()
	if err != nil {
		return nil, err
	}

	user, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, err
	}

	permErr, err := cluster.AuthZProvider.Get().CanGetSensitiveAgentInfo(ctx, user)
	switch {
	case err != nil:
		return nil, err
	case permErr != nil:
		for _, agent := range resp.Agents {
			if err := authz.ObfuscateAgent(agent); err != nil {
				return nil, err
			}
		}
	}

	// PERF: can perhaps be done before RBAC.
	for _, agent := range resp.Agents {
		if agent.SlotStats == nil {
			agent.SlotStats = model.SummarizeSlots(agent.Slots)
		}
		if req.ExcludeSlots {
			agent.Slots = nil
			agent.GpuTopology = nil
		} else {
			classifyGPUHealth(agent.GpuTopology)
		}
		if req.ExcludeContainers {
			agent.Containers = nil
		}
	}

	api.Sort(resp.Agents, req.OrderBy, req.SortBy, apiv1.GetAgentsRequest_SORT_BY_ID)
	return resp, api.Paginate(&resp.Pagination, &resp.Agents, req.Offset, req.Limit)
}

func (a *apiServer) GetAgent(
	ctx context.Context, req *apiv1.GetAgentRequest,
) (*apiv1.GetAgentResponse, error) {
	user, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, err
	}

	resp, err := a.m.rm.GetAgent(req)
	if err != nil {
		return nil, err
	}

	permErr, err := cluster.AuthZProvider.Get().CanGetSensitiveAgentInfo(ctx, user)
	switch {
	case err != nil:
		return nil, err
	case permErr != nil:
		if err := authz.ObfuscateAgent(resp.Agent); err != nil {
			return nil, err
		}
	}
	classifyGPUHealth(resp.Agent.GetGpuTopology())
	return resp, nil
}

// classifyGPUHealth sets the health of every GPU of an agent. The first matching row wins:
//   - ERROR: an NVML health call failed at the agent's last start;
//   - LINK_BELOW_MAX: at agent start, the current and maximum link widths were both known and
//     current < max;
//   - OK: the topology is known, and at agent start both widths were known and equal;
//   - UNKNOWN (unspecified): anything else.
//
// The link generation never changes the state, and an excluded GPU gets its own state by the same
// rules. It is the only place the state is computed, so the CLI and the WebUI never disagree.
func classifyGPUHealth(topo *agentv1.GpuTopology) {
	if topo == nil {
		return
	}
	known := topo.UnknownReason == ""
	for _, g := range topo.Gpus {
		widthsKnown := g.PcieLinkWidth > 0 && g.PcieLinkWidthMax > 0
		switch {
		case g.NvmlError != "":
			g.Health = agentv1.GpuHealth_GPU_HEALTH_ERROR
		case widthsKnown && g.PcieLinkWidth < g.PcieLinkWidthMax:
			g.Health = agentv1.GpuHealth_GPU_HEALTH_LINK_BELOW_MAX
		case known && widthsKnown && g.PcieLinkWidth == g.PcieLinkWidthMax:
			g.Health = agentv1.GpuHealth_GPU_HEALTH_OK
		default:
			g.Health = agentv1.GpuHealth_GPU_HEALTH_UNSPECIFIED
		}
	}
}

func (a *apiServer) GetSlots(
	ctx context.Context, req *apiv1.GetSlotsRequest,
) (*apiv1.GetSlotsResponse, error) {
	user, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, err
	}

	resp, err := a.m.rm.GetSlots(req)
	if err != nil {
		return nil, err
	}

	permErr, err := cluster.AuthZProvider.Get().CanGetSensitiveAgentInfo(ctx, user)
	switch {
	case err != nil:
		return nil, err
	case permErr != nil:
		for _, slot := range resp.Slots {
			if err := authz.ObfuscateSlot(slot); err != nil {
				return nil, err
			}
		}
	}

	return resp, nil
}

func (a *apiServer) GetSlot(
	ctx context.Context, req *apiv1.GetSlotRequest,
) (*apiv1.GetSlotResponse, error) {
	user, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, err
	}

	resp, err := a.m.rm.GetSlot(req)
	if err != nil {
		return nil, err
	}

	permErr, err := cluster.AuthZProvider.Get().CanGetSensitiveAgentInfo(ctx, user)
	switch {
	case err != nil:
		return nil, err
	case permErr != nil:
		if err := authz.ObfuscateSlot(resp.Slot); err != nil {
			return resp, err
		}
	}
	return resp, nil
}

func (a *apiServer) canUpdateAgents(ctx context.Context) error {
	user, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return err
	}
	permErr, err := cluster.AuthZProvider.Get().CanUpdateAgents(ctx, user)
	switch {
	case err != nil:
		return err
	case permErr != nil:
		return status.Error(codes.PermissionDenied, permErr.Error())
	}
	return nil
}

// gpuTopologyForUser prepares the GPU topology of an agent enable or disable response. Updating
// agents and viewing sensitive agent information are separate permissions, and GPU UUIDs and bus
// ids are sensitive (authz.ObfuscateAgent): a user without that permission gets no topology, as
// from GetAgents and GetAgent. Otherwise the health is classified. The rest of these responses is
// left as it was.
func (a *apiServer) gpuTopologyForUser(ctx context.Context, agent *agentv1.Agent) error {
	if agent.GetGpuTopology() == nil {
		return nil
	}
	user, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return err
	}
	permErr, err := cluster.AuthZProvider.Get().CanGetSensitiveAgentInfo(ctx, user)
	switch {
	case err != nil:
		return err
	case permErr != nil:
		agent.GpuTopology = nil
	default:
		classifyGPUHealth(agent.GpuTopology)
	}
	return nil
}

func (a *apiServer) EnableAgent(
	ctx context.Context, req *apiv1.EnableAgentRequest,
) (resp *apiv1.EnableAgentResponse, err error) {
	if err := a.canUpdateAgents(ctx); err != nil {
		return nil, err
	}
	resp, err = a.m.rm.EnableAgent(req)
	if err != nil {
		return resp, err
	}
	if err := a.gpuTopologyForUser(ctx, resp.GetAgent()); err != nil {
		return nil, err
	}
	return resp, nil
}

func (a *apiServer) DisableAgent(
	ctx context.Context, req *apiv1.DisableAgentRequest,
) (resp *apiv1.DisableAgentResponse, err error) {
	if err := a.canUpdateAgents(ctx); err != nil {
		return nil, err
	}
	resp, err = a.m.rm.DisableAgent(req)
	if err != nil {
		return resp, err
	}
	if err := a.gpuTopologyForUser(ctx, resp.GetAgent()); err != nil {
		return nil, err
	}
	return resp, nil
}

func (a *apiServer) EnableSlot(
	ctx context.Context, req *apiv1.EnableSlotRequest,
) (resp *apiv1.EnableSlotResponse, err error) {
	if err := a.canUpdateAgents(ctx); err != nil {
		return nil, err
	}

	resp, err = a.m.rm.EnableSlot(req)
	switch {
	case errors.Is(err, rmerrors.ErrNotSupported):
		return resp, status.Error(codes.Unimplemented, err.Error())
	case err != nil:
		return nil, err
	default:
		return resp, nil
	}
}

func (a *apiServer) DisableSlot(
	ctx context.Context, req *apiv1.DisableSlotRequest,
) (resp *apiv1.DisableSlotResponse, err error) {
	if err := a.canUpdateAgents(ctx); err != nil {
		return nil, err
	}

	resp, err = a.m.rm.DisableSlot(req)
	switch {
	case errors.Is(err, rmerrors.ErrNotSupported):
		return resp, status.Error(codes.Unimplemented, err.Error())
	case err != nil:
		return nil, err
	default:
		return resp, nil
	}
}
