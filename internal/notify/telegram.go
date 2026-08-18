// Package notify delivers operator alerts to somewhere outside the application.
//
// Every warning this platform produces is visible only to someone already
// looking at it: the missing-session banner renders in the page chrome, the
// startup warnings go to the container log, and /healthz has to be polled. That
// is fine for anything an operator discovers while working, and useless for the
// one class of problem that matters most here — a condition that starts while
// nobody is watching and gets more expensive the longer it runs.
//
// The daily Zerodha login is exactly that. The token dies around 06:00 IST, the
// UI keeps serving pages as though nothing were wrong, and the option capture at
// 15:40 quietly does nothing. Contracts that expire that day take their price
// history with them and Kite will not serve it again, so a missed login is not a
// delay — it is a permanent hole in the data the backtester runs on.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// telegramAPI is the base URL for the Bot API. A variable so tests can point it
// at an httptest server; there is no reason to change it in production.
var telegramAPI = "https://api.telegram.org"

// Telegram sends messages to one chat through a bot.
//
// Telegram is a reasonable choice for this: delivery is a single HTTPS POST with
// no account to provision, and it reaches a phone that is already unlocked and
// in a hand — which is the whole point, since the fix (log in to Zerodha) is
// something the operator does in a browser within a minute of being told.
type Telegram struct {
	token  string
	chatID string
	http   *http.Client
	log    *slog.Logger
}

// NewTelegram builds a sender. An empty token or chat ID yields a sender that
// reports itself unconfigured rather than one that fails on every send: the
// alert is optional, and a platform without it must still start.
func NewTelegram(token, chatID string, log *slog.Logger) *Telegram {
	return &Telegram{
		token:  strings.TrimSpace(token),
		chatID: strings.TrimSpace(chatID),
		// A short timeout on purpose. This runs on a timer inside the trading
		// process; a hung request to a third party must not pin a goroutine for
		// minutes, and a missed alert is retried on the next tick anyway.
		http: &http.Client{Timeout: 10 * time.Second},
		log:  log,
	}
}

// Configured reports whether both halves of the credential are present.
func (t *Telegram) Configured() bool { return t.token != "" && t.chatID != "" }

// Send posts one message to the configured chat.
//
// Plain text, deliberately: Telegram's parse modes require escaping and a stray
// underscore in a symbol name would turn a delivered alert into a 400. Bare URLs
// are linkified by the client anyway, so the only thing markup would buy is bold
// text, at the cost of a class of silent delivery failure. Link previews are off
// so an alert does not render a screenshot card of the trading UI in a chat.
func (t *Telegram) Send(ctx context.Context, text string) error {
	if !t.Configured() {
		return fmt.Errorf("telegram: not configured")
	}

	form := url.Values{
		"chat_id":                  {t.chatID},
		"text":                     {text},
		"disable_web_page_preview": {"true"},
	}
	endpoint := telegramAPI + "/bot" + t.token + "/sendMessage"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("telegram: build request: %w", t.scrub(err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: send: %w", t.scrub(err))
	}
	defer resp.Body.Close()

	// Read the body before looking at the status: the Bot API describes what it
	// refused in the body, and "400 Bad Request" alone gives an operator nothing
	// to act on — a wrong chat ID and a bot that was never started by the user
	// are both 400 and have different fixes.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

	var env struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		ErrorCode   int    `json:"error_code"`
	}
	_ = json.Unmarshal(body, &env)

	if resp.StatusCode != http.StatusOK || !env.OK {
		detail := env.Description
		if detail == "" {
			detail = strings.TrimSpace(string(body))
		}
		return fmt.Errorf("telegram: refused (HTTP %d): %s", resp.StatusCode, detail)
	}
	return nil
}

// scrub removes the bot token from an error before it can be logged.
//
// net/http puts the request URL into its errors, and the token is IN that URL —
// so the obvious `fmt.Errorf("...: %w", err)` writes a working bot credential
// into the container log, which is shipped, rotated and pasted into issues. This
// is the only reason Send does not simply wrap the error it was given.
func (t *Telegram) scrub(err error) error {
	if err == nil || t.token == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), t.token, "<bot-token>")
	return fmt.Errorf("%s", msg)
}
