package slot

import (
	"errors"
	"sync"
	"testing"
)

func TestQueueIsBoundedFIFO(t *testing.T) {
	q := NewQueue[int](2)

	if err := q.Push(1); err != nil {
		t.Fatalf("Push(1): %v", err)
	}
	if err := q.Push(2); err != nil {
		t.Fatalf("Push(2): %v", err)
	}
	if err := q.Push(3); !errors.Is(err, ErrFull) {
		t.Fatalf("Push() into full queue error = %v, want ErrFull", err)
	}

	if got, err := q.Pop(); err != nil || got != 1 {
		t.Fatalf("first Pop() = (%d, %v), want (1, nil)", got, err)
	}
	if err := q.Push(3); err != nil {
		t.Fatalf("Push() after Pop(): %v", err)
	}
	if got, err := q.Pop(); err != nil || got != 2 {
		t.Fatalf("second Pop() = (%d, %v), want (2, nil)", got, err)
	}
	if got, err := q.Pop(); err != nil || got != 3 {
		t.Fatalf("third Pop() = (%d, %v), want (3, nil)", got, err)
	}
	if _, err := q.Pop(); !errors.Is(err, ErrEmpty) {
		t.Fatalf("Pop() from empty queue error = %v, want ErrEmpty", err)
	}
}

func TestQueueCloseRejectsPushAndPop(t *testing.T) {
	q := NewQueue[int](1)
	if err := q.Push(1); err != nil {
		t.Fatalf("Push(): %v", err)
	}

	q.Close()
	q.Close()

	if err := q.Push(2); !errors.Is(err, ErrClosed) {
		t.Fatalf("Push() after Close() error = %v, want ErrClosed", err)
	}
	if _, err := q.Pop(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Pop() after Close() error = %v, want ErrClosed", err)
	}
	size, capacity := q.Usage()
	if !q.IsClosed() || size != 1 || capacity != 1 {
		t.Fatalf("queue after Close() = (size=%d, capacity=%d, closed=%v), want (1, 1, true)", size, capacity, q.IsClosed())
	}
}

func TestQueueConcurrentCloseAndPush(t *testing.T) {
	const producers = 128
	q := NewQueue[int](producers)
	start := make(chan struct{})
	errs := make(chan error, producers)

	var wg sync.WaitGroup
	for i := range producers {
		wg.Add(1)
		go func(value int) {
			defer wg.Done()
			<-start
			errs <- q.Push(value)
		}(i)
	}

	close(start)
	q.Close()
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil && !errors.Is(err, ErrClosed) {
			t.Fatalf("concurrent Push() error = %v, want nil or ErrClosed", err)
		}
	}
	size, capacity := q.Usage()
	if !q.IsClosed() || size > capacity {
		t.Fatalf("queue after concurrent Close() = (size=%d, capacity=%d, closed=%v)", size, capacity, q.IsClosed())
	}
}
