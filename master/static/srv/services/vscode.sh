#!/usr/bin/env bash

# The vscode service: VS Code in the browser, coder/code-server, on DET_CODE_SERVER_PORT, at the
# release that the master pinned when it created the task, DET_CODE_SERVER_VERSION. It logs in
# with the password whose argon2 hash is HASHED_PASSWORD. The supervisor sources this file after
# the init.
#
# code-server comes from the first of:
# - DET_CODE_SERVER_DIR, a copy that an administrator mounts, when it has bin/code-server;
# - code-server on the PATH, when it is the pinned release;
# - the cache, ${DET_CODE_SERVER_CACHE:-$HOME/.cache/determined/code-server}, which holds each
#   release for each architecture in <version>/linux-<arch>;
# - the master: the release tarball of the task's version and architecture and its checksum,
#   extracted into the cache. The tasks that share a cache install a release once: the install
#   holds a lock on <version>/linux-<arch>.lock, extracts into a temporary directory and renames
#   it into place, and a task that gets the lock after another task installed the release uses
#   that copy.
#
# Each run has its own config file, session socket, port and password, in the run directory;
# extensions and settings are in the data directories under the home.
#
# When code-server, or what it needs, is missing, the unit says why in one line and exits 127.

[[ ${supervisor_phase-} != pre-init ]] || return 0

vscode_unavailable() {
    echo "vscode service: $*" >&2
    exit 127
}

vscode_version=${DET_CODE_SERVER_VERSION-}
vscode_port=${DET_CODE_SERVER_PORT-}
if [[ ! $vscode_version =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
    vscode_unavailable "DET_CODE_SERVER_VERSION is not a release: \"$vscode_version\""
fi
# The config file sets session-socket, which code-server has since 4.14.1; earlier releases refuse
# to start with it.
if ((10#${BASH_REMATCH[1]} < 4 || (10#${BASH_REMATCH[1]} == 4 && (10#${BASH_REMATCH[2]} < 14 ||
    (10#${BASH_REMATCH[2]} == 14 && 10#${BASH_REMATCH[3]} < 1))))); then
    vscode_unavailable "needs code-server 4.14.1 or later, not $vscode_version"
fi
if [[ ! $vscode_port =~ ^[0-9]+$ ]]; then
    vscode_unavailable "DET_CODE_SERVER_PORT is not a port: \"$vscode_port\""
fi
if [[ -z ${HASHED_PASSWORD-} ]]; then
    vscode_unavailable "HASHED_PASSWORD is not set"
fi

# The Node.js that code-server bundles needs glibc 2.28 or later. ldd prints, for example,
# "ldd (Ubuntu GLIBC 2.35-0ubuntu3) 2.35" first.
vscode_glibc=$(ldd --version 2>/dev/null) || true
vscode_glibc=${vscode_glibc%%$'\n'*}
vscode_glibc=${vscode_glibc##* }
if [[ ! $vscode_glibc =~ ^([0-9]+)\.([0-9]+)$ ]] ||
    ((BASH_REMATCH[1] < 2 || (BASH_REMATCH[1] == 2 && BASH_REMATCH[2] < 28))); then
    vscode_unavailable "code-server needs glibc 2.28 or later, found ${vscode_glibc:-none}"
fi

# vscode_fetch downloads the master's file $1 and its checksum file $1.sha256, and writes the file
# to $2 if it matches. It goes through the task's session with the master, as the harness does, so
# that the master's certificate and the task's proxy settings apply. On a failure it prints the
# reason, last.
vscode_fetch='
import hashlib
import os
import sys

from determined.common import api
from determined.common.api import authentication, certs

path, dest = sys.argv[1], sys.argv[2]
master = api.canonicalize_master_url(os.environ["DET_MASTER"])
try:
    sess = authentication.login_from_task(master, cert=certs.default_load(master))
    want = sess.get(path + ".sha256", timeout=(30, 1200)).text.split()[0].lower()
    digest = hashlib.sha256()
    with sess.get(path, stream=True, timeout=(30, 1200)) as r, open(dest, "wb") as f:
        for chunk in r.iter_content(1 << 20):
            digest.update(chunk)
            f.write(chunk)
except Exception as e:
    sys.exit(" ".join(f"{type(e).__name__}: {e}".split()))
if digest.hexdigest() != want:
    sys.exit("the checksum does not match")
'

# vscode_install installs the release for the architecture $1 into vscode_home, unless another
# task has, and sets vscode_bin. On a failure it sets vscode_reason and returns 1. It closes the
# lock before it returns, so that code-server does not inherit it.
vscode_install() {
    local arch=$1 dir lock tmp file top err
    dir=$vscode_cache/$vscode_version
    file=code-server-$vscode_version-linux-$arch.tar.gz
    top=code-server-$vscode_version-linux-$arch
    if ! command -v tar >/dev/null; then
        vscode_reason="tar is not installed"
        return 1
    fi
    if ! command -v flock >/dev/null; then
        vscode_reason="flock is not installed"
        return 1
    fi
    if ! mkdir -p "$dir" 2>/dev/null || [[ ! -w $dir ]]; then
        vscode_reason="cannot write to $dir"
        return 1
    fi
    if ! { exec {lock}>>"$vscode_home.lock"; } 2>/dev/null; then
        vscode_reason="cannot open $vscode_home.lock"
        return 1
    fi
    if ! flock "$lock"; then
        exec {lock}>&-
        vscode_reason="cannot lock $vscode_home.lock"
        return 1
    fi
    # Another task has installed the release while this one waited.
    if [[ -x $vscode_home/bin/code-server ]]; then
        exec {lock}>&-
        vscode_bin=$vscode_home/bin/code-server
        return 0
    fi
    # Only the holder of the lock writes these: what is left is from a task that ended while it
    # installed.
    rm -rf "$vscode_home" "$vscode_home".tmp.*
    tmp=$vscode_home.tmp.$BASHPID
    if ! mkdir "$tmp" 2>/dev/null; then
        exec {lock}>&-
        vscode_reason="cannot write to $dir"
        return 1
    fi
    if ! err=$("$DET_PYTHON_EXECUTABLE" -c "$vscode_fetch" \
        "/det/code-server/$vscode_version/$file" "$tmp/$file" 2>&1); then
        rm -rf "$tmp"
        exec {lock}>&-
        vscode_reason="downloading $file from the master failed: ${err##*$'\n'}"
        return 1
    fi
    if ! tar -xzf "$tmp/$file" -C "$tmp" >/dev/null 2>&1 || [[ ! -x $tmp/$top/bin/code-server ]]; then
        rm -rf "$tmp"
        exec {lock}>&-
        vscode_reason="$file has no bin/code-server"
        return 1
    fi
    if ! mv "$tmp/$top" "$vscode_home" 2>/dev/null; then
        rm -rf "$tmp"
        exec {lock}>&-
        vscode_reason="cannot write to $dir"
        return 1
    fi
    rm -rf "$tmp"
    exec {lock}>&-
    vscode_bin=$vscode_home/bin/code-server
}

vscode_bin=
if [[ -n ${DET_CODE_SERVER_DIR-} && -x $DET_CODE_SERVER_DIR/bin/code-server ]]; then
    vscode_bin=$DET_CODE_SERVER_DIR/bin/code-server
elif vscode_path=$(command -v code-server); then
    # "code-server --version" prints, for example, "4.96.4 <commit> with Code 1.96.4". code-server
    # reads its config file first and, when there is none, writes one into the home and logs that;
    # the empty config file /dev/null keeps it from touching the home.
    vscode_path_out=$("$vscode_path" --config /dev/null --version 2>/dev/null) || true
    vscode_path_version=
    while IFS= read -r vscode_line; do
        if [[ $vscode_line =~ ^([0-9]+\.[0-9]+\.[0-9]+)([[:space:]]|$) ]]; then
            vscode_path_version=${BASH_REMATCH[1]}
            break
        fi
    done <<<"$vscode_path_out"
    if [[ $vscode_path_version == "$vscode_version" ]]; then
        vscode_bin=$vscode_path
    else
        echo "vscode service: skipping code-server ${vscode_path_version:-of unknown version} on the" \
            "PATH for $vscode_version" >&2
    fi
fi
if [[ -z $vscode_bin ]]; then
    case $(uname -m) in
    x86_64) vscode_arch=amd64 ;;
    aarch64 | arm64) vscode_arch=arm64 ;;
    *) vscode_unavailable "code-server is not available for $(uname -m)" ;;
    esac
    vscode_cache=${DET_CODE_SERVER_CACHE:-${HOME-}/.cache/determined/code-server}
    vscode_home=$vscode_cache/$vscode_version/linux-$vscode_arch
    if [[ -x $vscode_home/bin/code-server ]]; then
        vscode_bin=$vscode_home/bin/code-server
    elif ! vscode_install "$vscode_arch"; then
        vscode_unavailable "$vscode_reason"
    fi
fi

# The run's own files, and the data directories that persist with the home.
vscode_run=/run/determined/code-server
vscode_config=$vscode_run/config.yaml
vscode_data=${HOME-}/.local/share/code-server
mkdir -p "$vscode_run"

# A YAML string: JSON's quoting, which YAML takes.
vscode_quote() {
    local s=${1//\\/\\\\}
    printf '"%s"' "${s//\"/\\\"}"
}

# The password stays out of the config file: code-server reads its hash from HASHED_PASSWORD and
# removes it from its environment.
cat >"$vscode_config" <<VSCODE_CONFIG
bind-addr: $(vscode_quote "0.0.0.0:$vscode_port")
auth: password
user-data-dir: $(vscode_quote "$vscode_data")
extensions-dir: $(vscode_quote "$vscode_data/extensions")
session-socket: $(vscode_quote "$vscode_run/session.sock")
disable-telemetry: true
disable-update-check: true
disable-workspace-trust: true
VSCODE_CONFIG

# code-server logs, for example, "[...] info  HTTP server listening on http://0.0.0.0:8080/". The
# check takes the task's address only, so that a code-server that listens elsewhere is never READY.
READINESS_REGEX='^.*HTTP server listening on http://0\.0\.0\.0:'"$vscode_port/"

# code-server's log goes to tee, which copies it to the task log and to the readiness check. tee
# ignores the signals that stop code-server, so that it passes on its last lines, and waits for
# the check, so that it does not outlive this shell.
vscode_log() {
    trap '' INT TERM HUP
    tee -p >(vscode_ready_check) >&2
    wait "$!"
}

vscode_ready_check() {
    trap - INT TERM HUP
    exec "$DET_PYTHON_EXECUTABLE" /run/determined/check_ready_logs.py --ready-regex "${READINESS_REGEX}" "${service_ready_args[@]}"
}

# The group makes the log helper a child of this shell rather than of code-server. code-server
# logs to its standard output and error. It opens the task's working directory.
#
# code-server applies the environment variables PORT and CODE_SERVER_HOST after --bind-addr, so
# that a task that sets them would move it off the port the master proxies; --host and --port,
# which it applies after them, keep it there and leave the variables to the task's programs.
# Upstream deprecates these two flags: check that they still exist when the supported range of
# code-server releases changes.
#
# --ignore-last-opened: the task's directory, not the last one opened from the shared home.
{
    "$vscode_bin" --config "$vscode_config" \
        --bind-addr "0.0.0.0:$vscode_port" \
        --host 0.0.0.0 \
        --port "$vscode_port" \
        --auth password \
        --user-data-dir "$vscode_data" \
        --extensions-dir "$vscode_data/extensions" \
        --disable-telemetry \
        --disable-update-check \
        --disable-workspace-trust \
        --ignore-last-opened \
        "$PWD"
} > >(vscode_log) 2>&1
