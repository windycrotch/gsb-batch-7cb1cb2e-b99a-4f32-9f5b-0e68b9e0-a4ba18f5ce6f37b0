//go:build windows

package submonkey

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	modKernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modKernel32.NewProc("LockFileEx")
	procUnlockFileEx = modKernel32.NewProc("UnlockFileEx")
)

const (
	winLockfileExclusiveLock   = 0x00000002
	winLockfileFailImmediately = 0x00000001
)

// tryLockRegion attempts to take an exclusive 1-byte lock at offset 0.
// A false result (with nil error) means a conflicting lock is held.
func tryLockRegion(f *os.File) (bool, error) {
	var ol syscall.Overlapped
	r1, _, callErr := procLockFileEx.Call(
		f.Fd(),
		uintptr(winLockfileExclusiveLock|winLockfileFailImmediately),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 != 0 {
		return true, nil
	}
	if errno, ok := callErr.(syscall.Errno); ok {
		// ERROR_LOCK_VIOLATION (33) and ERROR_IO_PENDING (997) both mean the
		// non-blocking lock could not be granted right now.
		if errno == 33 || errno == syscall.ERROR_IO_PENDING {
			return false, nil
		}
		return false, errno
	}
	return false, callErr
}

func unlockRegion(f *os.File) error {
	var ol syscall.Overlapped
	r1, _, callErr := procUnlockFileEx.Call(
		f.Fd(),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		if errno, ok := callErr.(syscall.Errno); ok {
			return errno
		}
		return callErr
	}
	return nil
}
