package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrTooLarge is returned when the platform's answer is bigger than the limit the caller set.
var ErrTooLarge = errors.New("the platform's answer is larger than this agent will read")

// ErrUnreadable is returned when the platform answered but the answer is not the JSON the caller expected. It says nothing of what
// the answer held: the caller may be reading a secret.
var ErrUnreadable = errors.New("the platform's answer could not be read")

// GetJSON asks the platform for path with the agent's credential (GET), reads at most maxBytes of the answer and decodes it into out.
// It uses the same routes as every other call (the platform's name, then its fixed fallback addresses, TLS checked against the name),
// never follows a redirect (a redirect could carry the credential to another place), and never puts the answer in an error.
func (c *Client) GetJSON(ctx context.Context, path string, maxBytes int64, out any) error {
	return c.jsonCall(ctx, http.MethodGet, path, nil, maxBytes, out, 20*time.Second)
}

// postAttempt is how long one attempt of a PostJSON may take: it carries a volume's chunk (a few hundred KiB) over what may be a slow link.
const postAttempt = 2 * time.Minute

// PostJSON sends body to path as JSON with the agent's credential (POST), reads at most maxBytes of the answer and decodes it into out (nil:
// the answer is not read beyond its status). Like GetJSON it uses every route, never follows a redirect and never puts the answer in an
// error, only the platform's own `error` text for a refusal. A request whose connection failed is tried again by the next route, so the
// platform must treat a repeat of the same request as the same request.
func (c *Client) PostJSON(ctx context.Context, path string, body any, maxBytes int64, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.jsonCall(ctx, http.MethodPost, path, raw, maxBytes, out, postAttempt)
}

func (c *Client) jsonCall(ctx context.Context, method, path string, body []byte, maxBytes int64, out any, perAttempt time.Duration) error {
	hc := *c.HTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if hc.Timeout != 0 && hc.Timeout < perAttempt {
		hc.Timeout = perAttempt
	}
	var lastErr error
	for _, route := range c.order() {
		attempt, cancel := context.WithTimeout(context.WithValue(ctx, routeKey{}, route), perAttempt)
		err, answered := c.jsonOnce(attempt, &hc, method, path, body, maxBytes, out)
		cancel()
		if answered {
			c.mu.Lock()
			c.preferred = route
			c.mu.Unlock()
			return err
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	c.mu.Lock()
	c.preferred = 0
	c.mu.Unlock()
	return lastErr
}

// jsonOnce makes one attempt; answered says the platform replied (so another route would not help).
func (c *Client) jsonOnce(ctx context.Context, hc *http.Client, method, path string, body []byte, maxBytes int64, out any) (error, bool) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err, true
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Agent-Version", c.Version)
	req.Header.Set("Authorization", "Bearer "+c.Credential)
	res, err := hc.Do(req)
	if err != nil {
		return err, false
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		if res.StatusCode < 400 {
			return &StatusError{Status: res.StatusCode, Message: "the platform redirected the request; this agent does not follow redirects"}, true
		}
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return &StatusError{Status: res.StatusCode, Message: e.Error}, true
	}
	if out == nil {
		return nil, true
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return err, false
	}
	if int64(len(data)) > maxBytes {
		return ErrTooLarge, true
	}
	if err := json.Unmarshal(data, out); err != nil {
		return ErrUnreadable, true
	}
	return nil, true
}
