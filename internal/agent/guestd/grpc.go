// main.go - Agent gRPC service implementation
// Implements core gRPC methods: HealthCheck, ExecuteCommand, StartProcess, PostFiles, GetFile
package guestd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	pb "github.com/openeuler/Conch/api/go_proto"
	"github.com/openeuler/Conch/pkg/ulog"
)

const DirPerm = 0755
const FilePerm = 0644
const HealthMsgOK = "OK"

type AgentServer struct {
	Version string
}

func (s *AgentServer) HealthCheck(ctx context.Context, in *pb.Empty) (*pb.CheckReply, error) {
	ulog.Info("Received health check request")
	return &pb.CheckReply{Message: HealthMsgOK}, nil
}

func (s *AgentServer) ExecuteCommand(ctx context.Context, req *pb.CommandRequest) (*pb.CommandResponse, error) {
	ulog.Info("Received command execution request", ulog.F("command", req.Command), ulog.F("args", fmt.Sprintf("%v", req.Args)))

	cmd := exec.Command(req.Command, req.Args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		ulog.Error("Command execution failed", ulog.F("error", err))
		return &pb.CommandResponse{
			Stdout: err.Error(),
		}, err
	}

	return &pb.CommandResponse{
		Stdout: string(output),
	}, nil
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

// Write script file and return script path (if content exists)
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

	scriptName := "main.py"
	if name, ok := scriptExtMap[cmd]; ok {
		scriptName = name
	}

	scriptPath := filepath.Join(workDir, scriptName)
	if err := os.WriteFile(scriptPath, []byte(content), FilePerm); err != nil {
		return "", err
	}

	return scriptPath, nil
}

// Execute command and return output, stderr and exit code
func (s *AgentServer) executeCmd(ctx context.Context, cmdName string, args []string, workDir string, envMap map[string]string) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, cmdName, args...)
	cmd.Dir = workDir

	env := os.Environ()
	for k, v := range envMap {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", 0, err
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", "", 0, err
	}

	if err := cmd.Start(); err != nil {
		return "", "", 0, err
	}

	outBytes, _ := io.ReadAll(stdoutPipe)
	errBytes, _ := io.ReadAll(stderrPipe)

	if err := cmd.Wait(); err != nil {
		ulog.Error("Process exited with error", ulog.F("error", err))
	}

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	return string(outBytes), string(errBytes), exitCode, nil
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

	// Build command args
	args := req.Args
	if len(args) == 0 && scriptPath != "" {
		args = []string{scriptPath}
	}

	// Execute command
	stdout, stderr, exitCode, err := s.executeCmd(ctx, req.Cmd, args, workDir, req.Env)
	if err != nil {
		errMsg := "failed to execute process: " + err.Error()
		return buildErrorResponse(errMsg), nil
	}

	return &pb.StartProcessResponse{
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: int32(exitCode),
		Error:    "",
	}, nil
}

// Uploads multiple files to specified paths on server
// TODO: Use stream mode for file upload in the future.
func (s *AgentServer) PostFiles(ctx context.Context, req *pb.PostFilesRequest) (*pb.PostFilesResponse, error) {
	if len(req.Files) == 0 {
		errMsg := "no files provided for upload"
		ulog.Warn("PostFiles", ulog.F("message", errMsg))
		return &pb.PostFilesResponse{
			UploadedCount: 0,
			Error:         errMsg,
		}, nil
	}

	var uploadedCount int32
	for _, file := range req.Files {
		cleanedFilepath := filepath.Clean(file.Filepath)

		if cleanedFilepath == "" {
			errMsg := "empty filepath for upload"
			ulog.Error("PostFiles", ulog.F("message", errMsg))
			return &pb.PostFilesResponse{
				UploadedCount: uploadedCount,
				Error:         errMsg,
			}, nil
		}

		// Create parent directories if needed
		targetDir := filepath.Dir(cleanedFilepath)
		if err := os.MkdirAll(targetDir, DirPerm); err != nil {
			errMsg := "failed to create parent directory for " + cleanedFilepath + ": " + err.Error()
			ulog.Error("PostFiles", ulog.F("message", errMsg))
			return &pb.PostFilesResponse{
				UploadedCount: uploadedCount,
				Error:         errMsg,
			}, nil
		}

		// Write file content to target path
		if err := os.WriteFile(cleanedFilepath, file.Content, FilePerm); err != nil {
			errMsg := "failed to write file " + cleanedFilepath + ": " + err.Error()
			ulog.Error("PostFiles", ulog.F("message", errMsg))
			return &pb.PostFilesResponse{
				UploadedCount: uploadedCount,
				Error:         errMsg,
			}, nil
		}

		ulog.Info("Successfully uploaded file",
			ulog.F("file", cleanedFilepath),
			ulog.F("size", len(file.Content)))
		uploadedCount++
	}

	return &pb.PostFilesResponse{
		UploadedCount: uploadedCount,
		Error:         "",
	}, nil
}

// Reads and returns content of a specified file on the server
func (s *AgentServer) GetFile(ctx context.Context, req *pb.GetFileRequest) (*pb.GetFileResponse, error) {
	if req.Filepath == "" {
		errMsg := "filepath is required for file retrieval"
		ulog.Warn("GetFile", ulog.F("message", errMsg))
		return &pb.GetFileResponse{Error: errMsg}, nil
	}

	content, err := os.ReadFile(req.Filepath)
	if err != nil {
		var errMsg string
		if os.IsNotExist(err) {
			errMsg = "file not found: " + req.Filepath
			ulog.Warn("GetFile", ulog.F("message", errMsg))
		} else {
			errMsg = "failed to read file " + req.Filepath + ": " + err.Error()
			ulog.Warn("GetFile", ulog.F("message", errMsg))
		}
		return &pb.GetFileResponse{Error: errMsg}, nil
	}

	ulog.Info("Successfully read file",
		ulog.F("file", req.Filepath),
		ulog.F("size", len(content)))
	return &pb.GetFileResponse{Content: content}, nil
}
