package relay

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond until it returns true or the deadline expires.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// mustPublishBuffer publishes a single payload through the channel publisher.
func mustPublishBuffer(t *testing.T, r *Relay, pub chan<- *Buffer, payload []byte) {
	t.Helper()
	buf := r.AcquireBuffer()
	copy(buf.Bytes(), payload)
	buf.Resize(len(payload))
	select {
	case pub <- buf:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout publishing buffer")
	}
}

// readWithin reads one buffer from a subscriber. The third return value is
// false if the read did not finish in time; only the test goroutine may turn
// that into a fatal error.
func readWithin(sub *Subscriber, timeout time.Duration) (*Buffer, error, bool) {
	type result struct {
		buf *Buffer
		err error
	}
	done := make(chan result, 1)
	go func() {
		buf, err := sub.Read()
		done <- result{buf, err}
	}()
	select {
	case res := <-done:
		return res.buf, res.err, true
	case <-time.After(timeout):
		return nil, nil, false
	}
}

// mustRead reads one buffer from a subscriber or fails the test.
func mustRead(t *testing.T, sub *Subscriber, timeout time.Duration) (*Buffer, error) {
	t.Helper()
	buf, err, ok := readWithin(sub, timeout)
	if !ok {
		t.Fatal("subscriber read blocked")
	}
	return buf, err
}

// TestRelay_PublisherCloseReleasesNameSynchronously covers the takeover gap:
// once a publisher is closed, the same name must be claimable immediately,
// without waiting for any background goroutine.
func TestRelay_PublisherCloseReleasesNameSynchronously(t *testing.T) {
	r := NewRelay(&RelayConfig{BufferSize: 64, PacketSize: 4})
	for i := 0; i < 500; i++ {
		pub, err := r.NewPublisher("s")
		if err != nil {
			t.Fatalf("iteration %d: publish: %v", i, err)
		}
		pub.Close()

		// No sleep, no polling: the name has to be free right away.
		takeover, err := r.NewPublisher("s")
		if err != nil {
			t.Fatalf("iteration %d: immediate takeover failed: %v", i, err)
		}
		if !r.ChannelExists("s") {
			t.Fatalf("iteration %d: channel vanished after takeover", i)
		}
		takeover.Close()
	}
}

// TestRelay_PublishChannelTakeoverIsImmediate covers the raw channel API used by
// older callers: close(pub) followed directly by Publish must succeed even
// though the forwarding goroutine may not have run yet.
func TestRelay_PublishChannelTakeoverIsImmediate(t *testing.T) {
	r := NewRelay(&RelayConfig{BufferSize: 64, PacketSize: 4})
	for i := 0; i < 500; i++ {
		pub, err := r.Publish("s")
		if err != nil {
			t.Fatalf("iteration %d: publish: %v", i, err)
		}
		close(pub)
		takeover, err := r.Publish("s")
		if err != nil {
			t.Fatalf("iteration %d: immediate republish failed: %v", i, err)
		}
		close(takeover)
	}
}

// TestRelay_PublisherCloseEndsSubscribers checks that subscribers of a closed
// epoch observe an explicit end of stream rather than blocking forever.
func TestRelay_PublisherCloseEndsSubscribers(t *testing.T) {
	r := NewRelay(&RelayConfig{BufferSize: 64, PacketSize: 4})

	pub, err := r.NewPublisher("s")
	if err != nil {
		t.Fatal(err)
	}
	subs := make([]*Subscriber, 0, 8)
	for i := 0; i < 8; i++ {
		sub, unsub, err := r.Subscribe("s")
		if err != nil {
			t.Fatal(err)
		}
		defer unsub()
		subs = append(subs, sub)
	}

	pub.Write(r.acquireTestBuffer([]byte{1, 2, 3, 4}))
	// The buffer is delivered before the close is observed.
	for _, sub := range subs {
		buf, err := mustRead(t, sub, 5*time.Second)
		if err != nil {
			t.Fatalf("read before close: %v", err)
		}
		buf.Release()
	}

	pub.Close()

	var wg sync.WaitGroup
	for _, sub := range subs {
		wg.Add(1)
		go func(sub *Subscriber) {
			defer wg.Done()
			if _, err := mustRead(t, sub, 5*time.Second); err == nil {
				t.Error("expected end-of-stream error after publisher close")
			}
		}(sub)
	}
	wg.Wait()
}

func (r *Relay) acquireTestBuffer(payload []byte) *Buffer {
	buf := r.AcquireBuffer()
	copy(buf.Bytes(), payload)
	buf.Resize(len(payload))
	return buf
}

// TestRelay_NewEpochNeverLeaksOldData verifies that a subscriber of the new
// publisher epoch cannot receive data written by the previous publisher, and
// that an epoch change resets the client count.
func TestRelay_NewEpochNeverLeaksOldData(t *testing.T) {
	r := NewRelay(&RelayConfig{BufferSize: 64, PacketSize: 4})

	oldPub, err := r.NewPublisher("s")
	if err != nil {
		t.Fatal(err)
	}
	oldSub, oldUnsub, err := r.Subscribe("s")
	if err != nil {
		t.Fatal(err)
	}
	defer oldUnsub()

	// Queue data from the old epoch that the old subscriber has not read yet.
	for i := 0; i < 4; i++ {
		oldPub.Write(r.acquireTestBuffer([]byte{0xAA, 0xAA, 0xAA, 0xAA}))
	}
	oldPub.Close()

	// The old epoch is over, queued data must not be served any more.
	if _, err := mustRead(t, oldSub, 5*time.Second); err == nil {
		t.Fatal("old subscriber received data after epoch end")
	}

	newPub, err := r.NewPublisher("s")
	if err != nil {
		t.Fatal(err)
	}
	newSub, newUnsub, err := r.Subscribe("s")
	if err != nil {
		t.Fatal(err)
	}
	defer newUnsub()

	if _, _, err := r.Subscribe("other"); !errors.Is(err, ErrStreamNotExisting) {
		t.Fatalf("unexpected error for missing stream: %v", err)
	}

	if stats := r.GetStatistics(); len(stats) != 1 || stats[0].Clients != 1 {
		t.Fatalf("statistics after takeover: %+v", stats)
	}

	newPub.Write(r.acquireTestBuffer([]byte{0xBB, 0xBB, 0xBB, 0xBB}))
	buf, err := mustRead(t, newSub, 5*time.Second)
	if err != nil {
		t.Fatalf("new subscriber read: %v", err)
	}
	defer buf.Release()
	for _, b := range buf.Bytes() {
		if b != 0xBB {
			t.Fatalf("new subscriber got stale epoch data: %x", buf.Bytes())
		}
	}
}

// TestRelay_CloseWhileReading concurrency test: publishers disconnect and
// subscribers read at the same time. No buffer may be released twice, readers
// must terminate, and all buffers must return to the pool.
func TestRelay_CloseWhileReading(t *testing.T) {
	const iterations = 150
	const numReaders = 6
	const pubBatches = 16

	for iter := 0; iter < iterations; iter++ {
		// Small subscriber capacity on purpose: this test also covers dropping
		// overflowing subscribers while the publisher is being torn down.
		r := NewRelay(&RelayConfig{BufferSize: 64, PacketSize: 8})

		pub, err := r.NewPublisher("s")
		if err != nil {
			t.Fatalf("iteration %d: %v", iter, err)
		}

		var readers sync.WaitGroup
		unsubs := make([]UnsubscribeFunc, 0, numReaders)
		for i := 0; i < numReaders; i++ {
			sub, unsub, err := r.Subscribe("s")
			if err != nil {
				t.Fatalf("iteration %d: subscribe: %v", iter, err)
			}
			unsubs = append(unsubs, unsub)
			readers.Add(1)
			go func(sub *Subscriber) {
				defer readers.Done()
				for {
					buf, err := sub.Read()
					if err != nil {
						return
					}
					buf.Release()
				}
			}(sub)
		}

		var writers sync.WaitGroup
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := 0; i < pubBatches; i++ {
				buf := r.AcquireBuffer()
				buf.Resize(4)
				copy(buf.Bytes(), []byte{byte(i), 1, 2, 3})
				pub.Write(buf)
			}
		}()

		// Close while the writer and the readers are busy.
		time.Sleep(time.Duration(iter%3) * 50 * time.Microsecond)
		pub.Close()

		writers.Wait()
		done := make(chan struct{})
		go func() { readers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("iteration %d: readers blocked after close", iter)
		}
		for _, unsub := range unsubs {
			unsub()
		}

		if stats := r.GetStatistics(); len(stats) != 0 {
			t.Fatalf("iteration %d: statistics still report %d streams", iter, len(stats))
		}
		if got := r.BuffersInUse(); got != 0 {
			t.Fatalf("iteration %d: %d buffers leaked", iter, got)
		}
	}
}

// TestRelay_UnsubscribeWhilePublishing races unsubscribes against publisher
// writes. Queued buffers of a leaving subscriber must be recycled exactly once.
func TestRelay_UnsubscribeWhilePublishing(t *testing.T) {
	for iter := 0; iter < 100; iter++ {
		r := NewRelay(&RelayConfig{BufferSize: 64, PacketSize: 8})
		pub, err := r.NewPublisher("s")
		if err != nil {
			t.Fatal(err)
		}

		subs := make([]*Subscriber, 0, 8)
		unsubs := make([]UnsubscribeFunc, 0, 8)
		for i := 0; i < 8; i++ {
			sub, unsub, err := r.Subscribe("s")
			if err != nil {
				t.Fatal(err)
			}
			subs = append(subs, sub)
			unsubs = append(unsubs, unsub)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 64; i++ {
				buf := r.AcquireBuffer()
				buf.Resize(4)
				pub.Write(buf)
			}
		}()
		go func() {
			defer wg.Done()
			for _, unsub := range unsubs {
				unsub()
			}
		}()
		wg.Wait()

		for _, sub := range subs {
			// Every subscriber terminates, either by error or by draining.
			for {
				buf, err := sub.Read()
				if err != nil {
					break
				}
				buf.Release()
			}
		}
		pub.Close()
		if got := r.BuffersInUse(); got != 0 {
			t.Fatalf("iteration %d: %d buffers leaked", iter, got)
		}
	}
}

// TestRelay_RapidRepublishConverges hammers a single stream name with very
// short publish/close cycles, with subscribers and statistics readers in
// parallel, and requires the reported state to converge to the actual one.
func TestRelay_RapidRepublishConverges(t *testing.T) {
	r := NewRelay(&RelayConfig{BufferSize: 64, PacketSize: 8})
	const cycles = 400

	var stopReaders atomic.Bool
	var statsWG sync.WaitGroup
	statsWG.Add(1)
	go func() {
		defer statsWG.Done()
		for !stopReaders.Load() {
			for _, st := range r.GetStatistics() {
				if st.Name != "s" {
					t.Errorf("unexpected stream in statistics: %s", st.Name)
				}
				if st.Clients < 0 {
					t.Errorf("negative client count: %d", st.Clients)
				}
			}
		}
	}()

	var readerWG sync.WaitGroup
	for i := 0; i < 4; i++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for !stopReaders.Load() {
				sub, unsub, err := r.Subscribe("s")
				if err != nil {
					continue
				}
				for {
					buf, err := sub.Read()
					if err != nil {
						break
					}
					buf.Release()
				}
				unsub()
			}
		}()
	}

	for i := 0; i < cycles; i++ {
		pub, err := r.NewPublisher("s")
		if err != nil {
			t.Fatalf("cycle %d: publish: %v", i, err)
		}
		buf := r.AcquireBuffer()
		buf.Resize(4)
		pub.Write(buf)
		pub.Close()
	}

	stopReaders.Store(true)
	readerWG.Wait()
	statsWG.Wait()

	if stats := r.GetStatistics(); len(stats) != 0 {
		t.Fatalf("statistics did not converge, still %d streams: %+v", len(stats), stats)
	}
	if r.ChannelExists("s") {
		t.Fatal("closed stream still reported as existing")
	}
	waitFor(t, "buffers to be recycled", func() bool { return r.BuffersInUse() == 0 })
}

// TestRelay_StatisticsOfLiveStream covers the regular path: a live publisher
// with subscribers reports one stream and the correct client count.
func TestRelay_StatisticsOfLiveStream(t *testing.T) {
	r := NewRelay(&RelayConfig{BufferSize: 64, PacketSize: 4})
	pub, err := r.NewPublisher("live")
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	if stats := r.GetStatistics(); len(stats) != 1 || stats[0].Clients != 0 {
		t.Fatalf("unexpected statistics without subscribers: %+v", stats)
	}

	_, unsub1, err := r.Subscribe("live")
	if err != nil {
		t.Fatal(err)
	}
	_, unsub2, err := r.Subscribe("live")
	if err != nil {
		t.Fatal(err)
	}
	if stats := r.GetStatistics(); len(stats) != 1 || stats[0].Clients != 2 {
		t.Fatalf("unexpected statistics with subscribers: %+v", stats)
	}

	unsub1()
	if stats := r.GetStatistics(); len(stats) != 1 || stats[0].Clients != 1 {
		t.Fatalf("unexpected statistics after unsubscribe: %+v", stats)
	}
	unsub2()
}

// TestRelay_ConcurrentTakeoverMissesNoData is the end-to-end consistency check
// between the three event classes: a stream is republished while subscribers of
// the old epoch are still reading. Old subscribers must end, new subscribers
// must receive only new-epoch data.
func TestRelay_ConcurrentTakeoverMissesNoData(t *testing.T) {
	// Generous subscriber capacity: this test must not drop a subscriber as
	// slow, it verifies epoch isolation.
	r := NewRelay(&RelayConfig{BufferSize: 4096, PacketSize: 4})

	oldPub, err := r.NewPublisher("s")
	if err != nil {
		t.Fatal(err)
	}
	oldSub, oldUnsub, err := r.Subscribe("s")
	if err != nil {
		t.Fatal(err)
	}
	defer oldUnsub()

	oldPub.Write(r.acquireTestBuffer([]byte{1, 1, 1, 1}))
	first, err := mustRead(t, oldSub, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first.Release()

	// Start the replacement publisher while the old epoch still exists. This
	// must fail, the name is taken by a live publisher.
	if _, err := r.NewPublisher("s"); !errors.Is(err, ErrStreamAlreadyExists) {
		t.Fatalf("expected %v, got %v", ErrStreamAlreadyExists, err)
	}

	oldPub.Close()

	newPub, err := r.NewPublisher("s")
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	defer newPub.Close()

	if _, err := mustRead(t, oldSub, 5*time.Second); err == nil {
		t.Fatal("old subscriber did not see end of stream")
	}

	newSub, newUnsub, err := r.Subscribe("s")
	if err != nil {
		t.Fatal(err)
	}
	defer newUnsub()

	const packets = 32
	go func() {
		for i := 0; i < packets; i++ {
			buf := r.AcquireBuffer()
			buf.Resize(4)
			for j := range buf.Bytes() {
				buf.Bytes()[j] = 2
			}
			newPub.Write(buf)
		}
	}()

	for i := 0; i < packets; i++ {
		buf, err := mustRead(t, newSub, 5*time.Second)
		if err != nil {
			t.Fatalf("new subscriber read %d: %v", i, err)
		}
		for _, b := range buf.Bytes() {
			if b != 2 {
				buf.Release()
				t.Fatalf("new subscriber received stale data: %x", buf.Bytes())
			}
		}
		buf.Release()
	}
}
