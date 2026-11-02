#!/usr/bin/env bash

# The container init shared by the notebook and shell entrypoints. Each sources this file once,
# without arguments, after task-setup.sh and "set -e", so that the startup hooks run in the
# entrypoint's own shell and what they set reaches the service it starts.

"$DET_PYTHON_EXECUTABLE" -m determined.exec.prep_container --resources --proxy --download_context_directory

STARTUP_HOOK="startup-hook.sh"
set -x
test -f "${TCD_STARTUP_HOOK}" && source "${TCD_STARTUP_HOOK}"
test -f "${STARTUP_HOOK}" && source "${STARTUP_HOOK}"
# Keep this the last command: it makes the file's status 0 when a hook does not exist.
set +x
