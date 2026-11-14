package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetJSONUsesTheFallbackAddressLikeEveryOtherCall(t *testing.T) {
	allowLoopback(t)
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"a":1}`))
	}))
	defer srv.Close()
	c, _ := New("http://platform.invalid", "1", true)
	c.Credential = "nca_x"
	c.SetFallbacks([]string{strings.TrimPrefix(srv.URL, "http://")})
	var out struct{ A int }
	if err := c.GetJSON(context.Background(), "/x", 1024, &out); err != nil || out.A != 1 || auth != "Bearer nca_x" || !strings.HasPrefix(c.Route(), "by address ") {
		t.Fatalf("%v %+v %q %s", err, out, auth, c.Route())
	}
}

func TestGetJSONRefusesToFollowARedirectAndToReadMoreThanItWasToldTo(t *testing.T) {
	redirected := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			_, _ = w.Write([]byte(`{"pad":"` + strings.Repeat("x", 4096) + `"}`))
			return
		}
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "1", true)
	c.Credential = "nca_x"
	var out map[string]any
	var se *StatusError
	if err := c.GetJSON(context.Background(), "/r", 1024, &out); !errors.As(err, &se) || se.Status != 302 || redirected {
		t.Errorf("redirect: %v followed=%v", err, redirected)
	}
	if err := c.GetJSON(context.Background(), "/big", 1024, &out); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversize: %v", err)
	}
}

func TestPostJSONSendsTheBodyWithTheCredentialFollowsNoRedirectAndReturnsTheRefusalText(t *testing.T) {
	redirected := false
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer other.Close()
	var method, auth, ctype, version, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, auth, ctype, version = r.Method, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.Header.Get("X-Agent-Version")
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`{"a":2}`))
		case "/none":
			w.WriteHeader(http.StatusNoContent)
		case "/refused":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"the transfer is closed"}`))
		default:
			http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
		}
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "7.7.7", true)
	c.Credential = "nca_y"
	var out struct{ A int }
	if err := c.PostJSON(context.Background(), "/ok", map[string]any{"index": 3}, 1024, &out); err != nil || out.A != 2 {
		t.Fatalf("%v %+v", err, out)
	}
	if method != "POST" || auth != "Bearer nca_y" || ctype != "application/json" || version != "7.7.7" || body != `{"index":3}` {
		t.Errorf("%s %q %q %q %q", method, auth, ctype, version, body)
	}
	if err := c.PostJSON(context.Background(), "/none", map[string]any{}, 0, nil); err != nil {
		t.Errorf("no answer wanted: %v", err)
	}
	var se *StatusError
	if err := c.PostJSON(context.Background(), "/refused", map[string]any{}, 0, nil); !errors.As(err, &se) || se.Status != 409 || se.Message != "the transfer is closed" {
		t.Errorf("refusal: %v", err)
	}
	if err := c.PostJSON(context.Background(), "/r", map[string]any{}, 0, nil); !errors.As(err, &se) || se.Status != 307 || redirected {
		t.Errorf("redirect: %v followed=%v", err, redirected)
	}
}
