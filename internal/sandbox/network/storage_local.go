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

[MODIFIED] - Changes made on 2025-12-24 by Team conch: Refactored storage local module
*/
package network

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type StorageLocal struct {
	// the netns map of already created in host
	hostNs       map[string]struct{}
	maxSlotSize  int
	acquiredNs   map[string]struct{}
	acquiredNsMu sync.Mutex
}

func NewStorageLocal(maxSlotsSize int) (Storage, error) {
	hostNs, err := getForeignNamespaces()
	if err != nil {
		return nil, fmt.Errorf("error getting already used namespaces: %w", err)
	}
	hostNsMap := make(map[string]struct{})
	for _, ns := range hostNs {
		hostNsMap[ns] = struct{}{}
	}
	return &StorageLocal{
		hostNs:       hostNsMap,
		maxSlotSize:  maxSlotsSize,
		acquiredNs:   make(map[string]struct{}, maxSlotsSize),
		acquiredNsMu: sync.Mutex{},
	}, nil
}

// get host already exist namespaces
func getForeignNamespaces() ([]string, error) {
	var ns []string
	files, err := os.ReadDir(netNamespacesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return ns, nil
		}
		return nil, fmt.Errorf("error reading netns directory: %w", err)
	}
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		name := file.Name()
		if name == "host" {
			continue
		}
		ns = append(ns, name)
	}

	return ns, nil
}

func (s *StorageLocal) Acquire(ctx context.Context) (*Slot, error) {
	acquireTimeoutCtx, acquireCancel := context.WithTimeout(ctx, time.Millisecond*500)
	defer acquireCancel()

	s.acquiredNsMu.Lock()
	defer s.acquiredNsMu.Unlock()

	slotIdx := 1
	for {
		select {
		case <-acquireTimeoutCtx.Done():
			return nil, fmt.Errorf("failed to acquire IP slot: timeout")
		default:
			if len(s.acquiredNs) > s.maxSlotSize {
				return nil, fmt.Errorf("failed to acquire IP slot: no empty slots found")
			}

			slotIdx++
			slotName := getSlotName(slotIdx)
			// check if the netns  is already created in host
			if _, found := s.hostNs[slotName]; found {
				continue
			}
			// check if the netns is already acquired by sandbox
			if _, found := s.acquiredNs[slotName]; found {
				continue
			}
			// check if the slot can be acquired
			available, err := isNamespaceAvailable(slotName)
			if err != nil {
				return nil, fmt.Errorf("error checking if namespace is available: %w", err)
			}

			if !available {
				s.hostNs[slotName] = struct{}{}
				continue
			}

			slotKey := getLocalKey(slotIdx)
			slot, err := NewSlot(slotKey, slotIdx)
			if err == nil {
				s.acquiredNs[slotName] = struct{}{}
			}
			return slot, err
		}
	}
}

func getSlotName(slotIdx int) string {
	slotIdxStr := strconv.Itoa(slotIdx)
	return fmt.Sprintf("ns-%s", slotIdxStr)
}

func getLocalKey(slotIdx int) string {
	return strconv.Itoa(slotIdx)
}

func isNamespaceAvailable(name string) (bool, error) {
	nsPath := filepath.Join(netNamespacesDir, name)
	_, err := os.Stat(nsPath)

	if os.IsNotExist(err) {
		// Namespace does not exist, so it's available
		return true, nil
	} else if err != nil {
		// Some other error
		return false, err
	}

	// File exists so namespace is in use.
	return false, nil
}

func (s *StorageLocal) Release(ips *Slot) error {
	s.acquiredNsMu.Lock()
	defer s.acquiredNsMu.Unlock()
	slotName := getSlotName(ips.Idx)
	delete(s.acquiredNs, slotName)

	return nil
}
