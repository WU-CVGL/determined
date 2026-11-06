#!/usr/bin/env bash

# The vscode service: VS Code in the browser, coder/code-server, on DET_CODE_SERVER_PORT, at the
# release that the master pinned when it created the task, DET_CODE_SERVER_VERSION. It logs in
# with the password whose argon2 hash is HASHED_PASSWORD. The supervisor sources this file after
# the init.
#
# code-server comes from the first of:
# - DET_CODE_SERVER_DIR, a copy that an administrator mounts, when it has bin/code-server;
# - code-server on the PATH, when it is the pinned release;
# - the cache, which holds each release for each architecture in <version>/linux-<arch>: the
#   directory DET_CODE_SERVER_CACHE_DIR when it is set; else /var/cache/determined/code-server
#   when it exists, which an administrator mounts for the tasks of all users, unless the release
#   is not there and cannot be installed there; else $HOME/.cache/determined/code-server;
# - the master: the release tarball of the task's version and architecture and its checksum,
#   extracted into the cache. An install extracts into a temporary directory of its own next to
#   the release, adds the marker file .det-complete and renames the release into place. The first
#   rename wins; a task whose rename loses uses the winner's copy. A task removes only its own
#   temporary directory. The lock on <version>/linux-<arch>.lock only saves the tasks that share a
#   cache from downloading a release more than once: a task that gets it after another task
#   installed the release uses that copy, and a task that cannot take it (without flock, on an
#   error, or after waiting 600 s) installs without it. A release without the marker is
#   incomplete: a holder of the lock replaces it, and a task without the lock waits up to 600 s for
#   that, then runs code-server from its own temporary directory until it exits. A stop of the
#   shell ends an install, also while it waits, and removes its temporary directory. A cache that
#   is full or read-only counts as one that cannot be installed into.
#
# The unit logs which cache it took code-server from, whether it installed it there, and why it
# did so without the lock or outside the release directory.
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

# The service supports glibc 2.28 or later, whatever the minimum of the code-server release. ldd
# prints, for example, "ldd (Ubuntu GLIBC 2.35-0ubuntu3) 2.35" first.
vscode_glibc=$(ldd --version 2>/dev/null) || true
vscode_glibc=${vscode_glibc%%$'\n'*}
vscode_glibc=${vscode_glibc##* }
if [[ ! $vscode_glibc =~ ^([0-9]+)\.([0-9]+)$ ]] ||
    ((BASH_REMATCH[1] < 2 || (BASH_REMATCH[1] == 2 && BASH_REMATCH[2] < 28))); then
    vscode_unavailable "needs glibc 2.28 or later, found ${vscode_glibc:-none}"
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

vscode_shared_cache=/var/cache/determined/code-server
vscode_marker=.det-complete
# How long a task waits for another task's install of a release.
vscode_wait_seconds=600

# vscode_complete returns whether the directory $1 holds a complete release.
vscode_complete() {
    [[ -x $1/bin/code-server && -e $1/$vscode_marker ]]
}

# vscode_lock opens the lock file of the install of vscode_home, which is read-only to a user who
# did not create it, on the descriptor vscode_lock_fd and takes the lock. With $1 set, it waits up
# to vscode_wait_seconds for another task that holds the lock, and sets vscode_waited when it does.
# It returns 1 when it cannot open or take the lock, also when the file system cannot lock a file
# open only for reading, as NFSv4 cannot, and when flock is not installed, which it notes in
# vscode_nolock.
vscode_lock() {
    local rc
    vscode_waited=
    if ! command -v flock >/dev/null; then
        vscode_nolock="without a lock: flock is not installed"
        return 1
    fi
    if ! { exec {vscode_lock_fd}>>"$vscode_home.lock"; } 2>/dev/null &&
        ! { exec {vscode_lock_fd}<"$vscode_home.lock"; } 2>/dev/null; then
        vscode_lock_fd=
        return 1
    fi
    flock -n "$vscode_lock_fd" 2>/dev/null && return 0
    rc=$?
    # flock exits 1 when another task holds the lock, and with another status on an error.
    if [[ -n ${1-} ]] && ((rc == 1)); then
        vscode_waited=1
        flock -w "$vscode_wait_seconds" "$vscode_lock_fd" 2>/dev/null && return 0
    fi
    vscode_unlock
    return 1
}

# vscode_unlock closes the lock, so that code-server does not inherit it.
vscode_unlock() {
    if [[ -n ${vscode_lock_fd-} ]]; then
        exec {vscode_lock_fd}>&-
        vscode_lock_fd=
    fi
}

# vscode_drop_tmp removes the temporary directory vscode_tmp of this task's install, and only
# that one: the other tasks' are theirs.
vscode_drop_tmp() {
    if [[ -n ${vscode_tmp-} ]]; then
        rm -rf -- "$vscode_tmp" 2>/dev/null
        vscode_tmp=
    fi
}

# vscode_tmp_exit is vscode_drop_tmp in the EXIT trap. It turns errexit off, which would end the
# trap at its nonzero status before the supervisor's command, and returns the exit status for it.
vscode_tmp_exit() {
    # shellcheck disable=SC2319 # the exit status, in the trap
    local rc=$?
    set +e
    vscode_drop_tmp
    return "$rc"
}

# vscode_trap_tmp makes the EXIT trap remove vscode_tmp, before the trap that the supervisor set.
# vscode_untrap_tmp restores that trap.
vscode_trap_tmp() {
    vscode_exit_trap=$(trap -p EXIT)
    vscode_exit_command=
    [[ -z $vscode_exit_trap ]] || eval "vscode_trap_args ${vscode_exit_trap#trap }"
    # shellcheck disable=SC2064 # the supervisor's command, as it is now
    trap "vscode_tmp_exit${vscode_exit_command:+; $vscode_exit_command}" EXIT
}
vscode_trap_args() { vscode_exit_command=$2; }
vscode_untrap_tmp() {
    if [[ -n $vscode_exit_trap ]]; then
        eval "$vscode_exit_trap"
    else
        trap - EXIT
    fi
}

# vscode_write_error returns whether the error message $1 says that the file system is full or
# read-only.
vscode_write_error() {
    [[ $1 == *"No space left on device"* || $1 == *"Disk quota exceeded"* ||
        $1 == *"Read-only file system"* ]]
}

# vscode_install installs the release for the architecture $1 into vscode_home, in the cache
# vscode_cache, unless another task has, and sets vscode_bin, and vscode_how to installed, reused
# or waited (reused after waiting for another task's install). On a failure it sets
# vscode_reason and returns 2 when it cannot write to the cache, else 1. The files that it creates
# are readable by every user, for a cache that users share.
vscode_install() {
    local arch=$1 dir mode umask_was rc locked=
    dir=$vscode_cache/$vscode_version
    if ! command -v tar >/dev/null; then
        vscode_reason="tar is not installed"
        return 1
    fi
    umask_was=$(umask)
    umask 022
    # The release directory has the cache's mode, such as 1777 for a cache that users share, so
    # that other users can install other architectures there. It has it from its creation on, so
    # that another user who finds it can create a lock file in it.
    if [[ ! -d $dir ]]; then
        mkdir -p "$vscode_cache" 2>/dev/null
        if mode=$(stat -c %a "$vscode_cache" 2>/dev/null); then
            (umask 0 && mkdir -m "$mode" "$dir") 2>/dev/null
        fi
    fi
    # A stop ends the install, also a wait, so that the EXIT trap removes its temporary directory
    # before the supervisor kills the service. code-server gets the supervisor's handling.
    vscode_stop_traps=$(trap -p INT TERM HUP)
    trap 'service_stop_seen=1; exit 143' INT TERM HUP
    [[ -z ${service_stop_seen-} ]] || exit 143
    # The lock only saves the tasks that share a cache from installing a release more than once.
    # Without it, the rename decides which task's install the release is.
    vscode_deadline=$((SECONDS + vscode_wait_seconds))
    if vscode_lock wait; then
        locked=1
    fi
    vscode_install_release "$arch" "$dir" "$locked"
    rc=$?
    vscode_unlock
    trap - INT TERM HUP
    eval "$vscode_stop_traps"
    [[ -z ${service_stop_seen-} ]] || exit 143
    umask "$umask_was"
    return "$rc"
}

# vscode_install_release downloads and extracts the release for the architecture $1 into a
# temporary directory of its own in the release directory $2, and renames it to vscode_home,
# unless another task installed the release first. $3 is set when the lock is held. It returns as
# vscode_install does.
vscode_install_release() {
    local arch=$1 dir=$2 locked=$3 file top err='' rc
    file=code-server-$vscode_version-linux-$arch.tar.gz
    top=code-server-$vscode_version-linux-$arch
    # Another task has installed the release, maybe while this one waited for the lock.
    if vscode_complete "$vscode_home"; then
        vscode_reuse
        return 0
    fi
    if [[ ! -w $dir ]]; then
        vscode_reason="cannot write to $dir"
        return 2
    fi
    rc=0
    vscode_tmp=
    vscode_trap_tmp
    if ! vscode_tmp=$(mktemp -d "$vscode_home.tmp.XXXXXX" 2>/dev/null); then
        vscode_tmp=
        vscode_reason="cannot write to $dir"
        rc=2
    elif ! err=$("$DET_PYTHON_EXECUTABLE" -c "$vscode_fetch" \
        "/det/code-server/$vscode_version/$file" "$vscode_tmp/$file" 2>&1); then
        vscode_reason="downloading $file from the master failed: ${err##*$'\n'}"
        rc=1
    elif ! err=$(tar -xzf "$vscode_tmp/$file" -C "$vscode_tmp" 2>&1 >/dev/null); then
        # gzip starts its message with an empty line.
        err=${err#"${err%%[!$'\n']*}"}
        vscode_reason="extracting $file failed: ${err%%$'\n'*}"
        rc=1
    elif [[ ! -x $vscode_tmp/$top/bin/code-server ]]; then
        vscode_reason="$file has no bin/code-server"
        rc=1
    elif ! : >"$vscode_tmp/$top/$vscode_marker" 2>/dev/null; then
        vscode_reason="cannot write to $dir"
        rc=2
    else
        rm -f "$vscode_tmp/$file"
        vscode_publish "$vscode_tmp/$top" "$dir" "$locked"
        rc=$?
    fi
    # A cache that is full or read-only is one that cannot be written to.
    if ((rc == 1)) && vscode_write_error "$err"; then
        rc=2
    fi
    # A task that runs code-server from its own temporary directory keeps it until it exits.
    if [[ -z $vscode_in_tmp ]]; then
        vscode_drop_tmp
        vscode_untrap_tmp
    fi
    if ((rc != 0)) && vscode_complete "$vscode_home"; then
        vscode_reuse
        return 0
    fi
    return "$rc"
}

# vscode_reuse takes code-server from the complete release vscode_home, which another task
# installed.
vscode_reuse() {
    vscode_bin=$vscode_home/bin/code-server
    vscode_how=reused
    [[ -z $vscode_waited ]] || vscode_how=waited
}

# vscode_publish renames the complete release $1, which this task installed in the release
# directory $2, to vscode_home, the first task's rename wins. $3 is set when the lock is held.
#
# A rename replaces only an empty directory, and only a holder of the lock removes one. So with
# the lock held, a release that is incomplete stays incomplete: it is left from an install that did
# not finish, or copied by hand, and is moved into this task's temporary directory, which is then
# removed. Without the lock, the task waits for the holder's release up to vscode_deadline and
# then runs code-server from its own temporary directory, which it removes when it exits.
vscode_publish() {
    local src=$1 dir=$2 locked=$3
    while :; do
        if vscode_complete "$vscode_home"; then
            vscode_reuse
            return 0
        fi
        if [[ ! -e $vscode_home && ! -L $vscode_home ]]; then
            if mv -T "$src" "$vscode_home" 2>/dev/null; then
                vscode_bin=$vscode_home/bin/code-server
                vscode_how=installed
                return 0
            fi
            if [[ ! -e $vscode_home && ! -L $vscode_home ]]; then
                vscode_reason="cannot write to $dir"
                return 2
            fi
            continue
        fi
        if [[ -n $locked ]]; then
            # An empty directory that a rename replaced since is not empty: then it is checked
            # again before it is moved.
            if rmdir "$vscode_home" 2>/dev/null || vscode_complete "$vscode_home" ||
                mv -T "$vscode_home" "$vscode_tmp/incomplete" 2>/dev/null; then
                continue
            fi
            vscode_reason="cannot write to $dir"
            return 2
        fi
        if ((SECONDS >= vscode_deadline)); then
            vscode_bin=$src/bin/code-server
            vscode_how=installed
            vscode_in_tmp=1
            vscode_note="$vscode_home is incomplete"
            vscode_home=$src
            return 0
        fi
        vscode_waited=1
        sleep 1
    done
}

# vscode_use_cache takes code-server from the cache $1, or installs it there. It returns as
# vscode_install does.
vscode_use_cache() {
    vscode_cache=$1
    vscode_note=
    vscode_nolock=
    vscode_in_tmp=
    vscode_home=$vscode_cache/$vscode_version/linux-$vscode_arch
    if vscode_complete "$vscode_home"; then
        vscode_bin=$vscode_home/bin/code-server
        vscode_how=reused
        return 0
    fi
    vscode_install "$vscode_arch"
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
    vscode_status=0
    vscode_why=
    if [[ -n ${DET_CODE_SERVER_CACHE_DIR-} ]]; then
        vscode_use_cache "$DET_CODE_SERVER_CACHE_DIR" || vscode_unavailable "$vscode_reason"
    else
        if [[ -d $vscode_shared_cache ]]; then
            vscode_use_cache "$vscode_shared_cache" || vscode_status=$?
            if ((vscode_status == 1)); then
                vscode_unavailable "$vscode_reason"
            fi
        fi
        if [[ ! -d $vscode_shared_cache ]] || ((vscode_status == 2)); then
            [[ $vscode_status == 0 ]] || vscode_why=$vscode_reason
            vscode_use_cache "${HOME-}/.cache/determined/code-server" ||
                vscode_unavailable "$vscode_reason"
        fi
    fi
    case $vscode_how in
    installed) vscode_line="installed code-server $vscode_version into" ;;
    reused) vscode_line="reused code-server $vscode_version in" ;;
    waited) vscode_line="reused code-server $vscode_version, after waiting for its install, in" ;;
    esac
    # Why the unit took code-server from where it did, and how it installed it.
    vscode_notes=$vscode_why
    if [[ -n $vscode_note ]]; then
        vscode_notes+=${vscode_notes:+; }$vscode_note
    fi
    if [[ $vscode_how == installed && -n $vscode_nolock ]]; then
        vscode_notes+=${vscode_notes:+; }$vscode_nolock
    fi
    echo "vscode service: $vscode_line $vscode_home${vscode_notes:+ ($vscode_notes)}" >&2
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

# The log helper is a child of this shell rather than of code-server, which logs to its standard
# output and error and gets the helper's pipe as these only. It opens the task's working directory.
#
# code-server applies the environment variables PORT and CODE_SERVER_HOST after --bind-addr, so
# that a task that sets them would move it off the port the master proxies; --host and --port,
# which it applies after them, keep it there and leave the variables to the task's programs.
# Upstream deprecates these two flags: check that they still exist when the supported range of
# code-server releases changes.
#
# --ignore-last-opened: the task's directory, not the last one opened from the shared home.
exec {vscode_log_fd}> >(vscode_log)
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
    "$PWD" \
    >&"$vscode_log_fd" 2>&1 {vscode_log_fd}>&- {service_stdout}>&- {service_stderr}>&-
