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
#   it under the prefix, which the program's probe asks once TensorBoard has bound the port, and
#   writes to descriptor 3 for the readiness check. A TensorBoard that answers without the token is
#   never READY.
# - The task's activity record, which the master's proxy feeds, is the idle signal: the unit has
#   no idle timer of its own.
# - With DET_TENSORBOARD_SOURCES, the loader, /run/determined/tensorboard/tensorboard_loader.py,
#   runs with the task's python: first its check, whose failure makes the service UNAVAILABLE,
#   then in the background, where it publishes the sources' logs in the log directory.
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

# With sources, the loader publishes their logs in the log directory while TensorBoard reads
# it, in the background. Its check runs first: a loader that cannot run makes the service
# UNAVAILABLE with the check's last line.
if [[ -n ${DET_TENSORBOARD_SOURCES-} ]]; then
    tensorboard_loader=/run/determined/tensorboard/tensorboard_loader.py
    if ! tensorboard_error=$("$DET_PYTHON_EXECUTABLE" "$tensorboard_loader" --check \
        "$tensorboard_logdir" 2>&1); then
        [[ -n $tensorboard_error ]] || tensorboard_error="the loader's check failed without a message"
        tensorboard_unavailable "${tensorboard_error##*$'\n'}"
    fi
    "$DET_PYTHON_EXECUTABLE" "$tensorboard_loader" "$tensorboard_logdir" \
        {service_stdout}>&- {service_stderr}>&- &
fi

# The readiness check is a child of this shell rather than of the program, which gets the check's
# pipe as its descriptor 3 only.
exec {tensorboard_ready}> >("$DET_PYTHON_EXECUTABLE" /run/determined/check_ready_logs.py \
    --ready-regex '^tensorboard: http ready$' "${service_ready_args[@]}")
"$tensorboard_python" /run/determined/services/tensorboard_service.py "$tensorboard_logdir" \
    3>&"$tensorboard_ready" {tensorboard_ready}>&- {service_stdout}>&- {service_stderr}>&-
