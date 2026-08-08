package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

func MarshalPayload(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 || len(payload) > MaxPayloadSize {
		return nil, fmt.Errorf("payload is %d bytes, maximum is %d", len(payload), MaxPayloadSize)
	}
	return payload, nil
}

func WriteFrame(w io.Writer, value any) error {
	payload, err := MarshalPayload(value)
	if err != nil {
		return err
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(payload)))
	if err := writeFull(w, header); err != nil {
		return fmt.Errorf("write frame header: %w", err)
	}
	if err := writeFull(w, payload); err != nil {
		return fmt.Errorf("write frame payload: %w", err)
	}
	return nil
}

func ReadFrame(r io.Reader, value any) error {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return fmt.Errorf("read frame header: %w", err)
	}
	size := int(binary.BigEndian.Uint32(header))
	if size <= 0 || size > MaxPayloadSize {
		return fmt.Errorf("frame payload size %d is outside [1, %d]", size, MaxPayloadSize)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return fmt.Errorf("read frame payload: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode frame JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode frame JSON: trailing value")
		}
		return fmt.Errorf("decode frame JSON trailer: %w", err)
	}
	return nil
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
