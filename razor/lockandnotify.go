package razor

import (
	"sync"
	"time"
)

type LockAndNotify struct {
	mutex sync.Mutex

	// notifyCh is a one-slot notification channel. Using a buffered channel (rather
	// than tracking a separate "is the waiter parked" flag) means a Notify which
	// arrives just before Wait is entered is not lost: Wait consumes it immediately.
	// Without this a wakeup could be dropped and the handler would sleep until its
	// full idle timeout, delaying responses which were ready to be produced.
	notifyCh chan struct{}
}

func NewLockAndNotify() *LockAndNotify {
	return &LockAndNotify{
		notifyCh: make(chan struct{}, 1),
	}
}

func (l *LockAndNotify) Enter() {
	l.mutex.Lock()
}

func (l *LockAndNotify) Leave() {
	l.mutex.Unlock()
}

// Notify wakes a waiter. It never blocks: at most one notification is pending, which
// is all a single consumer loop needs.
func (l *LockAndNotify) Notify() {
	select {
	case l.notifyCh <- struct{}{}:
	default:
	}
}

func (l *LockAndNotify) Wait(delay time.Duration) {
	if delay <= 0 {
		select {
		case <-l.notifyCh:
		default:
		}
		return
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-l.notifyCh:
	case <-timer.C:
	}
}
