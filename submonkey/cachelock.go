package submonkey

import (
	"os"
	"path/filepath"
	"time"
)

// fileLock is an exclusive advisory byte-range lock on a file. The lock is
// released automatically by the operating system if the process dies, which
// makes it safe to use for liveness detection of concurrent generations.
type fileLock struct {
	f *os.File
}

func openLockFile(path string) (*fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &fileLock{f: f}, nil
}

// acquireFileLock blocks (up to timeout) until the exclusive lock is held.
func acquireFileLock(path string, timeout time.Duration) (*fileLock, error) {
	l, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		locked, lerr := tryLockRegion(l.f)
		if lerr != nil {
			_ = l.f.Close()
			return nil, lerr
		}
		if locked {
			return l, nil
		}
		if !time.Now().Before(deadline) {
			_ = l.f.Close()
			return nil, errLockTimeout
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// tryAcquireFileLock reports whether the exclusive lock could be taken
// immediately. A false locked result means another live owner holds it.
func tryAcquireFileLock(path string) (l *fileLock, locked bool, err error) {
	fl, err := openLockFile(path)
	if err != nil {
		return nil, false, err
	}
	ok, lerr := tryLockRegion(fl.f)
	if lerr != nil {
		_ = fl.f.Close()
		return nil, false, lerr
	}
	if !ok {
		_ = fl.f.Close()
		return nil, false, nil
	}
	return fl, true, nil
}

func (l *fileLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = unlockRegion(l.f)
	return l.f.Close()
}
