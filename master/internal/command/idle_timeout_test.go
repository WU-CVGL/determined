package command

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

// A task's idle watcher follows its persisted spec: a shell is watched through its activity record
// and only with an idle timeout; notebooks and TensorBoards keep their watchers; other tasks have
// none.
func TestIdleTimeoutConfig(t *testing.T) {
	const taskID = model.TaskID("task")
	timeout := ptrs.Ptr(model.Duration(30 * time.Minute))
	spec := func(idleTimeout *model.Duration, set func(*tasks.GenericCommandSpec)) tasks.GenericCommandSpec {
		s := tasks.GenericCommandSpec{Config: model.CommandConfig{IdleTimeout: idleTimeout}}
		set(&s)
		return s
	}
	shell := func(s *tasks.GenericCommandSpec) { s.WatchTaskActivity = true }
	notebook := func(s *tasks.GenericCommandSpec) {
		s.WatchProxyIdleTimeout, s.WatchRunnerIdleTimeout = true, true
	}
	tensorBoard := func(s *tasks.GenericCommandSpec) { s.WatchProxyIdleTimeout = true }
	command := func(*tasks.GenericCommandSpec) {}

	require.Nil(t, idleTimeoutConfig(spec(nil, shell), taskID))
	require.Equal(t, &sproto.IdleTimeoutConfig{
		ServiceID: "task", TaskID: taskID, UseTaskActivity: true, TimeoutDuration: 30 * time.Minute,
	}, idleTimeoutConfig(spec(timeout, shell), taskID))

	require.Nil(t, idleTimeoutConfig(spec(nil, notebook), taskID))
	require.Equal(t, &sproto.IdleTimeoutConfig{
		ServiceID: "task", UseProxyState: true, UseRunnerState: true, TimeoutDuration: 30 * time.Minute,
	}, idleTimeoutConfig(spec(timeout, notebook), taskID))
	require.Equal(t, &sproto.IdleTimeoutConfig{
		ServiceID: "task", UseProxyState: true, TimeoutDuration: 30 * time.Minute,
	}, idleTimeoutConfig(spec(timeout, tensorBoard), taskID))

	// A shell saved before the master watched shells, and a command, are not watched.
	require.Nil(t, idleTimeoutConfig(spec(timeout, command), taskID))
}
