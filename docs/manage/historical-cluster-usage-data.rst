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
   user's tasks hold at the time of the request, by resource pool.
-  ``GET /api/v1/users/{user_id}/usage``: the hours of each that the user's tasks held, by day or by
   month.

Both count what the tasks hold, not what they use:

-  Usage is counted in UTC days, or in UTC months as the sum of their days. Each allocation of a
   task counts as one interval, from when its resources were assigned to when it was released. A
   task that still runs counts until the time of the request.

-  Slots count as CPU slots in a pool whose slot type is CPU and as GPUs otherwise. History uses the
   pool's current slot type, so a pool without connected agents, or a pool that no longer exists,
   counts as GPUs.

-  An auxiliary container is a container without slots. Kubernetes pools do not report them, so
   neither endpoint counts them there.

-  Checkpoint garbage collection is not counted.

-  A container of a task that is released before the task's other containers counts in the history
   until the whole allocation is released.

-  History starts with the cluster's first allocation record, and allocation records that were
   deleted are not in it.

-  Pool names appear as in the job queue, whether or not ``hide_unusable_pools`` is set.

The usage history differs from the cluster usage charts above: it counts from when the resources
were assigned, where the charts count from when the containers started pulling their images; it does
not count checkpoint garbage collection, which the charts count; it counts auxiliary containers; and
it includes the current day, up to the time of the request.
