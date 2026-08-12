package netstack

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
)

func testCachedAttachment(id int, networkName, ifName string) cniAttachment {
	return cniAttachment{
		ContainerID:   cniContainerIDPrefix + strconv.Itoa(id),
		NetworkName:   networkName,
		InterfaceName: ifName,
		NetNS:         filepath.Join(networkNamespaceDir, networkNamespacePrefix+strconv.Itoa(id)),
	}
}

func TestReconcileStaleCacheRemovesOnlyCurrentConchAttachments(t *testing.T) {
	const bridgeNetwork = "conch-test"
	attachments := []cniAttachment{
		testCachedAttachment(2, bridgeNetwork, cniOuterInterfaceName),
		testCachedAttachment(3, bridgeNetwork, cniOuterInterfaceName),
	}
	historical := testCachedAttachment(4, bridgeNetwork, cniOuterInterfaceName)
	historical.NetNS = "/var/run/netns/ns-4"
	attachments = append(attachments, historical, testCachedAttachment(5, "other-network", cniOuterInterfaceName))

	var mu sync.Mutex
	var removed []string
	manager := &CNIManager{
		backend: &fakeCNIBackend{
			attachments: func() ([]cniAttachment, error) { return attachments, nil },
			remove: func(_ context.Context, id, path string) error {
				if path != "" {
					t.Errorf("Remove(%q) path = %q, want empty", id, path)
				}
				mu.Lock()
				removed = append(removed, id)
				mu.Unlock()
				return nil
			},
		},
		ifName:            cniOuterInterfaceName,
		bridgeNetworkName: bridgeNetwork,
	}

	count, err := manager.reconcileStaleCache(context.Background())
	if err != nil {
		t.Fatalf("reconcileStaleCache(): %v", err)
	}
	if count != 2 {
		t.Fatalf("reconcileStaleCache() count = %d, want 2", count)
	}
	mu.Lock()
	slices.Sort(removed)
	gotRemoved := append([]string(nil), removed...)
	mu.Unlock()
	if !slices.Equal(gotRemoved, []string{"conch-slot-2", "conch-slot-3"}) {
		t.Fatalf("removed attachments = %v", gotRemoved)
	}
}

func TestReconcileStaleCacheDoesNotRetryFailedCNIDel(t *testing.T) {
	const bridgeNetwork = "conch-test"
	wantErr := errors.New("device or resource busy")
	removeCalls := 0
	manager := &CNIManager{
		backend: &fakeCNIBackend{
			attachments: func() ([]cniAttachment, error) {
				return []cniAttachment{testCachedAttachment(6, bridgeNetwork, cniOuterInterfaceName)}, nil
			},
			remove: func(context.Context, string, string) error {
				removeCalls++
				return wantErr
			},
		},
		ifName:            cniOuterInterfaceName,
		bridgeNetworkName: bridgeNetwork,
	}

	count, err := manager.reconcileStaleCache(context.Background())
	if count != 1 || !errors.Is(err, wantErr) {
		t.Fatalf("reconcileStaleCache() = (%d, %v), want (1, %v)", count, err, wantErr)
	}
	if removeCalls != 1 {
		t.Fatalf("CNI Remove calls = %d, want 1", removeCalls)
	}
}

func TestReconcileStaleCacheReportsAttachmentEnumerationFailure(t *testing.T) {
	wantErr := errors.New("cache unavailable")
	manager := &CNIManager{
		backend:           &fakeCNIBackend{attachments: func() ([]cniAttachment, error) { return nil, wantErr }},
		ifName:            cniOuterInterfaceName,
		bridgeNetworkName: "conch-test",
	}

	count, err := manager.reconcileStaleCache(context.Background())
	if count != 0 || !errors.Is(err, wantErr) {
		t.Fatalf("reconcileStaleCache() = (%d, %v), want (0, %v)", count, err, wantErr)
	}
}
