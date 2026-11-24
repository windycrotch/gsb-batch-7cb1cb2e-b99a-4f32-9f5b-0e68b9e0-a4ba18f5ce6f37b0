package memfile

import (
	"errors"
	"io"
)

type FileLike interface {
	io.Writer
	io.Seeker
	io.Reader
}

func New() FileLike {
	return &memfile{}
}

type memfile struct {
	off int64
	buf []byte
}

func (mf *memfile) Write(p []byte) (int, error) {
	if mf.off < 0 {
		return 0, errors.New("memfile: write at negative offset")
	}

	end := mf.off + int64(len(p))
	if end > int64(len(mf.buf)) {
		// grow the buffer, zero-padding any gap between the
		// previous end of data and the current offset
		grown := make([]byte, end)
		copy(grown, mf.buf)
		mf.buf = grown
	}

	n := copy(mf.buf[mf.off:], p)
	mf.off += int64(n)
	return n, nil
}

func (mf *memfile) Read(p []byte) (int, error) {
	if mf.off >= int64(len(mf.buf)) {
		return 0, io.EOF
	}

	n := copy(p, mf.buf[mf.off:])
	mf.off += int64(n)
	return n, nil
}

func (mf *memfile) Seek(off int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = off
	case io.SeekCurrent:
		abs = mf.off + off
	case io.SeekEnd:
		abs = int64(len(mf.buf)) + off
	default:
		return 0, errors.New("memfile: invalid whence")
	}

	// per io.Seeker convention: seeking before the start is an error,
	// seeking past the end is legal
	if abs < 0 {
		return 0, errors.New("memfile: negative position")
	}

	mf.off = abs
	return abs, nil
}
