#!/usr/bin/env bash

# The tensorboard service: TensorBoard from the image on DET_TENSORBOARD_PORT, reading
# DET_TENSORBOARD_LOGDIR, under DET_TENSORBOARD_PATH_PREFIX, the path at which the master proxies
# it, with the more arguments of DET_TENSORBOARD_ARGS, a JSON array of strings, after its own. The
# supervisor sources this file after the init.
#
# - TensorBoard is the tensorboard program on the PATH, else the module tensorboard.main of the
#   first of python3 and python that has it. The unit installs nothing.
# - A relative log directory is resolved against the task's work directory. A missing one is
#   created, so that a training run can write to it later.
# - TensorBoard 2 and later listens on every address with --bind_all, an earlier one with
#   --host 0.0.0.0; TensorBoard 2.5 and later gets --load_fast=false, which loads every plugin.
# - The service is READY once TensorBoard answers HTTP under the prefix, which a probe in the
#   background asks every half second.
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
tensorboard_flags=(--port "$tensorboard_port" --path_prefix "${tensorboard_prefix%/}")
if ((10#${BASH_REMATCH[1]} >= 2)); then
    tensorboard_flags+=(--bind_all)
else
    tensorboard_flags+=(--host 0.0.0.0)
fi
if ((10#${BASH_REMATCH[1]} > 2 || (10#${BASH_REMATCH[1]} == 2 && 10#${BASH_REMATCH[2]} >= 5))); then
    tensorboard_flags+=(--load_fast=false)
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

# The probe asks TensorBoard directly, past any proxy of the task's environment, and prints one
# line once it answers, which marks the service READY.
"$DET_PYTHON_EXECUTABLE" -c '
import sys
import time
import urllib.request

opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
while True:
    try:
        with opener.open(sys.argv[1], timeout=5) as response:
            if response.status == 200:
                print("tensorboard: http ready", flush=True)
                break
    except (OSError, ValueError):
        pass
    time.sleep(0.5)
' "http://127.0.0.1:$tensorboard_port$tensorboard_prefix" \
    | "$DET_PYTHON_EXECUTABLE" /run/determined/check_ready_logs.py \
        --ready-regex '^tensorboard: http ready$' "${service_ready_args[@]}" &

"${tensorboard_cmd[@]}" --logdir "$tensorboard_logdir" "${tensorboard_flags[@]}" "${tensorboard_args[@]}"
