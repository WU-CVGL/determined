:orphan:

**New Features**

-  Shell: A shell runs TensorBoard from its image on a log directory of the container as the
   ``tensorboard`` service, with ``services.tensorboard.logdir``, a **TensorBoard** tab on the
   shell's page and ``det task open <shell id> tensorboard``. TensorBoard listens inside the
   container, and the master's proxy reaches it with a token of the task's, so the published port
   refuses other requests.

**Bug Fixes**

-  Master: The proxy no longer compresses a task's already compressed response a second time, which
   left TensorBoard's toolbar icons blank.

-  WebUI: A shell's service tabs show the services' end once the shell has ended, instead of the
   last page of the service.
