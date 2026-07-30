import pytest
import requests

from conch import Sandbox
import conch.sandbox as sandbox_module


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
        self.last_get_params = None

    def get(self, url, params=None):
        self.last_get_params = params
        if url.endswith("/health"):
            return FakeResponse(204)
        if url.endswith("/logs"):
            return FakeResponse(data={"logs": []})
        if url.endswith("/api/v1/sandboxes"):
            return FakeResponse(data=[{"sandboxID": "sandbox-1"}])
        return FakeResponse(data={
            "sandboxID": "sandbox-1",
            "namespace": "default",
            "templateID": "tmpl-1",
            "domain": "192.0.2.10",
            "allowInternetAccess": False,
            "network": {"allowOut": ["192.0.2.1"]},
            "metadata": {"owner": "test"},
        })

    def put(self, url, json=None, params=None):
        return FakeResponse(204)

    def delete(self, url, params=None):
        return FakeResponse(204)


def test_control_plane_methods_use_explicit_endpoint_without_config(monkeypatch):
    def fail_load_config(config_path=None):
        raise AssertionError("config loaded")

    fake_session = FakeSession()
    monkeypatch.setattr(sandbox_module, "load_config", fail_load_config)
    monkeypatch.setattr(sandbox_module.requests, "Session", lambda: fake_session)

    api_url = "http://control.example"
    assert Sandbox.service_health(api_url=api_url) is True
    assert Sandbox.list(api_url=api_url) == [{"sandboxID": "sandbox-1"}]

    sandbox = Sandbox.get("sandbox-1", api_url=api_url)
    assert sandbox.template_id == "tmpl-1"
    assert sandbox.network == {
        "allowOut": ["192.0.2.1"],
        "allow_internet_access": False,
    }
    assert sandbox.metadata == {"owner": "test"}
    assert sandbox.control_plane_only is True
    with pytest.raises(RuntimeError, match="Agent credentials unavailable"):
        sandbox.commands.list()
    with pytest.raises(RuntimeError, match="Agent credentials unavailable"):
        sandbox.files.list("/")
    with pytest.raises(RuntimeError, match="Agent credentials unavailable"):
        sandbox.health_check()
    assert sandbox.logs(
        cursor=1000,
        limit=10,
        direction="backward",
        level="error",
        search="failed",
    ) == {"logs": []}
    assert fake_session.last_get_params == {
        "cursor": 1000,
        "limit": 10,
        "direction": "backward",
        "level": "error",
        "search": "failed",
        "namespace": "default",
    }
    assert sandbox.update_network(allow_out=["192.0.2.1"]) == {}
    assert sandbox.delete() is True
    assert Sandbox.delete_sandbox("sandbox-1", api_url=api_url) is True


def test_control_plane_transport_failures(monkeypatch):
    class FailingSession:
        def get(self, url, params=None):
            raise requests.ConnectionError("unavailable")

    monkeypatch.setattr(sandbox_module.requests, "Session", FailingSession)

    assert Sandbox.service_health(api_url="http://control.example") is False
    with pytest.raises(RuntimeError, match="unavailable"):
        Sandbox.list(api_url="http://control.example")
