from conch import Sandbox


def test_health_check(sandbox):
    """Check sandbox agent health."""
    result = sandbox.health_check()
    assert result["status"] == "OK"


def test_get_info(sandbox):
    """Get sandbox info."""
    info = sandbox.get_info()
    assert info.sandbox_id is not None
    assert info.sandbox_id == sandbox.sandbox_id
    assert info.ip is not None
    assert info.snapshot_id is None or info.snapshot_id == ""


def test_delete_sandbox_static():
    """Delete sandbox via static method."""
    sbx = Sandbox.create()
    sandbox_id = sbx.sandbox_id
    assert Sandbox.delete_sandbox(sandbox_id) is True
