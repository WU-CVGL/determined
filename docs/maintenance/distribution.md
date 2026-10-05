# Install and deploy version 0.41.0

Use the [0.41.0 GitHub release](https://github.com/WU-CVGL/determined/releases/tag/0.41.0)
and its tag as the version reference. The matching master and agent images are
published to GHCR under `ghcr.io/wu-cvgl/determined-master:0.41.0` and
`ghcr.io/wu-cvgl/determined-agent:0.41.0`. Pin the images to the release's
published digests when preparing a deployment. Master and agent do not check
each other's version, and the agent's code and its messages to and from the
master are the same in 0.40.1 and 0.41.0: 0.40.1 agents ran under a 0.41.0
master during a rolling agent upgrade, and a 0.41.0 agent under the 0.40.1
master after a rollback.

## Install the CLI from the repository

Use a Python environment for the CLI and install from the release tag. The
`VERSION` build input sets the wheel's version; a plain install from a Git
checkout can otherwise report the repository's development version.

```sh
python3 -m venv .venv
. .venv/bin/activate
VERSION=0.41.0 python -m pip install --upgrade \
  'git+https://github.com/WU-CVGL/determined.git@0.41.0#subdirectory=harness'
det --version
```

Use the `main` ref only if you intentionally want development code; it may not
match a published image. Install the CLI on operator machines, and use task
images with the SDK version required by each workload. Installing a new CLI
does not replace the SDK inside running tasks.

## Pull and deploy the images

```sh
docker pull ghcr.io/wu-cvgl/determined-master:0.41.0
docker pull ghcr.io/wu-cvgl/determined-agent:0.41.0
```

`det deploy local master-up` and `agent-up` from the 0.41.0 CLI use these
images by default: the image repository defaults to `ghcr.io/wu-cvgl` and
`--det-version` to the CLI's version. A 0.40.1 or older CLI defaults to the
upstream `determinedai` images, which do not exist for this fork's versions;
pass it `--image-repo-prefix ghcr.io/wu-cvgl --det-version 0.41.0`. Images
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
0.40.1 does after the 0.41.0 migration, a rollback is a binary swap that keeps
tasks running; [upgrade with running tasks](hot-upgrade.md) lists its
conditions. Otherwise, stop agents and the master, restore the compatible
backup, then start the previous master and agents. Switching images does not
reverse database migrations. Test backup restore and task/checkpoint
visibility in a disposable environment first.

The release tag identifies the source used for the images. The
[`Fork release` workflow](https://github.com/WU-CVGL/determined/blob/main/.github/workflows/fork-release.yml)
defines the Linux amd64 build on a GitHub-hosted runner. It publishes
PR candidates under separate `0.41.0-rc.<run-id>` tags and publishes the final
version from the corresponding Git tag. Confirm the GHCR packages are public
before using the unauthenticated pull commands above. For a manual
source build, check out the release tag and set both `VERSION` and
`FORK_VERSION` to `0.41.0` so binaries, wheel, and WebUI agree; a local build
is not published automatically. Run the
[`tools/fork/smoke.sh`](https://github.com/WU-CVGL/determined/blob/main/tools/fork/smoke.sh) CPU packaging check for a
new build before deploying it; it does not validate GPU workloads.
