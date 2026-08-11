package cow

import (
	"errors"
	"io"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestProbeCapabilitiesClassifiesUnsupportedAndOperationalErrors(t *testing.T) {
	tests := []struct {
		name        string
		userfaultfd error
		ioctlErr    error
		api         uint64
		features    uint64
		pageSize    int
		pagemapErr  error
		wantState   string
		wantMissing []string
		wantError   bool
	}{
		{name: "supported", api: uffdAPI, features: requiredUFFDFeatures, pageSize: 4096, wantState: CapabilitySupported},
		{name: "64K host page", api: uffdAPI, features: requiredUFFDFeatures, pageSize: 64 * 1024, wantState: CapabilitySupported},
		{name: "syscall absent", userfaultfd: unix.ENOSYS, wantState: CapabilityUnsupported, wantMissing: requiredUFFDFeatureNames},
		{name: "missing async write protect", api: uffdAPI, features: requiredUFFDFeatures &^ uffdFeatureWPAsync, pageSize: 4096, wantState: CapabilityUnsupported, wantMissing: []string{"uffd.wp_async"}},
		{name: "incompatible host page", api: uffdAPI, features: requiredUFFDFeatures, pageSize: 6000, wantState: CapabilityUnsupported, wantMissing: []string{hostPageSizeFeatureName}},
		{name: "permission", userfaultfd: unix.EPERM, wantState: CapabilityUnknown, wantError: true},
		{name: "malformed API", api: 0, features: requiredUFFDFeatures, pageSize: 4096, wantState: CapabilityUnknown, wantError: true},
		{name: "pagemap permission", api: uffdAPI, features: requiredUFFDFeatures, pageSize: 4096, pagemapErr: unix.EACCES, wantState: CapabilityUnknown, wantMissing: []string{pagemapFeatureName}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			closed := false
			ops := probeOps{
				pageSize: func() int { return test.pageSize },
				userfaultfd: func(flags int) (int, error) {
					if flags != unix.O_CLOEXEC|unix.O_NONBLOCK {
						t.Fatalf("userfaultfd flags = %#x", flags)
					}
					return 91, test.userfaultfd
				},
				ioctlAPI: func(fd int, api *uffdioAPI) error {
					api.API = test.api
					api.Features = test.features
					return test.ioctlErr
				},
				openPagemap: func() (io.ReadCloser, error) {
					if test.pagemapErr != nil {
						return nil, test.pagemapErr
					}
					return &probeTestReader{closed: &closed}, nil
				},
				closeFD: func(int) error { return nil },
			}
			got := probeCapabilities(ops)
			if got.IncrementalMemory != test.wantState {
				t.Fatalf("state = %q, want %q (%+v)", got.IncrementalMemory, test.wantState, got)
			}
			if strings.Join(got.MissingFeatures, ",") != strings.Join(test.wantMissing, ",") {
				t.Fatalf("missing = %v, want %v", got.MissingFeatures, test.wantMissing)
			}
			if (got.ProbeError != "") != test.wantError {
				t.Fatalf("ProbeError = %q, want present=%v", got.ProbeError, test.wantError)
			}
		})
	}
}

type probeTestReader struct {
	closed *bool
}

func (reader *probeTestReader) Read(buffer []byte) (int, error) {
	if len(buffer) != 8 {
		return 0, errors.New("pagemap read must be exactly one entry")
	}
	for index := range buffer {
		buffer[index] = 1
	}
	return len(buffer), nil
}

func (reader *probeTestReader) Close() error {
	*reader.closed = true
	return nil
}
