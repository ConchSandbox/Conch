package memorymode

import (
	"context"
	"errors"
	"testing"

	"github.com/openeuler/Conch/internal/cow"
)

type fakeCapabilities struct {
	cap   cow.Capabilities
	err   error
	calls int
}

func (provider *fakeCapabilities) Capabilities(context.Context) (cow.Capabilities, error) {
	provider.calls++
	return provider.cap, provider.err
}

func TestResolveDecisionTable(t *testing.T) {
	operational := errors.New("operational")
	tests := []struct {
		name         string
		input        Input
		cap          cow.Capabilities
		capErr       error
		want         EffectiveMode
		precondition bool
		wantError    bool
		wantCalls    int
	}{
		{name: "full cold", input: Input{Requested: RequestedFull, VMMName: "stratovirt"}, want: EffectiveFull},
		{name: "incremental supported", input: Input{Requested: RequestedIncremental, VMMName: "stratovirt"}, cap: cow.Capabilities{IncrementalMemory: cow.CapabilitySupported}, want: EffectiveIncremental, wantCalls: 1},
		{name: "incremental unsupported", input: Input{Requested: RequestedIncremental, VMMName: "stratovirt"}, cap: cow.Capabilities{IncrementalMemory: cow.CapabilityUnsupported}, precondition: true, wantError: true, wantCalls: 1},
		{name: "auto supported", input: Input{Requested: RequestedAuto, VMMName: "stratovirt"}, cap: cow.Capabilities{IncrementalMemory: cow.CapabilitySupported}, want: EffectiveIncremental, wantCalls: 1},
		{name: "auto unsupported", input: Input{Requested: RequestedAuto, VMMName: "stratovirt"}, cap: cow.Capabilities{IncrementalMemory: cow.CapabilityUnsupported}, want: EffectiveFull, wantCalls: 1},
		{name: "auto unknown", input: Input{Requested: RequestedAuto, VMMName: "stratovirt"}, cap: cow.Capabilities{IncrementalMemory: cow.CapabilityUnknown}, wantError: true, wantCalls: 1},
		{name: "auto dial error", input: Input{Requested: RequestedAuto, VMMName: "stratovirt"}, capErr: operational, wantError: true, wantCalls: 1},
		{name: "auto resume full", input: Input{Requested: RequestedAuto, VMMName: "stratovirt", Resume: true, ArtifactFormat: FormatFullV1}, want: EffectiveFull},
		{name: "auto resume incremental", input: Input{Requested: RequestedAuto, VMMName: "stratovirt", Resume: true, ArtifactFormat: FormatIncrementalV1}, cap: cow.Capabilities{IncrementalMemory: cow.CapabilitySupported}, want: EffectiveIncremental, wantCalls: 1},
		{name: "auto incremental artifact unsupported", input: Input{Requested: RequestedAuto, VMMName: "stratovirt", Resume: true, ArtifactFormat: FormatIncrementalV1}, cap: cow.Capabilities{IncrementalMemory: cow.CapabilityUnsupported}, precondition: true, wantError: true, wantCalls: 1},
		{name: "full rejects incremental artifact", input: Input{Requested: RequestedFull, VMMName: "stratovirt", Resume: true, ArtifactFormat: FormatIncrementalV1}, precondition: true, wantError: true},
		{name: "incremental rejects full artifact", input: Input{Requested: RequestedIncremental, VMMName: "stratovirt", Resume: true, ArtifactFormat: FormatFullV1}, precondition: true, wantError: true},
		{name: "missing resume format", input: Input{Requested: RequestedAuto, VMMName: "stratovirt", Resume: true}, precondition: true, wantError: true},
		{name: "invalid requested mode", input: Input{Requested: "invalid", VMMName: "stratovirt"}, wantError: true},
		{name: "cloud hypervisor ignores cow", input: Input{Requested: RequestedIncremental, VMMName: "cloud-hypervisor", Resume: true, ArtifactFormat: FormatIncrementalV1}, want: EffectiveFull},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &fakeCapabilities{cap: test.cap, err: test.capErr}
			got, err := Resolve(context.Background(), provider, test.input)
			if test.wantError {
				if err == nil {
					t.Fatal("Resolve() error = nil")
				}
				if errors.Is(err, ErrPrecondition) != test.precondition {
					t.Fatalf("Resolve() error = %v, precondition=%v", err, errors.Is(err, ErrPrecondition))
				}
			} else if err != nil || got != test.want {
				t.Fatalf("Resolve() = (%q, %v), want (%q, nil)", got, err, test.want)
			}
			if provider.calls != test.wantCalls {
				t.Fatalf("Capabilities calls = %d, want %d", provider.calls, test.wantCalls)
			}
		})
	}
}
