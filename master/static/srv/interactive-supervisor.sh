#!/usr/bin/env bash

# The process of an interactive task, ship_logs.py's only child. It runs the container init once,
# then starts the services named in DET_SERVICES (comma-separated, the primary service first) from
# their units in /run/determined/services, waits for them and stops them:
#
# - A unit is sourced, with "set -e", in the service's own subshell, so that the service inherits
#   everything the init and the startup hooks set. It runs its service in the foreground, last. The
#   primary unit gets the supervisor's arguments; the others get none.
# - A unit passes "${service_ready_args[@]}" to check_ready_logs.py: only the primary service's
#   readiness marks the task ready, and each service's is logged.
# - The task ends with the primary service's exit status when it exits; the others are stopped.
#   A service that is not the primary one only logs its exit.
# - SIGINT, SIGTERM and SIGHUP stop all services, and the supervisor exits as if killed by the
#   signal.
# - Each service runs in its own process group, which a stop signals: SIGTERM (or the signal that
#   the supervisor got), then SIGKILL after supervisor_stop_grace_seconds.

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

# Reap the services that have exited and log each. The primary one ends the task. The supervisor's
# waits drop bash's reports of killed jobs, which are not part of the task log.
reap_services() {
    local name pid status
    for name in "${!service_pids[@]}"; do
        pid=${service_pids[$name]}
        if kill -0 "$pid" 2>/dev/null; then
            continue
        fi
        wait "$pid" 2>/dev/null
        status=$?
        unset "service_pids[$name]"
        printf '[%s] exited %d\n' "$name" "$status" >&2
        if [[ $name == "$supervisor_primary" ]]; then
            stop_services TERM
            exit "$status"
        fi
    done
}

stop_services() {
    local signal=$1 name i
    for name in "${!service_pids[@]}"; do
        kill -s "$signal" -- "-${service_pids[$name]}" 2>/dev/null
    done
    for ((i = 0; i < supervisor_stop_grace_seconds * 10 && ${#service_pids[@]} > 0; i++)); do
        sleep 0.1
        for name in "${!service_pids[@]}"; do
            if ! kill -0 "${service_pids[$name]}" 2>/dev/null; then
                wait "${service_pids[$name]}" 2>/dev/null
                printf '[%s] exited %d\n' "$name" "$?" >&2
                unset "service_pids[$name]"
            fi
        done
    done
    for name in "${!service_pids[@]}"; do
        kill -s KILL -- "-${service_pids[$name]}" 2>/dev/null
        wait "${service_pids[$name]}" 2>/dev/null
        printf '[%s] exited %d\n' "$name" "$?" >&2
        unset "service_pids[$name]"
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

stop_services "$stop_signal"
exit $((128 + $(kill -l "$stop_signal")))
