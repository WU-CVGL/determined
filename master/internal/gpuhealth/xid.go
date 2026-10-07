// Package gpuhealth classifies the health of an agent's GPUs and keeps the master's view of the
// GPUs' recent critical XIDs, which it reads from the cluster's DCGM-Exporter in Prometheus.
//
// The agent API (GetAgent, and GetAgents without exclude_slots) queries through XIDCache.Get.
package gpuhealth

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/determined-ai/determined/proto/pkg/agentv1"
)

const (
	// XIDWindow is how far back a GPU's critical XIDs count.
	XIDWindow = 24 * time.Hour
	// XIDStep is the step of the range query. It equals the lookback of max_over_time in the
	// query, so the windows of consecutive steps cover every sample.
	XIDStep = 5 * time.Minute
	// XIDCacheTTL is how long a query result, successful or not, is reused without a refresh.
	XIDCacheTTL = 30 * time.Second
	// XIDMaxStale is the age from which a request waits for a new result instead of getting the
	// last one while the cache refreshes in the background. It is one step: a result reused
	// before then misses less than one window of samples.
	XIDMaxStale = XIDStep
	// XIDQueryTimeout bounds one query.
	XIDQueryTimeout = 5 * time.Second

	// xidLookback is XIDStep as a PromQL duration.
	xidLookback = "5m"
	// ignoredXIDPattern matches the codes of ignoredXIDCodes. PromQL regular expressions are fully
	// anchored.
	ignoredXIDPattern = "13|31|43|45"

	errTimeout         = "timeout"
	errRequestFailed   = "request failed"
	errInvalidResponse = "invalid response"
)

// ignoredXIDCodes are the XIDs that never count: 13 (graphics engine exception), 31 (GPU memory
// page fault), 43 (GPU stopped processing) and 45 (preemptive cleanup). This is an exclusion
// policy, not a statement about their cause: it matches the cluster's gpu-xid-critical alert and
// keeps out codes that applications commonly trigger, which would raise false alarms. It does not
// mean that they always come from user code: NVIDIA says that XID 31 is usually an application
// error but can be a driver or hardware error. A fault seen only as these codes does not count.
var ignoredXIDCodes = map[int]bool{13: true, 31: true, 43: true, 45: true}

// IsCriticalXID reports whether an XID code counts as GPU-side evidence: every code except 0 and
// ignoredXIDCodes.
func IsCriticalXID(code int) bool {
	return code > 0 && !ignoredXIDCodes[code]
}

// XIDQuery is the PromQL expression for the critical XIDs of the GPUs of a cluster. The exporter's
// DCGM_EXP_XID_ERRORS_COUNT is a windowed gauge, the number of XID records per GPU and code (label
// xid) in its sliding window, with one 0-valued series without xid per GPU when there is none. So
// the query takes max_over_time of the gauge, never increase(), which is for counters.
func XIDQuery(detCluster string) string {
	return `max by (gpu_uuid, xid) (max_over_time(DCGM_EXP_XID_ERRORS_COUNT{job="dcgm", det_cluster=` +
		strconv.Quote(detCluster) + `, gpu_uuid!="", xid!="", xid!="0", xid!~"` + ignoredXIDPattern +
		`"}[` + xidLookback + `])) > 0`
}

// XIDRange is the range of the query at now: XIDWindow, ending at the first step at or after now.
// The steps lie on a grid of XIDStep, so the windows of a GPU's XIDs stay the same from one query
// to the next.
func XIDRange(now time.Time) (start, end time.Time) {
	end = now.Truncate(XIDStep)
	if end.Before(now) {
		end = end.Add(XIDStep)
	}
	return end.Add(-XIDWindow), end
}

// Sample is one point of a range query result.
type Sample struct {
	Time  time.Time
	Value float64
}

// Series is one series of a range query result.
type Series struct {
	Labels  map[string]string
	Samples []Sample
}

// RangeQuery runs a Prometheus range query.
type RangeQuery func(ctx context.Context, expr string, start, end time.Time, step time.Duration) (
	[]Series, error)

// XID is one critical XID code of a GPU. FirstObserved and LastObserved are the ends of the first
// and the last query windows of XIDStep that saw the code: the XID happened before them.
type XID struct {
	Code          int
	FirstObserved time.Time
	LastObserved  time.Time
}

// XIDSnapshot is the result of one query. It is never changed after it is made.
type XIDSnapshot struct {
	Status agentv1.GpuXidQueryStatus
	// Error is why the query failed: a fixed text that never holds the Prometheus URL or response.
	Error string
	// QueriedAt is when the master queried, or zero when it did not.
	QueriedAt time.Time
	// ByUUID holds the critical XIDs of each GPU by its UUID, by code. A failed query holds those of
	// the last successful one that are still recent (stillRecent).
	ByUUID map[string][]XID
}

var notConfigured = &XIDSnapshot{Status: agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_NOT_CONFIGURED}

// recentXIDs reads the first and last observed window of each GPU and code from a query result.
// A series without the labels of the query makes the result invalid. Codes that are not critical
// are dropped, also if Prometheus returns them.
func recentXIDs(series []Series) (map[string][]XID, error) {
	type key struct {
		uuid string
		code int
	}
	seen := map[key]*XID{}
	for _, s := range series {
		uuid := s.Labels["gpu_uuid"]
		code, err := strconv.Atoi(s.Labels["xid"])
		if uuid == "" || err != nil {
			return nil, fmt.Errorf("series without gpu_uuid or a numeric xid")
		}
		if !IsCriticalXID(code) {
			continue
		}
		for _, p := range s.Samples {
			if !(p.Value > 0) || math.IsInf(p.Value, 0) {
				continue
			}
			x, ok := seen[key{uuid, code}]
			if !ok {
				seen[key{uuid, code}] = &XID{Code: code, FirstObserved: p.Time, LastObserved: p.Time}
				continue
			}
			if p.Time.Before(x.FirstObserved) {
				x.FirstObserved = p.Time
			}
			if p.Time.After(x.LastObserved) {
				x.LastObserved = p.Time
			}
		}
	}
	byUUID := map[string][]XID{}
	for k, x := range seen {
		byUUID[k.uuid] = append(byUUID[k.uuid], *x)
	}
	for _, xids := range byUUID {
		sort.Slice(xids, func(i, j int) bool { return xids[i].Code < xids[j].Code })
	}
	return byUUID, nil
}

// XIDCache is the master's one cache of recent critical XIDs. Every result, successful or not, is
// reused for XIDCacheTTL, and one query runs at a time, so a master sends at most one query per
// TTL. After the TTL, a request gets the last result at once and starts a refresh in the
// background, unless the result is XIDMaxStale old: then it waits for the new one, at most
// XIDQueryTimeout. So a slow Prometheus delays a request only at the first query and after a quiet
// period, never while a client polls.
type XIDCache struct {
	query      RangeQuery
	detCluster string
	ttl        time.Duration
	maxStale   time.Duration
	timeout    time.Duration
	now        func() time.Time

	// mu is held while a query runs, in the background too; last and lastOK are read without it.
	mu     sync.Mutex
	last   atomic.Pointer[XIDSnapshot]
	lastOK atomic.Pointer[XIDSnapshot]
}

// NewXIDCache returns a cache that queries with query for the GPUs of detCluster. A nil query means
// that the master has no Prometheus: the status is then NOT_CONFIGURED.
func NewXIDCache(detCluster string, query RangeQuery) *XIDCache {
	return &XIDCache{
		query: query, detCluster: detCluster,
		ttl: XIDCacheTTL, maxStale: XIDMaxStale, timeout: XIDQueryTimeout, now: time.Now,
	}
}

// Get returns the recent critical XIDs. A result younger than the TTL is returned as it is. An
// older one is returned at once while one background query refreshes it, unless it is XIDMaxStale
// old or there is none yet: then Get queries and waits, and callers that arrive during a query
// wait for its result. A query does not end with ctx: its result serves every caller.
func (c *XIDCache) Get(ctx context.Context) *XIDSnapshot {
	if c == nil || c.query == nil {
		return notConfigured
	}
	seen := c.last.Load()
	if seen != nil {
		age := c.now().Sub(seen.QueriedAt)
		if age < c.ttl {
			return seen
		}
		if age < c.maxStale {
			// A failed TryLock means a query is running already.
			if c.mu.TryLock() {
				go func() {
					defer c.mu.Unlock()
					if c.last.Load() == seen {
						c.store(c.fetch(context.Background()))
					}
				}()
			}
			return seen
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// A query that ended while this caller waited serves it too.
	if s := c.last.Load(); s != seen {
		return s
	}
	s := c.fetch(ctx)
	c.store(s)
	return s
}

// Peek returns the last result without querying, whatever its age, or nil before the first query.
func (c *XIDCache) Peek() *XIDSnapshot {
	if c == nil || c.query == nil {
		return notConfigured
	}
	return c.last.Load()
}

// LastOK returns the last successful result without querying, whatever its age, or nil if no
// query has succeeded: a failed query leaves it as it was, and keeps its XIDs that are still
// recent.
func (c *XIDCache) LastOK() *XIDSnapshot {
	if c == nil {
		return nil
	}
	return c.lastOK.Load()
}

func (c *XIDCache) store(s *XIDSnapshot) {
	c.last.Store(s)
	if s.Status == agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_OK {
		c.lastOK.Store(s)
	}
}

func (c *XIDCache) fetch(ctx context.Context) *XIDSnapshot {
	queriedAt := c.now()
	start, end := XIDRange(queriedAt)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
	defer cancel()

	failed := func(text string) *XIDSnapshot {
		// Never log the Prometheus URL or response: the text is a fixed one.
		log.WithField("component", "gpu-xids").Debugf("the Prometheus query for GPU XIDs failed: %s", text)
		return &XIDSnapshot{
			Status: agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_FAILED, Error: text, QueriedAt: queriedAt,
			ByUUID: stillRecent(c.lastOK.Load(), queriedAt),
		}
	}
	series, err := c.query(ctx, XIDQuery(c.detCluster), start, end, XIDStep)
	switch {
	case err != nil && (errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil):
		return failed(errTimeout)
	case err != nil:
		return failed(errRequestFailed)
	}
	byUUID, err := recentXIDs(series)
	if err != nil {
		return failed(errInvalidResponse)
	}
	return &XIDSnapshot{
		Status: agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_OK, QueriedAt: queriedAt, ByUUID: byUUID,
	}
}

// stillRecent returns the XIDs of ok, the last successful result, that a query at now would still
// see: those whose last window is in XIDRange(now). A failed query keeps them, so a GPU in error
// does not turn green while Prometheus cannot be reached, and drops each one when it leaves the 24
// hours, however old ok is. It returns new maps and slices and nil when none is left, or no query
// has succeeded.
func stillRecent(ok *XIDSnapshot, now time.Time) map[string][]XID {
	if ok == nil {
		return nil
	}
	start, _ := XIDRange(now)
	var byUUID map[string][]XID
	for uuid, xids := range ok.ByUUID {
		var kept []XID
		for _, x := range xids {
			if !x.LastObserved.Before(start) {
				kept = append(kept, x)
			}
		}
		if len(kept) == 0 {
			continue
		}
		if byUUID == nil {
			byUUID = map[string][]XID{}
		}
		byUUID[uuid] = kept
	}
	return byUUID
}
