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

// fakeQuery is a RangeQuery that records its calls. A query with a gate waits for the gate to
// close; a blocking one waits for its context to end.
type fakeQuery struct {
	calls  atomic.Int32
	mu     sync.Mutex
	result []Series
	err    error
	block  bool
	gate   chan struct{}
	exprs  []string
	ranges [][2]time.Time
	steps  []time.Duration
}

// set changes the answer of the next queries.
func (f *fakeQuery) set(result []Series, err error, block bool, gate chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.result, f.err, f.block, f.gate = result, err, block, gate
}

func (f *fakeQuery) query(
	ctx context.Context, expr string, start, end time.Time, step time.Duration,
) ([]Series, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.exprs = append(f.exprs, expr)
	f.ranges = append(f.ranges, [2]time.Time{start, end})
	f.steps = append(f.steps, step)
	result, err, block, gate := f.result, f.err, f.block, f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return result, err
}

// testClock is the cache's clock in tests; background queries read it too.
type testClock struct{ ns atomic.Int64 }

func newTestClock(t time.Time) *testClock {
	c := &testClock{}
	c.ns.Store(t.UnixNano())
	return c
}

func (c *testClock) now() time.Time      { return time.Unix(0, c.ns.Load()).UTC() }
func (c *testClock) add(d time.Duration) { c.ns.Add(int64(d)) }
func (c *testClock) set(t time.Time)     { c.ns.Store(t.UnixNano()) }

func newTestCache(f *fakeQuery, clock *testClock) *XIDCache {
	c := NewXIDCache("lab-a", f.query)
	c.now = clock.now
	return c
}

// waitForQuery waits until no query runs, in the background either.
func waitForQuery(c *XIDCache) {
	c.mu.Lock()
	defer c.mu.Unlock()
}

func TestXIDCacheHitMissAndFailure(t *testing.T) {
	clock := newTestClock(time.Date(2026, 10, 7, 12, 27, 13, 0, time.UTC))
	f := &fakeQuery{result: []Series{series("GPU-a", "79", 3)}}
	c := newTestCache(f, clock)
	require.Nil(t, c.Peek())
	require.Nil(t, c.LastOK())

	// The first query: the caller waits for it.
	ctx := context.Background()
	s := c.Get(ctx)
	require.Equal(t, int32(1), f.calls.Load())
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_OK, s.Status)
	require.Equal(t, clock.now(), s.QueriedAt)
	require.Equal(t, []XID{{Code: 79, FirstObserved: step(3), LastObserved: step(3)}}, s.ByUUID["GPU-a"])
	require.Equal(t, XIDQuery("lab-a"), f.exprs[0])
	start, end := XIDRange(clock.now())
	require.Equal(t, [2]time.Time{start, end}, f.ranges[0])
	require.Equal(t, XIDStep, f.steps[0])
	require.Same(t, s, c.Peek())
	require.Same(t, s, c.LastOK())

	// A hit within the TTL.
	clock.add(XIDCacheTTL - time.Second)
	require.Same(t, s, c.Get(ctx))
	require.Equal(t, int32(1), f.calls.Load())

	// After it, the last result at once and a refresh in the background; a failure is cached too.
	clock.add(time.Second)
	f.set(nil, errors.New(`Get "http://prometheus:9090/api/v1/query_range?query=...": connection refused`),
		false, nil)
	require.Same(t, s, c.Get(ctx))
	waitForQuery(c)
	require.Equal(t, int32(2), f.calls.Load())
	failed := c.Peek()
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_FAILED, failed.Status)
	require.Equal(t, errRequestFailed, failed.Error, "never the URL")
	require.Equal(t, clock.now(), failed.QueriedAt)
	require.Empty(t, failed.ByUUID)
	require.Same(t, s, c.LastOK(), "a failure keeps the last successful result")
	clock.add(XIDCacheTTL - time.Second)
	require.Same(t, failed, c.Get(ctx))
	require.Equal(t, int32(2), f.calls.Load())

	// An invalid result fails too.
	clock.add(time.Second)
	f.set([]Series{{Labels: map[string]string{"xid": "79"}}}, nil, false, nil)
	require.Same(t, failed, c.Get(ctx))
	waitForQuery(c)
	require.Equal(t, int32(3), f.calls.Load())
	require.Equal(t, errInvalidResponse, c.Peek().Error)

	// A result XIDMaxStale old: the caller waits for the new one.
	clock.add(XIDMaxStale)
	f.set([]Series{series("GPU-b", "94", 4)}, nil, false, nil)
	fresh := c.Get(ctx)
	require.Equal(t, int32(4), f.calls.Load())
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_OK, fresh.Status)
	require.Equal(t, clock.now(), fresh.QueriedAt)
	require.Same(t, fresh, c.Peek())
	require.Same(t, fresh, c.LastOK())
}

// A slow Prometheus never delays a request that has a result to reuse.
func TestXIDCacheStaleWhileQuerying(t *testing.T) {
	t0 := time.Date(2026, 10, 7, 12, 27, 13, 0, time.UTC)
	clock := newTestClock(t0)
	f := &fakeQuery{result: []Series{series("GPU-a", "79", 3)}}
	c := newTestCache(f, clock)
	ctx := context.Background()
	old := c.Get(ctx)

	// Past the TTL, with a query that hangs until the gate closes: the last result at once, also
	// for a second caller, and one query.
	gate := make(chan struct{})
	f.set([]Series{series("GPU-a", "79", 3, 4)}, nil, false, gate)
	clock.set(t0.Add(XIDMaxStale - 2*time.Second))
	require.Same(t, old, c.Get(ctx))
	require.Same(t, old, c.Get(ctx))
	require.Eventually(t, func() bool { return f.calls.Load() == 2 }, 5*time.Second, time.Millisecond)

	// A caller whose result is XIDMaxStale old waits for the running query and gets its result,
	// without a query of its own.
	clock.set(t0.Add(XIDMaxStale))
	got := make(chan *XIDSnapshot, 1)
	go func() { got <- c.Get(ctx) }()
	select {
	case <-got:
		t.Fatal("a caller with a result XIDMaxStale old did not wait")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	refreshed := <-got
	require.Equal(t, int32(2), f.calls.Load())
	require.Same(t, refreshed, c.Peek())
	require.Equal(t, t0.Add(XIDMaxStale-2*time.Second), refreshed.QueriedAt)
	require.Equal(t, step(4), refreshed.ByUUID["GPU-a"][0].LastObserved)

	// Within the TTL of the new result: a hit.
	require.Same(t, refreshed, c.Get(ctx))
	require.Equal(t, int32(2), f.calls.Load())
}

func TestXIDCacheTimeout(t *testing.T) {
	clock := newTestClock(time.Now())
	f := &fakeQuery{block: true}
	c := newTestCache(f, clock)
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

	// A background query times out the same way.
	clock.add(XIDCacheTTL)
	require.Same(t, s, c.Get(context.Background()))
	waitForQuery(c)
	require.Equal(t, int32(2), f.calls.Load())
	require.Equal(t, errTimeout, c.Peek().Error)
	require.NotSame(t, s, c.Peek())
}

// Callers that arrive during the first query wait for it: one query for all of them.
func TestXIDCacheOneQueryAtATime(t *testing.T) {
	clock := newTestClock(time.Now())
	gate := make(chan struct{})
	f := &fakeQuery{gate: gate}
	c := newTestCache(f, clock)
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
	close(gate)
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

	require.Nil(t, c.LastOK())

	var none *XIDCache
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_NOT_CONFIGURED,
		none.Get(context.Background()).Status)
	require.Nil(t, none.LastOK())
}
