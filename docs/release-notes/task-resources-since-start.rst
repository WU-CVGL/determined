:orphan:

**Improvements**

-  WebUI: A task's resource charts now open on a new default range, **Since start**, instead of
   **Last hour**. It begins when the task's first allocation got its resources, so time spent queued
   is no longer shown. With one allocation selected, it covers that allocation. Ranges longer than 7
   days show the most recent 7 days; the other ranges are unchanged.

-  WebUI: The GPU chart legends of trials, notebooks, and shells number GPUs by the order
   ``nvidia-smi`` listed them in the container at start, when the GPUs listed by the allocation's
   containers add up to its slots; other GPUs show the start of their UUID. The legends name the
   node or allocation only when a chart spans more than one. Hover over a legend entry for the GPU
   UUID, host, allocation, PCI bus ID, host GPU index, and model.
