.. _agent-config-reference:

###############################
 Agent Configuration Reference
###############################

*****************
 ``config_file``
*****************

Path to the agent configuration file. Normally this should only be set via an environment variable
or command-line option. Defaults to ``/etc/determined/agent.yaml``.

*****************
 ``master_host``
*****************

Required. The hostname or IP address of the Determined master.

*****************
 ``master_port``
*****************

The port of the Determined master. Defaults to ``443`` if TLS is enabled and ``80`` otherwise.

**************
 ``agent_id``
**************

The ID of this agent; defaults to the hostname of the current machine. Agent IDs must be unique
within a cluster.

***************************
 ``container_master_host``
***************************

Master hostname that containers started by this agent will connect to. Defaults to the value of
``master_host``.

***************************
 ``container_master_port``
***************************

Master port that containers started by this agent will connect to. Defaults to the value of
``master_port``.

.. _agent-resource-pool-reference:

*******************
 ``resource_pool``
*******************

Which resource pool the agent should join. Defaults to the value of ``default``, which will work if
and only if there is a resource pool named ``default``. For more information please see
:ref:`resource-pools`.

******************
 ``visible_gpus``
******************

The GPUs that should be exposed as slots by the agent. A comma-separated list of GPUs, each
specified by a 0-based index, UUID, PCI bus ID, or board serial number. The 0-based index of NVIDIA
GPUs or AMD GPUs can be obtained via the ``nvidia-smi`` or ``rocm-smi`` commands.

.. _agent-exclude-gpus:

******************
 ``exclude_gpus``
******************

NVIDIA GPUs that the agent reports but never offers as slots, for example a faulty GPU. A
comma-separated list of GPU UUIDs, never indices (``nvidia-smi -L`` prints the UUIDs). Set it with
the ``exclude_gpus`` key of the agent configuration file or with the ``--exclude-gpus`` flag. There
is no environment variable. Start the agent container with all GPUs: the agent must see an excluded
GPU to report it.

-  An excluded GPU is not a slot: the master never schedules it and no task container gets it. The
   other GPUs keep their ``nvidia-smi`` index as slot ID, so the slot IDs can have gaps, as with
   ``visible_gpus``.

-  An entry that matches no detected NVIDIA GPU stops the agent with an error that names the entry,
   so that a typo never hands the GPU to tasks. If ``nvidia-smi`` fails, no GPU is detected and
   every entry fails this way.

-  With ``slot_type: auto``, excluded GPUs count as found NVIDIA GPUs: an agent whose GPUs are all
   excluded has no slots, and does not fall back to ROCm or CPU slots.

-  Excluded GPUs appear in the agent's :ref:`GPU topology <agent-gpu-topology>`, labelled as
   excluded, with the same measurements as the slots.

-  Changing the list changes the agent's slots. Drain the agent first: the master stops an agent
   whose slots changed when it reconnects.

-  Agents without this option refuse to start with ``exclude_gpus`` in their configuration file or
   with ``--exclude-gpus``. Before rolling an agent back to such a version, remove the option and
   hide the GPU from the agent container instead, for example with ``docker run --gpus
   '"device=<UUIDs of the other GPUs>"'``.

***************
 ``slot_type``
***************

The slot type that should be exposed. Dynamic agents having GPUs will be configured to ``cuda``,
agents without GPUs with ``cpu_slots_allowed: true`` provisioner option will be configured to
``cpu``, and ``none`` otherwise. For static agents this field defaults to ``auto``.

-  ``auto``: Automatically detects the slot type. The agent will detect if there are NVIDIA GPUs or
   AMD GPUs. If there are GPUs, it maps each GPU to one slot. Otherwise, it maps all the CPUs to a
   slot.

``none``: The agent will not create any slots for detected devices.

``cuda``: The agent will map each detected NVIDIA GPU to a slot. Prior to Determined 0.17.6, this
option was called ``gpu``.

``cpu``: Map all the CPUs to a slot, even when GPUs are present.

``rocm``: The agent will map each detected AMD ROCm GPU to a slot.

****************
 ``http_proxy``
****************

The HTTP proxy address for the agent's containers.

*****************
 ``https_proxy``
*****************

The HTTPS proxy address for the agent's containers.

***************
 ``ftp_proxy``
***************

The FTP proxy address for the agent's containers.

**************
 ``no_proxy``
**************

The addresses that the agent's containers should not proxy.

**************
 ``security``
**************

Security-related configuration settings.

``tls``
=======

Configuration settings for :ref:`TLS <tls>`.

-  ``enabled``: Whether to use TLS to connect to the master. Defaults to ``false``.
-  ``skip_verify``: Skip verifying the master certificate when using TLS. Defaults to ``false``.
   Enabling this setting will reduce the security of your Determined cluster.
-  ``master_cert``: CA cert file for the master when using TLS.
-  ``master_cert_name``: A hostname for which the master's TLS certificate is valid, if the value of
   the ``master_host`` option is an IP address or is not contained in the certificate.
-  ``client_cert``/``client_key``: Paths to files containing the client TLS certificate and key to
   use when connecting to the master.

******************************
 ``agent_reconnect_attempts``
******************************

Maximum number of times the agent will attempt to reconnect to master on connection failure.
Defaults to 30. With the default five-second backoff, attempts start immediately and continue for
approximately 145 seconds. Increasing this value keeps running containers available for recovery
through longer master outages, but delays the agent's connection-lost hook when recovery fails.

*****************************
 ``agent_reconnect_backoff``
*****************************

Time interval between reconnection attempts, in seconds. Defaults to 5 seconds.

********************************************
 ``container_auto_remove_disabled`` (debug)
********************************************

Whether to disable setting ``AutoRemove`` flag on task containers. Defaults to false.

***********
 ``hooks``
***********

Configuration for commands to run when certain events occur. The value of each option in this
section is an array of strings specifying the command and its arguments.

``on_connection_lost``
======================

A command to run when the agent fails to either connect to the master on startup or reconnect after
a loss of connection. When reconnecting, the agent will make several attempts as specified by the
``agent_reconnect_attempts`` and ``agent_reconnect_backoff`` configuration options.

In order to shut down the machine on which the agent is running, set this to ``["sudo", "shutdown",
"now"]``, or just ``["shutdown", "now"]`` if the agent is running as root. Additional system
configuration may be required in order to allow the agent to execute the command from inside a
Docker container or without the need to enter a password.

.. _agent-config-ref-debug:

***********
 ``debug``
***********

If ``true``, enables a more verbose form of logging that may be helpful in diagnosing issues.
Defaults to ``false``.

****************
 ``image_root``
****************

If set then specifies the path to a shared directory of previously downloaded Determined environment
images. If not defined, then Determined environments will be downloaded automatically. For more
information on setting up an image cache see :ref:`singularity-image-cache`. Defaults to undefined.

.. _agent-gpu-topology:

*************************
 GPU topology and health
*************************

When it starts, an agent measures its NVIDIA GPUs with NVML and reports the result to the master.
This is not an option: every agent with NVIDIA GPUs does it, and no configuration turns it on or
off. The master keeps the report in memory and serves it in the agent API (``gpu_topology``, left
out of agent lists requested with ``exclude_slots``), in ``det agent list``, in ``det agent describe
AGENT_ID``, and on the resource pool page of the WebUI. Users without permission to view sensitive
agent information see no topology.

NVML
====

The agent loads ``libnvidia-ml.so.1`` at runtime; the agent image does not bundle it. The NVIDIA
Container Toolkit mounts it into the agent container with the ``utility`` driver capability, which
containers started with ``--gpus`` get by default. An agent built without cgo, or for an operating
system other than Linux, has no NVML support and reports the topology as unknown.

The agent initializes NVML once per start, after device detection, and waits at most 60 seconds for
loading the library, initialization and the measurement together. After that it starts without the
measurement. On GPUs without persistence mode, initialization alone can add several seconds to agent
start. Device detection itself runs ``nvidia-smi`` without a timeout, so a hanging ``nvidia-smi``
still blocks agent start. The 60 seconds cover hangs only: a crash inside ``libnvidia-ml.so.1`` at
agent start stops the agent. To recover, run the previous agent image, after the rollback step of
:ref:`exclude_gpus <agent-exclude-gpus>` if the agent uses it.

NVML support links the agent binary dynamically against glibc. A custom agent image needs glibc
2.35, the version in ``ubuntu:22.04``, or newer; images based on musl, such as Alpine, cannot run
the agent.

What the agent measures
=======================

The agent measures once, at start, for every slot and every :ref:`excluded <agent-exclude-gpus>`
GPU, and for nothing else. A restart measures again, for example after a driver change.

-  For each GPU: its PCI bus ID, its NUMA node, the current and maximum PCIe link width and
   generation, and which of its NVML health calls failed. The NUMA node comes from sysfs
   (``/sys/bus/pci/devices/<bus ID>/numa_node``). When the kernel assigns the GPU no NUMA node
   (``-1``), it is node 0 on a host whose only online NUMA node is 0
   (``/sys/devices/system/node/online`` reads ``0``), and unknown otherwise.

-  For each pair of GPUs: the closest common ancestor (``INTERNAL``, ``PIX``, ``PXB``, ``PHB``,
   ``NODE`` or ``SYS``, as in ``nvidia-smi topo -m``), the number of active NVLinks between them,
   and the P2P ``READ`` and ``WRITE`` statuses in both directions.

-  The driver version and the time of the measurement, by the agent's clock.

A pair's P2P is usable only when ``READ`` and ``WRITE`` are ``OK`` in both directions, the condition
under which NCCL uses P2P between two GPUs. It is not usable when any of the four statuses is a
known status other than ``OK``, and unknown otherwise. NVLinks do not count without usable P2P.
Usable means that NVML reports ``OK``, not that a transfer was measured: a host whose BAR1 P2P setup
is broken can still report ``OK``.

NVML results appear as their symbolic name and number, for example ``ERROR_GPU_IS_LOST (15)``.

The topology is unknown, with the reason shown, when NVML cannot be loaded or initialized (for
example ``NVML init: ERROR_LIBRARY_NOT_FOUND (12)``), when NVML does not finish within 60 seconds,
for MIG instances, for an agent built without NVML support, for agents of earlier versions, and for
a few seconds after the master restarts, until each agent reconnects. The slots are still listed,
without measurements. So are excluded GPUs, except after a master restart: the master does not keep
the agent's report, so its excluded GPUs appear when the agent reconnects, and until then an agent
whose GPUs are all excluded shows no GPU topology.

Health
======

Each GPU gets one state, shown as a dot in the WebUI and as a word in the CLI. The first matching
row wins.

.. list-table::
   :header-rows: 1

   -  -  State
      -  Condition
      -  Dot
      -  CLI

   -  -  error
      -  One of the GPU's NVML health calls (handle, PCI info, link width or generation) failed at
         agent start. A failed query for a pair of GPUs makes only that pair's value unknown.
      -  red
      -  ``error``

   -  -  link below max
      -  At agent start, the current PCIe link width was below the maximum.
      -  amber
      -  ``narrow``

   -  -  ok
      -  The topology is known, and at agent start the link width was at its maximum.
      -  green
      -  ``ok``

   -  -  unknown
      -  Anything else, for example an unknown topology or an unknown link width.
      -  hollow gray
      -  ``unknown``

The link generation never changes the state. Green means that there was no NVML error and the link
width was at its maximum at agent start; it does not mean that the GPU is verified to be healthy.

The link width and generation are observations at agent start, not confirmed faults. A GPU can
reduce its link generation and width while it is idle, and a link can train to a different width
after a reboot. A lower link width lowers the bandwidth cap of the GPU's link; the actual collective
throughput depends on the workload.

The details of each GPU (``det agent describe`` and the WebUI's details) list four facts separately:
the link at agent start, the NVML errors at agent start, recent critical XIDs (``not collected``),
and the time of the measurement.

CLI and WebUI
=============

``det agent list`` shows two columns, also in ``--json`` as ``gpu_topology`` and ``gpu_health``.
Both count only the GPUs that are slots, and leave out excluded GPUs, except where noted.

-  GPU Topology: the number of slots per NUMA node (slots with an unknown NUMA node last), the link
   levels between slots from best to worst, and the P2P state of the pairs of slots, for example
   ``4+4 NODE/SYS p2p``. The P2P state is ``p2p`` when every pair is usable; ``no-p2p(<status>)``
   when no pair is usable and at least one is not, with the first status other than ``OK`` of the
   not-usable pair with the lowest slot IDs; ``p2p?`` when every pair is unknown; and otherwise the
   usable pairs over all pairs, for example ``p2p 12/28``. ``(<n> unknown)`` follows when some, but
   not all, pairs are unknown. An agent with one slot shows no P2P state, an agent whose GPUs are
   all excluded shows ``no slots``, and an unknown topology shows ``unknown: <reason>``.

-  GPU Health: ``ok`` when every GPU is ok and none is excluded. Otherwise the slots that are not
   ok, grouped as ``error``, ``narrow`` (with the widths at agent start) and ``unknown``, then the
   excluded GPUs by bus ID (by UUID when the bus ID is unknown), each with its state when it is not
   ok, for example ``narrow: 3,5 (x8 of x16 at start); excluded: 81:00.0``.

``det agent describe AGENT_ID`` lists each slot and excluded GPU with its state (``FREE``, the ID of
the container that uses it or ``OCCUPIED``, ``DISABLED``, ``DRAINING`` or ``EXCLUDED``), health,
UUID, bus ID, NUMA node, link width and generation, and the four facts of its health. When the
topology is known, it then prints the link levels between all GPUs and, when P2P is not usable for
every pair, the ``READ`` and ``WRITE`` statuses in each direction. ``--json`` prints the agent's
``gpu_topology``.

The WebUI's resource pool page groups each agent's GPUs by NUMA node and PCIe switch. A switch group
holds GPUs whose pairs are ``PIX``, one switch between them; ``PXB`` and the other levels show only
in the pairwise matrix. A tile's colour is the slot state, its dot is the GPU's health, stripes mark
disabled, draining and excluded GPUs, and the details of a GPU show on hover or focus and stay open
after a click. Each agent's slot count splits its slots into running, pending and unoccupied, and
the unoccupied slots into allocatable (enabled and not draining, so they can take new work),
disabled and draining. A slot of a disabled or draining agent counts and is striped as disabled or
draining whatever its own state, since the scheduler gives such an agent no new work. A running or
pending slot counts as running or pending also when it is disabled or draining, and a slot of the
topology without a matching slot record counts and shows as unknown.

Coverage
========

The measurement is a snapshot of the agent's start. A GPU that is lost, or a link that retrains,
while the agent runs shows only at its next start. A GPU that ``nvidia-smi`` no longer lists at
agent start is not measured: the agent registers fewer slots, or falls back to CPU slots with
``slot_type: auto``, and an agent that reconnects with fewer slots is stopped by the master. Use the
cluster's GPU monitoring for these cases.

``determined-agent gpu-topology``
=================================

The ``gpu-topology`` subcommand of the agent binary runs the agent's device detection and exclude
list, then the agent's NVML initialization and measurement, and prints the result as JSON, with the
raw P2P statuses and the derived P2P state of each pair. It accepts ``--visible-gpus``,
``--slot-type`` and ``--exclude-gpus``. It loads NVML also when it detects no GPU, and exits with 0
also when NVML is missing. ``nvml_init`` and ``nvml_init_code`` show the result of NVML's
initialization, for example ``ERROR_LIBRARY_NOT_FOUND`` and 12; ``NOT_BUILT`` and -1 for an agent
built without NVML support; and ``TIMEOUT`` and -2 when NVML does not finish within 60 seconds. An
``--exclude-gpus`` entry that matches no GPU appears as ``exclude_error`` instead of stopping the
command. It is a standalone probe: it starts from the defaults and its own flags, never from a
running agent's configuration file or environment, also when run inside the agent container with
``docker exec``, so pass the agent's ``--slot-type``, ``--visible-gpus`` and ``--exclude-gpus`` to
reproduce what the agent reports. For example, to check what an agent image would report on a host:

.. code:: bash

   docker run --rm --gpus all --entrypoint /usr/bin/determined-agent <agent image> gpu-topology
