:orphan:

**Security Fixes**

-  API: Only a user who may edit an experiment, which is its owner or an administrator unless RBAC
   grants it to others, can delete its TensorBoard files, with ``det experiment delete-tb-files``
   or ``DELETE /api/v1/experiments/{id}/tensorboard-files``. Before this change, any signed-in user
   could delete the TensorBoard files of every experiment.
