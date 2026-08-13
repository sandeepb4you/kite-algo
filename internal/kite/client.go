// Package kite is a thin client for the Zerodha Kite Connect REST + WebSocket
// APIs. It translates Kite's wire format into the platform's broker-agnostic
// types so the rest of the codebase never imports Kite-specific structures.
//
// Auth model: Kite uses OAuth-ish session tokens. The typical flow is:
//  1. User logs in at the Kite connect URL, receives a request_token.
//  2. We exchange it (plus a checksum) for an access_token via GenerateSession.
//  3. The access_token is used for every subsequent call (Authorization header).
//
// In practice we read a ready-made access_token from config/env (see config),
// which you can obtain once from the login flow and refresh daily.
package kite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"kite-algo/pkg/ratelimiter"
)

// APIVersion is the Kite Connect API version header value.
const APIVersion = "3"

// Client is a Kite Connect REST client.
type Client struct {
	apiKey       string
	apiSecret    string
	accessToken  string
	baseURL      string
	http         *http.Client
	limiter      *ratelimiter.Limiter
	logger       *slog.Logger
}

// New returns a Client. apiSecret may be empty if you only intend to use a
// pre-existing access_token (no session generation); it's required for
// GenerateSession.
func New(apiKey, apiSecret, accessToken, baseURL string, logger *slog.Logger) *Client {
	return &Client{
		apiKey:      apiKey,
		apiSecret:   apiSecret,
		accessToken: accessToken,
		baseURL:     strings.TrimRight(baseURL, "/"),
		http:        &http.Client{Timeout: 30 * time.Second},
		// Kite allows ~3 order requests/sec; 3 is a safe global default for the
		// non-order endpoints too (they share an overall throttle).
		limiter: ratelimiter.New(3),
		logger:  logger,
	}
}

// SetAccessToken sets or replaces the session access token at runtime.
func (c *Client) SetAccessToken(token string) {
	c.accessToken = token
}

// HasAccessToken reports whether a session token is configured.
func (c *Client) HasAccessToken() bool { return c.accessToken != "" }

// LoginURL returns the URL a user visits in a browser to authorize and obtain
// a request_token (returned as a redirect query param after login).
func (c *Client) LoginURL() string {
	return fmt.Sprintf("https://kite.trade/connect/login?api_key=%s&v=%s", c.apiKey, APIVersion)
}

// GenerateSession exchanges a request_token (from the login redirect) for an
// access_token. The checksum is sha256(api_key + request_token + api_secret).
// On success the access token is stored on the client and returned.
func (c *Client) GenerateSession(ctx context.Context, requestToken string) (string, error) {
	if c.apiKey == "" || c.apiSecret == "" {
		return "", errors.New("kite: api_key and api_secret required to generate session")
	}
	checksum := sha256Hex(c.apiKey + requestToken + c.apiSecret)

	form := url.Values{}
	form.Set("api_key", c.apiKey)
	form.Set("request_token", requestToken)
	form.Set("checksum", checksum)

	// Raw POST here because the response shape is session-specific.
	endpoint := c.baseURL + "/session/token"
	body := strings.NewReader(form.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Kite-Version", APIVersion)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("generate session: %w", err)
	}
	defer resp.Body.Close()

	var raw struct {
		Status string `json:"status"`
		Data   struct {
			AccessToken string `json:"access_token"`
			UserID      string `json:"user_id"`
		} `json:"data"`
		Message string `json:"message"`
		ErrorType string `json:"error_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return "", fmt.Errorf("decode session response: %w", err)
	}
	if raw.Status != "success" || raw.Data.AccessToken == "" {
		return "", fmt.Errorf("kite session failed: %s (%s)", raw.Message, raw.ErrorType)
	}

	c.accessToken = raw.Data.AccessToken
	if c.logger != nil {
		c.logger.Info("kite session established", "user", raw.Data.UserID)
	}
	return raw.Data.AccessToken, nil
}

// do performs an authenticated Kite API call, applying the rate limiter and
// unwrapping the standard {status, data, message} envelope. v receives the
// decoded "data" payload.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, form url.Values, v any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}

	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Kite-Version", APIVersion)
	if c.accessToken != "" {
		req.Header.Set("Authorization", "token "+c.apiKey+" "+c.accessToken)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kite %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	// Kite error envelope: {"status":"error","message":...,"error_type":...}
	var env struct {
		Status    string          `json:"status"`
		Data      json.RawMessage `json:"data"`
		Message   string          `json:"message"`
		ErrorType string          `json:"error_type"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode response (status %d): %w", resp.StatusCode, err)
	}
	if env.Status != "success" {
		return &APIError{
			StatusCode: resp.StatusCode,
			Message:    env.Message,
			ErrorType:  env.ErrorType,
		}
	}
	if v != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, v); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
}

// get is a convenience for GET requests.
func (c *Client) get(ctx context.Context, path string, query url.Values, v any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, v)
}

// postForm is a convenience for POST requests with form data.
func (c *Client) postForm(ctx context.Context, path string, form url.Values, v any) error {
	return c.do(ctx, http.MethodPost, path, nil, form, v)
}

// putForm is a convenience for PUT requests with form data.
func (c *Client) putForm(ctx context.Context, path string, form url.Values, v any) error {
	return c.do(ctx, http.MethodPut, path, nil, form, v)
}

// delete is a convenience for DELETE requests.
func (c *Client) delete(ctx context.Context, path string, query url.Values, v any) error {
	return c.do(ctx, http.MethodDelete, path, query, nil, v)
}

// rawGetBytes fetches a path and returns the raw body bytes (used for the
// instruments CSV, which is not JSON). Still subject to rate limiting.
func (c *Client) rawGetBytes(ctx context.Context, path string) ([]byte, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	endpoint := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Kite-Version", APIVersion)
	if c.accessToken != "" {
		req.Header.Set("Authorization", "token "+c.apiKey+" "+c.accessToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kite GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kite GET %s: status %d", path, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// postJSON is a convenience for POST requests with a JSON body (rare; mostly form).
func (c *Client) postJSON(ctx context.Context, path string, payload []byte, v any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	endpoint := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("X-Kite-Version", APIVersion)
	req.Header.Set("Content-Type", "application/json")
	if c.accessToken != "" {
		req.Header.Set("Authorization", "token "+c.apiKey+" "+c.accessToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kite POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var env struct {
		Status    string          `json:"status"`
		Data      json.RawMessage `json:"data"`
		Message   string          `json:"message"`
		ErrorType string          `json:"error_type"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode response (status %d): %w", resp.StatusCode, err)
	}
	if env.Status != "success" {
		return &APIError{StatusCode: resp.StatusCode, Message: env.Message, ErrorType: env.ErrorType}
	}
	if v != nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, v)
	}
	return nil
}

// APIError represents a non-success response from Kite.
type APIError struct {
	StatusCode int
	Message    string
	ErrorType  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kite api error: %s (%s, http %d)", e.Message, e.ErrorType, e.StatusCode)
}

// sha256Hex returns the hex-encoded SHA-256 of s.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
