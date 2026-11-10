.. _historical-cluster-usage-data:

###############################
 Historical Cluster Usage Data
###############################

Determined provides insights into the usage of your cluster, measured in compute hours allocated.
Note that this is based on allocation, not resource utilization. For example, if a user has 1 GPU
allocated but uses only 20% of it, we still report one slot-hour.

.. warning::

   The total used compute hours reported by Determined may be less than those reported by the cloud
   provider. This discrepancy occurs because we do not include idle time (e.g., waiting for a GPU to
   become active or when a GPU is not scheduled with any jobs).

.. warning::

   Data is aggregated by Determined metadata (e.g., label, user) nightly. Therefore, any data
   visualized on the WebUI or downloaded via the endpoint reflects the state as of the previous
   night. Changes to the metadata of a previously run experiment (e.g., labels) will be updated
   after the next nightly aggregation.

.. note::

   When using the export to CSV functionality, ``slot_hours`` reflects only the slot hours used
   during the export time window. This means that allocations overlapping the export window have
   their slot-hours calculated only for the time within the window. As a result, allocations not
   starting and ending within the export window may appear to have incorrect slot-hours when
   calculated manually from their start and end times.

*********************
 WebUI Visualization
*********************

WebUI visualizations provide a quick snapshot of the historical cluster usage:

.. image:: /assets/images/historical-cluster-usage-data.png
   :width: 100%
   :alt: WebUI showing historical cluster usage data

************************
 Command-line Interface
************************

Alternatively, you can use the :ref:`CLI <cli-ug>` or the API endpoints to download resource
allocation data for analysis:

-  ``det resources raw <start time> <end time>``: Get raw allocation information. Times are in the
   format yyyy-mm-ddThh:mm:ssZ.
-  ``det resources aggregated <start date> <end date>``: Get aggregated allocation information.
   Dates are in the format yyyy-mm-dd.

************
 User Pages
************

Every signed-in user can read the current resources and the usage history of any user:

-  ``GET /api/v1/users/{user_id}/resources``: the GPUs, CPU slots and auxiliary containers that the
   user's tasks hold at the time of the request, by resource pool, and whether auxiliary containers
   are metered on the cluster: they are unless every resource pool is a Kubernetes pool.

-  ``GET /api/v1/users/{user_id}/usage``: by day or by month, the GPU-, CPU-slot- and
   auxiliary-container-seconds that the user's tasks held, the slot-seconds of an unknown kind, and
   the seconds and the days with any GPU, with any auxiliary container, and with either held.

Both count what the tasks hold, not what they use:

-  Usage is counted in UTC days, or in UTC months as the sum of their days. Each allocation of a
   task counts from when the resource manager assigned its resources to when they were released,
   also when it ends before its containers start. A task that still runs counts until the time of
   the request. An allocation that a master failure left open counts until the master's last
   heartbeat.

-  What an allocation counts is fixed when its resources are assigned. GPU devices count as GPUs,
   and CPU devices as CPU slots; an allocation that holds both counts only its GPUs. An allocation
   without slots counts as one auxiliary container. On Kubernetes, slots count by the resource
   manager's slot type, and an allocation without slots is not counted.

-  Allocations recorded before the master metered assignments count from the end of their first
   queued period, or else from their start, to their end; one that ended without a recorded end
   counts nothing. Their slots take their pool's slot type the first time the master knows it, and
   keep it. A static pool's slot type is known once its connected agents have reported one type for
   ten minutes, with no fewer agents than the pool had earlier since the master started. Until then,
   and for pools that no longer exist, their slots count as slot-seconds of an unknown kind. Without
   slots they count as auxiliary containers, also in pools that no longer exist when the cluster has
   only agent resource pools, and are not counted on Kubernetes.

-  Checkpoint garbage collection is not counted.

-  A container of a task that is released before the task's other containers counts in the history
   until the whole allocation is released.

-  History starts with the cluster's first metered allocation, and allocation records that were
   deleted are not in it.

-  Pool names appear as in the job queue, whether or not ``hide_unusable_pools`` is set.

The usage history differs from the cluster usage charts above: it counts from when the resources
were assigned, where the charts count from when the containers started pulling their images; it does
not count checkpoint garbage collection, which the charts count; it counts auxiliary containers; and
it includes the current day, up to the time of the request.
