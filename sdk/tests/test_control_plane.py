from unittest.mock import Mock

import pytest
import requests

from conch import Sandbox
import conch.sandbox as sandbox_module

TEST_UNIX_SOCKET = "/run/test/conchd.sock"
TEST_CONTROL_PLANE_URL = "http+unix://%2Frun%2Ftest%2Fconchd.sock"


class FakeResponse:
    def __init__(self, status_code=200, data=None):
        self.status_code = status_code
        self.data = data

    def raise_for_status(self):
        return None

    def json(self):
        return self.data


class FakeSession:
    def __init__(self):
        self.put_request = None

    def get(self, url, params=None):
        if url.endswith("/health"):
            return FakeResponse(204)
        if url.endswith("/api/v1/sandboxes"):
            return FakeResponse(data=[{"sandboxID": "sandbox-1"}])
        return FakeResponse(data={
            "sandboxID": "sandbox-1",
            "templateName": "registry.example/conch/test:latest",
            "templateID": "template-1",
            "domain": "192.0.2.10",
            "metadata": {"owner": "test"},
            "network": {"denyOut": ["192.0.2.10"]},
        })

    def delete(self, url, params=None):
        return FakeResponse(204)

    def put(self, url, json=None):
        self.put_request = (url, json)
        return FakeResponse(204)


def test_control_plane_methods_use_configured_endpoint(monkeypatch):
    monkeypatch.setattr(sandbox_module, "DEFAULT_UNIX_SOCKET", TEST_UNIX_SOCKET)
    monkeypatch.setattr(sandbox_module.requests_unixsocket, "Session", lambda: FakeSession())

    assert Sandbox.service_health() is True
    assert Sandbox.list() == [{"sandboxID": "sandbox-1"}]

    sandbox = Sandbox.get("sandbox-1")
    assert sandbox.template_name == "registry.example/conch/test:latest"
    assert sandbox.template_id == "template-1"
    assert sandbox.metadata == {"owner": "test"}
    assert sandbox.control_plane_only is True
    with pytest.raises(RuntimeError, match="Agent credentials unavailable"):
        sandbox.commands.list()
    with pytest.raises(RuntimeError, match="Agent credentials unavailable"):
        sandbox.files.list("/")
    with pytest.raises(RuntimeError, match="Agent credentials unavailable"):
        sandbox.health_check()
    assert sandbox.delete() is True
    assert Sandbox.delete_sandbox("sandbox-1") is True


def test_control_plane_transport_failures(monkeypatch):
    class FailingSession:
        def get(self, url, params=None):
            raise requests.ConnectionError("unavailable")

    monkeypatch.setattr(sandbox_module.requests_unixsocket, "Session", FailingSession)
    monkeypatch.setattr(sandbox_module, "DEFAULT_UNIX_SOCKET", TEST_UNIX_SOCKET)

    assert Sandbox.service_health() is False
    with pytest.raises(RuntimeError, match="unavailable"):
        Sandbox.list()


@pytest.mark.parametrize("close_error", [None, RuntimeError("close failed")])
def test_delete_clears_local_sandbox_state(monkeypatch, close_error):
    monkeypatch.setattr(sandbox_module.requests_unixsocket, "Session", FakeSession)
    sandbox = Sandbox.get("sandbox-1")
    client = Mock()
    client.close.side_effect = close_error
    sandbox.client = client
    cached_fields = (
        "ip", "agent_token", "template_name", "template_id", "vcpu_num",
        "vcpu_max", "ram_mb", "vmm_name", "image_name", "snapshot_id",
        "started_at", "end_at", "disk_size_mb", "conch_init_version", "alias",
    )
    for field in cached_fields:
        setattr(sandbox, field, "cached")
    sandbox.volume_mounts = [{"path": "/data"}]
    sandbox.env = {"KEY": "value"}
    sandbox.lifecycle = {"state": "running"}

    assert sandbox.delete() is True

    assert sandbox.get_info() == sandbox_module.SandboxInfo("", "", None, None)
    assert all(getattr(sandbox, field) is None for field in cached_fields)
    assert sandbox.metadata == {}
    assert sandbox.lifecycle == {}
    assert sandbox.volume_mounts == []
    assert sandbox.env is None
    assert sandbox.network is None
    assert sandbox._client is None
    client.close.assert_called_once_with()
    with pytest.raises(RuntimeError, match="not initialized"):
        sandbox.health_check()


@pytest.mark.parametrize("delete_fails", [False, True])
def test_delete_preserves_state_on_failure_or_other_target(monkeypatch, delete_fails):
    monkeypatch.setattr(sandbox_module.requests_unixsocket, "Session", FakeSession)
    sandbox = Sandbox.get("sandbox-1")
    client = Mock()
    sandbox.client = client
    before = vars(sandbox).copy()
    delete = Mock(side_effect=requests.HTTPError("delete failed") if delete_fails else None)
    monkeypatch.setattr(sandbox._session, "delete", delete)

    if delete_fails:
        with pytest.raises(RuntimeError, match="delete failed"):
            sandbox.delete()
    else:
        delete.return_value = FakeResponse(204)
        assert sandbox.delete("sandbox-2") is True
        assert delete.call_args.args[0].endswith("/sandboxes/sandbox-2")

    assert vars(sandbox) == before
    client.close.assert_not_called()


def test_control_plane_structured_error_uses_code_and_message():
    response = requests.Response()
    response.status_code = 400
    response._content = (
        b'{"status":"error","code":"sandbox.invalid_environment",'
        b'"error":"invalid sandbox environment"}'
    )
    error = requests.HTTPError(response=response)

    assert sandbox_module._request_exception_message(error) == (
        "sandbox.invalid_environment: invalid sandbox environment"
    )


def test_control_plane_plain_text_error_fallback():
    response = requests.Response()
    response.status_code = 500
    response._content = b"legacy server failure\n"
    error = requests.HTTPError(response=response)

    assert sandbox_module._request_exception_message(error) == "legacy server failure"


def test_checkpoint_uses_requested_name_when_response_only_contains_id(monkeypatch):
    class CheckpointSession:
        def __init__(self):
            self.payload = None

        def post(self, url, json=None):
            self.payload = json
            return FakeResponse(data={"status": "ok", "template_id": "sha256:checkpoint"})

    session = CheckpointSession()
    monkeypatch.setattr(sandbox_module.requests_unixsocket, "Session", lambda: session)
    sandbox = Sandbox(sandbox_id="sandbox-1")

    result = sandbox.checkpoint("  registry.example/conch/checkpoint:latest  ")

    assert result.template_name == "registry.example/conch/checkpoint:latest"
    assert result.template_id == "sha256:checkpoint"
    assert session.payload == {
        "sandbox_id": "sandbox-1",
        "template_name": "registry.example/conch/checkpoint:latest",
    }


def test_network_config_is_hydrated_and_updated(monkeypatch):
    session = FakeSession()
    monkeypatch.setattr(sandbox_module, "DEFAULT_UNIX_SOCKET", TEST_UNIX_SOCKET)
    monkeypatch.setattr(sandbox_module.requests_unixsocket, "Session", lambda: session)

    sandbox = Sandbox.get("sandbox-1")
    assert sandbox.network == {"denyOut": ["192.0.2.10"]}
    assert sandbox.update_network(
        allow_out=["198.51.100.10"],
        deny_in=["203.0.113.0/24"],
        allow_internet_access=False,
    ) is True
    assert session.put_request == (
        TEST_CONTROL_PLANE_URL + "/api/v1/sandboxes/sandbox-1/network",
        {
            "allowOut": ["198.51.100.10"],
            "denyIn": ["203.0.113.0/24"],
            "allow_internet_access": False,
        },
    )
