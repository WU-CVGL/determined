//go:build linux

package etc

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// readyCheckStandIn replaces the parts of determinedStandIn that check_ready_logs.py uses: the
// errors of the SDK's session, as determined.common.api.errors defines them, and a task session
// whose posts wait for the file GATE, record "<path> <timeout>" in POSTS_FILE and then answer as
// the next outcome in READY_OUTCOMES says: ok, or the error that the SDK's session raises for a
// connection failure, a read timeout, or an HTTP status.
var readyCheckStandIn = map[string]string{
	"determined/common/api/errors.py": `class BadRequestException(Exception):
    pass


class MasterNotFoundException(BadRequestException):
    pass


class APIException(BadRequestException):
    def __init__(self, status_code):
        super().__init__(f"status {status_code}")
        self.status_code = status_code


class NotFoundException(APIException):
    def __init__(self, message):
        super().__init__(404)


class ForbiddenException(BadRequestException):
    pass


class UnauthenticatedException(BadRequestException):
    pass
`,
	"determined/common/api/authentication.py": `import os
import time

from determined.common.api import errors


class ScriptedSession:
    def post(self, path, json=None, timeout=None):
        for _ in range(200):
            if os.path.exists(os.environ["GATE"]):
                break
            time.sleep(0.05)
        posts = os.environ["POSTS_FILE"]
        done = 0
        if os.path.exists(posts):
            with open(posts) as f:
                done = len(f.readlines())
        with open(posts, "a") as f:
            f.write(f"{path} {timeout}\n")
        outcome = os.environ["READY_OUTCOMES"].split(",")[done]
        if outcome == "connection":
            raise errors.MasterNotFoundException("connection refused")
        if outcome == "read timeout":
            raise errors.BadRequestException("read timed out")
        if outcome == "forbidden":
            raise errors.ForbiddenException("forbidden")
        if outcome == "unauthenticated":
            raise errors.UnauthenticatedException("unauthenticated")
        if outcome == "not found":
            raise errors.NotFoundException("not found")
        if outcome != "ok":
            raise errors.APIException(int(outcome))


def login_from_task(master_address, cert):
    return ScriptedSession()
`,
}

// readyCheck runs the real check_ready_logs.py under python3 with the stand-in determined package,
// its backoff shortened, and the log lines on its input. The argument FIFO stands for the path of
// a FIFO that the test holds open, as the supervisor does.
type readyCheck struct {
	t     *testing.T
	dir   string
	gate  string
	posts string
	fifo  *os.File
	lines chan string
	cmd   *exec.Cmd
}

func newReadyCheck(t *testing.T, outcomes []string, input string, args ...string) *readyCheck {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	dir := t.TempDir()
	c := &readyCheck{
		t:     t,
		dir:   dir,
		gate:  filepath.Join(dir, "gate"),
		posts: filepath.Join(dir, "posts"),
		lines: make(chan string, 10),
	}
	script := staticScript(t, TaskCheckReadyLogsResource)
	require.Contains(t, script, "\nBACKOFF_SECONDS = 5\n")
	script = strings.Replace(script, "\nBACKOFF_SECONDS = 5\n", "\nBACKOFF_SECONDS = 0.05\n", 1)
	require.NoError(t, os.WriteFile(filepath.Join(dir, TaskCheckReadyLogsResource), []byte(script), 0o600))
	pythonPath := filepath.Join(dir, "pylib")
	writeDeterminedStandIn(t, pythonPath)
	for name, body := range readyCheckStandIn {
		require.NoError(t, os.WriteFile(filepath.Join(pythonPath, name), []byte(body), 0o600))
	}

	// The supervisor holds the FIFO open; without a reader the check's write would fail.
	fifo := filepath.Join(dir, "states")
	require.NoError(t, unix.Mkfifo(fifo, 0o600))
	c.fifo, err = os.OpenFile(fifo, os.O_RDWR, 0) //nolint:gosec
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.fifo.Close() })
	go func() {
		scanner := bufio.NewScanner(c.fifo)
		for scanner.Scan() {
			c.lines <- scanner.Text()
		}
	}()

	args = append([]string{filepath.Join(dir, TaskCheckReadyLogsResource)}, args...)
	for i, a := range args {
		if a == "FIFO" {
			args[i] = fifo
		}
	}
	c.cmd = exec.Command(python, args...) //nolint:gosec
	c.cmd.Stdin = strings.NewReader(input)
	c.cmd.Env = append(os.Environ(),
		"PYTHONPATH="+pythonPath,
		"DET_MASTER=http://master.invalid:8080",
		"DET_USER=det",
		"DET_SESSION_TOKEN="+standInAllocationToken,
		"DET_ALLOCATION_ID="+standInAllocationID,
		"GATE="+c.gate,
		"POSTS_FILE="+c.posts,
		"READY_OUTCOMES="+strings.Join(outcomes, ","),
	)
	return c
}

// openGate lets the stand-in session's posts go on.
func (c *readyCheck) openGate() {
	require.NoError(c.t, os.WriteFile(c.gate, nil, 0o600))
}

// readFIFO returns the next line that the check wrote to the FIFO.
func (c *readyCheck) readFIFO() string {
	c.t.Helper()
	select {
	case line := <-c.lines:
		return line
	case <-time.After(10 * time.Second):
		c.t.Fatal("no line in the FIFO")
		return ""
	}
}

// wait returns the check's exit status and its posts.
func (c *readyCheck) wait() (int, []string) {
	c.t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
		c.t.Fatal("the check did not exit")
	}
	b, err := os.ReadFile(c.posts)
	if !os.IsNotExist(err) {
		require.NoError(c.t, err)
	}
	return c.cmd.ProcessState.ExitCode(), strings.Split(strings.TrimSpace(string(b)), "\n")
}

// A readiness check writes the service's READY to the FIFO as soon as its regex matches, before it
// posts the allocation's readiness, so that a master it cannot reach does not hold up the service's
// state. It retries the post, with a timeout, while the session's error is one that may pass: a
// connection failure, a read timeout, or a 5xx status. Any other error ends it at once.
func TestCheckReadyLogsReportsBeforePost(t *testing.T) {
	ready := "/api/v1/allocations/" + standInAllocationID + "/ready 10"
	for name, tc := range map[string]struct {
		outcomes []string
		status   int
	}{
		"retried":         {[]string{"connection", "read timeout", "503", "500", "ok"}, 0},
		"forbidden":       {[]string{"forbidden"}, 1},
		"unauthenticated": {[]string{"unauthenticated"}, 1},
		"not found":       {[]string{"not found"}, 1},
		"bad request":     {[]string{"400"}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			c := newReadyCheck(t, tc.outcomes, "starting\nup\nlater\n",
				"--ready-regex", "up", "--service", "jupyter", "--report-to", "FIFO")
			var stderr strings.Builder
			c.cmd.Stderr = &stderr
			require.NoError(t, c.cmd.Start())
			// The posts wait for the gate, so the READY line comes before any of them.
			require.Equal(t, "jupyter READY", c.readFIFO())
			c.openGate()
			status, posts := c.wait()
			require.Equal(t, tc.status, status, stderr.String())
			require.Len(t, posts, len(tc.outcomes), posts)
			for _, p := range posts {
				require.Equal(t, ready, p)
			}
			require.Contains(t, stderr.String(), "[jupyter] ready\n")
		})
	}
}

// A TensorBoard's check, which has no service and reports nothing, posts the allocation's waiting
// state with the same retries.
func TestCheckReadyLogsPostsWaiting(t *testing.T) {
	c := newReadyCheck(t, []string{"connection", "502", "ok"}, "waiting\n",
		"--ready-regex", "up", "--waiting-regex", "waiting")
	c.openGate()
	require.NoError(t, c.cmd.Start())
	status, posts := c.wait()
	require.Equal(t, 0, status)
	waiting := "/api/v1/allocations/" + standInAllocationID + "/waiting 10"
	require.Equal(t, []string{waiting, waiting, waiting}, posts)
	select {
	case line := <-c.lines:
		t.Fatalf("unexpected line in the FIFO: %q", line)
	default:
	}
}
