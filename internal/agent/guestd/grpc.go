// main.go - Agent gRPC service implementation
// Implements core gRPC methods: HealthCheck, StartProcess, PostFileStream, GetFileStream.
package guestd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	pb "github.com/openeuler/Conch/api/go_proto"
	"github.com/openeuler/Conch/pkg/ulog"
)

const DirPerm = 0755
const FilePerm = 0644
const HealthMsgOK = "OK"
const agentFileChunkBytes = 1024 * 1024

type AgentServer struct {
	Version string
}

func (s *AgentServer) HealthCheck(ctx context.Context, in *pb.Empty) (*pb.CheckReply, error) {
	ulog.Info("Received health check request")
	return &pb.CheckReply{Message: HealthMsgOK}, nil
}

// buildErrorResponse creates a unified StartProcessResponse with error information
// This function eliminates duplicate error response construction logic
func buildErrorResponse(errMsg string) *pb.StartProcessResponse {
	ulog.Error("Process error", ulog.F("message", errMsg))
	return &pb.StartProcessResponse{
		Stdout:   "",
		Stderr:   "",
		ExitCode: -1,
		Error:    errMsg,
	}
}

func (s *AgentServer) prepareWorkDir(cwd string) (string, error) {
	if cwd == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(homeDir, DirPerm); err != nil {
			return "", err
		}
		return homeDir, nil
	}
	if err := os.MkdirAll(cwd, DirPerm); err != nil {
		return "", err
	}
	return cwd, nil
}

// Write script file and return script path (if content exists).
func (s *AgentServer) writeScript(workDir, cmd, content string) (string, error) {
	if content == "" {
		return "", nil
	}

	scriptExtMap := map[string]string{
		"python":  "main.py",
		"python3": "main.py",
		"python2": "main.py",
		"node":    "main.js",
		"nodejs":  "main.js",
		"bash":    "main.sh",
		"sh":      "main.sh",
		"zsh":     "main.sh",
		"fish":    "main.sh",
		"lua":     "main.lua",
		"ruby":    "main.rb",
		"rb":      "main.rb",
	}

	scriptName := "conch-script-*.py"
	if name, ok := scriptExtMap[cmd]; ok {
		ext := filepath.Ext(name)
		scriptName = "conch-script-*" + ext
	}

	scriptFile, err := os.CreateTemp(workDir, scriptName)
	if err != nil {
		return "", err
	}
	scriptPath := scriptFile.Name()
	if _, err := scriptFile.Write([]byte(content)); err != nil {
		scriptFile.Close()
		return "", err
	}
	if err := scriptFile.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(scriptPath, FilePerm); err != nil {
		return "", err
	}

	return scriptPath, nil
}

// Execute command and return output, stderr and exit code
func (s *AgentServer) executeCmd(ctx context.Context, cmdName string, args []string, workDir string, envMap map[string]string) (string, string, int, error) {
	if cmdName == "" {
		return "", "", -1, fmt.Errorf("command is required")
	}

	cmd := exec.CommandContext(ctx, cmdName, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}

	env := os.Environ()
	for k, v := range envMap {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return "", "", -1, err
	}

	waitErr := cmd.Wait()

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdoutBuf.String(), stderrBuf.String(), exitCode, ctxErr
	}
	if err := signaledProcessError(cmd.ProcessState); err != nil {
		return stdoutBuf.String(), stderrBuf.String(), exitCode, err
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return stdoutBuf.String(), stderrBuf.String(), exitCode, nil
		}
		return stdoutBuf.String(), stderrBuf.String(), exitCode, waitErr
	}

	return stdoutBuf.String(), stderrBuf.String(), exitCode, nil
}

func signaledProcessError(state *os.ProcessState) error {
	if state == nil {
		return nil
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return nil
	}
	return fmt.Errorf("process terminated by signal %s", status.Signal())
}

// Starts a process with custom working dir, environment, and script content
func (s *AgentServer) StartProcess(ctx context.Context, req *pb.StartProcessRequest) (*pb.StartProcessResponse, error) {
	ulog.Info("Received start process request",
		ulog.F("cmd", req.Cmd),
		ulog.F("args", fmt.Sprintf("%v", req.Args)),
		ulog.F("cwd", req.Cwd),
		ulog.F("has_content", req.Content != ""))

	// Prepare work dir
	workDir, err := s.prepareWorkDir(req.Cwd)
	if err != nil {
		errMsg := "failed to prepare working directory: " + err.Error()
		return buildErrorResponse(errMsg), nil
	}

	// Write script file
	scriptPath, err := s.writeScript(workDir, req.Cmd, req.Content)
	if err != nil {
		errMsg := "failed to write script file: " + err.Error()
		return buildErrorResponse(errMsg), nil
	}
	if scriptPath != "" {
		defer func() {
			if err := os.Remove(scriptPath); err != nil && !os.IsNotExist(err) {
				ulog.Warn("failed to remove temporary script", ulog.F("path", scriptPath), ulog.F("error", err))
			}
		}()
	}

	// Build command args
	args := req.Args
	if len(args) == 0 && scriptPath != "" {
		args = []string{scriptPath}
	}

	// Execute command
	stdout, stderr, exitCode, err := s.executeCmd(ctx, req.Cmd, args, workDir, req.Env)
	if err != nil {
		errMsg := "failed to execute process: " + err.Error()
		return &pb.StartProcessResponse{
			Stdout:   stdout,
			Stderr:   stderr,
			ExitCode: int32(exitCode),
			Error:    errMsg,
		}, nil
	}

	return &pb.StartProcessResponse{
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: int32(exitCode),
		Error:    "",
	}, nil
}

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

// PostFileStream uploads one file using client-streaming chunks. The first
// chunk must include filepath; later chunks may omit it.
func (s *AgentServer) PostFileStream(stream pb.AgentService_PostFileStreamServer) error {
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
				return stream.SendAndClose(&pb.PostFilesResponse{Error: "no file chunks provided"})
			}
			if err := file.Close(); err != nil {
				file = nil
				return stream.SendAndClose(&pb.PostFilesResponse{Error: "failed to close uploaded file " + targetPath + ": " + err.Error()})
			}
			file = nil
			if err := os.Rename(tempPath, targetPath); err != nil {
				return stream.SendAndClose(&pb.PostFilesResponse{Error: "failed to commit uploaded file " + targetPath + ": " + err.Error()})
			}
			committed = true
			logger.Info("Successfully uploaded file by stream", ulog.F("file", targetPath), ulog.F("size", totalBytes))
			return stream.SendAndClose(&pb.PostFilesResponse{UploadedCount: 1})
		}
		if err != nil {
			return err
		}
		if len(chunk.Content) > agentFileChunkBytes {
			return stream.SendAndClose(&pb.PostFilesResponse{Error: fmt.Sprintf("file chunk exceeds maximum size %d bytes", agentFileChunkBytes)})
		}

		if file == nil {
			cleanedFilepath, errMsg := cleanAgentFilepath(chunk.Filepath, "stream upload")
			if errMsg != "" {
				return stream.SendAndClose(&pb.PostFilesResponse{Error: errMsg})
			}
			targetPath = cleanedFilepath
			if err := os.MkdirAll(filepath.Dir(targetPath), DirPerm); err != nil {
				return stream.SendAndClose(&pb.PostFilesResponse{Error: "failed to create parent directory for " + targetPath + ": " + err.Error()})
			}
			created, err := os.CreateTemp(filepath.Dir(targetPath), ".conch-upload-*")
			if err != nil {
				return stream.SendAndClose(&pb.PostFilesResponse{Error: "failed to create temporary upload file for " + targetPath + ": " + err.Error()})
			}
			tempPath = created.Name()
			if err := created.Chmod(FilePerm); err != nil {
				created.Close()
				file = nil
				return stream.SendAndClose(&pb.PostFilesResponse{Error: "failed to set temporary upload file permissions for " + targetPath + ": " + err.Error()})
			}
			file = created
		} else if chunk.Filepath != "" && filepath.Clean(chunk.Filepath) != targetPath {
			return stream.SendAndClose(&pb.PostFilesResponse{Error: "filepath changed during stream upload"})
		}

		if len(chunk.Content) == 0 {
			continue
		}
		if _, err := file.Write(chunk.Content); err != nil {
			return stream.SendAndClose(&pb.PostFilesResponse{Error: "failed to write file " + targetPath + ": " + err.Error()})
		}
		totalBytes += int64(len(chunk.Content))
	}
}

// GetFileStream downloads one file using server-streaming chunks.
func (s *AgentServer) GetFileStream(req *pb.GetFileRequest, stream pb.AgentService_GetFileStreamServer) error {
	cleanedFilepath, errMsg := cleanAgentFilepath(req.Filepath, "stream file retrieval")
	if errMsg != "" {
		return fmt.Errorf("%s", errMsg)
	}

	info, err := os.Stat(cleanedFilepath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file not found: %s", cleanedFilepath)
		}
		return fmt.Errorf("failed to stat file %s: %w", cleanedFilepath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("path is a directory: %s", cleanedFilepath)
	}

	file, err := os.Open(cleanedFilepath)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", cleanedFilepath, err)
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
			return readErr
		}
	}
}
