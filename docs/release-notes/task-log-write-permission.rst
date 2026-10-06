:orphan:

**Security Fixes**

-  API: Only the task's own containers, its owner, or an administrator can now add lines to the logs
   of a command, notebook, shell, TensorBoard, generic task, or checkpoint GC task, through ``POST
   /api/v1/task/logs`` or ``POST /task-logs``. With RBAC, a user who may kill the task
   (``UPDATE_NSC`` in its workspace) can too, except for checkpoint GC tasks. Before this change,
   any user who could see the task could. Reading task logs is unchanged.
