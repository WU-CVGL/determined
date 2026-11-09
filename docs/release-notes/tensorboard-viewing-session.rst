:orphan:

**Breaking Changes**

-  TensorBoard: ``det tensorboard start`` starts a viewing session, a shell without SSH, instead of
   a TensorBoard task: nothing is installed in the image, the experiment's image and pull secrets
   are not inherited, ``DET_TENSORBOARD_PLUGIN_PROFILE`` has no effect, TensorBoard opens before
   the logs arrive, and starting one needs the permission to launch shells in the workspace.
   ``tensorboard_args`` is read as ``services.tensorboard.args``.

**New Features**

-  TensorBoard: The experiment and trial pages and ``det tensorboard start`` open a CPU viewing
   session that reads the selected experiments' logs, checked against the viewer's permission on
   them.

**Improvements**

-  WebUI: TensorBoard in the **Launch Interactive Task** form reads Determined experiments and
   trials, or a log directory in the task.
