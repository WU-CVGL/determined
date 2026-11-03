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

    def _send(self, method, path, json):
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
        with opener.open(request, timeout=5) as response:
            response.read()

    def post(self, path, json=None):
        self._send("POST", path, json)

    def put(self, path, json=None):
        self._send("PUT", path, json)
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
// request for the kernels starts a round of checks.
type checkIdleJupyter struct {
	baseURL string

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
		j.state = "idle"
		if j.rounds < len(checkIdleRounds) {
			j.state = checkIdleRounds[j.rounds]
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

// checkIdleMaster records the reports that come with the allocation token.
type checkIdleMaster struct {
	mu      sync.Mutex
	reports []checkIdleReport
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
	m.mu.Unlock()
	_, _ = w.Write([]byte("{}"))
}

// runCheckIdle runs check_idle.py under the system python3 with the stand-in determined package,
// against a stand-in JupyterLab at baseURL, with the extra environment env, until JupyterLab has
// answered every round of checkIdleRounds, and returns the reports that reached the master by
// then.
func runCheckIdle(t *testing.T, baseURL string, env ...string) []checkIdleReport {
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
	script = strings.Replace(script, "\nREPORT_IDLE_INTERVAL = 30\n",
		"\nREPORT_IDLE_INTERVAL = "+checkIdleReportInterval+"\n", 1)
	scriptPath := filepath.Join(dir, "check_idle.py")
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), 0o600))

	jupyter := &checkIdleJupyter{baseURL: baseURL}
	if baseURL == "" {
		jupyter.baseURL = "/proxy/task-1/"
	}
	jupyterSrv := httptest.NewUnstartedServer(http.HandlerFunc(jupyter.serve))
	// check_idle.py first waits for the port with a plain connection, which ends without TLS.
	jupyterSrv.Config.ErrorLog = log.New(io.Discard, "", 0)
	jupyterSrv.StartTLS()
	defer jupyterSrv.Close()
	jupyterURL, err := url.Parse(jupyterSrv.URL)
	require.NoError(t, err)
	master := &checkIdleMaster{}
	masterSrv := httptest.NewServer(http.HandlerFunc(master.serve))
	defer masterSrv.Close()

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
	var stderr syncBuffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Once the next round has started, the report of the last observed one has reached the master.
	deadline := time.Now().Add(30 * time.Second)
	for jupyter.roundsStarted() <= checkIdleRoundsToObserve {
		require.True(t, time.Now().Before(deadline), "check_idle.py stopped checking: %s", stderr.String())
		time.Sleep(10 * time.Millisecond)
	}
	master.mu.Lock()
	defer master.mu.Unlock()
	return append([]checkIdleReport{}, master.reports...)
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
