package slot

import (
	"errors"
	"sync"
)

var (
	ErrClosed = errors.New("slot queue is closed")
	ErrEmpty  = errors.New("slot queue is empty")
	ErrFull   = errors.New("slot queue is full")
)

// Queue is a fixed-capacity, concurrency-safe FIFO. Closing it rejects both
// Push and Pop without draining buffered values.
type Queue[T any] struct {
	mu     sync.Mutex
	values []T
	head   int
	size   int
	closed bool
}

func NewQueue[T any](capacity int) *Queue[T] {
	return &Queue[T]{values: make([]T, capacity)}
}

func (q *Queue[T]) Push(value T) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return ErrClosed
	}
	if q.size == len(q.values) {
		return ErrFull
	}

	tail := (q.head + q.size) % len(q.values)
	q.values[tail] = value
	q.size++
	return nil
}

func (q *Queue[T]) Pop() (T, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	var zero T
	if q.closed {
		return zero, ErrClosed
	}
	if q.size == 0 {
		return zero, ErrEmpty
	}

	value := q.values[q.head]
	q.values[q.head] = zero
	q.head = (q.head + 1) % len(q.values)
	q.size--
	return value, nil
}

func (q *Queue[T]) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
}

func (q *Queue[T]) Usage() (size, capacity int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.size, len(q.values)
}

func (q *Queue[T]) IsClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}
