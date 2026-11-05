//go:build linux

package etc

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/stretchr/testify/require"
)

// tensorBoardTestToken is the DET_TENSORBOARD_TOKEN of the tests.
const tensorBoardTestToken = "test-token"

// tensorBoardStandIn is a tensorboard package, by path under the package's directory. Its version
// is TB_STAND_IN_VERSION, 2.9.0 by default. Its core plugin's loader defines --bind_all with the
// default TB_STAND_IN_BIND_ALL, true by default as in some builds, and --host, --port,
// --path_prefix, --logdir and --load_fast. Its main, like TensorBoard's, refuses --host while
// --bind_all is true; otherwise it records its arguments NUL-separated, "tensorboard" in the order
// file and its interpreter, TB_STAND_IN_PYTHON, in "$ORDER_FILE.tensorboard-python", and serves HTTP
// at its --host, or all addresses with --bind_all, and --port, with 200 under its --path_prefix and
// 404 elsewhere, or with TB_STAND_IN_SERVE=no serves nothing, until the order file has "stop
// tensorboard". Then it returns TB_STAND_IN_EXIT, 0 by default.
var tensorBoardStandIn = map[string]string{
	"tensorboard/__init__.py":              "",
	"tensorboard/plugins/__init__.py":      "",
	"tensorboard/plugins/core/__init__.py": "",
	"tensorboard/version.py": `
import os

VERSION = os.environ.get("TB_STAND_IN_VERSION", "2.9.0")
`,
	"tensorboard/plugins/core/core_plugin.py": `
import os


class CorePluginLoader:
    def define_flags(self, parser):
        bind_all = os.environ.get("TB_STAND_IN_BIND_ALL", "true") == "true"
        parser.add_argument("--bind_all", default=bind_all, action="store_true")
        parser.add_argument("--host")
        parser.add_argument("--port", type=int)
        parser.add_argument("--path_prefix", default="")
        parser.add_argument("--logdir")
        parser.add_argument("--load_fast")
`,
	"tensorboard/main.py": `
import argparse
import http.server
import os
import sys
import threading
import time

from tensorboard.plugins.core import core_plugin


def stopped():
    try:
        with open(os.environ["ORDER_FILE"]) as f:
            return "stop tensorboard\n" in f.read()
    except OSError:
        return False


def run_main():
    argv = sys.argv[1:]
    parser = argparse.ArgumentParser(prog="tensorboard")
    core_plugin.CorePluginLoader().define_flags(parser)
    flags, _ = parser.parse_known_args(argv)
    if flags.bind_all and flags.host:
        print("Error: Must not specify both --host and --bind_all.", file=sys.stderr)
        return 1
    order = os.environ["ORDER_FILE"]
    with open(order + ".tensorboard-args", "w") as f:
        f.write("".join(arg + "\0" for arg in argv))
    with open(order + ".tensorboard-python", "w") as f:
        f.write(os.environ["TB_STAND_IN_PYTHON"])
    with open(order, "a") as f:
        f.write("tensorboard\n")
    if os.environ.get("TB_STAND_IN_SERVE") == "no":
        while not stopped():
            time.sleep(0.05)
        return 0

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(200 if self.path == flags.path_prefix + "/" else 404)
            self.end_headers()

        def log_message(self, *args):
            pass

    host = "" if flags.bind_all else flags.host or "localhost"
    server = http.server.HTTPServer((host, flags.port), Handler)

    def watch():
        while not stopped():
            time.sleep(0.05)
        server.shutdown()

    threading.Thread(target=watch, daemon=True).start()
    print("tensorboard stand-in serving", file=sys.stderr, flush=True)
    server.serve_forever()
    return int(os.environ.get("TB_STAND_IN_EXIT", "0"))
`,
}

// tensorBoardTask is a task with ssh, a stand-in that waits for "sshd done" in the order file, as
// its primary service, and the tensorboard service with the real unit and the harness's front, a
// directory on the PATH for stand-ins, a free port, the prefix /proxy/<task>:<port>/ and a token.
type tensorBoardTask struct {
	*supervisorRun
	bin    string
	port   string
	prefix string
}

func newTensorBoardTask(t *testing.T, logdir string) *tensorBoardTask {
	t.Helper()
	requireReporterPython(t)
	s := newSupervisorRun(t, "ssh,tensorboard")
	task := &tensorBoardTask{supervisorRun: s, bin: filepath.Join(s.dir, "bin"), port: freePort(t)}
	task.prefix = "/proxy/task-1:" + task.port + "/"
	require.NoError(t, os.Mkdir(task.bin, 0o700))
	s.unit("ssh", waitUntilOrder("sshd done")+"exit 0\n")
	s.staticUnit("tensorboard", staticScript(t, TensorBoardServiceResource))
	front, err := filepath.Abs("../../../harness/determined/exec/service_front.py")
	require.NoError(t, err)
	require.FileExists(t, front)
	s.env = slices.DeleteFunc(s.env, func(kv string) bool {
		return strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "DET_TENSORBOARD_") ||
			strings.HasPrefix(kv, "TB_STAND_IN_")
	})
	s.env = append(s.env,
		"SERVICE_FRONT="+front,
		"DET_TENSORBOARD_TOKEN="+tensorBoardTestToken,
		"PATH="+task.bin+":"+pathWithoutPrograms(t, filepath.Join(s.dir, "path"), "tensorboard"),
		"DET_TENSORBOARD_PORT="+task.port,
		"DET_TENSORBOARD_LOGDIR="+logdir,
		"DET_TENSORBOARD_ARGS=[]",
		"DET_TENSORBOARD_PATH_PREFIX="+task.prefix,
	)
	return task
}

// freePort returns a port of 127.0.0.1 that was free just now.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return strconv.Itoa(port)
}

// standInPython writes, at path, a python that has the stand-in tensorboard package: python3 with
// the package's directory on its PYTHONPATH and TB_STAND_IN_PYTHON set to path.
func (v *tensorBoardTask) standInPython(path string) {
	v.t.Helper()
	pkg := filepath.Join(v.dir, "tensorboard-package")
	for name, body := range tensorBoardStandIn {
		file := filepath.Join(pkg, name)
		require.NoError(v.t, os.MkdirAll(filepath.Dir(file), 0o700))
		require.NoError(v.t, os.WriteFile(file, []byte(body), 0o600))
	}
	python, err := exec.LookPath("python3")
	require.NoError(v.t, err)
	require.NoError(v.t, os.MkdirAll(filepath.Dir(path), 0o700))
	writeScript(v.t, path, `export TB_STAND_IN_PYTHON="$0" PYTHONPATH=`+strconv.Quote(pkg)+"\n"+
		`exec `+strconv.Quote(python)+` "$@"`+"\n")
}

// standInProgram puts on the PATH a tensorboard program whose first line is shebang.
func (v *tensorBoardTask) standInProgram(shebang string) {
	v.t.Helper()
	program := shebang + `
import sys

from tensorboard.main import run_main

sys.exit(run_main())
`
	//nolint:gosec // The program is executable.
	require.NoError(v.t, os.WriteFile(filepath.Join(v.bin, "tensorboard"), []byte(program), 0o700))
}

// tbPython is the python of the tensorboard program that standIn writes.
func (v *tensorBoardTask) tbPython() string {
	return filepath.Join(v.dir, "tensorboard-python", "python")
}

// standIn puts on the PATH a tensorboard program of a python that has the stand-in package, as a
// pip installation does.
func (v *tensorBoardTask) standIn() {
	v.standInPython(v.tbPython())
	v.standInProgram("#!" + v.tbPython())
}

// python returns the interpreter that ran the stand-in tensorboard.
func (v *tensorBoardTask) python() string {
	v.t.Helper()
	b, err := os.ReadFile(v.order + ".tensorboard-python")
	require.NoError(v.t, err)
	return string(b)
}

// args returns the arguments that the stand-in tensorboard got.
func (v *tensorBoardTask) args() []string {
	v.t.Helper()
	b, err := os.ReadFile(v.order + ".tensorboard-args")
	require.NoError(v.t, err)
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

// localPort returns TensorBoard's own port, its last --port.
func (v *tensorBoardTask) localPort() string {
	v.t.Helper()
	args := slices.Clone(v.args())
	slices.Reverse(args)
	i := slices.Index(args, "--port")
	require.Greater(v.t, i, 0, args)
	return args[i-1]
}

// get returns the status of a GET of the prefix on 127.0.0.1 at port, with the token header if
// token is set, or 0 if the port does not answer.
func (v *tensorBoardTask) get(port, token string) int {
	v.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://127.0.0.1:"+port+v.prefix, nil)
	require.NoError(v.t, err)
	if token != "" {
		req.Header.Set("X-Determined-Service-Token", token)
	}
	client := cleanhttp.DefaultClient()
	client.Timeout = 5 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	require.NoError(v.t, resp.Body.Close())
	return resp.StatusCode
}

// frontPid returns the pid of the front.
func (v *tensorBoardTask) frontPid() int {
	v.t.Helper()
	b, err := os.ReadFile(v.order + ".front-pid")
	require.NoError(v.t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	require.NoError(v.t, err)
	return pid
}

func (v *tensorBoardTask) addOrder(line string) {
	v.t.Helper()
	f, err := os.OpenFile(v.order, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(v.t, err)
	_, err = f.WriteString(line + "\n")
	require.NoError(v.t, err)
	require.NoError(v.t, f.Close())
}

// The unit runs TensorBoard, with the python of the tensorboard program, with the flags of the
// release, then the config's arguments, then the log directory, a relative one under the work
// directory, which it creates, a port of its own and the prefix, and --host 127.0.0.1, which holds
// whether --bind_all defaults to true or not, last, so that they win over the config's. The front on the service's
// port refuses a request without the token and passes on one with it. The service is READY once
// the front answers under the prefix; its readiness does not mark the task ready. TensorBoard's
// exit stops the front and leaves the shell running.
func TestTensorBoardUnitRuns(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), "abs", "logs")
	for _, tc := range []struct {
		name, version, logdir, args, bindAll string
		flags, extra                         []string
	}{
		{
			name: "relative", version: "2.9.0", logdir: "runs/exp", args: "[]",
			flags: []string{"--load_fast=false"},
		},
		{
			name: "bind_all false", version: "2.9.0", logdir: "runs", args: "[]", bindAll: "false",
			flags: []string{"--load_fast=false"},
		},
		{
			name: "absolute", version: "2.5.0", logdir: absolute,
			args:  `["--reload_interval=5", "two words", "--samples_per_plugin=scalars=500", ""]`,
			flags: []string{"--load_fast=false"},
			extra: []string{"--reload_interval=5", "two words", "--samples_per_plugin=scalars=500", ""},
		},
		{
			name: "overridden", version: "2.9.0", logdir: "runs",
			args:  `["--load_fast=true", "--hos=0.0.0.0", "--port", "1", "--path_p=/x"]`,
			flags: []string{"--load_fast=false"},
			extra: []string{"--load_fast=true", "--hos=0.0.0.0", "--port", "1", "--path_p=/x"},
		},
		{
			name: "before 2.5", version: "2.4.1", logdir: "runs", args: `["--x"]`,
			extra: []string{"--x"},
		},
		{
			name: "before 2", version: "1.15.0", logdir: "runs", args: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newTensorBoardTask(t, tc.logdir)
			v.standIn()
			v.env = append(v.env, "TB_STAND_IN_VERSION="+tc.version, "DET_TENSORBOARD_ARGS="+tc.args)
			if tc.bindAll != "" {
				v.env = append(v.env, "TB_STAND_IN_BIND_ALL="+tc.bindAll)
			}
			v.start()
			v.waitForLog("[tensorboard] ready")
			dir := tc.logdir
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(v.work, dir)
			}
			info, err := os.Stat(dir)
			require.NoError(t, err, "the log directory is created")
			require.True(t, info.IsDir())
			local := v.localPort()
			require.NotEqual(t, v.port, local)
			require.Equal(t, append(append(slices.Clone(tc.flags), tc.extra...),
				"--logdir", dir, "--port", local, "--path_prefix", strings.TrimSuffix(v.prefix, "/"),
				"--host", "127.0.0.1",
			), v.args())
			require.Equal(t, v.tbPython(), v.python())
			require.Equal(t, http.StatusUnauthorized, v.get(v.port, ""))
			require.Equal(t, http.StatusUnauthorized, v.get(v.port, "wrong"))
			require.Equal(t, http.StatusOK, v.get(v.port, tensorBoardTestToken))
			require.Contains(t, v.stderr.String(),
				"tensorboard service: TensorBoard "+tc.version+" reads "+dir+"\n")
			waitFor(t, "tensorboard READY", func() bool {
				return slices.Contains(statesOf(v.master.received(), "tensorboard"), "READY")
			})
			require.NotContains(t, v.orderLines(), "posted tensorboard", "not the primary service")

			v.addOrder("stop tensorboard")
			v.waitForLog("[tensorboard] exited 0")
			waitFor(t, "tensorboard EXITED", func() bool {
				states := statesOf(v.master.received(), "tensorboard")
				return states[len(states)-1] == "EXITED"
			})
			require.True(t, v.running(), "the shell stays up")
			require.Equal(t, 0, v.get(v.port, tensorBoardTestToken), "the front is stopped")
			v.addOrder("sshd done")
			require.Equal(t, 0, v.wait())
			require.Equal(t, []string{
				"[ssh] start", "[tensorboard] start", "[tensorboard] ready", "[tensorboard] exited 0",
				"[ssh] exited 0",
			}, v.logLines())
		})
	}
}

// TensorBoard's python is the first that has it of the tensorboard program's, named by its first
// line directly or through env, python3 and python.
func TestTensorBoardUnitPython(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(v *tensorBoardTask) string
	}{
		{
			name: "env",
			setup: func(v *tensorBoardTask) string {
				python := filepath.Join(v.bin, "python3.10")
				v.standInPython(python)
				v.standInProgram("#!/usr/bin/env -S python3.10 -u")
				return python
			},
		},
		{
			name: "program of no python",
			setup: func(v *tensorBoardTask) string {
				python := filepath.Join(v.bin, "python3")
				v.standInPython(python)
				v.standInProgram("#!/usr/bin/env bash")
				return python
			},
		},
		{
			name: "python3",
			setup: func(v *tensorBoardTask) string {
				python := filepath.Join(v.bin, "python3")
				v.standInPython(python)
				return python
			},
		},
		{
			name: "python",
			setup: func(v *tensorBoardTask) string {
				if err := exec.Command("python3", "-c", "import tensorboard.main").Run(); err == nil {
					t.Skip("python3 has tensorboard")
				}
				python := filepath.Join(v.bin, "python")
				v.standInPython(python)
				return python
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newTensorBoardTask(t, "runs")
			python := tc.setup(v)
			v.start()
			v.waitForLog("[tensorboard] ready")
			require.Equal(t, python, v.python())
			require.Equal(t, http.StatusOK, v.get(v.port, tensorBoardTestToken))
			v.addOrder("stop tensorboard")
			v.waitForLog("[tensorboard] exited 0")
			v.addOrder("sshd done")
			require.Equal(t, 0, v.wait())
		})
	}
}

// A TensorBoard that fails after it was READY is EXITED, and the shell stays up.
func TestTensorBoardUnitExitAfterReady(t *testing.T) {
	v := newTensorBoardTask(t, "runs")
	v.standIn()
	v.env = append(v.env, "TB_STAND_IN_EXIT=3")
	v.start()
	v.waitForLog("[tensorboard] ready")
	v.addOrder("stop tensorboard")
	v.waitForLog("[tensorboard] exited 3")
	waitFor(t, "tensorboard EXITED", func() bool {
		return slices.Equal([]string{"STARTING", "READY", "EXITED"},
			statesOf(v.master.received(), "tensorboard"))
	})
	require.True(t, v.running(), "the shell stays up")
	v.addOrder("sshd done")
	require.Equal(t, 0, v.wait())
}

// A front that exits makes the service EXITED with its status, and the unit stops TensorBoard; the
// shell stays up.
func TestTensorBoardUnitFrontExit(t *testing.T) {
	v := newTensorBoardTask(t, "runs")
	v.standIn()
	v.start()
	v.waitForLog("[tensorboard] ready")
	local := v.localPort()
	require.Equal(t, http.StatusOK, v.get(local, ""), "TensorBoard answers on its own port")
	require.NoError(t, syscall.Kill(v.frontPid(), syscall.SIGKILL))
	v.waitForLog("[tensorboard] exited 137")
	waitFor(t, "tensorboard EXITED", func() bool {
		return slices.Equal([]string{"STARTING", "READY", "EXITED"},
			statesOf(v.master.received(), "tensorboard"))
	})
	require.Equal(t, 0, v.get(local, ""), "TensorBoard is stopped")
	require.True(t, v.running(), "the shell stays up")
	v.addOrder("sshd done")
	require.Equal(t, 0, v.wait())
}

// A TensorBoard that never answers is never READY, and the front never starts; the probe goes with
// the service.
func TestTensorBoardUnitNotReadyWithoutHTTP(t *testing.T) {
	v := newTensorBoardTask(t, "runs")
	v.standIn()
	v.env = append(v.env, "TB_STAND_IN_SERVE=no")
	v.start()
	v.waitForOrder("tensorboard")
	v.addOrder("stop tensorboard")
	v.waitForLog("[tensorboard] exited 0")
	require.NotContains(t, statesOf(v.master.received(), "tensorboard"), "READY")
	v.addOrder("sshd done")
	require.Equal(t, 0, v.wait())
	require.NotContains(t, v.logLines(), "[tensorboard] ready")
	require.NoFileExists(t, v.order+".front-pid")
}

// A stop of the shell stops TensorBoard and its probe with it.
func TestTensorBoardUnitStop(t *testing.T) {
	v := newTensorBoardTask(t, "runs")
	v.standIn()
	v.start()
	v.waitForLog("[tensorboard] ready")
	v.signal(syscall.SIGTERM)
	require.Equal(t, 128+int(syscall.SIGTERM), v.wait())
	require.Contains(t, v.logLines(), "[tensorboard] exited 143")
}

// TensorBoard and the front take a stop's SIGINT, so the service exits with TensorBoard's status
// well within the grace, before the supervisor would kill it.
func TestTensorBoardUnitStopOnSIGINT(t *testing.T) {
	v := newTensorBoardTask(t, "runs")
	v.setting("stop_grace_seconds", 10)
	v.standIn()
	v.start()
	v.waitForLog("[tensorboard] ready")
	stopped := time.Now()
	v.signal(syscall.SIGINT)
	require.Equal(t, 128+int(syscall.SIGINT), v.wait())
	require.Less(t, time.Since(stopped), 5*time.Second)
	require.Contains(t, v.logLines(), "[tensorboard] exited 130")
	require.NotContains(t, v.stderr.String(), "KeyboardInterrupt")
}

// Without TensorBoard, or with what the unit needs missing, the unit says why in one line and
// exits 127: the service is UNAVAILABLE, and the shell stays up.
func TestTensorBoardUnitUnavailable(t *testing.T) {
	if err := exec.Command("python3", "-c", "import tensorboard.main").Run(); err == nil {
		t.Skip("python3 has tensorboard")
	}
	for _, tc := range []struct {
		name, line string
		setup      func(v *tensorBoardTask)
	}{
		{
			name: "not installed",
			line: "tensorboard service: tensorboard is not installed",
		},
		{
			name: "python without tensorboard",
			line: "tensorboard service: tensorboard is not installed",
			setup: func(v *tensorBoardTask) {
				py, err := exec.LookPath("python3")
				require.NoError(t, err)
				require.NoError(t, os.Symlink(py, filepath.Join(v.bin, "python3")))
			},
		},
		{
			name: "no release",
			line: `tensorboard service: cannot tell the release of the tensorboard of <python>: "unknown"`,
			setup: func(v *tensorBoardTask) {
				v.standIn()
				v.env = append(v.env, "TB_STAND_IN_VERSION=unknown")
			},
		},
		{
			name: "no port",
			line: `tensorboard service: DET_TENSORBOARD_PORT is not a port: ""`,
			setup: func(v *tensorBoardTask) {
				v.standIn()
				v.env = append(v.env, "DET_TENSORBOARD_PORT=")
			},
		},
		{
			name: "no prefix",
			line: `tensorboard service: DET_TENSORBOARD_PATH_PREFIX is not a path: "/proxy/x"`,
			setup: func(v *tensorBoardTask) {
				v.standIn()
				v.env = append(v.env, "DET_TENSORBOARD_PATH_PREFIX=/proxy/x")
			},
		},
		{
			name: "no token",
			line: "tensorboard service: DET_TENSORBOARD_TOKEN is not set",
			setup: func(v *tensorBoardTask) {
				v.standIn()
				v.env = append(v.env, "DET_TENSORBOARD_TOKEN=")
			},
		},
		{
			name: "args not a list",
			line: "tensorboard service: DET_TENSORBOARD_ARGS is not a JSON array of strings",
			setup: func(v *tensorBoardTask) {
				v.standIn()
				v.env = append(v.env, `DET_TENSORBOARD_ARGS=["--a", 1]`)
			},
		},
		{
			name: "log directory",
			line: "tensorboard service: cannot create the log directory /proc/no-such/runs: " +
				"No such file or directory",
			setup: func(v *tensorBoardTask) {
				v.standIn()
				v.env = append(v.env, "DET_TENSORBOARD_LOGDIR=/proc/no-such/runs")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newTensorBoardTask(t, "runs")
			path := pathWithoutPrograms(t, filepath.Join(v.dir, "nopython"),
				"tensorboard", "python3", "python")
			v.env = append(v.env, "PATH="+v.bin+":"+path)
			if tc.setup != nil {
				tc.setup(v)
			}
			v.start()
			v.waitForLog("[tensorboard] exited 127")
			line := strings.ReplaceAll(tc.line, "<python>", v.tbPython())
			require.Contains(t, v.stderr.String(), line+"\n[tensorboard] exited 127\n")
			require.Equal(t, 1, strings.Count(v.stderr.String(), "tensorboard service: "),
				v.stderr.String())
			waitFor(t, "tensorboard UNAVAILABLE", func() bool {
				return slices.Contains(statesOf(v.master.received(), "tensorboard"), "UNAVAILABLE")
			})
			require.True(t, v.running(), "the shell stays up")
			require.NotContains(t, v.orderLines(), "tensorboard")
			v.addOrder("sshd done")
			require.Equal(t, 0, v.wait())
		})
	}
}
