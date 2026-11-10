//go:build integration
// +build integration

package internal

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/webhooks"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/taskv1"
)

// TestJobPrivacyWebhookUnmanagedTrialLogs checks that the task logs of an unmanaged trial, whose
// task has no job of its own, fire a workspace webhook as its experiment's job shares the logs:
// shared, the event is queued; hidden, no event is queued and the trigger stays unused for the
// task; shared again, the event is queued.
func TestJobPrivacyWebhookUnmanagedTrialLogs(t *testing.T) {
	var aux, compute string
	api, admin, _ := setupAPITest(t, nil, mockRMWithResolver(resolveOmittedPoolTo(&aux, &compute)))
	lockJobPrivacyPolicy(t)
	aux = accessTestPool(t, "aux", admin, false)
	compute = accessTestPool(t, "compute", admin, false)
	owner := addGCTestOwner(t)
	ownerCtx := userContext(t, owner)
	ctx := context.Background()

	restore, err := webhooks.UseManagerForTests(ctx)
	require.NoError(t, err)
	t.Cleanup(restore)
	regex := "webhook-line-" + uuid.NewString()
	addWebhook := func(workspaceID *int32) *webhooks.Webhook {
		w := &webhooks.Webhook{
			URL:         uuid.NewString(),
			WebhookType: webhooks.WebhookTypeDefault,
			Mode:        webhooks.WebhookModeWorkspace,
			WorkspaceID: workspaceID,
			Name:        uuid.NewString(),
			Triggers: webhooks.Triggers{{
				TriggerType: webhooks.TriggerTypeTaskLog,
				Condition:   map[string]any{"regex": regex},
			}},
		}
		require.NoError(t, webhooks.AddWebhook(ctx, w))
		t.Cleanup(func() { require.NoError(t, webhooks.DeleteWebhook(ctx, w.ID)) })
		return w
	}
	workspace := addWebhook(ptrs.Ptr(int32(model.DefaultWorkspaceID)))
	global := addWebhook(nil)

	exp, err := api.PutExperiment(ownerCtx, &apiv1.PutExperimentRequest{
		CreateExperimentRequest: &apiv1.CreateExperimentRequest{
			Config: accessTestExperimentConfig(compute), Unmanaged: ptrs.Ptr(true), ProjectId: 1,
		},
		ExternalExperimentId: uuid.NewString(),
	})
	require.NoError(t, err)
	created, err := api.CreateTrial(ownerCtx, &apiv1.CreateTrialRequest{
		ExperimentId: exp.Experiment.Id, Unmanaged: true,
	})
	require.NoError(t, err)
	put, err := api.PutTrial(ownerCtx, &apiv1.PutTrialRequest{
		CreateTrialRequest: &apiv1.CreateTrialRequest{
			ExperimentId: exp.Experiment.Id, Unmanaged: true,
		},
		ExternalTrialId: uuid.NewString(),
	})
	require.NoError(t, err)
	// Each phase takes a task whose triggers are unused, since a trigger fires once per task.
	sharedTask := created.Trial.TaskIds[0]
	hiddenTask := put.Trial.TaskIds[0]
	for _, id := range []string{sharedTask, hiddenTask} {
		task, err := db.TaskByID(ctx, model.TaskID(id))
		require.NoError(t, err)
		require.Nil(t, task.JobID, "the task of an unmanaged trial has no job of its own")
	}

	postLog := func(taskID string) {
		t.Helper()
		_, err := api.PostTaskLogs(ownerCtx, &apiv1.PostTaskLogsRequest{
			Logs: []*taskv1.TaskLog{{
				TaskId: taskID, AgentId: ptrs.Ptr("a1"), Log: "a line with " + regex,
			}},
		})
		require.NoError(t, err)
	}
	events := func(w *webhooks.Webhook) int {
		t.Helper()
		n, err := db.Bun().NewSelect().Model((*webhooks.Event)(nil)).
			Where("url = ?", w.URL).Count(ctx)
		require.NoError(t, err)
		return n
	}
	triggerUsed := func(w *webhooks.Webhook, taskID string) bool {
		t.Helper()
		exists, err := db.Bun().NewSelect().Table("webhook_task_log_triggers").
			Where("task_id = ? AND trigger_id = ?", taskID, w.Triggers[0].ID).Exists(ctx)
		require.NoError(t, err)
		return exists
	}

	jobID := exp.Experiment.JobId
	shareJob(ownerCtx, t, api, jobID, true, true, true)
	postLog(sharedTask)
	require.Equal(t, 1, events(workspace), "shared: the workspace webhook fires")
	require.Equal(t, 1, events(global))

	shareJob(ownerCtx, t, api, jobID, true, false, true)
	postLog(hiddenTask)
	require.Equal(t, 2, events(global), "the log line matched")
	require.Equal(t, 1, events(workspace), "logs hidden: the workspace webhook does not fire")
	require.False(t, triggerUsed(workspace, hiddenTask),
		"logs hidden: the workspace trigger stays unused for the task")

	shareJob(ownerCtx, t, api, jobID, true, true, true)
	postLog(hiddenTask)
	require.Equal(t, 2, events(workspace), "shared again: the workspace webhook fires")
	require.True(t, triggerUsed(workspace, hiddenTask))
}
