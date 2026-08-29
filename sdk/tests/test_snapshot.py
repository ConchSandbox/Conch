from conch import Sandbox


def test_snapshot_lifecycle():
    """Full checkpoint lifecycle: create -> checkpoint -> restore -> delete."""
    # Step 1: create sandbox and write data
    sbx = Sandbox.create()
    sbx.commands.run(cmd="sh", args=["-c", "echo snapshot_data > /tmp/test.txt"])
    result = sbx.commands.run(cmd="cat", args=["/tmp/test.txt"])
    assert "snapshot_data" in result.stdout

    # Step 2: checkpoint and get source ID
    template = sbx.checkpoint(f"localhost/conch/snapshot-{sbx.sandbox_id}:latest")
    assert template.template_id is not None

    # Step 3: restore from checkpoint and verify data
    sbx2 = Sandbox.create(template_name=template.template_name)
    try:
        result = sbx2.commands.run(cmd="cat", args=["/tmp/test.txt"])
        assert "snapshot_data" in result.stdout
    finally:
        sbx2.delete()
