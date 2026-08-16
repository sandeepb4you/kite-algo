package history

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// recorder counts capture invocations and the days they were asked for.
type recorder struct {
	mu   sync.Mutex
	days []string
	err  error
}

func (r *recorder) capture(_ context.Context, day time.Time) (CaptureReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.days = append(r.days, day.In(IST).Format("2006-01-02"))
	if r.err != nil {
		return CaptureReport{}, r.err
	}
	return CaptureReport{Day: day, Contracts: 3}, nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.days)
}

func newTestSched(t *testing.T, runAt string, r *recorder, ready func() bool) *CaptureScheduler {
	t.Helper()
	s, err := NewCaptureScheduler(runAt, r.capture, ready, nil)
	if err != nil {
		t.Fatalf("NewCaptureScheduler: %v", err)
	}
	return s
}

func TestSchedulerWaitsUntilRunAt(t *testing.T) {
	r := &recorder{}
	s := newTestSched(t, "15:40", r, nil)

	// 15:39 IST — one minute early.
	s.tick(context.Background(), time.Date(2026, 8, 14, 15, 39, 0, 0, IST))
	if r.count() != 0 {
		t.Errorf("captured before run_at: %v", r.days)
	}

	s.tick(context.Background(), time.Date(2026, 8, 14, 15, 40, 0, 0, IST))
	if r.count() != 1 {
		t.Errorf("did not capture at run_at, got %d runs", r.count())
	}
}

// Re-running would burn historical quota re-fetching what coverage already has.
func TestSchedulerRunsOncePerDay(t *testing.T) {
	r := &recorder{}
	s := newTestSched(t, "15:40", r, nil)

	for _, min := range []int{40, 41, 42, 55} {
		s.tick(context.Background(), time.Date(2026, 8, 14, 15, min, 0, 0, IST))
	}
	if r.count() != 1 {
		t.Errorf("ran %d times in one day, want 1 (%v)", r.count(), r.days)
	}

	// Next day it must run again.
	s.tick(context.Background(), time.Date(2026, 8, 17, 15, 40, 0, 0, IST))
	if r.count() != 2 {
		t.Errorf("did not run on the next day, got %d runs", r.count())
	}
}

// The whole point of a one-minute tick rather than a sleep-until timer: a
// process that was down at 15:40, or an operator who logs in at 20:00, must
// still capture the day. Tomorrow is too late.
func TestSchedulerCatchesUpAfterALateStart(t *testing.T) {
	r := &recorder{}
	s := newTestSched(t, "15:40", r, nil)

	s.tick(context.Background(), time.Date(2026, 8, 14, 20, 15, 0, 0, IST))
	if r.count() != 1 {
		t.Fatalf("late start did not capture, got %d runs", r.count())
	}
	if r.days[0] != "2026-08-14" {
		t.Errorf("captured %s, want 2026-08-14", r.days[0])
	}
}

// Without a Zerodha session there is nothing to fetch through. The scheduler
// must keep waiting rather than marking the day done.
func TestSchedulerWaitsForSessionThenCaptures(t *testing.T) {
	r := &recorder{}
	var connected bool
	s := newTestSched(t, "15:40", r, func() bool { return connected })

	s.tick(context.Background(), time.Date(2026, 8, 14, 15, 40, 0, 0, IST))
	if r.count() != 0 {
		t.Fatal("captured with no session")
	}

	connected = true
	s.tick(context.Background(), time.Date(2026, 8, 14, 15, 41, 0, 0, IST))
	if r.count() != 1 {
		t.Errorf("did not capture once the session came up, got %d runs", r.count())
	}
}

// A failed run must not count as done, or a transient outage at 15:40 would
// silently cost the day.
func TestSchedulerRetriesAfterFailure(t *testing.T) {
	r := &recorder{err: fmt.Errorf("kite down")}
	s := newTestSched(t, "15:40", r, nil)

	s.tick(context.Background(), time.Date(2026, 8, 14, 15, 40, 0, 0, IST))
	if r.count() != 1 {
		t.Fatalf("got %d runs, want 1", r.count())
	}

	r.mu.Lock()
	r.err = nil
	r.mu.Unlock()

	s.tick(context.Background(), time.Date(2026, 8, 14, 15, 41, 0, 0, IST))
	if r.count() != 2 {
		t.Errorf("did not retry after a failure, got %d runs", r.count())
	}

	// And having succeeded, it stops.
	s.tick(context.Background(), time.Date(2026, 8, 14, 15, 42, 0, 0, IST))
	if r.count() != 2 {
		t.Errorf("kept running after success, got %d runs", r.count())
	}
}

func TestSchedulerRejectsBadRunAt(t *testing.T) {
	r := &recorder{}
	if _, err := NewCaptureScheduler("half past three", r.capture, nil, nil); err == nil {
		t.Error("accepted an unparseable run_at")
	}
}

func TestSchedulerStatusReportsLastRun(t *testing.T) {
	r := &recorder{}
	s := newTestSched(t, "15:40", r, nil)
	s.tick(context.Background(), time.Date(2026, 8, 14, 15, 40, 0, 0, IST))

	st := s.Status()
	if st.Contracts != 3 {
		t.Errorf("Contracts = %d, want 3", st.Contracts)
	}
	if st.Error != "" {
		t.Errorf("Error = %q, want empty", st.Error)
	}
	if st.Running {
		t.Error("still marked running after the run finished")
	}
}
