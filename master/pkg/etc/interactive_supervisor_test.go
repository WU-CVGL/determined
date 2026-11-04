//go:build linux

package etc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// pythonStandIn stands in for DET_PYTHON_EXECUTABLE: prep_container records itself and exits with
// PREP_EXIT; check_ready_logs.py reads its input like the real one and, on the first match, writes
// "<service> READY" to the --report-to FIFO, logs "[<service>] ready" and records
// "posted <service>" unless --no-post; check_idle.py records its pid; report_service_states.py,
// the reporter of the services' states, records its process group in REPORTER_GROUP_FILE and runs
// as itself with REPORTER_PYTHON, posting to the stand-in master, or without it only waits for the
// stop line; "-c" runs the program with REPORTER_PYTHON, as the vscode unit's download does.
const pythonStandIn = `
case $1 in
-c)
    exec "$REPORTER_PYTHON" "$@"
    ;;
-m)
    [[ "$*" == "-m determined.exec.prep_container --resources --proxy --download_context_directory" ]] || exit 2
    echo prep >>"$ORDER_FILE"
    exit "${PREP_EXIT:-0}"
    ;;
"$RUN_DIR"/check_ready_logs.py)
    shift
    regex= service= post=1 report=
    while (($#)); do
        case $1 in
        --ready-regex) regex=$2; shift 2 ;;
        --service) service=$2; shift 2 ;;
        --no-post) post=; shift ;;
        --report-to) report=$2; shift 2 ;;
        *) exit 2 ;;
        esac
    done
    printf '%s post=%s\n' "$service" "$post" >>"$READY_ARGS_FILE"
    while IFS= read -r line; do
        if [[ $line =~ ^($regex) ]]; then
            if [[ -n $service ]]; then
                if [[ -n $report ]]; then
                    printf '%s READY\n' "$service" >"$report"
                fi
                printf '[%s] ready\n' "$service" >&2
            fi
            if [[ -n $post ]]; then
                echo "posted $service" >>"$ORDER_FILE"
            fi
            exit 0
        fi
    done
    ;;
"$RUN_DIR"/report_service_states.py)
    stat=$(</proc/self/stat)
    stat=${stat##*) }
    read -r _ _ group _ <<<"$stat"
    echo "$group" >>"$REPORTER_GROUP_FILE"
    if [[ -n $REPORTER_PYTHON ]]; then
        exec "$REPORTER_PYTHON" "$@"
    fi
    while read -r name _; do
        [[ $name != - ]] || exit 0
    done <"$2"
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
	master *standInMaster
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
	require.NoError(t, os.WriteFile(filepath.Join(s.run, ServiceStateReporterResource),
		[]byte(staticScript(t, ServiceStateReporterResource)), 0o600))
	s.hook(defaultHook)
	s.master = newStandInMaster(t, filepath.Join(dir, "reports"))
	reporterPython, _ := exec.LookPath("python3")
	pythonPath := filepath.Join(dir, "pylib")
	writeDeterminedStandIn(t, pythonPath)

	s.env = append(os.Environ(),
		"DET_SERVICES="+services,
		"DET_PYTHON_EXECUTABLE="+filepath.Join(dir, "python"),
		"RUN_DIR="+s.run,
		"ORDER_FILE="+s.order,
		"READY_ARGS_FILE="+filepath.Join(dir, "ready-args"),
		"IDLE_PID_FILE="+filepath.Join(dir, "idle-pid"),
		"REPORTS_FILE="+filepath.Join(dir, "reports"),
		"REPORTER_GROUP_FILE="+filepath.Join(dir, "reporter-group"),
		"REPORTER_PYTHON="+reporterPython,
		"PYTHONPATH="+pythonPath,
		"DET_MASTER="+s.master.srv.URL,
		"DET_USER=det",
		"DET_SESSION_TOKEN="+standInAllocationToken,
		"DET_ALLOCATION_ID="+standInAllocationID,
		"DET_SERVICE_STATE_REPORT_SECONDS=",
		"PS4=+ ",
		"TCD_STARTUP_HOOK=",
		"HOOK_VAR=",
		"HOOK_LOCAL=",
	)
	return s
}

// defaultHook is the startup hook of every run. Exported and shell-only variables of the hook both
// reach the services; common names do not change the supervisor's own.
const defaultHook = "echo hook >>\"$ORDER_FILE\"\nexport HOOK_VAR=hook\nHOOK_LOCAL=local\n" +
	"services=x primary=x name=x\n"

// strictHook is defaultHook with the shell options that many scripts start with, which stay set
// after the init.
const strictHook = "set -euo pipefail\n" + defaultHook

// hook replaces the startup hook in the work dir.
func (s *supervisorRun) hook(body string) {
	s.t.Helper()
	writeScript(s.t, filepath.Join(s.work, "startup-hook.sh"), body)
}

// preInitGuard starts a unit that sets nothing before the init.
const preInitGuard = "[[ ${supervisor_phase-} != pre-init ]] || return 0\n"

// unit installs a stand-in service unit that sets nothing before the init, with /run/determined in
// the test's directory.
func (s *supervisorRun) unit(name, body string) {
	s.t.Helper()
	s.staticUnit(name, preInitGuard+body)
}

// staticUnit installs a service unit as it is, with /run/determined in the test's directory.
func (s *supervisorRun) staticUnit(name, body string) {
	s.t.Helper()
	body = strings.ReplaceAll(body, "/run/determined/", s.run+"/")
	require.NoError(s.t, os.WriteFile(filepath.Join(s.run, "services", name+".sh"),
		[]byte("#!/usr/bin/env bash\n"+body), 0o600))
}

// setting changes one of the supervisor's settings, "supervisor_<name>=<value>".
func (s *supervisorRun) setting(name string, value int) {
	s.t.Helper()
	path := filepath.Join(s.run, InteractiveSupervisorResource)
	b, err := os.ReadFile(path) //nolint:gosec
	require.NoError(s.t, err)
	line := regexp.MustCompile(`(?m)^supervisor_` + name + `=[0-9]+$`)
	require.Len(s.t, line.FindAll(b, -1), 1, name)
	b = line.ReplaceAll(b, []byte(fmt.Sprintf("supervisor_%s=%d", name, value)))
	require.NoError(s.t, os.WriteFile(path, b, 0o700)) //nolint:gosec
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
	waitForWithin(t, what, 20*time.Second, cond)
}

// waitForWithin waits up to timeout for cond and returns how long it took.
func waitForWithin(t *testing.T, what string, timeout time.Duration, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > timeout {
			t.Fatalf("timed out waiting for %q", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return time.Since(start)
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

// readyLog defines log helpers like the units' own: "2> >(log)" copies a service's log to the task
// log and to a readiness check for the line "up".
const readyLog = `
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
	// Without bash's report of the killed subshell.
	require.NotContains(t, s.stderr.String(), "Killed")
}

// A service that is not the primary one only logs its exit: the task keeps running, and the
// service's helpers are gone with it. Its readiness is logged but not reported.
func TestSupervisorKeepsRunningWhenOtherServiceExits(t *testing.T) {
	s := newSupervisorRun(t, "p,q")
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

// leaveChild is a service that starts a process in its process group, with its own standard
// descriptors, writes its pid to $1 and exits with $2.
const leaveChild = `bash -c 'sleep 300 </dev/null >/dev/null 2>&1 & echo "$!" >"$1"; exit "$2"' bash `

// reapLeftChild checks that the process whose pid is in pidFile has exited, as a zombie of this
// process, the child subreaper, reaps it and returns the signal that killed it.
func reapLeftChild(t *testing.T, pidFile string) syscall.Signal {
	t.Helper()
	b, err := os.ReadFile(pidFile) //nolint:gosec
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	require.NoError(t, err)
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	require.NoError(t, err)
	end := bytes.LastIndexByte(stat, ')')
	require.Equal(t, "Z", strings.Fields(string(stat[end+1:]))[0], "the left process has exited")
	var ws unix.WaitStatus
	_, err = unix.Wait4(pid, &ws, 0, nil)
	require.NoError(t, err)
	require.True(t, ws.Signaled())
	return ws.Signal()
}

// What a service that is not the primary one leaves in its process group is stopped before its exit
// is logged, and the task keeps running.
func TestSupervisorStopsWhatOtherServiceLeaves(t *testing.T) {
	s := newSupervisorRun(t, "p,q")
	s.unit("p", readyLog+`{ bash -c 'echo up >&2; exec sleep 300'; } 2> >(log)`+"\n")
	child := filepath.Join(s.dir, "child")
	s.unit("q", leaveChild+fmt.Sprintf("%q 5\n", child))
	s.start()
	s.waitForLog("[q] exited 5")
	require.Equal(t, unix.SIGTERM, reapLeftChild(t, child))
	s.waitForLog("[p] ready")
	require.True(t, s.running())
	require.Empty(t, orphans(t, s.cmd.Process.Pid))

	s.signal(syscall.SIGTERM)
	require.Equal(t, 128+int(syscall.SIGTERM), s.wait())
	require.ElementsMatch(t,
		[]string{"[p] start", "[q] start", "[p] ready", "[q] exited 5", "[p] exited 143"}, s.logLines())
}

// A process left in the group is found whatever its name, also with a newline in it: one that
// ignores SIGTERM is killed after the grace period.
func TestSupervisorStopsLeftProcessWithNewlineInName(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	require.NoError(t, err)
	s := newSupervisorRun(t, "p,q")
	s.unit("p", readyLog+`{ bash -c 'echo up >&2; exec sleep 300'; } 2> >(log)`+"\n")
	child := filepath.Join(s.dir, "child")
	require.NoError(t, os.Symlink(sleep, filepath.Join(s.dir, "a\nb")))
	s.unit("q", fmt.Sprintf(`bash -c 'trap "" TERM; "$1" 300 </dev/null >/dev/null 2>&1 & echo "$!" >"$2"; exit 5' `+
		`bash %q/a$'\n'b %q`+"\n", s.dir, child))
	s.start()
	s.waitForLog("[q] exited 5")
	require.Equal(t, unix.SIGKILL, reapLeftChild(t, child))
	require.True(t, s.running())

	s.signal(syscall.SIGTERM)
	require.Equal(t, 128+int(syscall.SIGTERM), s.wait())
}

// What the primary service leaves in its process group is stopped, and the task ends with the
// service's exit status.
func TestSupervisorStopsWhatPrimaryServiceLeaves(t *testing.T) {
	s := newSupervisorRun(t, "p,q")
	child := filepath.Join(s.dir, "child")
	s.unit("p", waitUntilOrder("q up")+leaveChild+fmt.Sprintf("%q 3\n", child))
	s.unit("q", "echo 'q up' >>\"$ORDER_FILE\"\nsleep 300\n")
	s.start()
	waitFor(t, "the supervisor's exit", func() bool { return !s.running() })
	require.Equal(t, unix.SIGTERM, reapLeftChild(t, child))
	require.Equal(t, 3, s.wait())
	require.Equal(t, []string{"[p] start", "[q] start", "[p] exited 3", "[q] exited 143"}, s.logLines())
}

// On a stop, a process that a service leaves in its process group and that ignores the signal is
// killed when the grace period has passed, not much later however long the scans of the group take,
// and the service keeps its exit status.
func TestSupervisorStopKillsWhatServiceLeaves(t *testing.T) {
	s := newSupervisorRun(t, "p,q")
	s.setting("stop_grace_seconds", 2)
	service := filepath.Join(s.dir, "service")
	writeScript(t, service, `
bash -c 'trap "" TERM; echo "$$" >"$1.tmp"; mv "$1.tmp" "$1"; exec sleep 300' bash "$1" \
    </dev/null >/dev/null 2>&1 &
for _ in $(seq 400); do [[ -e $1 ]] && break; sleep 0.05; done
trap 'echo "q term" >>"$ORDER_FILE"; exit 0' TERM
echo 'q up' >>"$ORDER_FILE"
# Without this shell's report of the stopped sleep.
exec 2>/dev/null
while :; do sleep 0.05; done
`)
	child := filepath.Join(s.dir, "child")
	s.unit("p", "sleep 300\n")
	s.unit("q", fmt.Sprintf("%q %q\n", service, child))
	s.start()
	s.waitForOrder("q up")
	s.signal(syscall.SIGTERM)
	s.waitForOrder("q term")
	took := waitForWithin(t, "[q] exited 0", 5*time.Second, func() bool {
		return strings.Contains(s.stderr.String(), "[q] exited 0\n")
	})
	// Less the time from the signal to the test's view of the service's record of it.
	require.Greater(t, took, 1800*time.Millisecond)
	waitFor(t, "the supervisor's exit", func() bool { return !s.running() })
	require.Equal(t, unix.SIGKILL, reapLeftChild(t, child))
	require.Equal(t, 128+int(syscall.SIGTERM), s.wait())
	require.ElementsMatch(t,
		[]string{"[p] start", "[q] start", "[p] exited 143", "[q] exited 0"}, s.logLines())
}

// Once a service has exited, its unit's background helpers get SIGTERM, and one that ignores it is
// killed after the grace period: the exit is logged then, and the task ends with the service's
// status.
func TestSupervisorKillsHelperThatIgnoresStop(t *testing.T) {
	s := newSupervisorRun(t, "p")
	helper := filepath.Join(s.dir, "helper")
	s.unit("p", fmt.Sprintf(`bash -c 'trap "" TERM; echo "$$" >"$1.tmp"; mv "$1.tmp" "$1"; exec sleep 300' bash %[1]q \
    </dev/null >/dev/null 2>&1 &
for _ in $(seq 400); do [[ -e %[1]q ]] && break; sleep 0.05; done
exit 7
`, helper))
	s.start()
	waitFor(t, "the helper", func() bool {
		_, err := os.Stat(helper)
		return err == nil
	})
	// The grace is 1 s; the service exits right after it starts the helper.
	took := waitForWithin(t, "[p] exited 7", 4*time.Second, func() bool {
		return strings.Contains(s.stderr.String(), "[p] exited 7\n")
	})
	require.Greater(t, took, 800*time.Millisecond, "the helper had the grace period")
	require.Equal(t, 7, s.wait())

	b, err := os.ReadFile(helper) //nolint:gosec
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join("/proc", strings.TrimSpace(string(b))))
	require.True(t, os.IsNotExist(err), "the helper is gone")
	require.Equal(t, []string{"[p] start", "[p] exited 7"}, s.logLines())
	require.NotContains(t, s.stderr.String(), "Killed")
}

// On a stop, the unit's background helpers have had the signal with the service: one that ignores
// it is killed as soon as the service has exited, within the supervisor's grace period, so that the
// service keeps its exit status.
func TestSupervisorStopKillsHelperThatIgnoresStop(t *testing.T) {
	s := newSupervisorRun(t, "p")
	s.setting("stop_grace_seconds", 2)
	service := filepath.Join(s.dir, "service")
	writeScript(t, service, `
trap 'echo "p term" >>"$ORDER_FILE"; exit 0' TERM
echo 'p up' >>"$ORDER_FILE"
# Without this shell's report of the stopped sleep.
exec 2>/dev/null
while :; do sleep 0.05; done
`)
	helper := filepath.Join(s.dir, "helper")
	s.unit("p", fmt.Sprintf(`bash -c 'trap "" TERM; echo "$$" >"$1.tmp"; mv "$1.tmp" "$1"; exec sleep 300' bash %[1]q \
    </dev/null >/dev/null 2>&1 &
for _ in $(seq 400); do [[ -e %[1]q ]] && break; sleep 0.05; done
%[2]q
`, helper, service))
	s.start()
	s.waitForOrder("p up")
	s.signal(syscall.SIGTERM)
	s.waitForOrder("p term")
	took := waitForWithin(t, "[p] exited 0", 5*time.Second, func() bool {
		return strings.Contains(s.stderr.String(), "[p] exited 0\n")
	})
	require.Less(t, took, time.Second, "the helper is killed before the grace period has passed")
	require.Equal(t, 128+int(syscall.SIGTERM), s.wait())

	b, err := os.ReadFile(helper) //nolint:gosec
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join("/proc", strings.TrimSpace(string(b))))
	require.True(t, os.IsNotExist(err), "the helper is gone")
	require.Equal(t, []string{"[p] start", "[p] exited 0"}, s.logLines())
	require.NotContains(t, s.stderr.String(), "Killed")
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

// A process that a service starts in a session of its own, like an sshd session, may keep the
// service's log open after the service has exited. The log helper is stopped then, the service
// keeps its exit status, and the task log gets no other lines.
func TestSupervisorLogHeldOutsideService(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stop   bool
		status int
		exited string
	}{
		{name: "exit", status: 255, exited: "[p] exited 255"},
		{name: "stop", stop: true, status: 128 + int(syscall.SIGTERM), exited: "[p] exited 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSupervisorRun(t, "p")
			// Longer than the log helper's two waits.
			s.setting("stop_grace_seconds", 10)
			service := filepath.Join(s.dir, "service")
			// Like sshd, the service keeps only its standard descriptors.
			writeScript(t, service, `
for fd in /proc/$$/fd/*; do
    fd=${fd##*/}
    if ((fd > 2 && fd < 255)) && [[ -e /proc/$$/fd/$fd ]]; then
        exec {fd}>&-
    fi
done
setsid sleep 300 >/dev/null &
echo "$!" >"$1"
echo up >&2
if [[ -z $2 ]]; then
    exit 255
fi
trap 'exit 0' TERM
# Without this shell's report of the stopped sleep.
exec 2>/dev/null
while :; do sleep 0.05; done
`)
			holder := filepath.Join(s.dir, "holder")
			s.unit("p", readyLog+fmt.Sprintf("{ %q %q %q; } 2> >(log)\n", service, holder,
				map[bool]string{true: "run"}[tc.stop]))
			s.start()
			s.waitForLog("[p] ready")
			if tc.stop {
				s.signal(syscall.SIGTERM)
			}
			waitFor(t, "the supervisor's exit", func() bool { return !s.running() })

			b, err := os.ReadFile(holder) //nolint:gosec
			require.NoError(t, err)
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			require.NoError(t, err)
			require.NoError(t, unix.Kill(pid, unix.SIGKILL))
			var ws unix.WaitStatus
			_, err = unix.Wait4(pid, &ws, 0, nil)
			require.NoError(t, err, "the holder is left to the container")

			require.Equal(t, tc.status, s.wait())
			_, log, found := strings.Cut(s.stderr.String(), "[p] start\n")
			require.True(t, found)
			require.ElementsMatch(t, []string{"up", "[p] ready", tc.exited},
				strings.Split(strings.TrimSuffix(log, "\n"), "\n"))
		})
	}
}

// A startup hook may leave "set -euo pipefail" on. The units run with those options, as the old
// entrypoints did, and the supervisor still starts, stops and reaps the services.
func TestSupervisorStrictHook(t *testing.T) {
	strict := `if [[ -o nounset && -o pipefail ]]; then echo "%s strict" >>"$ORDER_FILE"; fi` + "\n"
	t.Run("primary exit", func(t *testing.T) {
		s := newSupervisorRun(t, "p,q")
		s.hook(strictHook)
		s.unit("p", fmt.Sprintf(strict, "p")+waitUntilOrder("q up")+"exit 3\n")
		s.unit("q", fmt.Sprintf(strict, "q")+"echo 'q up' >>\"$ORDER_FILE\"\nsleep 300\n")
		s.start()
		require.Equal(t, 3, s.wait())
		require.Subset(t, s.orderLines(), []string{"p strict", "q strict"})
		require.Equal(t, []string{"[p] start", "[q] start", "[p] exited 3", "[q] exited 143"}, s.logLines())
	})
	t.Run("stop", func(t *testing.T) {
		s := newSupervisorRun(t, "p,q")
		s.hook(strictHook)
		s.unit("p", readyLog+`{ bash -c 'echo up >&2; exec sleep 300'; } 2> >(log)`+"\n")
		s.unit("q", "echo 'q up' >>\"$ORDER_FILE\"\nsleep 300\n")
		s.start()
		s.waitForLog("[p] ready")
		s.waitForOrder("q up")
		s.signal(syscall.SIGTERM)
		require.Equal(t, 128+int(syscall.SIGTERM), s.wait())
		require.ElementsMatch(t,
			[]string{"[p] start", "[q] start", "[p] ready", "[p] exited 143", "[q] exited 143"}, s.logLines())
	})
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

// sshUnitWith returns the ssh unit with sshd at path, in its check and in the line that runs it.
func sshUnitWith(t *testing.T, path string) string {
	t.Helper()
	unit := staticScript(t, SSHServiceResource)
	require.Equal(t, 2, strings.Count(unit, sshdPath), "the check and the line that runs sshd")
	return strings.ReplaceAll(unit, sshdPath, path)
}

// The ssh unit under the supervisor: the sessions' environment is the one after the init, sshd gets
// the supervisor's arguments, its log loses the login records message, and its readiness is
// reported and logged, also with a strict startup hook.
func TestSupervisorSSHUnit(t *testing.T) {
	for name, hook := range map[string]string{"default": defaultHook, "strict": strictHook} {
		t.Run(name, func(t *testing.T) { testSupervisorSSHUnit(t, hook) })
	}
}

func testSupervisorSSHUnit(t *testing.T, hook string) {
	s := newSupervisorRun(t, "ssh")
	s.hook(hook)
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
	s.staticUnit("ssh", sshUnitWith(t, sshd))
	require.NoError(t, os.WriteFile(filepath.Join(s.run, "ssh", "authorized_keys_unmodified"),
		[]byte("ssh-ed25519 AAAAkey det\n"), 0o600))
	s.env = append(s.env, "HASHED_PASSWORD="+codeServerHash)
	sshdArgs := []string{"-f", "/run/determined/ssh/sshd_config", "-p", "2222", "-D", "-e"}
	s.start(sshdArgs...)
	require.Equal(t, 0, s.wait())

	keys, err := os.ReadFile(filepath.Join(s.run, "ssh", "authorized_keys"))
	require.NoError(t, err)
	require.Contains(t, string(keys), `environment="HOOK_VAR=hook",`)
	require.Contains(t, string(keys), `environment="DET_SERVICES=ssh",`)
	// The vscode service's login stays out of the sessions.
	require.NotContains(t, string(keys), "HASHED_PASSWORD")
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

// The jupyter unit under the supervisor on a shell with an idle timeout: the idle check runs in the
// background while JupyterLab runs in the foreground with its flags, and the idle check ends with
// it, also with a strict startup hook. SHELL is the user's login shell before the init, so that the startup hook
// sees it and may set another one for JupyterLab.
func TestSupervisorJupyterUnit(t *testing.T) {
	out, err := exec.Command("bash", "-c", `getent passwd "$(whoami)" | cut -d: -f7`).Output()
	require.NoError(t, err)
	loginShell := strings.TrimSpace(string(out))
	if loginShell == "" {
		loginShell = "/bin/bash"
	}
	hookShell := `echo "$SHELL" >"$ORDER_FILE.hook-shell"` + "\n"
	for _, tc := range []struct {
		name, hook, shell string
	}{
		{name: "default", hook: defaultHook + hookShell, shell: loginShell},
		{name: "strict", hook: strictHook + hookShell, shell: loginShell},
		{
			name:  "hook shell",
			hook:  defaultHook + hookShell + "export SHELL=/opt/hook-chosen/bash\n",
			shell: "/opt/hook-chosen/bash",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testSupervisorJupyterUnit(t, tc.hook, shellJupyterWatched)
			seen, err := os.ReadFile(s.order + ".hook-shell")
			require.NoError(t, err)
			require.Equal(t, loginShell+"\n", string(seen), "the hook's SHELL")
			require.Contains(t, s.orderLines(), "SHELL="+tc.shell)
		})
	}
}

// jupyterTask is the task that the jupyter unit runs in: its environment from the master and the
// base URL that JupyterLab must get.
type jupyterTask struct {
	env     []string
	baseURL string
	// idle is whether the idle check runs.
	idle bool
}

var (
	// shellJupyter is a shell with the jupyter service: no idle type, a base URL at its port.
	shellJupyter = jupyterTask{
		env:     []string{"NOTEBOOK_BASE_URL=/proxy/task-1:2900/"},
		baseURL: "/proxy/task-1:2900/",
	}
	// shellJupyterWatched is a shell with the jupyter service and an idle timeout.
	shellJupyterWatched = jupyterTask{
		env: []string{
			"NOTEBOOK_BASE_URL=/proxy/task-1:2900/", "DET_JUPYTER_IDLE_TYPE=activity",
			"DET_IDLE_TIMEOUT_SECONDS=2700",
		},
		baseURL: "/proxy/task-1:2900/",
		idle:    true,
	}
)

func testSupervisorJupyterUnit(t *testing.T, hook string, task jupyterTask) *supervisorRun {
	s := newSupervisorRun(t, "jupyter")
	s.hook(hook)
	bin := filepath.Join(s.dir, "bin")
	require.NoError(t, os.Mkdir(bin, 0o700))
	writeScript(t, filepath.Join(bin, "jupyter"), `
printf '%s\0' "$@" >"$ORDER_FILE.jupyter-args"
if [[ -n $EXPECT_IDLE ]]; then
    for _ in $(seq 400); do
        [[ -s $IDLE_PID_FILE ]] && break
        sleep 0.05
    done
else
    # Time for an idle check to start, which it must not.
    sleep 1
fi
idle=$(cat "$IDLE_PID_FILE" 2>/dev/null)
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
	s.staticUnit("jupyter", staticScript(t, JupyterServiceResource))
	expectIdle := ""
	if task.idle {
		expectIdle = "1"
	}
	// The task's variables only, whatever the test's environment has.
	s.env = slices.DeleteFunc(s.env, func(kv string) bool {
		return strings.HasPrefix(kv, "DET_JUPYTER_IDLE_TYPE=") || strings.HasPrefix(kv, "NOTEBOOK_BASE_URL=")
	})
	s.env = append(s.env,
		"PATH="+bin+":"+os.Getenv("PATH"),
		"NOTEBOOK_PORT=2900",
		"DET_TASK_ID=task-1",
		"DET_NOTEBOOK_TOKEN=secret",
		"SHELL=/opt/image/sh",
		"EXPECT_IDLE="+expectIdle,
	)
	s.env = append(s.env, task.env...)
	s.start()
	require.Equal(t, 0, s.wait())

	order := s.orderLines()
	require.Contains(t, order, "posted jupyter")
	require.Regexp(t, regexp.MustCompile(`^SHELL=/`), findPrefix(order, "SHELL="))
	idle, err := os.ReadFile(filepath.Join(s.dir, "idle-pid"))
	if task.idle {
		require.Contains(t, order, "idle check runs")
		require.NoError(t, err)
		_, err = os.Stat(filepath.Join("/proc", strings.TrimSpace(string(idle))))
		require.True(t, os.IsNotExist(err), "the idle check is gone")
	} else {
		require.NotContains(t, order, "idle check runs")
		require.True(t, os.IsNotExist(err), "the idle check never started")
	}

	logFormat := "%(levelname)s: [%(name)s] %(message)s"
	want := []string{
		"lab", "--ServerApp.port=2900",
		"--ServerApp.port_retries=0",
		"--ServerApp.allow_origin=*",
		"--ServerApp.base_url=" + task.baseURL,
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
	return s
}

// Before the init, the units only set their defaults and return: the ssh and vscode units none, the
// jupyter unit SHELL.
func TestSupervisorUnitsPreInit(t *testing.T) {
	for unit, want := range map[string]string{
		SSHServiceResource:     "returned \n",
		JupyterServiceResource: "returned /",
		VSCodeServiceResource:  "returned \n",
	} {
		t.Run(unit, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "unit.sh")
			require.NoError(t, os.WriteFile(path, []byte(staticScript(t, unit)), 0o600))
			cmd := exec.Command("bash", "-c", //nolint:gosec
				`set -e; SHELL=; supervisor_phase=pre-init; source "$1"; echo "returned $SHELL"`,
				"bash", path)
			cmd.Env = append(os.Environ(), "DET_PYTHON_EXECUTABLE=false")
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))
			require.True(t, strings.HasPrefix(string(out), want), string(out))
			require.Equal(t, 1, strings.Count(string(out), "\n"), string(out))
		})
	}
}

// On a shell, the jupyter unit runs JupyterLab under the base URL that the master sets, and the
// idle check only when the master sets the idle type, for a shell with an idle timeout.
func TestSupervisorJupyterUnitOnShell(t *testing.T) {
	t.Run("not watched", func(t *testing.T) {
		testSupervisorJupyterUnit(t, defaultHook, shellJupyter)
	})
	t.Run("watched", func(t *testing.T) {
		testSupervisorJupyterUnit(t, defaultHook, shellJupyterWatched)
	})
}

// replace changes the one occurrence of old in the supervisor to new.
func (s *supervisorRun) replace(old, new string) {
	s.t.Helper()
	path := filepath.Join(s.run, InteractiveSupervisorResource)
	b, err := os.ReadFile(path) //nolint:gosec
	require.NoError(s.t, err)
	require.Equal(s.t, 1, strings.Count(string(b), old), old)
	b = []byte(strings.Replace(string(b), old, new, 1))
	require.NoError(s.t, os.WriteFile(path, b, 0o700)) //nolint:gosec
}

// The supervisor needs bash 5 or later and /proc, and says which is missing before the init.
func TestSupervisorRequiresBashAndProc(t *testing.T) {
	for _, tc := range []struct {
		name, old, new, want string
	}{
		{
			name: "bash",
			old:  "BASH_VERSINFO[0] < 5",
			new:  "BASH_VERSINFO[0] < 99",
			want: "interactive task: bash 5 or later is required, found ",
		},
		{
			name: "proc",
			old:  "[[ ! -r /proc/self/stat ]]",
			new:  "[[ ! -r /proc/self/no-such-file ]]",
			want: "interactive task: /proc is required\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSupervisorRun(t, "p")
			s.unit("p", "echo p >>\"$ORDER_FILE\"\n")
			s.replace(tc.old, tc.new)
			s.start()
			require.Equal(t, 1, s.wait())
			require.Empty(t, s.orderLines(), "nothing runs")
			require.True(t, strings.HasPrefix(s.stderr.String(), tc.want), s.stderr.String())
			require.Equal(t, 1, strings.Count(s.stderr.String(), "\n"), s.stderr.String())
		})
	}
}

// pathWithout returns $PATH without the directories that have program.
func pathWithout(t *testing.T, program string) string {
	t.Helper()
	var dirs []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(dir, program)); err == nil {
			if dir == "/usr/bin" || dir == "/bin" {
				t.Skipf("%s is installed in %s", program, dir)
			}
			continue
		}
		dirs = append(dirs, dir)
	}
	return strings.Join(dirs, string(filepath.ListSeparator))
}

// The jupyter unit says when the image has no jupyter and exits 127, before it starts anything;
// as a service besides the primary one, the task stays up.
func TestSupervisorJupyterUnitWithoutJupyter(t *testing.T) {
	for _, services := range []string{"jupyter", "p,jupyter"} {
		t.Run(services, func(t *testing.T) {
			s := newSupervisorRun(t, services)
			s.unit("p", waitUntilOrder("p done")+"exit 0\n")
			s.staticUnit("jupyter", staticScript(t, JupyterServiceResource))
			s.env = append(s.env, "PATH="+pathWithout(t, "jupyter"), "DET_JUPYTER_IDLE_TYPE=activity")
			s.start()
			missing := "jupyter service: jupyter is not installed\n[jupyter] exited 127\n"
			s.waitForLog("[jupyter] exited 127")
			require.Contains(t, s.stderr.String(), missing)
			if services == "jupyter" {
				require.Equal(t, 127, s.wait())
				require.Equal(t, []string{"[jupyter] start", "[jupyter] exited 127"}, s.logLines())
			} else {
				require.True(t, s.running())
				require.NoError(t, os.WriteFile(s.order, []byte("p done\n"), 0o600))
				require.Equal(t, 0, s.wait())
				require.Equal(t, []string{
					"[p] start", "[jupyter] start", "[jupyter] exited 127", "[p] exited 0",
				}, s.logLines())
			}
			_, err := os.Stat(filepath.Join(s.dir, "idle-pid"))
			require.True(t, os.IsNotExist(err), "the idle check never started")
		})
	}
}

// The ssh unit says when the image has no sshd and exits 127, before it writes the sessions'
// environment.
func TestSupervisorSSHUnitWithoutSSHD(t *testing.T) {
	s := newSupervisorRun(t, "ssh")
	s.staticUnit("ssh", sshUnitWith(t, filepath.Join(s.dir, "no-sshd")))
	require.NoError(t, os.WriteFile(filepath.Join(s.run, "ssh", "authorized_keys_unmodified"),
		[]byte("ssh-ed25519 AAAAkey det\n"), 0o600))
	s.start("-p", "2222")
	require.Equal(t, 127, s.wait())
	require.Contains(t, s.stderr.String(), "ssh service: sshd is not installed\n[ssh] exited 127\n")
	require.Equal(t, []string{"[ssh] start", "[ssh] exited 127"}, s.logLines())
	_, err := os.Stat(filepath.Join(s.run, "ssh", "authorized_keys"))
	require.True(t, os.IsNotExist(err), "no environment written")
}

func findPrefix(lines []string, prefix string) string {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

// The allocation of the stand-in master's task and its token.
const (
	standInAllocationID    = "task.1"
	standInAllocationToken = "allocation-token"
)

// determinedStandIn is the part of the determined package that report_service_states.py and the
// vscode unit's download use: the task session posts JSON and gets a response, streamed or not,
// with the allocation token, as the real one does, and raises on an HTTP error or a timeout.
var determinedStandIn = map[string]string{
	"determined/__init__.py":        "",
	"determined/common/__init__.py": "",
	"determined/common/api/__init__.py": `import json as _json
import urllib.request


def canonicalize_master_url(url):
    return url.rstrip("/")


class BaseSession:
    def __init__(self, master, token):
        self.master = master
        self.token = token

    def with_retry(self, retries):
        return self

    def post(self, path, json=None, timeout=None):
        request = urllib.request.Request(
            self.master + path,
            data=_json.dumps(json).encode(),
            method="POST",
            headers={
                "Content-Type": "application/json",
                "Grpc-Metadata-x-allocation-token": "Bearer " + self.token,
            },
        )
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(request, timeout=timeout) as response:
            response.read()

    def get(self, path, params=None, headers=None, timeout=None, stream=False):
        request = urllib.request.Request(
            self.master + path,
            headers={"Grpc-Metadata-x-allocation-token": "Bearer " + self.token},
        )
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        return _Response(opener.open(request))


class _Response:
    def __init__(self, response):
        self._response = response

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self._response.close()

    @property
    def text(self):
        with self._response:
            return self._response.read().decode()

    def iter_content(self, size):
        while True:
            chunk = self._response.read(size)
            if not chunk:
                return
            yield chunk
`,
	"determined/common/api/authentication.py": `import os

from determined.common import api


def login_from_task(master_address, cert):
    return api.BaseSession(master_address, os.environ["DET_SESSION_TOKEN"])
`,
	"determined/common/api/certs.py": `def default_load(master_url):
    return None
`,
}

func writeDeterminedStandIn(t *testing.T, dir string) {
	t.Helper()
	for name, body := range determinedStandIn {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}
}

// requireReporterPython skips a test of the reporter itself without python3.
func requireReporterPython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
}

// How the stand-in master answers a snapshot.
const (
	masterAccepts int32 = iota
	masterRefuses
	masterHangs
)

// servicesPost is a snapshot that the stand-in master received: its sequence, "<name>=<state>" of
// each service in the order posted, and whether the master took it.
type servicesPost struct {
	Sequence int
	States   string
	Accepted bool
}

// standInMaster takes the reporter's snapshots for the allocation standInAllocationID with its
// token, records each in order, in memory and as a line "<sequence> <states>" in a file, which
// units can wait for, and accepts, refuses or does not answer them as its mode says.
type standInMaster struct {
	srv  *httptest.Server
	file string
	mode atomic.Int32
	stop chan struct{}

	mu    sync.Mutex
	posts []servicesPost
	// codeServer holds the files that the master serves below /det/code-server/ to the
	// allocation's token, by their path below it; a tarball waits for codeServerGate when it is
	// set. codeServerGets lists every path below it that was asked for, in order.
	codeServer     map[string][]byte
	codeServerGate chan struct{}
	codeServerGets []string
}

func newStandInMaster(t *testing.T, file string) *standInMaster {
	t.Helper()
	m := &standInMaster{file: file, stop: make(chan struct{})}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	// Before the server's Close, which waits for the requests it does not answer.
	t.Cleanup(func() { close(m.stop) })
	return m
}

func (m *standInMaster) serve(w http.ResponseWriter, r *http.Request) {
	if path, ok := strings.CutPrefix(r.URL.Path, "/det/code-server/"); ok {
		m.serveCodeServer(w, r, path)
		return
	}
	var body struct {
		Sequence int `json:"sequence"`
		Services []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"services"`
	}
	if r.Method != http.MethodPost ||
		r.URL.Path != "/api/v1/allocations/"+standInAllocationID+"/services" ||
		r.Header.Get("Grpc-Metadata-x-allocation-token") != "Bearer "+standInAllocationToken ||
		json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "unexpected request", http.StatusBadRequest)
		return
	}
	states := make([]string, 0, len(body.Services))
	for _, s := range body.Services {
		states = append(states, s.Name+"="+strings.TrimPrefix(s.State, "STATE_"))
	}
	mode := m.mode.Load()
	post := servicesPost{
		Sequence: body.Sequence, States: strings.Join(states, " "), Accepted: mode == masterAccepts,
	}
	m.mu.Lock()
	m.posts = append(m.posts, post)
	f, err := os.OpenFile(m.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = fmt.Fprintf(f, "%d %s\n", post.Sequence, post.States)
		_ = f.Close()
	}
	m.mu.Unlock()

	switch mode {
	case masterAccepts:
		_, _ = w.Write([]byte("{}"))
	case masterRefuses:
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	default:
		select {
		case <-r.Context().Done():
		case <-m.stop:
		}
	}
}

// received returns the snapshots that the master received, in order; accepted those it took.
func (m *standInMaster) received() []servicesPost {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.posts)
}

func (m *standInMaster) accepted() []servicesPost {
	return slices.DeleteFunc(m.received(), func(p servicesPost) bool { return !p.Accepted })
}

// statesOf returns the states of the service name in the posts, without repeats.
func statesOf(posts []servicesPost, name string) []string {
	var states []string
	for _, p := range posts {
		for _, s := range strings.Fields(p.States) {
			if service, state, _ := strings.Cut(s, "="); service == name &&
				(len(states) == 0 || states[len(states)-1] != state) {
				states = append(states, state)
			}
		}
	}
	return states
}

// requireSnapshots checks that each post holds every service once, in order, and that the
// sequence starts at 1, increases with each change and stays with a re-post of the same
// snapshot.
func requireSnapshots(t *testing.T, posts []servicesPost, services ...string) {
	t.Helper()
	require.NotEmpty(t, posts)
	require.Equal(t, 1, posts[0].Sequence, posts)
	for i, p := range posts {
		var names []string
		for _, s := range strings.Fields(p.States) {
			name, _, _ := strings.Cut(s, "=")
			names = append(names, name)
		}
		require.Equal(t, services, names, p)
		if i == 0 {
			continue
		}
		if prev := posts[i-1]; p.States == prev.States {
			require.Equal(t, prev.Sequence, p.Sequence, posts)
		} else {
			require.Greater(t, p.Sequence, prev.Sequence, posts)
		}
	}
}

// waitUntilPosted is a unit line that waits until the master has received a snapshot with the
// state of the service name.
func waitUntilPosted(name, state string) string {
	return `for _ in $(seq 400); do grep -qE ' ` + name + `=` + state +
		`( |$)' "$REPORTS_FILE" && break; sleep 0.05; done` + "\n"
}

// inService quotes unit lines to run in the service, a "bash -c" of the unit.
func inService(lines string) string {
	return strings.ReplaceAll(lines, "'", `'\''`)
}

// requireReporterGone checks that nothing is left of the reporter's process groups.
func (s *supervisorRun) requireReporterGone() {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, "reporter-group"))
	require.NoError(s.t, err)
	groups := strings.Fields(string(b))
	require.NotEmpty(s.t, groups)
	for _, g := range groups {
		group, err := strconv.Atoi(g)
		require.NoError(s.t, err)
		require.ErrorIs(s.t, unix.Kill(-group, 0), unix.ESRCH, "the reporter's group %d is gone", group)
	}
}

const reportFailed = "interactive task: reporting the services' states failed: "

// The master gets the snapshot of every service's state whenever a state changes, with a new
// sequence: STARTING when a service starts, READY when its readiness check matches, EXITED once it
// has stopped, and UNAVAILABLE instead when it exited 127 before it was READY. The primary
// service's exit reaches the master before the supervisor exits, with the task's status.
func TestSupervisorReportsServiceStates(t *testing.T) {
	requireReporterPython(t)
	for name, hook := range map[string]string{"default hook": defaultHook, "strict hook": strictHook} {
		t.Run(name, func(t *testing.T) {
			s := newSupervisorRun(t, "p,q,r")
			s.hook(hook)
			s.unit("p", readyLog+waitUntilPosted("p", "STARTING")+waitUntilPosted("q", "EXITED")+
				waitUntilPosted("r", "UNAVAILABLE")+`{ bash -c 'echo up >&2; `+
				inService(waitUntilPosted("p", "READY"))+`exit 3'; } 2> >(log)`+"\n")
			s.unit("q", readyLog+waitUntilPosted("q", "STARTING")+`{ bash -c 'echo up >&2; `+
				inService(waitUntilPosted("q", "READY"))+`exit 5'; } 2> >(log)`+"\n")
			s.unit("r", waitUntilPosted("r", "STARTING")+"exit 127\n")
			s.start()
			require.Equal(t, 3, s.wait())

			posts := s.master.received()
			require.Equal(t, posts, s.master.accepted())
			requireSnapshots(t, posts, "p", "q", "r")
			require.Equal(t, []string{"STARTING", "READY", "EXITED"}, statesOf(posts, "p"), posts)
			require.Equal(t, []string{"STARTING", "READY", "EXITED"}, statesOf(posts, "q"), posts)
			require.Equal(t, []string{"STARTING", "UNAVAILABLE"}, statesOf(posts, "r"), posts)
			require.Equal(t, "p=EXITED q=EXITED r=UNAVAILABLE", posts[len(posts)-1].States)
			require.NotContains(t, s.stderr.String(), reportFailed)
			require.ElementsMatch(t, []string{
				"[p] start", "[q] start", "[r] start", "[r] exited 127", "[q] ready", "[q] exited 5",
				"[p] ready", "[p] exited 3",
			}, s.logLines())
			s.requireReporterGone()
		})
	}
}

// A service that exits 127 after it was READY has run in the image: it is EXITED, not UNAVAILABLE.
func TestSupervisorReportsExit127AfterReady(t *testing.T) {
	requireReporterPython(t)
	s := newSupervisorRun(t, "p,q")
	s.unit("p", readyLog+waitUntilPosted("q", "EXITED")+`{ bash -c 'echo up >&2; exit 3'; } 2> >(log)`+"\n")
	s.unit("q", readyLog+waitUntilPosted("q", "STARTING")+`{ bash -c 'echo up >&2; `+
		inService(waitUntilPosted("q", "READY"))+`exit 127'; } 2> >(log)`+"\n")
	s.start()
	require.Equal(t, 3, s.wait())

	posts := s.master.received()
	require.Equal(t, posts, s.master.accepted())
	requireSnapshots(t, posts, "p", "q")
	require.Equal(t, []string{"STARTING", "READY", "EXITED"}, statesOf(posts, "q"), posts)
	require.Equal(t, "p=EXITED q=EXITED", posts[len(posts)-1].States)
	require.Contains(t, s.logLines(), "[q] exited 127")
	s.requireReporterGone()
}

// The reporter posts the current snapshot again every DET_SERVICE_STATE_REPORT_SECONDS, with the
// same sequence, and is gone with the supervisor. A change that comes before the first post, as
// p's readiness may, is part of the first snapshot.
func TestSupervisorRePostsServiceStates(t *testing.T) {
	requireReporterPython(t)
	s := newSupervisorRun(t, "p,q")
	s.env = append(s.env, "DET_SERVICE_STATE_REPORT_SECONDS=1")
	s.unit("p", readyLog+`{ bash -c 'echo up >&2; exec sleep 300'; } 2> >(log)`+"\n")
	s.unit("q", "sleep 300\n")
	s.start()
	s.waitForLog("[p] ready")
	waitFor(t, "two re-posts of the current snapshot", func() bool {
		posts := s.master.received()
		n := 0
		for _, p := range posts {
			if p.States == "p=READY q=STARTING" {
				n++
			}
		}
		return n >= 3
	})
	s.signal(syscall.SIGTERM)
	require.Equal(t, 128+int(syscall.SIGTERM), s.wait())

	posts := s.master.received()
	requireSnapshots(t, posts, "p", "q")
	require.Equal(t, "p=EXITED q=EXITED", posts[len(posts)-1].States)
	require.NotContains(t, s.stderr.String(), reportFailed)
	s.requireReporterGone()
	require.ElementsMatch(t,
		[]string{"[p] start", "[q] start", "[p] ready", "[p] exited 143", "[q] exited 143"}, s.logLines())
}

// A master that refuses the snapshots or does not answer them holds up no service and does not
// change the task's status. The failure is logged once; the retry carries the current snapshot,
// not the ones the master missed. A master that does not answer at the stop does not hold up the
// supervisor's exit.
func TestSupervisorReportFailure(t *testing.T) {
	requireReporterPython(t)
	for name, tc := range map[string]struct {
		mode    int32
		recover bool
	}{
		"refused":                {masterRefuses, true},
		"unanswered":             {masterHangs, true},
		"unanswered at the stop": {masterHangs, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSupervisorRun(t, "p,q")
			s.master.mode.Store(tc.mode)
			s.unit("p", readyLog+`{ bash -c 'echo up >&2; exec sleep 300'; } 2> >(log)`+"\n")
			s.unit("q", waitUntilOrder("go")+"exit 4\n")
			started := time.Now()
			s.start()
			s.waitForLog("[p] ready")
			// Less than the reporter's timeout of a post.
			require.Less(t, time.Since(started), 4*time.Second)
			require.NoError(t, os.WriteFile(s.order, []byte("go\n"), 0o600))
			s.waitForLog("[q] exited 4")
			require.True(t, s.running())
			waitFor(t, "a snapshot", func() bool { return len(s.master.received()) > 0 })

			if tc.recover {
				s.master.mode.Store(masterAccepts)
				waitFor(t, "the current snapshot", func() bool { return len(s.master.accepted()) > 0 })
				received, accepted := s.master.received(), s.master.accepted()
				require.Equal(t, "p=READY q=EXITED", accepted[0].States, received)
				require.Equal(t, slices.MaxFunc(received, func(a, b servicesPost) int {
					return a.Sequence - b.Sequence
				}).Sequence, accepted[0].Sequence, received)
				require.Equal(t, 1, strings.Count(s.stderr.String(), reportFailed), s.stderr.String())
			}

			s.signal(syscall.SIGTERM)
			stopped := time.Now()
			require.Equal(t, 128+int(syscall.SIGTERM), s.wait())
			require.Less(t, time.Since(stopped), 10*time.Second)
			require.LessOrEqual(t, strings.Count(s.stderr.String(), reportFailed), 1, s.stderr.String())
			require.NotContains(t, s.stderr.String(), "Killed")
			require.ElementsMatch(t, []string{
				"[p] start", "[q] start", "[p] ready", "[q] exited 4", "[p] exited 143",
			}, s.logLines())
			received := s.master.received()
			requireSnapshots(t, received, "p", "q")
			if tc.recover {
				for _, p := range s.master.accepted() {
					require.Contains(t, []string{"p=READY q=EXITED", "p=EXITED q=EXITED"}, p.States)
				}
				require.Equal(t, "p=EXITED q=EXITED", received[len(received)-1].States)
			}
			s.requireReporterGone()
		})
	}
}
