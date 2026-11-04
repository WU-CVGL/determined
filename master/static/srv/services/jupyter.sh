#!/usr/bin/env bash

# The jupyter service: JupyterLab, with its idle check in the background when set, under the
# base URL that the master sets, NOTEBOOK_BASE_URL. The supervisor sources this file before the
# init, to set SHELL, and after it.

if [[ ${supervisor_phase-} == pre-init ]]; then
    # Use user's preferred SHELL in JupyterLab terminals, unless a startup hook sets another one.
    SHELL="$(
        set -o pipefail
        getent passwd "$(whoami)" | cut -d: -f7
    )" || SHELL="/bin/bash"
    export SHELL
    return 0
fi

if ! command -v jupyter >/dev/null; then
    echo "jupyter service: jupyter is not installed" >&2
    exit 127
fi

# The idle check reports a busy JupyterLab to its task's activity record. The master sets the idle
# type only where the idle time is watched.
if [[ -n ${DET_JUPYTER_IDLE_TYPE+set} ]]; then
    "$DET_PYTHON_EXECUTABLE" /run/determined/jupyter/check_idle.py &
fi

JUPYTER_LAB_LOG_FORMAT="%(levelname)s: [%(name)s] %(message)s"
READINESS_REGEX='^.*Jupyter Server .* is running.*$'

# JupyterLab's log goes to tee, which copies it to the task log and to the readiness check. tee
# ignores the signals that stop JupyterLab, so that it passes on its last lines, and waits for the
# check, so that it does not outlive this shell.
jupyter_log() {
    trap '' INT TERM HUP
    tee -p >(jupyter_ready_check) >&2
    wait "$!"
}

jupyter_ready_check() {
    trap - INT TERM HUP
    exec "$DET_PYTHON_EXECUTABLE" /run/determined/check_ready_logs.py --ready-regex "${READINESS_REGEX}" "${service_ready_args[@]}"
}

# The group makes the log helper a child of this shell rather than of JupyterLab.
{
    # A busy port fails the service: Jupyter Server would move off it, but the master proxies it.
    jupyter lab --ServerApp.port=${NOTEBOOK_PORT} \
        --ServerApp.port_retries=0 \
        --ServerApp.allow_origin="*" \
        --ServerApp.base_url="${NOTEBOOK_BASE_URL}" \
        --ServerApp.allow_root=True \
        --ServerApp.certfile=/run/determined/jupyter/jupyterCert.pem \
        --ServerApp.keyfile=/run/determined/jupyter/jupyterKey.key \
        --ServerApp.ip="*" \
        --ServerApp.open_browser=False \
        --ServerApp.token="$DET_NOTEBOOK_TOKEN" \
        --ServerApp.trust_xheaders=True \
        --Application.log_format="$JUPYTER_LAB_LOG_FORMAT" \
        --JupyterApp.log_format="$JUPYTER_LAB_LOG_FORMAT" \
        --ExtensionApp.log_format="$JUPYTER_LAB_LOG_FORMAT" \
        --LabServerApp.log_format="$JUPYTER_LAB_LOG_FORMAT" \
        --LabApp.log_format="$JUPYTER_LAB_LOG_FORMAT" \
        --ServerApp.log_format="$JUPYTER_LAB_LOG_FORMAT" \
        --ServerApp.root_dir="/" \
        --ContentsManager.preferred_dir="$PWD"
} 2> >(jupyter_log)
