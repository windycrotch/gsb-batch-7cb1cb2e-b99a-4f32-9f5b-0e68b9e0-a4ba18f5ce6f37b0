package relay

import (
	"sync"
	"testing"
	"time"
)

// TestChannel_CloseReturnsQueuedBuffers verifies that buffers still sitting in a
// subscriber queue are recycled when the channel is closed, not leaked.
func TestChannel_CloseReturnsQueuedBuffers(t *testing.T) {
	pool := NewBufferPool(8)
	ch := NewChannel("test", 16)

	sub, unsub := ch.Sub()
	defer unsub()

	// Queue more buffers than the subscriber will read.
	for i := 0; i < 10; i++ {
		buf := pool.Get()
		buf.Resize(4)
		ch.Pub(buf)
	}
	if got := pool.InUse(); got != 10 {
		t.Fatalf("expected 10 buffers in use after publish, got %d", got)
	}

	ch.Close()

	if got := pool.InUse(); got != 0 {
		t.Errorf("buffers leaked by close: %d still in use", got)
	}
	if _, err := sub.Read(); err == nil {
		t.Error("subscriber should report closed upstream after Close")
	}
	// Unsubscribing after close is a no-op and must not double-release.
	unsub()
	if got := pool.InUse(); got != 0 {
		t.Errorf("buffers leaked by late unsubscribe: %d still in use", got)
	}
}

// TestChannel_UnsubscribeReturnsQueuedBuffers verifies the same accounting for a
// plain unsubscribe while the channel keeps running.
func TestChannel_UnsubscribeReturnsQueuedBuffers(t *testing.T) {
	pool := NewBufferPool(8)
	ch := NewChannel("test", 16)
	defer ch.Close()

	_, unsubA := ch.Sub()
	subB, unsubB := ch.Sub()

	for i := 0; i < 8; i++ {
		buf := pool.Get()
		buf.Resize(4)
		ch.Pub(buf)
	}
	// Eight distinct buffers are held, each referenced by both subscribers.
	if got := pool.InUse(); got != 8 {
		t.Fatalf("expected 8 buffers in use, got %d", got)
	}

	// A leaves with 8 queued references; B still holds one reference each.
	unsubA()
	if got := pool.InUse(); got != 8 {
		t.Fatalf("expected 8 buffers still in use after A unsubscribed, got %d", got)
	}

	// B drains the rest and releases them.
	for i := 0; i < 8; i++ {
		buf, err := subB.Read()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		buf.Release()
	}
	if got := pool.InUse(); got != 0 {
		t.Fatalf("expected no buffers in use after drain, got %d", got)
	}
	unsubB()
}

// TestChannel_CloseWhileReadingNoDoubleRelease hammers close against reader
// release. Any double release panics inside Buffer.Release.
func TestChannel_CloseWhileReadingNoDoubleRelease(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		pool := NewBufferPool(8)
		ch := NewChannel("test", 4)

		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			sub, unsub := ch.Sub()
			wg.Add(1)
			go func(sub *Subscriber, unsub UnsubscribeFunc) {
				defer wg.Done()
				defer unsub()
				for {
					buf, err := sub.Read()
					if err != nil {
						return
					}
					buf.Release()
				}
			}(sub, unsub)
		}

		published := make(chan struct{})
		go func() {
			defer close(published)
			for i := 0; i < 12; i++ {
				buf := pool.Get()
				buf.Resize(4)
				ch.Pub(buf)
			}
			ch.Close()
		}()

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("iteration %d: readers blocked after close", iter)
		}
		// Only account buffers once the publisher has finished: it would
		// otherwise still hold or be about to publish one.
		<-published
		if got := pool.InUse(); got != 0 {
			t.Fatalf("iteration %d: %d buffers leaked", iter, got)
		}
	}
}

// TestChannel_DoubleCloseIsIdempotent guards the closeOnce path, which matters
// when retirement happens from several places (Close, takeover, reaping).
func TestChannel_DoubleCloseIsIdempotent(t *testing.T) {
	ch := NewChannel("test", 4)
	sub, unsub := ch.Sub()
	ch.Close()
	ch.Close()
	unsub()
	if _, err := sub.Read(); err == nil {
		t.Error("subscriber should be closed")
	}
}

// TestChannel_SubAfterClose checks that a subscription racing with Close cannot
// attach to the dead epoch; it gets an immediately finished subscriber.
func TestChannel_SubAfterClose(t *testing.T) {
	ch := NewChannel("test", 4)
	ch.Close()

	sub, unsub := ch.Sub()
	defer unsub()
	if _, err := sub.Read(); err == nil {
		t.Fatal("subscriber created after close should report closed upstream")
	}
	if got := ch.Stats().clients; got != 0 {
		t.Errorf("closed channel should report 0 clients, got %d", got)
	}
}

// TestChannel_SubAbandonedIsRecycled verifies that closing the channel returns
// buffers to the pool through the pool that created them.
func TestChannel_SubAbandonedIsRecycled(t *testing.T) {
	pool := NewBufferPool(8)
	ch := NewChannel("test", 8)
	_, unsub := ch.Sub()

	buf := pool.Get()
	buf.Resize(2)
	ch.Pub(buf)
	if got := pool.InUse(); got != 1 {
		t.Fatalf("expected 1 buffer in use, got %d", got)
	}

	unsub()
	if got := pool.InUse(); got != 0 {
		t.Fatalf("expected 0 buffers in use, got %d", got)
	}
	ch.Close()
}
