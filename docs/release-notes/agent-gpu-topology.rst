:orphan:

**New Features**

-  Agents: Each agent measures its NVIDIA GPUs with NVML when it starts: the PCIe topology between
   GPUs, the P2P ``READ`` and ``WRITE`` statuses in both directions, the NVLinks, each GPU's NUMA
   node, its PCIe link width and generation at agent start, and the NVML calls that failed. The
   agent API returns them as ``gpu_topology``, with a health state for each GPU: ok, link below max
   at agent start, error, or unknown. Masters of earlier versions ignore the report, and agents of
   earlier versions show as unknown. See :ref:`agent-gpu-topology`.

-  CLI: ``det agent list`` shows a GPU Topology column (NUMA groups, link levels and P2P state, for
   example ``4+4 NODE/SYS p2p``) and a GPU Health column, also in ``--json``. The new ``det agent
   describe AGENT_ID`` lists each GPU with its state, health, UUID, bus ID, NUMA node and link, and
   prints the link levels and, when P2P is not usable everywhere, the P2P statuses between all GPUs.

-  WebUI: The Topology section of the resource pool page shows each agent's GPUs grouped by NUMA
   node and PCIe switch. The tile colour is the slot state, a dot shows the GPU's health, stripes
   mark disabled, draining and excluded GPUs, and the details of each GPU show on hover or focus, or
   stay open on click.

-  Agents: The ``exclude_gpus`` option (``--exclude-gpus``) lists GPU UUIDs that the agent reports
   but never offers as slots, for example a faulty GPU, instead of hiding them from the agent
   container. An entry that matches no GPU stops the agent. Agents of earlier versions refuse to
   start with this option: before rolling an agent back, remove it and hide the GPU from the agent
   container again. See :ref:`agent-exclude-gpus`.

-  Agents: ``determined-agent gpu-topology`` prints what the agent would report on a host, as JSON,
   and exits with 0 also without NVML.

-  Images: The agent binary is now linked dynamically against glibc, which NVML support requires.
   The release build checks that it runs on the agent image's base, ``ubuntu:22.04``. A custom agent
   image needs a glibc at least as new.
