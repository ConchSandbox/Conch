package guestd

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"connectrpc.com/connect"
	pb "github.com/openeuler/Conch/api/go_proto"
)

const DirPerm = 0755
const FilePerm = 0644
const HealthMsgOK = "OK"
const agentFileChunkBytes = 1024 * 1024

type AgentServer struct {
	Version      string
	processMu    sync.Mutex
	processes    map[int32]*managedProcess
	processByTag map[string]int32
}

type processConnectStream interface {
	Send(*pb.ProcessEvent) error
	Context() context.Context
}

type postFileStream interface {
	Recv() (*pb.FileChunk, error)
	SendAndClose(*pb.PostFilesResponse) error
}

type getFileStream interface {
	Send(*pb.FileChunk) error
}

func connectError(code connect.Code, msg string) error {
	return connect.NewError(code, errors.New(msg))
}

func connectErrorf(code connect.Code, format string, args ...any) error {
	return connect.NewError(code, fmt.Errorf(format, args...))
}
