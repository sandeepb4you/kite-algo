package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kite-algo/internal/history"
)

func sampleReport() history.CaptureReport {
	return history.CaptureReport{
		Day:       time.Date(2026, 8, 19, 15, 40, 0, 0, history.IST),
		Contracts: 312,
		Candles:   6240,
		Duration:  48 * time.Second,
		Underlying: []history.UnderlyingReport{
			{Underlying: "NIFTY", Spot: 24512, Low: 23500, High: 25500,
				Contracts: 164, Candles: 3280},
			{Underlying: "SENSEX", Spot: 80120, Low: 78000, High: 82000,
				Contracts: 148, Candles: 2960},
		},
	}
}

// A good day reports per-underlying, not just a total.
//
// The total hides the failure that matters: SENSEX contributing nothing looks
// identical to a good day once it is summed, and BSE contracts are exactly the
// ones that used to be missed wholesale.
func TestCaptureMessageBreaksDownByUnderlying(t *testing.T) {
	msg := captureMessage(sampleReport(), nil, captureOK)

	for _, want := range []string{
		"complete", "19 Aug 2026",
		"NIFTY: 164 contracts, 3280 candles",
		"SENSEX: 148 contracts, 2960 candles",
		"Total 312 contracts, 6240 candles in 48s",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "CRITICAL") {
		t.Errorf("a successful capture is marked critical:\n%s", msg)
	}
}

// A failed capture must say what was lost, because the loss is permanent and the
// operator has a narrow window to salvage the contracts that have not expired.
func TestCaptureMessageOnFailureExplainsTheStakes(t *testing.T) {
	rep := sampleReport()
	msg := captureMessage(rep, errors.New("no Zerodha session"), captureFailed)

	if !strings.Contains(msg, "CRITICAL") {
		t.Errorf("a failed capture is not marked critical:\n%s", msg)
	}
	if !strings.Contains(msg, "no Zerodha session") {
		t.Errorf("the cause is missing:\n%s", msg)
	}
	if !strings.Contains(msg, "will not serve it again") {
		t.Errorf("the consequence is missing:\n%s", msg)
	}
	// A total is misleading on a failed run — those candles may not be stored.
	if strings.Contains(msg, "Total 312") {
		t.Errorf("a failed capture reported a total as though it succeeded:\n%s", msg)
	}
}

// A per-underlying failure has to surface even when the run as a whole returned
// no error.
func TestCaptureMessageSurfacesAPartialFailure(t *testing.T) {
	rep := sampleReport()
	rep.Failures = 4
	rep.Underlying[1].Err = "instrument master has no BFO contracts"

	msg := captureMessage(rep, nil, capturePartial)
	if !strings.Contains(msg, "4 failure(s)") {
		t.Errorf("failure count missing:\n%s", msg)
	}
	if !strings.Contains(msg, "SENSEX: FAILED") {
		t.Errorf("the failing underlying is not named:\n%s", msg)
	}
}

// A skipped run says nothing. A weekend produces one every day, and "nothing
// happened, as designed" is the traffic that trains an operator to ignore the
// channel — which then loses the real alert.
func TestSkippedCaptureIsSilent(t *testing.T) {
	ctx := context.Background()
	a := notifyTestApp(t)
	out := &recordingAlerter{}
	a.alerts = out

	rep := sampleReport()
	rep.Skipped = "2026-08-16 is not a trading day"
	a.notifyCaptureDone(ctx, rep, nil)

	if got := out.messages(); len(got) != 0 {
		t.Errorf("a skipped capture sent %d message(s): %v", len(got), got)
	}
}

// The de-duplication that makes this safe to hook into a retrying scheduler.
//
// A failed capture retries every MINUTE until midnight, because the scheduler
// only records successes. Without this the channel would receive hundreds of
// identical messages in an afternoon and be muted for good.
func TestRepeatedIdenticalCaptureOutcomeIsAnnouncedOnce(t *testing.T) {
	ctx := context.Background()
	a := notifyTestApp(t)
	out := &recordingAlerter{}
	a.alerts = out

	rep := sampleReport()
	capErr := errors.New("no Zerodha session")

	for i := 0; i < 5; i++ {
		a.notifyCaptureDone(ctx, rep, capErr)
	}
	if got := out.messages(); len(got) != 1 {
		t.Fatalf("a retrying capture sent %d messages, want 1", len(got))
	}

	// ...but the recovery IS worth saying, on the same day.
	a.notifyCaptureDone(ctx, rep, nil)
	got := out.messages()
	if len(got) != 2 {
		t.Fatalf("recovery after failure was suppressed: %d messages", len(got))
	}
	if !strings.Contains(got[1], "complete") {
		t.Errorf("the second message is not the success:\n%s", got[1])
	}

	// And a later day is a separate event even with the same outcome.
	next := sampleReport()
	next.Day = next.Day.AddDate(0, 0, 1)
	a.notifyCaptureDone(ctx, next, nil)
	if got := out.messages(); len(got) != 3 {
		t.Errorf("the next day's capture was suppressed: %d messages", len(got))
	}
}

// A send failure must not be recorded as announced, or a transient Telegram
// outage during a FAILED capture loses the notice about it entirely.
func TestFailedCaptureNotificationIsRetried(t *testing.T) {
	ctx := context.Background()
	a := notifyTestApp(t)
	a.alerts = &recordingAlerter{err: errors.New("telegram unreachable")}

	a.notifyCaptureDone(ctx, sampleReport(), nil)

	if st := a.loadCaptureNotifyState(ctx); st.Day != "" || st.Outcome != "" {
		t.Errorf("a failed notification was recorded as sent: %+v", st)
	}
}

// With no channel configured this must be inert rather than panicking on the
// nil interface — the capture hook runs on every deployment, configured or not.
func TestCaptureNotifyWithoutAChannelIsInert(t *testing.T) {
	a := notifyTestApp(t)
	a.alerts = nil
	a.notifyCaptureDone(context.Background(), sampleReport(), nil)
}
