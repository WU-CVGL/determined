"""
Tests of tensorboard_loader.py, the tensorboard unit's loader, with the stand-in
determined.tensorboard.fetchers of tensorboard_loader_test.go on the PYTHONPATH with the program's.
TestTensorBoardLoaderProgram runs them with python3 -S.
"""

import importlib
import io
import json
import os
import queue
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from typing import Any, Callable, Dict, List, Optional, Tuple
from unittest import mock

import tensorboard_loader as tl

fetchers: Any = importlib.import_module("determined.tensorboard.fetchers")

SENTINEL = "SENTINEL-SECRET-KEY"
PREFIX = "p/c1/tensorboard/"
LOADER = tl.__file__


def wait_until(what: str, cond: Callable[[], bool], timeout: float = 10) -> None:
    deadline = time.monotonic() + timeout
    while not cond():
        if time.monotonic() > deadline:
            raise AssertionError(f"timed out waiting for {what}")
        time.sleep(0.001)


def json_error(text: str) -> str:
    """The error of json.loads(text)."""
    try:
        json.loads(text)
    except ValueError as e:
        return str(e)
    raise AssertionError(f"{text!r} is JSON")


def staged_files(work: str) -> List[Tuple[str, int]]:
    """The files under a worker's staging directory, with their sizes."""
    found = []
    for d, _, files in os.walk(work):
        found += [(os.path.join(d, f), os.path.getsize(os.path.join(d, f))) for f in files]
    return found


def s3_source(run: str, prefix: str = PREFIX) -> tl.Source:
    return tl.Source(run, f"s3://bucket/{prefix}{run}/")


class Files:
    """A task's /run/determined/tensorboard: the sources, the storage config and logs/."""

    def __init__(self, root: str) -> None:
        self.root = root
        self.logdir = os.path.join(root, "logs")
        self.staging = os.path.join(root, "staging")
        self.sources_file = os.path.join(root, "sources.json")
        self.storage_file = os.path.join(root, "storage_config.json")
        os.makedirs(self.logdir)

    def write(
        self,
        storage_type: str,
        sources: List[Dict[str, Any]],
        storage: Any = None,
        **doc: Any,
    ) -> Dict[str, str]:
        if storage is None:
            storage = {"type": storage_type, "bucket": "bucket", "secret_key": SENTINEL}
        content: Dict[str, Any] = {
            "version": 1,
            "storage_type": storage_type,
            "storage_config": self.storage_file,
            "sources": sources,
        }
        content.update(doc)
        with open(self.sources_file, "w") as f:
            json.dump(content, f)
        with open(self.storage_file, "w") as f:
            json.dump(storage, f)
        return {"DET_TENSORBOARD_SOURCES": self.sources_file}


def run_loader(
    args: List[str], env: Dict[str, str], setup: str = "", timeout: float = 30
) -> "subprocess.CompletedProcess[str]":
    """Runs the loader's main with args after setup, python code with fetchers imported."""
    return subprocess.run(
        [sys.executable, "-S", "-c", loader_code(setup), *args],
        env={**os.environ, **env},
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        universal_newlines=True,
        timeout=timeout,
    )


def loader_code(setup: str) -> str:
    return (
        "import sys, threading, time\n"
        "from determined.tensorboard import fetchers\n"
        "import tensorboard_loader\n"
        f"{setup}\n"
        "sys.exit(tensorboard_loader.main(['tensorboard_loader.py'] + sys.argv[1:]))\n"
    )


class CheckTest(unittest.TestCase):
    def setUp(self) -> None:
        self.files = Files(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.files.root, True)

    def check(self, env: Dict[str, str], setup: str = "") -> Tuple[int, str]:
        p = run_loader(["--check", self.files.logdir], env, setup)
        self.assertEqual("", p.stdout)
        self.assertNotIn(SENTINEL, p.stderr)
        return p.returncode, p.stderr

    def test_check(self) -> None:
        f = self.files
        s3: List[Dict[str, Any]] = [
            {"run": "experiment/12", "experiment_id": 12, "path": "s3://bucket/c1/x/"}
        ]
        fs: List[Dict[str, Any]] = [
            {"run": "experiment/12", "experiment_id": 12, "path": "/determined_shared_fs/x/"}
        ]
        cases: List[Tuple[str, Callable[[], Dict[str, str]], str, str]] = [
            ("env unset", lambda: {}, "", "DET_TENSORBOARD_SOURCES is not set"),
            (
                "no sources file",
                lambda: {"DET_TENSORBOARD_SOURCES": f.root + "/none.json"},
                "",
                "cannot read the sources: [Errno 2] No such file or directory: "
                f"'{f.root}/none.json'",
            ),
            (
                "sources not JSON",
                lambda: self.raw(f.sources_file, "{"),
                "",
                "cannot read the sources: " + json_error("{"),
            ),
            (
                "version",
                lambda: f.write("s3", s3, version=2),
                "",
                "the sources have version 2, not 1",
            ),
            (
                "version string",
                lambda: f.write("s3", s3, version="1"),
                "",
                'the sources have version "1", not 1',
            ),
            (
                "storage_type",
                lambda: f.write("nfs", s3),
                "",
                "the sources are invalid: storage_type",
            ),
            ("no sources", lambda: f.write("s3", []), "", "the sources are invalid: sources"),
            (
                "sources not a list",
                lambda: f.write("s3", "x"),  # type: ignore[arg-type]
                "",
                "the sources are invalid: sources",
            ),
            (
                "bad run",
                lambda: f.write("s3", [{**s3[0], "run": "experiment/012"}]),
                "",
                "the sources are invalid: run experiment/012",
            ),
            (
                "run with a newline",
                lambda: f.write("s3", [{**s3[0], "run": "experiment/1\n2"}]),
                "",
                'the sources are invalid: run "experiment/1\\n2"',
            ),
            (
                "duplicate run",
                lambda: f.write("s3", s3 + s3),
                "",
                "the sources are invalid: run experiment/12",
            ),
            (
                "nested run",
                lambda: f.write(
                    "s3", s3 + [{**s3[0], "run": "experiment/12/trial/40", "path": "s3://b/y/"}]
                ),
                "",
                "the sources are invalid: run experiment/12/trial/40",
            ),
            (
                "s3 path of gcs",
                lambda: f.write("s3", [{**s3[0], "path": "gs://bucket/c1/x/"}]),
                "",
                "the sources are invalid: path of experiment/12",
            ),
            (
                "object path without a slash",
                lambda: f.write("s3", [{**s3[0], "path": "s3://bucket/c1/x"}]),
                "",
                "the sources are invalid: path of experiment/12",
            ),
            (
                "object path with ..",
                lambda: f.write("s3", [{**s3[0], "path": "s3://bucket/c1/../x/"}]),
                "",
                "the sources are invalid: path of experiment/12",
            ),
            (
                "object path with #",
                lambda: f.write("s3", [{**s3[0], "path": "s3://bucket/c1#/x/"}]),
                "",
                "the sources are invalid: path of experiment/12",
            ),
            (
                "object path with ?",
                lambda: f.write("s3", [{**s3[0], "path": "s3://bucket/c1?/x/"}]),
                "",
                "the sources are invalid: path of experiment/12",
            ),
            (
                "relative local path",
                lambda: f.write("shared_fs", [{**fs[0], "path": "determined_shared_fs/x/"}]),
                "",
                "the sources are invalid: path of experiment/12",
            ),
            (
                "unclean local path",
                lambda: f.write("directory", [{**fs[0], "path": "/a/../b/"}]),
                "",
                "the sources are invalid: path of experiment/12",
            ),
            (
                "no storage file",
                lambda: self.without(f.write("s3", s3), f.storage_file),
                "",
                "cannot read the checkpoint storage: [Errno 2] No such file or directory: "
                f"'{f.storage_file}'",
            ),
            (
                "storage not an object",
                lambda: f.write("s3", s3, storage=[SENTINEL]),
                "",
                "cannot read the checkpoint storage: not a JSON object",
            ),
            (
                "storage not JSON",
                lambda: self.raw(f.storage_file, '{"secret_key": "' + SENTINEL, f.write("s3", s3)),
                "",
                "cannot read the checkpoint storage: " + json_error('{"secret_key": "x'),
            ),
            (
                "storage of another type",
                lambda: f.write("s3", s3, storage={"type": "gcs", "secret_key": SENTINEL}),
                "",
                "the checkpoint storage is of type gcs, not s3",
            ),
            (
                "no fetchers",
                lambda: f.write("s3", s3),
                "sys.modules['determined.tensorboard.fetchers'] = None",
                "determined has no TensorBoard fetchers: import of "
                "determined.tensorboard.fetchers halted; None in sys.modules",
            ),
            (
                "other file system",
                lambda: f.write("s3", s3),
                "import os\n"
                "real_stat = os.stat\n"
                "def stat(path, *args, **kwargs):\n"
                "    r = real_stat(path, *args, **kwargs)\n"
                "    if not str(path).endswith('/logs'):\n"
                "        return r\n"
                "    return os.stat_result(tuple(r[:2]) + (r[2] + 1,) + tuple(r[3:10]))\n"
                "os.stat = stat",
                f"the log directory and {f.staging} are on different file systems",
            ),
            (
                "build fails",
                lambda: f.write(
                    "s3",
                    s3,
                    storage={
                        "type": "s3",
                        "secret_key": SENTINEL,
                        "endpoint_url": f"https://user:{SENTINEL}@minio",
                    },
                ),
                "fetchers.STATE['build_error'] = ValueError("
                f"'bad endpoint https://user:{SENTINEL}@minio, key {SENTINEL}\\nmore')",
                "s3 storage: bad endpoint https://***@minio, key ***",
            ),
            (
                "azure SAS",
                lambda: f.write(
                    "azure",
                    [{**s3[0], "path": "azure://c/c1/x/"}],
                    storage={
                        "type": "azure",
                        "container": "c",
                        "account_url": "https://a.blob/?sv=1&sig=SIGNATURE-VALUE",
                    },
                ),
                "fetchers.STATE['build_error'] = ValueError("
                "'GET https://a.blob/c?sig=SIGNATURE-VALUE&comp=list failed')",
                "azure storage: GET https://a.blob/c?sig=***&comp=list failed",
            ),
            (
                "azure connection string",
                lambda: f.write(
                    "azure",
                    [{**s3[0], "path": "azure://c/c1/x/"}],
                    storage={
                        "type": "azure",
                        "container": "c",
                        "connection_string": f"BlobEndpoint=https://a.blob/;AccountKey={SENTINEL};",
                    },
                ),
                f"fetchers.STATE['build_error'] = ValueError('bad key {SENTINEL}')",
                "azure storage: bad key ***",
            ),
            (
                "no _fetch",
                lambda: f.write("gcs", [{**s3[0], "path": "gs://bucket/c1/x/"}]),
                "fetchers.STATE['cls'] = 'NoFetch'",
                "the gcs fetcher lacks _fetch",
            ),
            (
                "slow check",
                lambda: f.write("s3", s3),
                "tensorboard_loader.CHECK_SECONDS = 1\nfetchers.STATE['build_sleep'] = 30",
                "the check took longer than 1 seconds",
            ),
        ]
        for name, write, setup, line in cases:
            with self.subTest(name):
                shutil.rmtree(f.root)
                os.makedirs(f.logdir)
                status, stderr = self.check(write(), setup)
                self.assertEqual((2, line + "\n"), (status, stderr))

        for storage_type, sources in [("s3", s3), ("shared_fs", fs), ("directory", fs)]:
            with self.subTest(storage_type):
                env = f.write(storage_type, sources)
                self.assertEqual((0, ""), self.check(env))

    def raw(self, path: str, content: str, env: Optional[Dict[str, str]] = None) -> Dict[str, str]:
        with open(path, "w") as f:
            f.write(content)
        return env or {"DET_TENSORBOARD_SOURCES": path}

    def without(self, env: Dict[str, str], path: str) -> Dict[str, str]:
        os.remove(path)
        return env

    def test_usage(self) -> None:
        usages: List[List[str]] = [[], ["--check"], ["--other", "/logs"], ["/logs", "/x"], ["logs"]]
        for args in usages:
            with self.subTest(args=args):
                p = run_loader(args, {})
                self.assertEqual(2, p.returncode)
                self.assertEqual("usage: tensorboard_loader.py [--check] LOGDIR\n", p.stderr)


class LoaderTest(unittest.TestCase):
    """An object store's loader, in this process, with one source per run."""

    def setUp(self) -> None:
        fetchers.reset()
        fetchers.STATE["lister_thread"] = "lister"
        tl.secrets[:] = []
        self.files = Files(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.files.root, True)
        patch = mock.patch.multiple(
            tl, RETRY_PAUSE=0.001, RETRY_MAX_PAUSE=0.001, LIST_SECONDS=0.05, TICK=0.05
        )
        patch.start()
        self.addCleanup(patch.stop)
        self.stderr = io.StringIO()
        stderr = mock.patch("sys.stderr", self.stderr)
        stderr.start()
        self.addCleanup(stderr.stop)

    def loader(self, sources: List[tl.Source], workers: int = 1) -> tl.Loader:
        staging = tl.staging_dir(self.files.logdir, fresh=True)
        plan = tl.Plan("s3", {"type": "s3", "bucket": "bucket"}, sources)
        lister = tl.build(fetchers, plan, staging)
        ws = [tl.build(fetchers, plan, os.path.join(staging, str(i))) for i in range(workers)]
        loader = tl.Loader(self.files.logdir, sources, lister, ws, staging)
        self.addCleanup(loader.stop)
        return loader

    def put(self, key: str, data: bytes) -> None:
        with fetchers.lock:
            objects = fetchers.STATE["objects"]
            mtime = objects[key][1] + 1 if key in objects else 1
            objects[key] = [data, mtime]

    def fetches(self, key: str) -> int:
        with fetchers.lock:
            return [k for _, k in fetchers.STATE["fetches"]].count(key)

    def listings(self) -> int:
        with fetchers.lock:
            return len(fetchers.STATE["lists"])

    def wait_listings(self, n: int) -> None:
        start = self.listings()
        wait_until(f"{n} listings", lambda: self.listings() >= start + n)

    def lines(self) -> List[str]:
        return self.stderr.getvalue().splitlines()

    def read(self, path: str) -> Optional[bytes]:
        try:
            with open(path, "rb") as f:
                return f.read()
        except FileNotFoundError:
            return None

    def test_key_validation(self) -> None:
        a = tl.Source("experiment/12", "s3://bucket/c1/tensorboard/experiment/12/")
        b = s3_source("experiment/13/trial/40")
        base = PREFIX + "experiment/13/trial/40/"
        unsafe = [
            base + "a/../../x",
            base + "/x",
            base + "./x",
            base + "\x1bx",
            base + "sub/../x",
            "other/x",
        ]
        markers = [base, base + "sub/"]
        for key in unsafe + markers:
            self.put(key, b"bad")
        fetchers.STATE["stray"] = ["other/x"]
        self.put("c1/tensorboard/experiment/12/f", b"f")
        self.put(base + "sub/f", b"g")
        loader = self.loader([a, b])
        loader.start()
        wait_until("both files", lambda: self.fetches(base + "sub/f") == 1)
        self.wait_listings(6)
        self.assertEqual(0, loader.stop())
        fetched = sorted(k for _, k in fetchers.STATE["fetches"])
        self.assertEqual(sorted(["c1/tensorboard/experiment/12/f", base + "sub/f"]), fetched)
        for key in unsafe:
            run = "experiment/12" if key == "other/x" else "experiment/13/trial/40"
            self.assertEqual(
                1, self.lines().count(f"tensorboard loader: {run}: skipped the object {key!r}")
            )
        self.assertEqual(len(unsafe), len(self.lines()), self.lines())
        published = []
        for d, _, files in os.walk(self.files.root):
            published += [os.path.relpath(os.path.join(d, f), self.files.root) for f in files]
        self.assertEqual(
            ["logs/experiment/12/f", "logs/experiment/13/trial/40/sub/f"], sorted(published)
        )
        self.assertEqual(b"g", self.read(self.files.logdir + "/experiment/13/trial/40/sub/f"))

    def test_new_file_never_lands_partial(self) -> None:
        key = PREFIX + "experiment/12/events.out.tfevents.1"
        dest = self.files.logdir + "/experiment/12/events.out.tfevents.1"
        data = b"0123456789" * 1000
        self.put(key, data)
        work = os.path.join(self.files.staging, "0")
        empty_before = []
        staged_inode = []

        def on_fetch(fetcher: Any, k: str, f: Any, d: bytes) -> None:
            empty_before.append(staged_files(work) == [(os.path.join(work, "bucket", k), 0)])
            if self.fetches(key) <= 5:
                f.write(d[: len(d) // 2])
                f.flush()
                raise OSError("storage unreachable")
            staged_inode.append(os.fstat(f.fileno()).st_ino)

        fetchers.STATE["on_fetch"] = on_fetch
        seen = []
        watching = threading.Event()

        def watch() -> None:
            while not watching.is_set():
                exists = os.path.exists(dest)
                if exists and self.fetches(key) <= 5:
                    seen.append(True)
                time.sleep(0.001)

        watcher = threading.Thread(target=watch, daemon=True)
        watcher.start()
        loader = self.loader([s3_source("experiment/12")])
        loader.start()
        wait_until("the download", lambda: self.read(dest) == data)
        watching.set()
        watcher.join()
        self.assertEqual([], seen)
        # Published by a rename: dest is the staged file.
        self.assertEqual(staged_inode, [os.stat(dest).st_ino])
        self.assertEqual([True] * 6, empty_before)
        self.assertEqual(6, self.fetches(key))
        self.assertFalse(os.path.exists(work) and os.listdir(work))
        self.assertEqual(
            [
                "tensorboard loader: experiment/12/events.out.tfevents.1: not loaded after 5 "
                "attempts: storage unreachable"
            ],
            self.lines(),
        )

    def test_growth_appends(self) -> None:
        key = PREFIX + "experiment/12/trial/40/events.out.tfevents.1"
        dest = self.files.logdir + "/experiment/12/trial/40/events.out.tfevents.1"
        v3 = bytes(range(256)) * 50
        v1, v2 = v3[:1000], v3[:7001]
        self.put(key, v1)
        loader = self.loader([s3_source("experiment/12/trial/40")])
        loader.start()
        wait_until("v1", lambda: self.read(dest) == v1)
        inode = os.stat(dest).st_ino
        with open(dest, "rb") as reader:
            reader.seek(0, os.SEEK_END)
            self.put(key, v2)
            wait_until("v2", lambda: self.read(dest) == v2)
            self.assertEqual(v2[len(v1) :], reader.read())
            self.put(key, v3)
            wait_until("v3", lambda: self.read(dest) == v3)
            self.assertEqual(v3[len(v2) :], reader.read())
        self.assertEqual(inode, os.stat(dest).st_ino)
        self.assertEqual([], self.lines())

    def test_rewrite_is_kept(self) -> None:
        key = PREFIX + "experiment/12/f"
        dest = self.files.logdir + "/experiment/12/f"
        self.put(key, b"0123456789")
        loader = self.loader([s3_source("experiment/12")])
        loader.start()
        wait_until("the file", lambda: self.read(dest) == b"0123456789")
        line = (
            "tensorboard loader: experiment/12/f: changed in storage other than by growing; "
            "the loaded copy is kept"
        )
        for n, data in enumerate([b"x123456789", b"x123456789", b"x1234567890", b"x12"]):
            fetched = self.fetches(key)
            self.put(key, data)

            def downloaded(n: int = fetched) -> bool:
                return self.fetches(key) > n

            wait_until("the download", downloaded)
            self.wait_listings(2)
            self.assertEqual(b"0123456789", self.read(dest))
            self.assertEqual([line] * (n if n else 1), self.lines())

    def test_stale_is_dropped(self) -> None:
        loader = self.loader([s3_source("experiment/12")])
        dest = self.files.logdir + "/experiment/12/f"
        pending = tl.Pending("experiment/12", "f")
        for data in [b"0123456789", b"01234"]:
            staged = os.path.join(self.files.staging, "staged")
            with open(staged, "wb") as f:
                f.write(data)
            loader.publish(staged, dest, pending)
        self.assertEqual(b"0123456789", self.read(dest))
        self.assertEqual([], self.lines())

    def test_same_version_is_noop(self) -> None:
        loader = self.loader([s3_source("experiment/12")])
        dest = self.files.logdir + "/experiment/12/f"
        pending = tl.Pending("experiment/12", "f")
        staged = os.path.join(self.files.staging, "staged")
        for i in range(2):
            with open(staged, "wb") as f:
                f.write(b"0123456789")
            loader.publish(staged, dest, pending)
            if i == 0:
                before = os.stat(dest)
                time.sleep(0.01)
        after = os.stat(dest)
        self.assertEqual((before.st_ino, before.st_mtime_ns), (after.st_ino, after.st_mtime_ns))
        self.assertEqual(b"0123456789", self.read(dest))

    def test_dedup(self) -> None:
        key = PREFIX + "experiment/12/f"
        dest = self.files.logdir + "/experiment/12/f"
        loader = self.loader([s3_source("experiment/12")])
        recording = RecordingQueue(loader)
        loader.queue = recording
        self.put(key, b"v1")
        # Yielded twice while queued: queued once.
        loader.offer("experiment/12", PREFIX + "experiment/12/", key)
        loader.offer("experiment/12", PREFIX + "experiment/12/", key)
        self.assertEqual(1, recording.qsize())
        # Yielded while running: fetched once more after, at the newer version.
        release = threading.Event()

        def on_fetch(fetcher: Any, k: str, f: Any, d: bytes) -> None:
            if self.fetches(key) == 1:
                release.wait(10)

        fetchers.STATE["on_fetch"] = on_fetch
        loader.start()
        wait_until("the first fetch", lambda: self.fetches(key) == 1)
        self.put(key, b"v1v2")
        self.wait_listings(3)
        self.assertTrue(loader.pending[key].again)
        release.set()
        wait_until("v2", lambda: self.read(dest) == b"v1v2")
        self.wait_listings(3)
        self.assertEqual(2, self.fetches(key))
        self.assertEqual(2, recording.puts)
        self.assertFalse(recording.doubled)

    def test_old_fetcher_without_forget(self) -> None:
        fetchers.STATE["cls"] = "OldFetcher"
        self.assertFalse(hasattr(fetchers.OldFetcher, "forget"))
        self.five_attempts_next_listing_and_recovery()

    def test_five_attempts_next_listing_and_recovery(self) -> None:
        self.five_attempts_next_listing_and_recovery()

    def five_attempts_next_listing_and_recovery(self) -> None:
        self.assertEqual(5, tl.ATTEMPTS)
        base = PREFIX + "experiment/12/trial/40/"
        a, b = base + "events.out.tfevents.a", base + "events.out.tfevents.b"
        dest_a = self.files.logdir + "/experiment/12/trial/40/events.out.tfevents.a"
        dest_b = self.files.logdir + "/experiment/12/trial/40/events.out.tfevents.b"
        data_a, data_b = b"a" * 4096, b"b" * 4096
        self.put(a, data_a)
        self.put(b, data_b)
        staging_empty = []
        listings_at = []  # The number of listings started at each fetch of a.
        staged_inode = {}  # type: Dict[str, int]

        def on_fetch(fetcher: Any, k: str, f: Any, d: bytes) -> None:
            local = os.path.join(fetcher.local_dir, "bucket", k)
            staging_empty.append(staged_files(fetcher.local_dir) == [(local, 0)])
            if k == a:
                listings_at.append(self.listings())
            if k == a and self.fetches(a) <= 7:
                f.write(d[: len(d) // 2])
                f.flush()
                raise OSError("storage unreachable")
            staged_inode[k] = os.fstat(f.fileno()).st_ino

        fetchers.STATE["on_fetch"] = on_fetch
        fetched_at_line = []
        real_log = tl.log

        def log(line: str) -> None:
            if "not loaded" in line:
                fetched_at_line.append(self.fetches(a))
            real_log(line)

        patch = mock.patch.object(tl, "log", log)
        patch.start()
        self.addCleanup(patch.stop)
        seen = []
        watching = threading.Event()

        def watch() -> None:
            while not watching.is_set():
                exists = os.path.exists(dest_a)
                if exists and self.fetches(a) < 8:
                    seen.append(True)
                time.sleep(0.001)

        watcher = threading.Thread(target=watch, daemon=True)
        watcher.start()
        loader = self.loader([s3_source("experiment/12/trial/40")], workers=2)
        loader.start()

        # Cycle 1: exactly five attempts on a, one line, b published whole.
        wait_until("five attempts", lambda: len(listings_at) >= 5)
        line = (
            "tensorboard loader: experiment/12/trial/40/events.out.tfevents.a: "
            "not loaded after 5 attempts: storage unreachable"
        )
        wait_until("the line", lambda: line in self.lines())
        wait_until("b", lambda: self.read(dest_b) == data_b)
        self.assertEqual([5], fetched_at_line)
        self.assertEqual(staged_inode[b], os.stat(dest_b).st_ino)

        # Cycle 2: a is forgotten before the listing, listed again and loaded on attempt 3.
        wait_until("a", lambda: self.read(dest_a) == data_a)
        watching.set()
        watcher.join()
        self.assertEqual([], seen)
        self.assertEqual(8, self.fetches(a))
        self.assertEqual(staged_inode[a], os.stat(dest_a).st_ino)
        with fetchers.lock:
            lists = list(fetchers.STATE["lists"])
        # The first listing after a was recorded whose records lack a, and its index.
        forgotten = [
            (i, records)
            for i, (_, records) in enumerate(lists)
            if i >= listings_at[0] and a not in records
        ]
        self.assertTrue(forgotten)
        i, records = forgotten[0]
        self.assertIn(b, records)
        # Attempts 1 to 5 come before that listing, and a succeeds on attempt 3 of the next
        # dequeue, 8 in total.
        self.assertTrue(all(n <= i for n in listings_at[:5]), (i, listings_at))
        self.assertTrue(all(n > i for n in listings_at[5:]), (i, listings_at))
        self.assertEqual(8, len(listings_at))

        # Cycle 3: nothing more to fetch.
        self.wait_listings(3)
        self.assertEqual((8, 1), (self.fetches(a), self.fetches(b)))
        self.assertEqual([line], self.lines())
        self.assertTrue(all(staging_empty), staging_empty)
        for i in range(2):
            work = os.path.join(self.files.staging, str(i))
            self.assertFalse(os.path.exists(work) and os.listdir(work))

        started = time.monotonic()
        self.assertEqual(0, loader.stop())
        self.assertLess(time.monotonic() - started, tl.STOP_SECONDS)
        self.assertFalse(os.path.exists(self.files.staging))

    def test_listing_failure_logged_once(self) -> None:
        fetchers.STATE["list_error"] = OSError("storage unreachable\nmore")
        loader = self.loader([s3_source("experiment/12")])
        loader.start()
        self.wait_listings(3)
        fetchers.STATE["list_error"] = None
        self.wait_listings(3)
        self.assertEqual(
            [
                "tensorboard loader: experiment/12: listing failed: storage unreachable",
                "tensorboard loader: experiment/12: listing works again",
            ],
            self.lines(),
        )

    def test_errors_are_scrubbed(self) -> None:
        account_key = "ACCOUNT-KEY-VALUE"
        env = self.files.write(
            "s3",
            [{"run": "experiment/12", "experiment_id": 12, "path": "s3://bucket/c1/x/"}],
            storage={
                "type": "s3",
                "bucket": "bucket",
                "secret_key": SENTINEL,
                "connection_string": f"BlobEndpoint=https://a.blob/;AccountKey={account_key};",
            },
        )
        tl.read_plan(env)
        key = PREFIX + "experiment/12/f"
        fetchers.STATE["list_error"] = OSError(f"denied: {SENTINEL} {account_key}")
        loader = self.loader([s3_source("experiment/12")])
        loader.start()
        self.wait_listings(2)
        fetchers.STATE["list_error"] = None

        def fail(*_: Any) -> None:
            if self.fetches(key) <= 5:
                raise OSError(f"bad key {account_key}, secret {SENTINEL}")

        fetchers.STATE["on_fetch"] = fail
        self.put(key, b"data")
        wait_until("the file", lambda: self.read(self.files.logdir + "/experiment/12/f") == b"data")
        self.assertEqual(
            [
                "tensorboard loader: experiment/12: listing failed: denied: *** ***",
                "tensorboard loader: experiment/12: listing works again",
                "tensorboard loader: experiment/12/f: not loaded after 5 attempts: "
                "bad key ***, secret ***",
            ],
            self.lines(),
        )

    def test_stop_order(self) -> None:
        key = PREFIX + "experiment/12/f"
        dest = self.files.logdir + "/experiment/12/f"
        self.put(key, b"data")
        release = threading.Event()
        fetchers.STATE["on_fetch"] = lambda *_: release.wait(30)
        loader = self.loader([s3_source("experiment/12")])
        recording = RecordingQueue(loader)
        loader.queue = recording
        events: List[str] = []
        real_set = loader.stop_event.set

        def set_stop() -> None:
            events.append("stop_event set")
            real_set()

        loader.stop_event.set = set_stop  # type: ignore[assignment]
        loader.publish_lock = RecordingLock(loader, events)  # type: ignore[assignment]
        loader.start()
        for t in loader.threads:
            t.join = recording_join(t, events)  # type: ignore[assignment]
        wait_until("the fetch", lambda: self.fetches(key) == 1)
        real_log, real_rmtree = tl.log, shutil.rmtree

        def log(line: str) -> None:
            events.append("line: " + line)
            real_log(line)

        def rmtree(path: str, *args: Any, **kwargs: Any) -> None:
            if path == self.files.staging:
                events.append("staging removed")
            real_rmtree(path, *args, **kwargs)

        with mock.patch.object(tl, "STOP_SECONDS", 0.5), mock.patch.object(
            tl, "log", log
        ), mock.patch.object(shutil, "rmtree", rmtree):
            started = time.monotonic()
            self.assertEqual(1, loader.stop())
            self.assertLess(time.monotonic() - started, 1)
        self.assertEqual(
            [
                "stop_event set",
                "publishing closed",
                "joined lister",
                "joined worker-0",
                "line: 1 downloads were still running at exit",
                "staging removed",
            ],
            events,
        )
        self.assertFalse(os.path.exists(self.files.staging))
        release.set()
        loader.threads[1].join(5)
        self.assertFalse(loader.threads[1].is_alive())
        self.assertFalse(os.path.exists(dest))
        self.assertFalse(os.path.exists(self.files.staging))
        self.assertEqual(
            ["tensorboard loader: 1 downloads were still running at exit"], self.lines()
        )
        self.assertFalse(recording.after_stop)
        # No publish after the stop.
        staged = os.path.join(self.files.root, "staged")
        with open(staged, "wb") as f:
            f.write(b"data")
        loader.publish(staged, dest, tl.Pending("experiment/12", "f"))
        self.assertFalse(os.path.exists(dest))

    def test_lister_stops_between_keys(self) -> None:
        keys = [PREFIX + f"experiment/12/f{n}" for n in range(4)]
        for key in keys:
            self.put(key, b"data")
        reached, release = threading.Event(), threading.Event()

        def after_yield(key: str) -> None:
            if not reached.is_set():
                reached.set()
                release.wait(10)

        fetchers.STATE["after_yield"] = after_yield
        loader = self.loader([s3_source("experiment/12")])
        recording = RecordingQueue(loader)
        loader.queue = recording
        loader.start()
        wait_until("the first key", reached.is_set)
        stopped: List[int] = []
        with mock.patch.object(tl, "STOP_SECONDS", 5):
            stopping = threading.Thread(target=lambda: stopped.append(loader.stop()))
            stopping.start()
            wait_until("the stop", loader.stop_event.is_set)
            release.set()
            stopping.join(10)
        self.assertEqual([0], stopped)
        self.assertEqual(1, recording.puts)
        self.assertFalse(recording.after_stop)
        self.assertEqual([], [k for k in keys[1:] if k in loader.pending])
        self.assertEqual([], [k for _, k in fetchers.STATE["fetches"] if k != keys[0]])

    def test_stop_in_a_pause(self) -> None:
        key = PREFIX + "experiment/12/f"
        self.put(key, b"data")

        def fail(*_: Any) -> None:
            raise OSError("storage unreachable")

        fetchers.STATE["on_fetch"] = fail
        loader = self.loader([s3_source("experiment/12")])
        with mock.patch.multiple(tl, RETRY_PAUSE=60, RETRY_MAX_PAUSE=60, LIST_SECONDS=60):
            loader.start()
            wait_until("the first attempt", lambda: self.fetches(key) == 1)
            time.sleep(0.1)
            started = time.monotonic()
            self.assertEqual(0, loader.stop())
            self.assertLess(time.monotonic() - started, 1)
        self.assertEqual(1, self.fetches(key))
        self.assertEqual([], self.lines())


class RecordingQueue(queue.Queue):  # type: ignore[type-arg]
    """A queue that records its puts and whether a key was ever in it twice, or put after stop."""

    def __init__(self, loader: tl.Loader) -> None:
        super().__init__()
        self.loader = loader
        self.puts = 0
        self.doubled = False
        self.after_stop = False

    def put(self, item: Any, block: bool = True, timeout: Optional[float] = None) -> None:
        with self.mutex:
            self.doubled = self.doubled or item in self.queue
        self.puts += 1
        self.after_stop = self.after_stop or self.loader.stop_event.is_set()
        super().put(item, block, timeout)


class RecordingLock:
    """The publish lock, which records when publishing has been closed."""

    def __init__(self, loader: tl.Loader, events: List[str]) -> None:
        self.lock = threading.Lock()
        self.loader = loader
        self.events = events

    def __enter__(self) -> None:
        self.lock.acquire()

    def __exit__(self, *_: Any) -> None:
        if self.loader.publishing_closed and "publishing closed" not in self.events:
            self.events.append("publishing closed")
        self.lock.release()


def recording_join(t: threading.Thread, events: List[str]) -> Callable[..., None]:
    real_join = t.join

    def join(timeout: Optional[float] = None) -> None:
        assert timeout is not None, "every join has a timeout"
        real_join(timeout)
        events.append("joined " + t.name)

    return join


class RunTest(unittest.TestCase):
    """The loader as the unit runs it, in the background."""

    def setUp(self) -> None:
        self.files = Files(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.files.root, True)

    def test_local_sources_are_linked(self) -> None:
        f = self.files
        source = os.path.join(f.root, "fs", "c1", "tensorboard", "experiment", "12")
        os.makedirs(os.path.join(source, "trial", "40"))
        with open(os.path.join(source, "trial", "40", "events.out.tfevents.1"), "wb") as e:
            e.write(b"events")
        missing = os.path.join(f.root, "fs", "c1", "tensorboard", "experiment", "15")
        trial = os.path.join(f.root, "fs", "c1", "tensorboard", "experiment", "13", "trial", "40")
        os.makedirs(trial)
        os.makedirs(f.logdir + "/experiment/13/trial")
        os.symlink("/elsewhere", f.logdir + "/experiment/13/trial/40")
        nested = os.path.join(f.root, "fs", "c1", "tensorboard", "experiment", "14", "trial", "41")
        os.makedirs(nested)
        for d, _, _ in os.walk(os.path.join(f.root, "fs")):
            os.chmod(d, 0o555)

        def restore() -> None:
            for d, _, _ in os.walk(f.root):
                os.chmod(d, 0o755)

        self.addCleanup(restore)
        tree = sorted((d, sorted(fs)) for d, _, fs in os.walk(os.path.join(f.root, "fs")))
        sources = [
            {"run": "experiment/12", "experiment_id": 12, "path": source + "/"},
            {"run": "experiment/13/trial/40", "experiment_id": 13, "path": trial + "/"},
            {"run": "experiment/15", "experiment_id": 15, "path": missing + "/"},
            {"run": "experiment/14/trial/41", "experiment_id": 14, "path": nested + "/"},
        ]
        for storage_type in ["shared_fs", "directory"]:
            with self.subTest(storage_type):
                env = f.write(storage_type, sources, storage={"type": storage_type})
                for _ in range(2):
                    p = run_loader(
                        [f.logdir],
                        env,
                        "fetchers.STATE['build_error'] = AssertionError('build was called')",
                    )
                    self.assertEqual((0, "", ""), (p.returncode, p.stdout, p.stderr))
                    self.assertEqual(source, os.readlink(f.logdir + "/experiment/12"))
                    self.assertEqual(
                        "/elsewhere", os.readlink(f.logdir + "/experiment/13/trial/40")
                    )
                    self.assertEqual(missing, os.readlink(f.logdir + "/experiment/15"))
                    self.assertEqual(nested, os.readlink(f.logdir + "/experiment/14/trial/41"))
                    self.assertFalse(os.path.exists(f.logdir + "/experiment/15"))
                    self.assertFalse(os.path.exists(f.staging))
                os.remove(f.logdir + "/experiment/12")
                os.remove(f.logdir + "/experiment/15")
                shutil.rmtree(f.logdir + "/experiment/14")
        self.assertEqual(
            tree, sorted((d, sorted(fs)) for d, _, fs in os.walk(os.path.join(f.root, "fs")))
        )

    def test_signal_exits(self) -> None:
        f = self.files
        env = f.write(
            "s3", [{"run": "experiment/12", "experiment_id": 12, "path": "s3://bucket/c1/x/"}]
        )
        started = os.path.join(f.root, "fetch-started")
        setup = (
            "tensorboard_loader.STOP_SECONDS = 1\n"
            "tensorboard_loader.TICK = 0.05\n"
            "fetchers.STATE['objects']['c1/x/events.out.tfevents.1'] = [b'data', 1]\n"
            "def on_fetch(*_):\n"
            f"    open({started!r}, 'w').close()\n"
            "    threading.Event().wait()\n"
            "fetchers.STATE['on_fetch'] = on_fetch\n"
        )
        for signum in [signal.SIGTERM, signal.SIGINT, signal.SIGHUP]:
            with self.subTest(signal=signum):
                p = subprocess.Popen(
                    [sys.executable, "-S", "-c", loader_code(setup), f.logdir],
                    env={**os.environ, **env},
                    stderr=subprocess.PIPE,
                    universal_newlines=True,
                )
                wait_until("the fetch", lambda: os.path.exists(started))
                self.assertTrue(os.path.isdir(f.staging))
                sent = time.monotonic()
                p.send_signal(signum)
                _, stderr = p.communicate(timeout=30)
                self.assertLess(time.monotonic() - sent, 1 + 1)
                self.assertEqual(0, p.returncode, stderr)
                self.assertEqual(
                    "tensorboard loader: loading 1 sources from s3 storage\n"
                    "tensorboard loader: 1 downloads were still running at exit\n",
                    stderr,
                )
                self.assertFalse(os.path.exists(f.staging))
                self.assertEqual([], os.listdir(f.logdir))
                os.remove(started)


if __name__ == "__main__":
    unittest.main(verbosity=2)
