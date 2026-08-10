package cow

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestFrameReadsPartialHeaderAndPayload(t *testing.T) {
	reader, writer := testUnixSocketPair(t)
	want := Request{Type: RequestCapabilities, ProtocolVersion: ProtocolVersion, RequestID: "partial"}
	payload := []byte(`{"type":"Capabilities","protocol_version":1,"request_id":"partial"}`)
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	done := make(chan error, 1)
	go func() {
		for _, piece := range [][]byte{header[:2], header[2:], payload[:7], payload[7:]} {
			if _, err := writer.Write(piece); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	var got Request
	fds, err := readFrame(reader, &got)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFDs(fds)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("request = %#v, want %#v", got, want)
	}
}

func TestFrameRejectsOversizeAndTrailingJSON(t *testing.T) {
	t.Run("oversize", func(t *testing.T) {
		reader, writer := testUnixSocketPair(t)
		header := make([]byte, 4)
		binary.BigEndian.PutUint32(header, maxFrameSize+1)
		if _, err := writer.Write(header); err != nil {
			t.Fatal(err)
		}
		var request Request
		if _, err := readFrame(reader, &request); err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("readFrame() error = %v, want too large", err)
		}
	})
	t.Run("trailing JSON", func(t *testing.T) {
		reader, writer := testUnixSocketPair(t)
		payload := []byte(`{"type":"Capabilities","protocol_version":1,"request_id":"first"}{}`)
		header := make([]byte, 4)
		binary.BigEndian.PutUint32(header, uint32(len(payload)))
		if _, err := writer.Write(append(header, payload...)); err != nil {
			t.Fatal(err)
		}
		var request Request
		if fds, err := readFrame(reader, &request); err == nil {
			closeFDs(fds)
			t.Fatal("readFrame accepted multiple JSON values")
		}
	})
}

func TestValidateRequestRequiresOperationFieldsAndFDs(t *testing.T) {
	valid := []struct {
		request Request
		fds     int
	}{
		{request: Request{Type: RequestCapabilities, ProtocolVersion: ProtocolVersion, RequestID: "cap"}},
		{request: Request{Type: RequestAttach, ProtocolVersion: ProtocolVersion, RequestID: "attach", MemorySnapshotRoot: "/memory"}},
		{request: Request{Type: RequestWaitAttachmentReady, ProtocolVersion: ProtocolVersion, RequestID: "wait", Token: "token", SandboxID: "sandbox"}},
		{request: Request{Type: RequestDetach, ProtocolVersion: ProtocolVersion, RequestID: "detach", Token: "token"}},
	}
	for _, test := range valid {
		if err := validateRequest(test.request, make([]int, test.fds)); err != nil {
			t.Fatalf("validateRequest(%s): %v", test.request.Type, err)
		}
	}
	invalid := []struct {
		name    string
		request Request
		fds     int
	}{
		{name: "old version", request: Request{Type: RequestCapabilities, RequestID: "id"}},
		{name: "future version", request: Request{Type: RequestCapabilities, ProtocolVersion: 2, RequestID: "id"}},
		{name: "unknown operation", request: Request{Type: "Future", ProtocolVersion: ProtocolVersion, RequestID: "id"}},
		{name: "attach root", request: Request{Type: RequestAttach, ProtocolVersion: ProtocolVersion, RequestID: "id"}},
		{name: "wait sandbox", request: Request{Type: RequestWaitAttachmentReady, ProtocolVersion: ProtocolVersion, RequestID: "id", Token: "token"}},
		{name: "wait token", request: Request{Type: RequestWaitAttachmentReady, ProtocolVersion: ProtocolVersion, RequestID: "id", SandboxID: "sandbox"}},
		{name: "wait descriptor", request: Request{Type: RequestWaitAttachmentReady, ProtocolVersion: ProtocolVersion, RequestID: "id", Token: "token", SandboxID: "sandbox"}, fds: 1},
		{name: "detach token", request: Request{Type: RequestDetach, ProtocolVersion: ProtocolVersion, RequestID: "id"}},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if err := validateRequest(test.request, make([]int, test.fds)); err == nil {
				t.Fatal("validateRequest accepted invalid request")
			}
		})
	}
}

func TestValidateResponseClosesUnexpectedFDs(t *testing.T) {
	fds := testPipeFDs(t, 2)
	response := Response{OK: true, ProtocolVersion: ProtocolVersion, RequestID: "request"}
	if err := validateResponse("request", response, fds, 0); err == nil {
		t.Fatal("validateResponse accepted unexpected descriptors")
	}
	for _, fd := range fds {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			t.Fatalf("descriptor %d remained open", fd)
		}
	}
}

func TestClientWaitAttachmentReadySendsNoDescriptors(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "cow.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		var request Request
		fds, err := readFrame(conn, &request)
		if err != nil {
			serverDone <- err
			return
		}
		defer closeFDs(fds)
		if err := validateRequest(request, fds); err != nil {
			serverDone <- err
			return
		}
		if request.Type != RequestWaitAttachmentReady || request.Token != "token" || request.SandboxID != "sandbox" || len(fds) != 0 {
			serverDone <- &testError{"unexpected WaitAttachmentReady request"}
			return
		}
		serverDone <- writeFrame(conn, Response{OK: true, ProtocolVersion: ProtocolVersion, RequestID: request.RequestID}, nil)
	}()

	client := NewClient(socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.WaitAttachmentReady(ctx, "token", "sandbox"); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

type testError struct{ message string }

func (err *testError) Error() string { return err.message }

func testUnixSocketPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	leftFile := os.NewFile(uintptr(fds[0]), "left")
	rightFile := os.NewFile(uintptr(fds[1]), "right")
	leftConnection, err := net.FileConn(leftFile)
	_ = leftFile.Close()
	if err != nil {
		_ = rightFile.Close()
		t.Fatal(err)
	}
	rightConnection, err := net.FileConn(rightFile)
	_ = rightFile.Close()
	if err != nil {
		_ = leftConnection.Close()
		t.Fatal(err)
	}
	left := leftConnection.(*net.UnixConn)
	right := rightConnection.(*net.UnixConn)
	t.Cleanup(func() { _ = left.Close(); _ = right.Close() })
	return left, right
}

func testPipeFDs(t *testing.T, count int) []int {
	t.Helper()
	fds := make([]int, 0, count)
	for range count {
		readEnd, writeEnd, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_ = writeEnd.Close()
		fd, err := unix.Dup(int(readEnd.Fd()))
		_ = readEnd.Close()
		if err != nil {
			t.Fatal(err)
		}
		fds = append(fds, fd)
	}
	return fds
}
