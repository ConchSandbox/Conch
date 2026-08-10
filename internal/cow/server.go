package cow

import (
	"context"
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

	uffdSocketPath  string
	uffdListener    *net.UnixListener
	uffd            *os.File
	uffdRanges      []uffdRange
	handoffPrepared bool
	handoffDone     chan struct{}
	handoffErr      error
	workerStop      chan struct{}
	workerDone      chan struct{}
	closeOnce       sync.Once
	closeErr        error
}

type Server struct {
	socketPath   string
	capabilities Capabilities

	mu                    sync.Mutex
	attachments           map[string]*attachment
	listener              *net.UnixListener
	closed                bool
	connections           sync.WaitGroup
	activeConnections     map[*net.UnixConn]struct{}
	uffdOps               uffdOperations
	uffdSequence          uint64
	controlSocketIdentity unixSocketIdentity
	controlSocketOwned    bool
	ready                 chan struct{}
	readyOnce             sync.Once
}

type unixSocketIdentity struct {
	device uint64
	inode  uint64
}

func NewServer(socketPath string) *Server {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	return newServer(socketPath, ProbeCapabilities())
}

func newServer(socketPath string, capabilities Capabilities) *Server {
	return &Server{
		socketPath:        socketPath,
		capabilities:      capabilities,
		attachments:       make(map[string]*attachment),
		activeConnections: make(map[*net.UnixConn]struct{}),
		uffdOps:           productionUFFDOperations(),
		ready:             make(chan struct{}),
	}
}

func (server *Server) Ready() <-chan struct{} {
	return server.ready
}

func baseResponse(request Request) Response {
	return Response{ProtocolVersion: ProtocolVersion, RequestID: request.RequestID}
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
	listener.SetUnlinkOnClose(false)
	identity, err := controlSocketIdentity(server.socketPath)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("identify cow socket: %w", err)
	}
	if err := os.Chmod(server.socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = removeControlSocketIfOwned(server.socketPath, identity)
		return fmt.Errorf("chmod cow socket: %w", err)
	}
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		_ = listener.Close()
		_ = removeControlSocketIfOwned(server.socketPath, identity)
		return fmt.Errorf("server is closed")
	}
	server.listener = listener
	server.controlSocketIdentity = identity
	server.controlSocketOwned = true
	server.mu.Unlock()
	server.readyOnce.Do(func() { close(server.ready) })

	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stop:
		}
	}()
	defer close(stop)
	defer server.removeOwnedControlSocket()
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
		server.connections.Add(1)
		server.activeConnections[conn] = struct{}{}
		server.mu.Unlock()
		go func(conn *net.UnixConn) {
			defer server.connections.Done()
			defer func() {
				server.mu.Lock()
				delete(server.activeConnections, conn)
				server.mu.Unlock()
			}()
			server.serveConnection(conn)
		}(conn)
	}
}

func (server *Server) serveConnection(conn *net.UnixConn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(requestTimeout))
	var request Request
	received, err := readFrame(conn, &request)
	if err != nil {
		return
	}
	response := baseResponse(request)
	if err := validateRequest(request, received); err != nil {
		closeFDs(received)
		response.Error = err.Error()
		_ = writeFrame(conn, response, nil)
		return
	}
	if request.Type == RequestWaitAttachmentReady {
		_ = conn.SetDeadline(time.Now().Add(server.uffdOps.acceptTimeout + waitReadyTimeoutMargin))
	}
	var sendFDs []int
	switch request.Type {
	case RequestCapabilities:
		response.OK = true
		capabilities := server.capabilities
		capabilities.MissingFeatures = append([]string(nil), capabilities.MissingFeatures...)
		response.Capabilities = &capabilities
	case RequestAttach:
		response, sendFDs = server.handleAttach(request, response)
	case RequestWaitAttachmentReady:
		response = server.handleWaitAttachmentReady(request, response)
	case RequestDetach:
		response = server.handleDetach(request, response)
	}
	closeFDs(received)
	defer closeFDs(sendFDs)
	if err := writeFrame(conn, response, sendFDs); err != nil && request.Type == RequestAttach && response.OK {
		server.releaseAttachment(response.Token)
	}
}

func (server *Server) handleAttach(request Request, response Response) (Response, []int) {
	if request.SandboxID == "" || request.MemorySnapshotRoot == "" {
		response.Error = "sandbox ID and memory snapshot root are required"
		return response, nil
	}
	if server.capabilities.IncrementalMemory != CapabilitySupported {
		response.Error = fmt.Sprintf("incremental memory is %s", server.capabilities.IncrementalMemory)
		return response, nil
	}
	pinned, err := memsnap.LoadAndPin(request.MemorySnapshotRoot)
	if err != nil {
		response.Error = fmt.Sprintf("load and pin manifest: %v", err)
		return response, nil
	}
	token := uuid.NewString()
	fd, err := unix.MemfdCreate("conch-memory-"+token, unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		_ = pinned.Close()
		response.Error = fmt.Sprintf("create memory memfd: %v", err)
		return response, nil
	}
	owner := os.NewFile(uintptr(fd), "conch-memory-owner")
	if owner == nil {
		_ = unix.Close(fd)
		_ = pinned.Close()
		response.Error = "wrap memory memfd"
		return response, nil
	}
	cleanup := func() {
		_ = owner.Close()
		_ = pinned.Close()
	}
	if pinned.Manifest.MemorySize > uint64(^uint64(0)>>1) {
		cleanup()
		response.Error = "memory size exceeds supported file size"
		return response, nil
	}
	if err := unix.Ftruncate(fd, int64(pinned.Manifest.MemorySize)); err != nil {
		cleanup()
		response.Error = fmt.Sprintf("size memory memfd: %v", err)
		return response, nil
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_GROW|unix.F_SEAL_SHRINK); err != nil {
		cleanup()
		response.Error = fmt.Sprintf("seal memory memfd: %v", err)
		return response, nil
	}
	duplicate, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		cleanup()
		response.Error = fmt.Sprintf("duplicate memory memfd: %v", err)
		return response, nil
	}
	item := &attachment{token: token, sandboxID: request.SandboxID, pinned: pinned, memfd: owner}
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		_ = unix.Close(duplicate)
		cleanup()
		response.Error = "server is closed"
		return response, nil
	}
	server.attachments[token] = item
	server.mu.Unlock()
	prepared := server.prepareUFFDHandoff(item, response)
	if !prepared.OK {
		_ = unix.Close(duplicate)
		server.releaseAttachment(token)
		return prepared, nil
	}
	prepared.Token = token
	prepared.MemorySize = pinned.Manifest.MemorySize
	prepared.BlockSize = pinned.Manifest.BlockSize
	return prepared, []int{duplicate}
}

func (server *Server) handleDetach(request Request, response Response) Response {
	server.releaseAttachment(request.Token)
	response.OK = true
	return response
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

func (server *Server) handleWaitAttachmentReady(request Request, response Response) Response {
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		response.Error = "server is closed"
		return response
	}
	item := server.attachments[request.Token]
	if item == nil || !item.handoffPrepared || item.sandboxID != request.SandboxID {
		server.mu.Unlock()
		response.Error = "UFFD handoff is not prepared for sandbox"
		return response
	}
	handoffDone := item.handoffDone
	server.mu.Unlock()
	if handoffDone != nil {
		timer := time.NewTimer(server.uffdOps.acceptTimeout + time.Second)
		defer timer.Stop()
		select {
		case <-handoffDone:
		case <-timer.C:
			response.Error = "timed out waiting for UFFD handoff"
			return response
		}
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.closed || server.attachments[request.Token] != item {
		response.Error = "attachment changed while waiting for UFFD"
		return response
	}
	if item.handoffErr != nil {
		response.Error = fmt.Sprintf("UFFD handoff failed: %v", item.handoffErr)
		return response
	}
	if item.uffd == nil {
		response.Error = "UFFD handoff did not provide a fault descriptor"
		return response
	}
	response.OK = true
	return response
}

func (server *Server) closeAttachment(item *attachment) error {
	if item == nil {
		return nil
	}
	item.closeOnce.Do(func() {
		var result error
		server.mu.Lock()
		listener := item.uffdListener
		handoffDone := item.handoffDone
		socketPath := item.uffdSocketPath
		server.mu.Unlock()
		if listener != nil {
			_ = listener.Close()
		}
		if socketPath != "" {
			if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				result = errors.Join(result, err)
			}
		}
		if handoffDone != nil {
			<-handoffDone
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
		item.closeErr = result
	})
	return item.closeErr
}

func (server *Server) Close() error {
	server.mu.Lock()
	firstClose := !server.closed
	server.closed = true
	listener := server.listener
	var handoffListeners []*net.UnixListener
	var activeConnections []*net.UnixConn
	for _, item := range server.attachments {
		if item.uffdListener != nil {
			handoffListeners = append(handoffListeners, item.uffdListener)
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
	attachments := make(map[string]*attachment, len(server.attachments))
	for token, item := range server.attachments {
		attachments[token] = item
	}
	server.mu.Unlock()
	var result error
	for token, item := range attachments {
		result = errors.Join(result, server.closeAttachment(item))
		server.mu.Lock()
		if server.attachments[token] == item {
			delete(server.attachments, token)
		}
		server.mu.Unlock()
	}
	return errors.Join(result, server.removeOwnedControlSocket())
}

func controlSocketIdentity(path string) (unixSocketIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return unixSocketIdentity{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return unixSocketIdentity{}, fmt.Errorf("path is not a Unix socket")
	}
	return unixSocketIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func prepareControlSocketPath(path string) error {
	identity, err := controlSocketIdentity(path)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("control socket path %q already exists", path)
	}
	connection, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return fmt.Errorf("control socket path %q already exists", path)
	}
	if !errors.Is(dialErr, unix.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
		return fmt.Errorf("control socket path %q already exists: %w", path, dialErr)
	}
	return removeControlSocketIfOwned(path, identity)
}

func removeControlSocketIfOwned(path string, expected unixSocketIdentity) error {
	actual, err := controlSocketIdentity(path)
	if errors.Is(err, unix.ENOENT) || err != nil || actual != expected {
		return nil
	}
	return unix.Unlink(path)
}

func (server *Server) removeOwnedControlSocket() error {
	server.mu.Lock()
	if !server.controlSocketOwned {
		server.mu.Unlock()
		return nil
	}
	server.controlSocketOwned = false
	identity := server.controlSocketIdentity
	path := server.socketPath
	server.mu.Unlock()
	return removeControlSocketIfOwned(path, identity)
}

func (server *Server) isClosed() bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.closed
}
