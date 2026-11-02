#!/usr/bin/env bash

# The process of an interactive task, ship_logs.py's only child. It runs the container init once,
# then starts the services named in DET_SERVICES (comma-separated, the primary service first) from
# their units in /run/determined/services, waits for them and stops them:
#
# - A unit is sourced, with "set -e", in the service's own subshell, so that the service inherits
#   everything the init and the startup hooks set. It runs its service in the foreground, last. The
#   primary unit gets the supervisor's arguments; the others get none.
# - Before the init, each unit is also sourced in the supervisor's shell with
#   supervisor_phase=pre-init. A unit then only sets the defaults that the startup hooks may
#   change, such as SHELL, and returns; one without any starts with
#   "[[ ${supervisor_phase-} != pre-init ]] || return 0".
# - A unit passes "${service_ready_args[@]}" to check_ready_logs.py: only the primary service's
#   readiness marks the task ready, and each service's is logged.
# - The task ends with the primary service's exit status when it exits; the others are stopped.
#   A service that is not the primary one does not end the task.
# - SIGINT, SIGTERM and SIGHUP stop all services, and the supervisor exits as if killed by the
#   signal.
# - Each service runs in its own process group, which a stop signals: SIGTERM (or the signal that
#   the supervisor got), then SIGKILL after supervisor_stop_grace_seconds.
# - A service has stopped once its process group is empty. When its unit's subshell exits, with
#   the service's exit status, the supervisor stops what is left of the group as a stop does and
#   logs the exit once the group is empty. The subshell only stops its own helpers, since it
#   cannot kill its own group.
# - Processes that leave the group on purpose (setsid) belong to the container's lifetime.

source /run/determined/task-setup.sh

set -e

# The supervisor's own variables carry its prefix, so that the startup hooks do not change them.
supervisor_units=/run/determined/services
supervisor_stop_grace_seconds=10
supervisor_log_drain_seconds=2

IFS=, read -r -a supervisor_services <<<"${DET_SERVICES-}"
if ((${#supervisor_services[@]} == 0)); then
    echo "DET_SERVICES is empty" >&2
    exit 1
fi
declare -A supervisor_seen=()
for supervisor_name in "${supervisor_services[@]}"; do
    if [[ ! $supervisor_name =~ ^[a-z][a-z0-9_-]*$ || ! -f $supervisor_units/$supervisor_name.sh ||
        -n ${supervisor_seen[$supervisor_name]-} ]]; then
        printf 'DET_SERVICES: invalid service "%s"\n' "$supervisor_name" >&2
        exit 1
    fi
    supervisor_seen[$supervisor_name]=1
done
supervisor_primary=${supervisor_services[0]}

supervisor_phase=pre-init
for supervisor_name in "${supervisor_services[@]}"; do
    source "$supervisor_units/$supervisor_name.sh"
done
unset supervisor_phase

source /run/determined/interactive-init.sh

# From here on the supervisor handles failures itself; each unit sets "set -e" again.
set +e

# Wait up to $2 seconds for the process $1, a child of this shell, to exit.
wait_for_child() {
    local i
    for ((i = 0; i < $2 * 10; i++)); do
        kill -0 "$1" 2>/dev/null || return 0
        sleep 0.1
    done
    ! kill -0 "$1" 2>/dev/null
}

# Kill the descendants of the process $1, the deepest first, so that each parent reaps its children
# before it is killed itself.
kill_descendants() {
    local tasks child i
    local -a ids children=()
    for tasks in /proc/"$1"/task/*/children; do
        [[ -r $tasks ]] || continue
        ids=()
        read -r -a ids 2>/dev/null <"$tasks"
        children+=("${ids[@]}")
    done
    for child in "${children[@]}"; do
        kill_descendants "$child"
        kill -KILL "$child" 2>/dev/null
        for ((i = 0; i < 10; i++)); do
            [[ -e /proc/$child ]] || break
            sleep 0.1
        done
    done
}

# The EXIT trap of a service's subshell: once the service has exited, stop the unit's background
# helpers and give its log helper a moment to pass on the last lines. A log helper that still
# waits for input is held up by what is left of the service's process group, which is stopped
# then, or by a process outside it, and is then killed, so that nothing outlives the subshell
# (ship_logs.py reaps nothing).
service_exited() {
    local status=$?
    set +e
    # A unit that ends inside its redirection, as "set -e" does, would keep its log helper's input
    # open: restore the output and close the other descriptors.
    exec 1>&"$service_stdout" 2>&"$service_stderr"
    local fd
    for fd in /proc/"$BASHPID"/fd/*; do
        fd=${fd##*/}
        if ((fd > 2 && fd < 255 && fd != service_stdout && fd != service_stderr)) \
            && [[ -e /proc/$BASHPID/fd/$fd ]]; then
            exec {fd}>&-
        fi
    done
    local helpers pid
    helpers=$(jobs -pr)
    for pid in $helpers; do
        kill "$pid" 2>/dev/null
        wait "$pid" 2>/dev/null
    done
    if [[ ${!-} != "$service_inherited_pid" ]]; then
        pid=$!
        if ! wait_for_child "$pid" "$supervisor_log_drain_seconds"; then
            kill -TERM -- "-$BASHPID" 2>/dev/null
            if ! wait_for_child "$pid" "$supervisor_log_drain_seconds"; then
                # Held up from outside the group, as by an sshd session: kill the log helper.
                kill_descendants "$pid"
                kill -KILL "$pid" 2>/dev/null
            fi
        fi
        wait "$pid" 2>/dev/null
    fi
    exit "$status"
}

declare -A service_pids=()

start_service() {
    local name=$1 is_primary=$2
    shift 2
    printf '[%s] start\n' "$name" >&2
    (
        # Outlive the service when a stop signals the process group, and end with its status.
        trap : INT TERM HUP
        service_inherited_pid=${!-}
        exec {service_stdout}>&1 {service_stderr}>&2
        trap service_exited EXIT
        service_ready_args=(--service "$name")
        if [[ -z $is_primary ]]; then
            service_ready_args+=(--no-post)
        fi
        set -e
        source "$supervisor_units/$name.sh" "$@"
    ) &
    service_pids[$name]=$!
}

# Print the processes of the process group $1, without zombies, which have exited and only wait
# for their parent.
group_members() {
    local stat line state
    # Fails for a group without processes, also without zombies.
    kill -0 -- "-$1" 2>/dev/null || return 0
    for stat in /proc/[0-9]*/stat; do
        line=
        # The whole file, since comm may contain a newline; read fails at its end.
        read -r -d '' line 2>/dev/null <"$stat"
        # pid (comm) state ppid pgrp ...; comm may contain ") ".
        line=${line##*) }
        state=${line%% *}
        line=${line#* }
        line=${line#* }
        if [[ ${line%% *} == "$1" && $state != [ZX] ]]; then
            stat=${stat%/stat}
            printf '%s\n' "${stat#/proc/}"
        fi
    done
}

declare -A service_status=()

# Whether the service $1 has stopped: its subshell has exited, which saves its status in
# service_status, and its process group is empty.
service_stopped() {
    local pid=${service_pids[$1]}
    if [[ -z ${service_status[$1]-} ]]; then
        kill -0 "$pid" 2>/dev/null && return 1
        wait "$pid" 2>/dev/null
        service_status[$1]=$?
    fi
    [[ -z $(group_members "$pid") ]]
}

# Wait up to $1 seconds for the services in supervisor_stopping to stop, or until a stop signal
# when $2 is set. Log each that stops and keep the others in supervisor_stopping.
await_services() {
    local name i
    local -a left
    for ((i = 0; ; i++)); do
        left=()
        for name in "${supervisor_stopping[@]}"; do
            if service_stopped "$name"; then
                printf '[%s] exited %d\n' "$name" "${service_status[$name]}" >&2
                unset "service_pids[$name]" "service_status[$name]"
            else
                left+=("$name")
            fi
        done
        supervisor_stopping=("${left[@]}")
        if ((${#left[@]} == 0 || i >= $1 * 10)) || [[ -n $2 && -n $stop_signal ]]; then
            return 0
        fi
        sleep 0.1
    done
}

# Stop the services $3... that run or have a process group left: the signal $1 to each group,
# SIGKILL after the grace. With $2 set, a stop signal ends the grace, and the services stay
# tracked for the stop that follows.
stop_services() {
    local signal=$1 interruptible=$2 name
    shift 2
    supervisor_stopping=()
    for name; do
        if [[ -n ${service_pids[$name]-} ]]; then
            supervisor_stopping+=("$name")
            kill -s "$signal" -- "-${service_pids[$name]}" 2>/dev/null
        fi
    done
    await_services "$supervisor_stop_grace_seconds" "$interruptible"
    if [[ -n $interruptible && -n $stop_signal ]]; then
        return 0
    fi
    # Wait for each killed subshell at once, before bash reports it as a killed job.
    for name in "${supervisor_stopping[@]}"; do
        kill -s KILL -- "-${service_pids[$name]}" 2>/dev/null
        if [[ -z ${service_status[$name]-} ]]; then
            wait "${service_pids[$name]}" 2>/dev/null
            service_status[$name]=$?
        fi
    done
    await_services 1 ""
    # Only a process that cannot be killed is left; the service has exited all the same.
    for name in "${supervisor_stopping[@]}"; do
        if [[ -z ${service_status[$name]-} ]]; then
            wait "${service_pids[$name]}" 2>/dev/null
            service_status[$name]=$?
        fi
        printf '[%s] exited %d\n' "$name" "${service_status[$name]}" >&2
        unset "service_pids[$name]" "service_status[$name]"
    done
}

# Stop the services whose subshells have exited, and log each. The primary one ends the task and
# stops the others. The supervisor's waits drop bash's reports of killed jobs, which are not part
# of the task log. A scan starts over after a stop, which takes time.
reap_services() {
    local name status again=1
    while [[ -n $again ]]; do
        again=
        for name in "${supervisor_services[@]}"; do
            if [[ -z ${service_pids[$name]-} ]] || kill -0 "${service_pids[$name]}" 2>/dev/null; then
                continue
            fi
            if [[ $name == "$supervisor_primary" ]]; then
                service_stopped "$name"
                status=${service_status[$name]}
                stop_services TERM "" "${supervisor_services[@]}"
                exit "$status"
            fi
            stop_services TERM 1 "$name"
            [[ -z $stop_signal ]] || return 0
            again=1
            break
        done
    done
}

stop_signal=
trap 'stop_signal=${stop_signal:-INT}' INT
trap 'stop_signal=${stop_signal:-TERM}' TERM
trap 'stop_signal=${stop_signal:-HUP}' HUP

# Job control puts each service in its own process group.
set -m

start_service "$supervisor_primary" 1 "$@"
for supervisor_name in "${supervisor_services[@]:1}"; do
    start_service "$supervisor_name" ""
done

while [[ -z $stop_signal ]]; do
    # Returns when a service exits or a trapped signal arrives.
    wait -n 2>/dev/null
    reap_services
done

stop_services "$stop_signal" "" "${supervisor_services[@]}"
exit $((128 + $(kill -l "$stop_signal")))
