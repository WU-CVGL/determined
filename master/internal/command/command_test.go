package command

import (
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/protoutils"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

func TestV1EndTime(t *testing.T) {
	key := "key"
	spec := tasks.GenericCommandSpec{}
	spec.Base.Owner = &model.User{Username: "alice"}
	spec.Metadata.PublicKey = &key
	c := &Command{
		GenericCommandSpec: spec,
		taskID:             model.NewTaskID(),
		allocationID:       model.AllocationID("unknown.1"),
		syslog:             logrus.WithField("component", "command"),
	}

	endTimes := func() []*timestamppb.Timestamp {
		return []*timestamppb.Timestamp{
			c.ToV1Command().EndTime,
			c.ToV1Notebook().EndTime,
			c.ToV1Shell().EndTime,
			c.ToV1Tensorboard().EndTime,
		}
	}

	for _, endTime := range endTimes() {
		require.Nil(t, endTime, "a running task has no end time")
	}

	ended := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)
	c.exitStatus = &task.AllocationExited{FinalState: task.AllocationState{
		State: model.AllocationStateTerminated,
	}}
	c.endTime = &ended
	for _, endTime := range endTimes() {
		require.Equal(t, protoutils.ToTimestamp(ended), endTime)
	}
}
