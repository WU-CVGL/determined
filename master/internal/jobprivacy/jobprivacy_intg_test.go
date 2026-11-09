//go:build integration
// +build integration

package jobprivacy

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/model"
)

func TestMain(m *testing.M) {
	pgDB, _, err := db.ResolveTestPostgres()
	if err != nil {
		log.Panicln(err)
	}
	if err := db.MigrateTestPostgres(pgDB, "file://../../static/migrations", "up"); err != nil {
		log.Panicln(err)
	}
	if err := etc.SetRootPath("../../static/srv"); err != nil {
		log.Panicln(err)
	}
	config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{Type: "basic"}
	// The tests need the seeded policy, which tests of other packages may change.
	unlock, err := db.LockJobPrivacyPolicyForTests(context.Background())
	if err != nil {
		log.Panicln(err)
	}
	code := m.Run()
	unlock()
	os.Exit(code)
}

// setPreferences writes a user's preference row.
func setPreferences(
	ctx context.Context, t *testing.T, idb bun.IDB, userID model.UserID,
	titles, logs bool, newTitles, newLogs []string,
) {
	t.Helper()
	_, err := idb.NewRaw(`INSERT INTO user_job_privacy
		(user_id, share_titles, share_logs, new_job_title_audience, new_job_logs_audience)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET share_titles = EXCLUDED.share_titles,
			share_logs = EXCLUDED.share_logs,
			new_job_title_audience = EXCLUDED.new_job_title_audience,
			new_job_logs_audience = EXCLUDED.new_job_logs_audience`,
		userID, titles, logs, pgdialect.Array(newTitles), pgdialect.Array(newLogs)).Exec(ctx)
	require.NoError(t, err)
}

// insertJob inserts a job with the given audiences, bypassing the stamping of db.AddJobTx.
func insertJob(
	ctx context.Context, t *testing.T, ownerID *model.UserID, title, logs, cfg []string,
) model.JobID {
	t.Helper()
	id := model.NewJobID()
	_, err := db.Bun().NewRaw(`INSERT INTO jobs
		(job_id, job_type, owner_id, title_audience, logs_audience, config_audience)
		VALUES (?, 'COMMAND', ?, ?, ?, ?)`,
		id, ownerID, pgdialect.Array(title), pgdialect.Array(logs), pgdialect.Array(cfg)).Exec(ctx)
	require.NoError(t, err)
	return id
}

// TestSQLMatchesEffective is the agreement of the two forms of the rule: for every viewer and
// policy, the SQL expressions of each job equal Effective on the facts that Facts reads.
func TestSQLMatchesEffective(t *testing.T) {
	ctx := context.Background()
	var jobIDs []model.JobID
	var owners []model.User
	for _, sw := range switchSets {
		u := db.RequireMockUser(t, db.SingleDB())
		owners = append(owners, u)
		if sw.hasPreferences {
			setPreferences(ctx, t, db.Bun(), u.ID, sw.titles, sw.logs, nil, nil)
		}
	}
	ownerIDs := []*model.UserID{nil}
	for i := range owners {
		ownerIDs = append(ownerIDs, &owners[i].ID)
	}
	for _, ownerID := range ownerIDs {
		for _, ta := range audiences {
			for _, la := range audiences {
				for _, ca := range audiences {
					jobIDs = append(jobIDs, insertJob(ctx, t, ownerID, ta, la, ca))
				}
			}
		}
	}
	// A job ID without a jobs row.
	jobIDs = append(jobIDs, model.NewJobID())

	facts, err := Facts(ctx, db.Bun(), jobIDs)
	require.NoError(t, err)
	require.Len(t, facts, len(jobIDs))
	require.False(t, facts[jobIDs[len(jobIDs)-1]].Exists)

	stranger := db.RequireMockUser(t, db.SingleDB())
	viewers := []Viewer{{ID: stranger.ID}, {ID: stranger.ID, Admin: true}}
	for _, u := range owners {
		viewers = append(viewers, Viewer{ID: u.ID})
	}
	ids := make([]string, len(jobIDs))
	for i, id := range jobIDs {
		ids[i] = string(id)
	}
	for _, p := range policies() {
		for _, v := range viewers {
			e := SQL(v, p, "ids.job_id")
			var rows []struct {
				JobID model.JobID `bun:"job_id"`
				Visible
			}
			q := db.Bun().NewSelect().
				TableExpr("unnest(?::text[]) AS ids(job_id)", pgdialect.Array(ids)).
				ColumnExpr("ids.job_id")
			q = e.Columns(e.Join(q))
			require.NoError(t, q.Scan(ctx, &rows))
			require.Len(t, rows, len(jobIDs))
			for _, r := range rows {
				name := fmt.Sprintf("viewer %+v, policy %+v, job %+v", v, p, facts[r.JobID])
				require.Equal(t, Effective(v, facts[r.JobID], p), r.Caps(), name)
			}

			// The filter form agrees with the projection.
			var titled []model.JobID
			require.NoError(t, db.Bun().NewSelect().
				TableExpr("unnest(?::text[]) AS ids(job_id)", pgdialect.Array(ids)).
				ColumnExpr("ids.job_id").
				Join(e.JoinSQL()).
				Where(e.Title).
				Scan(ctx, &titled))
			want := 0
			for _, f := range facts {
				if Effective(v, f, p).ViewTitle {
					want++
				}
			}
			require.Len(t, titled, want)

			// So does the form without joins, row by row.
			var noJoin []struct {
				JobID model.JobID `bun:"job_id"`
				Visible
			}
			nj := e.NoJoin()
			require.NoError(t, nj.Columns(db.Bun().NewSelect().
				TableExpr("unnest(?::text[]) AS ids(job_id)", pgdialect.Array(ids)).
				ColumnExpr("ids.job_id")).
				Scan(ctx, &noJoin))
			require.Len(t, noJoin, len(jobIDs))
			for _, r := range noJoin {
				name := fmt.Sprintf("no join: viewer %+v, policy %+v, job %+v", v, p, facts[r.JobID])
				require.Equal(t, Effective(v, facts[r.JobID], p), r.Caps(), name)
			}
		}
	}
}

func TestReadPolicy(t *testing.T) {
	ctx := context.Background()

	// The seeded policy is public.
	p, err := ReadPolicy(ctx, nil)
	require.NoError(t, err)
	public := KindPolicy{Allowed: true, NewJobAudience: []string{AudienceAuthenticated}}
	require.Equal(t, Policy{Title: public, Logs: public, Config: public}, p)

	// A missing row forbids its kind. The row is removed in a transaction that is rolled back, so
	// no other test sees it.
	require.ErrorIs(t, db.Bun().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.NewDelete().Table("job_privacy_policy").Where("kind = 'logs'").Exec(ctx)
		require.NoError(t, err)
		p, err := ReadPolicy(ctx, tx)
		require.NoError(t, err)
		require.Equal(t, KindPolicy{NewJobAudience: []string{}}, p.Logs)
		require.Equal(t, public, p.Title)
		return sql.ErrTxDone
	}), sql.ErrTxDone)

	// A read error is Unavailable.
	tx, err := db.Bun().BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	_, err = ReadPolicy(ctx, tx)
	require.Equal(t, codes.Unavailable, status.Code(err))
}

func TestReadUserPrivacy(t *testing.T) {
	ctx := context.Background()
	u := db.RequireMockUser(t, db.SingleDB())

	got, err := ReadUserPrivacy(ctx, nil, u.ID)
	require.NoError(t, err)
	require.Equal(t, DefaultUserPrivacy, got)

	setPreferences(ctx, t, db.Bun(), u.ID, false, true, []string{}, nil)
	got, err = ReadUserPrivacy(ctx, nil, u.ID)
	require.NoError(t, err)
	require.Equal(t, UserPrivacy{ShareTitles: false, ShareLogs: true, NewJobTitles: []string{}}, got)
	require.NotNil(t, got.NewJobTitles)
	require.Nil(t, got.NewJobLogs)
}

func TestForJobsAndRequire(t *testing.T) {
	ctx := context.Background()
	owner := db.RequireMockUser(t, db.SingleDB())
	other := db.RequireMockUser(t, db.SingleDB())
	all := []string{AudienceAuthenticated}

	public := insertJob(ctx, t, &owner.ID, all, all, all)
	private := insertJob(ctx, t, &owner.ID, all, []string{}, []string{})
	legacy := insertJob(ctx, t, &owner.ID, nil, nil, nil)
	missing := model.NewJobID()

	caps, err := ForJobs(ctx, Viewer{ID: other.ID}, []model.JobID{public, private, legacy, missing})
	require.NoError(t, err)
	require.Equal(t, map[model.JobID]Caps{
		public:  {ViewTitle: true, ViewLogs: true, ViewConfig: true},
		private: {ViewTitle: true},
		legacy:  {ViewTitle: true, ViewLogs: true, ViewConfig: true},
		missing: {},
	}, caps)

	caps, err = ForJobs(ctx, Viewer{ID: owner.ID}, []model.JobID{private, missing})
	require.NoError(t, err)
	require.Equal(t, map[model.JobID]Caps{private: AllCaps, missing: {}}, caps)

	caps, err = ForJobs(ctx, Viewer{ID: other.ID, Admin: true}, []model.JobID{private, missing})
	require.NoError(t, err)
	require.Equal(t, map[model.JobID]Caps{private: AllCaps, missing: AllCaps}, caps)

	for _, tc := range []struct {
		v    Viewer
		job  model.JobID
		kind Kind
		msg  string
	}{
		{Viewer{ID: other.ID}, private, Logs, "logs not shared"},
		{Viewer{ID: other.ID}, private, Config, "config not shared"},
		{Viewer{ID: other.ID}, missing, Logs, "logs not shared"},
		{Viewer{ID: other.ID}, public, Logs, ""},
		{Viewer{ID: owner.ID}, private, Logs, ""},
		{Viewer{ID: other.ID, Admin: true}, private, Config, ""},
	} {
		err := Require(ctx, tc.v, tc.job, tc.kind)
		if tc.msg == "" {
			require.NoError(t, err)
			continue
		}
		require.Equal(t, codes.PermissionDenied, status.Code(err))
		require.Equal(t, tc.msg, status.Convert(err).Message())
	}

	// The owner's switch is a live gate over existing jobs.
	setPreferences(ctx, t, db.Bun(), owner.ID, true, false, nil, nil)
	err = Require(ctx, Viewer{ID: other.ID}, public, Logs)
	require.Equal(t, "logs not shared", status.Convert(err).Message())
	require.NoError(t, Require(ctx, Viewer{ID: other.ID}, public, Config))
}

func TestForTasks(t *testing.T) {
	ctx := context.Background()
	owner := db.RequireMockUser(t, db.SingleDB())
	other := db.RequireMockUser(t, db.SingleDB())

	addTask := func(jobID *model.JobID) model.TaskID {
		id := model.NewTaskID()
		require.NoError(t, db.AddTask(ctx, &model.Task{
			TaskID: id, JobID: jobID, TaskType: model.TaskTypeCommand, StartTime: time.Now(),
		}))
		return id
	}
	private := insertJob(ctx, t, &owner.ID, []string{}, []string{}, []string{})
	withJob := addTask(&private)

	// A task without a job takes its owner from its command record.
	legacy := addTask(nil)
	allocationID := model.AllocationID(string(legacy) + ".0")
	require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
		AllocationID: allocationID, TaskID: legacy, ResourcePool: "default", Ports: map[string]int{},
	}))
	_, err := db.Bun().NewRaw(`INSERT INTO command_state (task_id, allocation_id, generic_command_spec)
		VALUES (?, ?, jsonb_build_object('Base', jsonb_build_object('Owner',
			jsonb_build_object('id', ?::int))))`, legacy, allocationID, owner.ID).Exec(ctx)
	require.NoError(t, err)

	// A task without a job or a command record.
	orphan := addTask(nil)
	unknown := model.NewTaskID()

	ids := []model.TaskID{withJob, legacy, orphan, unknown}
	caps, err := ForTasks(ctx, Viewer{ID: other.ID}, ids)
	require.NoError(t, err)
	require.Equal(t, map[model.TaskID]Caps{
		withJob: {},
		legacy:  {ViewTitle: true, ViewLogs: true, ViewConfig: true},
		orphan:  {},
		unknown: {},
	}, caps)

	caps, err = ForTasks(ctx, Viewer{ID: owner.ID}, ids)
	require.NoError(t, err)
	require.Equal(t, map[model.TaskID]Caps{
		withJob: AllCaps, legacy: AllCaps, orphan: {}, unknown: {},
	}, caps)

	caps, err = ForTasks(ctx, Viewer{ID: other.ID, Admin: true}, ids)
	require.NoError(t, err)
	for _, id := range ids {
		require.Equal(t, AllCaps, caps[id])
	}

	// The legacy owner's switch applies.
	setPreferences(ctx, t, db.Bun(), owner.ID, true, false, nil, nil)
	err = RequireTask(ctx, Viewer{ID: other.ID}, legacy, Logs)
	require.Equal(t, "logs not shared", status.Convert(err).Message())
	require.NoError(t, RequireTask(ctx, Viewer{ID: other.ID}, legacy, Config))
	err = RequireTask(ctx, Viewer{ID: other.ID}, orphan, Logs)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

// jobAudiences reads a job's stored audiences.
func jobAudiences(ctx context.Context, t *testing.T, idb bun.IDB, id model.JobID) model.Job {
	t.Helper()
	var j model.Job
	require.NoError(t, idb.NewSelect().Model(&j).Where("job_id = ?", id).Scan(ctx))
	return j
}

// TestAddJobStamps is the stamping rule of db.AddJobTx. Changes to the cluster policy are made in
// transactions that are rolled back, so no other test sees them.
func TestAddJobStamps(t *testing.T) {
	ctx := context.Background()
	all := []string{AudienceAuthenticated}
	none := []string{}

	addJob := func(idb bun.IDB, ownerID *model.UserID) model.Job {
		j := &model.Job{
			JobID: model.NewJobID(), JobType: model.JobTypeCommand, OwnerID: ownerID,
			// Values set by the caller are ignored.
			TitleAudience: all, LogsAudience: all, ConfigAudience: all,
		}
		require.NoError(t, db.AddJobTx(ctx, idb, j))
		stored := jobAudiences(ctx, t, idb, j.JobID)
		// AddJobTx writes the stamp back.
		require.Equal(t, stored.TitleAudience, j.TitleAudience)
		require.Equal(t, stored.LogsAudience, j.LogsAudience)
		require.Equal(t, stored.ConfigAudience, j.ConfigAudience)
		return stored
	}
	inTx := func(f func(tx bun.Tx)) {
		require.ErrorIs(t, db.Bun().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			f(tx)
			return sql.ErrTxDone
		}), sql.ErrTxDone)
	}
	setPolicy := func(tx bun.Tx, kind string, allowed bool, audience []string) {
		_, err := tx.NewRaw(`UPDATE job_privacy_policy SET sharing_allowed = ?, new_job_audience = ?
			WHERE kind = ?`, allowed, pgdialect.Array(audience), kind).Exec(ctx)
		require.NoError(t, err)
	}

	u := db.RequireMockUser(t, db.SingleDB())

	// The defaults: every audience is the policy's.
	j := addJob(db.Bun(), &u.ID)
	require.Equal(t, all, j.TitleAudience)
	require.Equal(t, all, j.LogsAudience)
	require.Equal(t, all, j.ConfigAudience)
	require.Nil(t, j.SharingUpdatedBy)
	require.Nil(t, j.SharingUpdatedAt)

	// An ownerless job takes the policy's audiences.
	j = addJob(db.Bun(), nil)
	require.Equal(t, all, j.TitleAudience)

	// The owner's new-job audience wins over the policy's; the config takes the policy's.
	setPreferences(ctx, t, db.Bun(), u.ID, true, true, none, none)
	j = addJob(db.Bun(), &u.ID)
	require.Equal(t, none, j.TitleAudience)
	require.Equal(t, none, j.LogsAudience)
	require.Equal(t, all, j.ConfigAudience)

	// A NULL preference follows the policy, also when the policy's audience is owner only.
	setPreferences(ctx, t, db.Bun(), u.ID, true, true, nil, all)
	inTx(func(tx bun.Tx) {
		setPolicy(tx, "title", true, none)
		setPolicy(tx, "logs", true, none)
		j := addJob(tx, &u.ID)
		require.Equal(t, none, j.TitleAudience)
		require.Equal(t, all, j.LogsAudience)
	})

	// A forbidden kind is stamped {}, whatever the owner prefers, and a later allow does not
	// publish it.
	inTx(func(tx bun.Tx) {
		setPolicy(tx, "logs", false, all)
		setPolicy(tx, "config", false, all)
		j := addJob(tx, &u.ID)
		require.Equal(t, all, j.TitleAudience)
		require.Equal(t, none, j.LogsAudience)
		require.Equal(t, none, j.ConfigAudience)

		setPolicy(tx, "logs", true, all)
		setPolicy(tx, "config", true, all)
		j = jobAudiences(ctx, t, tx, j.JobID)
		require.Equal(t, none, j.LogsAudience)
		require.Equal(t, none, j.ConfigAudience)
	})

	// A missing policy row stamps {} for its kind.
	inTx(func(tx bun.Tx) {
		_, err := tx.NewDelete().Table("job_privacy_policy").Where("kind = 'title'").Exec(ctx)
		require.NoError(t, err)
		j := addJob(tx, nil)
		require.Equal(t, none, j.TitleAudience)
		require.Equal(t, all, j.LogsAudience)
	})

	// If the policy cannot be read, no job is inserted.
	inTx(func(tx bun.Tx) {
		_, err := tx.NewRaw(`ALTER TABLE job_privacy_policy RENAME TO job_privacy_policy_gone`).Exec(ctx)
		require.NoError(t, err)
		id := model.NewJobID()
		_, err = tx.NewRaw("SAVEPOINT stamp").Exec(ctx)
		require.NoError(t, err)
		require.Error(t, db.AddJobTx(ctx, tx, &model.Job{JobID: id, JobType: model.JobTypeCommand}))
		_, err = tx.NewRaw("ROLLBACK TO SAVEPOINT stamp").Exec(ctx)
		require.NoError(t, err)
		n, err := tx.NewSelect().Table("jobs").Where("job_id = ?", id).Count(ctx)
		require.NoError(t, err)
		require.Zero(t, n)
	})

	// A job inserted without the audiences, as every job before the migration was, has NULL ones:
	// the columns have no default.
	legacy := model.NewJobID()
	_, err := db.Bun().NewRaw(`INSERT INTO jobs (job_id, job_type) VALUES (?, 'COMMAND')`, legacy).
		Exec(ctx)
	require.NoError(t, err)
	j = jobAudiences(ctx, t, db.Bun(), legacy)
	require.Nil(t, j.TitleAudience)
	require.Nil(t, j.LogsAudience)
	require.Nil(t, j.ConfigAudience)
}
