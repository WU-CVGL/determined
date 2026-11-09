package jobprivacy

import (
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/schema"
)

// Exprs is the rule of Effective as SQL, for queries that filter, sort or project on it. The
// expressions read the job through the aliases pj (jobs) and pu (user_job_privacy), which Join
// adds. Each is a boolean that is never NULL.
type Exprs struct {
	jobIDCol string

	// Title, Logs and Config are whether the viewer may read the kind of the row's job.
	Title, Logs, Config schema.QueryWithArgs
	// ManageSharing is whether the viewer may change the job's sharing: the owner and admins.
	ManageSharing schema.QueryWithArgs
}

// SQL returns the rule for the viewer under the policy, for rows whose job ID is the column
// expression jobIDCol (for example "e.job_id"). jobIDCol is written into the query as it is and
// must come from code, never from a request. The viewer's ID and the policy are bound parameters.
// An admin's expressions are TRUE.
func SQL(v Viewer, p Policy, jobIDCol string) Exprs {
	if v.Admin {
		return Exprs{
			jobIDCol:      jobIDCol,
			Title:         schema.SafeQuery("TRUE", nil),
			Logs:          schema.SafeQuery("TRUE", nil),
			Config:        schema.SafeQuery("TRUE", nil),
			ManageSharing: schema.SafeQuery("TRUE", nil),
		}
	}
	// A job exists, and either the viewer owns it or the non-owner branch holds. The owner test is
	// IS NOT DISTINCT FROM so that an ownerless job gives FALSE, not NULL.
	const wrap = `(pj.job_id IS NOT NULL AND (pj.owner_id IS NOT DISTINCT FROM ? OR (%s)))`
	// The non-owner branches. Logs and config also need the title's.
	const titleBranch = `? AND COALESCE(pu.share_titles, TRUE) AND
		(pj.title_audience IS NULL OR 'authenticated' = ANY(pj.title_audience))`
	const logsBranch = `? AND COALESCE(pu.share_logs, TRUE) AND
		(pj.logs_audience IS NULL OR 'authenticated' = ANY(pj.logs_audience))`
	const configBranch = `? AND
		(pj.config_audience IS NULL OR 'authenticated' = ANY(pj.config_audience))`
	uid := int(v.ID)
	return Exprs{
		jobIDCol: jobIDCol,
		Title: schema.SafeQuery(
			fmt.Sprintf(wrap, titleBranch),
			[]any{uid, p.Title.Allowed}),
		Logs: schema.SafeQuery(
			fmt.Sprintf(wrap, titleBranch+" AND "+logsBranch),
			[]any{uid, p.Title.Allowed, p.Logs.Allowed}),
		Config: schema.SafeQuery(
			fmt.Sprintf(wrap, titleBranch+" AND "+configBranch),
			[]any{uid, p.Title.Allowed, p.Config.Allowed}),
		ManageSharing: schema.SafeQuery(
			"(pj.job_id IS NOT NULL AND pj.owner_id IS NOT DISTINCT FROM ?)", []any{uid}),
	}
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

// Columns selects the expressions as title_visible, logs_visible, config_visible and
// manage_sharing, so that the capabilities of a returned row come from the same values its filter
// and sort used.
func (e Exprs) Columns(q *bun.SelectQuery) *bun.SelectQuery {
	return q.
		ColumnExpr("? AS title_visible", e.Title).
		ColumnExpr("? AS logs_visible", e.Logs).
		ColumnExpr("? AS config_visible", e.Config).
		ColumnExpr("? AS manage_sharing", e.ManageSharing)
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
