:orphan:

**Security Fixes**

-  API: Only the task's own containers, its owner, or an administrator (with RBAC, a user with
   ``UPDATE_NSC`` in the task's workspace instead; checkpoint GC tasks stay owner or administrator)
   can now add lines to the logs of a command, notebook, shell, TensorBoard, generic task, or
   checkpoint GC task, through ``POST /api/v1/task/logs`` or ``POST /task-logs``. Before this
   change, any user who could see the task could. Reading task logs is unchanged.
