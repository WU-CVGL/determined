#!/usr/bin/env bash

# The tensorboard service: TensorBoard from the image, reading DET_TENSORBOARD_LOGDIR, under
# DET_TENSORBOARD_PATH_PREFIX, the path at which the master proxies it, with the more arguments of
# DET_TENSORBOARD_ARGS, a JSON array of strings, before its own --logdir, --port, --path_prefix and
# --host, which therefore win, behind a front on DET_TENSORBOARD_PORT that takes only the master
# proxy's requests. The supervisor sources this file after the init.
#
# - TensorBoard is the tensorboard program on the PATH, else the module tensorboard.main of the
#   first of python3 and python that has it. The unit installs nothing.
# - A relative log directory is resolved against the task's work directory. A missing one is
#   created, so that a training run can write to it later.
# - TensorBoard listens on 127.0.0.1 only, on a port that the unit picks there and that the
#   container does not publish; TensorBoard 2.5 and later gets --load_fast=false, which loads every
#   plugin.
# - Once TensorBoard answers HTTP under the prefix, the front, determined.exec.service_front,
#   listens on DET_TENSORBOARD_PORT and passes on to TensorBoard the requests that carry
#   DET_TENSORBOARD_TOKEN, which the master's proxy adds, and refuses the others. The service is
#   READY once the front answers, which a probe in the background asks every half second.
# - When TensorBoard or the front exits, the unit exits with its status, and the supervisor stops
#   the other one.
# - The task's activity record, which the master's proxy feeds, is the idle signal: the unit has
#   no idle timer of its own.
#
# When TensorBoard, or what the unit needs, is missing, the unit says why in one line and exits
# 127.

[[ ${supervisor_phase-} != pre-init ]] || return 0

tensorboard_unavailable() {
    echo "tensorboard service: $*" >&2
    exit 127
}

tensorboard_port=${DET_TENSORBOARD_PORT-}
tensorboard_prefix=${DET_TENSORBOARD_PATH_PREFIX-}
tensorboard_logdir=${DET_TENSORBOARD_LOGDIR-}
if [[ ! $tensorboard_port =~ ^[0-9]+$ ]]; then
    tensorboard_unavailable "DET_TENSORBOARD_PORT is not a port: \"$tensorboard_port\""
fi
if [[ $tensorboard_prefix != /*/ ]]; then
    tensorboard_unavailable "DET_TENSORBOARD_PATH_PREFIX is not a path: \"$tensorboard_prefix\""
fi
if [[ -z $tensorboard_logdir ]]; then
    tensorboard_unavailable "DET_TENSORBOARD_LOGDIR is not set"
fi
if [[ -z ${DET_TENSORBOARD_TOKEN-} ]]; then
    tensorboard_unavailable "DET_TENSORBOARD_TOKEN is not set"
fi

tensorboard_cmd=()
if command -v tensorboard >/dev/null; then
    tensorboard_cmd=(tensorboard)
else
    for tensorboard_python in python3 python; do
        if command -v "$tensorboard_python" >/dev/null \
            && "$tensorboard_python" -c 'import tensorboard.main' >/dev/null 2>&1; then
            tensorboard_cmd=("$tensorboard_python" -m tensorboard.main)
            break
        fi
    done
fi
if ((${#tensorboard_cmd[@]} == 0)); then
    tensorboard_unavailable "tensorboard is not installed"
fi

# --version_tb prints the release, for example "2.9.0".
tensorboard_version=$("${tensorboard_cmd[@]}" --version_tb 2>/dev/null) || true
tensorboard_version=${tensorboard_version##*$'\n'}
if [[ ! $tensorboard_version =~ ^([0-9]+)\.([0-9]+) ]]; then
    tensorboard_unavailable "cannot tell the release of ${tensorboard_cmd[*]}: \"$tensorboard_version\""
fi
tensorboard_major=$((10#${BASH_REMATCH[1]})) tensorboard_minor=$((10#${BASH_REMATCH[2]}))

# TensorBoard's own port, one that 127.0.0.1 has free now.
tensorboard_local_port=$("$DET_PYTHON_EXECUTABLE" -c '
import socket

with socket.socket() as s:
    s.bind(("127.0.0.1", 0))
    print(s.getsockname()[1])
' 2>/dev/null) || true
if [[ ! $tensorboard_local_port =~ ^[0-9]+$ ]]; then
    tensorboard_unavailable "cannot find a free port on 127.0.0.1"
fi

# The release's flags go before the more arguments, which may change them; the service's own go
# after, so that TensorBoard, which keeps a flag's last value, takes them however the more arguments
# spell theirs.
tensorboard_release_flags=()
if ((tensorboard_major > 2 || (tensorboard_major == 2 && tensorboard_minor >= 5))); then
    tensorboard_release_flags+=(--load_fast=false)
fi

# The more arguments, each followed by NUL, then "ok" once all are decoded.
tensorboard_args=()
if [[ -n ${DET_TENSORBOARD_ARGS-} && $DET_TENSORBOARD_ARGS != "[]" ]]; then
    mapfile -d '' -t tensorboard_args < <("$DET_PYTHON_EXECUTABLE" -c '
import json
import os
import sys

args = json.loads(os.environ["DET_TENSORBOARD_ARGS"])
if not isinstance(args, list) or not all(isinstance(arg, str) for arg in args):
    sys.exit(1)
sys.stdout.buffer.write(b"".join(arg.encode("utf-8", "surrogateescape") + b"\0" for arg in args + ["ok"]))
' 2>/dev/null)
    if ((${#tensorboard_args[@]} == 0)) || [[ ${tensorboard_args[-1]} != ok ]]; then
        tensorboard_unavailable "DET_TENSORBOARD_ARGS is not a JSON array of strings"
    fi
    unset 'tensorboard_args[-1]'
fi

if [[ $tensorboard_logdir != /* ]]; then
    tensorboard_logdir=$PWD/$tensorboard_logdir
fi
if ! tensorboard_error=$(mkdir -p -- "$tensorboard_logdir" 2>&1); then
    tensorboard_unavailable "cannot create the log directory $tensorboard_logdir: ${tensorboard_error##*: }"
fi
echo "tensorboard service: TensorBoard $tensorboard_version reads $tensorboard_logdir" >&2

# tensorboard_probe URL PID asks URL every half second, past any proxy of the task's environment,
# with the token, until it answers 200, and then succeeds; it fails once PID has exited.
tensorboard_probe() {
    "$DET_PYTHON_EXECUTABLE" -c '
import os
import sys
import time
import urllib.request

url, pid = sys.argv[1], int(sys.argv[2])
headers = {"X-Determined-Service-Token": os.environ["DET_TENSORBOARD_TOKEN"]}
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
while True:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        sys.exit(1)
    try:
        with opener.open(urllib.request.Request(url, headers=headers), timeout=5) as response:
            if response.status == 200:
                sys.exit(0)
    except (OSError, ValueError):
        pass
    time.sleep(0.5)
' "$@"
}

# tensorboard_exit PID exits with the status of PID, which has exited.
tensorboard_exit() {
    local status=0
    wait "$1" || status=$?
    exit "$status"
}

"${tensorboard_cmd[@]}" "${tensorboard_release_flags[@]}" "${tensorboard_args[@]}" \
    --logdir "$tensorboard_logdir" --port "$tensorboard_local_port" \
    --path_prefix "${tensorboard_prefix%/}" --host 127.0.0.1 &
tensorboard_pid=$!

if ! tensorboard_probe "http://127.0.0.1:$tensorboard_local_port$tensorboard_prefix" "$tensorboard_pid"; then
    tensorboard_exit "$tensorboard_pid"
fi

"$DET_PYTHON_EXECUTABLE" -m determined.exec.service_front --listen "0.0.0.0:$tensorboard_port" \
    --upstream "127.0.0.1:$tensorboard_local_port" --token-env DET_TENSORBOARD_TOKEN &
tensorboard_front_pid=$!

# One line once the front answers marks the service READY.
{ tensorboard_probe "http://127.0.0.1:$tensorboard_port$tensorboard_prefix" "$tensorboard_front_pid" \
    && echo "tensorboard: http ready"; } \
    | "$DET_PYTHON_EXECUTABLE" /run/determined/check_ready_logs.py \
        --ready-regex '^tensorboard: http ready$' "${service_ready_args[@]}" &

# The probe is a job too, and a stop's signal ends a wait: wait until one of the two has exited.
while kill -0 "$tensorboard_pid" 2>/dev/null && kill -0 "$tensorboard_front_pid" 2>/dev/null; do
    wait -n || true
done
if ! kill -0 "$tensorboard_pid" 2>/dev/null; then
    tensorboard_exit "$tensorboard_pid"
fi
tensorboard_exit "$tensorboard_front_pid"
