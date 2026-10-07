package gpuhealth

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/proto/pkg/agentv1"
)

func TestXIDQuery(t *testing.T) {
	q := XIDQuery(`lab "a"`)
	require.Equal(t,
		`max by (gpu_uuid, xid) (max_over_time(DCGM_EXP_XID_ERRORS_COUNT{job="dcgm", `+
			`det_cluster="lab \"a\"", gpu_uuid!="", xid!="", xid!="0", xid!~"13|31|43|45"}[5m])) > 0`,
		q)
	// The gauge counts records in the exporter's window, so increase() would read each drop of the
	// window as a counter reset. DCGM_FI_DEV_XID_ERRORS holds only the last code.
	require.NotContains(t, q, "increase")
	require.NotContains(t, q, "DCGM_FI_DEV_XID_ERRORS")
	require.NotContains(t, q, "_TOTAL")

	// The regular expression of the query excludes exactly the application codes; PromQL anchors it.
	re := regexp.MustCompile("^(?:" + applicationXIDPattern + ")$")
	for code := 0; code < 200; code++ {
		require.Equal(t, applicationXIDs[code], re.MatchString(strconv.Itoa(code)), "XID %d", code)
	}
	for _, code := range []int{13, 31, 43, 45, 0, -1} {
		require.False(t, IsCriticalXID(code), "XID %d", code)
	}
	for _, code := range []int{8, 48, 61, 62, 63, 64, 74, 79, 92, 94, 95, 119, 120, 154} {
		require.True(t, IsCriticalXID(code), "XID %d", code)
	}
}

// The steps lie on a 5-minute grid, end at or after now, span 24 hours, and each step looks back
// exactly one step, so the step windows cover every sample once.
func TestXIDRangeAndStepCoverage(t *testing.T) {
	lookback, err := time.ParseDuration(xidLookback)
	require.NoError(t, err)
	require.Equal(t, XIDStep, lookback)

	now := time.Date(2026, 10, 7, 12, 27, 13, 5, time.UTC)
	start, end := XIDRange(now)
	require.Equal(t, time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC), end)
	require.Equal(t, XIDWindow, end.Sub(start))
	aligned := time.Date(2026, 10, 7, 12, 25, 0, 0, time.UTC)
	start, end = XIDRange(aligned)
	require.Equal(t, aligned, end)
	require.Equal(t, aligned.Add(-XIDWindow), start)

	// Every sample time in the range falls in the lookback (t-5m, t] of exactly one step t.
	start, end = XIDRange(now)
	for s := start.Add(-lookback + time.Second); !s.After(end); s = s.Add(17 * time.Second) {
		n := 0
		for step := start; !step.After(end); step = step.Add(XIDStep) {
			if s.After(step.Add(-lookback)) && !s.After(step) {
				n++
			}
		}
		require.Equal(t, 1, n, "sample at %s", s)
	}
}

func step(i int) time.Time {
	return time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC).Add(time.Duration(i) * XIDStep)
}

func series(uuid, xid string, steps ...int) Series {
	s := Series{Labels: map[string]string{"gpu_uuid": uuid, "xid": xid}}
	for _, i := range steps {
		s.Samples = append(s.Samples, Sample{Time: step(i), Value: 2})
	}
	return s
}

func TestRecentXIDsFirstAndLastObserved(t *testing.T) {
	odd := series("GPU-a", "79", 9, 3, 5)
	odd.Samples = append(odd.Samples,
		Sample{Time: step(1), Value: 0}, Sample{Time: step(12), Value: math.NaN()},
		Sample{Time: step(13), Value: math.Inf(1)})
	byUUID, err := recentXIDs([]Series{
		odd,
		series("GPU-a", "48", 7),
		series("GPU-b", "94", 2, 3),
		series("GPU-b", "94", 6), // merged with the series above
		series("GPU-c", "13", 1), // application codes never count
		series("GPU-c", "31", 1),
		series("GPU-c", "43", 1),
		series("GPU-c", "45", 1),
		series("GPU-c", "0", 1),
		series("GPU-d", "79"), // no sample
	})
	require.NoError(t, err)
	require.Equal(t, map[string][]XID{
		"GPU-a": {
			{Code: 48, FirstObserved: step(7), LastObserved: step(7)},
			{Code: 79, FirstObserved: step(3), LastObserved: step(9)},
		},
		"GPU-b": {{Code: 94, FirstObserved: step(2), LastObserved: step(6)}},
	}, byUUID)

	for _, labels := range []map[string]string{
		{"xid": "79"}, {"gpu_uuid": "GPU-a"}, {"gpu_uuid": "GPU-a", "xid": "x"},
	} {
		_, err := recentXIDs([]Series{{Labels: labels}})
		require.Error(t, err, "%v", labels)
	}
}

type fakeQuery struct {
	calls  atomic.Int32
	result []Series
	err    error
	block  bool
	gate   chan struct{}
	exprs  []string
	ranges [][2]time.Time
	steps  []time.Duration
	mu     sync.Mutex
}

func (f *fakeQuery) query(
	ctx context.Context, expr string, start, end time.Time, step time.Duration,
) ([]Series, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.exprs = append(f.exprs, expr)
	f.ranges = append(f.ranges, [2]time.Time{start, end})
	f.steps = append(f.steps, step)
	f.mu.Unlock()
	if f.gate != nil {
		<-f.gate
	}
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.result, f.err
}

func newTestCache(f *fakeQuery, now *time.Time) *XIDCache {
	c := NewXIDCache("lab-a", f.query)
	c.now = func() time.Time { return *now }
	return c
}

func TestXIDCacheHitMissAndFailure(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 27, 13, 0, time.UTC)
	f := &fakeQuery{result: []Series{series("GPU-a", "79", 3)}}
	c := newTestCache(f, &now)
	require.Nil(t, c.Peek())

	ctx := context.Background()
	s := c.Get(ctx)
	require.Equal(t, int32(1), f.calls.Load())
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_OK, s.Status)
	require.Equal(t, now, s.QueriedAt)
	require.Equal(t, []XID{{Code: 79, FirstObserved: step(3), LastObserved: step(3)}}, s.ByUUID["GPU-a"])
	require.Equal(t, XIDQuery("lab-a"), f.exprs[0])
	start, end := XIDRange(now)
	require.Equal(t, [2]time.Time{start, end}, f.ranges[0])
	require.Equal(t, XIDStep, f.steps[0])
	require.Same(t, s, c.Peek())

	// A hit within the TTL.
	now = now.Add(XIDCacheTTL - time.Second)
	require.Same(t, s, c.Get(ctx))
	require.Equal(t, int32(1), f.calls.Load())

	// A miss after it; a failure is cached too.
	now = now.Add(time.Second)
	f.err = errors.New(`Get "http://prometheus:9090/api/v1/query_range?query=...": connection refused`)
	failed := c.Get(ctx)
	require.Equal(t, int32(2), f.calls.Load())
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_FAILED, failed.Status)
	require.Equal(t, errRequestFailed, failed.Error, "never the URL")
	require.Equal(t, now, failed.QueriedAt)
	require.Empty(t, failed.ByUUID)
	now = now.Add(XIDCacheTTL - time.Second)
	require.Same(t, failed, c.Get(ctx))
	require.Equal(t, int32(2), f.calls.Load())

	// An invalid result fails too.
	now = now.Add(time.Second)
	f.err = nil
	f.result = []Series{{Labels: map[string]string{"xid": "79"}}}
	require.Equal(t, errInvalidResponse, c.Get(ctx).Error)
	require.Equal(t, int32(3), f.calls.Load())
}

func TestXIDCacheTimeout(t *testing.T) {
	now := time.Now()
	f := &fakeQuery{block: true}
	c := newTestCache(f, &now)
	c.timeout = 20 * time.Millisecond

	began := time.Now()
	// A canceled request does not end the shared query early, so its result is the timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := c.Get(ctx)
	require.Less(t, time.Since(began), 5*time.Second)
	require.GreaterOrEqual(t, time.Since(began), c.timeout)
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_FAILED, s.Status)
	require.Equal(t, errTimeout, s.Error)
	require.Same(t, s, c.Get(context.Background()))
	require.Equal(t, int32(1), f.calls.Load())
}

// Callers that arrive during a query wait for it: one query for all of them.
func TestXIDCacheOneQueryAtATime(t *testing.T) {
	now := time.Now()
	f := &fakeQuery{gate: make(chan struct{})}
	c := newTestCache(f, &now)
	var wg sync.WaitGroup
	results := make([]*XIDSnapshot, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.Get(context.Background())
		}(i)
	}
	require.Eventually(t, func() bool { return f.calls.Load() == 1 }, 5*time.Second, time.Millisecond)
	close(f.gate)
	wg.Wait()
	require.Equal(t, int32(1), f.calls.Load())
	for _, r := range results {
		require.Same(t, results[0], r)
	}
}

func TestXIDCacheNotConfigured(t *testing.T) {
	c := NewXIDCache("lab-a", nil)
	s := c.Get(context.Background())
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_NOT_CONFIGURED, s.Status)
	require.True(t, s.QueriedAt.IsZero())
	require.Same(t, s, c.Peek())

	var none *XIDCache
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_NOT_CONFIGURED,
		none.Get(context.Background()).Status)
}
