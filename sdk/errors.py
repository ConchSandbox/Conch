from connectrpc.code import Code
from connectrpc.errors import ConnectError


class SandboxError(RuntimeError):
    pass


class InvalidArgumentError(SandboxError):
    pass


class NotFoundError(SandboxError):
    pass


class ServiceUnavailableError(SandboxError):
    pass


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
    if exc.code == Code.UNAVAILABLE:
        return ServiceUnavailableError(exc.message)
    if exc.code == Code.DEADLINE_EXCEEDED:
        return TimeoutException(exc.message)
    return SandboxError(f"{exc.code}: {exc.message}")
