import pytest
from connectrpc.code import Code
from connectrpc.errors import ConnectError

from conch import (
    CommandExitException,
    CommandHandle,
    InvalidArgumentError,
    NotFoundError,
    Sandbox,
    SandboxError,
    TimeoutException,
)
from conch.client import AgentClient
from api.py_proto import agent_pb2


def test_process_execute_python_content(sandbox):
    """Execute Python script via content parameter."""
    result = sandbox.commands.run(cmd="python3", content='print("hello")')
    assert result.exit_code == 0
    assert "hello" in result.stdout


def test_process_execute_args(sandbox):
    """Execute command with args."""
    result = sandbox.commands.run(cmd="ls", args=["-l", "/root"])
    assert result.exit_code == 0


def test_process_execute_cwd(sandbox):
    """Execute command with specified working directory."""
    result = sandbox.commands.run(cmd="sh", args=["-c", "pwd"], cwd="/tmp")
    assert result.exit_code == 0
    assert "/tmp" in result.stdout.strip()


def test_process_execute_env_with_shell(sandbox):
    """Execute command with environment variables via sh -c."""
    result = sandbox.commands.run(cmd="sh", args=["-c", "echo $MY_VAR"],
                                  env={"MY_VAR": "conch_test"})
    assert result.exit_code == 0
    assert "conch_test" in result.stdout


def test_process_execute_exit_code(sandbox):
    """Non-zero exit code raises CommandExitException."""
    with pytest.raises(CommandExitException) as exc:
        sandbox.commands.run(cmd="ls", args=["/nonexistent_path"])
    assert exc.value.exit_code != 0
    assert exc.value.stderr != ""


def test_process_execute_no_shell_expansion(sandbox):
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

    sandbox = Sandbox(api_url="http://unused", template_id="test")
    sandbox.client = FakeClient()

    handle = sandbox.commands.run(cmd="sh", args=["-c", "echo fast-ok"], background=True)
    seen = []
    result = handle.wait(on_stdout=lambda text: seen.append(text))
    assert result.exit_code == 0
    assert result.stdout == "fast-ok\n"
    assert seen == ["fast-ok\n"]


def test_foreground_run_uses_stream_callbacks():
    """Foreground run should surface output chunks through E2B-style callbacks."""
    order = []

    class FakeClient:
        def stream_process(self, **kwargs):
            order.append(("stream", kwargs["cmd"]))
            yield {"start": {"pid": 123}}
            yield {"data": {"stdout": "fast-ok\n"}}
            order.append("after-stdout")
            yield {"data": {"stderr": "warn\n"}}
            order.append("after-stderr")
            yield {"end": {"exitCode": 0, "error": "", "exited": True, "status": "exited"}}

        def start_process(self, **kwargs):
            raise AssertionError("foreground callbacks should use the streaming path")

    sandbox = Sandbox(api_url="http://unused", template_id="test")
    sandbox.client = FakeClient()

    seen = []
    result = sandbox.commands.run(
        cmd="sh",
        args=["-c", "echo fast-ok"],
        on_stdout=lambda text: seen.append(("stdout", text)),
        on_stderr=lambda text: seen.append(("stderr", text)),
    )

    assert result.exit_code == 0
    assert result.stdout == "fast-ok\n"
    assert result.stderr == "warn\n"
    assert seen == [("stdout", "fast-ok\n"), ("stderr", "warn\n")]
    assert order == [("stream", "sh"), "after-stdout", "after-stderr"]


def test_foreground_run_with_pty_keeps_output_in_stdout():
    """PTY output in the foreground should still be folded into the result."""
    class FakeClient:
        def stream_process(self, **kwargs):
            yield {"start": {"pid": 123}}
            yield {"data": {"pty": "pty-ok"}}
            yield {"end": {"exitCode": 0, "error": "", "exited": True, "status": "exited"}}

        def start_process(self, **kwargs):
            raise AssertionError("foreground callbacks should use the streaming path")

    sandbox = Sandbox(api_url="http://unused", template_id="test")
    sandbox.client = FakeClient()

    seen = []
    result = sandbox.commands.run(
        cmd="sh",
        args=["-c", "echo pty"],
        pty={"cols": 80, "rows": 24},
        on_stdout=lambda text: seen.append(text),
    )

    assert result.exit_code == 0
    assert result.stdout == "pty-ok"
    assert seen == []


def test_agent_client_stream_process_yields_event_dicts():
    class FakeProcessClient:
        def start_process(self, request, headers=None):
            assert request.cmd == "sh"
            assert list(request.args) == ["-c", "echo ok"]
            return iter([
                agent_pb2.ProcessEvent(start=agent_pb2.ProcessStartEvent(pid=123)),
                agent_pb2.ProcessEvent(data=agent_pb2.ProcessDataEvent(stdout=b"ok\n")),
                agent_pb2.ProcessEvent(end=agent_pb2.ProcessEndEvent(exit_code=0, exited=True, status="exited")),
            ])

    client = AgentClient("127.0.0.1")
    client.process_client = FakeProcessClient()

    events = list(client.stream_process(cmd="sh", args=["-c", "echo ok"]))

    assert events == [
        {"start": {"pid": 123}},
        {"data": {"stdout": "ok\n"}},
        {"end": {"exitCode": 0, "exited": True, "status": "exited", "error": ""}},
    ]


def test_agent_client_stream_process_decodes_split_utf8_sequence():
    encoded = "中".encode("utf-8")

    class FakeProcessClient:
        def start_process(self, request, headers=None):
            return iter([
                agent_pb2.ProcessEvent(start=agent_pb2.ProcessStartEvent(pid=123)),
                agent_pb2.ProcessEvent(data=agent_pb2.ProcessDataEvent(stdout=encoded[:1])),
                agent_pb2.ProcessEvent(data=agent_pb2.ProcessDataEvent(stdout=encoded[1:])),
                agent_pb2.ProcessEvent(end=agent_pb2.ProcessEndEvent(exit_code=0, exited=True, status="exited")),
            ])

    client = AgentClient("127.0.0.1")
    client.process_client = FakeProcessClient()

    events = list(client.stream_process(cmd="sh"))

    assert events == [
        {"start": {"pid": 123}},
        {"data": {"stdout": "中"}},
        {"end": {"exitCode": 0, "exited": True, "status": "exited", "error": ""}},
    ]


def test_foreground_run_callbacks_propagate_start_rpc_errors():
    class FakeProcessClient:
        def start_process(self, request, headers=None):
            raise ConnectError(Code.INVALID_ARGUMENT, "failed to execute process: command not found")

    sandbox = Sandbox(api_url="http://unused", template_id="test")
    sandbox.client = AgentClient("127.0.0.1")
    sandbox.client.process_client = FakeProcessClient()

    with pytest.raises(InvalidArgumentError, match="command not found"):
        sandbox.commands.run(cmd="missing-command", on_stdout=lambda text: None)


def test_foreground_run_raises_timeout_exception():
    class FakeProcessClient:
        def start_process(self, request, headers=None):
            def events():
                yield agent_pb2.ProcessEvent(start=agent_pb2.ProcessStartEvent(pid=123))
                raise ConnectError(Code.DEADLINE_EXCEEDED, "process execution timed out")

            return events()

    sandbox = Sandbox(api_url="http://unused", template_id="test")
    sandbox.client = AgentClient("127.0.0.1")
    sandbox.client.process_client = FakeProcessClient()

    with pytest.raises(TimeoutException, match="process execution timed out"):
        sandbox.commands.run(cmd="sleep", args=["10"])


def test_foreground_callback_run_raises_timeout_exception():
    class FakeProcessClient:
        def start_process(self, request, headers=None):
            def events():
                yield agent_pb2.ProcessEvent(start=agent_pb2.ProcessStartEvent(pid=123))
                raise ConnectError(Code.DEADLINE_EXCEEDED, "process execution timed out")

            return events()

    sandbox = Sandbox(api_url="http://unused", template_id="test")
    sandbox.client = AgentClient("127.0.0.1")
    sandbox.client.process_client = FakeProcessClient()

    with pytest.raises(TimeoutException, match="process execution timed out"):
        sandbox.commands.run(cmd="sleep", args=["10"], on_stdout=lambda text: None)


def test_agent_client_start_process_builds_request_and_aggregates_output():
    class FakeProcessClient:
        def start_process(self, request, headers=None):
            assert request.cmd == "sh"
            assert list(request.args) == ["-c", "echo ok"]
            assert request.cwd == "/tmp"
            assert dict(request.env) == {"A": "B"}
            assert request.pty.cols == 100
            assert request.pty.rows == 40
            assert request.background is False
            return iter([
                agent_pb2.ProcessEvent(start=agent_pb2.ProcessStartEvent(pid=123)),
                agent_pb2.ProcessEvent(data=agent_pb2.ProcessDataEvent(stdout=b"ok\n")),
                agent_pb2.ProcessEvent(data=agent_pb2.ProcessDataEvent(pty=b"pty\n")),
                agent_pb2.ProcessEvent(data=agent_pb2.ProcessDataEvent(stderr=b"warn\n")),
                agent_pb2.ProcessEvent(end=agent_pb2.ProcessEndEvent(exit_code=0, exited=True, status="exited")),
            ])

    client = AgentClient("127.0.0.1")
    client.process_client = FakeProcessClient()

    result = client.start_process(
        cmd="sh",
        args=["-c", "echo ok"],
        cwd="/tmp",
        env={"A": "B"},
        pty={"cols": 100, "rows": 40},
    )

    assert result["status"] == AgentClient.STATUS_SUCCESS
    assert result["stdout"] == "ok\npty\n"
    assert result["stderr"] == "warn\n"
    assert result["exit_code"] == 0


def test_agent_client_start_process_sends_stdin():
    class FakeProcessClient:
        def start_process(self, request, headers=None):
            assert request.stdin == b"input\\n"
            return iter([
                agent_pb2.ProcessEvent(start=agent_pb2.ProcessStartEvent(pid=123)),
                agent_pb2.ProcessEvent(end=agent_pb2.ProcessEndEvent(exit_code=0, exited=True, status="exited")),
            ])

    client = AgentClient("127.0.0.1")
    client.process_client = FakeProcessClient()

    result = client.start_process(cmd="cat", stdin="input\\n")

    assert result["exit_code"] == 0


def test_agent_client_start_process_sets_connect_timeout():
    class FakeProcessClient:
        def start_process(self, request, headers=None, timeout_ms=None):
            assert timeout_ms == 1500
            return iter([
                agent_pb2.ProcessEvent(start=agent_pb2.ProcessStartEvent(pid=123)),
                agent_pb2.ProcessEvent(end=agent_pb2.ProcessEndEvent(exit_code=0, exited=True, status="exited")),
            ])

    client = AgentClient("127.0.0.1")
    client.process_client = FakeProcessClient()

    result = client.start_process(cmd="sleep", args=["1"], timeout_ms=1500)

    assert result["exit_code"] == 0


def test_command_manager_forwards_stdin():
    class FakeAgentClient:
        def start_process(self, **kwargs):
            assert kwargs["stdin"] == "input\n"
            assert kwargs["timeout_ms"] == 1500
            return {
                "status": AgentClient.STATUS_SUCCESS,
                "stdout": "input\n",
                "stderr": "",
                "exit_code": 0,
                "error": "",
            }

    sandbox = Sandbox(api_url="http://unused", template_id="test")
    sandbox.client = FakeAgentClient()

    result = sandbox.commands.run(cmd="cat", stdin="input\n", timeout=1.5)

    assert result.stdout == "input\n"


def test_command_manager_forwards_stdin_to_background_process():
    class FakeAgentClient:
        def start_process(self, **kwargs):
            assert kwargs["background"] is True
            assert kwargs["stdin"] == "input\n"
            assert kwargs["timeout_ms"] == 1500
            return {
                "process": {"pid": 123, "tag": "stdin-bg", "running": True},
                "events": iter(()),
            }

    sandbox = Sandbox(api_url="http://unused", template_id="test")
    sandbox.client = FakeAgentClient()

    handle = sandbox.commands.run(cmd="cat", background=True, stdin="input\n", timeout=1.5)

    assert handle.pid == 123


def test_command_manager_forwards_stdin_to_streamed_process():
    class FakeAgentClient:
        def stream_process(self, **kwargs):
            assert kwargs["stdin"] == "input\n"
            assert kwargs["timeout_ms"] == 1500
            return iter([
                {"start": {"pid": 123}},
                {"data": {"stdout": "input\n"}},
                {"end": {"exitCode": 0, "exited": True, "status": "exited", "error": ""}},
            ])

    sandbox = Sandbox(api_url="http://unused", template_id="test")
    sandbox.client = FakeAgentClient()
    output = []

    result = sandbox.commands.run(cmd="cat", stdin="input\n", timeout=1.5, on_stdout=output.append)

    assert result.stdout == "input\n"
    assert output == ["input\n"]


def test_agent_client_start_process_rejects_content_with_args():
    client = AgentClient("127.0.0.1")

    with pytest.raises(InvalidArgumentError, match="content cannot be used with args"):
        client.start_process(cmd="python3", content="print('x')", args=["script.py"])


def test_agent_client_background_process_response_uses_start_stream():
    class FakeProcessClient:
        def start_process(self, request, headers=None):
            assert request.background is True
            assert request.tag == "svc"
            return iter([
                agent_pb2.ProcessEvent(start=agent_pb2.ProcessStartEvent(pid=321)),
                agent_pb2.ProcessEvent(data=agent_pb2.ProcessDataEvent(stdout=b"boot\n")),
                agent_pb2.ProcessEvent(end=agent_pb2.ProcessEndEvent(exit_code=0, exited=True, status="exited")),
            ])

    client = AgentClient("127.0.0.1")
    client.process_client = FakeProcessClient()

    response = client.start_process(cmd="sleep", args=["10"], background=True, tag="svc")

    assert response["status"] == AgentClient.STATUS_SUCCESS
    assert response["process"]["pid"] == 321
    assert response["process"]["tag"] == "svc"
    assert response["process"]["config"]["cmd"] == "sleep"
    assert next(response["events"]) == {"data": {"stdout": "boot\n"}}


def test_agent_client_background_process_missing_start_event_fails():
    class FakeProcessClient:
        def start_process(self, request, headers=None):
            return iter([])

    client = AgentClient("127.0.0.1")
    client.process_client = FakeProcessClient()

    response = client.start_process(cmd="sleep", background=True)

    assert response["status"] == AgentClient.STATUS_FAILED
    assert response["process"] is None
    assert "missing start event" in response["error"]


def test_agent_client_background_process_without_start_preserves_remaining_events():
    class FakeProcessClient:
        def start_process(self, request, headers=None):
            return iter([
                agent_pb2.ProcessEvent(data=agent_pb2.ProcessDataEvent(stdout=b"partial\n")),
                agent_pb2.ProcessEvent(end=agent_pb2.ProcessEndEvent(exit_code=7, error="broken", exited=True, status="errored")),
            ])

    client = AgentClient("127.0.0.1")
    client.process_client = FakeProcessClient()

    response = client.start_process(cmd="sleep", background=True)

    assert response["status"] == AgentClient.STATUS_FAILED
    assert response["process"] is None
    assert response["stdout"] == "partial\n"
    assert response["exit_code"] == 7
    assert response["error"] == "broken"


def test_agent_client_connect_process_builds_selector_and_yields_events():
    class FakeProcessClient:
        def connect(self, request, headers=None):
            assert request.process.pid == 77
            assert request.process.tag == ""
            return iter([
                agent_pb2.ProcessEvent(start=agent_pb2.ProcessStartEvent(pid=77)),
                agent_pb2.ProcessEvent(data=agent_pb2.ProcessDataEvent(stdout=b"attached\n")),
            ])

    client = AgentClient("127.0.0.1")
    client.process_client = FakeProcessClient()

    assert list(client.connect_process(pid=77)) == [
        {"start": {"pid": 77}},
        {"data": {"stdout": "attached\n"}},
    ]


def test_agent_client_list_processes_maps_process_info():
    class FakeProcessClient:
        def list(self, request, headers=None):
            return agent_pb2.ListProcessesResponse(processes=[
                agent_pb2.ProcessInfo(
                    pid=7,
                    tag="svc",
                    running=True,
                    started_at="2026-07-24T00:00:00Z",
                    exit_code=-1,
                    stdout="out",
                    stderr="err",
                    config=agent_pb2.ProcessConfig(
                        cmd="python3",
                        args=["-m", "http.server"],
                        env={"PORT": "8000"},
                        cwd="/srv",
                        pty=agent_pb2.PTY(cols=120, rows=30),
                    ),
                ),
            ])

    client = AgentClient("127.0.0.1")
    client.process_client = FakeProcessClient()

    processes = client.list_processes()

    assert processes == [{
        "pid": 7,
        "tag": "svc",
        "running": True,
        "startedAt": "2026-07-24T00:00:00Z",
        "exitCode": -1,
        "finishedAt": "",
        "stdout": "out",
        "stderr": "err",
        "config": {
            "cmd": "python3",
            "args": ["-m", "http.server"],
            "env": {"PORT": "8000"},
            "cwd": "/srv",
            "pty": {"cols": 120, "rows": 30},
        },
    }]


def test_agent_client_send_signal_builds_tag_selector():
    class FakeProcessClient:
        def __init__(self):
            self.request = None

        def send_signal(self, request, headers=None):
            self.request = request
            return agent_pb2.SendSignalResponse()

    process_client = FakeProcessClient()
    client = AgentClient("127.0.0.1")
    client.process_client = process_client

    assert client.send_signal(tag="svc", signal=9) is True
    assert process_client.request.process.tag == "svc"
    assert process_client.request.signal == 9


def test_agent_client_send_signal_returns_false_for_missing_process():
    class FakeProcessClient:
        def send_signal(self, request, headers=None):
            raise ConnectError(Code.NOT_FOUND, "process not found")

    client = AgentClient("127.0.0.1")
    client.process_client = FakeProcessClient()

    assert client.send_signal(pid=404) is False


def test_background_run_rejects_callbacks():
    sandbox = Sandbox(api_url="http://unused", template_id="test")

    with pytest.raises(InvalidArgumentError, match="callbacks are only supported"):
        sandbox.commands.run(cmd="sleep", args=["10"], background=True, on_stdout=lambda text: None)


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

    sandbox = Sandbox(api_url="http://unused", template_id="test")
    sandbox.client = FakeClient()

    handle = sandbox.commands.run(cmd="sh", args=["-c", "echo pty"], background=True, pty={"cols": 80, "rows": 24})
    seen = []
    result = handle.wait(on_pty=lambda text: seen.append(text))
    assert result.exit_code == 0
    assert result.stdout == "pty-bg-ok"
    assert seen == ["pty-bg-ok"]


def test_connect_wait_uses_stream_callbacks():
    class FakeClient:
        def connect_process(self, **kwargs):
            assert kwargs["tag"] == "http-srv"
            return iter([
                {"start": {"pid": 123}},
                {"data": {"stdout": "alive\n"}},
                {"data": {"pty": "pty-alive"}},
                {"end": {"exitCode": 0, "error": "", "exited": True, "status": "exited"}},
            ])

    sandbox = Sandbox(api_url="http://unused", template_id="test")
    sandbox.client = FakeClient()

    handle = sandbox.commands.connect(tag="http-srv")
    seen = []
    result = handle.wait(
        on_stdout=lambda text: seen.append(("stdout", text)),
        on_pty=lambda text: seen.append(("pty", text)),
    )

    assert result.exit_code == 0
    assert result.stdout == "alive\npty-alive"
    assert seen == [("stdout", "alive\n"), ("pty", "pty-alive")]


def test_command_handle_without_event_stream_does_not_reconnect():
    class FakeClient:
        def connect_process(self, **kwargs):
            raise AssertionError("CommandHandle should not reconnect without an event stream")

    sandbox = Sandbox(api_url="http://unused", template_id="test")
    sandbox.client = FakeClient()
    handle = CommandHandle(sandbox, {"pid": 123, "tag": "missing-events"})

    with pytest.raises(RuntimeError, match="no event stream"):
        handle.wait()


def test_command_handle_iteration_propagates_stream_errors():
    def failing_events():
        yield {"data": {"stdout": "partial"}}
        raise SandboxError("stream failed")

    sandbox = Sandbox(api_url="http://unused", template_id="test")
    handle = CommandHandle(sandbox, {"pid": 123, "tag": "broken"}, events=failing_events())

    iterator = iter(handle)
    assert next(iterator) == ("partial", None, None)
    with pytest.raises(SandboxError, match="stream failed"):
        next(iterator)


def test_connect_missing_process_raises_not_found_error():
    class FakeProcessClient:
        def connect(self, request, headers=None):
            def events():
                raise ConnectError(Code.NOT_FOUND, f"process tag {request.process.tag!r} not found")
                yield
            return events()

    sandbox = Sandbox(api_url="http://unused", template_id="test")
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
