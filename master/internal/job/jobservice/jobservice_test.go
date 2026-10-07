package jobservice

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
)

func TestUpdateJobQInfoPlacement(t *testing.T) {
	job := &jobv1.Job{}
	updateJobQInfo(job, &sproto.RMJobInfo{
		State:          sproto.SchedulingStateScheduled,
		RequestedSlots: 6,
		AllocatedSlots: 4,
		Placement: map[aproto.ID][]device.ID{
			"node10": {7, 3, 3},
			"node02": {6, 0, 5, 1},
			"node05": {},
		},
	})
	// Agents by ID, each agent's IDs ascending and once, an agent without one left out.
	want := []*jobv1.JobPlacement{
		{AgentId: "node02", DeviceIds: []int32{0, 1, 5, 6}},
		{AgentId: "node10", DeviceIds: []int32{3, 7}},
	}
	require.Len(t, job.Placement, len(want))
	for i := range want {
		require.True(t, proto.Equal(want[i], job.Placement[i]), "got %v", job.Placement[i])
	}
	require.EqualValues(t, 4, job.AllocatedSlots)

	// A job without a device has no placement, also after it held one.
	updateJobQInfo(job, &sproto.RMJobInfo{State: sproto.SchedulingStateQueued, RequestedSlots: 6})
	require.Nil(t, job.Placement)

	job.Placement = want
	updateJobQInfo(job, nil)
	require.Nil(t, job.Placement)
}
