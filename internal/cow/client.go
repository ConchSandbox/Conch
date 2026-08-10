package cow

import (
	"context"
	"encoding/json"
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
	return &Client{
		socketPath:     socketPath,
		requestTimeout: requestTimeout,
		waitTimeout:    defaultUFFDAcceptTimeout + waitReadyTimeoutMargin,
	}
}

func (client *Client) Ping(ctx context.Context) error {
	return client.callWithoutFDs(ctx, RequestPing, PingRequest{}, &PingResponse{})
}

func (client *Client) Attach(ctx context.Context, request AttachRequest) (*os.File, AttachResponse, error) {
	conn, envelope, err := client.start(ctx, RequestAttach, request)
	if err != nil {
		return nil, AttachResponse{}, err
	}
	defer conn.Close()
	var response responseEnvelope
	fds, err := readFrame(conn, &response)
	if err != nil {
		return nil, AttachResponse{}, err
	}
	expectedFDs := 0
	if response.OK {
		expectedFDs = 1
	}
	if err := validateResponse(envelope.RequestID, response, fds, expectedFDs); err != nil {
		return nil, AttachResponse{}, err
	}
	if !response.OK {
		return nil, AttachResponse{}, fmt.Errorf("cow Attach failed: %s", response.Error)
	}
	var result AttachResponse
	if err := decodeStrictJSON(response.Result, &result); err != nil {
		closeFDs(fds)
		return nil, AttachResponse{}, fmt.Errorf("decode cow Attach result: %w", err)
	}
	file := os.NewFile(uintptr(fds[0]), "conch-cow-memory")
	if file == nil {
		closeFDs(fds)
		return nil, AttachResponse{}, fmt.Errorf("wrap cow memory descriptor")
	}
	return file, result, nil
}

func (client *Client) WaitAttachmentReady(ctx context.Context, token, sandboxID string) error {
	return client.callWithoutFDs(ctx, RequestWaitAttachmentReady, WaitAttachmentReadyRequest{Token: token, SandboxID: sandboxID}, &WaitAttachmentReadyResponse{})
}

func (client *Client) Detach(ctx context.Context, token string) error {
	return client.callWithoutFDs(ctx, RequestDetach, DetachRequest{Token: token}, &DetachResponse{})
}

func (client *Client) callWithoutFDs(ctx context.Context, requestType string, params, result any) error {
	conn, request, err := client.start(ctx, requestType, params)
	if err != nil {
		return err
	}
	defer conn.Close()
	var response responseEnvelope
	responseFDs, err := readFrame(conn, &response)
	if err != nil {
		return err
	}
	if err := validateResponse(request.RequestID, response, responseFDs, 0); err != nil {
		return err
	}
	if !response.OK {
		return fmt.Errorf("cow %s failed: %s", request.Type, response.Error)
	}
	if err := decodeStrictJSON(response.Result, result); err != nil {
		return fmt.Errorf("decode cow %s result: %w", request.Type, err)
	}
	return nil
}

func (client *Client) start(ctx context.Context, requestType string, params any) (*net.UnixConn, requestEnvelope, error) {
	timeout := client.requestTimeout
	if requestType == RequestWaitAttachmentReady {
		timeout = client.waitTimeout
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	payload, err := json.Marshal(params)
	if err != nil {
		return nil, requestEnvelope{}, fmt.Errorf("marshal cow %s params: %w", requestType, err)
	}
	request := requestEnvelope{Type: requestType, ProtocolVersion: ProtocolVersion, RequestID: uuid.NewString(), Params: payload}
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
	if err := writeFrame(conn, request, nil); err != nil {
		_ = conn.Close()
		return nil, request, err
	}
	return conn, request, nil
}
