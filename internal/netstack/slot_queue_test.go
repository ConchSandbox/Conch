package netstack

import (
	"errors"
	"sync"
	"testing"
)

func TestWarmSlotQueueIsBoundedFIFO(t *testing.T) {
	q := newWarmSlotQueue(2)
	first := &Slot{Key: "first"}
	second := &Slot{Key: "second"}
	third := &Slot{Key: "third"}

	if err := q.push(first); err != nil {
		t.Fatalf("push first slot: %v", err)
	}
	if err := q.push(second); err != nil {
		t.Fatalf("push second slot: %v", err)
	}
	if err := q.push(third); !errors.Is(err, errWarmPoolFull) {
		t.Fatalf("push into full queue error = %v, want errWarmPoolFull", err)
	}

	if got, err := q.pop(); err != nil || got != first {
		t.Fatalf("first pop = (%#v, %v), want first slot", got, err)
	}
	if err := q.push(third); err != nil {
		t.Fatalf("push after pop: %v", err)
	}
	if got, err := q.pop(); err != nil || got != second {
		t.Fatalf("second pop = (%#v, %v), want second slot", got, err)
	}
	if got, err := q.pop(); err != nil || got != third {
		t.Fatalf("third pop = (%#v, %v), want third slot", got, err)
	}
	if _, err := q.pop(); !errors.Is(err, errWarmPoolEmpty) {
		t.Fatalf("pop empty queue error = %v, want errWarmPoolEmpty", err)
	}
}

func TestWarmSlotQueueCloseRejectsPushAndPop(t *testing.T) {
	q := newWarmSlotQueue(1)
	if err := q.push(&Slot{Key: "buffered"}); err != nil {
		t.Fatalf("push buffered slot: %v", err)
	}

	q.close()
	q.close()

	if err := q.push(&Slot{Key: "late"}); !errors.Is(err, errWarmPoolClosed) {
		t.Fatalf("push after close error = %v, want errWarmPoolClosed", err)
	}
	if _, err := q.pop(); !errors.Is(err, errWarmPoolClosed) {
		t.Fatalf("pop after close error = %v, want errWarmPoolClosed", err)
	}
	size, capacity := q.usage()
	closed := q.isClosed()
	if size != 1 || capacity != 1 || !closed {
		t.Fatalf("queue after close = (size=%d, capacity=%d, closed=%v), want (1, 1, true)", size, capacity, closed)
	}
}

func TestWarmSlotQueueConcurrentCloseAndPush(t *testing.T) {
	const producers = 128
	q := newWarmSlotQueue(producers)
	start := make(chan struct{})
	errs := make(chan error, producers)

	var wg sync.WaitGroup
	for i := 0; i < producers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			errs <- q.push(&Slot{Key: string(rune(idx))})
		}(i)
	}

	close(start)
	q.close()
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil && !errors.Is(err, errWarmPoolClosed) {
			t.Fatalf("concurrent push error = %v, want nil or errWarmPoolClosed", err)
		}
	}
	size, capacity := q.usage()
	closed := q.isClosed()
	if size > capacity || !closed {
		t.Fatalf("queue after concurrent close = (size=%d, capacity=%d, closed=%v)", size, capacity, closed)
	}
}
