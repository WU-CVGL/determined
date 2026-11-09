package internal

import (
	"context"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/jobprivacy"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
)

// jobPrivacyExprs returns the job privacy rule for the viewer as SQL over the job ID column
// jobIDCol. The policy is read only for non-admins, whose expressions depend on it.
func jobPrivacyExprs(
	ctx context.Context, v jobprivacy.Viewer, jobIDCol string,
) (jobprivacy.Exprs, error) {
	var p jobprivacy.Policy
	if !v.Admin {
		var err error
		if p, err = jobprivacy.ReadPolicy(ctx, db.Bun()); err != nil {
			return jobprivacy.Exprs{}, err
		}
	}
	return jobprivacy.SQL(v, p, jobIDCol), nil
}

// setJobCapabilities sets the viewer's capabilities on each item, from its job ID, with one
// ForJobs call. An item without a job gets all of them for an admin and none for anyone else.
func setJobCapabilities[T any](
	ctx context.Context, v jobprivacy.Viewer, items []T,
	jobID func(T) string, set func(T, *jobv1.JobCapabilities),
) error {
	ids := make([]model.JobID, 0, len(items))
	for _, item := range items {
		if id := jobID(item); id != "" {
			ids = append(ids, model.JobID(id))
		}
	}
	caps, err := jobprivacy.ForJobs(ctx, v, ids)
	if err != nil {
		return err
	}
	for _, item := range items {
		c, ok := caps[model.JobID(jobID(item))]
		if !ok && v.Admin {
			c = jobprivacy.AllCaps
		}
		set(item, c.Proto())
	}
	return nil
}
