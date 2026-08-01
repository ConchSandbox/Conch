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
*/
package netstack

import "sync"

// warmSlotQueue is a fixed-capacity FIFO owned by Pool. Closing it rejects both
// push and pop without draining buffered slots; Pool persists those slots so
// their underlying network resources can be recovered by the next daemon.
type warmSlotQueue struct {
	mu     sync.Mutex
	slots  []*Slot
	head   int
	size   int
	closed bool
}

func newWarmSlotQueue(capacity int) *warmSlotQueue {
	return &warmSlotQueue{slots: make([]*Slot, capacity)}
}

func (q *warmSlotQueue) push(slot *Slot) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return errWarmPoolClosed
	}
	if q.size == len(q.slots) {
		return errWarmPoolFull
	}

	tail := (q.head + q.size) % len(q.slots)
	q.slots[tail] = slot
	q.size++
	return nil
}

func (q *warmSlotQueue) pop() (*Slot, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return nil, errWarmPoolClosed
	}
	if q.size == 0 {
		return nil, errWarmPoolEmpty
	}

	slot := q.slots[q.head]
	q.slots[q.head] = nil
	q.head = (q.head + 1) % len(q.slots)
	q.size--
	return slot, nil
}

func (q *warmSlotQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
}

func (q *warmSlotQueue) usage() (size, capacity int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.size, len(q.slots)
}

func (q *warmSlotQueue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}
