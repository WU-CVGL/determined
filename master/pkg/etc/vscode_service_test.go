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
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
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

// sharedCodeServerCache is the cache that an administrator mounts for the tasks of all users.
const sharedCodeServerCache = "/var/cache/determined/code-server"

// vscodeTask is a task with the vscode service, under the supervisor, with the real unit: its home,
// a directory on the PATH for stand-ins, the path that stands in for sharedCodeServerCache, which
// does not exist until a test creates it, and code-server pinned to codeServerPinned.
type vscodeTask struct {
	*supervisorRun
	home   string
	bin    string
	shared string
}

func newVSCodeTask(t *testing.T, services, home string) *vscodeTask {
	t.Helper()
	s := newSupervisorRun(t, services)
	task := &vscodeTask{
		supervisorRun: s, home: home, bin: filepath.Join(s.dir, "bin"),
		shared: filepath.Join(s.dir, "shared-cache"),
	}
	require.NoError(t, os.Mkdir(task.bin, 0o700))
	unit := staticScript(t, VSCodeServiceResource)
	require.Contains(t, unit, sharedCodeServerCache)
	s.staticUnit("vscode", strings.ReplaceAll(unit, sharedCodeServerCache, task.shared))
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

// setWaitSeconds makes the unit wait n seconds for another task's install instead of 600.
func (v *vscodeTask) setWaitSeconds(n int) {
	v.t.Helper()
	unit := strings.ReplaceAll(staticScript(v.t, VSCodeServiceResource), sharedCodeServerCache, v.shared)
	const wait = "\nvscode_wait_seconds=600\n"
	require.Contains(v.t, unit, wait)
	v.staticUnit("vscode", strings.Replace(unit, wait, "\nvscode_wait_seconds="+strconv.Itoa(n)+"\n", 1))
}

// pathWithoutPrograms is a PATH of one directory, dir, with links to the programs of this PATH
// except programs.
func pathWithoutPrograms(t *testing.T, dir string, programs ...string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	for _, from := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(from)
		if err != nil {
			continue
		}
		for _, e := range entries {
			link := filepath.Join(dir, e.Name())
			if slices.Contains(programs, e.Name()) {
				continue
			}
			if _, err := os.Lstat(link); err == nil {
				continue
			}
			require.NoError(t, os.Symlink(filepath.Join(from, e.Name()), link))
		}
	}
	return dir
}

// cacheDir is the home cache's directory of the release version for this machine.
func (v *vscodeTask) cacheDir(version string) string {
	return releaseDir(v.t, filepath.Join(v.home, ".cache", "determined", "code-server"), version)
}

// releaseDir is the directory of the release version for this machine in the cache cache.
func releaseDir(t *testing.T, cache, version string) string {
	return filepath.Join(cache, version, "linux-"+codeServerArch(t))
}

// cacheLine is the unit's line that names the cache that it took code-server from.
var cacheLine = regexp.MustCompile(`(?m)^vscode service: (installed|reused) `)

// requireCacheLine checks that the unit logged that it installed or reused (how) code-server in
// the release directory dir, with the suffix why.
func (v *vscodeTask) requireCacheLine(how, dir, why string) {
	v.t.Helper()
	verb := map[string]string{
		"installed": "installed code-server " + codeServerPinned + " into ",
		"reused":    "reused code-server " + codeServerPinned + " in ",
		"waited":    "reused code-server " + codeServerPinned + ", after waiting for its install, in ",
	}[how]
	require.NotEmpty(v.t, verb, how)
	require.Contains(v.t, v.stderr.String(), "\nvscode service: "+verb+dir+why+"\n")
	require.Len(v.t, cacheLine.FindAllString(v.stderr.String(), -1), 1, v.stderr.String())
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

// writeCachedCodeServer writes the stand-in code-server of version, tagged tag, as a complete
// release in a cache's directory dir.
func writeCachedCodeServer(t *testing.T, dir, version, tag string) {
	t.Helper()
	writeCodeServer(t, dir, version, tag)
	require.NoError(t, os.WriteFile(filepath.Join(dir, codeServerMarker), nil, 0o600))
}

// codeServerMarker is the file that marks a complete release in a cache.
const codeServerMarker = ".det-complete"

// readOnly makes the directory tree root read-only until the test ends.
func readOnly(t *testing.T, root string) {
	t.Helper()
	var dirs []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, path)
		}
		return err
	}))
	for _, dir := range dirs {
		require.NoError(t, os.Chmod(dir, 0o555)) //nolint:gosec
	}
	t.Cleanup(func() {
		for _, dir := range dirs {
			_ = os.Chmod(dir, 0o755) //nolint:gosec
		}
	})
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

// requireNoInstallLeft checks that no install left a temporary directory in the cache, except
// the ones named kept, which were there before.
func requireNoInstallLeft(t *testing.T, dir string, kept ...string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(dir))
	if os.IsNotExist(err) {
		return
	}
	require.NoError(t, err)
	for _, e := range entries {
		if !slices.Contains(kept, filepath.Join(filepath.Dir(dir), e.Name())) {
			require.NotContains(t, e.Name(), ".tmp", "left in the cache")
		}
	}
	for _, path := range kept {
		require.DirExists(t, path)
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
				writeCachedCodeServer(t, v.cacheDir(codeServerPinned), codeServerPinned, "cache")
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
			if tc.want == "cache" {
				v.requireCacheLine("reused", v.cacheDir(codeServerPinned), "")
			}
			if tc.want != "download" {
				require.Empty(t, v.master.codeServerRequests())
				return
			}
			v.requireCacheLine("installed", v.cacheDir(codeServerPinned), "")
			require.Equal(t, []string{
				codeServerPinned + "/" + name + ".sha256", codeServerPinned + "/" + name,
			}, v.master.codeServerRequests())
			dir := v.cacheDir(codeServerPinned)
			require.FileExists(t, filepath.Join(dir, "bin", "code-server"))
			require.FileExists(t, filepath.Join(dir, "lib", "node"))
			require.FileExists(t, filepath.Join(dir, codeServerMarker))
			requireNoInstallLeft(t, dir)

			// The next task of the user finds the release in the cache.
			next := newVSCodeTask(t, "vscode", home)
			next.env = append(next.env, "DET_MASTER="+v.master.srv.URL)
			next.runVSCode("download")
			next.requireCacheLine("reused", dir, "")
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
	installer, waiter := first, second
	if !strings.Contains(first.stderr.String(), "vscode service: installed ") {
		installer, waiter = second, first
	}
	installer.requireCacheLine("installed", dir, "")
	waiter.requireCacheLine("waited", dir, "")
}

// The unit prefers the cache that an administrator mounts for all users,
// /var/cache/determined/code-server, when it exists: it reuses a complete release there, also from
// a read-only mount, and installs one into it. When the release is not there and cannot be
// installed there, the unit installs it into the home cache and says why. DET_CODE_SERVER_CACHE_DIR
// names the cache instead of both. A release without the marker of a complete install is installed
// again. A task that cannot open or take the lock, that waited 600 s for it, or that has no flock,
// installs without it. The release directory has the cache's mode. A full cache counts as one that
// cannot be written to. A task leaves the temporary directories of other installs alone.
func TestVSCodeUnitSharedCache(t *testing.T) {
	requireReporterPython(t)
	if os.Geteuid() == 0 {
		t.Skip("root writes to read-only directories")
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, v *vscodeTask)
		// override is the directory, below the task's, that DET_CODE_SERVER_CACHE_DIR names.
		override string
		// cache is the cache that the unit takes code-server from: "shared", "home" or "override".
		cache, how, why string
		tag             string
		// check, when set, checks the task after it ran.
		check func(t *testing.T, v *vscodeTask)
		// downloads is how many times the task downloaded the release, when not once.
		downloads int
		// otherLock is whether the task leaves the lock file of the install alone: it was there
		// before the task, or the task takes no lock.
		otherLock bool
	}{
		{
			name:  "installs into the shared cache",
			setup: func(t *testing.T, v *vscodeTask) { require.NoError(t, os.Mkdir(v.shared, 0o777)) }, //nolint:gosec
			cache: "shared", how: "installed", tag: "download",
		},
		{
			name: "the release directory has the cache's mode",
			setup: func(t *testing.T, v *vscodeTask) {
				require.NoError(t, os.Mkdir(v.shared, 0o700))
				require.NoError(t, os.Chmod(v.shared, 0o777|os.ModeSticky)) //nolint:gosec
			},
			cache: "shared", how: "installed", tag: "download",
			check: func(t *testing.T, v *vscodeTask) {
				info, err := os.Stat(filepath.Join(v.shared, codeServerPinned))
				require.NoError(t, err)
				require.Equal(t, 0o777|os.ModeSticky, info.Mode()&(os.ModePerm|os.ModeSticky))
			},
		},
		{
			// A lock file that the user can neither write nor read, like another user's on NFSv4,
			// which locks files open for writing only.
			name: "installs without a lock file that it cannot open",
			setup: func(t *testing.T, v *vscodeTask) {
				require.NoError(t, os.MkdirAll(filepath.Join(v.shared, codeServerPinned), 0o700))
				require.NoError(t, os.WriteFile(releaseDir(t, v.shared, codeServerPinned)+".lock", nil,
					0o000))
			},
			cache: "shared", how: "installed", tag: "download", otherLock: true,
		},
		{
			name: "installs without a lock that it cannot take",
			setup: func(t *testing.T, v *vscodeTask) {
				require.NoError(t, os.Mkdir(v.shared, 0o700))
				standInFlock(t, v, 65)
			},
			cache: "shared", how: "installed", tag: "download",
			check: func(t *testing.T, v *vscodeTask) {
				require.Equal(t, []string{"-n"}, flockCalls(t, v))
			},
		},
		{
			name: "installs without the lock after waiting 600 s for it",
			setup: func(t *testing.T, v *vscodeTask) {
				require.NoError(t, os.Mkdir(v.shared, 0o700))
				standInFlock(t, v, 1)
			},
			cache: "shared", how: "installed", tag: "download",
			check: func(t *testing.T, v *vscodeTask) {
				require.Equal(t, []string{"-n", "-w 600"}, flockCalls(t, v))
			},
		},
		{
			name: "installs without flock and says so",
			setup: func(t *testing.T, v *vscodeTask) {
				require.NoError(t, os.Mkdir(v.shared, 0o700))
				v.env = append(v.env, "PATH="+v.bin+":"+pathWithoutPrograms(t,
					filepath.Join(v.dir, "path"), "flock", "code-server"))
			},
			cache: "shared", how: "installed", tag: "download", otherLock: true,
			why: " (without a lock: flock is not installed)",
			check: func(t *testing.T, v *vscodeTask) {
				require.NoFileExists(t, releaseDir(t, v.shared, codeServerPinned)+".lock")
			},
		},
		{
			// Another task's install, which may still run, next to a complete release.
			name: "leaves another install's temporary directory alone",
			setup: func(t *testing.T, v *vscodeTask) {
				dir := releaseDir(t, v.shared, codeServerPinned)
				writeCachedCodeServer(t, dir, codeServerPinned, "shared")
				writeCodeServer(t, dir+".tmp.other", codeServerPinned, "partial")
			},
			cache: "shared", how: "reused", tag: "shared",
			check: func(t *testing.T, v *vscodeTask) {
				dir := releaseDir(t, v.shared, codeServerPinned)
				requireNoInstallLeft(t, dir, dir+".tmp.other")
			},
		},
		{
			name: "falls back to the home from a full shared cache",
			setup: func(t *testing.T, v *vscodeTask) {
				require.NoError(t, os.Mkdir(v.shared, 0o700))
				tar, err := exec.LookPath("tar")
				require.NoError(t, err)
				writeScript(t, filepath.Join(v.bin, "tar"), `
if [[ $* == *`+v.shared+`* ]]; then
    echo "tar: code-server/lib/node: Cannot write: No space left on device" >&2
    echo "tar: Exiting with failure status due to previous errors" >&2
    exit 2
fi
exec `+tar+` "$@"
`)
			},
			cache: "home", how: "installed", tag: "download",
			why: " (extracting code-server-" + codeServerPinned + "-linux-" + runtime.GOARCH +
				".tar.gz failed: tar: code-server/lib/node: Cannot write: No space left on device)",
			downloads: 2,
		},
		{
			name: "reuses a read-only shared cache",
			setup: func(t *testing.T, v *vscodeTask) {
				writeCachedCodeServer(t, releaseDir(t, v.shared, codeServerPinned), codeServerPinned,
					"shared")
				readOnly(t, v.shared)
			},
			cache: "shared", how: "reused", tag: "shared",
		},
		{
			name: "reinstalls an incomplete release",
			setup: func(t *testing.T, v *vscodeTask) {
				writeCodeServer(t, releaseDir(t, v.shared, codeServerPinned), codeServerPinned, "partial")
			},
			cache: "shared", how: "installed", tag: "download",
		},
		{
			name: "falls back to the home from a read-only shared cache",
			setup: func(t *testing.T, v *vscodeTask) {
				require.NoError(t, os.Mkdir(v.shared, 0o700))
				readOnly(t, v.shared)
			},
			cache: "home", how: "installed", tag: "download",
			why: " (cannot write to /shared/" + codeServerPinned + ")",
		},
		{
			// Another user's release directory and lock, without the release.
			name: "falls back to the home from another user's release directory",
			setup: func(t *testing.T, v *vscodeTask) {
				dir := filepath.Join(v.shared, codeServerPinned)
				require.NoError(t, os.MkdirAll(dir, 0o700))
				require.NoError(t, os.WriteFile(releaseDir(t, v.shared, codeServerPinned)+".lock", nil,
					0o400))
				readOnly(t, dir)
			},
			cache: "home", how: "installed", tag: "download",
			why: " (cannot write to /shared/" + codeServerPinned + ")",
		},
		{
			name: "the override wins",
			setup: func(t *testing.T, v *vscodeTask) {
				writeCachedCodeServer(t, releaseDir(t, v.shared, codeServerPinned), codeServerPinned,
					"shared")
				writeCachedCodeServer(t, v.cacheDir(codeServerPinned), codeServerPinned, "home")
			},
			override: "mine", cache: "override", how: "installed", tag: "download",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			v := newVSCodeTask(t, "vscode", home)
			v.master.serveCodeServerRelease(t, codeServerPinned, "download", false)
			tc.setup(t, v)
			if tc.override != "" {
				v.env = append(v.env, "DET_CODE_SERVER_CACHE_DIR="+filepath.Join(v.dir, tc.override))
			}
			// The files that the unit installs are readable by every user, whatever the umask.
			umask := unix.Umask(0o077)
			v.start()
			unix.Umask(umask)
			v.waitForLog("[vscode] ready")
			v.addOrder("stop code-server")
			require.Equal(t, 0, v.wait(), v.stderr.String())
			v.requireServed(tc.tag)

			dir := map[string]string{
				"shared":   releaseDir(t, v.shared, codeServerPinned),
				"home":     v.cacheDir(codeServerPinned),
				"override": releaseDir(t, filepath.Join(v.dir, tc.override), codeServerPinned),
			}[tc.cache]
			v.requireCacheLine(tc.how, dir, strings.ReplaceAll(tc.why, "/shared", v.shared))
			if tc.check != nil {
				tc.check(t, v)
			}
			if tc.how == "reused" {
				require.Empty(t, v.master.codeServerRequests())
				return
			}
			require.Len(t, v.master.codeServerRequests(), 2*max(tc.downloads, 1))
			paths := []string{filepath.Join(dir, "bin", "code-server"), filepath.Join(dir, codeServerMarker)}
			if !tc.otherLock {
				paths = append(paths, dir+".lock")
			}
			for _, path := range paths {
				info, err := os.Stat(path)
				require.NoError(t, err)
				require.Equal(t, os.FileMode(0o004), info.Mode().Perm()&0o004, path)
			}
			requireNoInstallLeft(t, dir)
		})
	}
}

// Two tasks that share a cache install the same release at once, one without the lock and one
// with it, while a third install's temporary directory is there: each uses only its own temporary
// directory, the first rename wins, and the other task uses the winner's release. Both start
// code-server, and the third install's directory stays.
func TestVSCodeUnitOverlappingInstalls(t *testing.T) {
	requireReporterPython(t)
	home := t.TempDir()
	lockless := newVSCodeTask(t, "vscode", home)
	locked := newVSCodeTask(t, "vscode", home)
	master := lockless.master
	locked.env = append(locked.env, "DET_MASTER="+master.srv.URL)
	master.serveCodeServerRelease(t, codeServerPinned, "download", false)
	gate := make(chan struct{})
	master.mu.Lock()
	master.codeServerGate = gate
	master.mu.Unlock()
	standInFlock(t, lockless, 65)
	dir := lockless.cacheDir(codeServerPinned)
	other := dir + ".tmp.other"
	writeCodeServer(t, other, codeServerPinned, "partial")
	name := codeServerPinned + "/code-server-" + codeServerPinned + "-linux-" + codeServerArch(t) +
		".tar.gz"
	tarballs := func() int {
		n := 0
		for _, path := range master.codeServerRequests() {
			if path == name {
				n++
			}
		}
		return n
	}
	temporary := func() []string {
		dirs, err := filepath.Glob(dir + ".tmp.*")
		require.NoError(t, err)
		return slices.DeleteFunc(dirs, func(d string) bool { return d == other })
	}

	// The lockless task's install is under way, with its temporary directory, when the locked one
	// starts its own.
	lockless.start()
	waitFor(t, "the lockless task's tarball request", func() bool { return tarballs() == 1 })
	require.Len(t, temporary(), 1)
	locked.start()
	waitFor(t, "the locked task's tarball request", func() bool { return tarballs() == 2 })
	require.Len(t, temporary(), 2)
	close(gate)

	lockless.waitForLog("[vscode] ready")
	locked.waitForLog("[vscode] ready")
	lockless.addOrder("stop code-server")
	locked.addOrder("stop code-server")
	<-lockless.done
	<-locked.done
	require.Equal(t, 0, lockless.wait(), lockless.stderr.String())
	require.Equal(t, 0, locked.wait(), locked.stderr.String())
	lockless.requireServed("download")
	locked.requireServed("download")
	require.Equal(t, []string{"-n"}, flockCalls(t, lockless))
	require.FileExists(t, filepath.Join(dir, codeServerMarker))
	requireNoInstallLeft(t, dir, other)
	installer, reuser := lockless, locked
	if !strings.Contains(lockless.stderr.String(), "vscode service: installed ") {
		installer, reuser = locked, lockless
	}
	installer.requireCacheLine("installed", dir, "")
	reuser.requireCacheLine("reused", dir, "")
}

// A task without the lock does not replace a release without the marker, which only a holder of
// the lock does: after waiting for that, it runs code-server from its own temporary directory and
// removes it when it exits, also when code-server exits with an error or a stop of the shell ends
// it, before the supervisor's own cleanup of the service.
func TestVSCodeUnitIncompleteWithoutLock(t *testing.T) {
	requireReporterPython(t)
	for _, status := range []int{0, 3} {
		t.Run("exit "+strconv.Itoa(status), func(t *testing.T) {
			testVSCodeUnitIncompleteWithoutLock(t, status, false)
		})
	}
	t.Run("stop", func(t *testing.T) { testVSCodeUnitIncompleteWithoutLock(t, 0, true) })
}

// testVSCodeUnitIncompleteWithoutLock runs code-server, which exits with status, from the task's
// own temporary directory; with stop set, a stop of the shell ends it instead.
func testVSCodeUnitIncompleteWithoutLock(t *testing.T, status int, stop bool) {
	v := newVSCodeTask(t, "vscode", t.TempDir())
	arch := codeServerArch(t)
	script := codeServerStandIn(codeServerPinned, "download")
	require.True(t, strings.HasSuffix(script, "\nexit 0\n"))
	script = strings.TrimSuffix(script, "exit 0\n") + "exit " + strconv.Itoa(status) + "\n"
	name := "code-server-" + codeServerPinned + "-linux-" + arch + ".tar.gz"
	tarball := codeServerTarball(t, codeServerPinned, arch, script)
	sum := sha256.Sum256(tarball)
	v.master.codeServer = map[string][]byte{
		codeServerPinned + "/" + name:             tarball,
		codeServerPinned + "/" + name + ".sha256": []byte(hex.EncodeToString(sum[:]) + "  " + name + "\n"),
	}
	v.setWaitSeconds(1)
	standInFlock(t, v, 65)
	dir := v.cacheDir(codeServerPinned)
	writeCodeServer(t, dir, codeServerPinned, "partial")
	v.start()
	v.waitForLog("[vscode] ready")
	exited := status
	if stop {
		// The stop signals the service's group, whose members die at once.
		v.killedGroups = true
		v.signal(syscall.SIGTERM)
		exited = 128 + int(syscall.SIGTERM)
		require.Equal(t, exited, v.wait(), v.stderr.String())
	} else {
		v.addOrder("stop code-server")
		require.Equal(t, status, v.wait(), v.stderr.String())
	}
	v.requireServed("download")
	require.Equal(t, []string{
		"[vscode] start", "[vscode] ready", "[vscode] exited " + strconv.Itoa(exited),
	}, v.logLines())

	line := regexp.MustCompile(`(?m)^vscode service: installed code-server ` + codeServerPinned +
		` into (` + regexp.QuoteMeta(dir) + `\.tmp\.[^/]+)/code-server-` + codeServerPinned +
		`-linux-` + codeServerArch(t) + ` \(` + regexp.QuoteMeta(dir) + ` is incomplete\)$`)
	match := line.FindStringSubmatch(v.stderr.String())
	require.NotNil(t, match, v.stderr.String())
	require.NoDirExists(t, match[1])
	requireNoInstallLeft(t, dir)
	require.FileExists(t, filepath.Join(dir, "bin", "code-server"))
	require.NoFileExists(t, filepath.Join(dir, codeServerMarker))
}

// A stop of the shell ends an install of code-server, also while it waits for another task's lock
// or for the install of a release without the marker, before the supervisor kills it: the unit
// exits with the stop and leaves no temporary directory in the cache.
func TestVSCodeUnitStopDuringInstall(t *testing.T) {
	requireReporterPython(t)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, v *vscodeTask, dir string) (ready func() bool)
		// served is whether the master serves the tarball; else it holds it back.
		served bool
	}{
		{
			name: "waiting for the lock",
			setup: func(t *testing.T, v *vscodeTask, dir string) func() bool {
				require.NoError(t, os.MkdirAll(filepath.Dir(dir), 0o700))
				lock, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec
				require.NoError(t, err)
				t.Cleanup(func() { _ = lock.Close() })
				require.NoError(t, unix.Flock(int(lock.Fd()), unix.LOCK_EX))
				return func() bool { return lockWaiter(v.order) }
			},
		},
		{
			name: "downloading without the lock",
			setup: func(t *testing.T, v *vscodeTask, dir string) func() bool {
				standInFlock(t, v, 65)
				name := codeServerPinned + "/code-server-" + codeServerPinned + "-linux-" +
					codeServerArch(t) + ".tar.gz"
				return func() bool { return slices.Contains(v.master.codeServerRequests(), name) }
			},
		},
		{
			name: "waiting for the install of an incomplete release",
			setup: func(t *testing.T, v *vscodeTask, dir string) func() bool {
				standInFlock(t, v, 65)
				writeCodeServer(t, dir, codeServerPinned, "partial")
				return func() bool {
					done, err := filepath.Glob(dir + ".tmp.*/*/" + codeServerMarker)
					require.NoError(t, err)
					return len(done) == 1
				}
			},
			served: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newVSCodeTask(t, "vscode", t.TempDir())
			v.master.serveCodeServerRelease(t, codeServerPinned, "download", false)
			gate := make(chan struct{})
			v.master.mu.Lock()
			v.master.codeServerGate = gate
			v.master.mu.Unlock()
			dir := v.cacheDir(codeServerPinned)
			ready := tc.setup(t, v, dir)
			if tc.served {
				close(gate)
			}
			// The stop signals the service's group, whose members die at once.
			v.killedGroups = true
			v.start()
			waitFor(t, "the install "+tc.name, ready)
			v.signal(syscall.SIGTERM)
			require.Equal(t, 128+int(syscall.SIGTERM), v.wait(), v.stderr.String())
			require.Equal(t, []string{"[vscode] start", "[vscode] exited 143"}, v.logLines(),
				v.stderr.String())
			requireNoInstallLeft(t, dir)
			require.NoFileExists(t, filepath.Join(dir, codeServerMarker))
		})
	}
}

// standInFlock puts a flock on the task's PATH that fails with the status rc and records its
// options.
func standInFlock(t *testing.T, v *vscodeTask, rc int) {
	t.Helper()
	writeScript(t, filepath.Join(v.bin, "flock"), `
args=("${@:1:$#-1}")
printf '%s\n' "${args[*]}" >>"$ORDER_FILE.flock"
exit `+strconv.Itoa(rc)+"\n")
}

// flockCalls returns the options of the stand-in flock's calls.
func flockCalls(t *testing.T, v *vscodeTask) []string {
	t.Helper()
	calls, err := os.ReadFile(v.order + ".flock")
	require.NoError(t, err)
	return strings.Split(strings.TrimSuffix(string(calls), "\n"), "\n")
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
			line: "vscode service: extracting " + name + " failed: gzip: stdin: not in gzip format",
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
				v.env = append(v.env, "DET_CODE_SERVER_CACHE_DIR=/proc/no-cache")
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
