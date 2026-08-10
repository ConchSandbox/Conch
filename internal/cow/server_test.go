package cow

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openeuler/Conch/internal/memsnap"
	"golang.org/x/sys/unix"
)

func TestServerCloseReleasesPartialControlFrame(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "cow.sock")
	server := newServer(socketPath, Capabilities{IncrementalMemory: CapabilitySupported})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx) }()
	select {
	case <-server.Ready():
	case <-time.After(time.Second):
		t.Fatal("server did not become ready")
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- server.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		_ = conn.Close()
		<-closeDone
		t.Fatal("Server.Close blocked on a partial control frame")
	}
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}

func TestServerSignalsReadyAndReportsCapabilities(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "cow.sock")
	server := newServer(socketPath, Capabilities{IncrementalMemory: CapabilityUnsupported, MissingFeatures: []string{"uffd.wp_async"}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	select {
	case <-server.Ready():
	case <-time.After(time.Second):
		t.Fatal("server did not become ready")
	}
	capabilities, err := NewClient(socketPath).Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.IncrementalMemory != CapabilityUnsupported || len(capabilities.MissingFeatures) != 1 {
		t.Fatalf("Capabilities() = %+v", capabilities)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control socket remains after close: %v", err)
	}
}

func TestClientCapabilitiesRejectsInvalidIncrementalMemoryState(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "cow.sock")
	server := newServer(socketPath, Capabilities{IncrementalMemory: "invalid"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	select {
	case <-server.Ready():
	case <-time.After(time.Second):
		t.Fatal("server did not become ready")
	}

	_, err := NewClient(socketPath).Capabilities(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid incremental memory state") {
		t.Fatalf("Capabilities() error = %v", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAttachCreatesIndependentSealedMemfdsAndDetachIsIdempotent(t *testing.T) {
	root := writeServerManifest(t)
	server := newServer(filepath.Join(t.TempDir(), "cow.sock"), Capabilities{IncrementalMemory: CapabilitySupported})
	server.uffdOps.acceptTimeout = time.Second
	t.Cleanup(func() { _ = server.Close() })

	attach := func(sandboxID string) (Response, []int) {
		request := Request{RequestID: sandboxID, SandboxID: sandboxID, MemorySnapshotRoot: root}
		response, fds := server.handleAttach(request, baseResponse(request))
		if !response.OK || len(fds) != 1 || response.UFFDSocketPath == "" {
			closeFDs(fds)
			t.Fatalf("Attach(%s) = %+v, fds=%v", sandboxID, response, fds)
		}
		return response, fds
	}
	first, firstFDs := attach("vm-a")
	second, secondFDs := attach("vm-b")
	t.Cleanup(func() { closeFDs(firstFDs); closeFDs(secondFDs) })
	if first.Token == second.Token {
		t.Fatal("independent attaches share a token")
	}
	for _, fd := range []int{firstFDs[0], secondFDs[0]} {
		seals, err := unix.FcntlInt(uintptr(fd), unix.F_GET_SEALS, 0)
		if err != nil {
			t.Fatal(err)
		}
		want := unix.F_SEAL_GROW | unix.F_SEAL_SHRINK
		if seals&want != want {
			t.Fatalf("memfd seals = %#x, want %#x", seals, want)
		}
	}
	for range 2 {
		response := server.handleDetach(Request{Token: first.Token}, baseResponse(Request{}))
		if !response.OK {
			t.Fatalf("Detach() = %+v", response)
		}
	}
	server.mu.Lock()
	_, firstExists := server.attachments[first.Token]
	_, secondExists := server.attachments[second.Token]
	server.mu.Unlock()
	if firstExists || !secondExists {
		t.Fatalf("Detach affected wrong attachment: first=%v second=%v", firstExists, secondExists)
	}
}

func TestWaitAttachmentReadyMatchesSandboxAndCompletedHandoff(t *testing.T) {
	server := newServer(filepath.Join(t.TempDir(), "cow.sock"), Capabilities{IncrementalMemory: CapabilitySupported})
	handoffDone := make(chan struct{})
	close(handoffDone)
	uffdRead, uffdWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer uffdWrite.Close()
	attachment := &attachment{
		token:           "token",
		sandboxID:       "sandbox",
		handoffPrepared: true,
		handoffDone:     handoffDone,
		uffd:            uffdRead,
	}
	server.attachments[attachment.token] = attachment
	response := server.handleWaitAttachmentReady(Request{Token: "token", SandboxID: "sandbox"}, baseResponse(Request{}))
	if !response.OK {
		t.Fatalf("WaitAttachmentReady() = %+v", response)
	}
	wrong := server.handleWaitAttachmentReady(Request{Token: "token", SandboxID: "other"}, baseResponse(Request{}))
	if wrong.OK {
		t.Fatalf("WaitAttachmentReady() accepted wrong sandbox: %+v", wrong)
	}
}

func TestWaitAttachmentReadyReportsHandoffFailure(t *testing.T) {
	server := newServer(filepath.Join(t.TempDir(), "cow.sock"), Capabilities{IncrementalMemory: CapabilitySupported})
	handoffDone := make(chan struct{})
	close(handoffDone)
	item := &attachment{
		token:           "token",
		sandboxID:       "sandbox",
		handoffPrepared: true,
		handoffDone:     handoffDone,
		handoffErr:      errors.New("handoff failed"),
	}
	server.attachments[item.token] = item
	response := server.handleWaitAttachmentReady(Request{Token: "token", SandboxID: "sandbox"}, baseResponse(Request{}))
	if response.OK {
		t.Fatalf("WaitAttachmentReady() = %+v, want failure", response)
	}
}

func writeServerManifest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, memsnap.LayerDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	for index, value := range []byte{0x31, 0x72} {
		layer := make([]byte, 2*memsnap.DefaultBlockSize)
		for offset := uint64(0); offset < memsnap.DefaultBlockSize; offset++ {
			layer[uint64(index)*memsnap.DefaultBlockSize+offset] = value
		}
		if err := os.WriteFile(filepath.Join(root, memsnap.LayerDirName, string(rune('0'+index))+".mem"), layer, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := memsnap.Manifest{
		SchemaVersion: memsnap.SchemaVersion,
		MemorySize:    2 * memsnap.DefaultBlockSize,
		BlockSize:     memsnap.DefaultBlockSize,
		Layers:        []string{"layers/0.mem", "layers/1.mem"},
		BuildMap: []memsnap.BuildRange{
			{Offset: 0, Length: memsnap.DefaultBlockSize, LayerIndex: 0},
			{Offset: memsnap.DefaultBlockSize, Length: memsnap.DefaultBlockSize, LayerIndex: 1},
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, memsnap.ManifestFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}
