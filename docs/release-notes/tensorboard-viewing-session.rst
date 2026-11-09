:orphan:

**Breaking Changes**

-  TensorBoard: ``det tensorboard start`` starts a viewing session, a shell without SSH, instead of
   a TensorBoard task: nothing is installed in the image, the experiment's image and pull secrets
   are not inherited, and starting one needs the permission to launch shells in the workspace.

-  TensorBoard: ``det tensorboard list``, ``kill``, ``open`` and ``config`` act on TensorBoard tasks
   only. A viewing session is managed with ``det shell`` and ``det task``.

**New Features**

-  TensorBoard: The experiment and trial pages and ``det tensorboard start`` open a CPU viewing
   session that reads the selected experiments' logs, checked against the viewer's permission on
   them.

**Improvements**

-  WebUI: TensorBoard in the **Launch Interactive Task** form reads Determined experiments and
   trials, or a log directory in the task.
