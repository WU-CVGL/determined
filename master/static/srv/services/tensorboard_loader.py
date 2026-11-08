"""
tensorboard_loader.py publishes the TensorBoard logs of the experiments and trials of a shell's
tensorboard service in the service's log directory, LOGDIR, which TensorBoard reads. The unit
tensorboard.sh runs it with the task's python, first with --check, then in the background:

    tensorboard_loader.py [--check] LOGDIR

The sources are the JSON file DET_TENSORBOARD_SOURCES, {version, storage_type, storage_config,
sources: [{run, experiment_id, trial_id, path}]}, and the checkpoint storage is the JSON file that
its storage_config names.

- shared_fs and directory storage are read in place: the loader links each source's directory at
  LOGDIR/<run> and exits.
- s3, gcs and azure storage are listed every LIST_SECONDS by determined.tensorboard.fetchers, and
  each new or changed object is downloaded, with up to ATTEMPTS attempts, by one of WORKERS
  workers into its own staging directory beside LOGDIR, on the same file system. A new file is
  published by a rename, and a grown one by appending its new bytes once its old bytes are
  checked. A file that changed in any other way is not published, so that a published file only
  grows. A SIGTERM, SIGINT or SIGHUP stops the loader within STOP_SECONDS.

With --check, the loader checks, without contacting the storage, that it can run: it exits 0, or
2 after one line that says why.

The loader writes plain lines on stderr, which start with "tensorboard loader: " except for the
check's. None holds the storage's configuration, the environment or a credential. It imports only
the standard library, and determined.tensorboard.fetchers for s3, gcs and azure storage.
"""

import importlib
import json
import os
import posixpath
import queue
import re
import shutil
import signal
import stat
import sys
import threading
import time
import types
import urllib.parse
from typing import Any, Dict, List, NamedTuple, Optional, Set, Tuple

LIST_SECONDS = 20.0
WORKERS = 5
ATTEMPTS = 5
RETRY_PAUSE = 0.5
RETRY_MAX_PAUSE = 4.0
TICK = 1.0
STOP_SECONDS = 5.0
CHECK_SECONDS = 60
CHUNK = 1 << 20
ERROR_CHARS = 300

USAGE = "usage: tensorboard_loader.py [--check] LOGDIR"
PREFIX = "tensorboard loader: "
SCHEMES = {"s3": "s3://", "gcs": "gs://", "azure": "azure://"}
LOCAL = ("shared_fs", "directory")
RUN = re.compile(r"experiment/[1-9][0-9]*(/trial/[1-9][0-9]*)?")
CONTROL = re.compile(r"[\x00-\x1f\x7f]")
# The storage config's keys whose values are never credentials.
PLAIN_KEYS = {"type", "bucket", "prefix", "container", "host_path", "storage_path"}
# A URL, as opposed to a connection string that holds one.
URL = re.compile(r"[A-Za-z][A-Za-z0-9+.-]*://")
# The names, in "name=value" lists, of credentials.
SECRET_NAME = re.compile(r"(?i)key|sig|token|secret|password|credential")
# The parameters of a signed URL whose values are credentials.
SIGNED = re.compile(
    r"(?i)\b(sig|signature|x-amz-signature|x-amz-credential|x-amz-security-token|accountkey)="
    r"[^&;\s'\"]+"
)

secrets: List[str] = []
log_lock = threading.Lock()
stop_requested = False


class Invalid(Exception):
    """The loader cannot run; the message says why, in one line."""


class Timeout(BaseException):
    """The check took longer than CHECK_SECONDS."""


class Source(NamedTuple):
    run: str
    path: str


class Plan(NamedTuple):
    storage_type: str
    storage_config: Dict[str, Any]
    sources: List[Source]


def log(line: str) -> None:
    with log_lock:
        sys.stderr.write(PREFIX + line + "\n")
        sys.stderr.flush()


def scrub(text: str) -> str:
    """text without the storage's credentials."""
    for secret in secrets:
        text = text.replace(secret, "***")
    return SIGNED.sub(lambda m: m.group(1) + "=***", text)


def error_text(e: BaseException) -> str:
    """The first line of str(e), without credentials, cut to ERROR_CHARS characters."""
    return scrub(str(e).split("\n", 1)[0])[:ERROR_CHARS]


def shown(value: Any) -> str:
    """A value of the sources' file in one line."""
    if isinstance(value, str) and not CONTROL.search(value):
        return value[:ERROR_CHARS]
    return json.dumps(value)[:ERROR_CHARS]


def find_secrets(config: Any, key: str = "") -> List[str]:
    """
    The strings of the storage config that may be credentials: every value but those of
    PLAIN_KEYS, or, of a URL, its user information and its query. The credentials of
    "name=value" lists, as in connection strings, SAS tokens and queries, count too.
    """
    found: List[str] = []
    if isinstance(config, dict):
        for k, v in config.items():
            found += find_secrets(v, str(k))
    elif isinstance(config, list):
        for v in config:
            found += find_secrets(v, key)
    elif isinstance(config, str) and key not in PLAIN_KEYS:
        parts = [config]
        if URL.match(config):
            url = urllib.parse.urlsplit(config)
            userinfo = url.netloc.rpartition("@")[0]
            parts = [userinfo, userinfo.partition(":")[2], url.query]
        for part in parts + [config]:
            for item in re.split(r"[;&]", part):
                name, _, value = item.partition("=")
                if SECRET_NAME.search(name):
                    parts.append(urllib.parse.unquote(value))
                    parts.append(value)
        found += [p for p in parts if len(p) >= 4]
    return found


def read_json(path: Any) -> Any:
    try:
        with open(path, "rb") as f:
            data = f.read()
        return json.loads(data.decode("utf-8"))
    except UnicodeDecodeError:
        raise ValueError("the file is not UTF-8") from None


def read_plan(environ: Dict[str, str]) -> Plan:
    """The sources and the checkpoint storage, checked; Invalid says what is wrong."""
    sources_path = environ.get("DET_TENSORBOARD_SOURCES")
    if not sources_path:
        raise Invalid("DET_TENSORBOARD_SOURCES is not set")
    try:
        doc = read_json(sources_path)
    except (OSError, ValueError) as e:
        raise Invalid(f"cannot read the sources: {error_text(e)}")
    if not isinstance(doc, dict):
        doc = {"version": None}
    version = doc.get("version")
    if type(version) is not int or version != 1:
        raise Invalid(f"the sources have version {json.dumps(version)[:ERROR_CHARS]}, not 1")
    storage_type = doc.get("storage_type")
    if storage_type not in SCHEMES and storage_type not in LOCAL:
        raise Invalid("the sources are invalid: storage_type")
    entries = doc.get("sources")
    if (
        not isinstance(entries, list)
        or not entries
        or not all(isinstance(s, dict) for s in entries)
    ):
        raise Invalid("the sources are invalid: sources")
    runs: List[str] = []
    for entry in entries:
        run = entry.get("run")
        if (
            not isinstance(run, str)
            or not RUN.fullmatch(run)
            or run in runs
            or any(r.startswith(run + "/") or run.startswith(r + "/") for r in runs)
        ):
            raise Invalid(f"the sources are invalid: run {shown(run)}")
        runs.append(run)
    sources = []
    for entry in entries:
        path = entry.get("path")
        if not valid_path(storage_type, path):
            raise Invalid(f"the sources are invalid: path of {entry['run']}")
        sources.append(Source(entry["run"], path))
    try:
        storage_config = read_json(doc.get("storage_config"))
        if not isinstance(storage_config, dict):
            raise ValueError("not a JSON object")
    except (OSError, ValueError, TypeError) as e:
        raise Invalid(f"cannot read the checkpoint storage: {error_text(e)}")
    secrets[:] = sorted(set(find_secrets(storage_config)), key=len, reverse=True)
    if storage_config.get("type") != storage_type:
        raise Invalid(
            f"the checkpoint storage is of type {shown(storage_config.get('type'))}, "
            f"not {storage_type}"
        )
    return Plan(storage_type, storage_config, sources)


def valid_path(storage_type: str, path: Any) -> bool:
    if not isinstance(path, str) or CONTROL.search(path):
        return False
    if storage_type in SCHEMES:
        rest = path[len(SCHEMES[storage_type]) :]
        return (
            path.startswith(SCHEMES[storage_type])
            and path.endswith("/")
            and not {".", ".."} & set(rest.split("/"))
            and "?" not in path
            and "#" not in path
        )
    local = path[:-1] if path.endswith("/") else path
    return local.startswith("/") and posixpath.normpath(local) == local


def load_fetchers() -> Any:
    try:
        return importlib.import_module("determined.tensorboard.fetchers")
    except Exception as e:
        raise Invalid(f"determined has no TensorBoard fetchers: {error_text(e)}")


def staging_dir(logdir: str, fresh: bool) -> str:
    """The staging directory beside logdir, created, on logdir's file system."""
    staging = os.path.join(os.path.dirname(logdir), "staging")
    if fresh:
        shutil.rmtree(staging, ignore_errors=True)
    os.makedirs(staging, exist_ok=True)
    if os.stat(staging).st_dev != os.stat(logdir).st_dev:
        raise Invalid(f"the log directory and {staging} are on different file systems")
    return staging


def build(fetchers: Any, plan: Plan, local_dir: str) -> Any:
    try:
        fetcher = fetchers.build(plan.storage_config, [s.path for s in plan.sources], local_dir)
    except Exception as e:
        raise Invalid(f"{plan.storage_type} storage: {error_text(e)}")
    for name in ("_list", "_fetch"):
        if not callable(getattr(fetcher, name, None)):
            raise Invalid(f"the {plan.storage_type} fetcher lacks {name}")
    return fetcher


def check(environ: Dict[str, str], logdir: str) -> Optional[str]:
    """Why the loader cannot run, or None, within CHECK_SECONDS. It never contacts the storage."""

    def timeout(signum: int, frame: Optional[types.FrameType]) -> None:
        raise Timeout()

    signal.signal(signal.SIGALRM, timeout)
    signal.alarm(CHECK_SECONDS)
    try:
        plan = read_plan(environ)
        if plan.storage_type in SCHEMES:
            fetchers = load_fetchers()
            staging = staging_dir(logdir, fresh=False)
            build(fetchers, Plan(plan.storage_type, plan.storage_config, plan.sources[:1]), staging)
        return None
    except Timeout:
        return f"the check took longer than {CHECK_SECONDS} seconds"
    except Invalid as e:
        return str(e)
    except Exception as e:
        return f"unexpected error: {type(e).__name__}: {error_text(e)}"
    finally:
        signal.alarm(0)


def link(logdir: str, sources: List[Source]) -> None:
    """Links each source's directory at logdir/<run>, unless something is there."""
    for source in sources:
        dest = os.path.join(logdir, *source.run.split("/"))
        os.makedirs(os.path.dirname(dest), exist_ok=True)
        if not os.path.lexists(dest):
            os.symlink(source.path.rstrip("/"), dest)


def forget(fetcher: Any, key: str) -> None:
    """Makes the fetcher's next listing yield key again."""
    method = getattr(fetcher, "forget", None)
    if method is not None:
        method(key)
    else:
        fetcher._file_records.pop(key, None)


def same_start(a: str, b: str, size: int) -> bool:
    """Whether the first size bytes of the files a and b are equal."""
    with open(a, "rb") as fa, open(b, "rb") as fb:
        while size > 0:
            chunk = min(size, CHUNK)
            if fa.read(chunk) != fb.read(chunk):
                return False
            size -= chunk
    return True


class Pending:
    """A key that is queued or downloading; again asks for one more download after this one."""

    def __init__(self, run: str, rel: str) -> None:
        self.run = run
        self.rel = rel
        self.running = False
        self.again = False


class Loader:
    """
    The object stores' loader: a lister thread that queues each new or changed object of the
    sources, and worker threads that download and publish them.
    """

    def __init__(
        self, logdir: str, sources: List[Source], lister: Any, workers: List[Any], staging: str
    ) -> None:
        self.logdir = logdir
        self.sources = [
            (s, urllib.parse.urlparse(s.path).path.lstrip("/")) for s in sources
        ]  # type: List[Tuple[Source, str]]
        self.lister = lister
        self.workers = workers
        self.staging = staging
        self.stop_event = threading.Event()
        self.publish_lock = threading.Lock()
        self.publishing_closed = False
        self.queue = queue.Queue()  # type: queue.Queue[str]
        self.pending = {}  # type: Dict[str, Pending]
        self.pending_lock = threading.Lock()
        self.failed = set()  # type: Set[str]
        self.failed_lock = threading.Lock()
        self.skipped = set()  # type: Set[str]
        self.listing_failed = set()  # type: Set[str]
        self.changed = set()  # type: Set[Tuple[str, int]]
        self.threads = []  # type: List[threading.Thread]

    def start(self) -> None:
        self.threads = [threading.Thread(target=self.list_forever, name="lister", daemon=True)]
        for i in range(len(self.workers)):
            self.threads.append(
                threading.Thread(target=self.work, args=(i,), name=f"worker-{i}", daemon=True)
            )
        for t in self.threads:
            t.start()

    def stop(self) -> int:
        """
        Stops the threads within STOP_SECONDS, removes the staging directory and returns the
        number of threads still running.
        """
        self.stop_event.set()
        with self.publish_lock:
            self.publishing_closed = True
        deadline = time.monotonic() + STOP_SECONDS
        for t in self.threads:
            t.join(max(0.0, deadline - time.monotonic()))
        alive = [t for t in self.threads if t.is_alive()]
        downloads = sum(t.name != "lister" for t in alive)
        if downloads:
            log(f"{downloads} downloads were still running at exit")
        shutil.rmtree(self.staging, ignore_errors=True)
        return len(alive)

    def list_forever(self) -> None:
        while not self.stop_event.is_set():
            try:
                self.list_once()
            except Exception as e:
                log(f"unexpected error: {type(e).__name__}: {error_text(e)}")
            self.stop_event.wait(LIST_SECONDS)

    def list_once(self) -> None:
        with self.failed_lock:
            failed, self.failed = self.failed, set()
        for key in failed:
            forget(self.lister, key)
        for source, prefix in self.sources:
            if self.stop_event.is_set():
                return
            try:
                for key in self.lister._list(source.path):
                    if self.stop_event.is_set():
                        return
                    self.offer(source.run, prefix, key)
            except Exception as e:
                if source.run not in self.listing_failed:
                    self.listing_failed.add(source.run)
                    log(f"{source.run}: listing failed: {error_text(e)}")
                continue
            if source.run in self.listing_failed:
                self.listing_failed.discard(source.run)
                log(f"{source.run}: listing works again")

    def offer(self, run: str, prefix: str, key: str) -> None:
        """Queues the listed key, unless it is queued already or unsafe."""
        rel = key[len(prefix) :] if key.startswith(prefix) else None
        if rel is not None and (rel == "" or rel.endswith("/")):
            return
        if (
            rel is None
            or rel.startswith("/")
            or {"", ".", ".."} & set(rel.split("/"))
            or CONTROL.search(rel)
        ):
            if key not in self.skipped:
                self.skipped.add(key)
                log(f"{run}: skipped the object {repr(key)}")
            return
        with self.pending_lock:
            pending = self.pending.get(key)
            if pending is None:
                self.pending[key] = Pending(run, rel)
                self.queue.put(key)
            elif pending.running:
                pending.again = True

    def work(self, i: int) -> None:
        while not self.stop_event.is_set():
            try:
                key = self.queue.get(timeout=TICK)
            except queue.Empty:
                continue
            if self.stop_event.is_set():
                return
            try:
                self.load(i, key)
            except Exception as e:
                log(f"unexpected error: {type(e).__name__}: {error_text(e)}")
                with self.pending_lock:
                    self.pending.pop(key, None)

    def load(self, i: int, key: str) -> None:
        """Downloads and publishes key, with up to ATTEMPTS attempts."""
        with self.pending_lock:
            pending = self.pending[key]
            pending.running = True
            pending.again = False
        dest = os.path.join(self.logdir, *pending.run.split("/"), *pending.rel.split("/"))
        pause = RETRY_PAUSE
        error = None  # type: Optional[BaseException]
        for attempt in range(ATTEMPTS):
            if attempt:
                if self.stop_event.wait(pause):
                    return
                pause = min(pause * 2, RETRY_MAX_PAUSE)
            if self.stop_event.is_set():
                return
            try:
                self.attempt(i, key, dest, pending)
            except Exception as e:
                error = e
                continue
            with self.pending_lock:
                if pending.again:
                    pending.running = pending.again = False
                    self.queue.put(key)
                else:
                    del self.pending[key]
            return
        reason = error_text(error) if error is not None else ""
        log(f"{pending.run}/{pending.rel}: not loaded after {ATTEMPTS} attempts: {reason}")
        with self.pending_lock:
            del self.pending[key]
        with self.failed_lock:
            self.failed.add(key)

    def attempt(self, i: int, key: str, dest: str, pending: Pending) -> None:
        work = os.path.join(self.staging, str(i))
        shutil.rmtree(work, ignore_errors=True)
        os.makedirs(work)
        try:
            self.workers[i]._fetch(key, lambda: None)
            staged = []
            for d, _, files in os.walk(work):
                for f in files:
                    if stat.S_ISREG(os.lstat(os.path.join(d, f)).st_mode):
                        staged.append(os.path.join(d, f))
            if len(staged) != 1:
                raise RuntimeError(f"the download left {len(staged)} files")
            self.publish(staged[0], dest, pending)
        finally:
            # A download that ends after the stop leaves nothing.
            shutil.rmtree(self.staging if self.stop_event.is_set() else work, ignore_errors=True)

    def publish(self, staged: str, dest: str, pending: Pending) -> None:
        """
        Publishes the staged download at dest: a new file by a rename, a grown one by appending
        its new bytes. A stale download is dropped, and a file changed otherwise is not published.
        """
        with self.publish_lock:
            if self.publishing_closed:
                return
            try:
                published = os.stat(dest).st_size
            except FileNotFoundError:
                os.makedirs(os.path.dirname(dest), exist_ok=True)
                os.replace(staged, dest)
                return
            size = os.stat(staged).st_size
            if size >= published and same_start(staged, dest, published):
                if size > published:
                    with open(staged, "rb") as src, open(dest, "ab") as out:
                        src.seek(published)
                        shutil.copyfileobj(src, out, CHUNK)
                return
            if size < published and same_start(staged, dest, size):
                return
            if (dest, size) not in self.changed:
                self.changed.add((dest, size))
                log(
                    f"{pending.run}/{pending.rel}: changed in storage other than by growing; "
                    "the loaded copy is kept"
                )


def request_stop(signum: int, frame: Optional[types.FrameType]) -> None:
    global stop_requested
    stop_requested = True


def run(environ: Dict[str, str], logdir: str) -> int:
    for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
        signal.signal(signum, request_stop)
    try:
        plan = read_plan(environ)
        if plan.storage_type in LOCAL:
            link(logdir, plan.sources)
            return 0
        fetchers = load_fetchers()
        staging = staging_dir(logdir, fresh=True)
        lister = build(fetchers, plan, staging)
        workers = [build(fetchers, plan, os.path.join(staging, str(i))) for i in range(WORKERS)]
    except Invalid as e:
        log(str(e))
        return 1
    except Exception as e:
        log(f"unexpected error: {type(e).__name__}: {error_text(e)}")
        return 1
    log(f"loading {len(plan.sources)} sources from {plan.storage_type} storage")
    loader = Loader(logdir, plan.sources, lister, workers, staging)
    loader.start()
    while not stop_requested:
        time.sleep(TICK)
    alive = loader.stop()
    sys.stderr.flush()
    if alive:
        os._exit(0)
    return 0


def main(argv: List[str]) -> int:
    args = argv[1:]
    checking = bool(args) and args[0] == "--check"
    if checking:
        args = args[1:]
    if len(args) != 1 or not os.path.isabs(args[0]):
        print(USAGE, file=sys.stderr)
        return 2
    environ = dict(os.environ)
    if checking:
        reason = check(environ, args[0])
        if reason is None:
            return 0
        print(reason, file=sys.stderr)
        return 2
    return run(environ, args[0])


if __name__ == "__main__":
    sys.exit(main(sys.argv))
