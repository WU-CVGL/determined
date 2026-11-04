:orphan:

**Bug Fixes**

-  TensorBoard: The TensorBoard task keeps the image's ``numpy``, ``protobuf``, ``requests`` and
   ``tensorboard`` when it installs ``tensorboard-plugin-profile``. The plugin's newer releases
   pulled in ``numpy`` 2, which the image's TensorBoard cannot import, and the task exited with
   ``AttributeError: module 'numpy' has no attribute 'bool8'``. A plugin that cannot be installed
   under those versions is left out and TensorBoard runs without it.
