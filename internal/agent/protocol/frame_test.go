package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/openeuler/Conch/internal/netstack"
)

func TestFrameRoundTripHandlesShortReadsAndWrites(t *testing.T) {
	wire := &shortBuffer{max: 3}
	want := InitRequest{
		Version:    ProtocolVersion,
		SandboxID:  "sandbox-1",
		AgentToken: "token",
		Network: netstack.GuestNetworkConfig{
			GuestIP:      "192.168.100.21",
			PrefixLength: 24,
			Gateway:      "192.168.100.2",
		},
	}
	if err := WriteFrame(wire, want); err != nil {
		t.Fatalf("WriteFrame() error = %v", err)
	}
	var got InitRequest
	if err := ReadFrame(wire, &got); err != nil {
		t.Fatalf("ReadFrame() error = %v", err)
	}
	if got.Version != want.Version || got.SandboxID != want.SandboxID || !reflect.DeepEqual(got.Network, want.Network) {
		t.Fatalf("ReadFrame() = %#v, want %#v", got, want)
	}
}

func TestReadFrameRejectsInvalidPayloads(t *testing.T) {
	oversized := make([]byte, 4)
	binary.BigEndian.PutUint32(oversized, MaxPayloadSize+1)
	if err := ReadFrame(bytes.NewReader(oversized), &InitRequest{}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("oversized ReadFrame() error = %v", err)
	}

	var wire bytes.Buffer
	payload := []byte(`{"version":1,"unknown":true}`)
	_ = binary.Write(&wire, binary.BigEndian, uint32(len(payload)))
	wire.Write(payload)
	if err := ReadFrame(&wire, &InitRequest{}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown-field ReadFrame() error = %v", err)
	}
}

func TestMarshalPayloadRejectsOversizedValue(t *testing.T) {
	if _, err := MarshalPayload(strings.Repeat("x", MaxPayloadSize)); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("MarshalPayload() error = %v, want ErrPayloadTooLarge", err)
	}
}

type shortBuffer struct {
	bytes.Buffer
	max int
}

func (b *shortBuffer) Write(p []byte) (int, error) {
	if len(p) > b.max {
		p = p[:b.max]
	}
	return b.Buffer.Write(p)
}

func (b *shortBuffer) Read(p []byte) (int, error) {
	if len(p) > b.max {
		p = p[:b.max]
	}
	return b.Buffer.Read(p)
}
