package auth

import (
	"sync"
	"time"
)

// Login throttling. One operator logs in rarely, so these can be tight.
const (
	// MaxFailures before the door locks.
	MaxFailures = 5
	// BaseLockout after MaxFailures; doubles on each subsequent lockout.
	BaseLockout = 15 * time.Minute
	// MaxLockout caps the doubling.
	MaxLockout = 24 * time.Hour
	// FailureWindow is how long a failure counts against the total. Occasional
	// typos spread over days should not accumulate into a lockout.
	FailureWindow = time.Hour
)

// LoginGuard throttles password attempts per client.
//
// Keying on IP is only meaningful when the client address is trustworthy. Behind
// a reverse proxy every request carries the proxy's address, so the caller must
// pass a real client IP (see web.clientIP) and must only trust a forwarded
// header when a proxy it controls sets it — otherwise an attacker spoofs the
// header and every attempt looks like a fresh client.
type LoginGuard struct {
	mu       sync.Mutex
	attempts map[string]*attemptState
}

type attemptState struct {
	failures    int
	firstFail   time.Time
	lastFail    time.Time
	lockedUntil time.Time
	lockCount   int
}

// NewLoginGuard returns an empty guard.
func NewLoginGuard() *LoginGuard {
	return &LoginGuard{attempts: make(map[string]*attemptState)}
}

// Allow reports whether key may attempt a login now. When it may not, retryAfter
// says how long until it can.
func (g *LoginGuard) Allow(key string) (ok bool, retryAfter time.Duration) {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()

	st, found := g.attempts[key]
	if !found {
		return true, 0
	}
	if now.Before(st.lockedUntil) {
		return false, st.lockedUntil.Sub(now)
	}
	// The failure streak decays once the window passes without a new failure.
	if !st.lastFail.IsZero() && now.Sub(st.lastFail) > FailureWindow {
		st.failures = 0
		st.firstFail = time.Time{}
	}
	return true, 0
}

// Fail records a failed attempt, locking the key once failures reach the limit.
// It returns the lockout duration applied, or zero if the key is not yet locked.
func (g *LoginGuard) Fail(key string) time.Duration {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()

	st, found := g.attempts[key]
	if !found {
		st = &attemptState{}
		g.attempts[key] = st
	}
	if st.failures == 0 {
		st.firstFail = now
	}
	st.failures++
	st.lastFail = now

	if st.failures < MaxFailures {
		return 0
	}

	// Each successive lockout doubles, capped, so a persistent attacker slows
	// to a crawl while a fumbling operator waits only the base interval.
	lock := BaseLockout << st.lockCount
	if lock > MaxLockout || lock <= 0 {
		lock = MaxLockout
	}
	st.lockCount++
	st.failures = 0
	st.lockedUntil = now.Add(lock)
	return lock
}

// Succeed clears the failure record for key after a successful login.
func (g *LoginGuard) Succeed(key string) {
	g.mu.Lock()
	delete(g.attempts, key)
	g.mu.Unlock()
}

// Sweep drops records that have gone quiet, so the map cannot grow without
// bound under a distributed guessing attack.
func (g *LoginGuard) Sweep() {
	cutoff := time.Now().Add(-MaxLockout)
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, st := range g.attempts {
		if st.lockedUntil.Before(cutoff) && st.lastFail.Before(cutoff) {
			delete(g.attempts, k)
		}
	}
}
