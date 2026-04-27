from conch import Sandbox


def test_snapshot_lifecycle():
    """Full snapshot lifecycle: create -> pause -> restore -> delete."""
    # Step 1: create sandbox and write data
    sbx = Sandbox.create()
    sbx.execute(cmd="sh", args=["-c", "echo snapshot_data > /tmp/test.txt"])
    result = sbx.execute(cmd="cat", args=["/tmp/test.txt"])
    assert "snapshot_data" in result.stdout

    # Step 2: pause and get snapshot
    snapshot = sbx.pause()
    assert snapshot.snapshot_id is not None

    # Step 3: restore from snapshot and verify data
    sbx2 = Sandbox.create(snapshot_id=snapshot.snapshot_id)
    try:
        result = sbx2.execute(cmd="cat", args=["/tmp/test.txt"])
        assert "snapshot_data" in result.stdout
    finally:
        sbx2.delete()
