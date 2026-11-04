#!/usr/bin/env bash

# The ssh service: sshd with the task's environment in its sessions. The supervisor sources this
# file after the init, with sshd's arguments.

[[ ${supervisor_phase-} != pre-init ]] || return 0

if [[ ! -x /usr/sbin/sshd ]]; then
    echo "ssh service: sshd is not installed" >&2
    exit 127
fi

# Prepend each key in authorized_keys with a set of environment="KEY=VALUE"
# options to inject the entire docker environment into the eventual ssh
# session via an options in the authorized keys file.  See syntax described in
# `man 8 sshd`.  The purpose of this is to honor the environment variable
# settings as they are set for experiment or notebook configs, while still
# allowing customizations via normal ssh mechanisms.
#
# Not all variables should be overwritten this way; the HOME variable should be
# set by ssh, and the TERM, LANG, and LC_* variables should be passed in from
# the client. HASHED_PASSWORD, the hash that logs in to the vscode service, is
# for code-server only.
#
# Normal mechanisms like a ~/.bashrc will override these variables.
#
# After openssh 8+ is the only version of openssh supported (that is, after we
# only support ubuntu >= 20.04), we can use the more obvious SetEnv option and
# skip this awkwardness.
#
# For HPC systems, bash module support uses variables that store functions
# of the form below (with embedded parenthesis or %% in the name).
#   BASH_FUNC_ml()=() {  eval $($LMOD_DIR/ml_cmd "$@")
#   BASH_FUNC_module%%=() {  eval `/opt/lib/modulecmd bash $*`
# so we also filter variables with parens or % in the name.

# extglob enables +() notation in patterns of ${parameter/pattern/string} notation
shopt -s extglob

# convert NUL-delimited environment key-value pairs in an array
# -d '' means "use NUL byte as delimiter"
# -t means "strip the delimiter"
mapfile -d '' -t kvps < <(env -0)

# iterate through each key-value pair in the array
options="$(
    for kvp in "${kvps[@]}"; do
        # Variable name is what comes before the first '='
        var="${kvp/=*/}"
        # Variable content starts after the first '='
        val="${kvp/#+([^=])=/}"

        # Filter names we shouldn't forward
        if [[ $var =~ ^(_|HOME|TERM|LANG|LC_.*|HASHED_PASSWORD)$ ]]; then
            continue
        fi

        # For slurm: filter variables with %% or () in the name
        if [[ $var =~ (%%|\(\)) ]]; then
            continue
        fi

        # Convert any explicit newline to \n
        val="${val//$'\n'/'\n'}"
        # Backslash-escape quotes so that sshd works.
        val="${val//\"/\\\"}"
        # Backslash-escape backslashes so that sed doesn't interpret them.
        val="${val//\\/\\\\}"
        # Backslash-escape forward slashes so that sed works.
        val="${val////\\/}"
        echo -n "environment=\"$var=$val\","
    done
)"

# In k8s, the files we inject into the container are injected via individual
# file-level bind mounts, which are effectively read-only in docker, so we are
# unable to edit authorized_keys in place.
unmodified="/run/determined/ssh/authorized_keys_unmodified"
modified="/run/determined/ssh/authorized_keys"
sed -e "s/^/$options /" "$unmodified" >"$modified"
# Ensure permissions are restrictive enough for ssh
chmod 600 "$modified"

# sshd runs as the task's user, so it cannot write the login records (utmp/wtmp) of sessions with a
# terminal and logs this line when each such session starts and ends. No sshd_config option turns
# it off, and a higher LogLevel would also hide the line that the readiness check waits for. Drop
# exactly that line and pass every other line through unchanged, one at a time as it arrives. sshd
# ends its log lines with "\r\n". Plain bash, so that it works in any image.
drop_login_records_message() {
    local message="Attempt to write login records by non-root user (aborting)"
    local line
    while IFS= read -r line; do
        if [[ ${line%$'\r'} != "$message" ]]; then
            printf '%s\n' "$line"
        fi
    done
    # A last line without a newline.
    if [[ -n $line && ${line%$'\r'} != "$message" ]]; then
        printf '%s' "$line"
    fi
}

READINESS_REGEX="Server listening on"

# sshd's log goes through the filter, then to tee, which copies it to the task log and to the
# readiness check. The filter and tee ignore the signals that stop sshd, so that they pass on its
# last lines, and each helper waits for the one it started, so that none outlives this shell.
ssh_log() {
    trap '' INT TERM HUP
    drop_login_records_message > >(ssh_log_tee)
    wait "$!"
}

ssh_log_tee() {
    tee -p >(ssh_ready_check) >&2
    wait "$!"
}

ssh_ready_check() {
    trap - INT TERM HUP
    exec "$DET_PYTHON_EXECUTABLE" /run/determined/check_ready_logs.py --ready-regex "$READINESS_REGEX" "${service_ready_args[@]}"
}

# The group makes the log helper a child of this shell rather than of sshd.
{ /usr/sbin/sshd "$@"; } 2> >(ssh_log)
