import pytest
from conch import Sandbox


@pytest.fixture
def sandbox():
    """Create a sandbox before test, auto-delete after test."""
    sbx = Sandbox.create()
    yield sbx
    try:
        sbx.delete()
    except Exception:
        pass
