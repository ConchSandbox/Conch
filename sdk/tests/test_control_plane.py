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
    def get(self, url, params=None):
        if url.endswith("/health"):
            return FakeResponse(204)
        if url.endswith("/api/v1/sandboxes"):
            return FakeResponse(data=[{"sandboxID": "sandbox-1"}])
        return FakeResponse(data={
            "sandboxID": "sandbox-1",
            "templateID": "tmpl-1",
            "domain": "192.0.2.10",
            "metadata": {"owner": "test"},
        })

    def delete(self, url, params=None):
        return FakeResponse(204)


def test_control_plane_methods_use_configured_endpoint(monkeypatch):
    monkeypatch.setattr(
        sandbox_module,
        "load_config",
        lambda: {"sandbox": {"api_url": "http://control.example"}},
    )
    monkeypatch.setattr(sandbox_module.requests, "Session", lambda: FakeSession())

    assert Sandbox.service_health() is True
    assert Sandbox.list() == [{"sandboxID": "sandbox-1"}]

    sandbox = Sandbox.get("sandbox-1")
    assert sandbox.template_id == "tmpl-1"
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

    monkeypatch.setattr(sandbox_module.requests, "Session", FailingSession)
    monkeypatch.setattr(
        sandbox_module,
        "load_config",
        lambda: {"sandbox": {"api_url": "http://control.example"}},
    )

    assert Sandbox.service_health() is False
    with pytest.raises(RuntimeError, match="unavailable"):
        Sandbox.list()
