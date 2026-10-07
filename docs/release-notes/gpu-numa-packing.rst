:orphan:

**New Features**

-  Experiments, tasks: Give a task with 2 or more slots the best set of free GPUs of its agent by
   P2P, NVLinks, PCIe switches, and NUMA nodes with ``resources.prefer_gpu_topology: soft``. An
   earlier master rejects configs and templates that set it, also stored ones. See
   :ref:`prefer_gpu_topology <exp-config-resources-prefer-gpu-topology>`.

**Improvements**

-  Resource pools: Under ``fitting_policy: best``, pack each task's GPUs by NUMA node inside its
   agent and use GPUs in error last, instead of taking free GPUs in no particular order; turn it off
   per pool with ``scheduler.numa_packing: false``. See :ref:`numa_packing
   <master-config-numa-packing>`.
