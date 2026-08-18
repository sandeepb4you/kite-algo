package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withAPI points the package at a stub Bot API for one test.
func withAPI(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	prev := telegramAPI
	telegramAPI = srv.URL
	t.Cleanup(func() {
		telegramAPI = prev
		srv.Close()
	})
	return srv
}

func TestTelegramSendsToTheConfiguredChat(t *testing.T) {
	var gotPath, gotChat, gotText, gotPreview string
	withAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotPath = r.URL.Path
		gotChat = r.FormValue("chat_id")
		gotText = r.FormValue("text")
		gotPreview = r.FormValue("disable_web_page_preview")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	})

	tg := NewTelegram("123:SECRET", "42", nil)
	if err := tg.Send(context.Background(), "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// The token travels in the PATH, which is why scrubbing errors matters.
	if gotPath != "/bot123:SECRET/sendMessage" {
		t.Errorf("path = %q", gotPath)
	}
	if gotChat != "42" {
		t.Errorf("chat_id = %q, want 42", gotChat)
	}
	if gotText != "hello" {
		t.Errorf("text = %q, want hello", gotText)
	}
	// Otherwise an alert renders a preview card of the trading UI in the chat.
	if gotPreview != "true" {
		t.Errorf("disable_web_page_preview = %q, want true", gotPreview)
	}
}

// Telegram answers a refusal with HTTP 200 and ok:false as often as with a 4xx,
// so a status-only check reports success for a message nobody received.
func TestTelegramTreatsOkFalseAsFailure(t *testing.T) {
	withAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"chat not found"}`))
	})

	tg := NewTelegram("123:SECRET", "42", nil)
	err := tg.Send(context.Background(), "hello")
	if err == nil {
		t.Fatal("a refused message was reported as sent")
	}
	// The description is the only thing that tells the operator what to fix.
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("error does not carry Telegram's reason: %v", err)
	}
}

// The bot token must never reach a log. net/http puts the request URL into its
// transport errors, and the token is IN that URL — so an unscrubbed wrap writes a
// working credential into the container log, which is rotated and shipped.
func TestTelegramNeverLeaksTheTokenInAnError(t *testing.T) {
	// A server that closes immediately, to force a transport-level error whose
	// message contains the URL.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	prev := telegramAPI
	telegramAPI = srv.URL
	srv.Close() // now nothing is listening
	t.Cleanup(func() { telegramAPI = prev })

	const token = "8123456:AAHsuperSecretBotToken"
	tg := NewTelegram(token, "42", nil)
	err := tg.Send(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected a transport error against a closed server")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("the bot token appears in an error that will be logged: %v", err)
	}
	if !strings.Contains(err.Error(), "<bot-token>") {
		t.Errorf("token was neither present nor redacted, so scrubbing may have silently stopped: %v", err)
	}
}

// An unconfigured sender must be inert and say so, rather than being started and
// failing on every tick.
func TestTelegramUnconfigured(t *testing.T) {
	for _, tc := range []struct{ token, chat string }{
		{"", "42"}, {"123:SECRET", ""}, {"", ""}, {"  ", " "},
	} {
		tg := NewTelegram(tc.token, tc.chat, nil)
		if tg.Configured() {
			t.Errorf("Configured() true for token=%q chat=%q", tc.token, tc.chat)
		}
		if err := tg.Send(context.Background(), "x"); err == nil {
			t.Error("an unconfigured sender reported a send as successful")
		}
	}
}
