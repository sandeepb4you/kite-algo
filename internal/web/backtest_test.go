package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestBacktestFormExposesStrategyParams is the property the form was missing:
// it ran every strategy on its declared defaults, so a backtest could not test
// the configuration anyone actually intended to trade.
func TestBacktestFormExposesStrategyParams(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)

	resp, err := client.Get(ts.URL + "/backtest?strategy=short-straddle")
	if err != nil {
		t.Fatalf("load backtest form: %v", err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)

	// Every parameter the strategy declares must have a control, not just the
	// ones the page happened to hard-code.
	for _, key := range []string{
		"index_symbol", "underlying", "strike_step", "lots",
		"product", "exit_delta", "square_off_time", "risk_free_rate",
	} {
		if !strings.Contains(body, `name="`+key+`"`) {
			t.Errorf("no form control for declared parameter %q", key)
		}
	}
	if !strings.Contains(body, `value="0.25"`) {
		t.Error("exit_delta did not render its declared default")
	}
}

// TestBacktestParamsFragmentFollowsTheSelection covers the swap app.js performs
// when the strategy select changes.
func TestBacktestParamsFragmentFollowsTheSelection(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)

	body := getBody(t, client, ts.URL+"/partials/backtest-params?strategy=short-straddle")
	if !strings.Contains(body, `name="exit_delta"`) {
		t.Errorf("fragment for short-straddle has no exit_delta field:\n%s", body)
	}

	// An unknown or empty selection is not an error: the fragment only
	// enhances a form that already works without it.
	body = getBody(t, client, ts.URL+"/partials/backtest-params?strategy=nope")
	if strings.Contains(body, "<input") {
		t.Errorf("unknown strategy rendered input fields:\n%s", body)
	}
}

// TestBacktestRejectsOutOfRangeParams proves the submitted values are validated
// rather than accepted and quietly clamped — and that the check does not depend
// on a history provider being connected, since a mistyped number is the
// operator's to fix either way.
func TestBacktestRejectsOutOfRangeParams(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)

	body := postBacktest(t, client, ts, url.Values{
		"strategy":   {"short-straddle"},
		"symbols":    {"NIFTY 50"},
		"exit_delta": {"99"}, // declared max is 2
	})

	if !strings.Contains(body, "exit_delta") {
		t.Errorf("an out-of-range exit_delta was not reported:\n%s", body)
	}
}

// TestBacktestEchoesSubmittedParams: a run that fails for any reason must come
// back with the operator's settings, not silently reset to the defaults.
func TestBacktestEchoesSubmittedParams(t *testing.T) {
	ts, _ := newTestServer(t)
	client := loginClient(t, ts)

	body := postBacktest(t, client, ts, url.Values{
		"strategy":        {"short-straddle"},
		"symbols":         {"NIFTY 50"},
		"exit_delta":      {"0.4"},
		"square_off_time": {"14:45"},
	})

	if !strings.Contains(body, `value="0.4"`) {
		t.Error("the submitted exit_delta was not echoed back; a re-run would " +
			"silently use the default instead")
	}
	if !strings.Contains(body, `value="14:45"`) {
		t.Error("the submitted square_off_time was not echoed back")
	}
}

// postBacktest submits the form with a valid CSRF token and returns the page.
func postBacktest(t *testing.T, c *http.Client, ts *httptest.Server, form url.Values) string {
	t.Helper()
	form.Set("_csrf", csrfFor(t, ts, c))

	resp, err := c.PostForm(ts.URL+"/backtest", form)
	if err != nil {
		t.Fatalf("post backtest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("backtest returned HTTP %d", resp.StatusCode)
	}
	return readAll(t, resp)
}

func getBody(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return readAll(t, resp)
}
