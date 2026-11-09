package jobprivacy

import (
	"context"
	"fmt"
	"slices"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
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

// Audience is the stored audience of an API choice: {authenticated} for all users, {} for the
// owner and admins only.
func Audience(allUsers bool) []string {
	if allUsers {
		return []string{AudienceAuthenticated}
	}
	return []string{}
}

// AudienceProto is the API representation of a stored audience; NULL (nil) is unset.
func AudienceProto(audience []string) *jobv1.JobAudience {
	if audience == nil {
		return nil
	}
	return &jobv1.JobAudience{AllUsers: slices.Contains(audience, AudienceAuthenticated)}
}

// Rules is the API representation of the policy, one rule per kind in the order of Kinds.
func (p Policy) Rules() []*jobv1.JobPrivacyRule {
	rules := make([]*jobv1.JobPrivacyRule, 0, len(Kinds))
	for _, k := range Kinds {
		kp := p.Kind(k)
		rules = append(rules, &jobv1.JobPrivacyRule{
			Kind:           k.Proto(),
			SharingAllowed: kp.Allowed,
			NewJobAudience: &jobv1.JobAudience{
				AllUsers: slices.Contains(kp.NewJobAudience, AudienceAuthenticated),
			},
		})
	}
	return rules
}

// forbiddenError is the refusal to share a kind that the policy forbids.
func forbiddenError(k Kind) error {
	return status.Errorf(codes.FailedPrecondition, "sharing of %s is forbidden", k)
}

// requireAllowed returns FailedPrecondition for the first kind, in the order of Kinds, that is set
// to all users although the policy forbids sharing it.
func requireAllowed(p Policy, allUsers map[Kind]*bool) error {
	for _, k := range Kinds {
		if a := allUsers[k]; a != nil && *a && !p.Kind(k).Allowed {
			return forbiddenError(k)
		}
	}
	return nil
}

// readPolicyForShare reads the policy in tx and locks its rows until tx ends, so that a policy
// write cannot forbid a kind between the check of a sharing change and its write.
func readPolicyForShare(ctx context.Context, tx bun.Tx) (Policy, error) {
	var rows []policyRow
	err := tx.NewSelect().Model(&rows).For("SHARE").Scan(ctx)
	return policyFromRows(rows, err)
}

// WritePolicy replaces the cluster policy, one rule per kind, and records who changed it. The
// caller checks the admin predicate and that rules has every kind.
func WritePolicy(
	ctx context.Context, by model.UserID, rules map[Kind]KindPolicy,
) (Policy, error) {
	var p Policy
	err := db.Bun().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, k := range Kinds {
			r := rules[k]
			audience := r.NewJobAudience
			if audience == nil {
				audience = []string{}
			}
			// An UPSERT, so that a row that went missing (and so forbade its kind) comes back.
			if _, err := tx.NewRaw(`INSERT INTO job_privacy_policy
				(kind, sharing_allowed, new_job_audience, updated_by, updated_at)
				VALUES (?, ?, ?, ?, NOW())
				ON CONFLICT (kind) DO UPDATE SET
					sharing_allowed = EXCLUDED.sharing_allowed,
					new_job_audience = EXCLUDED.new_job_audience,
					updated_by = EXCLUDED.updated_by,
					updated_at = EXCLUDED.updated_at`,
				k.String(), r.Allowed, pgdialect.Array(audience), by).Exec(ctx); err != nil {
				return fmt.Errorf("writing the job privacy policy for %s: %w", k, err)
			}
		}
		var err error
		p, err = ReadPolicy(ctx, tx)
		return err
	})
	if err != nil {
		return Policy{}, err
	}
	return p, nil
}

// UserPrivacyPatch is a change of a user's preferences. A nil field is kept.
type UserPrivacyPatch struct {
	ShareTitles, ShareLogs *bool
	// NewJobTitles and NewJobLogs set the audience of new jobs: true is all users, false is the
	// owner and admins only.
	NewJobTitles, NewJobLogs *bool
	// ResetNewJobTitles and ResetNewJobLogs return the audience of new jobs to the cluster
	// default. Each excludes the field above it.
	ResetNewJobTitles, ResetNewJobLogs bool
}

// PatchUserPrivacy changes a user's preferences and returns them. A new-job audience of all users
// for a kind that the policy forbids is FailedPrecondition; turning a switch on is not, because
// the policy still applies when a job is read.
func PatchUserPrivacy(
	ctx context.Context, userID model.UserID, patch UserPrivacyPatch,
) (UserPrivacy, error) {
	if patch.NewJobTitles != nil && patch.ResetNewJobTitles {
		return UserPrivacy{}, status.Error(codes.InvalidArgument,
			"new_job_titles and reset_new_job_titles are exclusive")
	}
	if patch.NewJobLogs != nil && patch.ResetNewJobLogs {
		return UserPrivacy{}, status.Error(codes.InvalidArgument,
			"new_job_logs and reset_new_job_logs are exclusive")
	}
	var out UserPrivacy
	err := db.Bun().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		p, err := readPolicyForShare(ctx, tx)
		if err != nil {
			return err
		}
		if err := requireAllowed(p, map[Kind]*bool{
			Title: patch.NewJobTitles, Logs: patch.NewJobLogs,
		}); err != nil {
			return err
		}
		// Create the row with the defaults and lock it, so that concurrent patches apply in turn.
		if _, err := tx.NewRaw(`INSERT INTO user_job_privacy (user_id) VALUES (?)
			ON CONFLICT (user_id) DO NOTHING`, userID).Exec(ctx); err != nil {
			return fmt.Errorf("writing job privacy preferences: %w", err)
		}
		var row userPrivacyRow
		if err := tx.NewSelect().Model(&row).Where("user_id = ?", userID).For("UPDATE").
			Scan(ctx); err != nil {
			return fmt.Errorf("reading job privacy preferences: %w", err)
		}
		if patch.ShareTitles != nil {
			row.ShareTitles = *patch.ShareTitles
		}
		if patch.ShareLogs != nil {
			row.ShareLogs = *patch.ShareLogs
		}
		switch {
		case patch.ResetNewJobTitles:
			row.NewJobTitleAudience = nil
		case patch.NewJobTitles != nil:
			row.NewJobTitleAudience = Audience(*patch.NewJobTitles)
		}
		switch {
		case patch.ResetNewJobLogs:
			row.NewJobLogsAudience = nil
		case patch.NewJobLogs != nil:
			row.NewJobLogsAudience = Audience(*patch.NewJobLogs)
		}
		if _, err := tx.NewRaw(`UPDATE user_job_privacy SET
				share_titles = ?, share_logs = ?,
				new_job_title_audience = ?, new_job_logs_audience = ?, updated_at = NOW()
			WHERE user_id = ?`,
			row.ShareTitles, row.ShareLogs,
			pgdialect.Array(row.NewJobTitleAudience), pgdialect.Array(row.NewJobLogsAudience),
			userID).Exec(ctx); err != nil {
			return fmt.Errorf("writing job privacy preferences: %w", err)
		}
		out = UserPrivacy{
			ShareTitles:  row.ShareTitles,
			ShareLogs:    row.ShareLogs,
			NewJobTitles: row.NewJobTitleAudience,
			NewJobLogs:   row.NewJobLogsAudience,
		}
		return nil
	})
	if err != nil {
		return UserPrivacy{}, err
	}
	return out, nil
}

// Proto is the API representation of the preferences.
func (u UserPrivacy) Proto() *jobv1.UserJobPrivacy {
	return &jobv1.UserJobPrivacy{
		ShareTitles:  u.ShareTitles,
		ShareLogs:    u.ShareLogs,
		NewJobTitles: AudienceProto(u.NewJobTitles),
		NewJobLogs:   AudienceProto(u.NewJobLogs),
	}
}
