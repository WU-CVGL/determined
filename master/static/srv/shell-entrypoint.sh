#!/usr/bin/env bash

# A shell task: the supervisor with sshd as its only service. The arguments go to sshd.
DET_SERVICES=ssh exec /run/determined/interactive-supervisor.sh "$@"
