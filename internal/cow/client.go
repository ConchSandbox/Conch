package cow

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/google/uuid"
)

const (
	requestTimeout           = 5 * time.Second
	defaultUFFDAcceptTimeout = 30 * time.Second
	waitReadyTimeoutMargin   = 2 * time.Second
)

type Client struct {
	socketPath     string
	requestTimeout time.Duration
	waitTimeout    time.Duration
}

func NewClient(socketPath string) *Client {
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	return &Client{
		socketPath:     socketPath,
		requestTimeout: requestTimeout,
		waitTimeout:    defaultUFFDAcceptTimeout + waitReadyTimeoutMargin,
	}
}

func (client *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	response, err := client.simple(ctx, Request{Type: RequestCapabilities}, nil)
	if err != nil {
		return Capabilities{}, err
	}
	if response.Capabilities == nil {
		return Capabilities{}, fmt.Errorf("cow capabilities response is missing report")
	}
	switch response.Capabilities.IncrementalMemory {
	case CapabilitySupported, CapabilityUnsupported, CapabilityUnknown:
	default:
		return Capabilities{}, fmt.Errorf(
			"cow capabilities response has invalid incremental memory state %q",
			response.Capabilities.IncrementalMemory,
		)
	}
	return *response.Capabilities, nil
}

func (client *Client) Attach(ctx context.Context, request Request) (*os.File, Response, error) {
	request.Type = RequestAttach
	conn, request, err := client.start(ctx, request, nil)
	if err != nil {
		return nil, Response{}, err
	}
	defer conn.Close()
	var response Response
	fds, err := readFrame(conn, &response)
	if err != nil {
		return nil, Response{}, err
	}
	expectedFDs := 0
	if response.OK {
		expectedFDs = 1
	}
	if err := validateResponse(request.RequestID, response, fds, expectedFDs); err != nil {
		return nil, Response{}, err
	}
	if !response.OK {
		return nil, response, fmt.Errorf("cow Attach failed: %s", response.Error)
	}
	file := os.NewFile(uintptr(fds[0]), "conch-cow-memory")
	if file == nil {
		closeFDs(fds)
		return nil, response, fmt.Errorf("wrap cow memory descriptor")
	}
	return file, response, nil
}

func (client *Client) WaitAttachmentReady(ctx context.Context, token, sandboxID string) (Response, error) {
	return client.simple(ctx, Request{Type: RequestWaitAttachmentReady, Token: token, SandboxID: sandboxID}, nil)
}

func (client *Client) Detach(ctx context.Context, token string) (Response, error) {
	return client.simple(ctx, Request{Type: RequestDetach, Token: token}, nil)
}

func (client *Client) simple(ctx context.Context, request Request, fds []int) (Response, error) {
	conn, request, err := client.start(ctx, request, fds)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	var response Response
	responseFDs, err := readFrame(conn, &response)
	if err != nil {
		return Response{}, err
	}
	if err := validateResponse(request.RequestID, response, responseFDs, 0); err != nil {
		return Response{}, err
	}
	if !response.OK {
		return response, fmt.Errorf("cow %s failed: %s", request.Type, response.Error)
	}
	return response, nil
}

func (client *Client) start(ctx context.Context, request Request, fds []int) (*net.UnixConn, Request, error) {
	timeout := client.requestTimeout
	if request.Type == RequestWaitAttachmentReady {
		timeout = client.waitTimeout
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request.ProtocolVersion = ProtocolVersion
	if request.RequestID == "" {
		request.RequestID = uuid.NewString()
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(requestContext, "unix", client.socketPath)
	if err != nil {
		return nil, request, fmt.Errorf("dial cow: %w", err)
	}
	conn, ok := connection.(*net.UnixConn)
	if !ok {
		_ = connection.Close()
		return nil, request, fmt.Errorf("cow connection is not a Unix socket")
	}
	if deadline, ok := requestContext.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := writeFrame(conn, request, fds); err != nil {
		_ = conn.Close()
		return nil, request, err
	}
	return conn, request, nil
}
