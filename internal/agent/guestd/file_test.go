package guestd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/openeuler/Conch/api/go_proto"
)

type fakePostFileStream struct {
	chunks   []*pb.FileChunk
	index    int
	response *pb.PostFilesResponse
}

func (s *fakePostFileStream) Recv() (*pb.FileChunk, error) {
	if s.index >= len(s.chunks) {
		return nil, io.EOF
	}
	chunk := s.chunks[s.index]
	s.index++
	return chunk, nil
}

func (s *fakePostFileStream) SendAndClose(resp *pb.PostFilesResponse) error {
	s.response = resp
	return nil
}

type fakeGetFileStream struct {
	chunks []*pb.FileChunk
}

func (s *fakeGetFileStream) Send(chunk *pb.FileChunk) error {
	s.chunks = append(s.chunks, chunk)
	return nil
}

func TestCleanAgentFilepathRequiresNormalizedAbsoluteGuestPath(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		want      string
		wantError bool
	}{
		{
			name: "guest root",
			path: "/",
			want: "/",
		},
		{
			name: "absolute guest file",
			path: "/etc/passwd",
			want: "/etc/passwd",
		},
		{
			name: "volume-visible guest file",
			path: "/workspace/project/data.txt",
			want: "/workspace/project/data.txt",
		},
		{
			name:      "empty",
			path:      "",
			wantError: true,
		},
		{
			name:      "relative",
			path:      "etc/passwd",
			wantError: true,
		},
		{
			name:      "parent-relative",
			path:      "../etc/passwd",
			wantError: true,
		},
		{
			name:      "absolute parent traversal",
			path:      "/workspace/../etc/passwd",
			wantError: true,
		},
		{
			name:      "parent traversal above root",
			path:      "/../etc/passwd",
			wantError: true,
		},
		{
			name:      "dot segment",
			path:      "/etc/./passwd",
			wantError: true,
		},
		{
			name:      "repeated separator",
			path:      "/etc//passwd",
			wantError: true,
		},
		{
			name:      "trailing separator",
			path:      "/etc/passwd/",
			wantError: true,
		},
		{
			name:      "NUL byte",
			path:      "/etc/passwd\x00ignored",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, errMsg := cleanAgentFilepath(tt.path, "test")
			if tt.wantError {
				if errMsg == "" {
					t.Errorf("cleanAgentFilepath(%q) accepted path as %q, want validation error", tt.path, got)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("cleanAgentFilepath(%q) error = %q", tt.path, errMsg)
			}
			if got != tt.want {
				t.Fatalf("cleanAgentFilepath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestFileAccessCodeMapsPermissionDenied(t *testing.T) {
	if got := fileAccessCode(os.ErrPermission); got != connect.CodePermissionDenied {
		t.Fatalf("fileAccessCode(os.ErrPermission) = %v, want %v", got, connect.CodePermissionDenied)
	}
	if got := fileAccessCode(os.ErrInvalid); got != connect.CodeInternal {
		t.Fatalf("fileAccessCode(os.ErrInvalid) = %v, want %v", got, connect.CodeInternal)
	}
}

func TestPostFileStreamUploadsChunks(t *testing.T) {
	server := &AgentServer{}
	path := filepath.Join(t.TempDir(), "stream.txt")
	stream := &fakePostFileStream{
		chunks: []*pb.FileChunk{
			{Filepath: path, Content: []byte("hello ")},
			{Content: []byte("world")},
		},
	}

	if err := server.PostFileStream(stream); err != nil {
		t.Fatalf("PostFileStream() error = %v", err)
	}
	if stream.response == nil {
		t.Fatal("PostFileStream() did not send close response")
	}
	if stream.response.Error != "" {
		t.Fatalf("PostFileStream() response error = %q, want empty", stream.response.Error)
	}
	if stream.response.UploadedCount != 1 {
		t.Fatalf("PostFileStream() uploaded count = %d, want 1", stream.response.UploadedCount)
	}
	if len(stream.response.Entries) != 1 {
		t.Fatalf("PostFileStream() response entries = %d, want 1", len(stream.response.Entries))
	}
	entry := stream.response.Entries[0]
	if entry.Name != "stream.txt" || entry.Path != path || entry.Type != "file" {
		t.Fatalf("PostFileStream() entry = %+v, want name stream.txt path %q type file", entry, path)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(content) != "hello world" {
		t.Fatalf("uploaded content = %q, want hello world", content)
	}
}

func TestPostFileStreamRejectsOversizedChunk(t *testing.T) {
	server := &AgentServer{}
	stream := &fakePostFileStream{
		chunks: []*pb.FileChunk{
			{Filepath: filepath.Join(t.TempDir(), "large.bin"), Content: make([]byte, agentFileChunkBytes+1)},
		},
	}

	err := server.PostFileStream(stream)
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("PostFileStream() error = %v, want code %v", err, connect.CodeResourceExhausted)
	}
	if stream.response != nil {
		t.Fatalf("PostFileStream() response = %+v, want nil on status error", stream.response)
	}
}

func TestPostFileStreamFailureDoesNotOverwriteTarget(t *testing.T) {
	server := &AgentServer{}
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(path, []byte("original"), FilePerm); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}

	stream := &fakePostFileStream{
		chunks: []*pb.FileChunk{
			{Filepath: path, Content: []byte("partial")},
			{Filepath: filepath.Join(dir, "other.txt"), Content: []byte("should-fail")},
		},
	}

	err := server.PostFileStream(stream)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("PostFileStream() error = %v, want code %v", err, connect.CodeInvalidArgument)
	}
	if stream.response != nil {
		t.Fatalf("PostFileStream() response = %+v, want nil on status error", stream.response)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(content) != "original" {
		t.Fatalf("target content = %q, want original", content)
	}

	matches, err := filepath.Glob(filepath.Join(dir, ".conch-upload-*"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary upload files remain after failed stream: %v", matches)
	}
}

func TestPostFileStreamRejectsAmbiguousRepeatedFilepath(t *testing.T) {
	server := &AgentServer{}
	dir := t.TempDir()
	path := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(path, []byte("original"), FilePerm); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	separator := string(os.PathSeparator)
	ambiguousPath := dir + separator + "." + separator + filepath.Base(path)
	stream := &fakePostFileStream{
		chunks: []*pb.FileChunk{
			{Filepath: path, Content: []byte("partial")},
			{Filepath: ambiguousPath, Content: []byte("should-fail")},
		},
	}

	err := server.PostFileStream(stream)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("PostFileStream() error = %v, want code %v", err, connect.CodeInvalidArgument)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(content) != "original" {
		t.Fatalf("target content = %q, want original", content)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".conch-upload-*"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary upload files remain after rejected path: %v", matches)
	}
}

func TestPostFileStreamReturnsStatusErrors(t *testing.T) {
	server := &AgentServer{}

	tests := []struct {
		name   string
		chunks []*pb.FileChunk
		want   connect.Code
	}{
		{
			name:   "empty stream",
			chunks: nil,
			want:   connect.CodeInvalidArgument,
		},
		{
			name: "missing filepath",
			chunks: []*pb.FileChunk{
				{Content: []byte("data")},
			},
			want: connect.CodeInvalidArgument,
		},
		{
			name: "filepath changes",
			chunks: []*pb.FileChunk{
				{Filepath: filepath.Join(t.TempDir(), "one.txt"), Content: []byte("one")},
				{Filepath: filepath.Join(t.TempDir(), "two.txt"), Content: []byte("two")},
			},
			want: connect.CodeInvalidArgument,
		},
		{
			name: "oversized chunk",
			chunks: []*pb.FileChunk{
				{Filepath: filepath.Join(t.TempDir(), "large.bin"), Content: make([]byte, agentFileChunkBytes+1)},
			},
			want: connect.CodeResourceExhausted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := &fakePostFileStream{chunks: tt.chunks}
			err := server.PostFileStream(stream)
			if connect.CodeOf(err) != tt.want {
				t.Fatalf("PostFileStream() error = %v, want code %v", err, tt.want)
			}
			if stream.response != nil {
				t.Fatalf("PostFileStream() response = %+v, want nil on status error", stream.response)
			}
		})
	}
}

func TestGetFileStreamDownloadsChunks(t *testing.T) {
	server := &AgentServer{}
	path := filepath.Join(t.TempDir(), "stream.bin")
	want := strings.Repeat("a", agentFileChunkBytes) + "tail"
	if err := os.WriteFile(path, []byte(want), FilePerm); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}

	stream := &fakeGetFileStream{}
	if err := server.GetFileStream(&pb.GetFileRequest{Filepath: path}, stream); err != nil {
		t.Fatalf("GetFileStream() error = %v", err)
	}
	if len(stream.chunks) != 2 {
		t.Fatalf("GetFileStream() chunks = %d, want 2", len(stream.chunks))
	}
	if stream.chunks[0].Filepath != path {
		t.Fatalf("first chunk filepath = %q, want %q", stream.chunks[0].Filepath, path)
	}

	var got strings.Builder
	for _, chunk := range stream.chunks {
		got.Write(chunk.Content)
	}
	if got.String() != want {
		t.Fatalf("streamed content len = %d, want %d", len(got.String()), len(want))
	}
}

func TestGetFileStreamReturnsStatusErrors(t *testing.T) {
	server := &AgentServer{}
	dir := t.TempDir()

	tests := []struct {
		name string
		req  *pb.GetFileRequest
		want connect.Code
	}{
		{
			name: "nil request",
			req:  nil,
			want: connect.CodeInvalidArgument,
		},
		{
			name: "empty filepath",
			req:  &pb.GetFileRequest{},
			want: connect.CodeInvalidArgument,
		},
		{
			name: "missing file",
			req:  &pb.GetFileRequest{Filepath: filepath.Join(dir, "missing.txt")},
			want: connect.CodeNotFound,
		},
		{
			name: "directory path",
			req:  &pb.GetFileRequest{Filepath: dir},
			want: connect.CodeInvalidArgument,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := server.GetFileStream(tt.req, &fakeGetFileStream{})
			if connect.CodeOf(err) != tt.want {
				t.Fatalf("GetFileStream() error = %v, want code %v", err, tt.want)
			}
		})
	}
}

func TestFileEntryPointsRejectInvalidGuestPaths(t *testing.T) {
	server := &AgentServer{}
	entryPoints := []struct {
		name string
		call func(string) error
	}{
		{
			name: "PostFileStream",
			call: func(path string) error {
				return server.PostFileStream(&fakePostFileStream{chunks: []*pb.FileChunk{
					{Filepath: path, Content: []byte("data")},
				}})
			},
		},
		{
			name: "GetFileStream",
			call: func(path string) error {
				return server.GetFileStream(&pb.GetFileRequest{Filepath: path}, &fakeGetFileStream{})
			},
		},
		{
			name: "ListFiles",
			call: func(path string) error {
				_, err := server.ListFiles(context.Background(), &pb.ListFilesRequest{Path: path})
				return err
			},
		},
		{
			name: "SearchFiles",
			call: func(path string) error {
				_, err := server.SearchFiles(context.Background(), &pb.SearchFilesRequest{Path: path, Pattern: "*"})
				return err
			},
		},
	}
	invalidPaths := []struct {
		name string
		path string
	}{
		{name: "relative", path: "relative/file.txt"},
		{name: "parent-relative", path: "../etc/passwd"},
		{name: "parent traversal", path: "/workspace/../etc/passwd"},
		{name: "ambiguous cleaned form", path: "/workspace//data.txt"},
		{name: "NUL byte", path: "/workspace/data\x00.txt"},
	}

	for _, entryPoint := range entryPoints {
		t.Run(entryPoint.name, func(t *testing.T) {
			for _, invalidPath := range invalidPaths {
				t.Run(invalidPath.name, func(t *testing.T) {
					err := entryPoint.call(invalidPath.path)
					if connect.CodeOf(err) != connect.CodeInvalidArgument {
						t.Fatalf("entry point accepted path %q: error = %v, want code %v", invalidPath.path, err, connect.CodeInvalidArgument)
					}
				})
			}
		})
	}
}

func TestListFilesHonorsDepth(t *testing.T) {
	server := &AgentServer{}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "top.txt"), []byte("top"), FilePerm); err != nil {
		t.Fatalf("WriteFile(top.txt) error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), DirPerm); err != nil {
		t.Fatalf("Mkdir(sub) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "nested.txt"), []byte("nested"), FilePerm); err != nil {
		t.Fatalf("WriteFile(nested.txt) error = %v", err)
	}

	resp, err := server.ListFiles(context.Background(), &pb.ListFilesRequest{Path: root, Depth: 1})
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}

	paths := map[string]bool{}
	for _, entry := range resp.Entries {
		paths[entry.Path] = true
	}
	if !paths[filepath.Join(root, "top.txt")] || !paths[filepath.Join(root, "sub")] {
		t.Fatalf("ListFiles(depth=1) entries = %+v, want top file and sub dir", resp.Entries)
	}
	if paths[filepath.Join(root, "sub", "nested.txt")] {
		t.Fatalf("ListFiles(depth=1) included nested file: %+v", resp.Entries)
	}
}

func TestListFilesFollowsDirectorySymlink(t *testing.T) {
	server := &AgentServer{}
	root := t.TempDir()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "linked.txt"), []byte("linked"), FilePerm); err != nil {
		t.Fatalf("WriteFile(linked.txt) error = %v", err)
	}
	link := filepath.Join(root, "linked-dir")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	resp, err := server.ListFiles(context.Background(), &pb.ListFilesRequest{Path: root, Depth: 2})
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	paths := map[string]*pb.FileEntry{}
	for _, entry := range resp.Entries {
		paths[entry.Path] = entry
	}
	if entry := paths[link]; entry == nil || !entry.IsDirectory {
		t.Fatalf("symlink entry = %+v, want directory", entry)
	}
	if paths[filepath.Join(link, "linked.txt")] == nil {
		t.Fatalf("ListFiles() entries = %+v, want file below directory symlink", resp.Entries)
	}
}

func TestListFilesStopsDirectorySymlinkCycle(t *testing.T) {
	server := &AgentServer{}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("data"), FilePerm); err != nil {
		t.Fatalf("WriteFile(file.txt) error = %v", err)
	}
	if err := os.Symlink(root, filepath.Join(root, "cycle")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	resp, err := server.ListFiles(context.Background(), &pb.ListFilesRequest{Path: root, Depth: 10})
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("ListFiles() returned %d entries, want file and cycle link: %+v", len(resp.Entries), resp.Entries)
	}
}

func TestListFilesIncludesBrokenSymlink(t *testing.T) {
	server := &AgentServer{}
	root := t.TempDir()
	link := filepath.Join(root, "missing")
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	resp, err := server.ListFiles(context.Background(), &pb.ListFilesRequest{Path: root, Depth: 1})
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Path != link {
		t.Fatalf("ListFiles() entries = %+v, want broken symlink %s", resp.Entries, link)
	}
}

func TestSearchFilesMatchesAndExcludes(t *testing.T) {
	server := &AgentServer{}
	root := t.TempDir()
	for name, content := range map[string]string{
		"main.py":    "print('ok')",
		"ignore.py":  "print('skip')",
		"notes.txt":  "skip",
		"backup.bak": "skip",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), FilePerm); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}

	resp, err := server.SearchFiles(context.Background(), &pb.SearchFilesRequest{
		Path:            root,
		Pattern:         "*.py",
		ExcludePatterns: []string{"ignore.py"},
	})
	if err != nil {
		t.Fatalf("SearchFiles() error = %v", err)
	}
	if len(resp.Entries) != 1 || resp.Entries[0].Name != "main.py" {
		t.Fatalf("SearchFiles() entries = %+v, want only main.py", resp.Entries)
	}
}

func TestSearchFilesFollowsDirectorySymlink(t *testing.T) {
	server := &AgentServer{}
	root := t.TempDir()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "linked.py"), []byte("print('linked')"), FilePerm); err != nil {
		t.Fatalf("WriteFile(linked.py) error = %v", err)
	}
	link := filepath.Join(root, "packages")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	resp, err := server.SearchFiles(context.Background(), &pb.SearchFilesRequest{Path: root, Pattern: "*.py"})
	if err != nil {
		t.Fatalf("SearchFiles() error = %v", err)
	}
	want := filepath.Join(link, "linked.py")
	if len(resp.Entries) != 1 || resp.Entries[0].Path != want {
		t.Fatalf("SearchFiles() entries = %+v, want %s", resp.Entries, want)
	}
}
