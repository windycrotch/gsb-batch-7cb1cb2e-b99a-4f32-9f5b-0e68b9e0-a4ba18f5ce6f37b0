package relay

import (
	"fmt"
	"sync"
	"sync/atomic"
)

type BufferPool struct {
	size int
	pool sync.Pool

	// inUse counts buffers that are currently handed out. It never influences
	// pooling behaviour, it only makes buffer accounting observable for tests
	// and diagnostics.
	inUse atomic.Int64
}

func NewBufferPool(size uint) *BufferPool {
	return &BufferPool{size: int(size)}
}

// InUse returns the number of buffers that have been acquired but not yet
// released. A quiescent relay with no active streams must report zero.
func (p *BufferPool) InUse() int64 {
	return p.inUse.Load()
}

func (p *BufferPool) Get() *Buffer {
	p.inUse.Add(1)
	if value := p.pool.Get(); value != nil {
		buf := value.(*Buffer)
		buf.refs.Store(1)
		buf.data = buf.data[:p.size]
		return buf
	}

	buf := &Buffer{
		pool: p,
		data: make([]byte, p.size),
	}
	buf.refs.Store(1)
	return buf
}

func (p *BufferPool) put(buf *Buffer) {
	buf.data = buf.data[:p.size]
	p.inUse.Add(-1)
	p.pool.Put(buf)
}

type Buffer struct {
	pool *BufferPool
	data []byte
	refs atomic.Int32
}

func (b *Buffer) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.data
}

func (b *Buffer) Resize(size int) []byte {
	if size > cap(b.data) {
		panic(fmt.Sprintf("relay buffer resize %d exceeds capacity %d", size, cap(b.data)))
	}
	b.data = b.data[:size]
	return b.data
}

func (b *Buffer) Retain() {
	// refs should never drop below 1 before a retain
	// that implies a race condition
	if b.refs.Add(1) <= 1 {
		panic("retain on released relay buffer")
	}
}

func (b *Buffer) Release() {
	if b == nil {
		return
	}

	refs := b.refs.Add(-1)
	if refs < 0 {
		panic("release on recycled relay buffer")
	}
	if refs == 0 {
		b.pool.put(b)
	}
}
