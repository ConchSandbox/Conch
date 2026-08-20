package memorymode

import (
	"errors"
	"testing"

	conchimage "github.com/openeuler/Conch/internal/image"
)

func TestValidateDecisionTable(t *testing.T) {
	tests := []struct {
		name         string
		input        Input
		precondition bool
		wantError    bool
	}{
		{name: "full cold", input: Input{Mode: ModeFull, VMMName: "stratovirt"}},
		{name: "incremental cold", input: Input{Mode: ModeIncremental, VMMName: "stratovirt"}},
		{name: "full resume full", input: Input{Mode: ModeFull, VMMName: "stratovirt", Resume: true, ArtifactFormat: conchimage.MemoryFormatFull}},
		{name: "incremental resume incremental", input: Input{Mode: ModeIncremental, VMMName: "stratovirt", Resume: true, ArtifactFormat: conchimage.MemoryFormatIncrementalV1}},
		{name: "full rejects incremental artifact", input: Input{Mode: ModeFull, VMMName: "stratovirt", Resume: true, ArtifactFormat: conchimage.MemoryFormatIncrementalV1}, precondition: true, wantError: true},
		{name: "incremental rejects full artifact", input: Input{Mode: ModeIncremental, VMMName: "stratovirt", Resume: true, ArtifactFormat: conchimage.MemoryFormatFull}, precondition: true, wantError: true},
		{name: "missing resume format", input: Input{Mode: ModeIncremental, VMMName: "stratovirt", Resume: true}, precondition: true, wantError: true},
		{name: "invalid mode", input: Input{Mode: "invalid", VMMName: "stratovirt"}, wantError: true},
		{name: "cloud hypervisor full", input: Input{Mode: ModeFull, VMMName: "cloud-hypervisor"}},
		{name: "cloud hypervisor rejects incremental", input: Input{Mode: ModeIncremental, VMMName: "cloud-hypervisor"}, precondition: true, wantError: true},
		{name: "unknown VMM", input: Input{Mode: ModeFull, VMMName: "unknown"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Validate(test.input)
			if test.wantError {
				if err == nil {
					t.Fatal("Validate() error = nil")
				}
				if errors.Is(err, ErrPrecondition) != test.precondition {
					t.Fatalf("Validate() error = %v, precondition=%v", err, errors.Is(err, ErrPrecondition))
				}
			} else if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}
