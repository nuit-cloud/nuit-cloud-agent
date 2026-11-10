// Package client is the agent's side of the platform's HTTP API. The agent only ever calls out; nothing here listens.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
)

// Client talks to one platform.
//
// It reaches it by the platform's name, and, if that does not work, by fixed addresses the platform has told it about (see
// SetFallbacks). A fallback is only a different place to connect to: the request is still for the platform's name, so the
// certificate is checked against that name and an address that is not really the platform cannot be talked to. This is what keeps
// an agent in touch when the platform's name cannot be resolved, or what the name points at is down.
type Client struct {
	BaseURL    string
	HTTP       *http.Client
	Version    string
	Credential string

	// Observer, if set, reads the small facts the platform asks for with the check-in (see internal/observe). Nothing is read or sent unless the
	// platform's last answer listed the kind, this agent knows it, and it is due.
	Observer Observer
	// Now is the clock, replaceable in tests.
	Now func() time.Time
	// offset is the platform's time minus this machine's, as last measured from a check-in answer; offsetKnown says there has been one.
	offset      time.Duration
	offsetKnown bool

	mu        sync.Mutex
	fallbacks []string
	// wanted is what the platform last asked to be sent with the check-in; sent is what the agent last sent of each kind, and when it last read it.
	wanted []Want
	sent   map[string]sentRecord
	// preferred is the route that worked last: 0 is the name, n is fallbacks[n-1].
	preferred int
}

type routeKey struct{}

// MaxFallbacks is how many fixed addresses an agent keeps.
const MaxFallbacks = 4

// validFallback is ValidFallback; a test replaces it to use a loopback server.
var validFallback = ValidFallback

// ValidFallback reports whether s is `host:port` with a private IPv4 host. A platform that tells an agent to connect to a public
// address is refused: fallbacks exist for the platform's own network.
func ValidFallback(s string) bool {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil || !ip.IsPrivate() {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

// SetFallbacks replaces the fixed addresses to try when the name does not work, keeping only valid ones (at most MaxFallbacks),
// and returns what it kept.
func (c *Client) SetFallbacks(list []string) []string {
	var kept []string
	seen := map[string]bool{}
	for _, a := range list {
		if len(kept) < MaxFallbacks && validFallback(a) && !seen[a] {
			kept = append(kept, a)
			seen[a] = true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fallbacks = kept
	if c.preferred > len(kept) {
		c.preferred = 0
	}
	return kept
}

// Route says how the platform was last reached: its name, or a fixed address.
func (c *Client) Route() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.preferred == 0 || c.preferred > len(c.fallbacks) {
		return "by name"
	}
	return "by address " + c.fallbacks[c.preferred-1]
}

// order lists the routes to try: the one that worked last, then the name, then the rest.
func (c *Client) order() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	routes := []int{c.preferred}
	for r := 0; r <= len(c.fallbacks); r++ {
		if r != c.preferred {
			routes = append(routes, r)
		}
	}
	return routes
}

// dial is the transport's way of connecting: to the address the request names, unless the request was given a fallback route.
func (c *Client) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second}
	if r, ok := ctx.Value(routeKey{}).(int); ok && r > 0 {
		c.mu.Lock()
		var target string
		if r <= len(c.fallbacks) {
			target = c.fallbacks[r-1]
		}
		c.mu.Unlock()
		if target != "" {
			return d.DialContext(ctx, network, target)
		}
	}
	return d.DialContext(ctx, network, addr)
}

// New makes a client. It refuses a plain-http address unless allowInsecureHTTP is set: the enrollment code and the agent's
// credential must not cross a network in the clear.
func New(baseURL, version string, allowInsecureHTTP bool) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%q is not a platform address", baseURL)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && allowInsecureHTTP) {
		return nil, errors.New("the platform address must be https:// (pass --insecure-http only for a trusted test network)")
	}
	c := &Client{BaseURL: u.String(), Version: version}
	// Each request makes its own connection, so which route it used is never a pooled connection's doing.
	c.HTTP = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: c.dial, DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second, Proxy: http.ProxyFromEnvironment}}
	return c, nil
}

// StatusError is a refusal or failure the platform answered with.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("the platform answered %d: %s", e.Status, e.Message)
}

// Unauthorized reports a 401: the code, or the agent's credential, is not (or is no longer) accepted.
func (e *StatusError) Unauthorized() bool { return e.Status == http.StatusUnauthorized }

// EnrollResponse is the platform's answer to a first call.
type EnrollResponse struct {
	MachineID           string `json:"machineId"`
	Name                string `json:"name"`
	Credential          string `json:"credential"`
	PollIntervalSeconds int    `json:"pollIntervalSeconds"`
	TaskSigning         struct {
		Keys []taskverify.Key `json:"keys"`
	} `json:"taskSigning"`
	// FallbackAddresses are fixed places to connect to when the platform's name does not work (omitted: none configured).
	FallbackAddresses []string `json:"fallbackAddresses,omitempty"`
}

// CheckInResponse is the answer to a routine call: when to call again, and the signed tasks waiting.
type CheckInResponse struct {
	MachineID           string                `json:"machineId"`
	Name                string                `json:"name"`
	PollIntervalSeconds int                   `json:"pollIntervalSeconds"`
	Tasks               []taskverify.Envelope `json:"tasks"`
	// FallbackAddresses: see EnrollResponse. Absent means unchanged; an empty list clears them.
	FallbackAddresses *[]string `json:"fallbackAddresses,omitempty"`
	// Observe is what the platform wants sent with the check-in and how often. Absent means unchanged; an empty list asks for none.
	Observe *[]Want `json:"observe,omitempty"`
	// ServerTime is the platform's time as it answered (RFC 3339): what a task's expiry is judged against, so a machine whose own clock is wrong still runs
	// the tasks it is sent.
	ServerTime string `json:"serverTime,omitempty"`
}

// Report is what the agent says about a task.
type Report struct {
	Status   string `json:"status"` // running, done or failed
	ExitCode *int   `json:"exitCode,omitempty"`
	Output   string `json:"output,omitempty"`
	Result   any    `json:"result,omitempty"`
}

// Enroll trades an enrollment code for an identity of its own.
func (c *Client) Enroll(ctx context.Context, code, hostname string) (*EnrollResponse, error) {
	var out EnrollResponse
	err := c.do(ctx, "/api/agent/enroll", map[string]string{"code": code, "hostname": hostname, "agentVersion": c.Version}, false, &out)
	return &out, err
}

// ClockOffset is the platform's time minus this machine's, as measured at the last check-in, and whether there has been one. The agent judges a task's
// expiry by this machine's time plus it: the clock of a freshly installed machine can be hours out, and then every task would look expired, including
// the one that would put the clock right. The time comes over the same authenticated connection the tasks do.
func (c *Client) ClockOffset() (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset, c.offsetKnown
}

// Observer reads one kind of observation.
type Observer interface {
	Known(kind string) bool
	// Collect reads a kind: the data to send and its version (a digest of the fact, equal when the fact has not changed).
	Collect(ctx context.Context, kind string) (data any, version string, err error)
}

// Want is a kind of observation the platform asks for, and how often.
type Want struct {
	Kind            string `json:"kind"`
	IntervalSeconds int    `json:"intervalSeconds"`
	// Have is the version the platform already holds for this machine. A kind is sent only when this agent's version differs from it.
	Have string `json:"have,omitempty"`
}

// Observation is one fact sent with a check-in.
type Observation struct {
	Kind       string `json:"kind"`
	ObservedAt string `json:"observedAt"`
	Version    string `json:"version"`
	Data       any    `json:"data"`
}

// sentRecord is what the agent last sent of a kind, and when it last read it.
type sentRecord struct {
	version string
	sentAt  time.Time
	readAt  time.Time
}

type checkInBody struct {
	Observations []Observation `json:"observations,omitempty"`
}

const (
	minObserveInterval = time.Minute
	maxObserveInterval = 24 * time.Hour
	maxWanted          = 8
)

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// due reads what the platform asked for and sends only what has changed. A kind is read at most once per interval, and sent only when this
// agent's version of it differs from the one the platform says it holds (`Have`), so an unchanged fact costs no traffic at all. The agent remembers
// when it last read and sent each kind, so a kind the platform keeps not holding (it refused it, or lost it) is tried again once per interval,
// not at every check-in. A read that fails is skipped and tried at the next one.
func (c *Client) due(ctx context.Context) ([]Observation, []sentUpdate) {
	if c.Observer == nil {
		return nil, nil
	}
	c.mu.Lock()
	wanted := append([]Want(nil), c.wanted...)
	c.mu.Unlock()
	var list []Observation
	var updates []sentUpdate
	now := c.now()
	for _, w := range wanted {
		if !c.Observer.Known(w.Kind) {
			continue
		}
		c.mu.Lock()
		rec, had := c.sent[w.Kind]
		c.mu.Unlock()
		interval := time.Duration(w.IntervalSeconds) * time.Second
		if had && now.Sub(rec.readAt) < interval {
			continue
		}
		data, version, err := c.Observer.Collect(ctx, w.Kind)
		if err != nil || data == nil || version == "" {
			continue
		}
		rec.readAt = now
		if version == w.Have {
			// the platform already holds exactly this: nothing to send
			updates = append(updates, sentUpdate{kind: w.Kind, rec: sentRecord{version: version, sentAt: rec.sentAt, readAt: now}})
			continue
		}
		list = append(list, Observation{Kind: w.Kind, ObservedAt: now.UTC().Format(time.RFC3339), Version: version, Data: data})
		updates = append(updates, sentUpdate{kind: w.Kind, rec: sentRecord{version: version, sentAt: now, readAt: now}})
	}
	return list, updates
}

type sentUpdate struct {
	kind string
	rec  sentRecord
}

// CheckIn is the routine call. It carries the observations the platform asked for that are due.
func (c *Client) CheckIn(ctx context.Context) (*CheckInResponse, error) {
	list, updates := c.due(ctx)
	var out CheckInResponse
	sent := c.now()
	err := c.do(ctx, "/api/agent/checkin", checkInBody{Observations: list}, true, &out)
	if err == nil {
		c.mu.Lock()
		// The platform's time, taken to be that of the middle of the exchange.
		if t, perr := time.Parse(time.RFC3339Nano, out.ServerTime); perr == nil {
			received := c.now()
			if received.Before(sent) {
				received = sent
			}
			c.offset = t.Sub(sent.Add(received.Sub(sent) / 2))
			c.offsetKnown = true
		}
		if c.sent == nil {
			c.sent = map[string]sentRecord{}
		}
		for _, u := range updates {
			c.sent[u.kind] = u.rec
		}
		if out.Observe != nil {
			c.wanted = c.wanted[:0]
			for _, w := range *out.Observe {
				if len(c.wanted) >= maxWanted {
					break
				}
				d := time.Duration(w.IntervalSeconds) * time.Second
				if d < minObserveInterval {
					d = minObserveInterval
				}
				if d > maxObserveInterval {
					d = maxObserveInterval
				}
				c.wanted = append(c.wanted, Want{Kind: w.Kind, IntervalSeconds: int(d / time.Second), Have: w.Have})
			}
		}
		c.mu.Unlock()
	}
	return &out, err
}

// ReportTask tells the platform how a task is going. A 409 (the platform already has a finished result) is not an error: the
// agent has nothing more to do about that task.
func (c *Client) ReportTask(ctx context.Context, taskID string, report Report) error {
	err := c.do(ctx, "/api/agent/tasks/"+url.PathEscape(taskID)+"/result", report, true, nil)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusConflict {
		return nil
	}
	return err
}

// RotateCredential gets a new credential; the old one stops working at once, so the caller must save the new one first thing.
func (c *Client) RotateCredential(ctx context.Context) (string, error) {
	var out struct {
		Credential string `json:"credential"`
	}
	err := c.do(ctx, "/api/agent/rotate-credential", struct{}{}, true, &out)
	return out.Credential, err
}

// do makes a call. If the platform cannot be reached (not: if it answers with a refusal), it tries the other routes, and remembers the one
// that worked.
func (c *Client) do(ctx context.Context, path string, body any, authenticated bool, out any) error {
	var lastErr error
	for _, route := range c.order() {
		attempt, cancel := context.WithTimeout(context.WithValue(ctx, routeKey{}, route), 20*time.Second)
		err := c.once(attempt, path, body, authenticated, out)
		cancel()
		var se *StatusError
		if err == nil || errors.As(err, &se) {
			// the platform answered (even if with a refusal): this route works
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
	c.preferred = 0 // next time start with the name again
	c.mu.Unlock()
	return lastErr
}

// Do sends a request the way the platform calls are sent: by name first, then, if the connection itself fails, by each fixed
// fallback address, remembering the route that worked. Meant for the big downloads (a new agent build, the runtime), which
// must not depend on DNS either. The caller has no per-attempt timeout here: a connection that cannot be made fails fast.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	var lastErr error
	for _, route := range c.order() {
		res, err := c.HTTP.Do(req.Clone(context.WithValue(req.Context(), routeKey{}, route)))
		if err == nil {
			c.mu.Lock()
			c.preferred = route
			c.mu.Unlock()
			return res, nil
		}
		lastErr = err
		if req.Context().Err() != nil {
			break
		}
	}
	c.mu.Lock()
	c.preferred = 0
	c.mu.Unlock()
	return nil, lastErr
}

func (c *Client) once(ctx context.Context, path string, body any, authenticated bool, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Version", c.Version)
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+c.Credential)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return &StatusError{Status: res.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
