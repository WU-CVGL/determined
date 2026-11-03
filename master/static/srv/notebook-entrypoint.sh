#!/usr/bin/env bash

# A notebook task: the supervisor with JupyterLab as its only service.
DET_SERVICES=jupyter exec /run/determined/interactive-supervisor.sh "$@"
