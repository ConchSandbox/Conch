/*
Copyright the e2b-dev Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

[MODIFIED] - Changes made on 2026-08-12 by Team conch: Reconcile current-format
libcni result cache before populating the network pool.
*/
package netstack

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const cniCacheCleanupWorkers = 16

type cniCachedSlot struct {
	containerID string
}

// reconcileStaleCache removes current-format Conch CNI attachments which no
// longer have process-local Slot state. It must run during startup after stale
// mounted namespaces are torn down and before the new warm pool is populated.
// The returned count is the number of distinct stale Slot identities found.
func (m *CNIManager) reconcileStaleCache(ctx context.Context, namespaceDir string) (int, error) {
	if m == nil || m.backend == nil {
		return 0, fmt.Errorf("cni config not initialized")
	}
	slots, scanErr := m.currentCachedSlots(namespaceDir)
	if len(slots) == 0 {
		return 0, scanErr
	}

	workers := min(cniCacheCleanupWorkers, len(slots))
	jobs := make(chan cniCachedSlot)
	errs := make(chan error, len(slots)+1)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for slot := range jobs {
				if err := ctx.Err(); err != nil {
					errs <- err
					continue
				}
				// DelNetworkList removes the corresponding result cache only
				// after every plugin in the network has completed successfully.
				if err := m.TeardownSandboxNetwork(ctx, slot.containerID, ""); err != nil {
					errs <- fmt.Errorf("delete stale CNI attachment %s: %w", slot.containerID, err)
				}
			}
		}()
	}

sendJobs:
	for _, slot := range slots {
		select {
		case <-ctx.Done():
			errs <- ctx.Err()
			break sendJobs
		case jobs <- slot:
		}
	}
	close(jobs)
	wg.Wait()
	close(errs)

	allErrs := []error{scanErr}
	for err := range errs {
		allErrs = append(allErrs, err)
	}
	return len(slots), errors.Join(allErrs...)
}

func (m *CNIManager) currentCachedSlots(namespaceDir string) ([]cniCachedSlot, error) {
	if strings.TrimSpace(m.bridgeNetworkName) == "" {
		return nil, fmt.Errorf("CNI bridge network name is not initialized")
	}
	attachments, err := m.backend.CachedAttachments()
	if err != nil {
		return nil, fmt.Errorf("list cached CNI attachments: %w", err)
	}

	byContainerID := make(map[string]cniCachedSlot)
	for _, attachment := range attachments {
		if !m.isCurrentConchAttachment(attachment, namespaceDir) {
			continue
		}
		byContainerID[attachment.ContainerID] = cniCachedSlot{containerID: attachment.ContainerID}
	}

	slots := make([]cniCachedSlot, 0, len(byContainerID))
	for _, slot := range byContainerID {
		slots = append(slots, slot)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].containerID < slots[j].containerID })
	return slots, nil
}

func (m *CNIManager) isCurrentConchAttachment(attachment cniAttachment, namespaceDir string) bool {
	rawID := strings.TrimPrefix(attachment.ContainerID, cniContainerIDPrefix)
	id, err := strconv.Atoi(rawID)
	if err != nil || validateSlotID(id) != nil || attachment.ContainerID != cniContainerIDPrefix+strconv.Itoa(id) {
		return false
	}
	if attachment.NetNS != filepath.Join(namespaceDir, networkNamespacePrefix+strconv.Itoa(id)) {
		return false
	}

	return attachment.NetworkName == m.bridgeNetworkName && attachment.InterfaceName == m.ifName
}
