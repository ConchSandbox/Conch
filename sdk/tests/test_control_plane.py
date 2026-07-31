import pytest
import requests

from conch import InvalidArgumentError, NotFoundError, Sandbox, ServiceUnavailableError
import conch.sandbox as sandbox_module


class FakeResponse:
    def __init__(self, status_code=200, data=None, text=""):
        self.status_code = status_code
        self.data = data
        self.text = text

    def raise_for_status(self):
        if self.status_code >= 400:
            raise requests.HTTPError(self.text, response=self)

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
    fake_session = FakeSession()
    monkeypatch.setattr(sandbox_module.requests, "Session", lambda: fake_session)

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


@pytest.mark.parametrize(
    ("status_code", "error_type"),
    [
        (400, InvalidArgumentError),
        (404, NotFoundError),
        (503, ServiceUnavailableError),
    ],
)
def test_sandbox_logs_maps_http_errors(monkeypatch, status_code, error_type):
    class LogsSession:
        def get(self, url, params=None):
            return FakeResponse(status_code, text="logs request failed")

    monkeypatch.setattr(sandbox_module.requests, "Session", LogsSession)
    monkeypatch.setattr(
        sandbox_module,
        "load_config",
        lambda: {"sandbox": {"api_url": "http://control.example"}},
    )

    with pytest.raises(error_type, match="logs request failed"):
        Sandbox(sandbox_id="sandbox-1").logs()
