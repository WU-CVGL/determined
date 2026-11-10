package jobprivacy

import (
	"slices"

	"github.com/determined-ai/determined/master/pkg/model"
)

// JobFacts are what the rule reads about one job.
type JobFacts struct {
	JobID   model.JobID
	OwnerID *model.UserID
	// Exists is false when the job has no jobs row.
	Exists bool
	// Title, Logs and Config are the job's audiences; nil (NULL) is a job created before job
	// privacy.
	Title, Logs, Config []string
	// ShareTitles and ShareLogs are the owner's switches, true when the owner has no row.
	ShareTitles, ShareLogs bool
}

// Effective is the rule: what the viewer may do with the job's content. Admins and the owner may
// do everything. Anyone else reads a kind when the policy allows sharing it, the owner's switch is
// on (titles and logs) and the job's audience is NULL or includes all authenticated users; logs
// and config also need the title. A job without a jobs row is hidden from non-admins.
//
// SQL is the same rule for queries; the two are kept equal by test.
func Effective(v Viewer, j JobFacts, p Policy) Caps {
	if v.Admin {
		return AllCaps
	}
	if !j.Exists {
		return Caps{}
	}
	if j.OwnerID != nil && *j.OwnerID == v.ID {
		return AllCaps
	}
	t := p.Title.Allowed && j.ShareTitles && open(j.Title)
	l := t && p.Logs.Allowed && j.ShareLogs && open(j.Logs)
	c := t && p.Config.Allowed && open(j.Config)
	return Caps{ViewTitle: t, ViewLogs: l, ViewConfig: c}
}

// open reports whether an audience includes all authenticated users. NULL is a job created before
// job privacy, public under the default.
func open(audience []string) bool {
	return audience == nil || slices.Contains(audience, AudienceAuthenticated)
}
