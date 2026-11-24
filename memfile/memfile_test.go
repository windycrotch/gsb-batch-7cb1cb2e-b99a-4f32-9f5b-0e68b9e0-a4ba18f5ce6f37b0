package memfile

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

func readAll(t *testing.T, mf FileLike) []byte {
	t.Helper()
	_, err := mf.Seek(0, io.SeekStart)
	assert.NoError(t, err)
	out := make([]byte, 0, 64)
	buf := make([]byte, 8)
	for {
		n, err := mf.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		assert.NoError(t, err)
	}
	return out
}

func TestWriteReadRoundtrip(t *testing.T) {
	assert := assert.New(t)

	mf := New()
	n, err := mf.Write([]byte("hello world"))
	assert.NoError(err)
	assert.Equal(11, n)

	pos, err := mf.Seek(0, io.SeekEnd)
	assert.NoError(err)
	assert.EqualValues(11, pos)

	assert.Equal([]byte("hello world"), readAll(t, mf))
}

func TestSeekEndIsRelativeToEnd(t *testing.T) {
	assert := assert.New(t)

	mf := New()
	_, err := mf.Write([]byte("hello"))
	assert.NoError(err)

	// io.Seeker convention: offset is added to the end position,
	// so Seek(-2, SeekEnd) lands 2 bytes before the end
	pos, err := mf.Seek(-2, io.SeekEnd)
	assert.NoError(err)
	assert.EqualValues(3, pos)

	buf := make([]byte, 8)
	n, err := mf.Read(buf)
	assert.NoError(err)
	assert.Equal("lo", string(buf[:n]))

	pos, err = mf.Seek(0, io.SeekEnd)
	assert.NoError(err)
	assert.EqualValues(5, pos)
}

func TestSeekNegativePositionFails(t *testing.T) {
	assert := assert.New(t)

	mf := New()
	_, err := mf.Write([]byte("hello"))
	assert.NoError(err)

	_, err = mf.Seek(-1, io.SeekStart)
	assert.Error(err)

	_, err = mf.Seek(-100, io.SeekEnd)
	assert.Error(err)

	_, err = mf.Seek(-100, io.SeekCurrent)
	assert.Error(err)

	_, err = mf.Seek(0, 42)
	assert.Error(err)

	// a failed seek must not move the position
	pos, err := mf.Seek(0, io.SeekCurrent)
	assert.NoError(err)
	assert.EqualValues(5, pos)
}

func TestSeekBeyondEndAndWrite(t *testing.T) {
	assert := assert.New(t)

	mf := New()
	_, err := mf.Write([]byte("ab"))
	assert.NoError(err)

	// seeking past the end is legal per io.Seeker convention
	pos, err := mf.Seek(4, io.SeekStart)
	assert.NoError(err)
	assert.EqualValues(4, pos)

	// the gap must be zero-padded, and the write must land at
	// the seeked offset, not at the old end of data
	n, err := mf.Write([]byte("cd"))
	assert.NoError(err)
	assert.Equal(2, n)

	pos, err = mf.Seek(0, io.SeekEnd)
	assert.NoError(err)
	assert.EqualValues(6, pos)

	assert.Equal([]byte{'a', 'b', 0, 0, 'c', 'd'}, readAll(t, mf))
}

func TestSeekCurrent(t *testing.T) {
	assert := assert.New(t)

	mf := New()
	_, err := mf.Write([]byte("hello world"))
	assert.NoError(err)

	_, err = mf.Seek(6, io.SeekStart)
	assert.NoError(err)

	pos, err := mf.Seek(-6, io.SeekCurrent)
	assert.NoError(err)
	assert.EqualValues(0, pos)

	pos, err = mf.Seek(5, io.SeekCurrent)
	assert.NoError(err)
	assert.EqualValues(5, pos)

	buf := make([]byte, 1)
	n, err := mf.Read(buf)
	assert.NoError(err)
	assert.Equal(1, n)
	assert.Equal(byte(' '), buf[0])
}

func TestReadEOF(t *testing.T) {
	assert := assert.New(t)

	mf := New()
	_, err := mf.Write([]byte("ab"))
	assert.NoError(err)

	_, err = mf.Seek(0, io.SeekStart)
	assert.NoError(err)

	buf := make([]byte, 16)
	n, err := mf.Read(buf)
	assert.NoError(err)
	assert.Equal(2, n)

	_, err = mf.Read(buf)
	assert.Equal(io.EOF, err)

	// reading an empty memfile is an immediate EOF
	mf2 := New()
	_, err = mf2.Read(buf)
	assert.Equal(io.EOF, err)
}

func TestOverwriteKeepsLength(t *testing.T) {
	assert := assert.New(t)

	mf := New()
	_, err := mf.Write([]byte("aaaa"))
	assert.NoError(err)

	_, err = mf.Seek(0, io.SeekStart)
	assert.NoError(err)
	_, err = mf.Write([]byte("bb"))
	assert.NoError(err)

	pos, err := mf.Seek(0, io.SeekEnd)
	assert.NoError(err)
	assert.EqualValues(4, pos)

	assert.Equal([]byte("bbaa"), readAll(t, mf))
}
