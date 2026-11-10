from typing import Optional

from determined.common.api import bindings
from determined.common.api import errors as api_errors

# The master's refusals of a job's logs or config that are not shared with the user.
NOT_SHARED_MESSAGES = ("logs not shared", "config not shared")


class FeatureFlagDisabled(Exception):
    """
    Exception indicating that there is a currently disabled feature flag
    that is required to use a feature
    """

    pass


class CliError(Exception):
    """
    Base class for all CLI errors.
    """

    name: str

    def __init__(self, message: str, exit_code: int = 1) -> None:
        """
        Args:
        - e_stack: The exception that triggered this error.
        - exit_code: The exit code to use when exiting the CLI.
        """
        super().__init__(message)
        self.name = "Error"
        self.exit_code = exit_code
        self.message = message


def _stream_refusal(message: str) -> Optional[str]:
    """The refusal text in a stream error: the whole message, as a stream refused when it opens
    sends it, or the end of one of its lines, as a stream refused while it runs wraps it in a line
    "* failed to fetch batch: rpc error: code = PermissionDenied desc = logs not shared".
    """
    if message in NOT_SHARED_MESSAGES:
        return message
    for line in message.splitlines():
        for refusal in NOT_SHARED_MESSAGES:
            if line.rstrip().endswith(f"code = PermissionDenied desc = {refusal}"):
                return refusal
    return None


def not_shared_message(e: BaseException) -> Optional[str]:
    """The master's text when it refused a job's logs or config as not shared, else None."""
    if isinstance(e, api_errors.ForbiddenException):
        # ForbiddenException.message is "Forbidden(<the master's text>): ...".
        for message in NOT_SHARED_MESSAGES:
            if e.message.startswith(f"Forbidden({message})"):
                return message
    elif isinstance(e, bindings.APIHttpStreamError):
        return _stream_refusal(e.error.message or "")
    return None
