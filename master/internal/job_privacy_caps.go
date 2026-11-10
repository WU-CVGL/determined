package internal

import (
	"context"

	"github.com/determined-ai/determined/master/internal/jobprivacy"
	"github.com/determined-ai/determined/master/pkg/model"
)

// applyJobPrivacy computes the viewer's capabilities on each item's job with one ForJobs call and
// passes them to apply, which sets them on the item and clears what they hide. An item without a
// job gets all capabilities for an admin and none for anyone else.
func applyJobPrivacy[T any](
	ctx context.Context, v jobprivacy.Viewer, items []T,
	jobID func(T) string, apply func(T, jobprivacy.Caps),
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
		apply(item, c)
	}
	return nil
}
