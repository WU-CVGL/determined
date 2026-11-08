//go:build linux

package etc

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fetchersStandIn is a determined package, by path under the package's directory, with what
// tensorboard_loader.py takes of determined.tensorboard.fetchers. Its build records its arguments
// in STATE["built"], sleeps STATE["build_sleep"] seconds and raises STATE["build_error"] if set,
// or returns a fetcher of the class named STATE["cls"]: StandInFetcher, OldFetcher, which has no
// forget, or NoFetch, which has no _fetch. A fetcher serves STATE["objects"], [data, mtime] by key,
// from its bucket, storage_config["bucket"]. Its _list asserts that it runs on the thread
// STATE["lister_thread"] if that is set, records the fetcher's records in STATE["lists"], raises
// STATE["list_error"] if set, and yields the keys under the path's prefix and the keys of
// STATE["stray"] as s3.py does: only a newer mtime than the recorded one, which it records before
// it yields. Its _fetch records (thread, key) in STATE["fetches"] and opens
// <local_dir>/<bucket>/<key> "wb" as s3.py does; STATE["on_fetch"](fetcher, key, file, data), if
// set, may write to the file, raise or block before the fetch writes the data.
var fetchersStandIn = map[string]string{
	"determined/__init__.py":             "",
	"determined/tensorboard/__init__.py": "",
	"determined/tensorboard/fetchers/__init__.py": `
import os
import threading
import time
import urllib.parse

lock = threading.Lock()
STATE = {}


def reset():
    STATE.clear()
    STATE.update(
        objects={},
        stray=[],
        built=[],
        build_sleep=0,
        build_error=None,
        cls="StandInFetcher",
        lister_thread=None,
        lists=[],
        list_error=None,
        fetches=[],
        on_fetch=None,
    )


reset()


class OldFetcher:
    def __init__(self, storage_config, storage_paths, local_dir):
        self.bucket = str(storage_config.get("bucket", "bucket"))
        self.local_dir = local_dir
        self.storage_paths = storage_paths
        self._file_records = {}

    def _list(self, storage_path):
        if STATE["lister_thread"] is not None:
            assert threading.current_thread().name == STATE["lister_thread"]
        with lock:
            STATE["lists"].append((storage_path, dict(self._file_records)))
        if STATE["list_error"] is not None:
            raise STATE["list_error"]
        prefix = urllib.parse.urlparse(storage_path).path.lstrip("/")
        with lock:
            items = sorted((key, obj[1]) for key, obj in STATE["objects"].items())
        for key, mtime in items:
            if not key.startswith(prefix) and key not in STATE["stray"]:
                continue
            prev = self._file_records.get(key)
            if prev is not None and prev >= mtime:
                continue
            self._file_records[key] = mtime
            yield key

    def _fetch(self, filepath, new_file_callback):
        with lock:
            STATE["fetches"].append((threading.current_thread().name, filepath))
            data = STATE["objects"][filepath][0]
        local_path = os.path.join(self.local_dir, self.bucket, filepath)
        os.makedirs(os.path.dirname(local_path), exist_ok=True)
        with open(local_path, "wb") as f:
            if STATE["on_fetch"] is not None:
                STATE["on_fetch"](self, filepath, f, data)
            f.write(data)
        new_file_callback()


class StandInFetcher(OldFetcher):
    def forget(self, filepath):
        self._file_records.pop(filepath, None)


class NoFetch:
    def __init__(self, storage_config, storage_paths, local_dir):
        pass

    def _list(self, storage_path):
        return iter(())


def build(storage_config, paths, local_dir):
    STATE["built"].append((storage_config, list(paths), local_dir))
    time.sleep(STATE["build_sleep"])
    if STATE["build_error"] is not None:
        raise STATE["build_error"]
    return globals()[STATE["cls"]](storage_config, paths, local_dir)
`,
}

// The loader's own tests, testdata/tensorboard_loader_test.py, with python3 without its site
// packages and with the stand-in determined package. Python writes no bytecode next to the
// program, which the master's static files ship.
func TestTensorBoardLoaderProgram(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	pkg := t.TempDir()
	for name, body := range fetchersStandIn {
		file := filepath.Join(pkg, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
		require.NoError(t, os.WriteFile(file, []byte(body), 0o600))
	}
	services, err := filepath.Abs("../../static/srv/services")
	require.NoError(t, err)
	test, err := filepath.Abs("testdata/tensorboard_loader_test.py")
	require.NoError(t, err)
	cmd := exec.Command(python, "-S", test) //nolint:gosec
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "PYTHONPATH=") || strings.HasPrefix(kv, "DET_TENSORBOARD_")
	}), "PYTHONPATH="+pkg+":"+services, "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	t.Log(string(out))
}
