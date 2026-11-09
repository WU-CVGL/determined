package internal

import (
	"github.com/labstack/echo/v4"

	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/context"
	expauth "github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/model"
)

func (m *Master) getTasks(c echo.Context) (interface{}, error) {
	summary, err := m.rm.GetAllocationSummaries()
	if err != nil {
		return nil, err
	}

	curUser := c.(*context.DetContext).MustGetUser()
	ctx := c.Request().Context()
	for allocationID, allocationSummary := range summary {
		isExp, exp, err := expFromTaskID(ctx, allocationSummary.TaskID)
		if err != nil {
			return nil, err
		}

		if !isExp {
			_, _, err = canAccessNTSCTask(ctx, curUser, summary[allocationID].TaskID)
		} else {
			err = expauth.AuthZProvider.Get().CanGetExperiment(ctx, curUser, exp)
		}
		if authz.IsPermissionDenied(err) {
			delete(summary, allocationID)
		} else if err != nil {
			return nil, err
		}
	}

	// The name of a task whose title the user may not read is omitted, as in GetTasks. The
	// response is a copy, so the resource manager's summaries are never changed.
	taskIDs := make([]model.TaskID, 0, len(summary))
	for _, s := range summary {
		taskIDs = append(taskIDs, s.TaskID)
	}
	caps, err := allocationSummaryCaps(ctx, curUser, taskIDs)
	if err != nil {
		return nil, err
	}
	resp := make(map[model.AllocationID]sproto.AllocationSummary, len(summary))
	for allocationID, s := range summary {
		if !caps[s.TaskID].ViewTitle {
			s.Name = ""
		}
		resp[allocationID] = s
	}
	return resp, nil
}
