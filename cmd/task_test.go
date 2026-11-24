package cmd

import (
	"sync"
	"testing"
)

func TestTaskLifecycleTransitions(t *testing.T) {
	task := newTask("job-1", "client-1", false, Message{Input: "in.mp4", Output: "out.mp4"})

	if got := task.State(); got != stateQueued {
		t.Fatalf("new task state = %q, want queued", got)
	}

	// Queued -> running.
	if !task.markRunning() {
		t.Fatal("markRunning returned false for a queued task")
	}
	if got := task.State(); got != stateRunning {
		t.Fatalf("state = %q, want running", got)
	}

	// A running task cannot be cancelled through the queued-only transition.
	if task.cancelQueued() {
		t.Fatal("cancelQueued succeeded on a running task")
	}

	// Running -> exactly one terminal state.
	if !task.finish(stateComplete, "", 100) {
		t.Fatal("finish returned false for a running task")
	}
	if !task.isTerminal() {
		t.Fatal("task not reported terminal after finish")
	}

	// Every further terminal transition is rejected, so a late cancel can
	// never turn a completed task into a cancelled one.
	if task.finish(stateCancelled, "", 0) {
		t.Fatal("finish accepted a second terminal transition")
	}
	if task.finish(stateFailed, "boom", 0) {
		t.Fatal("finish accepted a third terminal transition")
	}
	if got := task.State(); got != stateComplete {
		t.Fatalf("state changed after terminal transition: %q", got)
	}
}

func TestCancelQueuedBeforeRun(t *testing.T) {
	task := newTask("job-2", "client-1", false, Message{})

	if !task.cancelQueued() {
		t.Fatal("first cancelQueued returned false")
	}
	if got := task.State(); got != stateCancelled {
		t.Fatalf("state = %q, want cancelled", got)
	}
	// Duplicate cancels are no-ops: exactly one cancellation event can fire.
	if task.cancelQueued() {
		t.Fatal("second cancelQueued returned true")
	}
	// Worker must skip a task cancelled while queued.
	if task.markRunning() {
		t.Fatal("markRunning succeeded on a cancelled task")
	}
	if got := task.State(); got != stateCancelled {
		t.Fatalf("state = %q, want cancelled", got)
	}
}

func TestSingleTerminalStateUnderConcurrency(t *testing.T) {
	// Hammer a running task from many goroutines with competing outcomes.
	// Whatever wins, exactly one terminal state must survive.
	for range 100 {
		task := newTask("job", "client", false, Message{})
		task.markRunning()

		var wg sync.WaitGroup
		outcomes := []string{stateComplete, stateFailed, stateCancelled}
		for _, outcome := range outcomes {
			for range 20 {
				wg.Add(1)
				go func(outcome string) {
					defer wg.Done()
					task.finish(outcome, "err", 0)
				}(outcome)
			}
		}
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				task.cancelQueued()
			}()
		}
		wg.Wait()

		state := task.State()
		if !isTerminalState(state) {
			t.Fatalf("task did not reach a terminal state: %q", state)
		}
		count := 0
		for _, outcome := range outcomes {
			if state == outcome {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("task ended in ambiguous state %q", state)
		}
	}
}

func TestRegistryRejectsDuplicateIDs(t *testing.T) {
	reg := newRegistry(10)
	msg := Message{}
	first := newTask("dup", "client-1", false, msg)
	second := newTask("dup", "client-2", false, msg)

	if err := reg.add(first); err != nil {
		t.Fatalf("first add failed: %v", err)
	}
	if err := reg.add(second); err != errDuplicateID {
		t.Fatalf("second add error = %v, want errDuplicateID", err)
	}
}

func TestRegistryOwnedByAndPruning(t *testing.T) {
	reg := newRegistry(3)

	mk := func(id, owner string, terminal bool) *Task {
		task := newTask(id, owner, false, Message{})
		task.markRunning()
		if terminal {
			task.finish(stateComplete, "", 100)
		}
		return task
	}

	a := mk("a", "alice", true)
	b := mk("b", "bob", true)
	c := mk("c", "alice", false) // Still active.
	for _, task := range []*Task{a, b, c} {
		if err := reg.add(task); err != nil {
			t.Fatalf("add %s: %v", task.ID, err)
		}
	}

	owned := reg.ownedBy("alice")
	if len(owned) != 2 || owned[0].ID != "a" || owned[1].ID != "c" {
		t.Fatalf("ownedBy(alice) = %v, want [a c] in insertion order", owned)
	}

	// Push past the retention limit: the oldest terminal record ("a") is
	// pruned, while the active task "c" must survive.
	d := mk("d", "bob", true)
	e := mk("e", "bob", true)
	if err := reg.add(d); err != nil {
		t.Fatal(err)
	}
	if err := reg.add(e); err != nil {
		t.Fatal(err)
	}

	if _, ok := reg.get("a"); ok {
		t.Fatal("oldest terminal task was not pruned")
	}
	if _, ok := reg.get("c"); !ok {
		t.Fatal("active task was pruned")
	}
	if got := reg.ownedBy("alice"); len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("alice's active task lost after prune: %v", got)
	}
}

func TestValidClientID(t *testing.T) {
	cases := map[string]bool{
		"":             false,
		"abc-123_XYZ.": true,
		"has space":    false,
		"slash/x":      false,
		"unicode-✓":    false,
		"ok":           true,
	}
	for id, want := range cases {
		if got := validClientID(id); got != want {
			t.Errorf("validClientID(%q) = %v, want %v", id, got, want)
		}
	}
}
