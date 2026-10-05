:orphan:

**Security Fixes**

-  TensorBoard: **Important:** A TensorBoard now takes the container image, the image pull secrets,
   and the ``registry_auth`` of the experiment it shows only when the user who starts it owns that
   experiment. A TensorBoard runs as the user who starts it, with that user's session token and
   agent user and group. Before this change, a TensorBoard on another user's experiment ran that
   experiment's image, so the experiment's owner could choose code that ran as anyone who opened a
   TensorBoard on the experiment, including an administrator, and that could act through the API
   as them.

-  TensorBoard: A TensorBoard on another user's experiment, administrators included, now uses the
   image from its own configuration or template, or the default task image
   (``task_container_defaults.image`` or Determined's default image), and takes no image pull
   secrets or ``registry_auth`` from the experiment. This applies to TensorBoards started from the
   WebUI and the CLI, on experiments or on trials. A TensorBoard that shows several experiments
   takes these settings from the newest of them, as before, and now only when its user owns that
   experiment. To use another user's image for a TensorBoard, name it in the TensorBoard's
   configuration, for example ``det tensorboard start <experiment-id> --config
   environment.image=<image>``, with your own ``registry_auth`` if the registry needs one.
