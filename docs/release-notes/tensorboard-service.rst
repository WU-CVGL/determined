:orphan:

**New Features**

-  Shell: A shell runs TensorBoard from its image on a log directory of the container as the
   ``tensorboard`` service, with ``services.tensorboard.logdir``, a **TensorBoard** tab on the
   shell's page and ``det task open <shell id> tensorboard``.

**Bug Fixes**

-  Master: The proxy no longer compresses a task's already compressed response a second time, which
   left TensorBoard's toolbar icons blank.
