// Package jobprivacy decides who may read a job's title, logs and config.
//
// A job's title (name, description, labels and notes), logs and config are readable by its owner
// and by admins. Another user reads a kind when the cluster policy allows sharing it, the owner's
// switch for it is on (titles and logs; there is no switch for configs) and the job's audience
// for it includes all authenticated users. Logs and config also need the title, because system
// log lines and the config carry the job's name. A job's existence, type, owner, workspace,
// project, pool, slots, queue position and state are never hidden.
//
// The rule is applied on top of the existing basic or RBAC check, which always runs first: it only
// narrows that check and never grants access.
//
// A job's audiences are stamped when it is created (db.AddJobTx); a NULL audience is a job created
// before job privacy and is public under the default. The policy and the owner's switches are read
// on every request; nothing is cached.
//
// Everything fails closed: a policy or preference read error is Unavailable, an admin predicate
// error is Internal, a missing policy row forbids its kind, and a job or task that cannot be found
// is hidden from non-admins.
package jobprivacy

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/cluster"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
)

// Kind is a kind of job content that job privacy controls.
type Kind int

const (
	// Title is the job's name, description, labels and notes.
	Title Kind = iota + 1
	// Logs are the job's task logs.
	Logs
	// Config is the job's config.
	Config
)

// Kinds are all kinds, in the order the API lists them.
var Kinds = []Kind{Title, Logs, Config}

// AudienceAuthenticated is the audience element for all authenticated users.
const AudienceAuthenticated = "authenticated"

// String returns the kind's name in the job_privacy_policy table.
func (k Kind) String() string {
	switch k {
	case Title:
		return "title"
	case Logs:
		return "logs"
	case Config:
		return "config"
	default:
		return fmt.Sprintf("Kind(%d)", int(k))
	}
}

// Proto returns the API representation of the kind.
func (k Kind) Proto() jobv1.JobContentKind {
	switch k {
	case Title:
		return jobv1.JobContentKind_JOB_CONTENT_KIND_TITLE
	case Logs:
		return jobv1.JobContentKind_JOB_CONTENT_KIND_LOGS
	case Config:
		return jobv1.JobContentKind_JOB_CONTENT_KIND_CONFIG
	default:
		return jobv1.JobContentKind_JOB_CONTENT_KIND_UNSPECIFIED
	}
}

// KindFromProto returns the kind of an API kind, and false for an unspecified or unknown one.
func KindFromProto(k jobv1.JobContentKind) (Kind, bool) {
	switch k {
	case jobv1.JobContentKind_JOB_CONTENT_KIND_TITLE:
		return Title, true
	case jobv1.JobContentKind_JOB_CONTENT_KIND_LOGS:
		return Logs, true
	case jobv1.JobContentKind_JOB_CONTENT_KIND_CONFIG:
		return Config, true
	default:
		return 0, false
	}
}

// Viewer is the user a request is answered for.
type Viewer struct {
	ID    model.UserID
	Admin bool
}

// NewViewer evaluates the admin predicate for the user once per request: cluster
// CanUpdateMasterConfig, the predicate of resource pool access, which is users.admin in basic
// mode. An error is Internal and never means "not an admin".
func NewViewer(ctx context.Context, u model.User) (Viewer, error) {
	permErr, err := cluster.AuthZProvider.Get().CanUpdateMasterConfig(ctx, &u)
	if err != nil {
		return Viewer{}, status.Errorf(codes.Internal,
			"checking job privacy for user %q: %s", u.Username, err)
	}
	return Viewer{ID: u.ID, Admin: permErr == nil}, nil
}

// Caps are what a viewer may do with one job's content.
type Caps struct {
	ViewTitle     bool
	ViewLogs      bool
	ViewConfig    bool
	ManageSharing bool
}

// Allows reports whether the capabilities include reading the kind.
func (c Caps) Allows(k Kind) bool {
	switch k {
	case Title:
		return c.ViewTitle
	case Logs:
		return c.ViewLogs
	case Config:
		return c.ViewConfig
	default:
		return false
	}
}

// Proto returns the API representation of the capabilities.
func (c Caps) Proto() *jobv1.JobCapabilities {
	return &jobv1.JobCapabilities{
		ViewTitle:     c.ViewTitle,
		ViewLogs:      c.ViewLogs,
		ViewConfig:    c.ViewConfig,
		ManageSharing: c.ManageSharing,
	}
}

// AllCaps are the capabilities of an admin and of a job's owner.
var AllCaps = Caps{ViewTitle: true, ViewLogs: true, ViewConfig: true, ManageSharing: true}

// deniedError is the refusal of a kind that is not shared with the viewer. It carries no name.
func deniedError(k Kind) error {
	switch k {
	case Logs:
		return status.Error(codes.PermissionDenied, "logs not shared")
	case Config:
		return status.Error(codes.PermissionDenied, "config not shared")
	default:
		return status.Errorf(codes.Internal, "job privacy: %s is redacted, not refused", k)
	}
}
