//go:build linux

package etc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The releases of code-server in the tests: the one that the task pins and another one, which a
// user image ships or the master is configured with when it is not the task's.
const (
	codeServerPinned = "4.96.4"
	codeServerOther  = "4.99.0"
	codeServerPort   = "2950"
	// codeServerHash stands in for the hash of the task's password.
	codeServerHash = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNo"
)

// serveCodeServer serves the code-server files that the stand-in master has, by their path below
// /det/code-server/, to the allocation's token, as a bearer token in either header that the
// master takes. A tarball waits for codeServerGate, when it is set.
func (m *standInMaster) serveCodeServer(w http.ResponseWriter, r *http.Request, path string) {
	m.mu.Lock()
	m.codeServerGets = append(m.codeServerGets, path)
	body, ok := m.codeServer[path]
	gate := m.codeServerGate
	m.mu.Unlock()
	bearer := "Bearer " + standInAllocationToken
	if r.Method != http.MethodGet || (r.Header.Get("Authorization") != bearer &&
		r.Header.Get("Grpc-Metadata-x-allocation-token") != bearer) {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	if gate != nil && strings.HasSuffix(path, ".tar.gz") {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		case <-m.stop:
			return
		}
	}
	_, _ = w.Write(body)
}

// codeServerRequests returns the paths below /det/code-server/ that were asked for, in order.
func (m *standInMaster) codeServerRequests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.codeServerGets)
}

// serveCodeServerRelease makes the master serve the tarball of version with the stand-in
// code-server tagged tag, and its checksum file, or a wrong one when badSum is set.
func (m *standInMaster) serveCodeServerRelease(t *testing.T, version, tag string, badSum bool) {
	t.Helper()
	arch := codeServerArch(t)
	name := "code-server-" + version + "-linux-" + arch + ".tar.gz"
	tarball := codeServerTarball(t, version, arch, codeServerStandIn(version, tag))
	sum := sha256.Sum256(tarball)
	if badSum {
		sum = sha256.Sum256(append(tarball, 0))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.codeServer == nil {
		m.codeServer = map[string][]byte{}
	}
	m.codeServer[version+"/"+name] = tarball
	m.codeServer[version+"/"+name+".sha256"] = []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n")
}

// codeServerArch is code-server's name of this machine's architecture.
func codeServerArch(t *testing.T) string {
	t.Helper()
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOARCH
	default:
		t.Skipf("code-server has no release for %s", runtime.GOARCH)
		return ""
	}
}

// codeServerStandIn is a code-server of release version. Like code-server, it reads its config
// file first, ~/.config/code-server/config.yaml without --config, and when that file is missing it
// writes it and logs so on its standard output. Then it prints the release for --version.
// Otherwise it records its arguments, its environment, the HASHED_PASSWORD that it gets and, in
// the order file, "code-server <tag>", logs the address that it serves on, and exits once the order
// file has "stop code-server".
func codeServerStandIn(version, tag string) string {
	return codeServerStandInWith(version, tag, true)
}

// codeServerStandInWith is codeServerStandIn, which takes its address as code-server does: from
// --bind-addr, then CODE_SERVER_HOST and PORT from the environment, then, when flags is set,
// --host and --port.
func codeServerStandInWith(version, tag string, flags bool) string {
	hostPort := ""
	if flags {
		hostPort = `
    --host) ((i++)) && arg_host=${!i} ;;
    --port) ((i++)) && arg_port=${!i} ;;`
	}
	return `
config=$HOME/.config/code-server/config.yaml
version=
host=localhost
port=8080
arg_host=
arg_port=
for ((i = 1; i <= $#; i++)); do
    case ${!i} in
    --config) ((i++)) && config=${!i} ;;
    --version) version=1 ;;
    --bind-addr) ((i++)) && host=${!i%:*} && port=${!i##*:} ;;` + hostPort + `
    esac
done
host=${arg_host:-${CODE_SERVER_HOST:-$host}}
port=${arg_port:-${PORT:-$port}}
mkdir -p "${config%/*}" || exit 1
if [[ ! -e $config ]]; then
    echo "password: generated" >"$config" || exit 1
    echo "[2026-01-01T00:00:00.000Z] info  Wrote default config file to $config"
fi
if [[ -n $version ]]; then
    echo "` + version + ` 0123abc with Code 1.0.0"
    exit 0
fi
printf '%s\0' "$@" >"$ORDER_FILE.code-server-args"
env -0 >"$ORDER_FILE.code-server-env"
printf '%s' "${HASHED_PASSWORD-}" >"$ORDER_FILE.code-server-hash"
echo "code-server ` + tag + `" >>"$ORDER_FILE"
echo "[2026-01-01T00:00:00.000Z] info  HTTP server listening on http://$host:$port/"
` + waitUntilOrder("stop code-server") + "exit 0\n"
}

// codeServerTarball returns a release tarball like code-server's, with the code-server script in
// code-server-<version>-linux-<arch>/bin.
func codeServerTarball(t *testing.T, version, arch, script string) []byte {
	t.Helper()
	top := "code-server-" + version + "-linux-" + arch + "/"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name, body string
		mode       int64
	}{
		{name: top, mode: 0o755},
		{name: top + "bin/", mode: 0o755},
		{name: top + "bin/code-server", body: "#!/usr/bin/env bash\n" + script, mode: 0o755},
		{name: top + "lib/", mode: 0o755},
		{name: top + "lib/node", body: "node\n", mode: 0o755},
	} {
		h := &tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(f.name, "/") {
			h.Typeflag = tar.TypeDir
		}
		require.NoError(t, tw.WriteHeader(h))
		_, err := tw.Write([]byte(f.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// vscodeTask is a task with the vscode service, under the supervisor, with the real unit: its home,
// a directory on the PATH for stand-ins, and code-server pinned to codeServerPinned.
type vscodeTask struct {
	*supervisorRun
	home string
	bin  string
}

func newVSCodeTask(t *testing.T, services, home string) *vscodeTask {
	t.Helper()
	s := newSupervisorRun(t, services)
	task := &vscodeTask{supervisorRun: s, home: home, bin: filepath.Join(s.dir, "bin")}
	require.NoError(t, os.Mkdir(task.bin, 0o700))
	s.staticUnit("vscode", staticScript(t, VSCodeServiceResource))
	s.env = slices.DeleteFunc(s.env, func(kv string) bool {
		for _, name := range []string{"HOME=", "PATH=", "HASHED_PASSWORD=", "DET_CODE_SERVER_"} {
			if strings.HasPrefix(kv, name) {
				return true
			}
		}
		return false
	})
	s.env = append(s.env,
		"HOME="+home,
		"PATH="+task.bin+":"+pathWithout(t, "code-server"),
		"DET_CODE_SERVER_VERSION="+codeServerPinned,
		"DET_CODE_SERVER_PORT="+codeServerPort,
		"HASHED_PASSWORD="+codeServerHash,
	)
	return task
}

// cacheDir is the cache's directory of the release version for this machine.
func (v *vscodeTask) cacheDir(version string) string {
	return filepath.Join(v.home, ".cache", "determined", "code-server", version,
		"linux-"+codeServerArch(v.t))
}

// addOrder appends a line to the order file, which the stand-ins wait for.
func (v *vscodeTask) addOrder(line string) {
	v.t.Helper()
	f, err := os.OpenFile(v.order, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(v.t, err)
	_, err = f.WriteString(line + "\n")
	require.NoError(v.t, err)
	require.NoError(v.t, f.Close())
}

// requireServed checks that code-server tagged tag ran for the task with the unit's arguments,
// config file and password hash, and was READY.
func (v *vscodeTask) requireServed(tag string) {
	v.t.Helper()
	require.Contains(v.t, v.orderLines(), "code-server "+tag)
	run := filepath.Join(v.run, "code-server")
	data := filepath.Join(v.home, ".local", "share", "code-server")
	want := []string{
		"--config", filepath.Join(run, "config.yaml"),
		"--bind-addr", "0.0.0.0:" + codeServerPort,
		"--host", "0.0.0.0",
		"--port", codeServerPort,
		"--auth", "password",
		"--user-data-dir", data,
		"--extensions-dir", filepath.Join(data, "extensions"),
		"--disable-telemetry",
		"--disable-update-check",
		"--disable-workspace-trust",
		"--ignore-last-opened",
		v.work,
	}
	args, err := os.ReadFile(v.order + ".code-server-args")
	require.NoError(v.t, err)
	require.Equal(v.t, strings.Join(want, "\x00")+"\x00", string(args))
	hash, err := os.ReadFile(v.order + ".code-server-hash")
	require.NoError(v.t, err)
	require.Equal(v.t, codeServerHash, string(hash))
	config, err := os.ReadFile(filepath.Join(run, "config.yaml")) //nolint:gosec
	require.NoError(v.t, err)
	require.Equal(v.t, `bind-addr: "0.0.0.0:`+codeServerPort+`"
auth: password
user-data-dir: "`+data+`"
extensions-dir: "`+filepath.Join(data, "extensions")+`"
session-socket: "`+filepath.Join(run, "session.sock")+`"
disable-telemetry: true
disable-update-check: true
disable-workspace-trust: true
`, string(config))
	require.NotContains(v.t, string(config), "password:")
	require.Contains(v.t, v.stderr.String(),
		"HTTP server listening on http://0.0.0.0:"+codeServerPort+"/\n")
}

// runVSCode runs the task with vscode as its only service until code-server is ready, then stops
// code-server and checks that it served for the task.
func (v *vscodeTask) runVSCode(tag string) {
	v.t.Helper()
	v.start()
	v.waitForLog("[vscode] ready")
	v.addOrder("stop code-server")
	require.Equal(v.t, 0, v.wait(), v.stderr.String())
	v.requireServed(tag)
	require.Equal(v.t, []string{"[vscode] start", "[vscode] ready", "[vscode] exited 0"}, v.logLines())
}

// requireEnv checks that code-server got the environment variable kv.
func (v *vscodeTask) requireEnv(kv string) {
	v.t.Helper()
	env, err := os.ReadFile(v.order + ".code-server-env")
	require.NoError(v.t, err)
	require.Contains(v.t, "\x00"+string(env), "\x00"+kv+"\x00")
}

// writeCodeServer writes the stand-in code-server of version, tagged tag, to dir/bin.
func writeCodeServer(t *testing.T, dir, version, tag string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o700))
	writeScript(t, filepath.Join(dir, "bin", "code-server"), codeServerStandIn(version, tag))
}

// lockWaiter returns whether a flock process runs for one of the tasks with the order files orders.
// The holder of the lock leaves flock as soon as it has the lock.
func lockWaiter(orders ...string) bool {
	pids, _ := filepath.Glob("/proc/[0-9]*")
	for _, pid := range pids {
		comm, err := os.ReadFile(filepath.Join(pid, "comm")) //nolint:gosec
		if err != nil || strings.TrimSpace(string(comm)) != "flock" {
			continue
		}
		env, err := os.ReadFile(filepath.Join(pid, "environ")) //nolint:gosec
		if err != nil {
			continue
		}
		env = append([]byte{0}, env...)
		for _, order := range orders {
			if bytes.Contains(env, []byte("\x00ORDER_FILE="+order+"\x00")) {
				return true
			}
		}
	}
	return false
}

// requireNoInstallLeft checks that no install left a temporary directory in the cache.
func requireNoInstallLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(dir))
	if os.IsNotExist(err) {
		return
	}
	require.NoError(t, err)
	for _, e := range entries {
		require.NotContains(t, e.Name(), ".tmp", "left in the cache")
	}
}

// The unit takes code-server from an administrator's DET_CODE_SERVER_DIR first, then from the
// PATH when it is the pinned release, then from the cache, and downloads it from the master into
// the cache last. A code-server on the PATH of another release is skipped with one line that names
// both releases. The download asks the master for the pinned release only, whatever else the
// master has, and the next task with the same home finds it in the cache.
func TestVSCodeUnitResolveOrder(t *testing.T) {
	requireReporterPython(t)
	skipped := "vscode service: skipping code-server " + codeServerOther + " on the PATH for " +
		codeServerPinned + "\n"
	for _, tc := range []struct {
		name string
		// dir is whether DET_CODE_SERVER_DIR has code-server; it is set but empty otherwise.
		dir bool
		// path is the release of the code-server on the PATH, if any.
		path  string
		cache bool
		want  string
		// strict runs the unit after a startup hook with "set -euo pipefail".
		strict bool
	}{
		{name: "dir", dir: true, path: codeServerPinned, cache: true, want: "dir"},
		{name: "path", path: codeServerPinned, cache: true, want: "path"},
		{name: "cache", path: codeServerOther, cache: true, want: "cache"},
		{name: "download", path: codeServerOther, want: "download"},
		{name: "download without code-server on the path", want: "download"},
		{name: "download after a strict hook", path: codeServerOther, want: "download", strict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			v := newVSCodeTask(t, "vscode", home)
			if tc.strict {
				v.hook(strictHook)
			}
			admin := filepath.Join(v.dir, "admin")
			require.NoError(t, os.Mkdir(admin, 0o700))
			if tc.dir {
				// The administrator's copy is used whatever its release.
				writeCodeServer(t, admin, codeServerOther, "dir")
			}
			v.env = append(v.env, "DET_CODE_SERVER_DIR="+admin)
			if tc.path != "" {
				writeScript(t, filepath.Join(v.bin, "code-server"), codeServerStandIn(tc.path, "path"))
			}
			if tc.cache {
				writeCodeServer(t, v.cacheDir(codeServerPinned), codeServerPinned, "cache")
			}
			// The master has the pinned release and the one it is configured with.
			v.master.serveCodeServerRelease(t, codeServerPinned, "download", false)
			v.master.serveCodeServerRelease(t, codeServerOther, "other", false)

			v.runVSCode(tc.want)
			// Asking code-server on the PATH for its release leaves the home as it was.
			require.NoDirExists(t, filepath.Join(home, ".config", "code-server"))
			if tc.path == codeServerOther && !tc.dir {
				require.Contains(t, v.stderr.String(), skipped)
				require.Equal(t, 1, strings.Count(v.stderr.String(), "skipping"))
			} else {
				require.NotContains(t, v.stderr.String(), "skipping")
			}
			name := "code-server-" + codeServerPinned + "-linux-" + codeServerArch(t) + ".tar.gz"
			if tc.want != "download" {
				require.Empty(t, v.master.codeServerRequests())
				return
			}
			require.Equal(t, []string{
				codeServerPinned + "/" + name + ".sha256", codeServerPinned + "/" + name,
			}, v.master.codeServerRequests())
			dir := v.cacheDir(codeServerPinned)
			require.FileExists(t, filepath.Join(dir, "bin", "code-server"))
			require.FileExists(t, filepath.Join(dir, "lib", "node"))
			requireNoInstallLeft(t, dir)

			// The next task of the user finds the release in the cache.
			next := newVSCodeTask(t, "vscode", home)
			next.env = append(next.env, "DET_MASTER="+v.master.srv.URL)
			next.runVSCode("download")
			require.Len(t, v.master.codeServerRequests(), 2)
		})
	}
}

// A task asks the master for the release that it pins only, also after the master's configured
// release changed, and also when its image ships the master's release.
func TestVSCodeUnitOldPin(t *testing.T) {
	requireReporterPython(t)
	v := newVSCodeTask(t, "vscode", t.TempDir())
	writeScript(t, filepath.Join(v.bin, "code-server"), codeServerStandIn(codeServerOther, "path"))
	v.master.serveCodeServerRelease(t, codeServerPinned, "pinned", false)
	v.master.serveCodeServerRelease(t, codeServerOther, "configured", false)
	v.runVSCode("pinned")
	for _, path := range v.master.codeServerRequests() {
		require.True(t, strings.HasPrefix(path, codeServerPinned+"/"), path)
	}
	require.Len(t, v.master.codeServerRequests(), 2)
	_, err := os.Stat(v.cacheDir(codeServerOther))
	require.True(t, os.IsNotExist(err), "the configured release is not installed")
}

// Two tasks of a user with one home that install the same release at once download and extract it
// once: the second waits for the first's lock and then uses the copy that the first installed. Both
// start code-server from it.
func TestVSCodeUnitConcurrentInstall(t *testing.T) {
	requireReporterPython(t)
	home := t.TempDir()
	first := newVSCodeTask(t, "vscode", home)
	second := newVSCodeTask(t, "vscode", home)
	master := first.master
	second.env = append(second.env, "DET_MASTER="+master.srv.URL)
	master.serveCodeServerRelease(t, codeServerPinned, "download", false)
	gate := make(chan struct{})
	master.mu.Lock()
	master.codeServerGate = gate
	master.mu.Unlock()
	name := codeServerPinned + "/code-server-" + codeServerPinned + "-linux-" + codeServerArch(t) +
		".tar.gz"

	first.start()
	second.start()
	// One task downloads the tarball, which the master holds back, while the other waits for the
	// lock.
	waitFor(t, "a tarball request", func() bool {
		return slices.Contains(master.codeServerRequests(), name)
	})
	waitFor(t, "a task waiting for the lock", func() bool {
		return lockWaiter(first.order, second.order)
	})
	require.NotContains(t, first.stderr.String()+second.stderr.String(), "[vscode] ready")
	close(gate)

	first.waitForLog("[vscode] ready")
	second.waitForLog("[vscode] ready")
	first.addOrder("stop code-server")
	second.addOrder("stop code-server")
	<-first.done
	<-second.done
	require.Equal(t, 0, first.wait())
	require.Equal(t, 0, second.wait())
	first.requireServed("download")
	second.requireServed("download")
	require.Equal(t, []string{name + ".sha256", name}, master.codeServerRequests())
	dir := first.cacheDir(codeServerPinned)
	require.FileExists(t, filepath.Join(dir, "bin", "code-server"))
	requireNoInstallLeft(t, dir)
}

// When the unit cannot have code-server, it says why in one line and exits 127, and the vscode
// service is UNAVAILABLE, while the shell and its SSH server stay up.
func TestVSCodeUnitUnavailable(t *testing.T) {
	requireReporterPython(t)
	arch := codeServerArch(t)
	name := "code-server-" + codeServerPinned + "-linux-" + arch + ".tar.gz"
	download := "vscode service: downloading " + name + " from the master failed: "
	for _, tc := range []struct {
		name, line string
		setup      func(v *vscodeTask)
	}{
		{
			name: "not on the master",
			line: download + "HTTPError: HTTP Error 404: Not Found",
		},
		{
			name: "checksum mismatch",
			line: download + "the checksum does not match",
			setup: func(v *vscodeTask) {
				v.master.serveCodeServerRelease(t, codeServerPinned, "download", true)
			},
		},
		{
			name: "not a release",
			line: "vscode service: " + name + " has no bin/code-server",
			setup: func(v *vscodeTask) {
				body := []byte("not a tarball")
				sum := sha256.Sum256(body)
				v.master.codeServer = map[string][]byte{
					codeServerPinned + "/" + name: body,
					codeServerPinned + "/" + name + ".sha256": []byte(
						hex.EncodeToString(sum[:]) + "  " + name + "\n"),
				}
			},
		},
		{
			name: "release before session-socket",
			line: "vscode service: needs code-server 4.14.1 or later, not 4.14.0",
			setup: func(v *vscodeTask) {
				v.env = append(v.env, "DET_CODE_SERVER_VERSION=4.14.0")
				v.master.serveCodeServerRelease(t, "4.14.0", "download", false)
			},
		},
		{
			name: "old glibc",
			line: "vscode service: needs glibc 2.28 or later, found 2.17",
			setup: func(v *vscodeTask) {
				writeScript(t, filepath.Join(v.bin, "ldd"), `echo "ldd (GNU libc) 2.17"`+"\n")
				v.master.serveCodeServerRelease(t, codeServerPinned, "download", false)
			},
		},
		{
			name: "cache not writable",
			line: "vscode service: cannot write to /proc/no-cache/" + codeServerPinned,
			setup: func(v *vscodeTask) {
				v.env = append(v.env, "DET_CODE_SERVER_CACHE=/proc/no-cache")
				v.master.serveCodeServerRelease(t, codeServerPinned, "download", false)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newVSCodeTask(t, "ssh,vscode", t.TempDir())
			sshd := filepath.Join(v.dir, "sshd")
			writeScript(t, sshd, `
printf '%s\r\n' "Server listening on 0.0.0.0 port 2222." >&2
`+waitUntilOrder("sshd done")+"exit 0\n")
			v.staticUnit("ssh", sshUnitWith(t, sshd))
			require.NoError(t, os.WriteFile(filepath.Join(v.run, "ssh", "authorized_keys_unmodified"),
				[]byte("ssh-ed25519 AAAAkey det\n"), 0o600))
			if tc.setup != nil {
				tc.setup(v)
			}
			v.start("-p", "2222")
			v.waitForLog("[vscode] exited 127")
			v.waitForLog("[ssh] ready")
			require.Contains(t, v.stderr.String(), tc.line+"\n[vscode] exited 127\n")
			require.Equal(t, 1, strings.Count(v.stderr.String(), "vscode service: "), v.stderr.String())
			waitFor(t, "vscode UNAVAILABLE", func() bool {
				return slices.Contains(statesOf(v.master.received(), "vscode"), "UNAVAILABLE")
			})
			require.True(t, v.running(), "the shell stays up")

			v.addOrder("sshd done")
			require.Equal(t, 0, v.wait())
			require.ElementsMatch(t, []string{
				"[ssh] start", "[vscode] start", "[vscode] exited 127", "[ssh] ready", "[ssh] exited 0",
			}, v.logLines())
			require.Equal(t, "UNAVAILABLE", statesOf(v.master.received(), "vscode")[len(statesOf(
				v.master.received(), "vscode"))-1])
			require.NotContains(t, strings.Join(v.orderLines(), "\n"), "code-server ")
			requireNoInstallLeft(t, v.cacheDir(codeServerPinned))
			keys, err := os.ReadFile(filepath.Join(v.run, "ssh", "authorized_keys"))
			require.NoError(t, err)
			require.NotContains(t, string(keys), "HASHED_PASSWORD")
		})
	}
}

// code-server takes PORT and CODE_SERVER_HOST from its environment over --bind-addr. The unit keeps
// it on the task's port when the task sets them, and leaves them to the task's programs.
func TestVSCodeUnitKeepsItsPort(t *testing.T) {
	requireReporterPython(t)
	v := newVSCodeTask(t, "vscode", t.TempDir())
	v.env = append(v.env, "PORT=3000", "CODE_SERVER_HOST=127.0.0.1")
	writeScript(t, filepath.Join(v.bin, "code-server"), codeServerStandIn(codeServerPinned, "path"))
	v.runVSCode("path")
	v.requireEnv("PORT=3000")
	v.requireEnv("CODE_SERVER_HOST=127.0.0.1")
}

// A code-server that listens on another port than the task's is never READY.
func TestVSCodeUnitNotReadyOnAnotherPort(t *testing.T) {
	requireReporterPython(t)
	v := newVSCodeTask(t, "vscode", t.TempDir())
	v.env = append(v.env, "PORT=3000")
	writeScript(t, filepath.Join(v.bin, "code-server"),
		codeServerStandInWith(codeServerPinned, "path", false))
	v.start()
	waitFor(t, "code-server listening", func() bool {
		return strings.Contains(v.stderr.String(), "HTTP server listening on http://0.0.0.0:3000/\n")
	})
	v.addOrder("stop code-server")
	require.Equal(t, 0, v.wait(), v.stderr.String())
	require.Equal(t, []string{"[vscode] start", "[vscode] exited 0"}, v.logLines())
	require.NotContains(t, statesOf(v.master.received(), "vscode"), "READY")
}
