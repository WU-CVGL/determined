# Upgrade with running tasks

This fork's master and agents can be replaced while tasks run, when the
release allows it. This page describes the master swap, the agent
replacement, the time limits that decide whether running tasks survive, and a
rollback by starting the previous master again. The upgrade from 0.40.1 to
0.41.0 serves as the example; for another release, check its release notes and
schema migrations against the conditions below. The
[upgrade procedure](../manage/upgrade.rst) describes the cold upgrade, which
stops all work first.

The procedure was rehearsed on a copy of a Docker-based deployment with
CPU agents, PostgreSQL 10, trials that use the Core API, and commands. GPU
reservations, browser sessions across a rollback, and the startup time of a
large database were not part of it.

## When a hot upgrade applies

- **No version gate.** The master accepts agents of any version; it only
  records the version that an agent reports, which `det agent list` shows. The
  CLI prints a warning when its version differs from the master's and works
  on. The agent's code and its messages to and from the master (`agent/`,
  `master/pkg/aproto`, `master/pkg/cproto`) are the same in 0.41.0, 0.41.1 and
  0.42.0. 0.41.0 added the GPU topology to the message an agent sends when it
  connects, which a 0.40.1 master ignores. A rolling agent upgrade runs with
  mixed versions: 0.40.1 agents ran under a 0.41.0 master and reattached
  running tasks after agent and master restarts, and a 0.41.0 agent kept its
  running tasks under the 0.40.1 master after a rollback.
- **Running tasks keep their SDK.** An upgrade does not replace the SDK inside a
  running task container. Trials with the 0.40.1 SDK continued across the
  master and agent upgrade.
- **Short, additive migrations.** The new master migrates the database at
  startup, within the outage. The previous master must still start against the
  migrated database for a rollback by binary swap. The migration of 0.41.0
  is described below.
- **Agents run under a restart policy.** An agent that cannot reach the
  master for about 145 seconds exits, and a restarted agent exits at once
  while the master is still down. Only a restart policy that keeps restarting
  without a limit, such as Docker's `--restart unless-stopped`, brings it back.
  A systemd unit, including the packaged `determined-agent.service`, also needs
  `RestartSec=` of several seconds: with the default 100 ms, systemd stops
  restarting it after five starts within ten seconds.
- **The new CLI.** Use the CLI of the new release for administration. The
  0.40.1 CLI cannot create dynamic pools, because it omits the JSON content
  type, and has no `update` or `adopt` command. Users of CLIs and SDKs from
  0.40.1 or earlier cannot change their own password or username against a
  0.41.0 master.

## Outage budget

The time without a master decides what survives. Measured from the moment the
master stops:

| Time without a master | What happens |
| --- | --- |
| Up to about 145 s | Agents retry every five seconds, 30 times by default. Task containers keep running. A trial blocks in a Core API call that must reach the master, such as a checkpoint report; metrics are queued and delivered afterward, without gaps or duplicates. Task output is buffered in the container. A trial that makes no Core API call keeps computing. |
| About 145 s | Agents log `exhausted reconnect attempts` and exit; their task containers keep running. A restart policy restarts them with a growing delay, capped at about one minute by Docker; each restarted agent exits at once until the master is back. |
| About 661 s (11 minutes) after a task's first failed log upload | The task's log shipper gives up and kills the task, whatever `agent_reconnect_wait` is: exit code 80, `RuntimeError: failure in log shipper; shipper thread died` in the container output. A task that writes no output during the outage is not affected. |
| About 26 minutes | The SDK's retries of a Core API call (`Retry(total=20, backoff_factor=0.5)`, or `DET_RETRY_CONFIG`) run out. For tasks that write output, the log shipper limit comes first. |

When the master returns:

- Agents that are still retrying reconnect within five seconds; agents that
  exited reconnect at their next restart, within about a minute. The master
  reattaches every task container that still runs, with the same allocation,
  container, and process.
- A trial blocked in a Core API call resumes at its next retry, up to about
  120 seconds after the master is back.
- For the agents it restores, the master counts `agent_reconnect_wait` from its
  own start. The setting bounds how long the master waits for agents after it
  is back, not how long it may be away. The default 150 seconds covers the
  restart delay of agents that exited.
- A task that its log shipper killed is gone: the master reports `RM failed to
  restore the allocation: container is gone on reattachment`. A trial counts
  this as a transient system error and starts a new allocation from its last
  recorded checkpoint, without using up `max_restarts`; the work since that
  checkpoint is lost. A checkpoint that was written to storage but whose report
  was still blocked stays on storage without a database record and is not
  garbage-collected. Commands and shells end as `TERMINATED`.

Keep every master outage well under ten minutes. A swap takes seconds plus the
new master's startup time. Task restore and cleanup can make the startup
longer on a large database, even when the migration is short; restart the
current master once in a quiet period to measure it.

## Migration in 0.41.0

0.41.0 adds one schema migration, `20261005000000_dynamic-resource-pool-spec`:
an `ALTER TABLE` on `dynamic_resource_pools`, which holds one row per dynamic
pool, adding four columns and a check constraint. PostgreSQL 10, which has no
fast column defaults, rewrites that table; at its size this takes
milliseconds. No other table changes, and the database views stay the same.
The new master logs `migrated from 20260925000000 to 20261005000000` and
`database views unchanged`.

The migration adds nullable columns, one column with a default, and a
constraint that the inserts of 0.40.1 satisfy. 0.40.1 starts against the
migrated database, logs `no migrations to apply; version: 20261005000000`,
and ignores the new columns, so a rollback is a binary swap.

## Migration in 0.42.0

0.42.0 adds one schema migration,
`master/static/migrations/20261006061744_add-resource-pool-access.tx.up.sql`:
it creates the tables `resource_pool_restrictions` and `resource_pool_grants`,
which hold pool access, start empty, and have foreign keys to `users`. No
existing table changes, and the database views stay the same. The 0.42.0
release notes list what to remove before a rollback to 0.41.

ROLLBACK: <to be filled after the rollback test>

## Before the upgrade

1. Read the release notes, in particular the breaking changes and the
   settings that only the new release accepts.
2. Pull the new images on the master host and on every agent host. Pulling
   does not touch running containers.
3. Install the new CLI on the operator machine; see
   [install and deploy](distribution.md).
4. Back up while the master runs: a `pg_dump -Fc` of the database, the master
   configuration file and environment file, and the output of
   `det resource-pool list-dynamic --json`. The listing redacts registry
   credentials and serves only to compare configurations later.
5. Record how the master runs: image, configuration file mount, environment
   file, network, network aliases, published port, storage mounts, and restart
   policy (`docker inspect <old-master>`).
6. Check that every agent has a restart policy and a unique agent ID.

## Replace the master

The new master needs the same configuration file, environment (including the
database connection), storage mounts, and address as the old one: agents and
running tasks keep connecting to the same host, port, and network alias. Never
run two masters against one database. With Docker:

```sh
docker stop <old-master>      # keep the container for a rollback
docker run -d --name <new-master> --restart unless-stopped \
    --network <network> -p 8080:8080 --env-file master.env \
    -v "$PWD/master.yaml:/etc/determined/master.yaml" \
    -v <checkpoint-storage>:<checkpoint-storage> \
    ghcr.io/wu-cvgl/determined-master@<digest> --config-file /etc/determined/master.yaml
until curl -fsS http://localhost:8080/info >/dev/null; do sleep 1; done
docker logs <new-master> 2>&1 | grep -E 'Determined master|migrated from|no migrations|views'
```

Add every other option that the old container had, such as `--network-alias`.
A container stopped under `--restart unless-stopped` stays stopped, also when
the Docker daemon restarts; one with `--restart always` starts again with the
daemon, so change its policy with `docker update --restart no <old-master>`.

The log shows the new version, the migration, and `database views unchanged`.
Each restored allocation adds a warning `added allocation <id> without a job
submission time`, which is expected. Then verify:

```sh
det master info                   # the new version
det agent list                    # every agent back and enabled, same container counts
det resource-pool list-dynamic    # every dynamic pool Ready
det experiment list; det task list
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/det/   # 200
```

For running trials, check that the allocation ID and the container are
unchanged and that steps, metrics, and checkpoints advance. The diagnostic in
[task continuity](task-continuity.md) runs these checks for a disposable
workload.

## Replace the agents

Replace the agents one node at a time, at any time after the master. On each
node, remove the old agent container and start the new image with the same
options: agent ID, resource pool, master host and port, container runtime
socket, and restart policy.

```sh
docker stop <agent>; docker rm <agent>
docker run -d --name <agent> <same options as before> ghcr.io/wu-cvgl/determined-agent:0.41.0
```

The stopped agent leaves its task containers running (`orphaning container`
in its log). The new agent reattaches them, and the master logs `going to try
to reattach containers`; a replacement connects within seconds when its image
was pulled beforehand. It must reconnect within the pool's
`agent_reconnect_wait`. A `docker restart` of an agent container reattaches the
same way. Never start the new container before the old one is removed: two
connections with one agent ID conflict. For agents that
`det deploy local agent-up` created, `agent-down` with the agent's name,
followed by `agent-up` with the same options and `--det-version 0.41.0`,
replaces the agent container and leaves task containers running.

An agent whose resource pool does not exist exits and, under a restart policy,
keeps restarting until the pool exists. Create or adopt a dynamic pool before
starting its agents.

`det agent list` shows each agent's version. Running work stays where it is;
check its allocations as after the master swap.

## Roll back

A rollback starts the previous master again against the migrated database. For
0.40.1, this works when:

- no `master.yaml` pool, including the built-in `default` pool that an omitted
  `resource_pools` key adds, shares a name with a saved dynamic pool, and
  `master.yaml` does not set `resource_pools: []` (see the rollback section of
  [dynamic resource pools](dynamic-pools.md));
- `master.yaml` contains no setting that only 0.41.0 knows, such as
  `shell_terminal`, `security.trusted_proxies`, `security.session_cookie`, or
  `security.csrf`. The master refuses to start with an unknown key.

```sh
docker exec <db> pg_dump -U postgres -Fc determined > determined-pre-rollback.dump
docker stop <new-master>
docker start <old-master>
```

0.40.1 logs `no migrations to apply`, runs every dynamic pool from its saved
effective configuration, and agents and running tasks carry on, as after the
upgrade. Agents already replaced with 0.41.0 can stay on it. A 0.41.0 CLI
shows an empty `Revision` column and `-` under `Active` in
`det resource-pool list-dynamic` against it. Features of 0.41.0, such as
browser terminals and generic task names, are not available. Roll forward with
`docker stop <old-master>` and `docker start <new-master>`. Once the new
release has proven itself, remove the old container.

A release whose migrations the previous master cannot run against needs the
cold rollback in [install and deploy](distribution.md): stop agents and the
master, restore the backup, and start the previous versions.

## Converting pools in the same window

Converting `master.yaml` pools to dynamic pools, as described in
[dynamic resource pools](dynamic-pools.md), needs one more master restart
after the upgrade. Upgrade first with `master.yaml` unchanged, then follow the
conversion. The restart is a hot restart like the upgrade: the same outage
budget applies, and running tasks in the converted pools continue. Note:

- The first conversion route, which omits `resource_pools`, keeps the binary
  rollback; the second, `resource_pools: []`, ends it.
- Give a dynamic pool saved without a spec its full configuration when
  updating it, including `agent_reconnect_wait`; a setting left out falls back
  to its built-in default at the next start.
- A pool-level `task_container_defaults` block resets `shm_size_bytes`,
  `network_mode`, and `preemption_timeout` to their built-in values unless it
  repeats them.
- A configuration file mounted as a single file keeps the old content in the
  running container when an editor or `mv` replaces the file, and the restart
  picks up the new one. After the restart, check the file inside the container,
  for example `docker exec <master> grep -c pool_name /etc/determined/master.yaml`.
