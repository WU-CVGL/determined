package etc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	prepContainerCall = `"$DET_PYTHON_EXECUTABLE" -m determined.exec.prep_container ` +
		`--resources --proxy --download_context_directory`
	tcdHookSource     = `test -f "${TCD_STARTUP_HOOK}" && source "${TCD_STARTUP_HOOK}"`
	startupHookSource = `test -f "${STARTUP_HOOK}" && source "${STARTUP_HOOK}"`
)

// initSource matches a line that sources the shared init, without arguments, so that the
// entrypoint's own arguments stay in place for the init and the hooks.
var initSource = regexp.MustCompile(
	`(?m)^[ \t]*(?:source|\.)[ \t]+/run/determined/` + regexp.QuoteMeta(InteractiveInitScriptResource) +
		`[ \t]*$`)

var (
	taskSetupSource = regexp.MustCompile(`(?m)^[ \t]*source[ \t]+/run/determined/task-setup\.sh[ \t]*$`)
	setErrexit      = regexp.MustCompile(`(?m)^[ \t]*set[ \t]+-e[ \t]*$`)
	setXtrace       = regexp.MustCompile(`(?m)^[ \t]*set[ \t]+-x[ \t]*$`)
	unsetXtrace     = regexp.MustCompile(`(?m)^[ \t]*set[ \t]+\+x[ \t]*$`)
)

func staticScript(t *testing.T, name string) string {
	t.Helper()
	require.NoError(t, SetRootPath("../../static/srv"))
	return string(MustStaticFile(name))
}

// The supervisor sources the shared init once, after task-setup.sh and "set -e" and before it starts
// any service. The units and the entrypoints, which only start the supervisor, run no init step.
func TestInteractiveSupervisorSourcesInitOnce(t *testing.T) {
	script := staticScript(t, InteractiveSupervisorResource)

	sources := initSource.FindAllStringIndex(script, -1)
	require.Len(t, sources, 1, "the supervisor sources %s once", InteractiveInitScriptResource)
	at := sources[0][0]
	for name, before := range map[string]*regexp.Regexp{
		"task-setup.sh": taskSetupSource, "set -e": setErrexit,
	} {
		loc := before.FindStringIndex(script)
		require.NotNil(t, loc, "the supervisor has %s", name)
		require.Less(t, loc[0], at, "%s comes before the init", name)
	}
	start := regexp.MustCompile(`(?m)^start_service `).FindStringIndex(script)
	require.NotNil(t, start)
	require.Greater(t, start[0], at, "services start after the init")
	require.NotContains(t, script, "prep_container")
	require.NotContains(t, script, "STARTUP_HOOK")

	for _, name := range []string{
		ShellEntrypointResource, SSHServiceResource, JupyterServiceResource, VSCodeServiceResource,
		TensorBoardServiceResource,
	} {
		script := staticScript(t, name)
		require.False(t, initSource.MatchString(script), "%s does not source the init", name)
		require.False(t, taskSetupSource.MatchString(script), "%s does not source task-setup.sh", name)
		require.NotContains(t, script, "prep_container", name)
		require.NotContains(t, script, "STARTUP_HOOK", name)
	}
}

// The shell entrypoint is a wrapper that runs the supervisor with its service and its arguments.
func TestInteractiveEntrypointsRunSupervisor(t *testing.T) {
	for entrypoint, service := range map[string]string{ShellEntrypointResource: "ssh"} {
		script := staticScript(t, entrypoint)
		require.Regexp(t, `(?m)^DET_SERVICES=`+service+` exec /run/determined/`+
			regexp.QuoteMeta(InteractiveSupervisorResource)+` "\$@"$`, script, entrypoint)
	}
}

// The shared init runs prep_container, then both hooks inside the set -x window, and ends the
// window with its last command.
func TestInteractiveInitSteps(t *testing.T) {
	script := staticScript(t, InteractiveInitScriptResource)

	require.Equal(t, 1, strings.Count(script, "prep_container"))
	prep := strings.Index(script, prepContainerCall+"\n")
	require.NotEqual(t, -1, prep, "the init runs %s", prepContainerCall)

	xtrace := setXtrace.FindAllStringIndex(script, -1)
	require.Len(t, xtrace, 1)
	unxtrace := unsetXtrace.FindAllStringIndex(script, -1)
	require.Len(t, unxtrace, 1)
	tcd := strings.Index(script, tcdHookSource+"\n")
	hook := strings.Index(script, startupHookSource+"\n")
	require.Equal(t, 1, strings.Count(script, tcdHookSource))
	require.Equal(t, 1, strings.Count(script, startupHookSource))
	require.Contains(t, script, "\nSTARTUP_HOOK=\"startup-hook.sh\"\n")

	require.Less(t, prep, xtrace[0][0])
	require.Less(t, xtrace[0][0], tcd)
	require.Less(t, tcd, hook)
	require.Less(t, hook, unxtrace[0][0])
	require.Empty(t, strings.TrimSpace(script[unxtrace[0][1]:]), "set +x is the last command")

	require.False(t, setErrexit.MatchString(script), "the init inherits set -e from its entrypoint")
}

// runInit sources the shared init the way an entrypoint does, in a work dir with stand-ins for
// python and the hooks, then prints what the hooks set and the entrypoint's arguments.
func runInit(t *testing.T, pythonExit int, hooks bool) (exitCode int, stdout, stderr, order string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	initPath, err := filepath.Abs(filepath.Join("../../static/srv", InteractiveInitScriptResource))
	require.NoError(t, err)

	dir := t.TempDir()
	orderFile := filepath.Join(dir, "order")
	python := filepath.Join(dir, "python")
	writeScript(t, python, `
[[ "$*" == "-m determined.exec.prep_container --resources --proxy --download_context_directory" ]] || exit 2
echo prep >>"$ORDER_FILE"
exit `+strconv.Itoa(pythonExit)+`
`)
	tcdHook := filepath.Join(dir, "tcd-hook.sh")
	if hooks {
		writeScript(t, tcdHook, "echo tcd >>\"$ORDER_FILE\"\nTCD_VAR=tcd\n")
		writeScript(t, filepath.Join(dir, "startup-hook.sh"), "echo hook >>\"$ORDER_FILE\"\nHOOK_VAR=hook\n")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", //nolint:gosec
		"set -e\nsource \"$INIT\"\nprintf '%s|%s|%s\\n' \"$TCD_VAR\" \"$HOOK_VAR\" \"$*\"",
		"entrypoint.sh", "-p", "2222", "two words")
	cmd.Dir = dir
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = append(os.Environ(),
		"INIT="+initPath,
		"DET_PYTHON_EXECUTABLE="+python,
		"TCD_STARTUP_HOOK="+tcdHook,
		"ORDER_FILE="+orderFile,
		"PS4=+ ",
		"TCD_VAR=",
		"HOOK_VAR=",
	)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	require.NoError(t, ctx.Err())
	if err != nil {
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		exitCode = exitErr.ExitCode()
	}
	orderBytes, err := os.ReadFile(orderFile) //nolint:gosec
	if !os.IsNotExist(err) {
		require.NoError(t, err)
	}
	return exitCode, out.String(), errOut.String(), string(orderBytes)
}

// The hooks run after prep_container in the sourcing shell, with the entrypoint's arguments in
// place, and only they are traced.
func TestInteractiveInitRunsHooksInEntrypointShell(t *testing.T) {
	code, stdout, stderr, order := runInit(t, 0, true)
	require.Equal(t, 0, code, stderr)
	require.Equal(t, "tcd|hook|-p 2222 two words\n", stdout)
	require.Equal(t, "prep\ntcd\nhook\n", order)

	// Each level of source adds a "+" to the trace, so the init's own lines start with "++".
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	require.Regexp(t, `^\++ test -f `, lines[0], "the trace starts at the hooks: %q", stderr)
	require.Regexp(t, `^\++ set \+x$`, lines[len(lines)-1])
	require.NotContains(t, stderr, "prep_container")
}

// Without hooks the init still succeeds under set -e, and the entrypoint goes on.
func TestInteractiveInitWithoutHooks(t *testing.T) {
	code, stdout, stderr, order := runInit(t, 0, false)
	require.Equal(t, 0, code, stderr)
	require.Equal(t, "||-p 2222 two words\n", stdout)
	require.Equal(t, "prep\n", order)
}

// A failed prep_container ends the entrypoint with its status before any hook runs.
func TestInteractiveInitStopsOnPrepContainerFailure(t *testing.T) {
	code, stdout, _, order := runInit(t, 4, true)
	require.Equal(t, 4, code)
	require.Empty(t, stdout)
	require.Equal(t, "prep\n", order)
}
