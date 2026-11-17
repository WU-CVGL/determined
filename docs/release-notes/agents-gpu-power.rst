:orphan:

**New Features**

-  API: ``GET /api/v1/agents?includeGpuPower=true`` returns each GPU's power draw and its
   configured, default and enforced power limits with their sample times, read from the cluster's
   DCGM-Exporter. See :ref:`GPU memory and power <agent-gpu-memory>`.
