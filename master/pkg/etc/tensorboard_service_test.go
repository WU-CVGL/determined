//go:build linux

package etc

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// tensorBoardStandIn is a tensorboard program: it prints TB_STAND_IN_VERSION, 2.9.0 by default, for
// --version_tb; otherwise it records its arguments NUL-separated and "tensorboard" in the order
// file, and serves HTTP on 127.0.0.1 at its --port, with 200 under its --path_prefix and 404
// elsewhere, until the order file has "stop tensorboard". Then it exits with
// TB_STAND_IN_EXIT, 0 by default.
const tensorBoardStandIn = `
for arg; do
    if [[ $arg == --version_tb ]]; then
        echo "${TB_STAND_IN_VERSION:-2.9.0}"
        exit 0
    fi
done
printf '%s\0' "$@" >"$ORDER_FILE.tensorboard-args"
echo tensorboard >>"$ORDER_FILE"
exec "$REPORTER_PYTHON" -c '
import http.server
import os
import sys
import threading
import time

argv = sys.argv[1:]
port = int(argv[argv.index("--port") + 1])
prefix = argv[argv.index("--path_prefix") + 1]


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200 if self.path == prefix + "/" else 404)
        self.end_headers()

    def log_message(self, *args):
        pass


server = http.server.HTTPServer(("127.0.0.1", port), Handler)


def watch():
    while True:
        try:
            with open(os.environ["ORDER_FILE"]) as f:
                if "stop tensorboard\n" in f.read():
                    break
        except OSError:
            pass
        time.sleep(0.05)
    server.shutdown()


threading.Thread(target=watch, daemon=True).start()
print("tensorboard stand-in serving", file=sys.stderr, flush=True)
server.serve_forever()
sys.exit(int(os.environ.get("TB_STAND_IN_EXIT", "0")))
' "$@"
`

// tensorBoardTask is a task with ssh, a stand-in that waits for "sshd done" in the order file, as
// its primary service, and the tensorboard service with the real unit, a directory on the PATH for
// stand-ins, a free port and the prefix /proxy/<task>:<port>/.
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
	s.env = slices.DeleteFunc(s.env, func(kv string) bool {
		return strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "DET_TENSORBOARD_") ||
			strings.HasPrefix(kv, "TB_STAND_IN_")
	})
	s.env = append(s.env,
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

// standIn puts the stand-in tensorboard on the PATH.
func (v *tensorBoardTask) standIn() {
	writeScript(v.t, filepath.Join(v.bin, "tensorboard"), tensorBoardStandIn)
}

// args returns the arguments that the stand-in tensorboard got.
func (v *tensorBoardTask) args() []string {
	v.t.Helper()
	b, err := os.ReadFile(v.order + ".tensorboard-args")
	require.NoError(v.t, err)
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

func (v *tensorBoardTask) addOrder(line string) {
	v.t.Helper()
	f, err := os.OpenFile(v.order, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(v.t, err)
	_, err = f.WriteString(line + "\n")
	require.NoError(v.t, err)
	require.NoError(v.t, f.Close())
}

// The unit runs TensorBoard on the log directory, a relative one under the work directory, which it
// creates, with the port, the prefix, the flags of the release and the config's arguments, in
// order, last. The service is READY once TensorBoard answers under the prefix; its readiness does
// not mark the task ready. TensorBoard's exit leaves the shell running.
func TestTensorBoardUnitRuns(t *testing.T) {
	absolute := filepath.Join(t.TempDir(), "abs", "logs")
	for _, tc := range []struct {
		name, version, logdir, args string
		flags, extra                []string
	}{
		{
			name: "relative", version: "2.9.0", logdir: "runs/exp", args: "[]",
			flags: []string{"--bind_all", "--load_fast=false"},
		},
		{
			name: "absolute", version: "2.5.0", logdir: absolute,
			args:  `["--reload_interval=5", "two words", "--samples_per_plugin=scalars=500", ""]`,
			flags: []string{"--bind_all", "--load_fast=false"},
			extra: []string{"--reload_interval=5", "two words", "--samples_per_plugin=scalars=500", ""},
		},
		{
			name: "before 2.5", version: "2.4.1", logdir: "runs", args: `["--x"]`,
			flags: []string{"--bind_all"}, extra: []string{"--x"},
		},
		{
			name: "before 2", version: "1.15.0", logdir: "runs", args: "",
			flags: []string{"--host", "0.0.0.0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newTensorBoardTask(t, tc.logdir)
			v.standIn()
			v.env = append(v.env, "TB_STAND_IN_VERSION="+tc.version, "DET_TENSORBOARD_ARGS="+tc.args)
			v.start()
			v.waitForLog("[tensorboard] ready")
			dir := tc.logdir
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(v.work, dir)
			}
			info, err := os.Stat(dir)
			require.NoError(t, err, "the log directory is created")
			require.True(t, info.IsDir())
			require.Equal(t, append(append([]string{
				"--logdir", dir, "--port", v.port, "--path_prefix", strings.TrimSuffix(v.prefix, "/"),
			}, tc.flags...), tc.extra...), v.args())
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
			v.addOrder("sshd done")
			require.Equal(t, 0, v.wait())
			require.Equal(t, []string{
				"[ssh] start", "[tensorboard] start", "[tensorboard] ready", "[tensorboard] exited 0",
				"[ssh] exited 0",
			}, v.logLines())
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

// A TensorBoard that never answers is never READY; its probe goes with the service.
func TestTensorBoardUnitNotReadyWithoutHTTP(t *testing.T) {
	v := newTensorBoardTask(t, "runs")
	writeScript(t, filepath.Join(v.bin, "tensorboard"), `
[[ $1 != --version_tb ]] || { echo 2.9.0; exit 0; }
echo tensorboard >>"$ORDER_FILE"
`+waitUntilOrder("stop tensorboard")+"exit 0\n")
	v.start()
	v.waitForOrder("tensorboard")
	v.addOrder("stop tensorboard")
	v.waitForLog("[tensorboard] exited 0")
	require.NotContains(t, statesOf(v.master.received(), "tensorboard"), "READY")
	v.addOrder("sshd done")
	require.Equal(t, 0, v.wait())
	require.NotContains(t, v.logLines(), "[tensorboard] ready")
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
			line: `tensorboard service: cannot tell the release of tensorboard: "unknown"`,
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
