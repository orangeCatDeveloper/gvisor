// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kernel

import (
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	sentrytime "gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sync"
)

// mockClocks is a sentrytime.Clocks that simply returns the times in the
// struct.
type mockClocks struct {
	mu        sync.Mutex
	monotonic int64
	realtime  int64
}

// Update implements sentrytime.Clocks.Update. It does nothing.
func (*mockClocks) Update(parked bool) (monotonicParams sentrytime.Parameters, monotonicOk bool, realtimeParam sentrytime.Parameters, realtimeOk bool) {
	return
}

// GetTime implements sentrytime.Clocks.GetTime.
func (c *mockClocks) GetTime(id sentrytime.ClockID) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch id {
	case sentrytime.Monotonic:
		return c.monotonic, nil
	case sentrytime.Realtime:
		return c.realtime, nil
	default:
		return 0, linuxerr.EINVAL
	}
}

// stateTestClocklessTimekeeper returns a test Timekeeper which has not had
// SetClocks called.
func stateTestClocklessTimekeeper(tb testing.TB) (*Timekeeper, *VDSOParamPage) {
	ctx := contexttest.Context(tb)
	mf := pgalloc.MemoryFileFromContext(ctx)
	fr, err := mf.Allocate(hostarch.PageSize, pgalloc.AllocOpts{Kind: usage.Anonymous})
	if err != nil {
		tb.Fatalf("failed to allocate memory: %v", err)
	}
	params := NewVDSOParamPage(mf, fr)
	return &Timekeeper{}, params
}

func stateTestTimekeeper(tb testing.TB) *Timekeeper {
	t, params := stateTestClocklessTimekeeper(tb)
	t.SetClocks(sentrytime.NewCalibratedClocks(), params)
	return t
}

// TestTimekeeperMonotonicZero tests that monotonic time starts at zero.
func TestTimekeeperMonotonicZero(t *testing.T) {
	c := &mockClocks{
		monotonic: 100000,
	}

	tk, params := stateTestClocklessTimekeeper(t)
	tk.SetClocks(c, params)
	defer tk.Destroy()

	now, err := tk.GetTime(sentrytime.Monotonic)
	if err != nil {
		t.Errorf("GetTime err got %v want nil", err)
	}
	if now != 0 {
		t.Errorf("GetTime got %d want 0", now)
	}

	c.mu.Lock()
	c.monotonic += 10
	c.mu.Unlock()

	now, err = tk.GetTime(sentrytime.Monotonic)
	if err != nil {
		t.Errorf("GetTime err got %v want nil", err)
	}
	if now != 10 {
		t.Errorf("GetTime got %d want 10", now)
	}
}

// TestTimekeeperMonotonicJumpForward tests that monotonic time jumps forward
// after restore.
func TestTimekeeperMonotonicForward(t *testing.T) {
	c := &mockClocks{
		monotonic: 900000,
		realtime:  600000,
	}

	tk, params := stateTestClocklessTimekeeper(t)
	tk.restored = make(chan struct{})
	tk.saveMonotonic = 100000
	tk.saveRealtime = 400000
	tk.SetClocks(c, params)
	defer tk.Destroy()

	// The monotonic clock should jump ahead by 200000 to 300000.
	//
	// The new system monotonic time (900000) is irrelevant to what the app
	// sees.
	now, err := tk.GetTime(sentrytime.Monotonic)
	if err != nil {
		t.Errorf("GetTime err got %v want nil", err)
	}
	if now != 300000 {
		t.Errorf("GetTime got %d want 300000", now)
	}
}

// TestTimekeeperMonotonicJumpBackwards tests that monotonic time does not jump
// backwards when realtime goes backwards.
func TestTimekeeperMonotonicJumpBackwards(t *testing.T) {
	c := &mockClocks{
		monotonic: 900000,
		realtime:  400000,
	}

	tk, params := stateTestClocklessTimekeeper(t)
	tk.restored = make(chan struct{})
	tk.saveMonotonic = 100000
	tk.saveRealtime = 600000
	tk.SetClocks(c, params)
	defer tk.Destroy()

	// The monotonic clock should remain at 100000.
	//
	// The new system monotonic time (900000) is irrelevant to what the app
	// sees and we don't want to jump the monotonic clock backwards like
	// realtime did.
	now, err := tk.GetTime(sentrytime.Monotonic)
	if err != nil {
		t.Errorf("GetTime err got %v want nil", err)
	}
	if now != 100000 {
		t.Errorf("GetTime got %d want 100000", now)
	}
}

// TestTimekeeperAfterFunc tests that AfterFunc creates a working timer that
// fires when the monotonic clock advances past its deadline.
func TestTimekeeperAfterFunc(t *testing.T) {
	c := &mockClocks{}

	tk, params := stateTestClocklessTimekeeper(t)
	tk.SetClocks(c, params)
	defer tk.Destroy()

	fired := make(chan struct{})
	timer := tk.AfterFunc(20*time.Millisecond, func() {
		close(fired)
	})
	defer timer.Stop()

	// Advance monotonic time by 50ms past the 20ms deadline.
	c.mu.Lock()
	c.monotonic += (50 * time.Millisecond).Nanoseconds()
	c.mu.Unlock()

	select {
	case <-fired:
		// Success.
	case <-time.After(2 * time.Second):
		t.Fatal("timer callback did not fire after monotonic clock advanced")
	}
}

// TestTimekeeperTimerStopReset tests that Stop prevents a timer from firing and
// Reset successfully re-arms it.
func TestTimekeeperTimerStopReset(t *testing.T) {
	c := &mockClocks{}

	tk, params := stateTestClocklessTimekeeper(t)
	tk.SetClocks(c, params)
	defer tk.Destroy()

	var firedCount atomicbitops.Int32
	timer := tk.AfterFunc(50*time.Millisecond, func() {
		firedCount.Add(1)
	})

	// Stopping before the deadline should return true.
	if !timer.Stop() {
		t.Fatalf("Stop() on active timer got false, want true")
	}

	// Advance monotonic time well past the original deadline.
	c.mu.Lock()
	c.monotonic += (100 * time.Millisecond).Nanoseconds()
	c.mu.Unlock()

	time.Sleep(50 * time.Millisecond)
	if count := firedCount.Load(); count != 0 {
		t.Fatalf("stopped timer fired %d times, want 0", count)
	}

	// Re-arm via Reset for 20ms.
	timer.Reset(20 * time.Millisecond)

	// Advance time past the new reset deadline.
	c.mu.Lock()
	c.monotonic += (50 * time.Millisecond).Nanoseconds()
	c.mu.Unlock()

	// Wait for the re-armed timer to fire.
	deadline := time.Now().Add(2 * time.Second)
	for firedCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if count := firedCount.Load(); count != 1 {
		t.Fatalf("re-armed timer fired %d times, want 1", count)
	}

	// Stop after expiration should return false.
	if timer.Stop() {
		t.Fatalf("Stop() on expired timer got true, want false")
	}
}

// TestTimekeeperPauseResumeTimers tests that Pausing freezes the AfterFunc scheduler
// and Resuming immediately fires timers that became overdue during the pause.
func TestTimekeeperPauseResumeTimers(t *testing.T) {
	c := &mockClocks{}

	tk, params := stateTestClocklessTimekeeper(t)
	tk.SetClocks(c, params)
	defer tk.Destroy()

	fired := make(chan struct{})
	timer := tk.AfterFunc(50*time.Millisecond, func() {
		close(fired)
	})
	defer timer.Stop()

	// Pause the timekeeper.
	tk.Pause()

	// Advance clock by 100ms while paused.
	c.mu.Lock()
	c.monotonic += (100 * time.Millisecond).Nanoseconds()
	c.mu.Unlock()

	// Ensure it does not fire while paused.
	select {
	case <-fired:
		t.Fatal("timer fired while Timekeeper was paused")
	case <-time.After(30 * time.Millisecond):
		// Expected.
	}

	// Resume the timekeeper. Overdue timer should fire immediately.
	tk.Resume(params)

	select {
	case <-fired:
		// Success.
	case <-time.After(2 * time.Second):
		t.Fatal("overdue timer did not fire after Resume")
	}
}

// TestTimekeeperPauseWaitsForInFlightAfterFunc tests that Pause() blocks until
// any already-running AfterFunc callback goroutine finishes.
func TestTimekeeperPauseWaitsForInFlightAfterFunc(t *testing.T) {
	c := &mockClocks{}
	tk, params := stateTestClocklessTimekeeper(t)
	tk.SetClocks(c, params)
	defer tk.Destroy()

	started := make(chan struct{})
	done := make(chan struct{})
	tk.AfterFunc(10*time.Millisecond, func() {
		close(started)
		time.Sleep(50 * time.Millisecond)
		close(done)
	})

	// Trigger the callback.
	c.mu.Lock()
	c.monotonic += (20 * time.Millisecond).Nanoseconds()
	c.mu.Unlock()

	// Wait until the callback has actually started executing in its goroutine.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("AfterFunc callback never started")
	}

	tk.Pause()

	// Pause() must have blocked until the callback finished.
	select {
	case <-done:
		// Success.
	default:
		t.Fatal("Pause() returned while AfterFunc was still running")
	}
}
