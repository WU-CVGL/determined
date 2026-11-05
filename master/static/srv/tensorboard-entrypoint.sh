#!/bin/bash

source /run/determined/task-setup.sh

set -e

if [ -z "$DET_SKIP_PIP_INSTALL" ]; then
    # Install tensorboard if not already installed (for custom PyTorch images). The image's numpy,
    # protobuf, requests and tensorboard stay at their versions: a newer package would otherwise
    # replace numpy with one the image's tensorboard cannot import. The profile plugin is installed
    # only when DET_TENSORBOARD_PLUGIN_PROFILE is set: it pulls in xprof and the Google Cloud
    # libraries, which takes minutes, and TensorBoard runs without it.
    image_packages="$(mktemp)"
    image_constraints="$(mktemp)"
    "$DET_PYTHON_EXECUTABLE" -m pip freeze > "$image_packages"
    grep -iE '^(numpy|protobuf|requests|tensorboard)==' "$image_packages" > "$image_constraints" || true
    "$DET_PYTHON_EXECUTABLE" -m pip install --constraint "$image_constraints" tensorboard
    if [ -n "$DET_TENSORBOARD_PLUGIN_PROFILE" ]; then
        "$DET_PYTHON_EXECUTABLE" -m pip install --constraint "$image_constraints" tensorboard-plugin-profile \
            || echo "tensorboard-plugin-profile was not installed; TensorBoard runs without it"
    fi
    rm -f "$image_packages" "$image_constraints"
fi

"$DET_PYTHON_EXECUTABLE" -m determined.exec.prep_container --proxy --download_context_directory

STARTUP_HOOK="startup-hook.sh"
set -x
test -f "${TCD_STARTUP_HOOK}" && source "${TCD_STARTUP_HOOK}"
test -f "${STARTUP_HOOK}" && source "${STARTUP_HOOK}"
set +x

READINESS_REGEX="TensorBoard contains metrics"
WAITING_REGEX="TensorBoard waits on metrics"
TENSORBOARD_VERSION=$("$DET_PYTHON_EXECUTABLE" -c "import tensorboard; print(tensorboard.__version__)")

"$DET_PYTHON_EXECUTABLE" -m determined.exec.tensorboard "$TENSORBOARD_VERSION" "$@" \
    > >(tee -p >("$DET_PYTHON_EXECUTABLE" /run/determined/check_ready_logs.py --ready-regex "$READINESS_REGEX" --waiting-regex "$WAITING_REGEX"))
