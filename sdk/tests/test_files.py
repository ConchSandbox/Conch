import os
import tempfile

import pytest
from connectrpc.code import Code
from connectrpc.errors import ConnectError

from conch import FileType, InvalidArgumentError, NotFoundError, Sandbox
from conch.client import AgentClient
from api.py_proto import agent_pb2


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
    sandbox = Sandbox(api_url="http://unused", template_id="test")
    sandbox.client = FakeFileClient(response)
    return sandbox


class FakeDownloadChunk:
    def __init__(self, content):
        self.content = content


class FakeDownloadClient:
    def __init__(self, chunks):
        self.chunks = chunks

    def get_file_stream(self, request, headers=None):
        return self.chunks


def client_with_download_chunks(chunks):
    client = AgentClient.__new__(AgentClient)
    client.token = None
    client.file_client = FakeDownloadClient(chunks)
    return client


def test_get_file_writes_complete_download():
    client = client_with_download_chunks([
        FakeDownloadChunk(b"hello "),
        FakeDownloadChunk(b"conch"),
    ])

    with tempfile.TemporaryDirectory() as tmpdir:
        local_path = os.path.join(tmpdir, "downloaded.txt")
        result = client.get_file("/tmp/remote.txt", local_path)

        assert result == {"status": AgentClient.STATUS_SUCCESS, "size": 11, "message": "OK"}
        with open(local_path, "rb") as f:
            assert f.read() == b"hello conch"


def test_get_file_failure_preserves_existing_file():
    def failing_chunks():
        yield FakeDownloadChunk(b"partial")
        raise OSError("download failed")

    client = client_with_download_chunks(failing_chunks())

    with tempfile.TemporaryDirectory() as tmpdir:
        local_path = os.path.join(tmpdir, "downloaded.txt")
        with open(local_path, "wb") as f:
            f.write(b"original")

        with pytest.raises(OSError, match="download failed"):
            client.get_file("/tmp/remote.txt", local_path)

        with open(local_path, "rb") as f:
            assert f.read() == b"original"
        assert not any(name.startswith(".conch-download-") for name in os.listdir(tmpdir))


def test_get_file_failure_does_not_leave_partial_file():
    def failing_chunks():
        yield FakeDownloadChunk(b"partial")
        raise OSError("download failed")

    client = client_with_download_chunks(failing_chunks())

    with tempfile.TemporaryDirectory() as tmpdir:
        local_path = os.path.join(tmpdir, "downloaded.txt")

        with pytest.raises(OSError, match="download failed"):
            client.get_file("/tmp/remote.txt", local_path)

        assert not os.path.exists(local_path)
        assert not any(name.startswith(".conch-download-") for name in os.listdir(tmpdir))


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


class FakeUploadFileClient:
    def __init__(self):
        self.calls = []

    def post_file_stream(self, chunks, headers=None):
        chunk_list = list(chunks)
        self.calls.append(chunk_list)
        path = chunk_list[0].filepath
        return agent_pb2.PostFilesResponse(
            uploaded_count=1,
            entries=[agent_pb2.WriteInfo(name=os.path.basename(path), path=path, type="file")],
        )


def test_agent_client_post_files_uploads_text_and_bytes_content():
    file_client = FakeUploadFileClient()
    client = AgentClient("127.0.0.1")
    client.file_client = file_client

    result = client.post_files([
        {"filepath": "/tmp/a.txt", "content": "hello"},
        {"filepath": "/tmp/b.bin", "content": b"world"},
    ])

    assert result["status"] == AgentClient.STATUS_SUCCESS
    assert result["uploaded_count"] == 2
    assert result["entries"] == [
        {"name": "a.txt", "path": "/tmp/a.txt", "type": "file"},
        {"name": "b.bin", "path": "/tmp/b.bin", "type": "file"},
    ]
    assert [(chunk.filepath, chunk.content) for chunk in file_client.calls[0]] == [("/tmp/a.txt", b"hello")]
    assert [(chunk.filepath, chunk.content) for chunk in file_client.calls[1]] == [("/tmp/b.bin", b"world")]


def test_agent_client_post_files_chunks_large_content():
    file_client = FakeUploadFileClient()
    client = AgentClient("127.0.0.1")
    client.file_client = file_client
    client.FILE_CHUNK_SIZE = 3

    client.post_files([{"filepath": "/tmp/large.bin", "content": b"abcdefg"}])

    assert [(chunk.filepath, chunk.content) for chunk in file_client.calls[0]] == [
        ("/tmp/large.bin", b"abc"),
        ("", b"def"),
        ("", b"g"),
    ]


def test_agent_client_post_files_uploads_empty_local_file():
    file_client = FakeUploadFileClient()
    client = AgentClient("127.0.0.1")
    client.file_client = file_client

    with tempfile.NamedTemporaryFile() as src:
        client.post_files([{"filepath": "/tmp/empty.txt", "local_path": src.name}])

    assert [(chunk.filepath, chunk.content) for chunk in file_client.calls[0]] == [("/tmp/empty.txt", b"")]


def test_agent_client_stream_and_read_file():
    class FakeFileClient:
        def get_file_stream(self, request, headers=None):
            assert request.filepath == "/tmp/read.txt"
            return iter([
                agent_pb2.FileChunk(content=b"hello "),
                agent_pb2.FileChunk(content=b"conch"),
            ])

    client = AgentClient("127.0.0.1")
    client.file_client = FakeFileClient()

    assert list(client.stream_file("/tmp/read.txt")) == [b"hello ", b"conch"]
    assert client.read_file("/tmp/read.txt") == b"hello conch"


def test_agent_client_list_and_search_files_map_entries():
    file_entry = agent_pb2.FileEntry(
        name="app.py",
        path="/tmp/app.py",
        type="file",
        size=12,
        permissions="0644",
        modified_time="2026-07-24T00:00:00Z",
        metadata={"lang": "python"},
        is_directory=False,
    )

    class FakeFileClient:
        def list_files(self, request, headers=None):
            assert request.path == "/tmp"
            assert request.depth == 2
            return agent_pb2.ListFilesResponse(entries=[file_entry])

        def search_files(self, request, headers=None):
            assert request.path == "/tmp"
            assert request.pattern == "*.py"
            assert list(request.exclude_patterns) == ["venv"]
            return agent_pb2.SearchFilesResponse(entries=[file_entry])

    client = AgentClient("127.0.0.1")
    client.file_client = FakeFileClient()

    expected = [{
        "name": "app.py",
        "path": "/tmp/app.py",
        "type": "file",
        "size": 12,
        "permissions": "0644",
        "modifiedTime": "2026-07-24T00:00:00Z",
        "metadata": {"lang": "python"},
        "isDirectory": False,
    }]
    assert client.list_files("/tmp", depth=2) == expected
    assert client.search_files("/tmp", "*.py", exclude_patterns=["venv"]) == expected


def test_files_read_rejects_invalid_format():
    sandbox = Sandbox(api_url="http://unused", template_id="test")

    with pytest.raises(InvalidArgumentError, match="format must be one of"):
        sandbox.files.read("/tmp/read.txt", format="json")


def test_file_stream_rpc_errors_are_mapped_to_sdk_errors():
    class FakeFileClient:
        def get_file_stream(self, request, headers=None):
            def events():
                raise ConnectError(Code.NOT_FOUND, f"file not found: {request.filepath}")
                yield
            return events()

    client = AgentClient("127.0.0.1")
    client.file_client = FakeFileClient()

    with pytest.raises(NotFoundError, match="file not found: /tmp/missing") as exc:
        client.read_file("/tmp/missing")
    assert exc.value.__suppress_context__ is True
