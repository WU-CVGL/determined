package jobprivacy

import (
	"context"
	"fmt"
	"strconv"

	"github.com/uptrace/bun"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
)

// Exprs is the rule of Effective as SQL, for queries that filter, sort or project on it. The
// expressions read the job through the aliases pj (jobs) and pu (user_job_privacy), which Join
// adds. Each is a boolean that is never NULL.
//
// The expressions are complete SQL text with no placeholders, so that they can be written into
// ORDER BY and WHERE strings that carry their own arguments. The only values in them are the
// viewer's ID and the policy's booleans, which come from the session and the database, never from
// a request.
//
// The experiment column expressions (ExpName and the others) read the experiment through the alias
// e. The raw title and config literals of experiments appear only here and in the write paths.
type Exprs struct {
	jobIDCol string

	// Title, Logs and Config are whether the viewer may read the kind of the row's job.
	Title, Logs, Config string
	// ManageSharing is whether the viewer may change the job's sharing: the owner and admins.
	ManageSharing string
}

// sqlTrue is the SQL of an expression that always holds, an admin's.
const sqlTrue = "TRUE"

// sqlBool is a Go boolean as SQL.
func sqlBool(b bool) string {
	if b {
		return sqlTrue
	}
	return "FALSE"
}

// SQL returns the rule for the viewer under the policy, for rows whose job ID is the column
// expression jobIDCol (for example "e.job_id"). jobIDCol is written into the query as it is and
// must come from code, never from a request. An admin's expressions are TRUE.
func SQL(v Viewer, p Policy, jobIDCol string) Exprs {
	if v.Admin {
		return Unrestricted(jobIDCol)
	}
	// A job exists, and either the viewer owns it or the non-owner branch holds. The owner test is
	// IS NOT DISTINCT FROM so that an ownerless job gives FALSE, not NULL.
	const wrap = `(pj.job_id IS NOT NULL AND (pj.owner_id IS NOT DISTINCT FROM %s OR (%s)))`
	// The non-owner branches. Logs and config also need the title's.
	titleBranch := sqlBool(p.Title.Allowed) + ` AND COALESCE(pu.share_titles, TRUE) AND
		(pj.title_audience IS NULL OR 'authenticated' = ANY(pj.title_audience))`
	logsBranch := sqlBool(p.Logs.Allowed) + ` AND COALESCE(pu.share_logs, TRUE) AND
		(pj.logs_audience IS NULL OR 'authenticated' = ANY(pj.logs_audience))`
	configBranch := sqlBool(p.Config.Allowed) + ` AND
		(pj.config_audience IS NULL OR 'authenticated' = ANY(pj.config_audience))`
	uid := strconv.Itoa(int(v.ID))
	return Exprs{
		jobIDCol:      jobIDCol,
		Title:         fmt.Sprintf(wrap, uid, titleBranch),
		Logs:          fmt.Sprintf(wrap, uid, titleBranch+" AND "+logsBranch),
		Config:        fmt.Sprintf(wrap, uid, titleBranch+" AND "+configBranch),
		ManageSharing: "(pj.job_id IS NOT NULL AND pj.owner_id IS NOT DISTINCT FROM " + uid + ")",
	}
}

// Unrestricted is the rule of an admin: every expression is TRUE. It is also for internal reads
// whose rows are written back, such as the labels an editor changes, which must read the stored
// values whoever asks.
func Unrestricted(jobIDCol string) Exprs {
	return Exprs{
		jobIDCol: jobIDCol, Title: sqlTrue, Logs: sqlTrue, Config: sqlTrue, ManageSharing: sqlTrue,
	}
}

// ForViewer reads the policy (for non-admins, whose expressions depend on it) and returns the rule
// for the viewer over jobIDCol. A policy read error is Unavailable.
func ForViewer(ctx context.Context, v Viewer, jobIDCol string) (Exprs, error) {
	var p Policy
	if !v.Admin {
		var err error
		if p, err = ReadPolicy(ctx, db.Bun()); err != nil {
			return Exprs{}, err
		}
	}
	return SQL(v, p, jobIDCol), nil
}

// ForUser is ForViewer for a user: the admin predicate, then the policy.
func ForUser(ctx context.Context, u model.User, jobIDCol string) (Exprs, error) {
	v, err := NewViewer(ctx, u)
	if err != nil {
		return Exprs{}, err
	}
	return ForViewer(ctx, v, jobIDCol)
}

// JoinSQL is the joins the expressions read, for raw queries.
func (e Exprs) JoinSQL() string {
	return "LEFT JOIN jobs AS pj ON pj.job_id = " + e.jobIDCol +
		" LEFT JOIN user_job_privacy AS pu ON pu.user_id = pj.owner_id"
}

// Join adds the joins the expressions read to the query.
func (e Exprs) Join(q *bun.SelectQuery) *bun.SelectQuery {
	return q.
		Join("LEFT JOIN jobs AS pj ON pj.job_id = " + e.jobIDCol).
		Join("LEFT JOIN user_job_privacy AS pu ON pu.user_id = pj.owner_id")
}

// NoJoin returns the expressions as subqueries that need no joins, for queries that do not join
// the job, such as filters of bulk actions.
func (e Exprs) NoJoin() Exprs {
	exists := func(cond string) string {
		if cond == sqlTrue {
			return cond
		}
		return "EXISTS (SELECT 1 FROM jobs AS pj " +
			"LEFT JOIN user_job_privacy AS pu ON pu.user_id = pj.owner_id " +
			"WHERE pj.job_id = " + e.jobIDCol + " AND " + cond + ")"
	}
	return Exprs{
		jobIDCol:      e.jobIDCol,
		Title:         exists(e.Title),
		Logs:          exists(e.Logs),
		Config:        exists(e.Config),
		ManageSharing: exists(e.ManageSharing),
	}
}

// Columns selects the expressions as title_visible, logs_visible, config_visible and
// manage_sharing, so that the capabilities of a returned row come from the same values its filter
// and sort used.
func (e Exprs) Columns(q *bun.SelectQuery) *bun.SelectQuery {
	return q.
		ColumnExpr(e.Title + " AS title_visible").
		ColumnExpr(e.Logs + " AS logs_visible").
		ColumnExpr(e.Config + " AS config_visible").
		ColumnExpr(e.ManageSharing + " AS manage_sharing")
}

// Visible are the selected expressions of one row.
type Visible struct {
	TitleVisible  bool `bun:"title_visible"`
	LogsVisible   bool `bun:"logs_visible"`
	ConfigVisible bool `bun:"config_visible"`
	ManageSharing bool `bun:"manage_sharing"`
}

// Caps returns the capabilities of the row.
func (r Visible) Caps() Caps {
	return Caps{
		ViewTitle:     r.TitleVisible,
		ViewLogs:      r.LogsVisible,
		ViewConfig:    r.ConfigVisible,
		ManageSharing: r.ManageSharing,
	}
}

// CapabilitiesJSON is the expressions as a JSON object with the fields of jobv1.JobCapabilities,
// for queries that scan a column into that message.
func (e Exprs) CapabilitiesJSON() string {
	return "jsonb_build_object('view_title', " + e.Title + ", 'view_logs', " + e.Logs +
		", 'view_config', " + e.Config + ", 'manage_sharing', " + e.ManageSharing + ")"
}

// when is expr when the condition holds, else NULL. A condition that is always TRUE (an admin's)
// gives expr itself, so an admin's queries are the ones from before job privacy.
func when(cond, expr string) string {
	if cond == sqlTrue {
		return expr
	}
	return "CASE WHEN " + cond + " THEN " + expr + " END"
}

// whenTitle is expr when the title is visible, else NULL.
func (e Exprs) whenTitle(expr string) string { return when(e.Title, expr) }

// ExpName is the experiment's name, NULL when its title is hidden.
func (e Exprs) ExpName() string { return e.whenTitle("e.config->>'name'") }

// ExpNameSortKey is the experiment's name as a sort key: empty for a config without a name, NULL
// when the title is hidden, so that hidden rows sort with the missing values.
func (e Exprs) ExpNameSortKey() string { return e.whenTitle("COALESCE(e.config->>'name', '')") }

// ExpDescription is the experiment's description, NULL when its title is hidden.
func (e Exprs) ExpDescription() string { return e.whenTitle("e.config->>'description'") }

// ExpLabelsText is the experiment's labels as JSON text, NULL when its title is hidden.
func (e Exprs) ExpLabelsText() string { return e.whenTitle("e.config->>'labels'") }

// ExpLabelsJSON is the experiment's labels as jsonb, NULL when its title is hidden or the labels
// are a JSON null.
func (e Exprs) ExpLabelsJSON() string {
	// NULLIF reads the config once; a CASE that tests and then returns the labels detoasts the
	// config twice per row.
	return e.whenTitle("NULLIF(e.config->'labels', 'null'::jsonb)")
}

// ExpNotesMarker is 'omitted' when the experiment has notes and its title is visible, else NULL.
func (e Exprs) ExpNotesMarker() string {
	return "CASE WHEN " + e.Title + " AND NULLIF(e.notes, '') IS NOT NULL THEN 'omitted' END"
}

// ExpConfig is the experiment's config when it is visible, else only its slots per trial and
// resource pool, which are never hidden.
func (e Exprs) ExpConfig() string {
	if e.Config == sqlTrue {
		return "e.config"
	}
	return "CASE WHEN " + e.Config + " THEN e.config ELSE jsonb_build_object('resources', " +
		"jsonb_strip_nulls(jsonb_build_object(" +
		"'slots_per_trial', e.config->'resources'->'slots_per_trial', " +
		"'resource_pool', e.config->'resources'->'resource_pool'))) END"
}

// ExpPachyderm is the experiment's Pachyderm integration from its config, NULL when the config is
// hidden.
func (e Exprs) ExpPachyderm() string {
	return when(e.Config, "NULLIF(e.config#>'{integrations, pachyderm}', 'null')")
}

// LogPolicyMatched is the column col, a log policy match, NULL when the logs are hidden.
func (e Exprs) LogPolicyMatched(col string) string { return when(e.Logs, col) }
