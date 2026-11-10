# Install and deploy version 0.44.0

Use the [0.44.0 GitHub release](https://github.com/WU-CVGL/determined/releases/tag/0.44.0)
and its tag as the version reference. The matching master and agent images are
published to GHCR under `ghcr.io/wu-cvgl/determined-master:0.44.0` and
`ghcr.io/wu-cvgl/determined-agent:0.44.0`. Pin the images to the release's
published digests when preparing a deployment. Master and agent do not check
each other's version, and their messages are the same from 0.41.0 to 0.44.0, so
0.41.0 to 0.42.2 agents run under a 0.44.0 master; `gpu_groups` works with them,
since the master sets `DET_GPU_GROUPS`. 0.41.0 changed the agent: it reads the GPU
topology through NVML and sends it when it connects, which a 0.40.1 master
ignores. 0.42.2 changes the agent again: it starts CUDA tasks with the `nvidia`
runtime, which Docker on each GPU host must have registered before the 0.42.2
agent starts (see [upgrade with running tasks](hot-upgrade.md)), and it
reattaches containers by GPU UUID. 0.42.0 adds one database migration, for
resource pool access, and a 0.41.1 master starts on the migrated database.
0.42.1 to 0.42.4 add no migration, so a rollback to 0.42.1 is a binary swap;
end the experiments that set `gpu_groups` and remove it from templates and config
policies first, since 0.42.1 refuses configs that set it. 0.42.3 and 0.42.4
change only the WebUI. 0.42.5 adds optional list API fields and an index on
completed queue events for Started timestamps and sorting; it does not change
the agent. Agents can stay on 0.42.2. The 0.42.4 master starts on the indexed
database, so rollback to 0.42.4 is a binary swap of the master. 0.43.0 does not
change the agent either, so agents stay on 0.42.2. Its two migrations delete
the notebook records and move `notebook_idle_type` to
`services.jupyter.idle_type` in stored configurations and templates, so a
rollback to 0.42.5 restores the database backup taken before the upgrade; a
0.42.5 master also refuses the `interactive` section of the master
configuration. 0.43.1 changes the master, the WebUI and the harness, with no
agent change, database migration or API change. A rollback to 0.43.0 is a swap
of the master image; first end the TensorBoard tasks and the shells with the
`tensorboard` service that started under 0.43.1, since a 0.43.0 master does not
send their token.

0.44.0 does not change the agent, so agents stay on 0.42.2; the files that the
master ships into task containers change only for tasks started after the
upgrade. Shells, commands and trials that run at the swap continue under the
0.44.0 master, and a shell's JupyterLab and VS Code links keep working. 0.44.0
removes the TensorBoard task type, and the upgrade deletes the TensorBoard
tasks' records.
Before the swap, remove `tensorboard_timeout` from the master configuration,
since the 0.44.0 master refuses to start with it, and end every TensorBoard
task: as an administrator under 0.43.1, kill the tasks that
`det tensorboard list --all` lists until it shows each one as `TERMINATED`.
Then back up the database. The first start runs seven migrations before the
master accepts agents, so they count toward the master outage; the master then
rebuilds the cluster usage history, keeping the earlier aggregates in
`resource_aggregates_legacy`. A TensorBoard task still running at the swap
keeps its container running with no record in Determined. After the start, run
this on every agent host and `docker kill` each container it prints:

```sh
for c in $(docker ps -q); do
  docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$c" |
    grep -qx 'DET_TASK_TYPE=TENSORBOARD' && echo "$c"
done
```

Allocations from before the upgrade, including those still running at the
swap, count as GPUs in the user usage only when their task container recorded
GPUs; the administrator statements in
[historical cluster usage data](../manage/historical-cluster-usage-data.rst)
classify the rest. The 0.44.0 master refuses task configurations and templates
that set `tensorboard_args`. A rollback restores the database backup taken
before the upgrade, since the release deletes the TensorBoard task records,
drops `notebook_sessions` and moves `tensorboard_args`. A 0.43.1 master started
on the upgraded database without the restore runs tasks but cannot start a
shell with the JupyterLab service; the tasks it runs are never in the user
usage after upgrading again, the cluster usage days it aggregates use the old
rule, and it does not enforce job privacy, so every job's title, logs and
config are readable again.

## Install the CLI from the repository

Use a Python environment for the CLI and install from the release tag. The
`VERSION` build input sets the wheel's version; a plain install from a Git
checkout can otherwise report the repository's development version.

```sh
python3 -m venv .venv
. .venv/bin/activate
VERSION=0.44.0 python -m pip install --upgrade \
  'git+https://github.com/WU-CVGL/determined.git@0.44.0#subdirectory=harness'
det --version
```

Use the `main` ref only if you intentionally want development code; it may not
match a published image. Install the CLI on operator machines, and use task
images with the SDK version required by each workload. Installing a new CLI
does not replace the SDK inside running tasks.

## Pull and deploy the images

```sh
docker pull ghcr.io/wu-cvgl/determined-master:0.44.0
docker pull ghcr.io/wu-cvgl/determined-agent:0.44.0
```

`det deploy local master-up` and `agent-up` from the 0.44.0 CLI use these
images by default: the image repository defaults to `ghcr.io/wu-cvgl` and
`--det-version` to the CLI's version. A 0.40.1 or older CLI defaults to the
upstream `determinedai` images, which do not exist for this fork's versions;
pass it `--image-repo-prefix ghcr.io/wu-cvgl --det-version 0.44.0`. Images
built from source are tagged `determinedai/...` and need
`--image-repo-prefix determinedai`.

Update the master and agent image references in your deployment to those
exact tags or, preferably, their verified digests. Use the same deployment
method and configuration as the existing cluster; see the
[upgrade procedure](../manage/upgrade.rst) and the
[on-premises deployment options](../setup-cluster/on-prem/options/deploy.rst).
The master needs its PostgreSQL connection and writable checkpoint/cache
storage. Agents need network access to the master and their container runtime.
For CPU-only agents, configure `slot_type: cpu`. A Docker socket mount grants
control of its host and belongs only on trusted agent machines.

A live cluster can be upgraded in two ways. For a cold upgrade, disable
agents and wait for running tasks to checkpoint and stop, then take a
PostgreSQL backup. Start the new master, confirm database migration and login,
then reconnect or update agents and verify tasks, metrics, and checkpoints. To
keep tasks running, take the backup, replace the master, and then replace the
agents one node at a time, as described in
[upgrade with running tasks](hot-upgrade.md). Tasks that write output are
killed after about 11 minutes without a master; keep master outages well under
ten minutes. The procedure was rehearsed with CPU tasks. See
[task continuity](task-continuity.md) for the reconnect limits and a
disposable diagnostic.

## Rollback and source builds

Keep the previous image references and a database backup taken before the
upgrade. When the previous master starts against the migrated database, as
0.41.1 does after the 0.42.0 migration, a rollback is a binary swap that keeps tasks running; [upgrade with
running tasks](hot-upgrade.md) lists its conditions. Otherwise, stop agents and
the master, restore the compatible backup, then start the previous master and
agents. Switching images does not reverse database migrations. Test backup
restore and task/checkpoint visibility in a disposable environment first.

The release tag identifies the source used for the images. The
[`Fork release` workflow](https://github.com/WU-CVGL/determined/blob/main/.github/workflows/fork-release.yml)
defines the Linux amd64 build on a GitHub-hosted runner. It publishes
PR candidates under separate `0.44.0-rc.<run-id>` tags and publishes the final
version from the corresponding Git tag. Confirm the GHCR packages are public
before using the unauthenticated pull commands above. For a manual
source build, check out the release tag and set both `VERSION` and
`FORK_VERSION` to `0.44.0` so binaries, wheel, and WebUI agree; a local build
is not published automatically. Run the
[`tools/fork/smoke.sh`](https://github.com/WU-CVGL/determined/blob/main/tools/fork/smoke.sh) CPU packaging check for a
new build before deploying it; it does not validate GPU workloads.
