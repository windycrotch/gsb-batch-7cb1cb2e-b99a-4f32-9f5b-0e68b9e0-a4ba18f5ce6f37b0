package sfu

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// Submit lists from many goroutines while connections come and go, then check that the
// visible state matches the last accepted submission exactly: one entry per reachable
// address, no duplicates, no stragglers from earlier intents.
func TestConcurrentSubmitAndLifecycleConverges(t *testing.T) {
	for round := 0; round < 10; round++ {
		s, factory := newTestSfu(t)
		s.serverRetryBaseDelay = 5 * time.Millisecond
		s.serverMaxConnectAttempts = 3

		addresses := []string{"ws://a", "ws://b", "ws://c"}

		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()

				subset := []string{addresses[i%len(addresses)]}
				if i%4 == 0 {
					subset = append(subset, addresses[(i+1)%len(addresses)])
				}
				if i%5 == 0 {
					subset = append(subset, subset...) // duplicates in one payload
				}

				s.SetCurrentServers(&CurrentServers{Servers: subset})

				for _, url := range subset {
					if c := factory.lastFor(url); c != nil {
						if i%3 == 0 {
							c.connect()
						} else if i%3 == 1 {
							c.disconnect(fmt.Errorf("blip"))
						}
					}
				}
			}(i)
		}
		wg.Wait()

		// Last word: exactly one address.
		final := []string{"ws://c"}
		s.SetCurrentServers(&CurrentServers{Servers: final})

		// Settle any lifecycle events which were already queued ahead of the final
		// submission's result (they are all older than it, on the same serial queue).
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			s.GetCurrentServers()
			time.Sleep(time.Millisecond)
		}

		got := s.GetCurrentServers()

		if len(got.ServerStatuses) != 1 || got.ServerStatuses[0].Url != "ws://c" {
			t.Fatalf("round %d: expected only ws://c, got %v", round, got.ServerStatuses)
		}

		// Nothing but the last address may be connected.
		for _, url := range serversOf(got) {
			if url != "ws://c" {
				t.Fatalf("round %d: unexpected live connection %s (all %v)", round, url, got.Servers)
			}
		}
	}
}

// A submission which drops an address must stop that address's live connection even
// while other addresses stay untouched.
func TestRemovingOneAddressLeavesOthersConnected(t *testing.T) {
	s, factory := newTestSfu(t)

	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://keep", "ws://drop"}})
	keep := factory.lastFor("ws://keep")
	drop := factory.lastFor("ws://drop")
	keep.connect()
	drop.connect()

	waitFor(t, time.Second, "both connected", func() bool {
		return len(s.GetCurrentServers().Servers) == 2
	})

	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://keep"}})

	got := s.GetCurrentServers()
	assertServersEqual(t, serversOf(got), "ws://keep")

	if drop.stopped() != 1 {
		t.Fatalf("expected the dropped address to be stopped once, got %d", drop.stopped())
	}
	if keep.stopped() != 0 {
		t.Fatalf("the retained address must not be stopped, got %d stops", keep.stopped())
	}

	time.Sleep(50 * time.Millisecond)

	after := s.GetCurrentServers()
	assertServersEqual(t, serversOf(after), "ws://keep")
}

// A relay which keeps accepting a connection and immediately dropping it must not
// reconnect forever at the base delay: the attempt count carries over so it backs off
// and eventually gives up, ending in a visibly Failed state.
func TestFlappingServerBacksOffAndEventuallyFails(t *testing.T) {
	s, factory := newTestSfu(t)
	s.serverRetryBaseDelay = time.Millisecond
	s.serverRetryMaxDelay = 2 * time.Millisecond
	s.serverMaxConnectAttempts = 3
	// A flapping connection is one which drops straight away, so it never earns a reset.
	s.serverHealthyConnectionDuration = time.Hour

	s.SetCurrentServers(&CurrentServers{Servers: []string{"ws://flap"}})

	for i := 0; i < 3; i++ {
		waitFor(t, 2*time.Second, "a flapping connection attempt", func() bool {
			return factory.count() > i
		})

		c := factory.snapshot()[i]
		c.connect()
		waitFor(t, 2*time.Second, "flapping connection to be listed", func() bool {
			return len(s.GetCurrentServers().Servers) == 1
		})
		c.disconnect(fmt.Errorf("dropped immediately"))
	}

	waitFor(t, 3*time.Second, "flapping server to give up", func() bool {
		got := s.GetCurrentServers()
		return len(got.ServerStatuses) == 1 && got.ServerStatuses[0].State == ServerState_Failed
	})

	attempts := s.GetCurrentServers().ServerStatuses[0].Attempts
	if attempts != 3 {
		t.Fatalf("expected attempts to accumulate across flaps, got %d", attempts)
	}
}
