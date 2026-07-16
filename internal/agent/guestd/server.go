package guestd

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"

	"connectrpc.com/connect"
	pb "github.com/openeuler/Conch/api/go_proto"
	agentconnect "github.com/openeuler/Conch/api/go_proto/pbconnect"
	"github.com/openeuler/Conch/pkg/ulog"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

const (
	processStartProcessPath = agentconnect.ProcessServiceStartProcessProcedure
	processConnectPath      = agentconnect.ProcessServiceConnectProcedure
	processListPath         = agentconnect.ProcessServiceListProcedure
	processSendSignalPath   = agentconnect.ProcessServiceSendSignalProcedure
	filePostFileStreamPath  = agentconnect.FileServicePostFileStreamProcedure
	fileGetFileStreamPath   = agentconnect.FileServiceGetFileStreamProcedure
	fileListFilesPath       = agentconnect.FileServiceListFilesProcedure
	fileSearchFilesPath     = agentconnect.FileServiceSearchFilesProcedure
)

func serveAgentAPI(listener net.Listener) error {
	httpServer := &http.Server{
		Handler: h2c.NewHandler(newAgentHTTPHandler(), &http2.Server{}),
	}
	return httpServer.Serve(listener)
}

func newAgentHTTPHandler() http.Handler {
	server := &AgentServer{Version: ServerVersion}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleAgentHealth)

	opts := []connect.HandlerOption{connect.WithRequireConnectProtocolHeader()}
	handler := &agentConnectHandler{server: server}
	processPath, processHandler := agentconnect.NewProcessServiceHandler(handler, opts...)
	filePath, fileHandler := agentconnect.NewFileServiceHandler(handler, opts...)
	mux.Handle(processPath, processHandler)
	mux.Handle(filePath, fileHandler)
	return mux
}

func handleAgentHealth(w http.ResponseWriter, r *http.Request) {
	ulog.Info("Received agent health check request")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"OK","message":"` + HealthMsgOK + `"}`))
}

func verifyConnectAuth(header http.Header) error {
	if err := agentAuth.verifyHTTPHeader(header); err != nil {
		return connect.NewError(connect.CodeUnauthenticated, err)
	}
	return nil
}

func toConnectError(err error) error {
	if err == nil {
		return nil
	}
	if connectErr := new(connect.Error); errors.As(err, &connectErr) {
		return err
	}
	return err
}

type agentConnectHandler struct {
	server *AgentServer
}

func (h *agentConnectHandler) StartProcess(ctx context.Context, req *connect.Request[pb.StartProcessRequest], stream *connect.ServerStream[pb.ProcessEvent]) error {
	if err := verifyConnectAuth(req.Header()); err != nil {
		return err
	}
	return toConnectError(h.server.StartProcess(ctx, req.Msg, &connectProcessStream{ctx: ctx, stream: stream}))
}

func (h *agentConnectHandler) Connect(ctx context.Context, req *connect.Request[pb.ConnectProcessRequest], stream *connect.ServerStream[pb.ProcessEvent]) error {
	if err := verifyConnectAuth(req.Header()); err != nil {
		return err
	}
	return toConnectError(h.server.Connect(req.Msg, &connectProcessStream{ctx: ctx, stream: stream}))
}

func (h *agentConnectHandler) List(ctx context.Context, req *connect.Request[pb.ListProcessesRequest]) (*connect.Response[pb.ListProcessesResponse], error) {
	if err := verifyConnectAuth(req.Header()); err != nil {
		return nil, err
	}
	resp, err := h.server.List(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (h *agentConnectHandler) SendSignal(ctx context.Context, req *connect.Request[pb.SendSignalRequest]) (*connect.Response[pb.SendSignalResponse], error) {
	if err := verifyConnectAuth(req.Header()); err != nil {
		return nil, err
	}
	resp, err := h.server.SendSignal(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (h *agentConnectHandler) PostFileStream(ctx context.Context, stream *connect.ClientStream[pb.FileChunk]) (*connect.Response[pb.PostFilesResponse], error) {
	if err := verifyConnectAuth(stream.RequestHeader()); err != nil {
		return nil, err
	}
	adapter := &connectPostFileStream{stream: stream}
	if err := h.server.PostFileStream(adapter); err != nil {
		return nil, toConnectError(err)
	}
	if adapter.response == nil {
		adapter.response = &pb.PostFilesResponse{}
	}
	return connect.NewResponse(adapter.response), nil
}

func (h *agentConnectHandler) GetFileStream(ctx context.Context, req *connect.Request[pb.GetFileRequest], stream *connect.ServerStream[pb.FileChunk]) error {
	if err := verifyConnectAuth(req.Header()); err != nil {
		return err
	}
	return toConnectError(h.server.GetFileStream(req.Msg, &connectFileStream{stream: stream}))
}

func (h *agentConnectHandler) ListFiles(ctx context.Context, req *connect.Request[pb.ListFilesRequest]) (*connect.Response[pb.ListFilesResponse], error) {
	if err := verifyConnectAuth(req.Header()); err != nil {
		return nil, err
	}
	resp, err := h.server.ListFiles(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

func (h *agentConnectHandler) SearchFiles(ctx context.Context, req *connect.Request[pb.SearchFilesRequest]) (*connect.Response[pb.SearchFilesResponse], error) {
	if err := verifyConnectAuth(req.Header()); err != nil {
		return nil, err
	}
	resp, err := h.server.SearchFiles(ctx, req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(resp), nil
}

type connectProcessStream struct {
	ctx    context.Context
	stream *connect.ServerStream[pb.ProcessEvent]
}

func (s *connectProcessStream) Send(event *pb.ProcessEvent) error {
	return s.stream.Send(event)
}

func (s *connectProcessStream) Context() context.Context {
	return s.ctx
}

type connectPostFileStream struct {
	stream   *connect.ClientStream[pb.FileChunk]
	response *pb.PostFilesResponse
}

func (s *connectPostFileStream) Recv() (*pb.FileChunk, error) {
	if s.stream.Receive() {
		return s.stream.Msg(), nil
	}
	if err := s.stream.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (s *connectPostFileStream) SendAndClose(resp *pb.PostFilesResponse) error {
	s.response = resp
	return nil
}

type connectFileStream struct {
	stream *connect.ServerStream[pb.FileChunk]
}

func (s *connectFileStream) Send(chunk *pb.FileChunk) error {
	return s.stream.Send(chunk)
}
