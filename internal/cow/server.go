package cow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/openeuler/Conch/internal/memsnap"
	"golang.org/x/sys/unix"
)

type attachment struct {
	token     string
	sandboxID string
	pinned    *memsnap.PinnedManifest
	memfd     *os.File

	handoff    *uffdHandoff
	uffd       *os.File
	workerStop chan struct{}
	workerDone chan struct{}
}

type uffdHandoff struct {
	socketPath string
	listener   *net.UnixListener
	done       chan struct{}
	err        error
}

type Server struct {
	socketPath string

	mu                sync.Mutex
	attachments       map[string]*attachment
	listener          *net.UnixListener
	closed            bool
	connections       sync.WaitGroup
	activeConnections map[*net.UnixConn]struct{}
	uffdSequence      uint64
	ready             chan struct{}
}

func NewServer(socketPath string) *Server {
	return newServer(socketPath)
}

func newServer(socketPath string) *Server {
	return &Server{
		socketPath:        socketPath,
		attachments:       make(map[string]*attachment),
		activeConnections: make(map[*net.UnixConn]struct{}),
		ready:             make(chan struct{}),
	}
}

func (server *Server) Ready() <-chan struct{} {
	return server.ready
}

func baseResponse(request requestEnvelope) responseEnvelope {
	return responseEnvelope{ProtocolVersion: ProtocolVersion, RequestID: request.RequestID}
}

func (server *Server) Serve(ctx context.Context) error {
	if server.socketPath == "" {
		return fmt.Errorf("socket path is required")
	}
	if err := os.MkdirAll(filepath.Dir(server.socketPath), 0o755); err != nil {
		return fmt.Errorf("create cow socket directory: %w", err)
	}
	if err := prepareControlSocketPath(server.socketPath); err != nil {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: server.socketPath, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen cow socket: %w", err)
	}
	listener.SetUnlinkOnClose(true)
	defer listener.Close()
	if err := os.Chmod(server.socketPath, 0o600); err != nil {
		return fmt.Errorf("chmod cow socket: %w", err)
	}
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		return fmt.Errorf("server is closed")
	}
	server.listener = listener
	server.mu.Unlock()
	close(server.ready)

	stopClose := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stopClose()
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || server.isClosed() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept cow connection: %w", err)
		}
		server.mu.Lock()
		if server.closed {
			server.mu.Unlock()
			_ = conn.Close()
			continue
		}
		server.activeConnections[conn] = struct{}{}
		server.connections.Go(func() {
			defer func() {
				server.mu.Lock()
				delete(server.activeConnections, conn)
				server.mu.Unlock()
			}()
			server.serveConnection(conn)
		})
		server.mu.Unlock()
	}
}

func (server *Server) serveConnection(conn *net.UnixConn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(requestTimeout))
	var request requestEnvelope
	received, err := readFrame(conn, &request)
	if err != nil {
		return
	}
	response := baseResponse(request)
	if err := validateRequestEnvelope(request, received); err != nil {
		closeFDs(received)
		response.Error = err.Error()
		_ = writeFrame(conn, response, nil)
		return
	}
	if request.Type == RequestWaitAttachmentReady {
		_ = conn.SetDeadline(time.Now().Add(defaultUFFDAcceptTimeout + waitReadyTimeoutMargin))
	}
	closeFDs(received)

	var result any
	var sendFDs []int
	var attachmentToken string
	switch request.Type {
	case RequestPing:
		var params PingRequest
		err = decodeStrictJSON(request.Params, &params)
		result = PingResponse{}
	case RequestAttach:
		var params AttachRequest
		if err = decodeStrictJSON(request.Params, &params); err == nil {
			var attachResult AttachResponse
			attachResult, sendFDs, err = server.handleAttach(params)
			attachmentToken = attachResult.Token
			result = attachResult
		}
	case RequestWaitAttachmentReady:
		var params WaitAttachmentReadyRequest
		if err = decodeStrictJSON(request.Params, &params); err == nil {
			result, err = server.handleWaitAttachmentReady(params)
		}
	case RequestDetach:
		var params DetachRequest
		if err = decodeStrictJSON(request.Params, &params); err == nil {
			result, err = server.handleDetach(params)
		}
	}
	if err != nil {
		response.Error = fmt.Sprintf("%s: %v", request.Type, err)
	} else {
		response.Result, err = json.Marshal(result)
		if err != nil {
			response.Error = fmt.Sprintf("encode %s result: %v", request.Type, err)
		} else {
			response.OK = true
		}
	}
	defer closeFDs(sendFDs)
	if !response.OK && attachmentToken != "" {
		server.releaseAttachment(attachmentToken)
		attachmentToken = ""
	}
	if err := writeFrame(conn, response, sendFDs); err != nil && attachmentToken != "" {
		server.releaseAttachment(attachmentToken)
	}
}

func (server *Server) handleAttach(request AttachRequest) (AttachResponse, []int, error) {
	if request.SandboxID == "" || request.MemorySnapshotRoot == "" {
		return AttachResponse{}, nil, fmt.Errorf("sandbox ID and memory snapshot root are required")
	}
	pinned, err := memsnap.LoadAndPin(request.MemorySnapshotRoot)
	if err != nil {
		return AttachResponse{}, nil, fmt.Errorf("load and pin manifest: %w", err)
	}
	token := uuid.NewString()
	fd, err := unix.MemfdCreate("conch-memory-"+token, unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		_ = pinned.Close()
		return AttachResponse{}, nil, fmt.Errorf("create memory memfd: %w", err)
	}
	owner := os.NewFile(uintptr(fd), "conch-memory-owner")
	if owner == nil {
		_ = unix.Close(fd)
		_ = pinned.Close()
		return AttachResponse{}, nil, fmt.Errorf("wrap memory memfd")
	}
	cleanup := func() {
		_ = owner.Close()
		_ = pinned.Close()
	}
	if pinned.Manifest.MemorySize > uint64(^uint64(0)>>1) {
		cleanup()
		return AttachResponse{}, nil, fmt.Errorf("memory size exceeds supported file size")
	}
	if err := unix.Ftruncate(fd, int64(pinned.Manifest.MemorySize)); err != nil {
		cleanup()
		return AttachResponse{}, nil, fmt.Errorf("size memory memfd: %w", err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_GROW|unix.F_SEAL_SHRINK); err != nil {
		cleanup()
		return AttachResponse{}, nil, fmt.Errorf("seal memory memfd: %w", err)
	}
	duplicate, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		cleanup()
		return AttachResponse{}, nil, fmt.Errorf("duplicate memory memfd: %w", err)
	}
	item := &attachment{token: token, sandboxID: request.SandboxID, pinned: pinned, memfd: owner}
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		_ = unix.Close(duplicate)
		cleanup()
		return AttachResponse{}, nil, fmt.Errorf("server is closed")
	}
	server.attachments[token] = item
	server.mu.Unlock()
	uffdSocketPath, err := server.prepareUFFDHandoff(item)
	if err != nil {
		_ = unix.Close(duplicate)
		server.releaseAttachment(token)
		return AttachResponse{}, nil, err
	}
	return AttachResponse{
		Token:          token,
		UFFDSocketPath: uffdSocketPath,
		MemorySize:     pinned.Manifest.MemorySize,
		BlockSize:      pinned.Manifest.BlockSize,
	}, []int{duplicate}, nil
}

func (server *Server) handleDetach(request DetachRequest) (DetachResponse, error) {
	if request.Token == "" {
		return DetachResponse{}, fmt.Errorf("token is required")
	}
	server.releaseAttachment(request.Token)
	return DetachResponse{}, nil
}

func (server *Server) releaseAttachment(token string) bool {
	server.mu.Lock()
	item := server.attachments[token]
	if item != nil {
		delete(server.attachments, token)
	}
	server.mu.Unlock()
	if item == nil {
		return false
	}
	_ = server.closeAttachment(item)
	return true
}

func (server *Server) handleWaitAttachmentReady(request WaitAttachmentReadyRequest) (WaitAttachmentReadyResponse, error) {
	if request.Token == "" || request.SandboxID == "" {
		return WaitAttachmentReadyResponse{}, fmt.Errorf("token and sandbox ID are required")
	}
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		return WaitAttachmentReadyResponse{}, fmt.Errorf("server is closed")
	}
	item := server.attachments[request.Token]
	if item == nil || item.handoff == nil || item.sandboxID != request.SandboxID {
		server.mu.Unlock()
		return WaitAttachmentReadyResponse{}, fmt.Errorf("UFFD handoff is not prepared for sandbox")
	}
	handoff := item.handoff
	server.mu.Unlock()
	timer := time.NewTimer(defaultUFFDAcceptTimeout + time.Second)
	defer timer.Stop()
	select {
	case <-handoff.done:
	case <-timer.C:
		return WaitAttachmentReadyResponse{}, fmt.Errorf("timed out waiting for UFFD handoff")
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.closed || server.attachments[request.Token] != item || item.handoff != handoff {
		return WaitAttachmentReadyResponse{}, fmt.Errorf("attachment changed while waiting for UFFD")
	}
	if handoff.err != nil {
		return WaitAttachmentReadyResponse{}, fmt.Errorf("UFFD handoff failed: %w", handoff.err)
	}
	if item.uffd == nil {
		return WaitAttachmentReadyResponse{}, fmt.Errorf("UFFD handoff did not provide a fault descriptor")
	}
	return WaitAttachmentReadyResponse{}, nil
}

func (server *Server) closeAttachment(item *attachment) error {
	if item == nil {
		return nil
	}
	var result error
	server.mu.Lock()
	handoff := item.handoff
	server.mu.Unlock()
	if handoff != nil {
		_ = handoff.listener.Close()
		if err := os.Remove(handoff.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
		<-handoff.done
	}
	server.mu.Lock()
	workerStop := item.workerStop
	workerDone := item.workerDone
	uffd := item.uffd
	server.mu.Unlock()
	if workerStop != nil {
		close(workerStop)
	}
	if workerDone != nil {
		<-workerDone
	} else if uffd != nil {
		result = errors.Join(result, uffd.Close())
	}
	if item.memfd != nil {
		result = errors.Join(result, item.memfd.Close())
	}
	if item.pinned != nil {
		result = errors.Join(result, item.pinned.Close())
	}
	return result
}

func (server *Server) Close() error {
	server.mu.Lock()
	firstClose := !server.closed
	server.closed = true
	listener := server.listener
	var handoffListeners []*net.UnixListener
	var activeConnections []*net.UnixConn
	for _, item := range server.attachments {
		if item.handoff != nil {
			handoffListeners = append(handoffListeners, item.handoff.listener)
		}
	}
	for conn := range server.activeConnections {
		activeConnections = append(activeConnections, conn)
	}
	server.mu.Unlock()
	if firstClose && listener != nil {
		_ = listener.Close()
	}
	for _, handoffListener := range handoffListeners {
		_ = handoffListener.Close()
	}
	for _, conn := range activeConnections {
		_ = conn.Close()
	}
	server.connections.Wait()
	server.mu.Lock()
	attachments := server.attachments
	server.attachments = make(map[string]*attachment)
	server.mu.Unlock()
	var result error
	for _, item := range attachments {
		result = errors.Join(result, server.closeAttachment(item))
	}
	return result
}

func prepareControlSocketPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect control socket path %q: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("control socket path %q already exists and is not a Unix socket", path)
	}
	connection, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return fmt.Errorf("control socket path %q already exists", path)
	}
	if !errors.Is(dialErr, unix.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
		return fmt.Errorf("control socket path %q already exists: %w", path, dialErr)
	}
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("remove stale control socket %q: %w", path, err)
	}
	return nil
}

func (server *Server) isClosed() bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.closed
}
