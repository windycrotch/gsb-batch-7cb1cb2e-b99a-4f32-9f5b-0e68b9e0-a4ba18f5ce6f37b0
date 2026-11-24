// Copyright 2026 The Oto Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mux_test

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ebitengine/oto/v3/internal/mux"
)

// A PauseAndStopReading that is waiting for an in-flight Read must not return
// while a Seek that began during the wait is still seeking the source: the
// source must not be both read and sought, and the control must not overtake
// the Seek.
func TestPauseAndStopReadingWaitsForSeekDuringRead(t *testing.T) {
	// The source fills the buffer once, then blocks a Read until released, and
	// blocks Seek until released, recording whether Read and Seek overlapped.
	src := &seekingReader{
		readBeforeBlocking: true,
		gate:               make(chan struct{}),
		began:              make(chan struct{}),
		seekBegan:          make(chan struct{}),
		seekGate:           make(chan struct{}),
	}
	m := mux.New(48000, 2, mux.FormatSignedInt16LE)
	defer m.Stop()
	p := newPlayer(t, m, src)
	var readRelease, seekRelease sync.Once
	unblockRead := func() { readRelease.Do(func() { close(src.gate) }) }
	unblockSeek := func() { seekRelease.Do(func() { close(src.seekGate) }) }
	defer unblockSeek()
	defer unblockRead()

	p.SetBufferSize(64)
	p.Play()
	// Fill the buffer, then consume a little so the next Read from the source
	// starts and blocks. Seek's discarding the buffer is then observable.
	waitForBufferedSize(t, p, 64)
	m.ReadFloat32s(make([]float32, 2))
	<-src.began

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		p.PauseAndStopReading()
	}()

	seekDone := make(chan struct{})
	go func() {
		defer close(seekDone)
		if _, err := p.Seek(0, io.SeekStart); err != nil {
			t.Error(err)
		}
	}()

	// Seek sets the buffer aside before waiting for the in-flight Read, so a zero
	// BufferedSize shows Seek is now waiting for it (seeking is already set).
	deadline := time.Now().Add(time.Second)
	for p.BufferedSize() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("Seek did not wait for the in-flight Read")
		}
		time.Sleep(time.Millisecond)
	}
	// Finish the Read so that both waiters wake up.
	unblockRead()

	select {
	case <-src.seekBegan:
	case <-time.After(time.Second):
		t.Fatal("the source Seek did not begin")
	}

	// The Seek is now blocked in the source. PauseAndStopReading must wait.
	select {
	case <-stopped:
		t.Error("PauseAndStopReading returned while a source Seek was in flight")
	case <-time.After(50 * time.Millisecond):
	}

	unblockSeek()
	for _, done := range []chan struct{}{stopped, seekDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("an operation did not finish after the source Seek returned")
		}
	}
	if src.overlapped.Load() {
		t.Error("the source was read and sought at the same time")
	}
}

// errSeeker returns a ramp of data and always fails Seek.
type errSeeker struct {
	err error

	m   sync.Mutex
	pos int
}

func (s *errSeeker) Read(buf []byte) (int, error) {
	s.m.Lock()
	defer s.m.Unlock()
	for i := range buf {
		buf[i] = byte(s.pos + i)
	}
	s.pos += len(buf)
	return len(buf), nil
}

func (s *errSeeker) Seek(int64, int) (int64, error) { return 0, s.err }

// A failed Seek must leave the player's buffer, playing state, and subsequent
// controls usable.
func TestFailedSeekKeepsPlayerUsable(t *testing.T) {
	errSeek := errors.New("seek failed")

	for _, playing := range []bool{false, true} {
		t.Run(func() string {
			if playing {
				return "playing"
			}
			return "paused"
		}(), func(t *testing.T) {
			src := &errSeeker{err: errSeek}
			m := mux.New(48000, 1, mux.FormatUnsignedInt8)
			defer m.Stop()
			p := newPlayer(t, m, src)
			p.SetBufferSize(64)
			p.Play()
			waitForBufferedSize(t, p, 64)
			if !playing {
				p.PauseAndStopReading()
			}
			before := p.BufferedSize()

			if _, err := p.Seek(0, io.SeekStart); !errors.Is(err, errSeek) {
				t.Fatalf("Seek error: got %v; want %v", err, errSeek)
			}
			if got := p.BufferedSize(); got != before {
				t.Errorf("BufferedSize after a failed Seek: got %d; want %d", got, before)
			}
			if got := p.IsPlaying(); got != playing {
				t.Errorf("IsPlaying after a failed Seek: got %v; want %v", got, playing)
			}

			// Subsequent controls must not block.
			done := make(chan struct{})
			go func() {
				defer close(done)
				p.Play()
				p.PauseAndStopReading()
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("controls blocked after a failed Seek")
			}
		})
	}
}

// readErrorSeeker returns data, then err, and always fails Seek.
type readErrorSeeker struct {
	data []byte
	err  error

	m sync.Mutex
}

func (s *readErrorSeeker) Read(buf []byte) (int, error) {
	s.m.Lock()
	defer s.m.Unlock()
	n := copy(buf, s.data)
	s.data = s.data[n:]
	if n == 0 {
		return 0, s.err
	}
	return n, s.err
}

func (*readErrorSeeker) Seek(int64, int) (int64, error) {
	return 0, errors.New("seek failed")
}

// A source error met while data is buffered is kept across a failed Seek and
// reported after that data is played.
func TestFailedSeekKeepsSourceError(t *testing.T) {
	errSource := errors.New("source failed")

	src := &readErrorSeeker{
		data: make([]byte, 64),
		err:  errSource,
	}
	m := mux.New(48000, 1, mux.FormatUnsignedInt8)
	defer m.Stop()
	p := newPlayer(t, m, src)
	p.SetBufferSize(64)
	p.Play()
	waitForBufferedSize(t, p, 64)

	if _, err := p.Seek(0, io.SeekStart); err == nil {
		t.Fatal("Seek on a failing source succeeded")
	}
	// The buffered data is still pending, so the error is not reported yet.
	if err := p.Err(); err != nil {
		t.Errorf("Err before the data is played: got %v; want nil", err)
	}
	m.ReadFloat32s(make([]float32, 64))
	deadline := time.Now().Add(time.Second)
	for p.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the source error was not reported after the data was played")
		}
		time.Sleep(time.Millisecond)
	}
	if err := p.Err(); !errors.Is(err, errSource) {
		t.Errorf("Err: got %v; want %v", err, errSource)
	}
}

// A successful Seek while playing keeps the player playing, and the player
// keeps filling its buffer from the new position.
func TestSeekWhilePlayingKeepsPlaying(t *testing.T) {
	src := &errSeeker{err: nil}
	m := mux.New(48000, 1, mux.FormatUnsignedInt8)
	defer m.Stop()
	p := newPlayer(t, m, src)
	p.SetBufferSize(64)
	p.Play()
	waitForBufferedSize(t, p, 64)
	if _, err := p.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if !p.IsPlaying() {
		t.Error("a successful Seek while playing paused the player")
	}
	// The buffer is refilled from the new position, so the player stays usable.
	waitForBufferedSize(t, p, 64)
}

// overlapSeeker records whether its Read and Seek ever ran at the same time.
type overlapSeeker struct {
	m          sync.Mutex
	pos        int
	reading    atomic.Int32
	seeking    atomic.Int32
	overlapped atomic.Bool
}

func (s *overlapSeeker) Read(buf []byte) (int, error) {
	if s.seeking.Load() != 0 {
		s.overlapped.Store(true)
	}
	s.reading.Add(1)
	defer s.reading.Add(-1)
	s.m.Lock()
	for i := range buf {
		buf[i] = byte(s.pos + i)
	}
	s.pos += len(buf)
	s.m.Unlock()
	time.Sleep(20 * time.Microsecond)
	return len(buf), nil
}

func (s *overlapSeeker) Seek(int64, int) (int64, error) {
	if s.reading.Load() != 0 {
		s.overlapped.Store(true)
	}
	s.seeking.Add(1)
	defer s.seeking.Add(-1)
	time.Sleep(20 * time.Microsecond)
	return 0, nil
}

// Concurrent Play, Pause, PauseAndStopReading, Reset, Seek, and unplayed/size
// queries must all complete, never hang, and never let the source be read and
// sought at the same time.
func TestConcurrentControlsNeverOverlapSource(t *testing.T) {
	for iter := range 5 {
		src := &overlapSeeker{}
		m := mux.New(48000, 2, mux.FormatSignedInt16LE)
		p := m.NewPlayer(src)
		p.SetBufferSize(256)
		p.Play()

		stop := make(chan struct{})
		var driver sync.WaitGroup
		driver.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				m.ReadFloat32s(make([]float32, 64))
				time.Sleep(20 * time.Microsecond)
			}
		})

		var controls sync.WaitGroup
		for i := range 8 {
			controls.Go(func() {
				for range 50 {
					switch i % 5 {
					case 0:
						p.Pause()
					case 1:
						p.PauseAndStopReading()
					case 2:
						_, _ = p.Seek(0, io.SeekStart)
					case 3:
						p.Reset()
					case 4:
						p.Play()
					}
					p.BufferedSize()
					p.UnplayedSize()
					p.IsPlaying()
				}
			})
		}
		done := make(chan struct{})
		go func() {
			controls.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("iteration %d: controls hung", iter)
		}
		close(stop)
		driver.Wait()
		m.Stop()
		_ = p.Close()

		if src.overlapped.Load() {
			t.Fatalf("iteration %d: the source was read and sought at the same time", iter)
		}
	}
}

// gatedRampSeeker is a source whose Read blocks until gate is closed, then
// produces an incrementing byte sequence; Seek always fails after signalling.
type gatedRampSeeker struct {
	gate    chan struct{}
	began   chan struct{}
	beganOn sync.Once

	seekEntered chan struct{}

	m   sync.Mutex
	pos int
}

func (s *gatedRampSeeker) Read(buf []byte) (int, error) {
	s.beganOn.Do(func() { close(s.began) })
	<-s.gate
	s.m.Lock()
	defer s.m.Unlock()
	for i := range buf {
		buf[i] = byte(s.pos + i)
	}
	s.pos += len(buf)
	return len(buf), nil
}

func (s *gatedRampSeeker) Seek(int64, int) (int64, error) {
	close(s.seekEntered)
	return 0, errors.New("seek failed")
}

// A failed Seek with a Read in flight must keep that read's data (it is at the
// current position) and keep the buffered data continuous.
func TestFailedSeekKeepsInFlightReadData(t *testing.T) {
	src := &gatedRampSeeker{
		gate:        make(chan struct{}),
		began:       make(chan struct{}),
		seekEntered: make(chan struct{}),
	}
	m := mux.New(48000, 1, mux.FormatUnsignedInt8)
	defer m.Stop()
	p := newPlayer(t, m, src)
	p.SetBufferSize(64)
	p.Play()

	// Wait until a read from the source is in flight.
	<-src.began

	seekDone := make(chan struct{})
	go func() {
		defer close(seekDone)
		if _, err := p.Seek(0, io.SeekStart); err == nil {
			t.Error("Seek on a failing source succeeded")
		}
	}()

	// Let the in-flight read finish so that Seek can reach the source, then fail.
	close(src.gate)
	select {
	case <-src.seekEntered:
	case <-time.After(time.Second):
		t.Fatal("the source Seek did not begin")
	}
	select {
	case <-seekDone:
	case <-time.After(time.Second):
		t.Fatal("Seek did not return after the source Seek failed")
	}

	// The read's data must be kept: it is at the current position.
	waitForBufferedSize(t, p, 64)

	// The buffered data must start at the beginning of the source, so no byte is
	// lost: the first sample is byte 0.
	buf := make([]float32, 64)
	m.ReadFloat32s(buf)
	for i, v := range buf {
		if got, want := byte(int(v*(1<<7))+(1<<7)), byte(i); got != want {
			t.Fatalf("sample %d: got %d; want %d (data is not continuous)", i, got, want)
		}
	}
}
