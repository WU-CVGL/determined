package jobprivacy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
)

// Sharing is a job's own sharing.
type Sharing struct {
	// Title, Logs and Config are the job's audiences; nil (NULL) is a job created before job
	// privacy.
	Title, Logs, Config []string
	// UpdatedBy and UpdatedAt record the last change; nil when the sharing was never changed.
	UpdatedBy *model.UserID
	UpdatedAt *time.Time
}

// Proto is the API representation of the sharing.
func (s Sharing) Proto() *jobv1.JobSharing {
	out := &jobv1.JobSharing{
		Title:  AudienceProto(s.Title),
		Logs:   AudienceProto(s.Logs),
		Config: AudienceProto(s.Config),
	}
	if s.UpdatedBy != nil {
		by := int32(*s.UpdatedBy)
		out.UpdatedBy = &by
	}
	if s.UpdatedAt != nil {
		out.UpdatedAt = timestamppb.New(*s.UpdatedAt)
	}
	return out
}

// SharingPatch is a change of a job's sharing. A nil field is kept; true is all users and false
// is the owner and admins only.
type SharingPatch struct {
	Title, Logs, Config *bool
}

// sharingRow is the sharing columns of a jobs row.
type sharingRow struct {
	bun.BaseModel `bun:"table:jobs"`

	JobID            model.JobID   `bun:"job_id,pk"`
	OwnerID          *model.UserID `bun:"owner_id"`
	TitleAudience    []string      `bun:"title_audience,array"`
	LogsAudience     []string      `bun:"logs_audience,array"`
	ConfigAudience   []string      `bun:"config_audience,array"`
	SharingUpdatedBy *model.UserID `bun:"sharing_updated_by"`
	SharingUpdatedAt *time.Time    `bun:"sharing_updated_at"`
}

func (r sharingRow) sharing() Sharing {
	return Sharing{
		Title:     r.TitleAudience,
		Logs:      r.LogsAudience,
		Config:    r.ConfigAudience,
		UpdatedBy: r.SharingUpdatedBy,
		UpdatedAt: r.SharingUpdatedAt,
	}
}

// readSharing reads the sharing of a job the viewer may manage: NotFound for an unknown job (its
// existence is public) and PermissionDenied unless the viewer is its owner or an admin. forUpdate
// locks the row until the transaction ends.
func readSharing(
	ctx context.Context, idb bun.IDB, v Viewer, jobID model.JobID, forUpdate bool,
) (sharingRow, error) {
	var row sharingRow
	q := idb.NewSelect().Model(&row).Where("job_id = ?", jobID)
	if forUpdate {
		q = q.For("UPDATE")
	}
	switch err := q.Scan(ctx); {
	case errors.Is(err, sql.ErrNoRows):
		return sharingRow{}, api.NotFoundErrs("job", string(jobID), true)
	case err != nil:
		return sharingRow{}, readError(err)
	}
	if !v.Admin && (row.OwnerID == nil || *row.OwnerID != v.ID) {
		return sharingRow{}, status.Error(codes.PermissionDenied,
			"only the job's owner and admins manage its sharing")
	}
	return row, nil
}

// GetSharing returns the sharing of a job to its owner or an admin.
func GetSharing(ctx context.Context, v Viewer, jobID model.JobID) (Sharing, error) {
	row, err := readSharing(ctx, db.Bun(), v, jobID, false)
	if err != nil {
		return Sharing{}, err
	}
	return row.sharing(), nil
}

// PatchSharing changes the sharing of a job for its owner or an admin, records who changed it and
// returns it. A kind set to all users that the policy forbids is FailedPrecondition. The audiences
// are stored as sent: logs or config shared with an unshared title stay unreadable, because the
// rule needs the title for them. A patch without changes returns the sharing as it is.
func PatchSharing(
	ctx context.Context, v Viewer, jobID model.JobID, patch SharingPatch,
) (Sharing, error) {
	var out Sharing
	err := db.Bun().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		row, err := readSharing(ctx, tx, v, jobID, true)
		if err != nil {
			return err
		}
		if patch.Title == nil && patch.Logs == nil && patch.Config == nil {
			out = row.sharing()
			return nil
		}
		p, err := readPolicyForShare(ctx, tx)
		if err != nil {
			return err
		}
		if err := requireAllowed(p, map[Kind]*bool{
			Title: patch.Title, Logs: patch.Logs, Config: patch.Config,
		}); err != nil {
			return err
		}
		if patch.Title != nil {
			row.TitleAudience = Audience(*patch.Title)
		}
		if patch.Logs != nil {
			row.LogsAudience = Audience(*patch.Logs)
		}
		if patch.Config != nil {
			row.ConfigAudience = Audience(*patch.Config)
		}
		// The unchanged audiences are written back as they were read under the row lock.
		if err := tx.NewRaw(`UPDATE jobs SET
				title_audience = ?, logs_audience = ?, config_audience = ?,
				sharing_updated_by = ?, sharing_updated_at = NOW()
			WHERE job_id = ?
			RETURNING sharing_updated_by, sharing_updated_at`,
			pgdialect.Array(row.TitleAudience), pgdialect.Array(row.LogsAudience),
			pgdialect.Array(row.ConfigAudience), v.ID, jobID,
		).Scan(ctx, &row.SharingUpdatedBy, &row.SharingUpdatedAt); err != nil {
			return fmt.Errorf("writing the sharing of job %s: %w", jobID, err)
		}
		out = row.sharing()
		return nil
	})
	if err != nil {
		return Sharing{}, err
	}
	return out, nil
}
