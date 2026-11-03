//go:build linux

package etc

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// checkIdleDetermined is the part of the determined package that check_idle.py uses: the task
// session sends JSON with the allocation token, as the real one does, and raises on an HTTP error;
// it refuses a shell's report that would be retried, and it bounds a report by the total time of a
// urllib3 timeout.
var checkIdleDetermined = map[string]string{
	"determined/__init__.py":        "",
	"determined/common/__init__.py": "",
	"determined/common/api/__init__.py": `import json as _json
import urllib.request


def canonicalize_master_url(url):
    return url.rstrip("/")


class TaskSession:
    def __init__(self, master, token):
        self.master = master
        self.token = token
        self.max_retries = None

    def _send(self, method, path, json, timeout):
        request = urllib.request.Request(
            self.master + path,
            data=_json.dumps(json).encode(),
            method=method,
            headers={
                "Content-Type": "application/json",
                "Grpc-Metadata-x-allocation-token": "Bearer " + self.token,
            },
        )
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        timeout = getattr(timeout, "total", timeout)
        with opener.open(request, timeout=timeout) as response:
            response.read()

    def with_retry(self, max_retries):
        self.max_retries = max_retries
        return self

    def post(self, path, json=None, timeout=None):
        if self.max_retries != 0:
            raise RuntimeError("a shell's report is not retried")
        self._send("POST", path, json, timeout)

    def put(self, path, json=None, timeout=None):
        self._send("PUT", path, json, timeout)
`,
	"determined/common/api/authentication.py": `import os

from determined.common import api


def login_from_task(master_address, cert):
    return api.TaskSession(master_address, os.environ["DET_SESSION_TOKEN"])
`,
	"determined/common/api/certs.py": `def default_load(master_url):
    return None
`,
}

const (
	checkIdleToken           = "allocation-token"
	checkIdleNotebookToken   = "secret"
	checkIdleAllocationID    = "task-1.1"
	checkIdleReportInterval  = "0.05"
	checkIdleRoundsToObserve = 8
	checkIdleKernelsPath     = "/api/kernels"
	checkIdleTerminalsPath   = "/api/terminals"
	checkIdleSessionsPath    = "/api/sessions"
)

// checkIdleRounds is what the stand-in JupyterLab answers in each round of checks, one per report:
// idle, then busy, then unreadable. Later rounds are idle.
var checkIdleRounds = []string{"idle", "idle", "idle", "busy", "busy", "broken", "broken", "broken"}

// checkIdleJupyter stands in for JupyterLab's REST API under its base URL with its token. Each
// request for the kernels starts a round of checks, which answers as states says, then as after,
// and every answer comes delay late, every TLS handshake handshakeDelay late. It records the path
// of every request and when it came.
type checkIdleJupyter struct {
	baseURL        string
	states         []string
	after          string
	delay          time.Duration
	handshakeDelay time.Duration

	mu       sync.Mutex
	rounds   int
	state    string
	requests []checkIdleRequest
}

// checkIdleRequest is a request that the stand-in JupyterLab received.
type checkIdleRequest struct {
	path string
	at   time.Time
}

func (j *checkIdleJupyter) serve(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, strings.TrimSuffix(j.baseURL, "/"))
	if !ok || r.Header.Get("Authorization") != "token "+checkIdleNotebookToken {
		http.NotFound(w, r)
		return
	}
	j.mu.Lock()
	j.requests = append(j.requests, checkIdleRequest{path: path, at: time.Now()})
	if path == checkIdleKernelsPath {
		j.state = j.after
		if j.rounds < len(j.states) {
			j.state = j.states[j.rounds]
		}
		j.rounds++
	}
	state := j.state
	j.mu.Unlock()
	if j.delay > 0 {
		select {
		case <-time.After(j.delay):
		case <-r.Context().Done():
			return
		}
	}

	switch {
	case state == "broken":
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	case state == "busy" && path == checkIdleKernelsPath:
		_, _ = w.Write([]byte(`[{"id": "k", "execution_state": "busy", "connections": 1,
			"last_activity": "2024-01-01T00:00:00Z"}]`))
	default:
		_, _ = w.Write([]byte("[]"))
	}
}

func (j *checkIdleJupyter) roundsStarted() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.rounds
}

// received returns the requests that the stand-in JupyterLab has received.
func (j *checkIdleJupyter) received() []checkIdleRequest {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.requests)
}

// roundStarts returns when each round of checks started.
func (j *checkIdleJupyter) roundStarts() []time.Time {
	var starts []time.Time
	for _, r := range j.received() {
		if r.path == checkIdleKernelsPath {
			starts = append(starts, r.at)
		}
	}
	return starts
}

// checkIdleStallListener delays the first read on each connection it accepts by stall, and so the
// TLS handshake on it.
type checkIdleStallListener struct {
	net.Listener
	stall time.Duration
}

func (l checkIdleStallListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &checkIdleStallConn{Conn: conn, stall: l.stall}, nil
}

type checkIdleStallConn struct {
	net.Conn
	stall time.Duration
	once  sync.Once
}

func (c *checkIdleStallConn) Read(b []byte) (int, error) {
	c.once.Do(func() { time.Sleep(c.stall) })
	return c.Conn.Read(b)
}

// checkIdleReport is a report that the stand-in master received.
type checkIdleReport struct {
	Method, Path string
	Body         map[string]any
}

// checkIdleMaster records the reports that come with the allocation token, and when they came, and
// answers each delay later.
type checkIdleMaster struct {
	delay time.Duration

	mu       sync.Mutex
	reports  []checkIdleReport
	arrivals []time.Time
}

// received returns the reports that the stand-in master has received and when each came.
func (m *checkIdleMaster) received() ([]checkIdleReport, []time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.reports), slices.Clone(m.arrivals)
}

func (m *checkIdleMaster) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if r.Header.Get("Grpc-Metadata-x-allocation-token") != "Bearer "+checkIdleToken ||
		json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "unexpected request", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.reports = append(m.reports, checkIdleReport{Method: r.Method, Path: r.URL.Path, Body: body})
	m.arrivals = append(m.arrivals, time.Now())
	m.mu.Unlock()
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-r.Context().Done():
			return
		}
	}
	_, _ = w.Write([]byte("{}"))
}

// checkIdleRun is a running check_idle.py with its stand-ins.
type checkIdleRun struct {
	jupyter *checkIdleJupyter
	master  *checkIdleMaster
	stderr  *syncBuffer
}

// startCheckIdle starts check_idle.py under the system python3 with the stand-in determined
// package, against a stand-in JupyterLab at baseURL, which answers as jupyter's states and after
// say, with the extra environment env, until the test ends. With interval set, the script's report
// interval is replaced by it; otherwise the script runs as the master ships it.
func startCheckIdle(
	t *testing.T, jupyter *checkIdleJupyter, interval string, env ...string,
) *checkIdleRun {
	t.Helper()
	return startCheckIdleWith(t, jupyter, &checkIdleMaster{}, interval, env...)
}

// startCheckIdleWith is startCheckIdle with the stand-in master master.
func startCheckIdleWith(
	t *testing.T, jupyter *checkIdleJupyter, master *checkIdleMaster, interval string,
	env ...string,
) *checkIdleRun {
	t.Helper()
	if out, err := exec.Command("python3", "-c", "import requests").CombinedOutput(); err != nil {
		t.Skipf("python3 with requests is not installed: %v %s", err, out)
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib")
	for name, body := range checkIdleDetermined {
		path := filepath.Join(lib, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}
	script := staticScript(t, NotebookIdleCheckResource)
	require.Contains(t, script, "\nREPORT_IDLE_INTERVAL = 30\n")
	if interval != "" {
		script = strings.Replace(script, "\nREPORT_IDLE_INTERVAL = 30\n",
			"\nREPORT_IDLE_INTERVAL = "+interval+"\n", 1)
	}
	scriptPath := filepath.Join(dir, "check_idle.py")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o600))

	if jupyter.baseURL == "" {
		jupyter.baseURL = "/proxy/task-1/"
	}
	jupyterSrv := httptest.NewUnstartedServer(http.HandlerFunc(jupyter.serve))
	// check_idle.py first waits for the port with a plain connection, which ends without TLS.
	jupyterSrv.Config.ErrorLog = log.New(io.Discard, "", 0)
	if jupyter.handshakeDelay > 0 {
		jupyterSrv.Listener = checkIdleStallListener{
			Listener: jupyterSrv.Listener, stall: jupyter.handshakeDelay,
		}
	}
	jupyterSrv.StartTLS()
	t.Cleanup(jupyterSrv.Close)
	jupyterURL, err := url.Parse(jupyterSrv.URL)
	require.NoError(t, err)
	masterSrv := httptest.NewServer(http.HandlerFunc(master.serve))
	t.Cleanup(masterSrv.Close)

	cmd := exec.Command("python3", scriptPath) //nolint:gosec
	// The task's environment only: no proxy reaches the stand-ins.
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"PYTHONPATH=" + lib,
		"NO_PROXY=127.0.0.1,localhost",
		"DET_MASTER=" + masterSrv.URL,
		"DET_SESSION_TOKEN=" + checkIdleToken,
		"DET_ALLOCATION_ID=" + checkIdleAllocationID,
		"DET_TASK_ID=task-1",
		"DET_NOTEBOOK_TOKEN=" + checkIdleNotebookToken,
		"NOTEBOOK_PORT=" + jupyterURL.Port(),
		"NOTEBOOK_IDLE_TYPE=kernels_or_terminals",
	}, env...)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	require.NoError(t, cmd.Start())
	// Registered last, so it runs before the stand-ins close.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return &checkIdleRun{jupyter: jupyter, master: master, stderr: stderr}
}

// waitRounds waits until the stand-in JupyterLab has seen n rounds of checks start.
func (r *checkIdleRun) waitRounds(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for r.jupyter.roundsStarted() < n {
		require.True(t, time.Now().Before(deadline), "check_idle.py stopped checking: %s",
			r.stderr.String())
		time.Sleep(10 * time.Millisecond)
	}
}

// runCheckIdle runs check_idle.py with a report interval of checkIdleReportInterval against a
// stand-in JupyterLab at baseURL that answers as checkIdleRounds says, with the extra environment
// env, until JupyterLab has answered every round, and returns the reports that reached the master
// by then.
func runCheckIdle(t *testing.T, baseURL string, env ...string) []checkIdleReport {
	t.Helper()
	run := startCheckIdle(t, &checkIdleJupyter{
		baseURL: baseURL, states: checkIdleRounds, after: "idle",
	}, checkIdleReportInterval, env...)

	// Once the next round has started, the report of the last observed one has reached the master.
	deadline := time.Now().Add(30 * time.Second)
	for run.jupyter.roundsStarted() <= checkIdleRoundsToObserve {
		require.True(t, time.Now().Before(deadline), "check_idle.py stopped checking: %s",
			run.stderr.String())
		time.Sleep(10 * time.Millisecond)
	}
	run.master.mu.Lock()
	defer run.master.mu.Unlock()
	return append([]checkIdleReport{}, run.master.reports...)
}

// On a shell, check_idle.py checks JupyterLab under its base URL and reports a busy JupyterLab to
// the task's activity endpoint; an idle or unreadable one is not reported.
func TestCheckIdleReportsTaskActivity(t *testing.T) {
	reports := runCheckIdle(t, "/proxy/task-1:2900/",
		"NOTEBOOK_BASE_URL=/proxy/task-1:2900/", "DET_ACTIVITY_REPORT=task")
	activity := checkIdleReport{
		Method: http.MethodPost,
		Path:   "/api/v1/allocations/" + checkIdleAllocationID + "/activity",
		Body:   map[string]any{"active": true},
	}
	require.Equal(t, []checkIdleReport{activity, activity}, reports)
}

// On a notebook, check_idle.py reports every check to the notebook API as before, an unreadable
// JupyterLab as active; tasks saved before the master set the base URL are at their task ID.
func TestCheckIdleReportsNotebookIdle(t *testing.T) {
	for name, env := range map[string][]string{
		"base URL":    {"NOTEBOOK_BASE_URL=/proxy/task-1/"},
		"no base URL": nil,
	} {
		t.Run(name, func(t *testing.T) {
			reports := runCheckIdle(t, "", env...)
			require.GreaterOrEqual(t, len(reports), checkIdleRoundsToObserve)
			var idle []any
			for _, r := range reports[:checkIdleRoundsToObserve] {
				require.Equal(t, http.MethodPut, r.Method)
				require.Equal(t, "/api/v1/notebooks/task-1/report_idle", r.Path)
				idle = append(idle, r.Body["idle"])
			}
			require.Equal(t, []any{true, true, true, false, false, false, false, false}, idle)
		})
	}
}

// On a shell, check_idle.py reports a busy JupyterLab at a quarter of the task's idle timeout, at
// least every second, counted from the start of each round: with a timeout of 4 s, the reports
// reach the master about a second apart, so that its record is touched more often than the
// timeout. The script runs with the report interval it ships with.
func TestCheckIdleReportsWithinIdleTimeout(t *testing.T) {
	run := startCheckIdle(t, &checkIdleJupyter{baseURL: "/proxy/task-1:2900/", after: "busy"}, "",
		"NOTEBOOK_BASE_URL=/proxy/task-1:2900/", "DET_ACTIVITY_REPORT=task",
		"NOTEBOOK_IDLE_TYPE=activity", "DET_IDLE_TIMEOUT_SECONDS=4")

	const (
		observe = 4 * time.Second
		maxGap  = 1500 * time.Millisecond
		meanGap = 1200 * time.Millisecond
	)
	var arrivals []time.Time
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, arrivals = run.master.received()
		if len(arrivals) > 1 && arrivals[len(arrivals)-1].Sub(arrivals[0]) >= observe {
			break
		}
		require.True(t, time.Now().Before(deadline), "check_idle.py stopped reporting: %s",
			run.stderr.String())
		time.Sleep(50 * time.Millisecond)
	}
	require.GreaterOrEqual(t, len(arrivals), 5, "a report about every second over %s", observe)
	for i := 1; i < len(arrivals); i++ {
		require.LessOrEqual(t, arrivals[i].Sub(arrivals[i-1]), maxGap, "report %d", i)
	}
	// On average a second apart, not a third of the timeout.
	span := arrivals[len(arrivals)-1].Sub(arrivals[0])
	require.Less(t, span, time.Duration(len(arrivals)-1)*meanGap, "%d reports", len(arrivals))
}

// At a shell's shortest idle timeout, 10 s, a busy kernel keeps the shell alive although every
// JupyterLab answer takes 2 s: the rounds start 2.5 s apart and check_idle.py reads only the
// kernels before it reports, so the reports reach the master at most the interval and one answer
// apart.
func TestCheckIdleSlowJupyterShortestTimeout(t *testing.T) {
	const (
		interval = 2500 * time.Millisecond
		delay    = 2 * time.Second
	)
	run := startCheckIdle(t, &checkIdleJupyter{after: "busy", delay: delay}, "",
		"DET_ACTIVITY_REPORT=task", "NOTEBOOK_IDLE_TYPE=activity", "DET_IDLE_TIMEOUT_SECONDS=10")

	var arrivals []time.Time
	deadline := time.Now().Add(60 * time.Second)
	for len(arrivals) < 4 {
		require.True(t, time.Now().Before(deadline), "check_idle.py stopped reporting: %s",
			run.stderr.String())
		time.Sleep(50 * time.Millisecond)
		_, arrivals = run.master.received()
	}
	for i := 1; i < len(arrivals); i++ {
		require.Less(t, arrivals[i].Sub(arrivals[i-1]), interval+delay, "report %d", i)
	}
	starts := run.jupyter.roundStarts()
	for i := 1; i < len(starts); i++ {
		gap := starts[i].Sub(starts[i-1])
		require.GreaterOrEqual(t, gap, 2400*time.Millisecond, "round %d", i)
		require.Less(t, gap, 2900*time.Millisecond, "round %d", i)
	}
	for _, r := range run.jupyter.received() {
		require.Equal(t, checkIdleKernelsPath, r.path)
	}
}

// On a shell, the activity report gives up after a quarter of the idle timeout when that is
// shorter than 10 s: with a timeout of 10 s, a master that answers after 4 s still receives every
// report, and the next round starts 2.5 s after the report was sent.
func TestCheckIdleSlowMaster(t *testing.T) {
	run := startCheckIdleWith(t, &checkIdleJupyter{after: "busy"},
		&checkIdleMaster{delay: 4 * time.Second}, "",
		"DET_ACTIVITY_REPORT=task", "NOTEBOOK_IDLE_TYPE=activity", "DET_IDLE_TIMEOUT_SECONDS=10")
	run.waitRounds(t, 4)

	starts := run.jupyter.roundStarts()
	_, arrivals := run.master.received()
	require.GreaterOrEqual(t, len(arrivals), len(starts)-1)
	for i := 1; i < len(starts); i++ {
		gap := starts[i].Sub(arrivals[i-1])
		require.GreaterOrEqual(t, gap, 2400*time.Millisecond, "round %d", i)
		require.Less(t, gap, 2900*time.Millisecond, "round %d", i)
	}
}

// On a shell, check_idle.py reads what the idle type needs and reports as soon as it knows:
// a busy kernel in activity mode, and a kernel in kernels_or_terminals mode, after the kernels
// alone; kernel_connections reads only the kernels; the sessions are never read. A notebook reads
// the kernels, the terminals and the sessions in every round, as before.
func TestCheckIdleQueriesByIdleType(t *testing.T) {
	kernels := []string{checkIdleKernelsPath}
	kernelsTerminals := []string{checkIdleKernelsPath, checkIdleTerminalsPath}
	all := []string{checkIdleKernelsPath, checkIdleTerminalsPath, checkIdleSessionsPath}
	shell := "DET_ACTIVITY_REPORT=task"
	for name, tc := range map[string]struct {
		idleType, state string
		notebook        bool
		wantPaths       []string
		wantReports     bool
	}{
		"activity, busy":                {"activity", "busy", false, kernels, true},
		"activity, idle":                {"activity", "idle", false, kernelsTerminals, false},
		"kernel_connections, connected": {"kernel_connections", "busy", false, kernels, true},
		"kernel_connections, idle":      {"kernel_connections", "idle", false, kernels, false},
		"kernels_or_terminals, kernel":  {"kernels_or_terminals", "busy", false, kernels, true},
		"kernels_or_terminals, idle": {
			"kernels_or_terminals", "idle", false, kernelsTerminals, false,
		},
		"notebook, activity, busy":             {"activity", "busy", true, all, true},
		"notebook, kernel_connections, idle":   {"kernel_connections", "idle", true, all, true},
		"notebook, kernels_or_terminals, idle": {"kernels_or_terminals", "idle", true, all, true},
	} {
		t.Run(name, func(t *testing.T) {
			env := []string{"NOTEBOOK_IDLE_TYPE=" + tc.idleType}
			if !tc.notebook {
				env = append(env, shell)
			}
			run := startCheckIdle(t, &checkIdleJupyter{after: tc.state}, checkIdleReportInterval,
				env...)
			run.waitRounds(t, 5)

			// The rounds before the last, which may still be under way.
			received := run.jupyter.received()
			var rounds [][]string
			for _, r := range received {
				if r.path == checkIdleKernelsPath {
					rounds = append(rounds, nil)
				}
				rounds[len(rounds)-1] = append(rounds[len(rounds)-1], r.path)
			}
			for i, paths := range rounds[:len(rounds)-1] {
				require.Equal(t, tc.wantPaths, paths, "round %d", i)
			}
			reports, _ := run.master.received()
			if tc.wantReports {
				require.GreaterOrEqual(t, len(reports), len(rounds)-1)
			} else {
				require.Empty(t, reports)
			}
		})
	}
}

// On a shell, a JupyterLab request times out after a quarter of the idle timeout when that is
// shorter than 3 s: with a timeout of 10 s, a JupyterLab that answers after 4 s is unreadable, so
// it is not reported, and the rounds start 2.5 s apart.
func TestCheckIdleJupyterTimeoutFollowsInterval(t *testing.T) {
	run := startCheckIdle(t, &checkIdleJupyter{after: "busy", delay: 4 * time.Second}, "",
		"DET_ACTIVITY_REPORT=task", "NOTEBOOK_IDLE_TYPE=activity", "DET_IDLE_TIMEOUT_SECONDS=10")
	run.waitRounds(t, 3)

	starts := run.jupyter.roundStarts()
	for i := 1; i < len(starts); i++ {
		gap := starts[i].Sub(starts[i-1])
		require.GreaterOrEqual(t, gap, 2400*time.Millisecond, "round %d", i)
		require.Less(t, gap, 2900*time.Millisecond, "round %d", i)
	}
	reports, _ := run.master.received()
	require.Empty(t, reports)
}

// On a shell, a JupyterLab request's timeout bounds it from connecting to the answer: with a
// timeout of 10 s, a JupyterLab that delays its TLS handshake and its answer by 1.5 s each is
// unreadable, so it is not reported, and one that delays each by 1 s is reported.
func TestCheckIdleJupyterTimeoutBoundsWholeRequest(t *testing.T) {
	for name, tc := range map[string]struct {
		delay    time.Duration
		reported bool
	}{
		"3 s in all": {1500 * time.Millisecond, false},
		"2 s in all": {time.Second, true},
	} {
		t.Run(name, func(t *testing.T) {
			run := startCheckIdle(t, &checkIdleJupyter{
				after: "busy", delay: tc.delay, handshakeDelay: tc.delay,
			}, "", "DET_ACTIVITY_REPORT=task", "NOTEBOOK_IDLE_TYPE=activity",
				"DET_IDLE_TIMEOUT_SECONDS=10")
			// The third round starts once the second has sent its report, if any.
			run.waitRounds(t, 3)
			reports, _ := run.master.received()
			if tc.reported {
				require.GreaterOrEqual(t, len(reports), 2)
			} else {
				require.Empty(t, reports)
			}
		})
	}
}

// A JupyterLab request past check_idle.py's 3 s request timeout is unreadable on a shell, so it is
// not reported, and the next round starts on schedule: at once, since the round overran its
// interval. A notebook waits for the answer as before: an idle notebook is reported idle, not
// active.
func TestCheckIdleSlowJupyter(t *testing.T) {
	const delay = 3500 * time.Millisecond
	for name, tc := range map[string]struct {
		after string
		env   []string
		want  []checkIdleReport
	}{
		"shell": {
			after: "busy",
			env:   []string{"DET_ACTIVITY_REPORT=task", "DET_IDLE_TIMEOUT_SECONDS=30"},
		},
		"notebook": {
			after: "idle",
			want: []checkIdleReport{{
				Method: http.MethodPut,
				Path:   "/api/v1/notebooks/task-1/report_idle",
				Body:   map[string]any{"idle": true},
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			run := startCheckIdle(t, &checkIdleJupyter{after: tc.after, delay: delay},
				checkIdleReportInterval, tc.env...)
			// The second round starts once the first has ended and its report, if any, is sent.
			run.waitRounds(t, 2)
			reports, _ := run.master.received()
			require.Equal(t, tc.want, reports)
			if tc.want == nil {
				starts := run.jupyter.roundStarts()
				gap := starts[1].Sub(starts[0])
				require.GreaterOrEqual(t, gap, 3*time.Second)
				require.Less(t, gap, delay)
			}
		})
	}
}
