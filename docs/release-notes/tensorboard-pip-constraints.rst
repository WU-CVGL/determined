:orphan:

**Bug Fixes**

-  TensorBoard: The TensorBoard task keeps the image's ``numpy``, ``protobuf``, ``requests`` and
   ``tensorboard`` when it installs packages. The newer ``tensorboard-plugin-profile`` releases
   pulled in ``numpy`` 2, which the image's TensorBoard cannot import, and the task exited with
   ``AttributeError: module 'numpy' has no attribute 'bool8'``.

**Breaking Changes**

-  TensorBoard: The TensorBoard task no longer installs ``tensorboard-plugin-profile`` unless the
   environment variable ``DET_TENSORBOARD_PLUGIN_PROFILE`` is set. The plugin's install pulled in
   xprof and the Google Cloud libraries and took minutes, longer than the default idle timeout.
