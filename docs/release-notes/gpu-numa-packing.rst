:orphan:

**New Features**

-  Experiments, tasks: Give a task with 2 or more slots the best set of free GPUs of its agent by
   P2P, NVLinks, PCIe switches, NUMA nodes, and PCIe link width with
   ``resources.prefer_gpu_topology: soft``. After a rollback, an earlier master moves running and
   paused experiments whose config sets it, even to ``false``, to ERROR when it starts. See
   :ref:`prefer_gpu_topology <exp-config-resources-prefer-gpu-topology>`.

**Improvements**

-  Resource pools: Under ``fitting_policy: best``, pack each task's GPUs by NUMA node inside its
   agent, preferring fewer GPUs in error, instead of taking free GPUs in no particular order; turn
   it off per pool with ``scheduler.numa_packing: false``. An earlier master does not start while
   ``master.yaml`` or a pool spec sets the option. See :ref:`numa_packing
   <master-config-numa-packing>`.
