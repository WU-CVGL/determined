package jobprivacy

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
)

// KindPolicy is the cluster policy for one kind.
type KindPolicy struct {
	// Allowed is whether jobs may share the kind with users other than the owner and admins.
	Allowed bool
	// NewJobAudience is the audience new jobs get unless the owner chose one.
	NewJobAudience []string
}

// Policy is the cluster policy, read once per request.
type Policy struct {
	Title, Logs, Config KindPolicy
}

// Kind returns the policy for one kind.
func (p Policy) Kind(k Kind) KindPolicy {
	switch k {
	case Title:
		return p.Title
	case Logs:
		return p.Logs
	case Config:
		return p.Config
	default:
		return KindPolicy{NewJobAudience: []string{}}
	}
}

// policyRow is a row of job_privacy_policy.
type policyRow struct {
	bun.BaseModel `bun:"table:job_privacy_policy"`

	Kind           string        `bun:"kind,pk"`
	SharingAllowed bool          `bun:"sharing_allowed"`
	NewJobAudience []string      `bun:"new_job_audience,array"`
	UpdatedBy      *model.UserID `bun:"updated_by"`
	UpdatedAt      time.Time     `bun:"updated_at"`
}

// ReadPolicy reads the cluster policy. A query error is Unavailable and is never taken as the
// default. A kind without a row is forbidden and its new-job audience is {}; the seeded rows are
// never deleted, so that is logged as an error.
func ReadPolicy(ctx context.Context, idb bun.IDB) (Policy, error) {
	if idb == nil {
		idb = db.Bun()
	}
	var rows []policyRow
	err := idb.NewSelect().Model(&rows).Scan(ctx)
	return policyFromRows(rows, err)
}

// policyFromRows builds the policy from the rows of job_privacy_policy and the error of reading
// them.
func policyFromRows(rows []policyRow, err error) (Policy, error) {
	if err != nil {
		return Policy{}, status.Errorf(codes.Unavailable,
			"could not read the job privacy policy: %s; try again", err)
	}
	byKind := make(map[string]policyRow, len(rows))
	for _, r := range rows {
		byKind[r.Kind] = r
	}
	kindPolicy := func(k Kind) KindPolicy {
		r, ok := byKind[k.String()]
		if !ok {
			log.Errorf("job privacy policy has no row for %s: sharing of %s is forbidden", k, k)
			return KindPolicy{NewJobAudience: []string{}}
		}
		audience := r.NewJobAudience
		if audience == nil {
			audience = []string{}
		}
		return KindPolicy{Allowed: r.SharingAllowed, NewJobAudience: audience}
	}
	return Policy{Title: kindPolicy(Title), Logs: kindPolicy(Logs), Config: kindPolicy(Config)}, nil
}

// UserPrivacy is a user's job privacy preferences.
type UserPrivacy struct {
	// ShareTitles and ShareLogs are live switches over all of the user's jobs.
	ShareTitles, ShareLogs bool
	// NewJobTitles and NewJobLogs are copied into the user's new jobs; nil is the cluster default.
	NewJobTitles, NewJobLogs []string
}

// userPrivacyRow is a row of user_job_privacy.
type userPrivacyRow struct {
	bun.BaseModel `bun:"table:user_job_privacy"`

	UserID              model.UserID `bun:"user_id,pk"`
	ShareTitles         bool         `bun:"share_titles"`
	ShareLogs           bool         `bun:"share_logs"`
	NewJobTitleAudience []string     `bun:"new_job_title_audience,array"`
	NewJobLogsAudience  []string     `bun:"new_job_logs_audience,array"`
	UpdatedAt           time.Time    `bun:"updated_at"`
}

// DefaultUserPrivacy are the preferences of a user without a row: both switches on and the
// cluster default for new jobs.
var DefaultUserPrivacy = UserPrivacy{ShareTitles: true, ShareLogs: true}

// ReadUserPrivacy reads a user's preferences. A user without a row has DefaultUserPrivacy. A
// query error is Unavailable.
func ReadUserPrivacy(ctx context.Context, idb bun.IDB, userID model.UserID) (UserPrivacy, error) {
	if idb == nil {
		idb = db.Bun()
	}
	var rows []userPrivacyRow
	if err := idb.NewSelect().Model(&rows).Where("user_id = ?", userID).Scan(ctx); err != nil {
		return UserPrivacy{}, status.Errorf(codes.Unavailable,
			"could not read job privacy preferences: %s; try again", err)
	}
	if len(rows) == 0 {
		return DefaultUserPrivacy, nil
	}
	return UserPrivacy{
		ShareTitles:  rows[0].ShareTitles,
		ShareLogs:    rows[0].ShareLogs,
		NewJobTitles: rows[0].NewJobTitleAudience,
		NewJobLogs:   rows[0].NewJobLogsAudience,
	}, nil
}
