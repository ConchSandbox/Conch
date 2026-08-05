from connectrpc.code import Code
from connectrpc.errors import ConnectError


class SandboxError(RuntimeError):
    pass


class InvalidArgumentError(SandboxError):
    pass


class NotFoundError(SandboxError):
    pass


class TemplateNotFoundError(NotFoundError):
    """The selected control-plane Template does not exist."""

    code = "not_found"
    resource_type = "template"

    def __init__(self, message: str = "template not found", request_id: str = ""):
        super().__init__(message)
        self.request_id = request_id


class AuthenticationError(SandboxError):
    pass


class TimeoutException(SandboxError):
    pass


def handle_rpc_error(exc: ConnectError) -> Exception:
    if exc.code == Code.INVALID_ARGUMENT:
        return InvalidArgumentError(exc.message)
    if exc.code == Code.UNAUTHENTICATED:
        return AuthenticationError(exc.message)
    if exc.code == Code.NOT_FOUND:
        return NotFoundError(exc.message)
    if exc.code == Code.DEADLINE_EXCEEDED:
        return TimeoutException(exc.message)
    return SandboxError(f"{exc.code}: {exc.message}")
