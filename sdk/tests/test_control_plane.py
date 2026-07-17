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
        if url.endswith("/logs"):
            return FakeResponse(data={"logs": []})
        if url.endswith("/api/v1/sandboxes"):
            return FakeResponse(data=[{"sandboxID": "sandbox-1"}])
        return FakeResponse(data={
            "sandboxID": "sandbox-1",
            "namespace": "default",
            "templateID": "tmpl-1",
        })

    def put(self, url, json=None, params=None):
        return FakeResponse(204)

    def delete(self, url, params=None):
        return FakeResponse(204)


def test_control_plane_methods_use_explicit_endpoint_without_config(monkeypatch):
    def fail_load_config(config_path=None):
        raise AssertionError("config loaded")

    monkeypatch.setattr(sandbox_module, "load_config", fail_load_config)
    monkeypatch.setattr(sandbox_module.requests, "Session", lambda: FakeSession())

    api_url = "http://control.example"
    assert Sandbox.service_health(api_url=api_url) is True
    assert Sandbox.list(api_url=api_url) == [{"sandboxID": "sandbox-1"}]

    sandbox = Sandbox.get("sandbox-1", api_url=api_url)
    assert sandbox.template_id == "tmpl-1"
    assert sandbox.logs() == {"logs": []}
    assert sandbox.update_network(allow_out=["192.0.2.1"]) == {}
    assert sandbox.delete() is True
    assert Sandbox.delete_sandbox("sandbox-1", api_url=api_url) is True
