import pytest
import requests

from conch import Sandbox, TemplateNotFoundError


class FakeControlPlane:
    def post(self, *_args, **_kwargs):
        response = requests.Response()
        response.status_code = 404
        response._content = (
            b'{"version":1,"code":"not_found","resource_type":"template",'
            b'"message":"template not found","request_id":"req_sdk_test"}'
        )
        response.url = "http://fake/api/v1/sandboxes"
        raise requests.HTTPError(response=response)


def test_missing_template_is_a_dedicated_sdk_exception(monkeypatch):
    monkeypatch.setattr(
        "conch.sandbox.load_config",
        lambda config_path=None: {
            "sandbox": {"api_url": "http://fake"},
            "image": {"vmm_name": "", "vcpu_num": 2, "vcpu_max": 2, "ram_mb": 4096},
        },
    )
    sandbox = Sandbox(template_id="tmpl_missing")
    sandbox._session = FakeControlPlane()

    with pytest.raises(TemplateNotFoundError) as raised:
        sandbox._do_create()

    assert raised.value.code == "not_found"
    assert raised.value.resource_type == "template"
    assert raised.value.request_id == "req_sdk_test"
