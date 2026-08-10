package memorymode

import (
	"errors"
	"testing"

	conchimage "github.com/openeuler/Conch/internal/image"
)

func TestResolveDecisionTable(t *testing.T) {
	tests := []struct {
		name         string
		input        Input
		want         EffectiveMode
		precondition bool
		wantError    bool
	}{
		{name: "full cold", input: Input{Requested: RequestedFull, VMMName: "stratovirt"}, want: EffectiveFull},
		{name: "incremental cold", input: Input{Requested: RequestedIncremental, VMMName: "stratovirt"}, want: EffectiveIncremental},
		{name: "full resume full", input: Input{Requested: RequestedFull, VMMName: "stratovirt", Resume: true, ArtifactFormat: conchimage.MemoryFormatFull}, want: EffectiveFull},
		{name: "incremental resume incremental", input: Input{Requested: RequestedIncremental, VMMName: "stratovirt", Resume: true, ArtifactFormat: conchimage.MemoryFormatIncrementalV1}, want: EffectiveIncremental},
		{name: "full rejects incremental artifact", input: Input{Requested: RequestedFull, VMMName: "stratovirt", Resume: true, ArtifactFormat: conchimage.MemoryFormatIncrementalV1}, precondition: true, wantError: true},
		{name: "incremental rejects full artifact", input: Input{Requested: RequestedIncremental, VMMName: "stratovirt", Resume: true, ArtifactFormat: conchimage.MemoryFormatFull}, precondition: true, wantError: true},
		{name: "missing resume format", input: Input{Requested: RequestedIncremental, VMMName: "stratovirt", Resume: true}, precondition: true, wantError: true},
		{name: "invalid requested mode", input: Input{Requested: "invalid", VMMName: "stratovirt"}, wantError: true},
		{name: "cloud hypervisor ignores cow", input: Input{Requested: RequestedIncremental, VMMName: "cloud-hypervisor", Resume: true, ArtifactFormat: conchimage.MemoryFormatIncrementalV1}, want: EffectiveFull},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Resolve(test.input)
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
		})
	}
}
