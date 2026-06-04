package guestd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/openeuler/Conch/api/go_proto"
	"google.golang.org/grpc"
)

func TestStartProcessReturnsExitCodeForNonZeroExit(t *testing.T) {
	server := &AgentServer{}
	resp, err := server.StartProcess(context.Background(), &pb.StartProcessRequest{
		Cmd:  "sh",
		Args: []string{"-c", "echo out; echo err >&2; exit 7"},
		Cwd:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("StartProcess() error = %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("StartProcess() response error = %q, want empty", resp.Error)
	}
	if resp.ExitCode != 7 {
		t.Fatalf("StartProcess() exit code = %d, want 7", resp.ExitCode)
	}
	if !strings.Contains(resp.Stdout, "out") {
		t.Fatalf("StartProcess() stdout = %q, want out", resp.Stdout)
	}
	if !strings.Contains(resp.Stderr, "err") {
		t.Fatalf("StartProcess() stderr = %q, want err", resp.Stderr)
	}
}

func TestStartProcessRemovesTemporaryScript(t *testing.T) {
	server := &AgentServer{}
	workDir := t.TempDir()

	resp, err := server.StartProcess(context.Background(), &pb.StartProcessRequest{
		Cmd:     "sh",
		Cwd:     workDir,
		Content: "echo script-ran",
	})
	if err != nil {
		t.Fatalf("StartProcess() error = %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("StartProcess() response error = %q, want empty", resp.Error)
	}
	if !strings.Contains(resp.Stdout, "script-ran") {
		t.Fatalf("StartProcess() stdout = %q, want script-ran", resp.Stdout)
	}

	matches, err := filepath.Glob(filepath.Join(workDir, "conch-script-*"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary scripts remain after StartProcess: %v", matches)
	}
}

func TestStartProcessRunsExistingFileFromArgs(t *testing.T) {
	server := &AgentServer{}
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "main.sh"), []byte("echo uploaded-file-ran"), FilePerm); err != nil {
		t.Fatalf("WriteFile(main.sh) error = %v", err)
	}

	resp, err := server.StartProcess(context.Background(), &pb.StartProcessRequest{
		Cmd:  "sh",
		Cwd:  workDir,
		Args: []string{"main.sh"},
	})
	if err != nil {
		t.Fatalf("StartProcess() error = %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("StartProcess() response error = %q, want empty", resp.Error)
	}
	if !strings.Contains(resp.Stdout, "uploaded-file-ran") {
		t.Fatalf("StartProcess() stdout = %q, want uploaded-file-ran", resp.Stdout)
	}
}

type fakePostFileStream struct {
	grpc.ServerStream
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
	grpc.ServerStream
	chunks []*pb.FileChunk
}

func (s *fakeGetFileStream) Send(chunk *pb.FileChunk) error {
	s.chunks = append(s.chunks, chunk)
	return nil
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

	if err := server.PostFileStream(stream); err != nil {
		t.Fatalf("PostFileStream() error = %v", err)
	}
	if stream.response == nil || stream.response.Error == "" {
		t.Fatal("PostFileStream() response error is empty, want oversized chunk error")
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

	if err := server.PostFileStream(stream); err != nil {
		t.Fatalf("PostFileStream() error = %v", err)
	}
	if stream.response == nil || stream.response.Error == "" {
		t.Fatal("PostFileStream() response error is empty, want filepath change error")
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
