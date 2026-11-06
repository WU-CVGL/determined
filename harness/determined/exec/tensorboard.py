import json
import logging
import os
import queue
import select
import subprocess
import sys
import tempfile
import threading
import time
from typing import Any, Callable, Dict, List, Mapping, Tuple

import boto3

import determined as det
import determined.common
from determined.tensorboard import fetchers

TENSORBOARD_TRIGGER_READY_MSG = "TensorBoard contains metrics"

# The program that runs TensorBoard behind the master proxy's token, which the master ships.
SERVICE_PROGRAM = "/run/determined/tensorboard/tensorboard_service.py"
SERVICE_READY_LINE = b"tensorboard: http ready\n"

TICK_INTERVAL = 1  # How many seconds to wait on each iteration of our check loop
MAX_WAIT_TIME = 600  # How many seconds to wait for the first metric file to download
TB_RESPONSE_WAIT_TIME = 300  # How many seconds to wait for TensorBoard to initially start up
WORK_QUEUE_MAX_SIZE = 20  # Size of the threading work queue for fetching
FULL_ITERATION_SLEEP_TIME = 20  # How long to wait between a full iteration run (in seconds)
NUM_FETCH_THREADS = 5  # Number of fetching threads to run concurrently
READY_SIGNAL_DELAY = 7  # How many seconds to wait before sending the ready signal
FETCH_ATTEMPTS = 5  # How many times a fetch thread tries a file before it gives up on it
FETCH_RETRY_PAUSE = 0.5  # How many seconds to wait after a file's first failed attempt
FETCH_RETRY_MAX_PAUSE = 4  # The longest wait between two attempts, in seconds

logger = logging.getLogger("determined")


def set_s3_region() -> None:
    bucket = os.environ.get("AWS_BUCKET")
    if bucket is None:
        return

    endpoint_url = os.environ.get("DET_S3_ENDPOINT_URL")
    client = boto3.client("s3", endpoint_url=endpoint_url)
    bucket_location = client.get_bucket_location(Bucket=bucket)

    region = bucket_location["LocationConstraint"]

    if region is not None:
        # We have observed that in US-EAST-1 the region comes back as None
        # and if AWS_REGION is set to None, tensorboard fails to pull events.
        print(f"Setting AWS_REGION environment variable to {region}.")
        os.environ["AWS_REGION"] = str(region)


def service_env(environ: Mapping[str, str], add_args: List[str], ready_fd: int) -> Dict[str, str]:
    """The environment of the service program: the task's, with TensorBoard's port, path prefix
    and arguments and the descriptor of the ready line. The token is in the task's environment.
    """
    return {
        **environ,
        "DET_TENSORBOARD_PORT": environ["TENSORBOARD_PORT"],
        "DET_TENSORBOARD_PATH_PREFIX": f"/proxy/{environ['DET_TASK_ID']}/",
        "DET_TENSORBOARD_ARGS": json.dumps(add_args),
        "DET_TENSORBOARD_READY_FD": str(ready_fd),
    }


def start_service(tfevents_dir: str, add_args: List[str]) -> Tuple[subprocess.Popen, int]:
    """Start the service program, which runs TensorBoard behind the master proxy's token, on
    tfevents_dir. The program writes its ready line, once TensorBoard has bound its port and
    refuses requests without the token, to a pipe, whose read end is returned with the process.
    """
    args = [sys.executable, SERVICE_PROGRAM, tfevents_dir]
    logger.debug(f"tensorboard service args: {args}")
    ready, ready_w = os.pipe()
    try:
        # The program gets the standard streams and the write end only.
        process = subprocess.Popen(
            args,
            env=service_env(os.environ, add_args, ready_w),
            pass_fds=(ready_w,),
        )
    except BaseException:
        os.close(ready)
        raise
    finally:
        os.close(ready_w)
    return process, ready


def check_service_ready(p: subprocess.Popen, ready: int, received: bytearray) -> bool:
    """Whether the program has written its ready line to ready, waiting on it up to a tick, to
    prevent 502 from master. received keeps what the program has written so far.
    """
    readable, _, _ = select.select([ready], [], [], TICK_INTERVAL)
    if not readable:
        return False
    chunk = os.read(ready, 4096)
    if not chunk:
        try:
            p.wait(timeout=TICK_INTERVAL)
        except subprocess.TimeoutExpired:
            raise RuntimeError(
                "TensorBoard closed its readiness channel before reporting ready."
            ) from None
        raise_if_dead(p)
    received.extend(chunk)
    return SERVICE_READY_LINE in received


def check_launch(environ: Mapping[str, str]) -> bool:
    """Whether the task has the token and the program, which a TensorBoard launched by an earlier
    master lacks.
    """
    return bool(environ.get("DET_TENSORBOARD_TOKEN")) and os.path.exists(SERVICE_PROGRAM)


def raise_if_dead(p: subprocess.Popen) -> None:
    ret_code = p.poll()
    if ret_code is not None:
        raise RuntimeError(f"Tensorboard process died, exit code({ret_code}).")


def start_tensorboard(
    storage_config: Dict[str, Any],
    storage_paths: List[str],
    add_tb_args: List[str],
) -> int:
    """Start Tensorboard and look for new files."""
    with tempfile.TemporaryDirectory() as local_dir:
        # Get fetcher and perform initial fetch
        logger.debug(
            f"Building fetcher...\n"
            f"\tstorage_paths: {storage_paths}\n"
            f"\tlocal_dir: {local_dir}"
        )
        fetcher = fetchers.build(storage_config, storage_paths, local_dir)

        tensorboard_process, ready = start_service(local_dir, add_tb_args)
        tb_fetch_manager = TBFetchManager()
        work_queue: queue.Queue = queue.Queue(maxsize=WORK_QUEUE_MAX_SIZE)

        iteration_thread = TBFetchIterationThread(
            fetcher=fetcher, work_queue=work_queue, daemon=True
        )
        fetch_threads = [
            TBFetchThread(
                fetcher=fetcher,
                work_queue=work_queue,
                new_file_callback=tb_fetch_manager.on_file_fetched,
                daemon=True,
            )
            for _ in range(NUM_FETCH_THREADS)
        ]

        with det.util.forward_signals(tensorboard_process):
            try:
                tb_unresponsive_stop_time = time.time() + TB_RESPONSE_WAIT_TIME

                # Wait for the Tensorboard process to start responding before proceeding.
                responsive = False
                received = bytearray()
                while not responsive:
                    raise_if_dead(tensorboard_process)

                    if time.time() > tb_unresponsive_stop_time:
                        raise RuntimeError("Tensorboard wasn't responsive before the timeout.")

                    responsive = check_service_ready(tensorboard_process, ready, received)
                os.close(ready)
                ready = -1

                # Continuously loop checking for new files
                stop_time = time.time() + MAX_WAIT_TIME
                iteration_thread.start()
                for fetch_thread in fetch_threads:
                    fetch_thread.start()
                while True:
                    raise_if_dead(tensorboard_process)

                    # Check if we have reached a timeout without downloading any files
                    if tb_fetch_manager.get_num_fetched_files() == 0:
                        if time.time() > stop_time:
                            raise RuntimeError("No new files were fetched before the timeout.")
                        else:
                            # TODO: This should trigger an actual task state change (DET-10001).
                            # For now, just print a message to the logs.
                            print("TensorBoard is waiting for metrics.", flush=True)
                    time.sleep(TICK_INTERVAL)

            finally:
                for fetch_thread in fetch_threads:
                    fetch_thread.stop_event.set()
                if ready >= 0:
                    os.close(ready)
                if tensorboard_process.poll() is None:
                    logger.info("Killing tensorboard process")
                    tensorboard_process.kill()
                    tensorboard_process.wait()


class TBFetchManager:
    """Simple Container Class to manage the state of the TensorBoard remote fetchers"""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._ready = False
        self._num_fetched_files = 0

    def on_file_fetched(self) -> None:
        """Runs on each callback from a fetcher to update the state.
        We delay signalling the READY_SIGNAL to give TensorBoard a moment
        to do its internal book keeping and prevent the "no data"
        issues we were previously seeing when signalling ready immediately
        after the first file is fetched.

        TensorBoard >=v2.5.0 has a "load_fast=true" flag to alleviate this problem
        but for backwards compatibility reasons, we cannot use it yet.
        More info here: https://github.com/tensorflow/tensorboard/issues/4784
        """
        with self._lock:
            if not self._ready:
                self._ready = True
                t = threading.Timer(READY_SIGNAL_DELAY, self._emit_ready_signal)
                t.start()
            self._num_fetched_files += 1

    def _emit_ready_signal(self) -> None:
        print(TENSORBOARD_TRIGGER_READY_MSG, flush=True)

    def get_num_fetched_files(self) -> int:
        with self._lock:
            return self._num_fetched_files


class TBFetchIterationThread(threading.Thread):
    """Thread to continuously iterate over the fetchers files and add them to a threading.Queue

    Note: We are making the assumption that there will only be one of these running per process.
    If we add more, then the base fetcher will need to support locking around the _file_records
    dictionary. Defined in <ROOT>/harness/determined/tensorboard/fetchers/base.py
    The fetch threads only remove a record (Fetcher.forget), which at worst fetches a file twice.
    """

    def __init__(
        self,
        fetcher: fetchers.Fetcher,
        work_queue: queue.Queue,
        *args: Any,
        **kwargs: Any,
    ) -> None:
        self._fetcher = fetcher
        self._work_queue = work_queue
        super().__init__(*args, **kwargs)

    def run(self) -> None:
        while True:
            try:
                for filepath in self._fetcher.list_all_generator():
                    self._work_queue.put(filepath, block=True)
            except Exception as e:
                logger.warning(
                    f"Failure listing TensorBoard files from {self._fetcher}. Error: {e}"
                    f" (retrying in {FULL_ITERATION_SLEEP_TIME}s)...",
                    exc_info=True,
                )
            finally:
                time.sleep(FULL_ITERATION_SLEEP_TIME)


class TBFetchThread(threading.Thread):
    """Thread to continuously read from the queue and fetch the files. A file that fails is tried
    again by the thread itself, up to FETCH_ATTEMPTS times each time it comes off the queue, and
    never put back on the queue; after the last attempt the fetcher forgets it, so that a later
    listing yields it again. stop_event ends the thread's wait on the queue and its pauses between
    attempts; a fetch that is already running returns when the storage client does.
    """

    def __init__(
        self,
        fetcher: fetchers.Fetcher,
        work_queue: queue.Queue,
        new_file_callback: Callable,
        *args: Any,
        **kwargs: Any,
    ) -> None:
        self._fetcher = fetcher
        self._work_queue = work_queue
        self._new_file_callback = new_file_callback
        self.stop_event = threading.Event()
        super().__init__(*args, **kwargs)

    def run(self) -> None:
        while not self.stop_event.is_set():
            try:
                filepath = self._work_queue.get(timeout=TICK_INTERVAL)
            except queue.Empty:
                continue
            self._fetch_with_retries(filepath)

    def _fetch_with_retries(self, filepath: str) -> None:
        pause = FETCH_RETRY_PAUSE
        for attempt in range(1, FETCH_ATTEMPTS + 1):
            try:
                self._fetcher._fetch(filepath, self._new_file_callback)
                return
            except Exception as e:
                if attempt == FETCH_ATTEMPTS:
                    logger.error(f"Failed to fetch TensorBoard file {filepath}: {e}")
                    self._fetcher.forget(filepath)
                    return
                logger.warning(
                    f"Failed to fetch TensorBoard file {filepath} (attempt {attempt} of"
                    f" {FETCH_ATTEMPTS}): {e}"
                )
            if self.stop_event.wait(pause):
                return
            pause = min(pause * 2, FETCH_RETRY_MAX_PAUSE)


if __name__ == "__main__":
    tb_version = sys.argv[1]
    storage_config_path = sys.argv[2]
    storage_paths = sys.argv[3].split(",")
    additional_tb_args = sys.argv[4:]

    config = {}  # type: Dict[str, Any]
    with open(storage_config_path) as config_file:
        storage_config = json.load(config_file)

    determined.common.set_logger(determined.common.util.debug_mode())
    if not check_launch(os.environ):
        print("TensorBoard: launched by an earlier master; launch it again.", file=sys.stderr)
        sys.exit(1)
    logger.debug(
        f"Tensorboard (v{tb_version}) Initializing...\n"
        f"\tstorage_config_path: {storage_config_path}\n"
        f"\tstorage_paths: {storage_paths}\n"
        f"\tadditional_tb_args: {additional_tb_args}"
    )

    ret = start_tensorboard(storage_config, storage_paths, additional_tb_args)
    sys.exit(ret)
