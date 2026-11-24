package razor

import (
	"testing"
	"time"
)

// A notification which arrives before Wait must not be lost, otherwise callers sleep
// for their full timeout even though work is ready.
func TestNotifyBeforeWaitIsNotLost(t *testing.T) {
	l := NewLockAndNotify()

	l.Notify()
	l.Notify()
	l.Notify()

	start := time.Now()
	l.Wait(2 * time.Second)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("a pending notification was lost: waited %s", elapsed)
	}
}

// Wait must return promptly on a notification delivered while it is parked.
func TestNotifyWakesWaiter(t *testing.T) {
	l := NewLockAndNotify()

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		l.Wait(5 * time.Second)
		done <- time.Since(start)
	}()

	time.Sleep(20 * time.Millisecond)
	l.Notify()

	select {
	case elapsed := <-done:
		if elapsed > time.Second {
			t.Fatalf("waiter was not woken promptly: %s", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not wake at all")
	}
}

// With no notification, Wait still honours its timeout.
func TestWaitHonoursTimeout(t *testing.T) {
	l := NewLockAndNotify()

	start := time.Now()
	l.Wait(30 * time.Millisecond)
	elapsed := time.Since(start)

	if elapsed < 25*time.Millisecond {
		t.Fatalf("Wait returned too early: %s", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("Wait blocked far longer than its timeout: %s", elapsed)
	}
}

// A zero delay must not block, and must consume any pending notification.
func TestWaitZeroDelayConsumesNotification(t *testing.T) {
	l := NewLockAndNotify()

	l.Notify()
	l.Wait(0)

	start := time.Now()
	l.Wait(20 * time.Millisecond)
	if time.Since(start) < 15*time.Millisecond {
		t.Fatal("a consumed notification woke a later zero-timeout wait")
	}
}

// Send/Timeout/Cancel on a message handler must wake the loop promptly, including when
// the notification is issued immediately after construction.
func TestHandlerProcessesImmediatelyAfterSend(t *testing.T) {
	logger := NewLogger(LoggingLevelOff, false)

	processed := make(chan int, 16)

	handler := NewMessageHandler[int, int](logger, "test", 16, func(what int, payload *int) bool {
		processed <- what
		return true
	})

	handler.Loop(nil)
	defer handler.Abort()

	for i := 0; i < 8; i++ {
		handler.Send(i, nil)
	}

	deadline := time.After(2 * time.Second)
	for i := 0; i < 8; i++ {
		select {
		case <-processed:
		case <-deadline:
			t.Fatalf("handler did not process queued work promptly (got %d of 8)", i)
		}
	}
}

// Send drops a message when the queue is full; SendWait must not. This matters for
// teardown and retry messages whose loss would leave state inconsistent forever.
func TestSendWaitDoesNotDropWhenQueueFull(t *testing.T) {
	logger := NewLogger(LoggingLevelOff, false)

	started := make(chan struct{})
	release := make(chan struct{})
	processed := make(chan int, 8)

	// A length-1 queue whose first item blocks the handler goroutine, so the queue can
	// be filled deterministically while the handler is stuck.
	handler := NewMessageHandler[int, int](logger, "full", 1, func(what int, payload *int) bool {
		if what == 0 {
			close(started)
			<-release
		}
		processed <- what
		return true
	})

	handler.Loop(nil)
	defer handler.Abort()

	handler.Send(0, nil)

	// Wait until the handler is actually blocked on item 0, so the next send lands in
	// the queue rather than being popped straight away.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	if !handler.Send(1, nil) {
		t.Fatal("expected the single queue slot to accept one item")
	}

	// A best-effort send is dropped, but a reliable send waits for room.
	if handler.Send(2, nil) {
		t.Fatal("expected Send to report the full queue")
	}

	sent := make(chan bool, 1)
	go func() {
		sent <- handler.SendWait(3, nil, 2*time.Second)
	}()

	// Give SendWait a moment to be blocked, then drain.
	time.Sleep(20 * time.Millisecond)
	close(release)

	select {
	case ok := <-sent:
		if !ok {
			t.Fatal("SendWait gave up while the queue was draining")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SendWait never returned")
	}

	// Every queued item (0 blocking, 1 queued, 3 via SendWait) must be processed; only
	// 2 (the dropped best-effort send) is allowed to be missing.
	seen := map[int]bool{}
	timeout := time.After(2 * time.Second)
	for len(seen) < 3 {
		select {
		case what := <-processed:
			seen[what] = true
		case <-timeout:
			t.Fatalf("queued messages were lost; processed %v", seen)
		}
	}

	if !seen[1] {
		t.Fatalf("the item queued before SendWait was not processed: %v", seen)
	}
	if !seen[3] {
		t.Fatalf("SendWait's message was lost: %v", seen)
	}
	if seen[2] {
		t.Fatalf("the best-effort send was unexpectedly delivered: %v", seen)
	}
}
