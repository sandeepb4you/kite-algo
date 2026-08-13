package events

import (
	"sync"
	"testing"
	"time"
)

func TestBusDeliversToAllSubscribers(t *testing.T) {
	b := NewBus(nil)
	defer b.Close()

	c1, cancel1 := b.Subscribe(4)
	defer cancel1()
	c2, cancel2 := b.Subscribe(4)
	defer cancel2()

	b.Publish(Event{Kind: KindFill, Symbol: "NIFTY24AUG24500CE"})

	for i, ch := range []<-chan Event{c1, c2} {
		select {
		case e := <-ch:
			if e.Kind != KindFill || e.Symbol != "NIFTY24AUG24500CE" {
				t.Errorf("subscriber %d got %+v", i, e)
			}
			if e.At.IsZero() {
				t.Errorf("subscriber %d: At not stamped", i)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d received nothing", i)
		}
	}
}

func TestBusFiltersByKind(t *testing.T) {
	b := NewBus(nil)
	defer b.Close()

	ch, cancel := b.Subscribe(8, KindOrder, KindFill)
	defer cancel()

	b.Publish(Event{Kind: KindTick, Symbol: "ignored"})
	b.Publish(Event{Kind: KindOrder, Symbol: "wanted"})

	select {
	case e := <-ch:
		if e.Kind != KindOrder {
			t.Fatalf("got %s, want the tick to have been filtered out", e.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("received nothing")
	}
}

// TestBusPublishNeverBlocks is the load-bearing guarantee: Publish runs on the
// market-data goroutine, so a subscriber that has stopped reading must not be
// able to stall it.
func TestBusPublishNeverBlocks(t *testing.T) {
	b := NewBus(nil)
	defer b.Close()

	_, cancel := b.Subscribe(1) // deliberately never drained
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10_000; i++ {
			b.Publish(Event{Kind: KindTick})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a full subscriber queue")
	}

	published, dropped, _ := b.Stats()
	if published != 10_000 {
		t.Errorf("published = %d, want 10000", published)
	}
	if dropped == 0 {
		t.Error("expected drops against a queue of depth 1 that was never drained")
	}
}

func TestBusCancelIsIdempotentAndClosesChannel(t *testing.T) {
	b := NewBus(nil)
	defer b.Close()

	ch, cancel := b.Subscribe(2)
	cancel()
	cancel() // must not panic on a double close

	if _, open := <-ch; open {
		t.Error("channel should be closed after cancel")
	}

	// Publishing to a cancelled subscription must be harmless.
	b.Publish(Event{Kind: KindTick})

	if _, _, n := b.Stats(); n != 0 {
		t.Errorf("subscribers = %d after cancel, want 0", n)
	}
}

func TestBusConcurrentSubscribeAndPublish(t *testing.T) {
	b := NewBus(nil)
	defer b.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel := b.Subscribe(16)
			defer cancel()
			deadline := time.After(200 * time.Millisecond)
			for {
				select {
				case <-ch:
				case <-deadline:
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5_000; i++ {
			b.Publish(Event{Kind: KindTick})
		}
	}()
	wg.Wait()
}

func TestBusCloseStopsDelivery(t *testing.T) {
	b := NewBus(nil)
	ch, cancel := b.Subscribe(4)
	defer cancel()

	b.Close()
	if _, open := <-ch; open {
		t.Error("Close should close subscriber channels")
	}
	b.Publish(Event{Kind: KindTick}) // must not panic on a closed channel
	b.Close()                        // idempotent
}

func TestNopPublisher(t *testing.T) {
	var p Publisher = Nop{}
	p.Publish(Event{Kind: KindTick}) // must not panic
}
