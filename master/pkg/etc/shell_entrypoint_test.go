package etc

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	loginRecordsMessage = "Attempt to write login records by non-root user (aborting)"
	sshdListening       = "Server listening on 0.0.0.0 port 2222."
	sshdAccepted        = "Accepted publickey for det from 10.0.0.1 port 51234 ssh2: ED25519 SHA256:abc"
	sshdDisconnect      = "Received disconnect from 10.0.0.1 port 51234:11: disconnected by user"
	filterFunc          = "drop_login_records_message"
)

// shellEntrypointTail returns shell-entrypoint.sh from the definition of the sshd log filter to the
// end: the filter, the readiness check and sshd.
func shellEntrypointTail(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	require.NoError(t, SetRootPath("../../static/srv"))
	script := string(MustStaticFile(ShellEntrypointResource))

	start := strings.Index(script, "\n"+filterFunc+"() {\n")
	require.NotEqual(t, -1, start, "shell-entrypoint.sh defines %s", filterFunc)
	tail := script[start+1:]
	// sshd's log goes through the filter before tee and the readiness check.
	require.Regexp(t, `(?m)^/usr/sbin/sshd "\$@" \\\n\s+2> >\(`+filterFunc+` \| tee -p >\("\$DET_PYTHON_EXECUTABLE" `+
		`/run/determined/check_ready_logs.py --ready-regex "\$READINESS_REGEX"\) >&2\)\n$`, tail)
	return tail
}

// filterCommand runs the sshd log filter from shell-entrypoint.sh with the shell options that the
// script sets.
func filterCommand(t *testing.T) *exec.Cmd {
	t.Helper()
	tail := shellEntrypointTail(t)
	end := strings.Index(tail, "\n}\n")
	require.NotEqual(t, -1, end)
	return exec.Command("bash", "-c", "set -e\nshopt -s extglob\n"+tail[:end+3]+filterFunc) //nolint:gosec
}

func TestShellEntrypointDropsLoginRecordsMessage(t *testing.T) {
	// Lines that only resemble the message, and lines that a careless read would change.
	unchanged := strings.Join([]string{
		sshdListening + "\r",
		sshdListening,
		" " + loginRecordsMessage,
		loginRecordsMessage + " ",
		loginRecordsMessage + "\r\r",
		"sshd: " + loginRecordsMessage,
		"Attempt to write login records by non-root user aborting",
		"Attempt to write login records by non-root user (aborting) again",
		`back\slash \\ \n \t \`,
		"    leading spaces",
		"\tleading tab",
		"trailing spaces   ",
		"",
		"*",
		"-n",
		"-e",
	}, "\n") + "\n"

	for _, tc := range []struct {
		name, input, want string
	}{
		{
			name: "drops the message",
			input: sshdListening + "\r\n" + sshdAccepted + "\r\n" + loginRecordsMessage + "\r\n" +
				loginRecordsMessage + "\n" + sshdDisconnect + "\r\n",
			want: sshdListening + "\r\n" + sshdAccepted + "\r\n" + sshdDisconnect + "\r\n",
		},
		{name: "passes other lines unchanged and in order", input: unchanged, want: unchanged},
		{
			name:  "keeps a last line without a newline",
			input: sshdAccepted + "\n  last line \\",
			want:  sshdAccepted + "\n  last line \\",
		},
		{
			name:  "drops the message on a last line without a newline",
			input: sshdAccepted + "\n" + loginRecordsMessage + "\r",
			want:  sshdAccepted + "\n",
		},
		{name: "empty", input: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := filterCommand(t)
			cmd.Stdin = strings.NewReader(tc.input)
			out, err := cmd.Output()
			require.NoError(t, err)
			require.Equal(t, tc.want, string(out))
		})
	}
}

// The readiness check must get "Server listening on" while sshd still runs, so the filter passes
// each line on as soon as it reads it.
func TestShellEntrypointLoginRecordsFilterStreams(t *testing.T) {
	cmd := filterCommand(t)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	lines := make(chan string)
	go func() {
		defer close(lines)
		r := bufio.NewReader(stdout)
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				lines <- line
			}
			if err != nil {
				return
			}
		}
	}()
	next := func() (string, bool) {
		select {
		case line, ok := <-lines:
			return line, ok
		case <-time.After(5 * time.Second):
			t.Fatal("the filter held back its input")
			return "", false
		}
	}
	write := func(s string) {
		_, err := stdin.Write([]byte(s))
		require.NoError(t, err)
	}

	write(sshdListening + "\r\n")
	line, ok := next()
	require.True(t, ok)
	require.Equal(t, sshdListening+"\r\n", line)

	write(sshdAccepted + "\r\n" + loginRecordsMessage + "\r\n" + sshdDisconnect + "\r\n")
	for _, want := range []string{sshdAccepted, sshdDisconnect} {
		line, ok := next()
		require.True(t, ok)
		require.Equal(t, want+"\r\n", line)
	}

	require.NoError(t, stdin.Close())
	_, ok = next()
	require.False(t, ok)
	require.NoError(t, cmd.Wait())
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte("#!/usr/bin/env bash\n"+body), 0o700)) //nolint:gosec
}

// The end of shell-entrypoint.sh, with stand-ins for sshd and check_ready_logs.py: sshd gets the
// script's arguments, the readiness check sees "Server listening on" while sshd runs, the task log
// gets every line but the login records message, and the script exits with sshd's exit status.
func TestShellEntrypointSSHDLogs(t *testing.T) {
	tail := shellEntrypointTail(t)
	dir := t.TempDir()
	readyFile := filepath.Join(dir, "ready")
	argsFile := filepath.Join(dir, "args")

	python := filepath.Join(dir, "python")
	writeScript(t, python, `
[[ $1 == /run/determined/check_ready_logs.py && $2 == --ready-regex ]] || exit 2
# Like check_ready_logs.py: report the first line that starts with a match, then stop reading.
while IFS= read -r line; do
    if [[ $line == "$3"* ]]; then
        printf '%s\n' "$line" >"$READY_FILE"
        exit 0
    fi
done
`)
	sshd := filepath.Join(dir, "sshd")
	writeScript(t, sshd, `
printf '%s\n' "$*" >"$ARGS_FILE"
printf '%s\r\n' "$LISTENING" >&2
for _ in $(seq 200); do
    if [[ -s $READY_FILE ]]; then
        break
    fi
    sleep 0.05
done
[[ -s $READY_FILE ]] || printf 'readiness check did not get the line\r\n' >&2
printf '%s\r\n' "$ACCEPTED" "$MESSAGE" "$DISCONNECT" "$MESSAGE" >&2
exit 3
`)

	const sshdCommand = `/usr/sbin/sshd "$@"`
	require.Equal(t, 1, strings.Count(tail, sshdCommand))
	tail = strings.Replace(tail, sshdCommand, sshd+` "$@"`, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sshdArgs := []string{"-f", "/run/determined/ssh/sshd_config", "-p", "2222", "-D", "-e"}
	cmd := exec.CommandContext(ctx, "bash", //nolint:gosec
		append([]string{"-c", "set -e\nshopt -s extglob\n" + tail, "shell-entrypoint.sh"}, sshdArgs...)...)
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = append(os.Environ(),
		"DET_PYTHON_EXECUTABLE="+python,
		"READY_FILE="+readyFile,
		"ARGS_FILE="+argsFile,
		"LISTENING="+sshdListening,
		"ACCEPTED="+sshdAccepted,
		"MESSAGE="+loginRecordsMessage,
		"DISCONNECT="+sshdDisconnect,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	require.NoError(t, ctx.Err())

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, 3, exitErr.ExitCode())
	require.Equal(t, sshdListening+"\r\n"+sshdAccepted+"\r\n"+sshdDisconnect+"\r\n", stderr.String())
	require.Empty(t, stdout.String())

	ready, err := os.ReadFile(readyFile) //nolint:gosec
	require.NoError(t, err)
	require.Equal(t, sshdListening+"\r\n", string(ready))
	args, err := os.ReadFile(argsFile) //nolint:gosec
	require.NoError(t, err)
	require.Equal(t, strings.Join(sshdArgs, " ")+"\n", string(args))
}
