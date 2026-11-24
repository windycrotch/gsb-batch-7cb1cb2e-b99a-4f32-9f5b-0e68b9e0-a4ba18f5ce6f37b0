package goff

import (
	"io"
	"testing"
	"unsafe"

	"github.com/itchio/goff/memfile"
	"github.com/stretchr/testify/assert"
)

// Data written into the AVIOContext's internal buffer but not yet
// flushed must still reach the underlying writer when the bridge is
// freed.
func TestWriterFreeFlushesBufferedData(t *testing.T) {
	assert := assert.New(t)

	mf := memfile.New()
	w := NewWriter(mf)

	data := "some buffered, not yet flushed bytes"

	// PutStr writes into avio's 4KB internal buffer; the data is
	// small enough that nothing is delivered to the underlying
	// writer yet. avio_put_str also writes a terminating NUL.
	assert.NoError(w.ctx.PutStr(data))

	pos, err := mf.Seek(0, io.SeekEnd)
	assert.NoError(err)
	assert.EqualValues(0, pos)

	assert.NoError(w.Free())

	// Free flushed the buffered data through the write callback
	pos, err = mf.Seek(0, io.SeekEnd)
	assert.NoError(err)
	assert.EqualValues(len(data)+1, pos)

	got := make([]byte, len(data)+1)
	_, err = mf.Seek(0, io.SeekStart)
	assert.NoError(err)
	n, err := mf.Read(got)
	assert.NoError(err)
	assert.Equal(len(data)+1, n)
	assert.Equal(data, string(got[:len(got)-1]))
	assert.Equal(byte(0), got[len(got)-1])
}

// After the underlying writer fails, avio-level flushes must keep
// failing instead of delivering to the broken writer.
func TestWriterAvioWriteAfterFailure(t *testing.T) {
	assert := assert.New(t)

	fw := &failingWriteSeeker{writeErr: errInjected, seekErr: errInjected}
	w := NewWriter(fw)
	defer w.Free()

	// the first flush delivers to the failing writer and records
	// the error on the bridge
	assert.NoError(w.ctx.PutStr("x"))
	w.ctx.Flush()
	assert.Error(w.Error())

	// the bridge remembers the failure: direct callbacks keep failing
	// without touching the underlying writer
	buf := make([]byte, 1)
	ret := goff_writer_write_packet(writerOpaque(w), unsafe.Pointer(&buf[0]), len(buf))
	assert.EqualValues(int(ERROR_EXTERNAL), ret)
	assert.EqualValues(1, fw.writeCalls)
}

var errInjected = errorString("injected failure")

type errorString string

func (e errorString) Error() string { return string(e) }
