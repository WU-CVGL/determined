:orphan:

**New Features**

-  TensorBoard: The experiment and trial pages and ``det tensorboard start`` open a CPU viewing
   session that reads the selected experiments' logs, checked against the viewer's permission on
   them.

**Improvements**

-  TensorBoard: A viewing session runs the TensorBoard of its image and installs nothing: it does
   not take an experiment's image or pull secrets, and ``DET_TENSORBOARD_PLUGIN_PROFILE`` has no
   effect. It opens before the logs arrive. Starting one needs the permission to launch shells in
   its workspace. ``det tensorboard start`` reads ``tensorboard_args`` as
   ``services.tensorboard.args``. ``det tensorboard list``, ``kill``, ``open`` and ``config`` act on
   TensorBoard tasks only; a viewing session is managed with ``det shell`` and ``det task``.

-  WebUI: TensorBoard in the **Launch Interactive Task** form reads Determined experiments and
   trials, or a log directory in the task.
