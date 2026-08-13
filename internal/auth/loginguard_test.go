package auth

import (
	"testing"
	"time"
)

func TestLoginGuardAllowsUntilLimit(t *testing.T) {
	g := NewLoginGuard()
	const ip = "203.0.113.7"

	for i := 0; i < MaxFailures-1; i++ {
		if ok, _ := g.Allow(ip); !ok {
			t.Fatalf("locked out after only %d failures, want %d", i, MaxFailures)
		}
		if lock := g.Fail(ip); lock != 0 {
			t.Fatalf("failure %d triggered a lockout of %v too early", i+1, lock)
		}
	}

	// The final failure locks the door.
	if ok, _ := g.Allow(ip); !ok {
		t.Fatal("locked before the final permitted attempt")
	}
	lock := g.Fail(ip)
	if lock != BaseLockout {
		t.Errorf("lockout = %v, want %v", lock, BaseLockout)
	}
	ok, retry := g.Allow(ip)
	if ok {
		t.Error("attempt allowed while locked out")
	}
	if retry <= 0 || retry > BaseLockout {
		t.Errorf("retryAfter = %v, want (0, %v]", retry, BaseLockout)
	}
}

func TestLoginGuardIsolatesKeys(t *testing.T) {
	g := NewLoginGuard()
	for i := 0; i < MaxFailures; i++ {
		g.Fail("attacker")
	}
	if ok, _ := g.Allow("attacker"); ok {
		t.Error("attacker should be locked out")
	}
	if ok, _ := g.Allow("operator"); !ok {
		t.Error("one client's lockout must not affect another")
	}
}

func TestLoginGuardSucceedClearsFailures(t *testing.T) {
	g := NewLoginGuard()
	const ip = "198.51.100.4"
	for i := 0; i < MaxFailures-1; i++ {
		g.Fail(ip)
	}
	g.Succeed(ip)

	// The streak is gone, so a full fresh run of failures is needed to lock.
	for i := 0; i < MaxFailures-1; i++ {
		if lock := g.Fail(ip); lock != 0 {
			t.Fatalf("locked after %d failures post-success; counter was not reset", i+1)
		}
	}
}

func TestLoginGuardLockoutDoubles(t *testing.T) {
	g := NewLoginGuard()
	const ip = "192.0.2.1"

	trip := func() time.Duration {
		var last time.Duration
		for i := 0; i < MaxFailures; i++ {
			last = g.Fail(ip)
		}
		return last
	}

	if got := trip(); got != BaseLockout {
		t.Fatalf("first lockout = %v, want %v", got, BaseLockout)
	}
	if got := trip(); got != BaseLockout*2 {
		t.Errorf("second lockout = %v, want %v", got, BaseLockout*2)
	}
	if got := trip(); got != BaseLockout*4 {
		t.Errorf("third lockout = %v, want %v", got, BaseLockout*4)
	}
}

func TestLoginGuardLockoutIsCapped(t *testing.T) {
	g := NewLoginGuard()
	const ip = "192.0.2.99"
	var last time.Duration
	// Enough rounds that unbounded doubling would overflow the shift.
	for round := 0; round < 20; round++ {
		for i := 0; i < MaxFailures; i++ {
			last = g.Fail(ip)
		}
	}
	if last != MaxLockout {
		t.Errorf("lockout = %v, want it capped at %v", last, MaxLockout)
	}
	if last <= 0 {
		t.Error("lockout went non-positive; the shift overflowed")
	}
}

func TestLoginGuardSweepDropsStaleRecords(t *testing.T) {
	g := NewLoginGuard()
	g.Fail("old")

	g.mu.Lock()
	st := g.attempts["old"]
	st.lastFail = time.Now().Add(-2 * MaxLockout)
	st.lockedUntil = time.Now().Add(-2 * MaxLockout)
	g.mu.Unlock()

	g.Sweep()

	g.mu.Lock()
	_, still := g.attempts["old"]
	g.mu.Unlock()
	if still {
		t.Error("Sweep should drop records that have gone quiet")
	}
}
