import os
import tempfile

import pytest

from conch import FileType, InvalidArgumentError, NotFoundError, Sandbox
from conch.client import AgentClient


def test_upload_local_file(sandbox):
    """Upload a local file to sandbox."""
    with tempfile.NamedTemporaryFile(mode="w", suffix=".txt", delete=False) as f:
        f.write("hello conch")
        local_path = f.name

    try:
        remote_path = "/tmp/test_upload.txt"
        result = sandbox.files.upload(local_path, remote_path)
        assert result.path == remote_path
        assert result.name == "test_upload.txt"
        assert result.type == FileType.FILE

        # Verify content in sandbox
        result = sandbox.commands.run(cmd="cat", args=[remote_path])
        assert "hello conch" in result.stdout
    finally:
        os.unlink(local_path)


def test_upload_batch_content(sandbox):
    """Upload files with in-memory content in batch."""
    result_a = sandbox.files.write("/tmp/a.txt", b"hello")
    result_b = sandbox.files.write("/tmp/b.txt", b"world")
    assert result_a.path == "/tmp/a.txt"
    assert result_b.path == "/tmp/b.txt"
    assert result_a.type == FileType.FILE
    assert result_b.type == FileType.FILE

    # Verify content
    r1 = sandbox.commands.run(cmd="cat", args=["/tmp/a.txt"])
    r2 = sandbox.commands.run(cmd="cat", args=["/tmp/b.txt"])
    assert "hello" in r1.stdout
    assert "world" in r2.stdout


def test_read_text_bytes_and_stream(sandbox):
    """Read files in E2B-style text, bytes and stream formats."""
    sandbox.files.write("/tmp/read_test.txt", "hello")

    assert sandbox.files.read("/tmp/read_test.txt") == "hello"
    assert sandbox.files.read("/tmp/read_test.txt", format="bytes") == b"hello"
    assert b"".join(sandbox.files.read("/tmp/read_test.txt", format="stream")) == b"hello"


def test_download(sandbox):
    """Download a file from sandbox."""
    # Prepare a file in sandbox
    sandbox.commands.run(cmd="sh", args=["-c", "echo download_test > /tmp/test_dl.txt"])

    with tempfile.TemporaryDirectory() as tmpdir:
        local_path = os.path.join(tmpdir, "downloaded.txt")
        result = sandbox.files.download("/tmp/test_dl.txt", local_path)
        assert result["status"] == 0
        assert result["size"] > 0
        with open(local_path, "r") as f:
            assert "download_test" in f.read()


def test_list_files(sandbox):
    """List files in sandbox directory."""
    sandbox.commands.run(cmd="sh", args=["-c", "echo test > /tmp/list_test.txt"])
    files = sandbox.files.list("/tmp")
    assert any(entry.name == "list_test.txt" and entry.type == FileType.FILE for entry in files)


def test_search_files(sandbox):
    """Search files in sandbox directory."""
    sandbox.files.write("/tmp/search_test.py", "print('hello')")
    files = sandbox.files.search("/tmp", "*.py")
    assert any(entry.path == "/tmp/search_test.py" for entry in files)


class FakeFileClient:
    STATUS_SUCCESS = 0

    def __init__(self, response):
        self.response = response

    def post_files(self, files):
        return self.response


def sandbox_with_file_response(response):
    sandbox = Sandbox(api_url="http://unused", image_name="test")
    sandbox.client = FakeFileClient(response)
    return sandbox


def test_write_uses_server_entries():
    """WriteInfo should come from server entries when available."""
    sandbox = sandbox_with_file_response({
        "status": FakeFileClient.STATUS_SUCCESS,
        "uploaded_count": 1,
        "entries": [{"name": "server.txt", "path": "/tmp/server.txt", "type": "file"}],
        "message": "uploaded 1 files",
    })

    result = sandbox.files.write("/tmp/server.txt", "hello")
    assert result.name == "server.txt"
    assert result.path == "/tmp/server.txt"
    assert result.type == FileType.FILE


def test_write_requires_server_entries():
    """WriteInfo must come from conch-init upload response entries."""
    sandbox = sandbox_with_file_response({
        "status": FakeFileClient.STATUS_SUCCESS,
        "uploaded_count": 1,
        "entries": [],
        "message": "uploaded 1 files",
    })

    try:
        sandbox.files.write("/tmp/missing-entry.txt", "hello")
    except RuntimeError as exc:
        assert "file upload response incomplete" in str(exc)
    else:
        raise AssertionError("files.write() succeeded without server entries")


def test_get_files_collects_local_write_failures():
    """Batch download should report local filesystem failures per file."""
    client = AgentClient.__new__(AgentClient)

    def fail_local_write(remote, local):
        raise OSError("disk full")

    client.get_file = fail_local_write
    result = client.get_files([{"remote": "/tmp/remote.txt", "local": "/tmp/local.txt"}])

    assert result["status"] == AgentClient.STATUS_FAILED
    assert result["downloaded_count"] == 0
    assert result["failed"] == [{
        "remote": "/tmp/remote.txt",
        "local": "/tmp/local.txt",
        "error": "disk full",
    }]


def test_get_files_propagates_sdk_errors():
    """RPC/SDK errors should not be hidden as per-file download failures."""
    client = AgentClient.__new__(AgentClient)

    def fail_remote_lookup(remote, local):
        raise NotFoundError("file not found")

    client.get_file = fail_remote_lookup

    with pytest.raises(NotFoundError, match="file not found"):
        client.get_files([{"remote": "/tmp/missing.txt", "local": "/tmp/local.txt"}])


def test_get_files_requires_remote_and_local():
    client = AgentClient.__new__(AgentClient)

    with pytest.raises(InvalidArgumentError, match="need 'remote' and 'local'"):
        client.get_files([{"remote": "/tmp/remote.txt"}])
