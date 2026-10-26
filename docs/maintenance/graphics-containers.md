# NVIDIA graphics in Docker tasks

CUDA task containers explicitly select the `nvidia` OCI runtime and request
only Docker's `gpu` device capability. This leaves
`NVIDIA_DRIVER_CAPABILITIES` under the control of the task configuration or
image, instead of replacing it with `compute,utility`. Requests such as
`compute,utility,graphics,display` and `all` therefore reach the NVIDIA runtime.
When neither the task nor image sets the variable, NVIDIA's default remains
`compute,utility`.

The NVIDIA runtime adds the Vulkan ICD, driver libraries, and render-device
mounts when `graphics` or `display` is requested. CPU-only tasks keep their
existing runtime.

## Configure each GPU agent host before upgrading

The NVIDIA Container Toolkit and its `nvidia-container-runtime` executable must
be installed. Register that runtime with Docker before starting the new agent:

```sh
sudo cp -a /etc/docker/daemon.json /etc/docker/daemon.json.before-graphics
sudo nvidia-ctk runtime configure --runtime=docker
sudo dockerd --validate --config-file=/etc/docker/daemon.json
sudo systemctl reload docker
docker info --format '{{json .Runtimes}} {{.DefaultRuntime}}'
```

If `daemon.json` does not exist, omit the backup command and record that it was
absent. Preserve all existing daemon settings. Do not pass `--set-as-default`:
the agent selects `nvidia` only for CUDA tasks, so the Docker default can stay
`runc`. Docker supports reloading the runtime registry without restarting the
daemon or existing containers.

An unregistered `nvidia` runtime causes a new GPU container to fail at creation
with `unknown or invalid runtime name: nvidia`. A plain Docker `--gpus` request
can expose CUDA while omitting the Vulkan ICD and render-device mounts;
requesting `graphics` without the NVIDIA runtime can be insufficient.

## Task permissions and images

The NVIDIA driver capabilities select driver components; they are separate from
Linux capabilities such as `CAP_SYS_ADMIN`. This change adds no privileged
mode, Linux capabilities, Docker socket mount, or access to unallocated GPUs.
GPU UUIDs still come from the task's allocated devices. Users do not need
membership in the host's `docker` group. The agent retains its existing Docker
socket access to create task containers.

For a headless graphics task, set the following in the image or task config:

```yaml
environment:
  environment_variables:
    - NVIDIA_DRIVER_CAPABILITIES=compute,utility,graphics
```

Task images also need a Vulkan loader and the application or diagnostic tool,
for example `libvulkan1` and `vulkan-tools` on Ubuntu. Driver components come
from the host. A headless `vulkaninfo --summary` needs neither X11 access nor
the `display` capability. Warnings about an unset `DISPLAY` or
`XDG_RUNTIME_DIR` can occur when the tool skips window-system surfaces.

For an isolated host-level smoke test, replace the image and allocated GPU UUID:

```sh
docker run --rm --runtime=nvidia --gpus device=GPU-allocated \
  -e NVIDIA_DRIVER_CAPABILITIES=compute,utility,graphics \
  --user 1001:1001 --cap-drop ALL --network none \
  --entrypoint vulkaninfo <image-with-vulkan-tools> --summary
```

## Running tasks during deployment

Use the [rolling agent replacement procedure](hot-upgrade.md#replace-the-agents).
Keep each agent's ID, pool, mounts, environment, and GPU UUID allowlist unchanged.
Stage the new image before stopping the old agent, then verify that running
task containers retain their IDs, process IDs, and start times after reattachment.

Reattaching an existing task does not recreate its container or add driver
mounts. Existing tasks keep running with their original capabilities; the new
behavior applies to newly created GPU task containers.
