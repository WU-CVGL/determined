package idle

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/model"
)

func TestIdleTimeoutWatcherUseRunnerState(t *testing.T) {
	TickInterval = 10 * time.Millisecond
	var actionDone atomic.Bool
	timeout := time.Second
	cfg := sproto.IdleTimeoutConfig{
		ServiceID:       "test",
		TimeoutDuration: timeout,
		UseRunnerState:  true,
	}

	Register(cfg, func(context.Context, error) {
		actionDone.Store(true)
	})
	defer Unregister(cfg.ServiceID)

	RecordActivity(cfg.ServiceID)

	require.True(t, waitForCondition(10*timeout, actionDone.Load))
}

func waitForCondition(timeout time.Duration, condition func() bool) bool {
	for i := 0; i < int(timeout/TickInterval); i++ {
		if condition() {
			return true
		}
		time.Sleep(TickInterval)
	}
	return false
}

func TestActivityRecord(t *testing.T) {
	const taskID = model.TaskID("record")
	// Without a watcher's record, activity is not recorded.
	Touch(taskID)
	Hold(taskID)()
	_, ok := ActivityOf(taskID)
	require.False(t, ok)

	startActivity(taskID)
	defer stopActivity(taskID)
	a, ok := ActivityOf(taskID)
	require.True(t, ok)
	require.Equal(t, Activity{}, a, "no activity yet")

	before := time.Now()
	Touch(taskID)
	a, _ = ActivityOf(taskID)
	require.False(t, a.LastActivity.Before(before))
	require.Zero(t, a.Holds)

	first, second := Hold(taskID), Hold(taskID)
	a, _ = ActivityOf(taskID)
	require.Equal(t, 2, a.Holds)
	first()
	first()
	a, _ = ActivityOf(taskID)
	require.Equal(t, 1, a.Holds, "a release counts once")
	beforeRelease := time.Now()
	second()
	a, _ = ActivityOf(taskID)
	require.Zero(t, a.Holds)
	require.False(t, a.LastActivity.Before(beforeRelease), "the last release is the last activity")

	// A release from before the record was replaced does not reach the new one.
	stale := Hold(taskID)
	startActivity(taskID)
	stale()
	a, _ = ActivityOf(taskID)
	require.Equal(t, Activity{}, a)
}

// registerTaskWatcher registers a watcher of the task's activity, which records when it fires.
func registerTaskWatcher(t *testing.T, taskID model.TaskID, timeout time.Duration) *atomic.Pointer[time.Time] {
	TickInterval = 10 * time.Millisecond
	var fired atomic.Pointer[time.Time]
	Register(sproto.IdleTimeoutConfig{
		ServiceID:       string(taskID),
		TaskID:          taskID,
		UseTaskActivity: true,
		TimeoutDuration: timeout,
	}, func(context.Context, error) {
		now := time.Now()
		fired.Store(&now)
	})
	t.Cleanup(func() { Unregister(string(taskID)) })
	return &fired
}

func TestIdleTimeoutWatcherUseTaskActivityHeld(t *testing.T) {
	const taskID = model.TaskID("held")
	timeout := 200 * time.Millisecond
	fired := registerTaskWatcher(t, taskID, timeout)

	// The idle time does not count before the first activity, nor while a session is open.
	time.Sleep(3 * timeout)
	require.Nil(t, fired.Load())
	release := Hold(taskID)
	time.Sleep(3 * timeout)
	require.Nil(t, fired.Load())

	// It counts from the release.
	released := time.Now()
	release()
	require.True(t, waitForCondition(10*timeout, func() bool { return fired.Load() != nil }))
	require.GreaterOrEqual(t, fired.Load().Sub(released), timeout)
}

// A held task is never timed out, however short its timeout.
func TestIdleTimeoutWatcherUseTaskActivityHeldTinyTimeout(t *testing.T) {
	const taskID = model.TaskID("held-tiny")
	fired := registerTaskWatcher(t, taskID, time.Nanosecond)

	release := Hold(taskID)
	time.Sleep(20 * TickInterval)
	require.Nil(t, fired.Load())

	release()
	require.True(t, waitForCondition(time.Second, func() bool { return fired.Load() != nil }))
}

func TestIdleTimeoutWatcherUseTaskActivityTouched(t *testing.T) {
	const taskID = model.TaskID("touched")
	timeout := 200 * time.Millisecond
	fired := registerTaskWatcher(t, taskID, timeout)

	var last time.Time
	for end := time.Now().Add(3 * timeout); time.Now().Before(end); {
		last = time.Now()
		Touch(taskID)
		time.Sleep(timeout / 4)
	}
	require.Nil(t, fired.Load())
	require.True(t, waitForCondition(10*timeout, func() bool { return fired.Load() != nil }))
	require.GreaterOrEqual(t, fired.Load().Sub(last), timeout)
}

func TestIdleTimeoutWatcherUnregisterRemovesActivity(t *testing.T) {
	const taskID = model.TaskID("unregistered")
	registerTaskWatcher(t, taskID, time.Hour)
	_, ok := ActivityOf(taskID)
	require.True(t, ok)

	Unregister(string(taskID))
	_, ok = ActivityOf(taskID)
	require.False(t, ok)
	Touch(taskID)
	_, ok = ActivityOf(taskID)
	require.False(t, ok)
}
