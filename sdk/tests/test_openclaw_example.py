import importlib.util
from pathlib import Path

import pytest


EXAMPLE_PATH = Path(__file__).resolve().parents[2] / "examples" / "openclaw.py"
SPEC = importlib.util.spec_from_file_location("conch_openclaw_example", EXAMPLE_PATH)
openclaw = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(openclaw)


@pytest.mark.parametrize("username", ["root; touch /tmp/pwned", "-oProxyCommand=id", "user name", ""])
def test_openclaw_rejects_untrusted_sandbox_username(username):
    with pytest.raises(ValueError, match="invalid account name"):
        openclaw.build_openclaw_ssh_command(username, "192.0.2.10")


@pytest.mark.parametrize("address", ["192.0.2.10;id", "example.com", "127.0.0.1 -oProxyCommand=id", ""])
def test_openclaw_rejects_untrusted_sandbox_address(address):
    with pytest.raises(ValueError, match="invalid IPv4 address"):
        openclaw.build_openclaw_ssh_command("root", address)


def test_openclaw_builds_argv_without_persisting_host_keys():
    command = openclaw.build_openclaw_ssh_command("sandbox-user", "192.0.2.10")

    assert command[0] == "ssh"
    assert command[-2] == "sandbox-user@192.0.2.10"
    assert command[-1] == openclaw.OPENCLAW_REMOTE_COMMAND
    assert "StrictHostKeyChecking=no" in command
    assert "StrictHostKeyChecking=accept-new" not in command
    assert "UserKnownHostsFile=/dev/null" in command
