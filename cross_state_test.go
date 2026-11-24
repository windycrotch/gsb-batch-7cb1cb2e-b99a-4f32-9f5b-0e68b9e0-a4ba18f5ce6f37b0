package signal_test

import (
	"sync"
	"testing"

	"pipelined.dev/signal"
)

// interleaved builds an interleaved buffer where each sample encodes its
// position as channel*100 + frame, making channel ownership of every
// sample easy to verify.
func interleaved[T signal.SignalTypes](channels, length, capacity int, valueOf func(c, frame int) T) *signal.Buffer[T] {
	b := signal.Alloc[T](signal.Allocator{
		Channels: channels,
		Length:   length,
		Capacity: capacity,
	})
	for c := 0; c < channels; c++ {
		for frame := 0; frame < length; frame++ {
			b.SetSample(b.BufferIndex(c, frame), valueOf(c, frame))
		}
	}
	return b
}

func intValue(c, frame int) int {
	return c*100 + frame
}

// TestSliceChannelLayout verifies for several channel counts that slicing
// preserves channel ownership of every sample and reports single-channel
// length/capacity.
func TestSliceChannelLayout(t *testing.T) {
	channelsCases := []int{1, 2, 3, 5, 7}
	for _, channels := range channelsCases {
		channels := channels
		t.Run("channels", func(t *testing.T) {
			const length, capacity = 6, 10
			input := interleaved[int](channels, length, capacity, intValue)

			s := input.Slice(2, 5)
			assertEqual(t, "sliced length per channel", s.Length(), 3)
			assertEqual(t, "sliced capacity per channel", s.Capacity(), capacity-2)
			assertEqual(t, "sliced total length", s.Len(), 3*channels)
			assertEqual(t, "sliced total capacity", s.Cap(), (capacity-2)*channels)
			assertEqual(t, "channels", s.Channels(), channels)

			// every sample must still belong to its original channel
			for c := 0; c < channels; c++ {
				for frame := 0; frame < s.Length(); frame++ {
					want := intValue(c, frame+2)
					got := s.Sample(s.BufferIndex(c, frame))
					assertEqual(t, "sample channel ownership", got, want)
				}
			}

			// a sliced view shares storage: writes through the slice are
			// visible through the parent at the matching channel/frame
			s.SetSample(s.BufferIndex(1%channels, 0), -1)
			assertEqual(t, "shared backing array",
				input.Sample(input.BufferIndex(1%channels, 2)), -1)
		})
	}
}

// TestSliceFromStartKeepsFullCapacity verifies slicing at position 0 keeps
// the original capacity, while slicing further reduces it exactly by the
// skipped number of frames.
func TestSliceCapacitySemantics(t *testing.T) {
	const channels, length, capacity = 4, 4, 8
	input := signal.Alloc[int](signal.Allocator{
		Channels: channels,
		Length:   length,
		Capacity: capacity,
	})
	full := input.Slice(0, length)
	assertEqual(t, "full slice capacity", full.Capacity(), capacity)
	assertEqual(t, "full slice total capacity", full.Cap(), capacity*channels)

	half := input.Slice(2, 4)
	assertEqual(t, "half slice capacity", half.Capacity(), capacity-2)
	assertEqual(t, "half slice total capacity", half.Cap(), (capacity-2)*channels)
}

// TestAppendKeepsChannelLayout appends a multi-channel buffer into another
// one whose capacity is sufficient and verifies no sample shifts channels.
func TestAppendKeepsChannelLayout(t *testing.T) {
	const channels = 3
	dst := interleaved[int](channels, 2, 8, intValue)
	src := interleaved[int](channels, 3, 3, func(c, frame int) int {
		return 1000 + c*100 + frame
	})

	dst.Append(src)

	assertEqual(t, "length per channel", dst.Length(), 5)
	assertEqual(t, "total length", dst.Len(), 5*channels)
	if dst.Cap()%channels != 0 {
		t.Fatalf("capacity %d not aligned to %d channels", dst.Cap(), channels)
	}
	for c := 0; c < channels; c++ {
		for frame := 0; frame < 5; frame++ {
			var want int
			if frame < 2 {
				want = intValue(c, frame)
			} else {
				want = 1000 + c*100 + (frame - 2)
			}
			assertEqual(t, "appended sample ownership",
				dst.Sample(dst.BufferIndex(c, frame)), want)
		}
	}
}

// TestAppendGrowsWithAlignedCapacity forces a grow and checks that every
// source sample is preserved, channel placement is intact and the grown
// capacity is a multiple of the channel count.
func TestAppendGrowsWithAlignedCapacity(t *testing.T) {
	for _, channels := range []int{1, 2, 3, 6} {
		channels := channels
		t.Run("channels", func(t *testing.T) {
			// destination exactly fits its own length, no room to append
			dst := interleaved[int](channels, 2, 2, intValue)
			src := interleaved[int](channels, 4, 4, func(c, frame int) int {
				return 500 + c*100 + frame
			})

			dst.Append(src)

			assertEqual(t, "length per channel", dst.Length(), 6)
			assertEqual(t, "total length", dst.Len(), 6*channels)
			if r := dst.Cap() % channels; r != 0 {
				t.Fatalf("grown capacity %d misaligned for %d channels", dst.Cap(), channels)
			}
			if dst.Cap() < dst.Len() {
				t.Fatalf("grown capacity %d smaller than length %d", dst.Cap(), dst.Len())
			}
			for c := 0; c < channels; c++ {
				for frame := 0; frame < 6; frame++ {
					var want int
					if frame < 2 {
						want = intValue(c, frame)
					} else {
						want = 500 + c*100 + (frame - 2)
					}
					assertEqual(t, "grown append sample ownership",
						dst.Sample(dst.BufferIndex(c, frame)), want)
				}
			}
		})
	}
}

// TestAppendOnlyValidLength fills a buffer's capacity tail with sentinel
// values, then appends it: only samples within the valid length may be
// copied, the capacity tail must never leak into the destination.
func TestAppendOnlyValidLength(t *testing.T) {
	const channels = 2
	src := signal.Alloc[int](signal.Allocator{
		Channels: channels,
		Length:   1,
		Capacity: 4,
	})
	src.SetSample(src.BufferIndex(0, 0), 7)
	src.SetSample(src.BufferIndex(1, 0), 9)
	// poison capacity slots beyond the valid length
	for src.Length() < src.Capacity() {
		src.AppendSample(-999)
	}
	// restore the valid length; the poisoned slots remain in the capacity tail
	src = src.Slice(0, 1)

	dst := signal.Alloc[int](signal.Allocator{
		Channels: channels,
		Length:   1,
		Capacity: 8,
	})
	dst.Append(src)

	assertEqual(t, "length per channel", dst.Length(), 2)
	assertEqual(t, "channel 0 frame 1", dst.Sample(dst.BufferIndex(0, 1)), 7)
	assertEqual(t, "channel 1 frame 1", dst.Sample(dst.BufferIndex(1, 1)), 9)
	for frame := dst.Length(); frame < dst.Capacity(); frame++ {
		tail := dst.Slice(frame, frame+1)
		for c := 0; c < channels; c++ {
			assertEqual(t, "no tail leak",
				tail.Sample(tail.BufferIndex(c, 0)), 0)
		}
	}
}

// TestAppendSlicedBuffer reproduces the slice -> append cross-state: the
// sliced buffer must carry its own length/capacity semantics into Append.
func TestAppendSlicedBuffer(t *testing.T) {
	const channels = 3
	input := interleaved[int](channels, 6, 6, intValue)
	part := input.Slice(2, 5)

	dst := signal.Alloc[int](signal.Allocator{
		Channels: channels,
		Length:   0,
		Capacity: 8,
	})
	dst.Append(part)

	assertEqual(t, "length per channel", dst.Length(), 3)
	for c := 0; c < channels; c++ {
		for frame := 0; frame < 3; frame++ {
			assertEqual(t, "sliced append ownership",
				dst.Sample(dst.BufferIndex(c, frame)), intValue(c, frame+2))
		}
	}
}

// TestAppendGrowsUnalignedLength covers the growth rounding branch: a
// buffer whose interleaved length is not a whole number of frames is
// appended to and forced to grow; the grown capacity is rounded up to the
// next channel-aligned size and no sample is lost.
func TestAppendGrowsUnalignedLength(t *testing.T) {
	const channels = 3
	dst := signal.Alloc[int](signal.Allocator{
		Channels: channels,
		Length:   0,
		Capacity: 2,
	})
	// reachable half-frame state through the public AppendSample API
	dst.AppendSample(1)
	dst.AppendSample(2) // len is now 2, not a multiple of 3

	src := interleaved[int](channels, 5, 5, func(c, frame int) int {
		return 900 + c*100 + frame
	})
	dst.Append(src)

	if r := dst.Cap() % channels; r != 0 {
		t.Fatalf("grown capacity %d not aligned to %d channels", dst.Cap(), channels)
	}
	if dst.Cap() < dst.Len() {
		t.Fatalf("grown capacity %d below length %d", dst.Cap(), dst.Len())
	}
	// the two pre-existing samples are preserved
	assertEqual(t, "pre-existing sample 0", dst.Sample(0), 1)
	assertEqual(t, "pre-existing sample 1", dst.Sample(1), 2)
	// every appended frame keeps its channel ownership
	for c := 0; c < channels; c++ {
		for frame := 0; frame < 5; frame++ {
			assertEqual(t, "unaligned-grow ownership",
				dst.Sample(dst.BufferIndex(c, frame)+2),
				900+c*100+frame)
		}
	}
}

// TestChannelViewIsolation verifies that reads and writes through a single
// channel view land only on that channel's own frames, for every channel
// of a multi-channel buffer.
func TestChannelViewIsolation(t *testing.T) {
	const channels, length, capacity = 4, 5, 5

	for c := 0; c < channels; c++ {
		// fresh buffer per channel so writes from previous iterations
		// cannot influence neighbour assertions
		buf := interleaved[int](channels, length, capacity, intValue)
		view := buf.Channel(c)
		assertEqual(t, "view channels", view.Channels(), 1)
		assertEqual(t, "view length", view.Length(), length)
		assertEqual(t, "view capacity", view.Capacity(), capacity)
		for frame := 0; frame < length; frame++ {
			assertEqual(t, "view read isolation",
				view.Sample(frame), intValue(c, frame))
			assertEqual(t, "view index equals buffer index",
				view.BufferIndex(0, frame), buf.BufferIndex(c, frame))
		}

		// write through the view and ensure only this channel changes
		view.SetSample(2, 42)
		assertEqual(t, "view write lands on channel",
			buf.Sample(buf.BufferIndex(c, 2)), 42)
		for other := 0; other < channels; other++ {
			if other == c {
				continue
			}
			assertEqual(t, "neighbour channel untouched",
				buf.Sample(buf.BufferIndex(other, 2)), intValue(other, 2))
		}
	}
}

// TestChannelViewOnSlice verifies a channel view of a sliced buffer maps
// to the sliced frames, not the parent's original frame positions.
func TestChannelViewOnSlice(t *testing.T) {
	const channels = 3
	input := interleaved[int](channels, 6, 8, intValue)
	s := input.Slice(2, 5)
	view := s.Channel(2)
	for frame := 0; frame < view.Length(); frame++ {
		assertEqual(t, "sliced view read",
			view.Sample(frame), intValue(2, frame+2))
	}
	view.SetSample(1, 77)
	assertEqual(t, "sliced view write",
		input.Sample(input.BufferIndex(2, 3)), 77)
}

// TestPoolGetResetsState covers the recycle path: a buffer used, mutated
// in length and contents, then returned, must come back with the allocated
// length/capacity and fully zeroed samples.
func TestPoolGetResetsState(t *testing.T) {
	alloc := signal.Allocator{Channels: 3, Length: 4, Capacity: 6}
	pool := signal.PoolAlloc[int](alloc)

	b := pool.Get()
	assertEqual(t, "initial length", b.Length(), alloc.Length)
	assertEqual(t, "initial capacity", b.Capacity(), alloc.Capacity)
	// poison the contents while keeping the allocated geometry untouched
	for i := 0; i < b.Len(); i++ {
		b.SetSample(i, i+1)
	}
	if !pool.Put(b) {
		t.Fatal("buffer matching pool configuration must be accepted")
	}

	again := pool.Get()
	assertEqual(t, "recycled channels", again.Channels(), alloc.Channels)
	assertEqual(t, "recycled length", again.Length(), alloc.Length)
	assertEqual(t, "recycled total length", again.Len(), alloc.Channels*alloc.Length)
	assertEqual(t, "recycled capacity", again.Capacity(), alloc.Capacity)
	assertEqual(t, "recycled total capacity", again.Cap(), alloc.Channels*alloc.Capacity)
	for i := 0; i < again.Len(); i++ {
		assertEqual(t, "recycled sample zeroed", again.Sample(i), 0)
	}
}

// TestPoolGetZeroesSamples ensures written samples cannot survive a
// round-trip through the pool: Put clears the whole backing array before
// the buffer is reused.
func TestPoolGetZeroesSamples(t *testing.T) {
	alloc := signal.Allocator{Channels: 2, Length: 4, Capacity: 4}
	pool := signal.PoolAlloc[int](alloc)

	b := pool.Get()
	for i := 0; i < b.Len(); i++ {
		b.SetSample(i, 123)
	}
	if !pool.Put(b) {
		t.Fatal("buffer must be accepted")
	}

	again := pool.Get()
	for i := 0; i < again.Len(); i++ {
		assertEqual(t, "sample zeroed after recycle", again.Sample(i), 0)
	}
}

// TestPoolRejectsGrownBuffer verifies that a buffer grown beyond its
// configured length cannot be returned to the pool, so it can never corrupt
// buffers handed out to later Get calls.
func TestPoolRejectsGrownBuffer(t *testing.T) {
	alloc := signal.Allocator{Channels: 2, Length: 1, Capacity: 4}
	pool := signal.PoolAlloc[int](alloc)

	grown := pool.Get()
	for grown.Length() < grown.Capacity() {
		grown.AppendSample(7)
	}
	if pool.Put(grown) {
		t.Fatal("grown buffer must be rejected")
	}

	// the rejected buffer never entered the pool: subsequent Get calls keep
	// returning clean, correctly configured buffers.
	fresh := pool.Get()
	assertEqual(t, "fresh buffer length", fresh.Length(), alloc.Length)
	assertEqual(t, "fresh buffer capacity", fresh.Capacity(), alloc.Capacity)
	for i := 0; i < fresh.Len(); i++ {
		assertEqual(t, "fresh buffer zeroed", fresh.Sample(i), 0)
	}
}

// TestPoolPutRejectsMismatchedBuffers ensures buffers whose configuration
// differs from the pool are rejected instead of polluting it.
func TestPoolPutRejectsMismatchedBuffers(t *testing.T) {
	alloc := signal.Allocator{Channels: 2, Length: 2, Capacity: 4}
	pool := signal.PoolAlloc[int](alloc)

	t.Run("nil", func(t *testing.T) {
		if pool.Put(nil) {
			t.Fatal("nil buffer must be rejected")
		}
	})
	t.Run("different channels", func(t *testing.T) {
		wrong := signal.Alloc[int](signal.Allocator{Channels: 3, Length: 2, Capacity: 4})
		if pool.Put(wrong) {
			t.Fatal("buffer with different channels must be rejected")
		}
	})
	t.Run("different capacity", func(t *testing.T) {
		wrong := signal.Alloc[int](signal.Allocator{Channels: 2, Length: 2, Capacity: 5})
		if pool.Put(wrong) {
			t.Fatal("buffer with different capacity must be rejected")
		}
	})
	t.Run("different length", func(t *testing.T) {
		wrong := signal.Alloc[int](signal.Allocator{Channels: 2, Length: 3, Capacity: 4})
		if pool.Put(wrong) {
			t.Fatal("buffer with different length must be rejected")
		}
	})
	t.Run("sliced buffer", func(t *testing.T) {
		good := signal.Alloc[int](alloc)
		s := good.Slice(0, 1)
		if pool.Put(s) {
			t.Fatal("sliced buffer with reduced capacity must be rejected")
		}
	})

	// rejected buffers must not enter the pool: every Get returns a clean,
	// correctly configured buffer.
	for i := 0; i < 5; i++ {
		b := pool.Get()
		assertEqual(t, "pool unpolluted channels", b.Channels(), alloc.Channels)
		assertEqual(t, "pool unpolluted length", b.Length(), alloc.Length)
		assertEqual(t, "pool unpolluted capacity", b.Capacity(), alloc.Capacity)
		for j := 0; j < b.Len(); j++ {
			assertEqual(t, "pool unpolluted samples", b.Sample(j), 0)
		}
		if !pool.Put(b) {
			t.Fatal("matching buffer must be accepted")
		}
	}
}

// TestPoolConcurrent exercises concurrent Get/Put: every buffer handed out
// must be zeroed and correctly sized, i.e. no goroutine can observe data
// another goroutine has not yet returned.
func TestPoolConcurrent(t *testing.T) {
	alloc := signal.Allocator{Channels: 4, Length: 8, Capacity: 16}
	pool := signal.PoolAlloc[int64](alloc)

	const goroutines, iterations = 16, 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for n := 0; n < iterations; n++ {
				b := pool.Get()
				if b.Channels() != alloc.Channels ||
					b.Length() != alloc.Length ||
					b.Capacity() != alloc.Capacity {
					t.Errorf("unexpected buffer geometry: channels=%d length=%d capacity=%d",
						b.Channels(), b.Length(), b.Capacity())
					return
				}
				for i := 0; i < b.Len(); i++ {
					if b.Sample(i) != 0 {
						t.Errorf("buffer not zeroed on Get at index %d", i)
						return
					}
					b.SetSample(i, int64(n+1))
				}
				if !pool.Put(b) {
					t.Error("valid buffer rejected by Put")
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
}
