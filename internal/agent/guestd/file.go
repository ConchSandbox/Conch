package guestd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	pb "github.com/openeuler/Conch/api/go_proto"
	"github.com/openeuler/Conch/pkg/ulog"
)

func cleanAgentFilepath(path, operation string) (string, string) {
	if path == "" {
		return "", "filepath is required for " + operation
	}
	cleaned := filepath.Clean(path)
	if cleaned == "." {
		return "", "invalid filepath for " + operation
	}
	return cleaned, ""
}

func fileAccessCode(err error) connect.Code {
	if os.IsPermission(err) {
		return connect.CodePermissionDenied
	}
	return connect.CodeInternal
}

// PostFileStream uploads one file using client-streaming chunks. The first
// chunk must include filepath; later chunks may omit it.
func (s *AgentServer) PostFileStream(stream postFileStream) error {
	logger := ulog.GetLogger()
	var (
		targetPath string
		tempPath   string
		file       *os.File
		totalBytes int64
		committed  bool
	)

	cleanup := func() {
		if file != nil {
			file.Close()
			file = nil
		}
		if tempPath != "" && !committed {
			if err := os.Remove(tempPath); err != nil && !os.IsNotExist(err) {
				logger.Warn("failed to remove temporary upload file", ulog.F("path", tempPath), ulog.F("error", err))
			}
		}
	}
	defer cleanup()

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if file == nil {
				return connectError(connect.CodeInvalidArgument, "no file chunks provided")
			}
			if err := file.Close(); err != nil {
				file = nil
				return connectErrorf(connect.CodeInternal, "failed to close uploaded file %s: %v", targetPath, err)
			}
			file = nil
			if err := os.Rename(tempPath, targetPath); err != nil {
				if os.IsPermission(err) {
					return connectErrorf(connect.CodePermissionDenied, "permission denied committing uploaded file %s: %v", targetPath, err)
				}
				return connectErrorf(connect.CodeInternal, "failed to commit uploaded file %s: %v", targetPath, err)
			}
			committed = true
			logger.Info("Successfully uploaded file by stream", ulog.F("file", targetPath), ulog.F("size", totalBytes))
			return stream.SendAndClose(&pb.PostFilesResponse{
				UploadedCount: 1,
				Entries:       []*pb.WriteInfo{writeInfoFromPath(targetPath)},
			})
		}
		if err != nil {
			return err
		}
		if len(chunk.Content) > agentFileChunkBytes {
			return connectErrorf(connect.CodeResourceExhausted, "file chunk exceeds maximum size %d bytes", agentFileChunkBytes)
		}

		if file == nil {
			cleanedFilepath, errMsg := cleanAgentFilepath(chunk.Filepath, "stream upload")
			if errMsg != "" {
				return connectError(connect.CodeInvalidArgument, errMsg)
			}
			targetPath = cleanedFilepath
			if err := os.MkdirAll(filepath.Dir(targetPath), DirPerm); err != nil {
				if os.IsPermission(err) {
					return connectErrorf(connect.CodePermissionDenied, "permission denied creating parent directory for %s: %v", targetPath, err)
				}
				return connectErrorf(connect.CodeInternal, "failed to create parent directory for %s: %v", targetPath, err)
			}
			created, err := os.CreateTemp(filepath.Dir(targetPath), ".conch-upload-*")
			if err != nil {
				if os.IsPermission(err) {
					return connectErrorf(connect.CodePermissionDenied, "permission denied creating temporary upload file for %s: %v", targetPath, err)
				}
				return connectErrorf(connect.CodeInternal, "failed to create temporary upload file for %s: %v", targetPath, err)
			}
			tempPath = created.Name()
			if err := created.Chmod(FilePerm); err != nil {
				created.Close()
				file = nil
				if os.IsPermission(err) {
					return connectErrorf(connect.CodePermissionDenied, "permission denied setting temporary upload file permissions for %s: %v", targetPath, err)
				}
				return connectErrorf(connect.CodeInternal, "failed to set temporary upload file permissions for %s: %v", targetPath, err)
			}
			file = created
		} else if chunk.Filepath != "" && filepath.Clean(chunk.Filepath) != targetPath {
			return connectError(connect.CodeInvalidArgument, "filepath changed during stream upload")
		}

		if len(chunk.Content) == 0 {
			continue
		}
		if _, err := file.Write(chunk.Content); err != nil {
			if os.IsPermission(err) {
				return connectErrorf(connect.CodePermissionDenied, "permission denied writing file %s: %v", targetPath, err)
			}
			return connectErrorf(connect.CodeInternal, "failed to write file %s: %v", targetPath, err)
		}
		totalBytes += int64(len(chunk.Content))
	}
}

func writeInfoFromPath(path string) *pb.WriteInfo {
	return &pb.WriteInfo{
		Name: filepath.Base(path),
		Path: path,
		Type: "file",
	}
}

// GetFileStream downloads one file using server-streaming chunks.
func (s *AgentServer) GetFileStream(req *pb.GetFileRequest, stream getFileStream) error {
	if req == nil {
		return connectError(connect.CodeInvalidArgument, "get file request is required")
	}
	cleanedFilepath, errMsg := cleanAgentFilepath(req.Filepath, "stream file retrieval")
	if errMsg != "" {
		return connectError(connect.CodeInvalidArgument, errMsg)
	}

	info, err := os.Stat(cleanedFilepath)
	if err != nil {
		if os.IsNotExist(err) {
			return connectErrorf(connect.CodeNotFound, "file not found: %s", cleanedFilepath)
		}
		if os.IsPermission(err) {
			return connectErrorf(connect.CodePermissionDenied, "permission denied stat file %s: %v", cleanedFilepath, err)
		}
		return connectErrorf(connect.CodeInternal, "failed to stat file %s: %v", cleanedFilepath, err)
	}
	if info.IsDir() {
		return connectErrorf(connect.CodeInvalidArgument, "path is a directory: %s", cleanedFilepath)
	}

	file, err := os.Open(cleanedFilepath)
	if err != nil {
		if os.IsPermission(err) {
			return connectErrorf(connect.CodePermissionDenied, "permission denied open file %s: %v", cleanedFilepath, err)
		}
		return connectErrorf(connect.CodeInternal, "failed to open file %s: %v", cleanedFilepath, err)
	}
	defer file.Close()

	buf := make([]byte, agentFileChunkBytes)
	first := true
	for {
		n, readErr := file.Read(buf)
		if n > 0 {
			chunk := &pb.FileChunk{Content: append([]byte(nil), buf[:n]...)}
			if first {
				chunk.Filepath = cleanedFilepath
				first = false
			}
			if err := stream.Send(chunk); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			ulog.Info("Successfully streamed file", ulog.F("file", cleanedFilepath), ulog.F("size", info.Size()))
			return nil
		}
		if readErr != nil {
			return connectErrorf(connect.CodeInternal, "failed to read file %s: %v", cleanedFilepath, readErr)
		}
	}
}

func (s *AgentServer) ListFiles(ctx context.Context, req *pb.ListFilesRequest) (*pb.ListFilesResponse, error) {
	requestPath := ""
	requestDepth := int32(0)
	if req != nil {
		requestPath = req.Path
		requestDepth = req.Depth
	}
	ulog.Info("Received list files request",
		ulog.F("path", requestPath),
		ulog.F("depth", requestDepth))

	if req == nil || req.Path == "" {
		return nil, connectError(connect.CodeInvalidArgument, "path is required")
	}
	root, errMsg := cleanAgentFilepath(req.Path, "list files")
	if errMsg != "" {
		return nil, connectError(connect.CodeInvalidArgument, errMsg)
	}
	depth := int(req.Depth)
	if depth <= 0 {
		depth = 1
	}

	rootInfo, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, connectErrorf(connect.CodeNotFound, "path not found: %s", root)
		}
		return nil, connectErrorf(fileAccessCode(err), "failed to stat path %s: %v", root, err)
	}
	if !rootInfo.IsDir() {
		resp := &pb.ListFilesResponse{Entries: []*pb.FileEntry{fileEntryFromInfo(root, rootInfo)}}
		ulog.Info("Listed files",
			ulog.F("path", root),
			ulog.F("depth", depth),
			ulog.F("count", len(resp.Entries)))
		return resp, nil
	}

	resp := &pb.ListFilesResponse{}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		currentDepth := pathDepth(rel)
		if currentDepth > depth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		resp.Entries = append(resp.Entries, fileEntryFromInfo(path, info))
		return nil
	})
	if err != nil {
		return nil, connectErrorf(fileAccessCode(err), "failed to list files under %s: %v", root, err)
	}
	ulog.Info("Listed files",
		ulog.F("path", root),
		ulog.F("depth", depth),
		ulog.F("count", len(resp.Entries)))
	return resp, nil
}

func (s *AgentServer) SearchFiles(ctx context.Context, req *pb.SearchFilesRequest) (*pb.SearchFilesResponse, error) {
	requestPath := ""
	requestPattern := ""
	var requestExcludes []string
	if req != nil {
		requestPath = req.Path
		requestPattern = req.Pattern
		requestExcludes = req.ExcludePatterns
	}
	ulog.Info("Received search files request",
		ulog.F("path", requestPath),
		ulog.F("pattern", requestPattern),
		ulog.F("exclude_patterns", fmt.Sprintf("%v", requestExcludes)))

	if req == nil || req.Path == "" {
		return nil, connectError(connect.CodeInvalidArgument, "path is required")
	}
	if req.Pattern == "" {
		return nil, connectError(connect.CodeInvalidArgument, "pattern is required")
	}
	root, errMsg := cleanAgentFilepath(req.Path, "search files")
	if errMsg != "" {
		return nil, connectError(connect.CodeInvalidArgument, errMsg)
	}
	if _, err := filepath.Match(req.Pattern, ""); err != nil {
		return nil, connectErrorf(connect.CodeInvalidArgument, "invalid search pattern %q: %v", req.Pattern, err)
	}
	for _, pattern := range req.ExcludePatterns {
		if _, err := filepath.Match(pattern, ""); err != nil {
			return nil, connectErrorf(connect.CodeInvalidArgument, "invalid exclude pattern %q: %v", pattern, err)
		}
	}

	resp := &pb.SearchFilesResponse{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !globMatches(req.Pattern, rel, d.Name()) || excludedByGlob(req.ExcludePatterns, rel, d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		resp.Entries = append(resp.Entries, fileEntryFromInfo(path, info))
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, connectErrorf(connect.CodeNotFound, "path not found: %s", root)
		}
		return nil, connectErrorf(fileAccessCode(err), "failed to search files under %s: %v", root, err)
	}
	ulog.Info("Searched files",
		ulog.F("path", root),
		ulog.F("pattern", req.Pattern),
		ulog.F("count", len(resp.Entries)))
	return resp, nil
}

func pathDepth(rel string) int {
	if rel == "." || rel == "" {
		return 0
	}
	depth := 1
	for _, r := range rel {
		if r == os.PathSeparator {
			depth++
		}
	}
	return depth
}

func fileEntryFromInfo(path string, info os.FileInfo) *pb.FileEntry {
	entryType := "file"
	if info.IsDir() {
		entryType = "directory"
	}
	return &pb.FileEntry{
		Name:         info.Name(),
		Path:         path,
		Type:         entryType,
		Size:         info.Size(),
		Permissions:  info.Mode().String(),
		ModifiedTime: info.ModTime().UTC().Format(time.RFC3339),
		Metadata:     map[string]string{},
		IsDirectory:  info.IsDir(),
	}
}

func globMatches(pattern, rel, name string) bool {
	if ok, _ := filepath.Match(pattern, rel); ok {
		return true
	}
	ok, _ := filepath.Match(pattern, name)
	return ok
}

func excludedByGlob(patterns []string, rel, name string) bool {
	for _, pattern := range patterns {
		if globMatches(pattern, rel, name) {
			return true
		}
	}
	return false
}
