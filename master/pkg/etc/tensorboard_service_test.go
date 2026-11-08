//go:build linux

package etc

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/stretchr/testify/require"
)

// tensorBoardTestToken is the DET_TENSORBOARD_TOKEN of the tests.
const tensorBoardTestToken = "test-token"

// tensorBoardStandIn is a tensorboard package, by path under the package's directory, with what
// tensorboard_service.py takes of TensorBoard's. Its version is TB_STAND_IN_VERSION, 2.9.0 by
// default. Its core plugin's loader defines --bind_all with the default TB_STAND_IN_BIND_ALL, true
// by default as in some builds, and --host, --port, --path_prefix, --logdir, --load_fast,
// --reuse_port and --reload_multifile, and refuses --host while --bind_all is true, as TensorBoard's
// does. Its program.TensorBoard, without
// server_class when TB_STAND_IN_NO_SERVER_CLASS is "yes", configures its flags as TensorBoard's
// does: the command line, then the keyword arguments, then the loaders' checks. Its main records
// its arguments NUL-separated in "$ORDER_FILE.tensorboard-args", its flags as JSON in
// "$ORDER_FILE.tensorboard-flags", --reload_multifile only when it is set, its interpreter, TB_STAND_IN_PYTHON, in
// "$ORDER_FILE.tensorboard-python", and "tensorboard" in the order file. Then it serves, until the
// order file has "stop tensorboard", an application that answers 200 under the --path_prefix and
// 404 elsewhere and that creates "$ORDER_FILE.tensorboard-saw-token" when a request reaches it
// with the token's header. Its default server, a model of TensorBoard's werkzeug server, speaks
// HTTP/1.1 with keep-alive, sends an application's headers as they are, so that "Connection: close"
// closes the connection after the response, and leaves a body that the application does not read
// on the connection. It builds its server with its server_class, once the order file has
// the line TB_STAND_IN_WAIT if that is set, and serves the application through the server's, or
// directly when TB_STAND_IN_SERVE is "unchecked", or not at all when TB_STAND_IN_SERVE is "no".
// Like TensorBoard's, the server ends on SIGINT and main exits 0 on SIGTERM; otherwise main returns
// TB_STAND_IN_EXIT, 0 by default.
var tensorBoardStandIn = map[string]string{
	"tensorboard/__init__.py":              "",
	"tensorboard/plugins/__init__.py":      "",
	"tensorboard/plugins/core/__init__.py": "",
	"tensorboard/version.py": `
import os

VERSION = os.environ.get("TB_STAND_IN_VERSION", "2.9.0")
`,
	"tensorboard/plugins/base_plugin.py": `
class FlagsError(ValueError):
    pass
`,
	"tensorboard/plugins/core/core_plugin.py": `
import os

from tensorboard.plugins import base_plugin


class CorePluginLoader:
    def define_flags(self, parser):
        bind_all = os.environ.get("TB_STAND_IN_BIND_ALL", "true") == "true"
        parser.add_argument("--bind_all", default=bind_all, action="store_true")
        parser.add_argument("--host", default=None)
        parser.add_argument("--port", type=int, default=None)
        parser.add_argument("--path_prefix", default="")
        parser.add_argument("--logdir", default="")
        parser.add_argument("--load_fast", default="auto")
        parser.add_argument(
            "--reuse_port",
            type=lambda v: {"true": True, "false": False}.get(v.lower(), v),
            choices=[True, False],
            default=False,
        )
        parser.add_argument(
            "--reload_multifile",
            type=lambda v: {"true": True, "false": False}.get(v.lower(), v),
            choices=[True, False],
            default=None,
        )

    def fix_flags(self, flags):
        if flags.host is not None and flags.bind_all:
            raise base_plugin.FlagsError("Must not specify both --host and --bind_all.")
        flags.path_prefix = flags.path_prefix.rstrip("/")
`,
	"tensorboard/default.py": `
from tensorboard.plugins.core import core_plugin


def get_plugins():
    return [core_plugin.CorePluginLoader]
`,
	"tensorboard/program.py": `
import argparse
import http.server
import json
import os
import signal
import socketserver
import sys
import threading
import time


class _Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_GET(self):
        path, _, query = self.path.partition("?")
        environ = {
            "REQUEST_METHOD": self.command,
            "PATH_INFO": path,
            "QUERY_STRING": query,
            "SERVER_PROTOCOL": self.request_version,
            "wsgi.input": self.rfile,
        }
        for name, value in self.headers.items():
            if "_" in name:
                continue
            key = name.upper().replace("-", "_")
            if key not in ("CONTENT_TYPE", "CONTENT_LENGTH"):
                key = "HTTP_" + key
            environ[key] = environ[key] + "," + value if key in environ else value

        def start_response(status, headers, exc_info=None):
            code, _, reason = status.partition(" ")
            self.send_response(int(code), reason)
            for name, value in headers:
                self.send_header(name, value)
            self.end_headers()
            return self.wfile.write

        for data in self.server.app(environ, start_response):
            self.wfile.write(data)

    do_POST = do_GET


class _Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True

    def set_app(self, app):
        self.app = app

    def serve_forever(self, poll_interval=0.5):
        try:
            super().serve_forever(poll_interval)
        except KeyboardInterrupt:
            pass
        finally:
            self.server_close()


def create_port_scanning_werkzeug_server(wsgi_app, flags):
    host = "" if flags.bind_all else flags.host or "localhost"
    server = _Server((host, flags.port), _Handler)
    server.set_app(wsgi_app)
    return server


def _ordered(line):
    try:
        with open(os.environ["ORDER_FILE"]) as f:
            return line + "\n" in f.read().splitlines(True)
    except OSError:
        return False


def _stopped():
    return _ordered("stop tensorboard")


class TensorBoard:
    def __init__(self, plugins=None, assets_zip_provider=None, server_class=None, subcommands=None):
        if plugins is None:
            from tensorboard import default

            plugins = default.get_plugins()
        self.plugin_loaders = [p() if isinstance(p, type) else p for p in plugins]
        self.server_class = server_class or create_port_scanning_werkzeug_server
        self.flags = None

    def configure(self, argv=("",), **kwargs):
        parser = argparse.ArgumentParser(prog="tensorboard")
        for loader in self.plugin_loaders:
            loader.define_flags(parser)
        flags, _ = parser.parse_known_args(argv[1:])
        self.argv = list(argv)
        for k, v in kwargs.items():
            if not hasattr(flags, k):
                raise ValueError("Unknown TensorBoard flag: %s" % k)
            setattr(flags, k, v)
        for loader in self.plugin_loaders:
            loader.fix_flags(flags)
        self.flags = flags
        return [argv[0] if argv else ""]

    def main(self, ignored_argv=("",)):
        signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
        flags = self.flags
        order = os.environ["ORDER_FILE"]
        with open(order + ".tensorboard-args", "w") as f:
            f.write("".join(arg + "\0" for arg in self.argv[1:]))
        with open(order + ".tensorboard-flags", "w") as f:
            names = ["bind_all", "host", "port", "path_prefix", "logdir", "load_fast", "reuse_port"]
            if flags.reload_multifile is not None:
                names.append("reload_multifile")
            json.dump({name: getattr(flags, name) for name in names}, f)
        with open(order + ".tensorboard-python", "w") as f:
            f.write(os.environ["TB_STAND_IN_PYTHON"])
        with open(order, "a") as f:
            f.write("tensorboard\n")
        serve = os.environ.get("TB_STAND_IN_SERVE", "")
        if serve == "no":
            while not _stopped():
                time.sleep(0.05)
            return 0

        def app(environ, start_response):
            if "HTTP_X_DETERMINED_SERVICE_TOKEN" in environ:
                open(order + ".tensorboard-saw-token", "w").close()
            found = environ.get("PATH_INFO") == flags.path_prefix + "/"
            start_response("200 OK" if found else "404 Not Found", [("Content-Length", "0")])
            return [b""]

        wait = os.environ.get("TB_STAND_IN_WAIT")
        while wait and not _ordered(wait):
            time.sleep(0.05)
        server = self.server_class(app, flags)
        if serve == "unchecked":
            server.set_app(app)

        def watch():
            while not _stopped():
                time.sleep(0.05)
            server.shutdown()

        threading.Thread(target=watch, daemon=True).start()
        print("tensorboard stand-in serving", file=sys.stderr, flush=True)
        server.serve_forever()
        return int(os.environ.get("TB_STAND_IN_EXIT", "0"))


if os.environ.get("TB_STAND_IN_NO_SERVER_CLASS") == "yes":
    _init = TensorBoard.__init__

    def _init_without_server_class(self, plugins=None, assets_zip_provider=None, subcommands=None):
        _init(self, plugins, assets_zip_provider, None, subcommands)

    TensorBoard.__init__ = _init_without_server_class
`,
}

// tensorBoardTask is a task with ssh, a stand-in that waits for "sshd done" in the order file, as
// its primary service, and the tensorboard service with the real unit and program, a directory on
// the PATH for stand-ins, a free port, the prefix /proxy/<task>:<port>/ and a token.
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
	require.NoError(t, os.WriteFile(filepath.Join(s.run, "services", "tensorboard_service.py"),
		MustStaticFile(TensorBoardServiceProgramResource), 0o600))
	s.env = slices.DeleteFunc(s.env, func(kv string) bool {
		return strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "DET_TENSORBOARD_") ||
			strings.HasPrefix(kv, "TB_STAND_IN_")
	})
	s.env = append(s.env,
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

// writeTensorBoardStandIn writes the stand-in tensorboard package under dir.
func writeTensorBoardStandIn(t *testing.T, dir string) {
	t.Helper()
	for name, body := range tensorBoardStandIn {
		file := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
		require.NoError(t, os.WriteFile(file, []byte(body), 0o600))
	}
}

// standInPython writes, at path, a python that has the stand-in tensorboard package: python3 without
// its site packages, with the package's directory on its PYTHONPATH and TB_STAND_IN_PYTHON set to
// path.
func (v *tensorBoardTask) standInPython(path string) {
	v.t.Helper()
	pkg := filepath.Join(v.dir, "tensorboard-package")
	writeTensorBoardStandIn(v.t, pkg)
	python, err := exec.LookPath("python3")
	require.NoError(v.t, err)
	require.NoError(v.t, os.MkdirAll(filepath.Dir(path), 0o700))
	writeScript(v.t, path, `export TB_STAND_IN_PYTHON="$0" PYTHONPATH=`+strconv.Quote(pkg)+"\n"+
		`if [[ ${1-} == */tensorboard_service.py ]]; then `+recordFDs("tensorboard")+"fi\n"+
		`exec `+strconv.Quote(python)+` -S "$@"`+"\n")
}

// standInProgram puts on the PATH a tensorboard program whose first line is shebang.
func (v *tensorBoardTask) standInProgram(shebang string) {
	v.t.Helper()
	program := shebang + "\nimport tensorboard.program\n"
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
	if len(b) == 0 {
		return []string{}
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

// flags returns the flags that the stand-in tensorboard served with.
func (v *tensorBoardTask) flags() map[string]any {
	v.t.Helper()
	b, err := os.ReadFile(v.order + ".tensorboard-flags")
	require.NoError(v.t, err)
	var flags map[string]any
	require.NoError(v.t, json.Unmarshal(b, &flags))
	return flags
}

// serviceFlags are the flags that the service sets.
func (v *tensorBoardTask) serviceFlags(logdir string) map[string]any {
	port, err := strconv.Atoi(v.port)
	require.NoError(v.t, err)
	return map[string]any{
		"logdir": logdir, "port": float64(port), "path_prefix": strings.TrimSuffix(v.prefix, "/"),
		"host": "0.0.0.0", "bind_all": false, "load_fast": "false", "reuse_port": false,
	}
}

// get returns the status of a GET of the prefix on 127.0.0.1 at the service's port, with the
// token header if token is set, or 0 if the port does not answer.
func (v *tensorBoardTask) get(token string) int {
	v.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://127.0.0.1:"+v.port+v.prefix, nil)
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

func (v *tensorBoardTask) addOrder(line string) {
	v.t.Helper()
	f, err := os.OpenFile(v.order, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(v.t, err)
	_, err = f.WriteString(line + "\n")
	require.NoError(v.t, err)
	require.NoError(v.t, f.Close())
}

// pythonHasTensorBoard tells whether python3 has a tensorboard package of its own.
func pythonHasTensorBoard() bool {
	return exec.Command("python3", "-c", "import tensorboard.program").Run() == nil
}

// The program's own tests, testdata/tensorboard_service_test.py, with python3 without its site
// packages and with the stand-in tensorboard package. Python writes no bytecode next to the
// program, which the master's static files ship.
func TestTensorBoardServiceProgram(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	pkg := t.TempDir()
	writeTensorBoardStandIn(t, pkg)
	services, err := filepath.Abs("../../static/srv/services")
	require.NoError(t, err)
	test, err := filepath.Abs("testdata/tensorboard_service_test.py")
	require.NoError(t, err)
	cmd := exec.Command(python, "-S", test) //nolint:gosec
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "PYTHONPATH=") || strings.HasPrefix(kv, "DET_TENSORBOARD_") ||
			strings.HasPrefix(kv, "TB_STAND_IN_")
	}), "PYTHONPATH="+pkg+":"+services, "TENSORBOARD_STAND_IN="+pkg, "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	t.Log(string(out))
}

// The unit runs the program with the python of the tensorboard program, on the log directory, a
// relative one under the work directory, which it creates. TensorBoard gets the config's
// arguments and then the service's flags, which win over them, whether --bind_all defaults to
// true or not: the log directory, the service's port on every IPv4 address, the prefix and
// --load_fast=false and --reuse_port=false. TensorBoard refuses a request without the token or with a wrong one, and
// answers one with it, without the header. The service is READY once TensorBoard refuses the one
// and answers the other under the prefix; its readiness does not mark the task ready.
// TensorBoard's exit leaves the shell running.
func TestTensorBoardUnitRuns(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), "abs", "logs")
	for _, tc := range []struct {
		name, version, logdir, args, bindAll string
		extra                                []string
	}{
		{name: "relative", version: "2.9.0", logdir: "runs/exp", args: "[]", extra: []string{}},
		{
			name: "bind_all false", version: "2.9.0", logdir: "runs", args: "", bindAll: "false",
			extra: []string{},
		},
		{
			name: "absolute", version: "2.18.0", logdir: absolute,
			args:  `["--reload_interval=5", "two words", "--samples_per_plugin=scalars=500", ""]`,
			extra: []string{"--reload_interval=5", "two words", "--samples_per_plugin=scalars=500", ""},
		},
		{
			name: "overridden", version: "2.9.0", logdir: "runs",
			args: `["--load_fast=true", "--hos=127.0.0.2", "--port", "1", "--path_p=/x",
				"--reuse_port=true"]`,
			extra: []string{
				"--load_fast=true", "--hos=127.0.0.2", "--port", "1", "--path_p=/x", "--reuse_port=true",
			},
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
			require.Equal(t, tc.extra, v.args())
			require.Equal(t, v.serviceFlags(dir), v.flags())
			require.Equal(t, v.tbPython(), v.python())
			// The program gets the readiness check's pipe on 3, and no other end of it.
			ready, err := os.ReadFile(filepath.Join(v.dir, "ready-args.tensorboard-input"))
			require.NoError(t, err)
			fds := requireProgramFDs(t, v.order, "tensorboard", strings.TrimSpace(string(ready)))
			require.Equal(t, strings.TrimSpace(string(ready)), fds["3"])
			require.Equal(t, http.StatusUnauthorized, v.get(""))
			require.Equal(t, http.StatusUnauthorized, v.get("wrong"))
			require.NoFileExists(t, v.order+".tensorboard-saw-token")
			require.Equal(t, http.StatusOK, v.get(tensorBoardTestToken))
			require.NoFileExists(t, v.order+".tensorboard-saw-token", "TensorBoard gets no token")
			require.Contains(t, v.stderr.String(),
				"tensorboard service: TensorBoard "+tc.version+" reads "+dir+"\n")
			require.NotContains(t, v.stderr.String(), tensorBoardTestToken)
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
			require.Equal(t, 0, v.get(tensorBoardTestToken), "TensorBoard is stopped")
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
				if pythonHasTensorBoard() {
					v.t.Skip("python3 has tensorboard")
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
			require.Equal(t, http.StatusOK, v.get(tensorBoardTestToken))
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

// A TensorBoard that never answers is never READY; the probe goes with the service.
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
}

// A TensorBoard whose server answers without the token is never READY, and the probe says so
// once.
func TestTensorBoardUnitNotReadyWithoutTokenCheck(t *testing.T) {
	v := newTensorBoardTask(t, "runs")
	v.standIn()
	v.env = append(v.env, "TB_STAND_IN_SERVE=unchecked")
	v.start()
	v.waitForLog("tensorboard service: TensorBoard answers without the token")
	require.Equal(t, http.StatusOK, v.get(""))
	v.addOrder("stop tensorboard")
	v.waitForLog("[tensorboard] exited 0")
	require.NotContains(t, statesOf(v.master.received(), "tensorboard"), "READY")
	require.Equal(t, 1, strings.Count(v.stderr.String(), "answers without the token"))
	v.addOrder("sshd done")
	require.Equal(t, 0, v.wait())
	require.NotContains(t, v.logLines(), "[tensorboard] ready")
}

// The probe sends the token only to TensorBoard's own server: a listener that holds the port while
// TensorBoard starts gets no request, and the service is READY once TensorBoard has the port.
func TestTensorBoardUnitProbesOnlyTensorBoard(t *testing.T) {
	v := newTensorBoardTask(t, "runs")
	v.standIn()
	v.env = append(v.env, "TB_STAND_IN_WAIT=serve tensorboard")
	l, err := net.Listen("tcp", "127.0.0.1:"+v.port)
	require.NoError(t, err)
	var requests, tokens atomic.Int32
	other := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if r.Header.Values("X-Determined-Service-Token") != nil {
				tokens.Add(1)
			}
			w.WriteHeader(http.StatusUnauthorized)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	served := make(chan error, 1)
	go func() { served <- other.Serve(l) }()
	v.start()
	v.waitForOrder("tensorboard")
	// Longer than a probe that asked while TensorBoard starts would take.
	time.Sleep(2 * time.Second)
	require.NoError(t, other.Close())
	require.ErrorIs(t, <-served, http.ErrServerClosed)
	require.Zero(t, requests.Load(), "the listener gets no request")
	v.addOrder("serve tensorboard")
	v.waitForLog("[tensorboard] ready")
	require.Equal(t, http.StatusOK, v.get(tensorBoardTestToken))
	require.Zero(t, requests.Load())
	require.Zero(t, tokens.Load(), "the listener never gets the token")
	require.NotContains(t, v.stderr.String(), tensorBoardTestToken)
	v.addOrder("stop tensorboard")
	v.waitForLog("[tensorboard] exited 0")
	v.addOrder("sshd done")
	require.Equal(t, 0, v.wait())
}

// A stop of the shell stops TensorBoard, which exits 0 on SIGTERM, and its probe with it.
func TestTensorBoardUnitStop(t *testing.T) {
	v := newTensorBoardTask(t, "runs")
	v.standIn()
	v.start()
	v.waitForLog("[tensorboard] ready")
	v.signal(syscall.SIGTERM)
	require.Equal(t, 128+int(syscall.SIGTERM), v.wait())
	require.Contains(t, v.logLines(), "[tensorboard] exited 0")
}

// TensorBoard takes a stop's SIGINT, so the service exits with TensorBoard's status well within
// the grace, before the supervisor would kill it.
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
	require.Contains(t, v.logLines(), "[tensorboard] exited 0")
	require.NotContains(t, v.stderr.String(), "KeyboardInterrupt")
}

// Without TensorBoard, or with what the service needs missing, the unit or the program says why in
// one line and exits 127: the service is UNAVAILABLE, and the shell stays up.
func TestTensorBoardUnitUnavailable(t *testing.T) {
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
				if pythonHasTensorBoard() {
					v.t.Skip("python3 has tensorboard")
				}
				py, err := exec.LookPath("python3")
				require.NoError(v.t, err)
				require.NoError(v.t, os.Symlink(py, filepath.Join(v.bin, "python3")))
			},
		},
		{
			name: "before 2.9",
			line: "tensorboard service: TensorBoard 2.8.0 is older than 2.9",
			setup: func(v *tensorBoardTask) {
				v.standIn()
				v.env = append(v.env, "TB_STAND_IN_VERSION=2.8.0")
			},
		},
		{
			name: "before 2",
			line: "tensorboard service: TensorBoard 1.15.0 is older than 2.9",
			setup: func(v *tensorBoardTask) {
				v.standIn()
				v.env = append(v.env, "TB_STAND_IN_VERSION=1.15.0")
			},
		},
		{
			name: "no release",
			line: `tensorboard service: cannot tell the release of TensorBoard: "unknown"`,
			setup: func(v *tensorBoardTask) {
				v.standIn()
				v.env = append(v.env, "TB_STAND_IN_VERSION=unknown")
			},
		},
		{
			name: "no server_class",
			line: "tensorboard service: TensorBoard 2.9.0 takes no server_class",
			setup: func(v *tensorBoardTask) {
				v.standIn()
				v.env = append(v.env, "TB_STAND_IN_NO_SERVER_CLASS=yes")
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
			require.Contains(t, v.stderr.String(), tc.line+"\n[tensorboard] exited 127\n")
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

// tensorBoardLoaderStandIn stands in for the loader at <run>/tensorboard/tensorboard_loader.py,
// which loaderPython runs with bash. With --check it records "check <logdir> <python>" in
// "$ORDER_FILE.loader" and "loader check" in the order file, prints LOADER_CHECK_OUTPUT and exits
// with LOADER_CHECK_EXIT, 0 by default. Otherwise it records its descriptors, its pid in
// "$ORDER_FILE.loader-pid" and "loader <logdir>" in the order file, logs a line and exits with
// LOADER_EXIT if that is set, or else waits until a SIGTERM, which it records as "loader stopped".
const tensorBoardLoaderStandIn = `
if [[ $1 == --check ]]; then
    printf 'check %s %s\n' "$2" "$DET_PYTHON_EXECUTABLE" >>"$ORDER_FILE.loader"
    echo "loader check" >>"$ORDER_FILE"
    printf '%b' "${LOADER_CHECK_OUTPUT-}"
    exit "${LOADER_CHECK_EXIT:-0}"
fi
RECORD_FDS
echo "$$" >"$ORDER_FILE.loader-pid"
echo "loader $1" >>"$ORDER_FILE"
echo "tensorboard loader: loading 1 sources from s3 storage" >&2
[[ -z ${LOADER_EXIT-} ]] || exit "$LOADER_EXIT"
trap 'echo "loader stopped" >>"$ORDER_FILE"; kill "$!"; wait "$!"; exit 0' TERM
sleep 300 &
wait
`

// newTensorBoardLoaderTask is a tensorboard task with sources, whose DET_PYTHON_EXECUTABLE runs
// the stand-in loader and passes everything else to the supervisor's python stand-in.
func newTensorBoardLoaderTask(t *testing.T) *tensorBoardTask {
	t.Helper()
	v := newTensorBoardTask(t, "runs")
	v.standIn()
	loaderDir := filepath.Join(v.run, "tensorboard")
	require.NoError(t, os.MkdirAll(loaderDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(loaderDir, "tensorboard_loader.py"),
		[]byte(strings.Replace(tensorBoardLoaderStandIn, "RECORD_FDS\n", recordFDs("loader"), 1)), 0o600))
	python := filepath.Join(v.dir, "loader-python")
	writeScript(t, python, `if [[ ${1-} == "$RUN_DIR"/tensorboard/tensorboard_loader.py ]]; then
    exec bash "$@"
fi
exec `+strconv.Quote(filepath.Join(v.dir, "python"))+` "$@"
`)
	v.env = append(v.env, "DET_PYTHON_EXECUTABLE="+python,
		"DET_TENSORBOARD_SOURCES="+filepath.Join(loaderDir, "sources.json"))
	return v
}

// loaderGone tells whether the stand-in loader has exited: its process is gone or a zombie.
func (v *tensorBoardTask) loaderGone() bool {
	v.t.Helper()
	pid, err := os.ReadFile(v.order + ".loader-pid")
	require.NoError(v.t, err)
	stat, err := os.ReadFile(filepath.Join("/proc", strings.TrimSpace(string(pid)), "stat"))
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

// With sources, the unit runs the loader's check with the task's python on the absolute log
// directory before TensorBoard, then the loader in the background, without the readiness pipe or
// the service's own descriptors, with its lines in the task log. Loading does not gate readiness:
// the service is READY once TensorBoard answers, with --reload_multifile=true, and stays READY
// when the loader exits. When TensorBoard exits, or the task stops, the loader gets SIGTERM and is
// gone by the time the service's exit is logged.
func TestTensorBoardUnitLoader(t *testing.T) {
	t.Run("runs", func(t *testing.T) {
		v := newTensorBoardLoaderTask(t)
		v.start()
		v.waitForLog("[tensorboard] ready")
		v.waitForOrder("loader " + filepath.Join(v.work, "runs"))
		logdir := filepath.Join(v.work, "runs")
		check, err := os.ReadFile(v.order + ".loader")
		require.NoError(t, err)
		require.Equal(t, "check "+logdir+" "+filepath.Join(v.dir, "loader-python")+"\n",
			string(check))
		lines := v.orderLines()
		require.Less(t, slices.Index(lines, "loader check"), slices.Index(lines, "tensorboard"))
		require.Less(t, slices.Index(lines, "loader check"), slices.Index(lines, "loader "+logdir))
		requireProgramFDs(t, v.order, "loader")
		require.Contains(t, v.stderr.String(),
			"tensorboard loader: loading 1 sources from s3 storage\n")
		flags := v.serviceFlags(logdir)
		flags["reload_multifile"] = true
		require.Equal(t, flags, v.flags())
		require.Equal(t, http.StatusOK, v.get(tensorBoardTestToken))
		waitFor(t, "tensorboard READY", func() bool {
			return slices.Contains(statesOf(v.master.received(), "tensorboard"), "READY")
		})

		// TensorBoard exits by itself: the loader gets SIGTERM within the grace.
		v.addOrder("stop tensorboard")
		v.waitForLog("[tensorboard] exited 0")
		require.Contains(t, v.orderLines(), "loader stopped")
		require.True(t, v.loaderGone())
		v.addOrder("sshd done")
		require.Equal(t, 0, v.wait())
	})

	t.Run("task stop", func(t *testing.T) {
		v := newTensorBoardLoaderTask(t)
		v.start()
		v.waitForLog("[tensorboard] ready")
		v.waitForOrder("loader " + filepath.Join(v.work, "runs"))
		v.signal(syscall.SIGTERM)
		require.Equal(t, 128+int(syscall.SIGTERM), v.wait())
		require.Contains(t, v.logLines(), "[tensorboard] exited 0")
		require.Contains(t, v.orderLines(), "loader stopped")
		require.True(t, v.loaderGone())
	})

	t.Run("loader exits", func(t *testing.T) {
		v := newTensorBoardLoaderTask(t)
		v.env = append(v.env, "LOADER_EXIT=1")
		v.start()
		v.waitForLog("[tensorboard] ready")
		v.waitForOrder("loader " + filepath.Join(v.work, "runs"))
		waitFor(t, "the loader's exit", v.loaderGone)
		time.Sleep(500 * time.Millisecond)
		require.Equal(t, http.StatusOK, v.get(tensorBoardTestToken))
		require.Equal(t, []string{"STARTING", "READY"}, statesOf(v.master.received(), "tensorboard"))
		v.addOrder("stop tensorboard")
		v.waitForLog("[tensorboard] exited 0")
		v.addOrder("sshd done")
		require.Equal(t, 0, v.wait())
	})

	for _, tc := range []struct{ name, output, line string }{
		{
			name: "check fails", output: `first\nthe reason\n`,
			line: "tensorboard service: the reason",
		},
		{
			name: "check fails without a message",
			line: "tensorboard service: the loader's check failed without a message",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newTensorBoardLoaderTask(t)
			v.env = append(v.env, "LOADER_CHECK_EXIT=2", "LOADER_CHECK_OUTPUT="+tc.output)
			v.start()
			v.waitForLog("[tensorboard] exited 127")
			require.Contains(t, v.stderr.String(), tc.line+"\n[tensorboard] exited 127\n")
			require.Equal(t, 1, strings.Count(v.stderr.String(), "tensorboard service: "))
			waitFor(t, "tensorboard UNAVAILABLE", func() bool {
				return slices.Contains(statesOf(v.master.received(), "tensorboard"), "UNAVAILABLE")
			})
			require.Equal(t, []string{"loader check"}, slices.DeleteFunc(v.orderLines(),
				func(line string) bool { return line == "setup" || line == "hook" || line == "prep" }))
			v.addOrder("sshd done")
			require.Equal(t, 0, v.wait())
		})
	}

	t.Run("without sources", func(t *testing.T) {
		v := newTensorBoardLoaderTask(t)
		v.env = append(v.env, "DET_TENSORBOARD_SOURCES=")
		v.start()
		v.waitForLog("[tensorboard] ready")
		require.Equal(t, v.serviceFlags(filepath.Join(v.work, "runs")), v.flags())
		v.addOrder("stop tensorboard")
		v.waitForLog("[tensorboard] exited 0")
		v.addOrder("sshd done")
		require.Equal(t, 0, v.wait())
		require.NoFileExists(t, v.order+".loader")
		for _, line := range v.orderLines() {
			require.False(t, strings.HasPrefix(line, "loader"), line)
		}
	})
}
