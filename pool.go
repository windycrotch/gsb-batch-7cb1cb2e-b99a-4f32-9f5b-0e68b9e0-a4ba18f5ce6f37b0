package signal

import (
	"sync"
)

// PoolAllocator allows to decrease a number of allocations at runtime.
// Internally it relies on sync.Pool to manage objects in memory.
type PoolAllocator[T SignalTypes] struct {
	pool  *sync.Pool
	alloc Allocator
}

// PoolAlloc returns new PoolAllocator.
func PoolAlloc[T SignalTypes](a Allocator) PoolAllocator[T] {
	return PoolAllocator[T]{
		alloc: a,
		pool: &sync.Pool{
			New: func() any {
				return Alloc[T](a)
			},
		},
	}
}

// Get returns a Buffer from the Pool. The Buffer is always reset to the
// allocated length and capacity and all of its samples are zeroed, so a
// caller can never observe samples written by another goroutine before it
// returned the Buffer to the Pool.
func (p *PoolAllocator[T]) Get() *Buffer[T] {
	b := p.pool.Get().(*Buffer[T])
	// Restore the allocated length and clear every sample within the
	// allocated capacity, including slots beyond the current length.
	b.data = b.data[:p.alloc.Channels*p.alloc.Length]
	b.clear()
	return b
}

// Put returns a Buffer to the Pool. A Buffer whose channels, length or
// capacity do not match the Pool configuration is rejected and never
// enters the Pool, so it cannot pollute buffers handed out later. Put
// reports whether the Buffer was accepted. Accepted buffers are zeroed
// before being pooled.
func (p *PoolAllocator[T]) Put(b *Buffer[T]) bool {
	if b == nil ||
		b.Channels() != p.alloc.Channels ||
		b.Cap() != p.alloc.Capacity*p.alloc.Channels ||
		b.Len() != p.alloc.Length*p.alloc.Channels {
		return false
	}
	b.clear()
	p.pool.Put(b)
	return true
}
