package cow

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"

	"golang.org/x/sys/unix"
)

const (
	maxFrameSize = 1 << 20
	maxFrameFDs  = 8
)

var defaultFrameOOBLen = unix.CmsgSpace(maxFrameFDs * 4)

func writeFrame(conn *net.UnixConn, message any, fds []int) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}
	if len(payload) > maxFrameSize {
		return fmt.Errorf("frame too large: %d", len(payload))
	}
	if len(fds) > maxFrameFDs {
		return fmt.Errorf("too many frame descriptors: %d", len(fds))
	}
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	var rights []byte
	if len(fds) != 0 {
		rights = unix.UnixRights(fds...)
	}
	written, _, err := conn.WriteMsgUnix(frame, rights, nil)
	if err != nil {
		return fmt.Errorf("write frame: %w", err)
	}
	if written < len(frame) {
		if _, err := conn.Write(frame[written:]); err != nil {
			return fmt.Errorf("finish frame write: %w", err)
		}
	}
	return nil
}

func readFrame(conn *net.UnixConn, message any) ([]int, error) {
	header := make([]byte, 4)
	oob := make([]byte, defaultFrameOOBLen)
	read, oobRead, flags, _, err := conn.ReadMsgUnix(header, oob)
	if err != nil {
		return nil, fmt.Errorf("read frame header: %w", err)
	}
	fds, err := parseFDs(oob[:oobRead])
	if err != nil {
		return nil, err
	}
	if flags&unix.MSG_CTRUNC != 0 {
		closeFDs(fds)
		return nil, fmt.Errorf("frame ancillary data truncated")
	}
	if flags&unix.MSG_TRUNC != 0 {
		closeFDs(fds)
		return nil, fmt.Errorf("frame header truncated")
	}
	if read < len(header) {
		if _, err := io.ReadFull(conn, header[read:]); err != nil {
			closeFDs(fds)
			return nil, fmt.Errorf("read frame header: %w", err)
		}
	}
	length := binary.BigEndian.Uint32(header)
	if length > maxFrameSize {
		closeFDs(fds)
		return nil, fmt.Errorf("frame too large: %d", length)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(conn, payload); err != nil {
		closeFDs(fds)
		return nil, fmt.Errorf("read frame payload: %w", err)
	}
	if err := decodeStrictJSON(payload, message); err != nil {
		closeFDs(fds)
		return nil, fmt.Errorf("decode frame: %w", err)
	}
	return fds, nil
}

func decodeStrictJSON(payload []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

func parseFDs(oob []byte) ([]int, error) {
	if len(oob) == 0 {
		return nil, nil
	}
	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, fmt.Errorf("parse socket control message: %w", err)
	}
	var fds []int
	for _, message := range messages {
		if message.Header.Level != unix.SOL_SOCKET || message.Header.Type != unix.SCM_RIGHTS {
			closeFDs(fds)
			return nil, fmt.Errorf("unexpected socket control message")
		}
		rights, err := unix.ParseUnixRights(&message)
		if err != nil {
			closeFDs(fds)
			return nil, fmt.Errorf("parse SCM_RIGHTS: %w", err)
		}
		for _, fd := range rights {
			unix.CloseOnExec(fd)
		}
		fds = append(fds, rights...)
	}
	return fds, nil
}

func validateResponse(requestID string, response responseEnvelope, fds []int, expectedFDs int) error {
	if response.ProtocolVersion != ProtocolVersion {
		closeFDs(fds)
		return fmt.Errorf("response protocol version %d is not v%d", response.ProtocolVersion, ProtocolVersion)
	}
	if response.RequestID != requestID {
		closeFDs(fds)
		return fmt.Errorf("response request ID %q does not match %q", response.RequestID, requestID)
	}
	if len(fds) != expectedFDs {
		closeFDs(fds)
		return fmt.Errorf("response returned %d descriptors, expected %d", len(fds), expectedFDs)
	}
	return nil
}

func closeFDs(fds []int) {
	for _, fd := range fds {
		_ = unix.Close(fd)
	}
}
