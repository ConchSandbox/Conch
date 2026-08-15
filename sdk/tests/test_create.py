import os

import pytest

from conch import Sandbox


def test_create_default(sandbox):
    """Create sandbox from default source in config."""
    assert sandbox.sandbox_id is not None
    assert sandbox.sandbox_id.startswith("sandbox_")
    assert sandbox.ip is not None


def test_create_with_template_id():
    """Create sandbox with specified source."""
    template_id = os.getenv("CONCH_TEST_TEMPLATE_ID")
    if not template_id:
        pytest.skip("set CONCH_TEST_TEMPLATE_ID to run this test")
    sbx = Sandbox.create(template_id=template_id)
    try:
        assert sbx.sandbox_id is not None
        result = sbx.commands.run(cmd="ls", args=["/"])
        assert result.exit_code == 0
    finally:
        sbx.delete()


def test_create_with_checkpoint():
    """Create sandbox from checkpoint: create -> checkpoint -> restore."""
    sbx = Sandbox.create()
    template = sbx.checkpoint()
    assert template.template_id is not None
    assert template.sandbox_id == sbx.sandbox_id

    sbx2 = Sandbox.create(template_id=template.template_id)
    try:
        assert sbx2.sandbox_id is not None
        result = sbx2.commands.run(cmd="ls", args=["/"])
        assert result.exit_code == 0
    finally:
        sbx2.delete()


def test_context_manager():
    """Sandbox supports context manager protocol."""
    with Sandbox.create() as sbx:
        assert sbx.sandbox_id is not None
        result = sbx.commands.run(cmd="echo", args=["hello"])
        assert result.exit_code == 0


def test_build_create_payload_without_vmm_name_uses_server_default():
    sbx = Sandbox(sandbox_id="sandbox-test", template_id="template-test")

    payload = sbx._build_create_payload()
    assert "vmm_name" not in payload
    assert "namespace" not in payload


def test_build_create_payload_omits_template_id_for_daemon_default():
    payload = Sandbox(sandbox_id="sandbox-test")._build_create_payload()

    assert "template_id" not in payload


def test_build_create_payload_keeps_explicit_vmm_name():
    payload = Sandbox(
        sandbox_id="sandbox-test",
        vmm_name="cloud-hypervisor",
    )._build_create_payload()

    assert payload["vmm_name"] == "cloud-hypervisor"


def test_build_create_payload_trims_vmm_name():
    payload = Sandbox(
        sandbox_id="sandbox-test",
        vmm_name="  stratovirt  ",
    )._build_create_payload()

    assert payload["vmm_name"] == "stratovirt"


def test_build_create_payload_omits_whitespace_vmm_name():
    payload = Sandbox(sandbox_id="sandbox-test", vmm_name=" \t ")._build_create_payload()

    assert "vmm_name" not in payload


def test_build_create_payload_omits_unset_resources_for_daemon_defaults():
    payload = Sandbox(sandbox_id="sandbox-test")._build_create_payload()

    assert "vcpu_num" not in payload
    assert "vcpu_max" not in payload
    assert "ram_mb" not in payload


def test_build_create_payload_keeps_explicit_resources():
    payload = Sandbox(
        sandbox_id="sandbox-test",
        vcpu_num=4,
        vcpu_max=8,
        ram_mb=8192,
    )._build_create_payload()

    assert payload["vcpu_num"] == 4
    assert payload["vcpu_max"] == 8
    assert payload["ram_mb"] == 8192


def test_build_create_payload_omits_whitespace_template_id():
    payload = Sandbox(sandbox_id="sandbox-test", template_id=" \t ")._build_create_payload()

    assert "template_id" not in payload


def test_build_create_payload_keeps_explicit_template_id():
    payload = Sandbox(sandbox_id="sandbox-test", template_id="template-explicit")._build_create_payload()

    assert payload["template_id"] == "template-explicit"


def test_build_create_payload_preserves_environment():
    environment = {
        "EMPTY": "",
        "WITH_EQUALS": "value=with spaces",
    }

    payload = Sandbox(
        sandbox_id="sandbox-test",
        template_id="template-explicit",
        env=environment,
    )._build_create_payload()

    assert payload["env"] == environment


def test_build_create_payload_distinguishes_absent_and_empty_environment():
    without_environment = Sandbox(
        sandbox_id="sandbox-test",
        template_id="template-explicit",
    )._build_create_payload()
    with_empty_environment = Sandbox(
        sandbox_id="sandbox-test",
        template_id="template-explicit",
        env={},
    )._build_create_payload()

    assert "env" not in without_environment
    assert with_empty_environment["env"] == {}
