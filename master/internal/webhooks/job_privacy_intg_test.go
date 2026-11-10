//go:build integration

package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/jobprivacy"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// shareJobKinds sets a job's title and logs audiences as its owner: all users or the owner only.
func shareJobKinds(
	ctx context.Context, t *testing.T, owner model.UserID, jobID model.JobID, title, logs bool,
) {
	t.Helper()
	_, err := jobprivacy.PatchSharing(ctx, jobprivacy.Viewer{ID: owner}, jobID,
		jobprivacy.SharingPatch{Title: &title, Logs: &logs})
	require.NoError(t, err)
}

// eventPayloads returns the payloads of the queued events for a URL.
func eventPayloads(ctx context.Context, t *testing.T, url string) []string {
	t.Helper()
	var es []Event
	require.NoError(t, db.Bun().NewSelect().Model(&es).Where("url = ?", url).Scan(ctx))
	payloads := make([]string, 0, len(es))
	for _, e := range es {
		payloads = append(payloads, string(e.Payload))
	}
	return payloads
}

// onePayload returns the only queued payload for a URL.
func onePayload(ctx context.Context, t *testing.T, url string) string {
	t.Helper()
	payloads := eventPayloads(ctx, t, url)
	require.Len(t, payloads, 1)
	return payloads[0]
}

// experimentData returns the experiment of a default webhook payload.
func experimentData(t *testing.T, payload string) map[string]any {
	t.Helper()
	var p struct {
		Data struct {
			Experiment map[string]any `json:"experiment"`
		} `json:"event_data"`
	}
	require.NoError(t, json.Unmarshal([]byte(payload), &p))
	require.NotNil(t, p.Data.Experiment)
	return p.Data.Experiment
}

// clearEvents deletes the queued events and keeps the task log dedup rows.
func clearEvents(ctx context.Context, t *testing.T) {
	t.Helper()
	_, err := db.Bun().NewDelete().Model((*Event)(nil)).Where("true").Exec(ctx)
	require.NoError(t, err)
}

// privacyWebhooks are a workspace and a global webhook of each type with one trigger each.
type privacyWebhooks struct {
	workspaceDefault, workspaceSlack, globalDefault, globalSlack *Webhook
}

func (w privacyWebhooks) all() []*Webhook {
	return []*Webhook{w.workspaceDefault, w.workspaceSlack, w.globalDefault, w.globalSlack}
}

func newPrivacyWebhooks(
	ctx context.Context, t *testing.T, add func(*Webhook) error, trigger func() *Trigger,
) privacyWebhooks {
	t.Helper()
	mk := func(wt WebhookType, workspaceID *int32) *Webhook {
		w := &Webhook{
			URL:         uuid.NewString(),
			WebhookType: wt,
			Mode:        WebhookModeWorkspace,
			WorkspaceID: workspaceID,
			Name:        uuid.NewString(),
			Triggers:    Triggers{trigger()},
		}
		require.NoError(t, add(w))
		return w
	}
	ws := ptrs.Ptr(int32(model.DefaultWorkspaceID))
	return privacyWebhooks{
		workspaceDefault: mk(WebhookTypeDefault, ws),
		workspaceSlack:   mk(WebhookTypeSlack, ws),
		globalDefault:    mk(WebhookTypeDefault, nil),
		globalSlack:      mk(WebhookTypeSlack, nil),
	}
}

// requireExperimentPayloads checks the events of one experiment event: every webhook got one,
// the global ones carry the name and the workspace ones carry it only when showName is set.
func requireExperimentPayloads(
	ctx context.Context, t *testing.T, ws privacyWebhooks, exp *model.Experiment, name string,
	showName bool,
) {
	t.Helper()
	for _, w := range []*Webhook{ws.globalDefault, ws.globalSlack} {
		require.Contains(t, onePayload(ctx, t, w.URL), name, "global webhooks carry the name")
	}
	slack := onePayload(ctx, t, ws.workspaceSlack.URL)
	data := experimentData(t, onePayload(ctx, t, ws.workspaceDefault.URL))
	require.InDelta(t, float64(exp.ID), data["id"], 0)
	require.Equal(t, string(model.CompletedState), data["state"])
	if showName {
		require.Equal(t, name, data["name"])
		require.Contains(t, slack, fmt.Sprintf("%s (#%d)", name, exp.ID))
		return
	}
	require.NotContains(t, data, "name")
	require.NotContains(t, onePayload(ctx, t, ws.workspaceDefault.URL), name)
	require.NotContains(t, slack, name)
	require.Contains(t, slack, fmt.Sprintf("#%d", exp.ID))
}

func TestWorkspaceWebhooksExperimentName(t *testing.T) {
	ctx := context.Background()
	clearWebhooksTables(ctx, t)
	t.Cleanup(func() { clearWebhooksTables(ctx, t) })
	singletonShipper = &shipper{wake: make(chan<- struct{})} // mock shipper

	user := db.RequireMockUser(t, pgDB)
	exp := db.RequireMockExperiment(t, pgDB, user)
	exp.State = model.CompletedState
	name := "secret-" + uuid.NewString()
	config := schemas.WithDefaults(expconf.ExperimentConfig{
		RawName: expconf.Name{RawString: &name},
	})

	ws := newPrivacyWebhooks(ctx, t, func(w *Webhook) error { return AddWebhook(ctx, w) },
		func() *Trigger {
			return &Trigger{
				TriggerType: TriggerTypeStateChange,
				Condition:   map[string]any{"state": model.CompletedState},
			}
		})
	report := func(t *testing.T) {
		clearEvents(ctx, t)
		require.NoError(t, ReportExperimentStateChanged(ctx, *exp, config))
	}

	t.Run("a hidden title is left out of workspace webhooks", func(t *testing.T) {
		shareJobKinds(ctx, t, user.ID, exp.JobID, false, false)
		report(t)
		requireExperimentPayloads(ctx, t, ws, exp, name, false)
	})

	t.Run("a shared title is in workspace webhooks", func(t *testing.T) {
		shareJobKinds(ctx, t, user.ID, exp.JobID, true, true)
		report(t)
		requireExperimentPayloads(ctx, t, ws, exp, name, true)
	})

	t.Run("a policy read failure leaves the name out", func(t *testing.T) {
		restore := jobprivacy.FailPolicyReadsForTests(errors.New("the policy table is gone"))
		defer restore()
		report(t)
		requireExperimentPayloads(ctx, t, ws, exp, name, false)
	})
}

func TestWorkspaceWebhooksCustomTriggerName(t *testing.T) {
	ctx := context.Background()
	clearWebhooksTables(ctx, t)
	t.Cleanup(func() { clearWebhooksTables(ctx, t) })
	singletonShipper = &shipper{wake: make(chan<- struct{})} // mock shipper

	ws := newPrivacyWebhooks(ctx, t, func(w *Webhook) error { return AddWebhook(ctx, w) },
		func() *Trigger { return &Trigger{TriggerType: TriggerTypeCustom, Condition: map[string]any{}} })
	var names []string
	for _, w := range ws.all() {
		names = append(names, w.Name)
	}

	user := db.RequireMockUser(t, pgDB)
	exp := db.RequireMockExperimentParams(t, pgDB, user, db.MockExperimentParams{
		Integrations: &expconf.IntegrationsConfigV0{
			Webhooks: &expconf.WebhooksConfigV0{WebhookName: &names},
		},
	}, db.DefaultProjectID)
	name := "secret-" + uuid.NewString()
	_, err := db.Bun().NewRaw(`UPDATE experiments SET config = config || jsonb_build_object('name', ?::text)
		WHERE id = ?`, name, exp.ID).Exec(ctx)
	require.NoError(t, err)
	trial, _ := db.RequireMockTrial(t, pgDB, exp)
	data := CustomTriggerData{Title: "alert title", Description: "alert text", Level: "INFO"}

	shareJobKinds(ctx, t, user.ID, exp.JobID, false, false)
	require.NoError(t, handleCustomTriggerData(ctx, data, exp.ID, &trial.ID))
	for _, w := range []*Webhook{ws.globalDefault, ws.globalSlack} {
		require.Contains(t, onePayload(ctx, t, w.URL), name)
	}
	payload := onePayload(ctx, t, ws.workspaceDefault.URL)
	require.NotContains(t, payload, name)
	require.NotContains(t, experimentData(t, payload), "name")
	require.Contains(t, payload, "alert text")
	require.NotContains(t, onePayload(ctx, t, ws.workspaceSlack.URL), name)

	clearEvents(ctx, t)
	shareJobKinds(ctx, t, user.ID, exp.JobID, true, true)
	require.NoError(t, handleCustomTriggerData(ctx, data, exp.ID, &trial.ID))
	require.Equal(t, name, experimentData(t, onePayload(ctx, t, ws.workspaceDefault.URL))["name"])
	require.Contains(t, onePayload(ctx, t, ws.workspaceSlack.URL), name)
}

func TestWorkspaceWebhooksTaskLogs(t *testing.T) {
	ctx := context.Background()
	clearWebhooksTables(ctx, t)
	t.Cleanup(func() { clearWebhooksTables(ctx, t) })
	singletonShipper = &shipper{wake: make(chan<- struct{})} // mock shipper

	manager, err := New(ctx)
	require.NoError(t, err)
	regex := "secret-line-" + uuid.NewString()
	ws := newPrivacyWebhooks(ctx, t, func(w *Webhook) error { return manager.addWebhook(ctx, w) },
		func() *Trigger {
			return &Trigger{TriggerType: TriggerTypeTaskLog, Condition: map[string]any{"regex": regex}}
		})

	user := db.RequireMockUser(t, pgDB)
	exp := db.RequireMockExperiment(t, pgDB, user)
	// newTask returns a task whose job shares its title and logs as given.
	newTask := func(t *testing.T, logsShared bool) *model.Task {
		_, task := db.RequireMockTrial(t, pgDB, exp)
		shareJobKinds(ctx, t, user.ID, *task.JobID, true, logsShared)
		return task
	}
	scan := func(t *testing.T, task *model.Task) {
		clearEvents(ctx, t)
		require.NoError(t, manager.scanLogs(ctx, []*model.TaskLog{
			{TaskID: string(task.TaskID), AgentID: ptrs.Ptr("node"), Log: regex},
		}, model.AccessScopeID(model.DefaultWorkspaceID), nil))
	}
	requireFired := func(t *testing.T, workspace bool) {
		t.Helper()
		for _, w := range []*Webhook{ws.globalDefault, ws.globalSlack} {
			require.Contains(t, onePayload(ctx, t, w.URL), regex, "global webhooks fire")
		}
		for _, w := range []*Webhook{ws.workspaceDefault, ws.workspaceSlack} {
			if workspace {
				require.Contains(t, onePayload(ctx, t, w.URL), regex)
			} else {
				require.Empty(t, eventPayloads(ctx, t, w.URL))
			}
		}
	}
	// requireOnlyWorkspaceFired checks a scan of a task that fired the global webhooks before.
	requireOnlyWorkspaceFired := func(t *testing.T) {
		t.Helper()
		for _, w := range ws.all() {
			if w.WorkspaceID != nil {
				require.Len(t, eventPayloads(ctx, t, w.URL), 1)
			} else {
				require.Empty(t, eventPayloads(ctx, t, w.URL))
			}
		}
	}

	t.Run("hidden logs do not fire workspace webhooks until they are shared", func(t *testing.T) {
		task := newTask(t, false)
		scan(t, task)
		requireFired(t, false)

		shareJobKinds(ctx, t, user.ID, *task.JobID, true, true)
		scan(t, task)
		requireOnlyWorkspaceFired(t)
	})

	t.Run("shared logs fire workspace webhooks", func(t *testing.T) {
		scan(t, newTask(t, true))
		requireFired(t, true)
	})

	t.Run("a policy forbidding logs keeps them from workspace webhooks", func(t *testing.T) {
		task := newTask(t, true)
		p, err := jobprivacy.ReadPolicy(ctx, nil)
		require.NoError(t, err)
		forbid := map[jobprivacy.Kind]jobprivacy.KindPolicy{
			jobprivacy.Title: p.Title, jobprivacy.Logs: {NewJobAudience: []string{}},
			jobprivacy.Config: p.Config,
		}
		_, err = jobprivacy.WritePolicy(ctx, user.ID, forbid)
		require.NoError(t, err)
		defer func() {
			_, err := jobprivacy.WritePolicy(ctx, user.ID, map[jobprivacy.Kind]jobprivacy.KindPolicy{
				jobprivacy.Title: p.Title, jobprivacy.Logs: p.Logs, jobprivacy.Config: p.Config,
			})
			require.NoError(t, err)
		}()
		scan(t, task)
		requireFired(t, false)
	})

	t.Run("a policy read failure keeps logs from workspace webhooks", func(t *testing.T) {
		task := newTask(t, true)
		restore := jobprivacy.FailPolicyReadsForTests(errors.New("the policy table is gone"))
		defer restore()
		scan(t, task)
		restore()
		requireFired(t, false)

		scan(t, task)
		requireOnlyWorkspaceFired(t)
	})
}
