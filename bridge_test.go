package goff

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
)

// failingReadSeeker fails every Read/Seek with the configured errors
// and counts how often it was called.
type failingReadSeeker struct {
	readErr   error
	seekErr   error
	readCalls int32
	seekCalls int32
}

func (f *failingReadSeeker) Read(p []byte) (int, error) {
	atomic.AddInt32(&f.readCalls, 1)
	return 0, f.readErr
}

func (f *failingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	atomic.AddInt32(&f.seekCalls, 1)
	return 0, f.seekErr
}

// failingWriteSeeker fails every Write/Seek with the configured errors
// and counts how often it was called.
type failingWriteSeeker struct {
	writeErr   error
	seekErr    error
	writeCalls int32
	seekCalls  int32
}

func (f *failingWriteSeeker) Write(p []byte) (int, error) {
	atomic.AddInt32(&f.writeCalls, 1)
	return 0, f.writeErr
}

func (f *failingWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	atomic.AddInt32(&f.seekCalls, 1)
	return 0, f.seekErr
}

// readerOpaque mirrors the id-as-opaque-pointer encoding in NewReader.
//
//go:nocheckptr
func readerOpaque(r *Reader) unsafe.Pointer {
	return unsafe.Pointer(uintptr(r.id))
}

// writerOpaque mirrors the id-as-opaque-pointer encoding in NewWriter.
//
//go:nocheckptr
func writerOpaque(w *Writer) unsafe.Pointer {
	return unsafe.Pointer(uintptr(w.id))
}

func TestReaderReadSuccessAndEOF(t *testing.T) {
	assert := assert.New(t)

	data := []byte("hello")
	r := NewReader(bytes.NewReader(data), int64(len(data)))
	defer r.Free()

	buf := make([]byte, 4)

	ret := goff_reader_read_packet(readerOpaque(r), unsafe.Pointer(&buf[0]), len(buf))
	assert.Equal(4, ret)
	assert.Equal("hell", string(buf))

	ret = goff_reader_read_packet(readerOpaque(r), unsafe.Pointer(&buf[0]), len(buf))
	assert.Equal(1, ret)
	assert.Equal(byte('o'), buf[0])

	// at end of stream the bridge reports AVERROR_EOF, and EOF is
	// not recorded as an error
	ret = goff_reader_read_packet(readerOpaque(r), unsafe.Pointer(&buf[0]), len(buf))
	assert.EqualValues(int(ERROR_EOF), ret)
	assert.NoError(r.Error())
}

func TestReaderStickyError(t *testing.T) {
	assert := assert.New(t)

	readErr := errors.New("read exploded")
	fr := &failingReadSeeker{readErr: readErr, seekErr: errors.New("seek exploded")}
	r := NewReader(fr, 128)
	defer r.Free()

	buf := make([]byte, 16)

	ret := goff_reader_read_packet(readerOpaque(r), unsafe.Pointer(&buf[0]), len(buf))
	assert.EqualValues(int(ERROR_EXTERNAL), ret)
	assert.Equal(readErr, r.Error())

	// subsequent reads fail stably without touching the underlying reader
	ret = goff_reader_read_packet(readerOpaque(r), unsafe.Pointer(&buf[0]), len(buf))
	assert.EqualValues(int(ERROR_EXTERNAL), ret)
	assert.EqualValues(1, atomic.LoadInt32(&fr.readCalls))

	// seeks fail too, without touching the underlying seeker
	sret := goff_reader_seek(readerOpaque(r), 0, 0)
	assert.EqualValues(int64(ERROR_EXTERNAL), sret)
	assert.EqualValues(0, atomic.LoadInt32(&fr.seekCalls))
}

func TestReaderSeekErrorIsSticky(t *testing.T) {
	assert := assert.New(t)

	seekErr := errors.New("seek exploded")
	fr := &failingReadSeeker{readErr: errors.New("read exploded"), seekErr: seekErr}
	r := NewReader(fr, 128)
	defer r.Free()

	sret := goff_reader_seek(readerOpaque(r), 0, 0)
	assert.EqualValues(int64(ERROR_EXTERNAL), sret)
	assert.Equal(seekErr, r.Error())

	// once the underlying object failed, reads must not touch it either
	buf := make([]byte, 16)
	ret := goff_reader_read_packet(readerOpaque(r), unsafe.Pointer(&buf[0]), len(buf))
	assert.EqualValues(int(ERROR_EXTERNAL), ret)
	assert.EqualValues(0, atomic.LoadInt32(&fr.readCalls))
	assert.EqualValues(1, atomic.LoadInt32(&fr.seekCalls))
}

func TestReaderCallbacksAfterFree(t *testing.T) {
	assert := assert.New(t)

	r := NewReader(bytes.NewReader([]byte("hello")), 5)
	opaque := readerOpaque(r)
	r.Free()

	buf := make([]byte, 4)

	// callbacks hitting a freed bridge must fail safely, not crash
	// or read dirty memory
	ret := goff_reader_read_packet(opaque, unsafe.Pointer(&buf[0]), len(buf))
	assert.EqualValues(int(ERROR_EXTERNAL), ret)

	sret := goff_reader_seek(opaque, 0, 0)
	assert.EqualValues(int64(ERROR_EXTERNAL), sret)
}

func TestReaderConcurrentAndRepeatedFree(t *testing.T) {
	assert := assert.New(t)

	r := NewReader(bytes.NewReader([]byte("hello")), 5)
	opaque := readerOpaque(r)

	// concurrent frees must not double-free the C resources
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Free()
		}()
	}
	wg.Wait()

	// repeated free is a no-op
	r.Free()
	r.Free()

	buf := make([]byte, 4)
	ret := goff_reader_read_packet(opaque, unsafe.Pointer(&buf[0]), len(buf))
	assert.EqualValues(int(ERROR_EXTERNAL), ret)
}

func TestWriterStickyError(t *testing.T) {
	assert := assert.New(t)

	writeErr := errors.New("write exploded")
	fw := &failingWriteSeeker{writeErr: writeErr, seekErr: errors.New("seek exploded")}
	w := NewWriter(fw)
	defer w.Free()

	buf := make([]byte, 16)

	ret := goff_writer_write_packet(writerOpaque(w), unsafe.Pointer(&buf[0]), len(buf))
	assert.EqualValues(int(ERROR_EXTERNAL), ret)
	assert.Equal(writeErr, w.Error())

	// subsequent writes fail stably without touching the underlying writer
	ret = goff_writer_write_packet(writerOpaque(w), unsafe.Pointer(&buf[0]), len(buf))
	assert.EqualValues(int(ERROR_EXTERNAL), ret)
	assert.EqualValues(1, atomic.LoadInt32(&fw.writeCalls))

	// seeks fail too, without touching the underlying seeker
	sret := goff_writer_seek(writerOpaque(w), 0, 0)
	assert.EqualValues(int64(ERROR_EXTERNAL), sret)
	assert.EqualValues(0, atomic.LoadInt32(&fw.seekCalls))
}

func TestWriterSeekErrorIsSticky(t *testing.T) {
	assert := assert.New(t)

	seekErr := errors.New("seek exploded")
	fw := &failingWriteSeeker{writeErr: errors.New("write exploded"), seekErr: seekErr}
	w := NewWriter(fw)
	defer w.Free()

	sret := goff_writer_seek(writerOpaque(w), 0, 0)
	assert.EqualValues(int64(ERROR_EXTERNAL), sret)
	assert.Equal(seekErr, w.Error())

	// once the underlying object failed, writes must not touch it either
	buf := make([]byte, 16)
	ret := goff_writer_write_packet(writerOpaque(w), unsafe.Pointer(&buf[0]), len(buf))
	assert.EqualValues(int(ERROR_EXTERNAL), ret)
	assert.EqualValues(0, atomic.LoadInt32(&fw.writeCalls))
	assert.EqualValues(1, atomic.LoadInt32(&fw.seekCalls))
}

func TestWriterCallbacksAfterFree(t *testing.T) {
	assert := assert.New(t)

	w := NewWriter(&failingWriteSeeker{})
	opaque := writerOpaque(w)
	assert.NoError(w.Free())

	buf := make([]byte, 4)

	// callbacks hitting a freed bridge must fail safely
	ret := goff_writer_write_packet(opaque, unsafe.Pointer(&buf[0]), len(buf))
	assert.EqualValues(int(ERROR_EXTERNAL), ret)

	sret := goff_writer_seek(opaque, 0, 0)
	assert.EqualValues(int64(ERROR_EXTERNAL), sret)
}

func TestWriterConcurrentAndRepeatedFree(t *testing.T) {
	assert := assert.New(t)

	w := NewWriter(&failingWriteSeeker{})
	opaque := writerOpaque(w)

	// concurrent frees must not double-free the C resources
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(w.Free())
		}()
	}
	wg.Wait()

	// repeated free is a no-op
	assert.NoError(w.Free())
	assert.NoError(w.Free())

	buf := make([]byte, 4)
	ret := goff_writer_write_packet(opaque, unsafe.Pointer(&buf[0]), len(buf))
	assert.EqualValues(int(ERROR_EXTERNAL), ret)
}

func TestOpenReaderWithFailingSource(t *testing.T) {
	assert := assert.New(t)

	ctx := FormatAllocContext()
	// note: no ctx.Free() here — avformat_open_input frees the
	// AVFormatContext itself when it fails

	readErr := errors.New("read exploded")
	fr := &failingReadSeeker{readErr: readErr, seekErr: errors.New("seek exploded")}
	r := NewReader(fr, 128)
	defer r.Free()

	// the failure must propagate to the caller of avformat_open_input...
	err := ctx.OpenReader(r, nil, nil)
	assert.Error(err)

	// ...and remain observable on the bridge
	assert.Equal(readErr, r.Error())

	// further callbacks fail without touching the underlying reader again
	before := atomic.LoadInt32(&fr.readCalls)
	buf := make([]byte, 16)
	ret := goff_reader_read_packet(readerOpaque(r), unsafe.Pointer(&buf[0]), len(buf))
	assert.EqualValues(int(ERROR_EXTERNAL), ret)
	assert.EqualValues(before, atomic.LoadInt32(&fr.readCalls))
}
