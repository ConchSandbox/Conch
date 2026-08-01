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

[MODIFIED] - Changes made on 2025-12-24 by Team conch: Simplify storage interface
*/
package netstack

import (
	"context"
	"errors"
	"fmt"
)

const netNamespacesDir = "/var/run/netns"

var ErrNoAvailableNetworkSlots = errors.New("network slot capacity reached")

type Storage interface {
	Acquire(ctx context.Context) (*Slot, error)
	Claim(s *Slot) error
	Release(s *Slot) error
}

func NewStorage(maxSlotsSize int) (Storage, error) {
	if maxSlotsSize <= 0 {
		return nil, fmt.Errorf("invalid max slots size %d", maxSlotsSize)
	}
	return NewStorageLocal(maxSlotsSize)
}
