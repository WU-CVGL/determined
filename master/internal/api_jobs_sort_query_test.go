package internal

import (
	"net/url"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/grpc-ecosystem/grpc-gateway/utilities"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// The REST query parameters of the Jobs page's filters and sorts reach the requests, with
// slots_above 0 set rather than left out.
func TestJobsSortQueryParameters(t *testing.T) {
	for _, names := range [][4]string{
		{"slots", "slotsAbove", "workspaceIds", "sortBy"},
		{"slots", "slots_above", "workspace_ids", "sort_by"},
	} {
		query := url.Values{
			names[0]: {"0", "2"}, names[1]: {"0"}, names[2]: {"3", "4"},
			names[3]: {"SORT_BY_SLOTS"}, "orderBy": {"ORDER_BY_DESC"},
		}

		var experiments apiv1.GetExperimentsRequest
		require.NoError(t, runtime.PopulateQueryParameters(
			&experiments, query, utilities.NewDoubleArray(nil)))
		require.Equal(t, []int32{0, 2}, experiments.Slots, names)
		require.NotNil(t, experiments.SlotsAbove, names)
		require.Equal(t, int32(0), *experiments.SlotsAbove)
		require.Equal(t, []int32{3, 4}, experiments.WorkspaceIds)
		require.Equal(t, apiv1.GetExperimentsRequest_SORT_BY_SLOTS, experiments.SortBy)
		require.Equal(t, apiv1.OrderBy_ORDER_BY_DESC, experiments.OrderBy)

		var tasks apiv1.GetGenericTasksRequest
		require.NoError(t, runtime.PopulateQueryParameters(
			&tasks, query, utilities.NewDoubleArray(nil)))
		require.Equal(t, []int32{0, 2}, tasks.Slots, names)
		require.NotNil(t, tasks.SlotsAbove, names)
		require.Equal(t, int32(0), *tasks.SlotsAbove)
		require.Equal(t, []int32{3, 4}, tasks.WorkspaceIds)
		require.Equal(t, apiv1.GetGenericTasksRequest_SORT_BY_SLOTS, tasks.SortBy)
		require.Equal(t, apiv1.OrderBy_ORDER_BY_DESC, tasks.OrderBy)
	}

	// Without slots_above, no lower bound.
	var tasks apiv1.GetGenericTasksRequest
	require.NoError(t, runtime.PopulateQueryParameters(
		&tasks, url.Values{"slots": {"0"}}, utilities.NewDoubleArray(nil)))
	require.Nil(t, tasks.SlotsAbove)

	// A stale page's slotsFilter is ignored.
	require.NoError(t, runtime.PopulateQueryParameters(
		&tasks, url.Values{"slotsFilter": {"SLOTS_FILTER_HAS_SLOTS"}}, utilities.NewDoubleArray(nil)))
}
