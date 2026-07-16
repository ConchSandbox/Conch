import pytest
from connectrpc.code import Code
from connectrpc.errors import ConnectError

from conch import CommandExitException, CommandHandle, InvalidArgumentError, NotFoundError, Sandbox, SandboxError
from conch.client import AgentClient


def test_execute_python_content(sandbox):
    """Execute Python script via content parameter."""
    result = sandbox.commands.run(cmd="python3", content='print("hello")')
    assert result.exit_code == 0
    assert "hello" in result.stdout


def test_execute_args(sandbox):
    """Execute command with args."""
    result = sandbox.commands.run(cmd="ls", args=["-l", "/root"])
    assert result.exit_code == 0


def test_execute_cwd(sandbox):
    """Execute command with specified working directory."""
    result = sandbox.commands.run(cmd="sh", args=["-c", "pwd"], cwd="/tmp")
    assert result.exit_code == 0
    assert "/tmp" in result.stdout.strip()


def test_execute_env_with_shell(sandbox):
    """Execute command with environment variables via sh -c."""
    result = sandbox.commands.run(cmd="sh", args=["-c", "echo $MY_VAR"],
                                  env={"MY_VAR": "conch_test"})
    assert result.exit_code == 0
    assert "conch_test" in result.stdout


def test_execute_exit_code(sandbox):
    """Non-zero exit code raises CommandExitException."""
    with pytest.raises(CommandExitException) as exc:
        sandbox.commands.run(cmd="ls", args=["/nonexistent_path"])
    assert exc.value.exit_code != 0
    assert exc.value.stderr != ""


def test_execute_no_shell_expansion(sandbox):
    """Verify $VAR is NOT expanded without shell (FAQ case)."""
    result = sandbox.commands.run(cmd="echo", args=["$HOME"])
    assert result.exit_code == 0
    assert "$HOME" in result.stdout


def test_background_wait_uses_start_stream():
    """Background wait should consume the start stream instead of reconnecting."""
    class FakeClient:
        STATUS_SUCCESS = 0

        def start_process(self, **kwargs):
            return {
                "status": self.STATUS_SUCCESS,
                "message": "OK",
                "stdout": "",
                "stderr": "",
                "exit_code": -1,
                "error": "",
                "process": {
                    "pid": 123,
                    "tag": kwargs.get("tag") or "",
                    "running": True,
                    "config": {"cmd": kwargs["cmd"], "args": kwargs.get("args") or []},
                },
                "events": iter([
                    {"data": {"stdout": "fast-ok\n"}},
                    {"end": {"exitCode": 0, "error": "", "exited": True, "status": "exited"}},
                ]),
            }

        def connect_process(self, **kwargs):
            raise AssertionError("wait() should not reconnect")

    sandbox = Sandbox(api_url="http://unused", image_name="test")
    sandbox.client = FakeClient()

    handle = sandbox.commands.run(cmd="sh", args=["-c", "echo fast-ok"], background=True)
    result = handle.wait()
    assert result.exit_code == 0
    assert result.stdout == "fast-ok\n"


def test_background_pty_wait_preserves_output_in_stdout():
    """Background PTY output should match foreground PTY aggregation behavior."""
    class FakeClient:
        STATUS_SUCCESS = 0

        def start_process(self, **kwargs):
            return {
                "status": self.STATUS_SUCCESS,
                "message": "OK",
                "stdout": "",
                "stderr": "",
                "exit_code": -1,
                "error": "",
                "process": {
                    "pid": 123,
                    "tag": kwargs.get("tag") or "",
                    "running": True,
                    "config": {"cmd": kwargs["cmd"], "args": kwargs.get("args") or [], "pty": kwargs.get("pty")},
                },
                "events": iter([
                    {"data": {"pty": "pty-bg-ok"}},
                    {"end": {"exitCode": 0, "error": "", "exited": True, "status": "exited"}},
                ]),
            }

    sandbox = Sandbox(api_url="http://unused", image_name="test")
    sandbox.client = FakeClient()

    handle = sandbox.commands.run(cmd="sh", args=["-c", "echo pty"], background=True, pty={"cols": 80, "rows": 24})
    result = handle.wait()
    assert result.exit_code == 0
    assert result.stdout == "pty-bg-ok"


def test_command_handle_without_event_stream_does_not_reconnect():
    class FakeClient:
        def connect_process(self, **kwargs):
            raise AssertionError("CommandHandle should not reconnect without an event stream")

    sandbox = Sandbox(api_url="http://unused", image_name="test")
    sandbox.client = FakeClient()
    handle = CommandHandle(sandbox, {"pid": 123, "tag": "missing-events"})

    with pytest.raises(RuntimeError, match="no event stream"):
        handle.wait()


def test_connect_missing_process_raises_not_found_error():
    class FakeProcessClient:
        def connect(self, request, headers=None):
            def events():
                raise ConnectError(Code.NOT_FOUND, f"process tag {request.process.tag!r} not found")
                yield
            return events()

    sandbox = Sandbox(api_url="http://unused", image_name="test")
    sandbox.client = AgentClient("127.0.0.1")
    sandbox.client.process_client = FakeProcessClient()

    with pytest.raises(NotFoundError, match="process tag 'missing' not found") as exc:
        sandbox.commands.connect(tag="missing")
    assert exc.value.__suppress_context__ is True


def test_kill_missing_selector_raises_invalid_argument_error():
    client = AgentClient("127.0.0.1")

    with pytest.raises(InvalidArgumentError, match="process pid or tag is required"):
        client.send_signal()


def test_process_rpc_errors_are_mapped_to_sdk_errors():
    class FakeProcessClient:
        def list(self, request, headers=None):
            raise ConnectError(Code.UNAUTHENTICATED, "agent token is invalid")

    client = AgentClient("127.0.0.1")
    client.process_client = FakeProcessClient()

    with pytest.raises(SandboxError, match="agent token is invalid") as exc:
        client.list_processes()
    assert exc.value.__suppress_context__ is True


def test_file_stream_rpc_errors_are_mapped_to_sdk_errors():
    class FakeFileClient:
        def get_file_stream(self, request, headers=None):
            def events():
                raise ConnectError(Code.NOT_FOUND, f"file not found: {request.filepath}")
                yield
            return events()

    client = AgentClient("127.0.0.1")
    client.file_client = FakeFileClient()

    with pytest.raises(NotFoundError, match="file not found: /tmp/missing") as exc:
        client.read_file("/tmp/missing")
    assert exc.value.__suppress_context__ is True
