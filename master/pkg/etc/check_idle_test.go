//go:build linux

package etc

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// checkIdleDetermined is the part of the determined package that check_idle.py uses: the task
// session sends JSON with the allocation token, as the real one does, and raises on an HTTP error.
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
        with opener.open(request, timeout=timeout) as response:
            response.read()

    def post(self, path, json=None, timeout=None):
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
)

// checkIdleRounds is what the stand-in JupyterLab answers in each round of checks, one per report:
// idle, then busy, then unreadable. Later rounds are idle.
var checkIdleRounds = []string{"idle", "idle", "idle", "busy", "busy", "broken", "broken", "broken"}

// checkIdleJupyter stands in for JupyterLab's REST API under its base URL with its token. Each
// request for the kernels starts a round of checks, which answers as states says, then as after.
type checkIdleJupyter struct {
	baseURL string
	states  []string
	after   string

	mu     sync.Mutex
	rounds int
	state  string
}

func (j *checkIdleJupyter) serve(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, strings.TrimSuffix(j.baseURL, "/"))
	if !ok || r.Header.Get("Authorization") != "token "+checkIdleNotebookToken {
		http.NotFound(w, r)
		return
	}
	j.mu.Lock()
	if path == checkIdleKernelsPath {
		j.state = j.after
		if j.rounds < len(j.states) {
			j.state = j.states[j.rounds]
		}
		j.rounds++
	}
	state := j.state
	j.mu.Unlock()

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

// checkIdleReport is a report that the stand-in master received.
type checkIdleReport struct {
	Method, Path string
	Body         map[string]any
}

// checkIdleMaster records the reports that come with the allocation token, and when they came.
type checkIdleMaster struct {
	mu       sync.Mutex
	reports  []checkIdleReport
	arrivals []time.Time
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
	jupyterSrv.StartTLS()
	t.Cleanup(jupyterSrv.Close)
	jupyterURL, err := url.Parse(jupyterSrv.URL)
	require.NoError(t, err)
	master := &checkIdleMaster{}
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

// On a shell, check_idle.py reports a busy JupyterLab at a third of the task's idle timeout, at
// least every second, counted from the start of each round: with a timeout of 3 s, the reports
// reach the master about a second apart, so that its record is touched more often than the
// timeout. The script runs with the report interval it ships with.
func TestCheckIdleReportsWithinIdleTimeout(t *testing.T) {
	run := startCheckIdle(t, &checkIdleJupyter{baseURL: "/proxy/task-1:2900/", after: "busy"}, "",
		"NOTEBOOK_BASE_URL=/proxy/task-1:2900/", "DET_ACTIVITY_REPORT=task",
		"NOTEBOOK_IDLE_TYPE=activity", "DET_IDLE_TIMEOUT_SECONDS=3")

	const (
		observe = 4 * time.Second
		maxGap  = 1500 * time.Millisecond
	)
	var arrivals []time.Time
	deadline := time.Now().Add(30 * time.Second)
	for {
		run.master.mu.Lock()
		arrivals = append([]time.Time{}, run.master.arrivals...)
		run.master.mu.Unlock()
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
}
