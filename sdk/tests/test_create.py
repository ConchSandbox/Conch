from conch import Sandbox
from conch.config_loader import load_config
from conch.sandbox import VMM_NAME_KEY


def test_create_default(sandbox):
    """Create sandbox from default source in config."""
    assert sandbox.sandbox_id is not None
    assert sandbox.sandbox_id.startswith("sandbox_")
    assert sandbox.ip is not None


def test_create_with_template_id():
    """Create sandbox with specified source."""
    template_id = load_config()["sandbox"]["template_id"]
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


def test_build_create_payload_without_vmm_name_uses_server_default(monkeypatch):
    config = load_config()
    config["image"].pop(VMM_NAME_KEY, None)

    monkeypatch.setattr("conch.sandbox.load_config", lambda: config)
    sbx = Sandbox(sandbox_id="sandbox-test", template_id="tmpl_test")

    payload = sbx._build_create_payload()
    assert payload[VMM_NAME_KEY] == ""
    assert "namespace" not in payload


def test_build_create_payload_omits_template_id_for_daemon_default(monkeypatch):
    config = load_config()
    config["sandbox"]["template_id"] = "tmpl-client-config-must-not-override-daemon"
    monkeypatch.setattr("conch.sandbox.load_config", lambda: config)

    payload = Sandbox(sandbox_id="sandbox-test")._build_create_payload()

    assert "template_id" not in payload


def test_build_create_payload_omits_whitespace_template_id():
    payload = Sandbox(sandbox_id="sandbox-test", template_id=" \t ")._build_create_payload()

    assert "template_id" not in payload


def test_build_create_payload_keeps_explicit_template_id():
    payload = Sandbox(sandbox_id="sandbox-test", template_id="tmpl-explicit")._build_create_payload()

    assert payload["template_id"] == "tmpl-explicit"


def test_build_create_payload_preserves_environment():
    environment = {
        "EMPTY": "",
        "WITH_EQUALS": "value=with spaces",
    }

    payload = Sandbox(
        sandbox_id="sandbox-test",
        template_id="tmpl-explicit",
        env=environment,
    )._build_create_payload()

    assert payload["env"] == environment


def test_build_create_payload_distinguishes_absent_and_empty_environment():
    without_environment = Sandbox(
        sandbox_id="sandbox-test",
        template_id="tmpl-explicit",
    )._build_create_payload()
    with_empty_environment = Sandbox(
        sandbox_id="sandbox-test",
        template_id="tmpl-explicit",
        env={},
    )._build_create_payload()

    assert "env" not in without_environment
    assert with_empty_environment["env"] == {}
