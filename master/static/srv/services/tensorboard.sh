#!/usr/bin/env bash

# The tensorboard service: TensorBoard from the image on DET_TENSORBOARD_PORT, reading
# DET_TENSORBOARD_LOGDIR, under DET_TENSORBOARD_PATH_PREFIX, the path at which the master proxies
# it, with the more arguments of DET_TENSORBOARD_ARGS, a JSON array of strings. The supervisor
# sources this file after the init.
#
# - TensorBoard is the package tensorboard of the first interpreter that has it of the tensorboard
#   program's on the PATH, python3 and python. That interpreter runs tensorboard_service.py, which
#   starts TensorBoard with the more arguments and then the service's own flags, which win, and
#   makes it refuse every request without DET_TENSORBOARD_TOKEN, which the master's proxy adds.
#   The unit installs nothing.
# - A relative log directory is resolved against the task's work directory. A missing one is
#   created, so that a training run can write to it later.
# - The service is READY once TensorBoard refuses a request without the token and answers one with
#   it under the prefix, which a probe in the background asks every half second. A TensorBoard that
#   answers without the token is never READY.
# - The task's activity record, which the master's proxy feeds, is the idle signal: the unit has
#   no idle timer of its own.
#
# When TensorBoard, or what the service needs, is missing, the unit or the program says why in one
# line and exits 127.

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

# The interpreters to try: the tensorboard program's, from its "#!" line, if it is a python, then
# python3 and python.
tensorboard_pythons=()
if tensorboard_program=$(command -v tensorboard) \
    && IFS= read -r tensorboard_shebang <"$tensorboard_program" 2>/dev/null \
    && [[ $tensorboard_shebang == '#!'* ]]; then
    read -r -a tensorboard_words <<<"${tensorboard_shebang#??}"
    for tensorboard_word in "${tensorboard_words[@]}"; do
        if [[ $tensorboard_word != -* && $tensorboard_word != */env ]]; then
            if [[ ${tensorboard_word##*/} == python* ]]; then
                tensorboard_pythons+=("$tensorboard_word")
            fi
            break
        fi
    done
fi
tensorboard_pythons+=(python3 python)

tensorboard_python=
for tensorboard_candidate in "${tensorboard_pythons[@]}"; do
    if command -v "$tensorboard_candidate" >/dev/null \
        && "$tensorboard_candidate" -c 'import tensorboard.program' 2>/dev/null; then
        tensorboard_python=$tensorboard_candidate
        break
    fi
done
if [[ -z $tensorboard_python ]]; then
    tensorboard_unavailable "tensorboard is not installed"
fi

if [[ $tensorboard_logdir != /* ]]; then
    tensorboard_logdir=$PWD/$tensorboard_logdir
fi
if ! tensorboard_error=$(mkdir -p -- "$tensorboard_logdir" 2>&1); then
    tensorboard_unavailable "cannot create the log directory $tensorboard_logdir: ${tensorboard_error##*: }"
fi

# The probe asks TensorBoard under the prefix, past any proxy of the task's environment, without
# and with the token, and prints one line, which marks the service READY, once TensorBoard refuses
# the first and answers the second.
"$DET_PYTHON_EXECUTABLE" -c '
import os
import sys
import time
import urllib.error
import urllib.request

url = sys.argv[1]
token = {"X-Determined-Service-Token": os.environ["DET_TENSORBOARD_TOKEN"]}
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def status(headers):
    try:
        with opener.open(urllib.request.Request(url, headers=headers), timeout=5) as response:
            return response.status
    except urllib.error.HTTPError as e:
        return e.code
    except (OSError, ValueError):
        return None


try:
    while True:
        refused = status({})
        if refused == 200:
            print("tensorboard service: TensorBoard answers without the token", file=sys.stderr)
            sys.exit(1)
        if refused == 401 and status(token) == 200:
            print("tensorboard: http ready", flush=True)
            break
        time.sleep(0.5)
except KeyboardInterrupt:
    sys.exit(130)
' "http://127.0.0.1:$tensorboard_port$tensorboard_prefix" \
    | "$DET_PYTHON_EXECUTABLE" /run/determined/check_ready_logs.py \
        --ready-regex '^tensorboard: http ready$' "${service_ready_args[@]}" &

"$tensorboard_python" /run/determined/services/tensorboard_service.py "$tensorboard_logdir"
