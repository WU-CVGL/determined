:orphan:

**New Features**

-  Experiments, tasks: Place named groups of GPUs of exact models on one agent with
   ``resources.gpu_groups``, for example one RTX 4090 for simulation and one RTX 6000 Ada for
   learning; the container finds each group's GPU UUIDs in ``DET_GPU_GROUPS``.
