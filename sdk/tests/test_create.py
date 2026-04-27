from conch import Sandbox
from conch.config_loader import load_config


def test_create_default(sandbox):
    """Create sandbox from default image in config."""
    assert sandbox.sandbox_id is not None
    assert sandbox.sandbox_id.startswith("sandbox_")
    assert sandbox.ip is not None


def test_create_with_image_name():
    """Create sandbox with specified image."""
    image = load_config()["image"]["image_name"]
    sbx = Sandbox.create(image_name=image)
    try:
        assert sbx.sandbox_id is not None
        result = sbx.execute(cmd="ls", args=["/"])
        assert result.exit_code == 0
    finally:
        sbx.delete()


def test_create_with_snapshot():
    """Create sandbox from snapshot: create -> pause -> restore."""
    sbx = Sandbox.create()
    snapshot = sbx.pause()
    assert snapshot.snapshot_id is not None
    assert snapshot.sandbox_id == sbx.sandbox_id

    sbx2 = Sandbox.create(snapshot_id=snapshot.snapshot_id)
    try:
        assert sbx2.sandbox_id is not None
        result = sbx2.execute(cmd="ls", args=["/"])
        assert result.exit_code == 0
    finally:
        sbx2.delete()


def test_context_manager():
    """Sandbox supports context manager protocol."""
    with Sandbox.create() as sbx:
        assert sbx.sandbox_id is not None
        result = sbx.execute(cmd="echo", args=["hello"])
        assert result.exit_code == 0
