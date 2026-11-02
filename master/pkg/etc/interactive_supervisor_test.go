//go:build linux

package etc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// pythonStandIn stands in for DET_PYTHON_EXECUTABLE: prep_container records itself and exits with
// PREP_EXIT; check_ready_logs.py reads its input like the real one and, on the first match, records
// "posted <service>" unless --no-post, and logs "[<service>] ready"; check_idle.py records its pid.
const pythonStandIn = `
case $1 in
-m)
    [[ "$*" == "-m determined.exec.prep_container --resources --proxy --download_context_directory" ]] || exit 2
    echo prep >>"$ORDER_FILE"
    exit "${PREP_EXIT:-0}"
    ;;
"$RUN_DIR"/check_ready_logs.py)
    shift
    regex= service= post=1
    while (($#)); do
        case $1 in
        --ready-regex) regex=$2; shift 2 ;;
        --service) service=$2; shift 2 ;;
        --no-post) post=; shift ;;
        *) exit 2 ;;
        esac
    done
    printf '%s post=%s\n' "$service" "$post" >>"$READY_ARGS_FILE"
    while IFS= read -r line; do
        if [[ $line =~ ^($regex) ]]; then
            if [[ -n $post ]]; then
                echo "posted $service" >>"$ORDER_FILE"
            fi
            if [[ -n $service ]]; then
                printf '[%s] ready\n' "$service" >&2
            fi
            exit 0
        fi
    done
    ;;
"$RUN_DIR"/jupyter/check_idle.py)
    echo "$$" >"$IDLE_PID_FILE"
    exec sleep 300
    ;;
*)
    exit 2
    ;;
esac
`

// syncBuffer collects a process's output while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// supervisorRun runs interactive-supervisor.sh as this process's child, like ship_logs.py runs it,
// with /run/determined in a temporary directory, stand-ins for task-setup.sh and python, the real
// init and a startup hook in the work dir. The test process becomes a child subreaper that, like
// ship_logs.py, never reaps an orphan, so that anything the supervisor leaves behind stays visible
// as its child.
type supervisorRun struct {
	t      *testing.T
	dir    string
	run    string
	work   string
	order  string
	env    []string
	cmd    *exec.Cmd
	stderr syncBuffer
	done   chan struct{}
	err    error
	// killedGroups is set when a service is killed with its process group: its members die at
	// once, so that one may die after its parent and stay a zombie until the container ends.
	killedGroups bool
}

func newSupervisorRun(t *testing.T, services string) *supervisorRun {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	require.NoError(t, unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0))
	t.Cleanup(func() {
		reapOrphans()
		require.NoError(t, unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 0, 0, 0, 0))
	})

	dir := t.TempDir()
	s := &supervisorRun{
		t:     t,
		dir:   dir,
		run:   filepath.Join(dir, "run"),
		work:  filepath.Join(dir, "work"),
		order: filepath.Join(dir, "order"),
		done:  make(chan struct{}),
	}
	for _, d := range []string{s.work, filepath.Join(s.run, "services"), filepath.Join(s.run, "ssh")} {
		require.NoError(t, os.MkdirAll(d, 0o700))
	}

	supervisor := strings.ReplaceAll(staticScript(t, InteractiveSupervisorResource),
		"/run/determined/", s.run+"/")
	require.Contains(t, supervisor, "\nsupervisor_stop_grace_seconds=10\n")
	supervisor = strings.Replace(supervisor,
		"\nsupervisor_stop_grace_seconds=10\n", "\nsupervisor_stop_grace_seconds=1\n", 1)
	writeScript(t, filepath.Join(s.run, InteractiveSupervisorResource), supervisor)
	require.NoError(t, os.WriteFile(filepath.Join(s.run, InteractiveInitScriptResource),
		[]byte(staticScript(t, InteractiveInitScriptResource)), 0o600))
	writeScript(t, filepath.Join(s.run, TaskSetupScriptResource), `echo setup >>"$ORDER_FILE"`+"\n")
	writeScript(t, filepath.Join(dir, "python"), pythonStandIn)
	// Exported and shell-only variables of the hook both reach the services; common names do not
	// change the supervisor's own.
	writeScript(t, filepath.Join(s.work, "startup-hook.sh"),
		"echo hook >>\"$ORDER_FILE\"\nexport HOOK_VAR=hook\nHOOK_LOCAL=local\nservices=x primary=x name=x\n")

	s.env = append(os.Environ(),
		"DET_SERVICES="+services,
		"DET_PYTHON_EXECUTABLE="+filepath.Join(dir, "python"),
		"RUN_DIR="+s.run,
		"ORDER_FILE="+s.order,
		"READY_ARGS_FILE="+filepath.Join(dir, "ready-args"),
		"IDLE_PID_FILE="+filepath.Join(dir, "idle-pid"),
		"PS4=+ ",
		"TCD_STARTUP_HOOK=",
		"HOOK_VAR=",
		"HOOK_LOCAL=",
	)
	return s
}

// unit installs a service unit, with /run/determined in the test's directory.
func (s *supervisorRun) unit(name, body string) {
	s.t.Helper()
	body = strings.ReplaceAll(body, "/run/determined/", s.run+"/")
	require.NoError(s.t, os.WriteFile(filepath.Join(s.run, "services", name+".sh"),
		[]byte("#!/usr/bin/env bash\n"+body), 0o600))
}

func (s *supervisorRun) start(args ...string) {
	s.t.Helper()
	s.cmd = exec.Command( //nolint:gosec
		"bash", append([]string{filepath.Join(s.run, InteractiveSupervisorResource)}, args...)...)
	s.cmd.Dir = s.work
	s.cmd.Env = s.env
	s.cmd.Stdout = &s.stderr
	s.cmd.Stderr = &s.stderr
	s.cmd.WaitDelay = 5 * time.Second
	require.NoError(s.t, s.cmd.Start())
	go func() {
		s.err = s.cmd.Wait()
		close(s.done)
	}()
	s.t.Cleanup(func() {
		select {
		case <-s.done:
		default:
			_ = s.cmd.Process.Kill()
			<-s.done
		}
	})
}

// wait waits for the supervisor to exit and returns its exit status, which is 128+n when a signal
// killed it. Nothing the supervisor started may be left: no process, no zombie.
func (s *supervisorRun) wait() int {
	s.t.Helper()
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
		s.t.Fatalf("the supervisor did not exit; log:\n%s", s.stderr.String())
	}
	var exitErr *exec.ExitError
	if s.err != nil && !errors.As(s.err, &exitErr) {
		require.NoError(s.t, s.err, "log:\n%s", s.stderr.String())
	}
	left := orphans(s.t, 0)
	if s.killedGroups {
		left = slices.DeleteFunc(left, func(o string) bool { return strings.Contains(o, " Z ") })
	}
	require.Empty(s.t, left, "left behind; log:\n%s", s.stderr.String())
	state := s.cmd.ProcessState
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return state.ExitCode()
}

func (s *supervisorRun) signal(sig syscall.Signal) {
	s.t.Helper()
	require.NoError(s.t, s.cmd.Process.Signal(sig))
}

func (s *supervisorRun) running() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func (s *supervisorRun) orderLines() []string {
	s.t.Helper()
	b, err := os.ReadFile(s.order)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(s.t, err)
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

var serviceLogLine = regexp.MustCompile(`^\[[a-z][a-z0-9_-]*\] (start|ready|exited [0-9]+)$`)

// logLines returns the supervisor's own lines from the task log, "[<service>] ...".
func (s *supervisorRun) logLines() []string {
	var lines []string
	for _, line := range strings.Split(s.stderr.String(), "\n") {
		if serviceLogLine.MatchString(line) {
			lines = append(lines, line)
		}
	}
	return lines
}

func (s *supervisorRun) waitForLog(line string) {
	s.t.Helper()
	waitFor(s.t, line, func() bool { return strings.Contains(s.stderr.String(), line+"\n") })
}

func (s *supervisorRun) waitForOrder(line string) {
	s.t.Helper()
	waitFor(s.t, line, func() bool {
		b, _ := os.ReadFile(s.order)
		return strings.Contains("\n"+string(b), "\n"+line+"\n")
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// children returns this process's children by pid, each with its state and stat line.
func children() map[int]string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	found := map[int]string{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat")) //nolint:gosec
		if err != nil {
			continue
		}
		// pid (comm) state ppid ...
		end := bytes.LastIndexByte(stat, ')')
		fields := strings.Fields(string(stat[end+1:]))
		if len(fields) >= 2 && fields[1] == strconv.Itoa(os.Getpid()) {
			found[pid] = fields[0] + " " + string(stat[:end+1])
		}
	}
	return found
}

// orphans returns this process's children other than except: processes and zombies that were
// reparented to it.
func orphans(t *testing.T, except int) []string {
	t.Helper()
	var found []string
	for pid, stat := range children() {
		if pid != except {
			found = append(found, fmt.Sprintf("%d %s", pid, stat))
		}
	}
	return found
}

// reapOrphans kills and reaps whatever a failed test left with this process.
func reapOrphans() {
	for pid := range children() {
		_ = unix.Kill(pid, unix.SIGKILL)
		var ws unix.WaitStatus
		_, _ = unix.Wait4(pid, &ws, 0, nil)
	}
}

// waitUntilOrder is a unit line that waits for a line in the order file.
func waitUntilOrder(line string) string {
	return `for _ in $(seq 400); do grep -qx '` + line + `' "$ORDER_FILE" && break; sleep 0.05; done` + "\n"
}

// The init runs once, after task-setup.sh and before any service, and each service inherits what
// the hooks set. The primary service gets the supervisor's arguments, the others none.
func TestSupervisorRunsInitOnceBeforeServices(t *testing.T) {
	s := newSupervisorRun(t, "p,q")
	s.unit("p", `echo "p $HOOK_VAR $HOOK_LOCAL $#:$*" >>"$ORDER_FILE"`+"\n"+waitUntilOrder("q.*")+"exit 0\n")
	s.unit("q", `echo "q $HOOK_VAR $HOOK_LOCAL $#:$*" >>"$ORDER_FILE"`+"\nsleep 300\n")
	s.start("-p", "2222", "two words")
	require.Equal(t, 0, s.wait())

	order := s.orderLines()
	require.Equal(t, []string{"setup", "prep", "hook"}, order[:3])
	require.ElementsMatch(t, []string{"p hook local 3:-p 2222 two words", "q hook local 0:"}, order[3:])
	require.Equal(t, []string{"[p] start", "[q] start", "[p] exited 0", "[q] exited 143"}, s.logLines())
}

// When the primary service exits, the supervisor stops the others, with SIGKILL for one that
// outlives the grace period, and exits with the primary service's status.
func TestSupervisorPrimaryExitStopsOthers(t *testing.T) {
	s := newSupervisorRun(t, "p,q,stubborn")
	s.unit("p", waitUntilOrder("q up")+waitUntilOrder("stubborn up")+"exit 7\n")
	s.unit("q", "echo 'q up' >>\"$ORDER_FILE\"\nsleep 300\n")
	s.unit("stubborn", "trap '' TERM\necho 'stubborn up' >>\"$ORDER_FILE\"\nsleep 300\n")
	s.killedGroups = true
	s.start()
	require.Equal(t, 7, s.wait())

	lines := s.logLines()
	require.Equal(t, []string{"[p] start", "[q] start", "[stubborn] start", "[p] exited 7"}, lines[:4])
	require.ElementsMatch(t, []string{"[q] exited 143", "[stubborn] exited 137"}, lines[4:])
}

// A service that is not the primary one only logs its exit: the task keeps running, and the
// service's helpers are gone with it. Its readiness is logged but not reported.
func TestSupervisorKeepsRunningWhenOtherServiceExits(t *testing.T) {
	s := newSupervisorRun(t, "p,q")
	// Log helpers like the units' own.
	readyLog := `
log() {
    trap '' INT TERM HUP
    tee -p >(ready_check) >&2
    wait "$!"
}
ready_check() {
    trap - INT TERM HUP
    exec "$DET_PYTHON_EXECUTABLE" /run/determined/check_ready_logs.py --ready-regex up "${service_ready_args[@]}"
}
`
	s.unit("p", readyLog+`{ bash -c 'echo up >&2; exec sleep 300'; } 2> >(log)`+"\n")
	s.unit("q", readyLog+`sleep 300 &
echo "$!" >"$ORDER_FILE.q-helper"
{ bash -c 'echo up >&2; sleep 0.2; echo bye >&2; exit 5'; } 2> >(log)
`)
	s.start()
	s.waitForLog("[q] exited 5")
	s.waitForLog("[p] ready")
	require.True(t, s.running())
	require.Empty(t, orphans(t, s.cmd.Process.Pid))
	helper, err := os.ReadFile(s.order + ".q-helper")
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join("/proc", strings.TrimSpace(string(helper))))
	require.True(t, os.IsNotExist(err), "the helper of q is gone")
	require.Contains(t, s.stderr.String(), "\nbye\n")

	require.Contains(t, s.orderLines(), "posted p")
	require.NotContains(t, s.orderLines(), "posted q")
	readyArgs, err := os.ReadFile(filepath.Join(s.dir, "ready-args"))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"p post=1", "q post="}, strings.Split(strings.TrimSpace(string(readyArgs)), "\n"))

	s.signal(syscall.SIGTERM)
	require.Equal(t, 128+int(syscall.SIGTERM), s.wait())
	lines := s.logLines()
	require.Equal(t, []string{"[p] start", "[q] start"}, lines[:2])
	require.ElementsMatch(t, []string{"[p] ready", "[q] ready", "[q] exited 5", "[p] exited 143"}, lines[2:])
	require.Equal(t, "[p] exited 143", lines[len(lines)-1])
}

// SIGINT, SIGTERM and SIGHUP reach every service, and the supervisor then exits as if killed by the
// signal.
func TestSupervisorForwardsStopSignals(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(unix.SignalName(sig), func(t *testing.T) {
			s := newSupervisorRun(t, "p,q")
			service := filepath.Join(s.dir, "service")
			writeScript(t, service, `
for sig in INT TERM HUP; do
    trap "echo '$1 $sig' >>\"\$ORDER_FILE\"; exit 0" "$sig"
done
echo "$1 up" >>"$ORDER_FILE"
while :; do sleep 0.05; done
`)
			s.unit("p", fmt.Sprintf("%q p\n", service))
			s.unit("q", fmt.Sprintf("%q q\n", service))
			s.start()
			s.waitForOrder("p up")
			s.waitForOrder("q up")
			s.signal(sig)
			require.Equal(t, 128+int(sig), s.wait())

			name := strings.TrimPrefix(unix.SignalName(sig), "SIG")
			require.Subset(t, s.orderLines(), []string{"p " + name, "q " + name})
			require.ElementsMatch(t,
				[]string{"[p] start", "[q] start", "[p] exited 0", "[q] exited 0"}, s.logLines())
		})
	}
}

// A failed init ends the task with its status before any service starts.
func TestSupervisorStopsOnInitFailure(t *testing.T) {
	s := newSupervisorRun(t, "p")
	s.unit("p", "echo p >>\"$ORDER_FILE\"\n")
	s.env = append(s.env, "PREP_EXIT=4")
	s.start()
	require.Equal(t, 4, s.wait())
	require.Equal(t, []string{"setup", "prep"}, s.orderLines())
	require.Empty(t, s.logLines())
}

// DET_SERVICES must name known services, each once; otherwise the init does not run.
func TestSupervisorRejectsInvalidServices(t *testing.T) {
	for _, services := range []string{"", "p,nope", "p,p", "../p", ",p", "P"} {
		t.Run(services, func(t *testing.T) {
			s := newSupervisorRun(t, services)
			s.unit("p", "echo p >>\"$ORDER_FILE\"\n")
			s.start()
			require.Equal(t, 1, s.wait())
			require.Equal(t, []string{"setup"}, s.orderLines())
			require.Empty(t, s.logLines())
		})
	}
}

// The ssh unit under the supervisor: the sessions' environment is the one after the init, sshd gets
// the supervisor's arguments, its log loses the login records message, and its readiness is
// reported and logged.
func TestSupervisorSSHUnit(t *testing.T) {
	s := newSupervisorRun(t, "ssh")
	sshd := filepath.Join(s.dir, "sshd")
	writeScript(t, sshd, `
printf '%s\0' "$@" >"$ORDER_FILE.sshd-args"
printf '%s\r\n' "Server listening on 0.0.0.0 port 2222." >&2
for _ in $(seq 400); do
    grep -qx 'posted ssh' "$ORDER_FILE" && break
    sleep 0.05
done
printf '%s\r\n' "Attempt to write login records by non-root user (aborting)" "Accepted publickey for det" >&2
exit 0
`)
	unit := staticScript(t, SSHServiceResource)
	require.Contains(t, unit, sshdPath)
	s.unit("ssh", strings.Replace(unit, sshdPath, sshd, 1))
	require.NoError(t, os.WriteFile(filepath.Join(s.run, "ssh", "authorized_keys_unmodified"),
		[]byte("ssh-ed25519 AAAAkey det\n"), 0o600))
	sshdArgs := []string{"-f", "/run/determined/ssh/sshd_config", "-p", "2222", "-D", "-e"}
	s.start(sshdArgs...)
	require.Equal(t, 0, s.wait())

	keys, err := os.ReadFile(filepath.Join(s.run, "ssh", "authorized_keys"))
	require.NoError(t, err)
	require.Contains(t, string(keys), `environment="HOOK_VAR=hook",`)
	require.Contains(t, string(keys), `environment="DET_SERVICES=ssh",`)
	require.True(t, strings.HasSuffix(string(keys), " ssh-ed25519 AAAAkey det\n"), string(keys))
	args, err := os.ReadFile(s.order + ".sshd-args")
	require.NoError(t, err)
	require.Equal(t, strings.Join(sshdArgs, "\x00")+"\x00", string(args))

	require.Contains(t, s.orderLines(), "posted ssh")
	log := strings.Replace(s.stderr.String(), "[ssh] ready\n", "", 1)
	require.Contains(t, log, "Server listening on 0.0.0.0 port 2222.\r\nAccepted publickey for det\r\n")
	require.NotContains(t, log, "login records")
	require.Equal(t, []string{"[ssh] start", "[ssh] ready", "[ssh] exited 0"}, s.logLines())
}

// The jupyter unit under the supervisor: the idle check runs in the background while JupyterLab
// runs in the foreground with today's flags, and the idle check ends with it.
func TestSupervisorJupyterUnit(t *testing.T) {
	s := newSupervisorRun(t, "jupyter")
	bin := filepath.Join(s.dir, "bin")
	require.NoError(t, os.Mkdir(bin, 0o700))
	writeScript(t, filepath.Join(bin, "jupyter"), `
printf '%s\0' "$@" >"$ORDER_FILE.jupyter-args"
for _ in $(seq 400); do
    [[ -s $IDLE_PID_FILE ]] && break
    sleep 0.05
done
idle=$(cat "$IDLE_PID_FILE")
if [[ -n $idle && -d /proc/$idle ]]; then
    echo "idle check runs" >>"$ORDER_FILE"
fi
echo "SHELL=$SHELL" >>"$ORDER_FILE"
echo "[I 2024-01-01 ServerApp] Jupyter Server 2.14.0 is running at:" >&2
for _ in $(seq 400); do
    grep -qx 'posted jupyter' "$ORDER_FILE" && break
    sleep 0.05
done
exit 0
`)
	s.unit("jupyter", staticScript(t, JupyterServiceResource))
	s.env = append(s.env,
		"PATH="+bin+":"+os.Getenv("PATH"),
		"NOTEBOOK_PORT=2900",
		"DET_TASK_ID=task-1",
		"DET_NOTEBOOK_TOKEN=secret",
	)
	s.start()
	require.Equal(t, 0, s.wait())

	order := s.orderLines()
	require.Contains(t, order, "idle check runs")
	require.Contains(t, order, "posted jupyter")
	require.Regexp(t, regexp.MustCompile(`^SHELL=/`), findPrefix(order, "SHELL="))
	idle, err := os.ReadFile(filepath.Join(s.dir, "idle-pid"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join("/proc", strings.TrimSpace(string(idle))))
	require.True(t, os.IsNotExist(err), "the idle check is gone")

	logFormat := "%(levelname)s: [%(name)s] %(message)s"
	want := []string{
		"lab", "--ServerApp.port=2900",
		"--ServerApp.allow_origin=*",
		"--ServerApp.base_url=/proxy/task-1/",
		"--ServerApp.allow_root=True",
		"--ServerApp.certfile=" + s.run + "/jupyter/jupyterCert.pem",
		"--ServerApp.keyfile=" + s.run + "/jupyter/jupyterKey.key",
		"--ServerApp.ip=*",
		"--ServerApp.open_browser=False",
		"--ServerApp.token=secret",
		"--ServerApp.trust_xheaders=True",
		"--Application.log_format=" + logFormat,
		"--JupyterApp.log_format=" + logFormat,
		"--ExtensionApp.log_format=" + logFormat,
		"--LabServerApp.log_format=" + logFormat,
		"--LabApp.log_format=" + logFormat,
		"--ServerApp.log_format=" + logFormat,
		"--ServerApp.root_dir=/",
		"--ContentsManager.preferred_dir=" + s.work,
	}
	args, err := os.ReadFile(s.order + ".jupyter-args")
	require.NoError(t, err)
	require.Equal(t, strings.Join(want, "\x00")+"\x00", string(args))
	require.Equal(t, []string{"[jupyter] start", "[jupyter] ready", "[jupyter] exited 0"}, s.logLines())
	require.Contains(t, s.stderr.String(), "Jupyter Server 2.14.0 is running at:\n")
}

func findPrefix(lines []string, prefix string) string {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}
