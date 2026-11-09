"""
Checks that the publish rules of the tensorboard service's loader keep TensorBoard's reading
correct. The loader, a program that the master ships, is loaded by path; it publishes the objects
of a stand-in fetcher, and TensorBoard's own multiplexer reads its log directory as the service
does with --reload_multifile=true: the real TensorFlow reader in this process, and the stub reader
in a subprocess.
"""

import base64
import datetime
import importlib.util
import json
import os
import pathlib
import queue
import subprocess
import sys
import threading
import time
import urllib.parse
from typing import Any, Callable, Dict, Generator, List, NamedTuple, Optional, Tuple

import pytest
from tensorboard.backend.event_processing import plugin_event_multiplexer
from tensorboard.util import tensor_util

from determined.tensorboard.fetchers import base

LOADER_PATH = (
    pathlib.Path(__file__).resolve().parents[3] / "master/static/srv/services/tensorboard_loader.py"
)


def load_loader() -> Any:
    spec = importlib.util.spec_from_file_location("tensorboard_loader", LOADER_PATH)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)  # type: ignore
    return module


tbl = load_loader()

BUCKET = "bucket"
RUN = "experiment/12/trial/40"
PREFIX = f"p/c1/tensorboard/{RUN}/"
PATH = f"s3://{BUCKET}/{PREFIX}"
TAG = "loss"
EPOCH = datetime.datetime(2026, 1, 1)


class Events(NamedTuple):
    """An event file's bytes, and the byte length of the file after each step."""

    data: bytes
    ends: List[int]

    def through(self, steps: int) -> bytes:
        """The file as a writer leaves it after steps steps."""
        return self.data[: self.ends[steps - 1]]


def write_events(directory: pathlib.Path, steps: List[int], scale: float = 1.0) -> Events:
    """Writes the scalar TAG at each step with TensorFlow's summary writer."""
    tf = pytest.importorskip("tensorflow")
    directory.mkdir(parents=True)
    writer = tf.summary.create_file_writer(str(directory))
    ends = []
    with writer.as_default():
        writer.flush()
        (path,) = directory.iterdir()
        for step in steps:
            tf.summary.scalar(TAG, step * scale, step=step)
            writer.flush()
            ends.append(path.stat().st_size)
    writer.close()
    return Events(path.read_bytes(), ends)


class Store:
    """The objects of a stand-in bucket: their bytes and modification times."""

    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.objects: Dict[str, Tuple[bytes, datetime.datetime]] = {}
        self.clock = 0
        self.failures: Dict[str, int] = {}
        self.fetches: List[str] = []

    def put(self, key: str, data: bytes) -> None:
        with self.lock:
            self.clock += 1
            self.objects[key] = (data, EPOCH + datetime.timedelta(seconds=self.clock))


class StandInFetcher(base.Fetcher):
    """
    A fetcher of the Store, which lists like the S3 fetcher and downloads to
    local_dir/<bucket>/<key>. A key with failures left fails after writing half of its bytes.
    """

    def __init__(self, store: Store, local_dir: str) -> None:
        self.store = store
        self.local_dir = local_dir
        self.storage_paths: List[str] = []
        self._file_records: Dict[str, datetime.datetime] = {}

    def _list(self, storage_path: str) -> Generator[str, None, None]:
        prefix = urllib.parse.urlparse(storage_path).path.lstrip("/")
        with self.store.lock:
            objects = sorted(self.store.objects.items())
        for key, (_, mtime) in objects:
            if not key.startswith(prefix):
                continue
            recorded = self._file_records.get(key)
            if recorded is not None and recorded >= mtime:
                continue
            self._file_records[key] = mtime
            yield key

    def _fetch(self, filepath: str, new_file_callback: Callable) -> None:
        with self.store.lock:
            data = self.store.objects[filepath][0]
            fail = self.store.failures.get(filepath, 0) > 0
            if fail:
                self.store.failures[filepath] -= 1
            self.store.fetches.append(filepath)
        local_path = os.path.join(self.local_dir, BUCKET, filepath)
        os.makedirs(os.path.dirname(local_path), exist_ok=True)
        with open(local_path, "wb") as f:
            if fail:
                f.write(data[: len(data) // 2])
                f.flush()
                raise ConnectionError("storage unreachable")
            f.write(data)
        new_file_callback()


def multiplexer(
    active_filter: Optional[Callable[[float], bool]]
) -> plugin_event_multiplexer.EventMultiplexer:
    """
    TensorBoard's multiplexer, which keeps every step; with a filter, it reads with
    DirectoryLoader.
    """
    return plugin_event_multiplexer.EventMultiplexer(
        size_guidance={"tensors": 0},
        tensor_size_guidance={"scalars": 0},
        event_file_active_filter=active_filter,
    )


def reload_multifile(t: float) -> bool:
    """--reload_multifile=true with the default --reload_multifile_inactive_secs."""
    return t + 86400 >= time.time()


def read(
    mux: plugin_event_multiplexer.EventMultiplexer, logdir: pathlib.Path
) -> Dict[str, List[Tuple[int, float]]]:
    """One reload as TensorBoard's reload thread does it: the (step, value) pairs of each run."""
    mux.AddRunsFromDirectory(str(logdir))
    mux.Reload()
    return {
        run: [
            (e.step, float(tensor_util.make_ndarray(e.tensor_proto).item()))
            for e in mux.Tensors(run, TAG)
        ]
        for run, tags in mux.Runs().items()
        if TAG in tags["tensors"]
    }


def steps(read_result: Dict[str, List[Tuple[int, float]]]) -> Dict[str, List[int]]:
    return {run: [step for step, _ in pairs] for run, pairs in read_result.items()}


class Session:
    """A loader for one object store source, with the log directory layout of the service."""

    def __init__(self, tmp: pathlib.Path) -> None:
        self.logdir = tmp / "tensorboard" / "logs"
        self.logdir.mkdir(parents=True)
        self.staging = tbl.staging_dir(str(self.logdir), fresh=True)
        self.store = Store()
        self.lister = StandInFetcher(self.store, self.staging)
        self.worker = StandInFetcher(self.store, os.path.join(self.staging, "0"))
        self.loader = tbl.Loader(
            str(self.logdir), [tbl.Source(RUN, PATH)], self.lister, [self.worker], self.staging
        )
        self.mux = multiplexer(reload_multifile)

    def dest(self, name: str) -> pathlib.Path:
        return self.logdir / RUN / name

    def cycle(self) -> None:
        """One listing, then every queued download, in this thread."""
        self.loader.list_once()
        while True:
            try:
                key = self.loader.queue.get_nowait()
            except queue.Empty:
                return
            self.loader.load(0, key)

    def read(self) -> Dict[str, List[Tuple[int, float]]]:
        return read(self.mux, self.logdir)


def growth_case(tmp: pathlib.Path, events: Events) -> None:
    """A growing object is read in full, each step once, from one published file."""
    session = Session(tmp)
    inode = None
    for n in (3, 6, 9):
        session.store.put(PREFIX + "events.out.tfevents.a", events.through(n))
        session.cycle()
        assert steps(session.read()) == {RUN: list(range(1, n + 1))}
        st = session.dest("events.out.tfevents.a").stat()
        assert inode in (None, st.st_ino)
        inode = st.st_ino
    assert set(session.mux.Runs()) == {RUN}


def cut_case(tmp: pathlib.Path, events: Events, cut: int) -> None:
    """An object that ends inside a record is read up to it, then on from it once it grows."""
    session = Session(tmp)
    key = PREFIX + "events.out.tfevents.a"
    session.store.put(key, events.through(3))
    session.cycle()
    assert steps(session.read()) == {RUN: [1, 2, 3]}
    session.store.put(key, events.data[: events.ends[2] + cut])
    session.cycle()
    assert steps(session.read()) == {RUN: [1, 2, 3]}
    session.store.put(key, events.through(6))
    session.cycle()
    assert steps(session.read()) == {RUN: [1, 2, 3, 4, 5, 6]}
    session.store.put(key, events.data)
    session.cycle()
    assert steps(session.read()) == {RUN: list(range(1, len(events.ends) + 1))}


def rewrite_case(tmp: pathlib.Path, events: Events, rewritten: Events) -> None:
    """An object rewritten other than by growing leaves the steps that were read unchanged."""
    session = Session(tmp)
    key = PREFIX + "events.out.tfevents.a"
    session.store.put(key, events.through(3))
    session.cycle()
    before = session.read()
    assert steps(before) == {RUN: [1, 2, 3]}
    published = session.dest("events.out.tfevents.a").read_bytes()
    for n in (3, 6, 2):
        session.store.put(key, rewritten.through(n))
        session.cycle()
        assert session.read() == before
        assert session.dest("events.out.tfevents.a").read_bytes() == published


def test_new_file_failing_then_succeeding(
    tmp_path: pathlib.Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.setattr(tbl, "RETRY_PAUSE", 0.0)
    monkeypatch.setattr(tbl, "RETRY_MAX_PAUSE", 0.0)
    events = write_events(tmp_path / "writer", list(range(1, 6)))
    session = Session(tmp_path)
    key = PREFIX + "events.out.tfevents.a"
    session.store.put(key, events.data)
    session.store.failures[key] = 5

    session.cycle()
    assert session.store.fetches == [key] * 5
    assert session.read() == {}
    assert not (session.logdir / "experiment").exists()
    assert os.listdir(session.staging) == []
    err = capsys.readouterr().err
    assert err == (
        f"tensorboard loader: {RUN}/events.out.tfevents.a: not loaded after 5 attempts: "
        "storage unreachable\n"
    )

    session.cycle()
    assert steps(session.read()) == {RUN: [1, 2, 3, 4, 5]}
    assert session.dest("events.out.tfevents.a").read_bytes() == events.data
    assert capsys.readouterr().err == ""


def test_growth(tmp_path: pathlib.Path) -> None:
    growth_case(tmp_path, write_events(tmp_path / "writer", list(range(1, 10))))


@pytest.mark.parametrize("cut", [5, 13, 30])
def test_append_cut_mid_record(tmp_path: pathlib.Path, cut: int) -> None:
    events = write_events(tmp_path / "writer", list(range(1, 10)))
    assert events.ends[2] + cut < events.ends[3]
    cut_case(tmp_path, events, cut)


def test_rewritten_file_is_kept(tmp_path: pathlib.Path, capsys: pytest.CaptureFixture[str]) -> None:
    events = write_events(tmp_path / "writer", list(range(1, 7)))
    rewritten = write_events(tmp_path / "rewriter", list(range(1, 7)), scale=2.0)
    assert rewritten.ends == events.ends
    rewrite_case(tmp_path, events, rewritten)
    changed = (
        f"tensorboard loader: {RUN}/events.out.tfevents.a: changed in storage other than by "
        "growing; the loaded copy is kept"
    )
    assert capsys.readouterr().err.splitlines() == [changed] * 3


def test_later_named_file_first(tmp_path: pathlib.Path) -> None:
    # The trainer wrote a, then b; b is published first.
    a = write_events(tmp_path / "writer-a", list(range(1, 6)))
    b = write_events(tmp_path / "writer-b", list(range(6, 11)))
    session = Session(tmp_path)
    watcher = multiplexer(None)
    session.store.put(PREFIX + "events.out.tfevents.b", b.data)
    session.cycle()
    assert steps(session.read()) == {RUN: [6, 7, 8, 9, 10]}
    assert steps(read(watcher, session.logdir)) == {RUN: [6, 7, 8, 9, 10]}

    session.store.put(PREFIX + "events.out.tfevents.a", a.data)
    session.cycle()
    assert sorted(steps(session.read())[RUN]) == list(range(1, 11))
    assert set(session.mux.Runs()) == {RUN}
    # Without --reload_multifile, TensorBoard never reads a file that sorts before one it read.
    assert steps(read(watcher, session.logdir)) == {RUN: [6, 7, 8, 9, 10]}


def test_shared_fs_source_is_read_through_its_link(tmp_path: pathlib.Path) -> None:
    shared = tmp_path / "shared" / "c1" / "tensorboard"
    write_events(shared / "experiment/12/trial/40", [1, 2])
    write_events(shared / "experiment/12/trial/40/sub", [3])
    storage = tmp_path / "storage_config.json"
    storage.write_text(json.dumps({"type": "shared_fs", "host_path": str(tmp_path / "shared")}))
    sources = tmp_path / "sources.json"
    sources.write_text(
        json.dumps(
            {
                "version": 1,
                "storage_type": "shared_fs",
                "storage_config": str(storage),
                "sources": [
                    {
                        "run": run,
                        "experiment_id": int(run.split("/")[1]),
                        "path": f"{shared / run}/",
                    }
                    for run in ("experiment/12/trial/40", "experiment/13")
                ],
            }
        )
    )
    logdir = tmp_path / "tensorboard" / "logs"
    logdir.mkdir(parents=True)
    plan = tbl.read_plan({"DET_TENSORBOARD_SOURCES": str(sources)})
    tbl.link(str(logdir), plan.sources)
    assert (logdir / "experiment/12/trial/40").is_symlink()
    # experiment/13 has written nothing yet: its link dangles.
    assert not (logdir / "experiment/13").exists()

    mux = multiplexer(reload_multifile)
    assert steps(read(mux, logdir)) == {
        "experiment/12/trial/40": [1, 2],
        "experiment/12/trial/40/sub": [3],
    }
    write_events(shared / "experiment/13/trial/41", [4])
    assert steps(read(mux, logdir)) == {
        "experiment/12/trial/40": [1, 2],
        "experiment/12/trial/40/sub": [3],
        "experiment/13/trial/41": [4],
    }


def wait_until(predicate: Callable[[], bool], timeout: float = 30.0) -> None:
    deadline = time.monotonic() + timeout
    while not predicate():
        assert time.monotonic() < deadline, "timed out"
        time.sleep(0.01)


def test_five_attempts_next_listing_and_recovery(
    tmp_path: pathlib.Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    # The loader's threads publish while TensorBoard reloads.
    monkeypatch.setattr(tbl, "RETRY_PAUSE", 0.001)
    monkeypatch.setattr(tbl, "RETRY_MAX_PAUSE", 0.001)
    monkeypatch.setattr(tbl, "LIST_SECONDS", 0.05)
    monkeypatch.setattr(tbl, "TICK", 0.05)
    a = write_events(tmp_path / "writer-a", list(range(1, 6)))
    b = write_events(tmp_path / "writer-b", list(range(6, 11)))
    session = Session(tmp_path)
    key_a = PREFIX + "events.out.tfevents.a"
    session.store.put(key_a, a.data)
    session.store.put(PREFIX + "events.out.tfevents.b", b.data)
    session.store.failures[key_a] = 7

    seen: List[Dict[str, List[int]]] = []

    def read_all(n: int) -> bool:
        seen.append(steps(session.read()))
        return len(seen[-1].get(RUN, [])) == n

    session.loader.start()
    try:
        wait_until(lambda: read_all(10))
    finally:
        alive = session.loader.stop()
    assert alive == 0
    assert not os.path.exists(session.staging)
    assert session.store.fetches.count(key_a) == 8
    assert sorted(seen[-1][RUN]) == list(range(1, 11))
    assert session.dest("events.out.tfevents.a").read_bytes() == a.data
    # Every read showed each step at most once, and a's steps only once it was whole.
    for result in seen:
        run_steps = result.get(RUN, [])
        assert len(run_steps) == len(set(run_steps))
        assert set(run_steps) & {1, 2, 3, 4, 5} in (set(), {1, 2, 3, 4, 5})
    assert set(session.mux.Runs()) == {RUN}
    assert capsys.readouterr().err.splitlines() == [
        f"tensorboard loader: {RUN}/events.out.tfevents.a: not loaded after 5 attempts: "
        "storage unreachable"
    ]


def test_reader_is_tensorflow() -> None:
    pytest.importorskip("tensorflow")
    import tensorboard.compat

    assert tensorboard.compat.tf.__version__ != "stub"


STUB_READER = """
import importlib.util, sys, types
sys.modules["tensorboard.compat.notf"] = types.ModuleType("tensorboard.compat.notf")
spec = importlib.util.spec_from_file_location("stub_reader_cases", sys.argv[1])
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
module.stub_cases(sys.argv[2])
"""


def save_events(path: pathlib.Path, events: Events) -> None:
    data = base64.b64encode(events.data).decode()
    path.write_text(json.dumps({"data": data, "ends": events.ends}))


def load_events(path: pathlib.Path) -> Events:
    doc = json.loads(path.read_text())
    return Events(base64.b64decode(doc["data"]), doc["ends"])


def stub_cases(directory: str) -> None:
    """The growth, cut and rewrite cases with TensorBoard's stub reader, in a subprocess."""
    import tensorboard.compat

    assert tensorboard.compat.tf.__version__ == "stub"
    tmp = pathlib.Path(directory)
    events = load_events(tmp / "events.json")
    growth_case(tmp / "growth", events)
    for cut in (5, 13, 30):
        cut_case(tmp / f"cut-{cut}", events, cut)
    rewrite_case(tmp / "rewrite", events, load_events(tmp / "rewritten.json"))
    assert "tensorflow" not in sys.modules


def test_stub_reader(tmp_path: pathlib.Path) -> None:
    save_events(tmp_path / "events.json", write_events(tmp_path / "writer", list(range(1, 10))))
    save_events(
        tmp_path / "rewritten.json",
        write_events(tmp_path / "rewriter", list(range(1, 10)), scale=2.0),
    )
    proc = subprocess.run(
        [sys.executable, "-c", STUB_READER, __file__, str(tmp_path)],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        universal_newlines=True,
        timeout=300,
    )
    assert proc.returncode == 0, proc.stderr
    changed = [line for line in proc.stderr.splitlines() if "changed in storage" in line]
    assert len(changed) == 3
