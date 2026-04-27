import os
import tempfile


def test_upload_local_file(sandbox):
    """Upload a local file to sandbox."""
    with tempfile.NamedTemporaryFile(mode="w", suffix=".txt", delete=False) as f:
        f.write("hello conch")
        local_path = f.name

    try:
        remote_path = "/tmp/test_upload.txt"
        result = sandbox.upload(local_path, remote_path)
        assert result["status"] == 0

        # Verify content in sandbox
        result = sandbox.execute(cmd="cat", args=[remote_path])
        assert "hello conch" in result.stdout
    finally:
        os.unlink(local_path)


def test_upload_batch_content(sandbox):
    """Upload files with in-memory content in batch."""
    result = sandbox.upload([
        {"filepath": "/tmp/a.txt", "content": b"hello"},
        {"filepath": "/tmp/b.txt", "content": b"world"},
    ])
    assert result["status"] == 0
    assert result["uploaded_count"] == 2

    # Verify content
    r1 = sandbox.execute(cmd="cat", args=["/tmp/a.txt"])
    r2 = sandbox.execute(cmd="cat", args=["/tmp/b.txt"])
    assert "hello" in r1.stdout
    assert "world" in r2.stdout


def test_download(sandbox):
    """Download a file from sandbox."""
    # Prepare a file in sandbox
    sandbox.execute(cmd="sh", args=["-c", "echo download_test > /tmp/test_dl.txt"])

    with tempfile.TemporaryDirectory() as tmpdir:
        local_path = os.path.join(tmpdir, "downloaded.txt")
        result = sandbox.download("/tmp/test_dl.txt", local_path)
        assert result["status"] == 0
        assert result["size"] > 0
        with open(local_path, "r") as f:
            assert "download_test" in f.read()


def test_list_files(sandbox):
    """List files in sandbox directory."""
    sandbox.execute(cmd="sh", args=["-c", "echo test > /tmp/list_test.txt"])
    files = sandbox.list_files("/tmp")
    assert any("list_test.txt" in f for f in files)
