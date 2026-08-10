package cow

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestFrameReadsPartialHeaderAndPayload(t *testing.T) {
	reader, writer := testUnixSocketPair(t)
	want := requestEnvelope{Type: RequestPing, ProtocolVersion: ProtocolVersion, RequestID: "partial", Params: json.RawMessage(`{}`)}
	payload := []byte(`{"type":"Ping","protocol_version":1,"request_id":"partial","params":{}}`)
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
	var got requestEnvelope
	fds, err := readFrame(reader, &got)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFDs(fds)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
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
		var request requestEnvelope
		if _, err := readFrame(reader, &request); err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("readFrame() error = %v, want too large", err)
		}
	})
	t.Run("trailing JSON", func(t *testing.T) {
		reader, writer := testUnixSocketPair(t)
		payload := []byte(`{"type":"Ping","protocol_version":1,"request_id":"first","params":{}}{}`)
		header := make([]byte, 4)
		binary.BigEndian.PutUint32(header, uint32(len(payload)))
		if _, err := writer.Write(append(header, payload...)); err != nil {
			t.Fatal(err)
		}
		var request requestEnvelope
		if fds, err := readFrame(reader, &request); err == nil {
			closeFDs(fds)
			t.Fatal("readFrame accepted multiple JSON values")
		}
	})
}

func TestValidateRequestEnvelope(t *testing.T) {
	valid := []struct {
		request requestEnvelope
		fds     int
	}{
		{request: requestEnvelope{Type: RequestPing, ProtocolVersion: ProtocolVersion, RequestID: "ping"}},
		{request: requestEnvelope{Type: RequestAttach, ProtocolVersion: ProtocolVersion, RequestID: "attach"}},
		{request: requestEnvelope{Type: RequestWaitAttachmentReady, ProtocolVersion: ProtocolVersion, RequestID: "wait"}},
		{request: requestEnvelope{Type: RequestDetach, ProtocolVersion: ProtocolVersion, RequestID: "detach"}},
	}
	for _, test := range valid {
		if err := validateRequestEnvelope(test.request, make([]int, test.fds)); err != nil {
			t.Fatalf("validateRequestEnvelope(%s): %v", test.request.Type, err)
		}
	}
	invalid := []struct {
		name    string
		request requestEnvelope
		fds     int
	}{
		{name: "old version", request: requestEnvelope{Type: RequestPing, RequestID: "id"}},
		{name: "future version", request: requestEnvelope{Type: RequestPing, ProtocolVersion: 2, RequestID: "id"}},
		{name: "unknown operation", request: requestEnvelope{Type: "Future", ProtocolVersion: ProtocolVersion, RequestID: "id"}},
		{name: "descriptor", request: requestEnvelope{Type: RequestPing, ProtocolVersion: ProtocolVersion, RequestID: "id"}, fds: 1},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if err := validateRequestEnvelope(test.request, make([]int, test.fds)); err == nil {
				t.Fatal("validateRequestEnvelope accepted invalid request")
			}
		})
	}
}

func TestOperationPayloadsRejectForeignFields(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		destination any
	}{
		{name: "ping", payload: `{"token":"unexpected"}`, destination: &PingRequest{}},
		{name: "attach", payload: `{"memory_snapshot_root":"/memory","sandbox_id":"sandbox","token":"unexpected"}`, destination: &AttachRequest{}},
		{name: "wait", payload: `{"token":"token","sandbox_id":"sandbox","memory_snapshot_root":"unexpected"}`, destination: &WaitAttachmentReadyRequest{}},
		{name: "detach", payload: `{"token":"token","sandbox_id":"unexpected"}`, destination: &DetachRequest{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := decodeStrictJSON([]byte(test.payload), test.destination); err == nil {
				t.Fatal("decodeStrictJSON accepted a foreign field")
			}
		})
	}
}

func TestValidateResponseClosesUnexpectedFDs(t *testing.T) {
	fds := testPipeFDs(t, 2)
	response := responseEnvelope{OK: true, ProtocolVersion: ProtocolVersion, RequestID: "request"}
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
		var request requestEnvelope
		fds, err := readFrame(conn, &request)
		if err != nil {
			serverDone <- err
			return
		}
		defer closeFDs(fds)
		if err := validateRequestEnvelope(request, fds); err != nil {
			serverDone <- err
			return
		}
		var params WaitAttachmentReadyRequest
		if err := decodeStrictJSON(request.Params, &params); err != nil {
			serverDone <- err
			return
		}
		if request.Type != RequestWaitAttachmentReady || params.Token != "token" || params.SandboxID != "sandbox" || len(fds) != 0 {
			serverDone <- &testError{"unexpected WaitAttachmentReady request"}
			return
		}
		serverDone <- writeFrame(conn, responseEnvelope{OK: true, ProtocolVersion: ProtocolVersion, RequestID: request.RequestID, Result: json.RawMessage(`{}`)}, nil)
	}()

	client := NewClient(socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.WaitAttachmentReady(ctx, "token", "sandbox"); err != nil {
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
