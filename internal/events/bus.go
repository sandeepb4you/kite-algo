package events

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultBuffer is the per-subscriber queue depth used when Subscribe is given
// a non-positive size. Deep enough to absorb a burst of ticks arriving while a
// subscriber renders a frame; shallow enough that a wedged subscriber is
// detected quickly rather than growing unbounded.
const DefaultBuffer = 256

// Bus is a fan-out event bus with bounded, lossy delivery.
//
// Publish never blocks. When a subscriber's queue is full its events are
// dropped and counted, so a slow or wedged consumer degrades only itself. This
// is deliberate: the publisher is the market-data goroutine, and back-pressure
// from a browser tab must never reach it.
//
// A Bus is safe for concurrent use. The zero value is not usable; call NewBus.
type Bus struct {
	logger *slog.Logger

	mu     sync.RWMutex
	subs   map[int]*subscription
	nextID int
	closed bool

	published atomic.Uint64
	dropped   atomic.Uint64

	// lastWarn rate-limits the "dropping events" log so a persistently slow
	// subscriber cannot flood the log with one line per dropped tick.
	warnMu   sync.Mutex
	lastWarn time.Time
}

type subscription struct {
	id      int
	ch      chan Event
	kinds   map[Kind]struct{} // nil means "every kind"
	dropped atomic.Uint64
}

// wants reports whether this subscription is interested in k.
func (s *subscription) wants(k Kind) bool {
	if s.kinds == nil {
		return true
	}
	_, ok := s.kinds[k]
	return ok
}

// NewBus returns an empty Bus. logger may be nil.
func NewBus(logger *slog.Logger) *Bus {
	return &Bus{logger: logger, subs: make(map[int]*subscription)}
}

// Subscribe registers a consumer and returns its channel plus a cancel func.
//
// If kinds is empty the subscriber receives every kind. buffer sets the queue
// depth; pass 0 for DefaultBuffer.
//
// The caller MUST invoke cancel when done — typically deferred — or the
// subscription leaks and the Bus keeps trying to deliver to it. cancel is
// idempotent and closes the returned channel, so a `range` over it terminates.
//
// Consume the channel promptly. Events that arrive while the buffer is full are
// dropped, not queued.
func (b *Bus) Subscribe(buffer int, kinds ...Kind) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	s := &subscription{ch: make(chan Event, buffer)}
	if len(kinds) > 0 {
		s.kinds = make(map[Kind]struct{}, len(kinds))
		for _, k := range kinds {
			s.kinds[k] = struct{}{}
		}
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(s.ch)
		return s.ch, func() {}
	}
	b.nextID++
	s.id = b.nextID
	b.subs[s.id] = s
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.subs[s.id]; ok {
				delete(b.subs, s.id)
				close(s.ch)
			}
			b.mu.Unlock()
		})
	}
	return s.ch, cancel
}

// Publish delivers e to every interested subscriber, dropping it for any whose
// queue is full. It never blocks.
//
// At is stamped with the current time when the caller left it zero.
func (b *Bus) Publish(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	b.published.Add(1)

	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for _, s := range b.subs {
		if !s.wants(e.Kind) {
			continue
		}
		select {
		case s.ch <- e:
		default:
			s.dropped.Add(1)
			b.dropped.Add(1)
			b.warnSlow(s)
		}
	}
}

// warnSlow logs at most once a minute that delivery is falling behind. Called
// with b.mu held for reading, so it must not touch b.subs.
func (b *Bus) warnSlow(s *subscription) {
	if b.logger == nil {
		return
	}
	b.warnMu.Lock()
	defer b.warnMu.Unlock()
	if time.Since(b.lastWarn) < time.Minute {
		return
	}
	b.lastWarn = time.Now()
	b.logger.Warn("event bus dropping events for a slow subscriber",
		"subscriber", s.id, "dropped_this_subscriber", s.dropped.Load(),
		"dropped_total", b.dropped.Load())
}

// Close removes every subscription and closes their channels. Subsequent
// Publish calls are no-ops and Subscribe returns an already-closed channel.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, s := range b.subs {
		close(s.ch)
		delete(b.subs, id)
	}
}

// Stats reports lifetime counters, for a health endpoint. A non-zero dropped
// count means some subscriber is not keeping up.
func (b *Bus) Stats() (published, dropped uint64, subscribers int) {
	b.mu.RLock()
	n := len(b.subs)
	b.mu.RUnlock()
	return b.published.Load(), b.dropped.Load(), n
}
