//go:build !windows

package submonkey

import (
	"errors"
	"os"
	"syscall"
)

// tryLockRegion attempts a non-blocking exclusive advisory lock.
func tryLockRegion(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return false, err
}

func unlockRegion(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
