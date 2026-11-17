:orphan:

**New Features**

-  API: ``GET /api/v1/agents?includeGpuMemory=true`` returns each GPU's latest total, used, free and
   reserved memory with its sample time, read from the cluster's DCGM-Exporter. See :ref:`GPU memory
   <agent-gpu-memory>`.
