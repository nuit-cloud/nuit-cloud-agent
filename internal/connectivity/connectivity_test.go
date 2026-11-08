package connectivity

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func local(t *testing.T) {
	t.Helper()
	old := allowLoopback
	allowLoopback = true
	t.Cleanup(func() { allowLoopback = old })
}

func shortTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := perTarget
	perTarget = d
	t.Cleanup(func() { perTarget = old })
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return n
}

func one(t *testing.T, tg Target) Result {
	t.Helper()
	rs := Run(context.Background(), []Target{tg}, "0.4.1")
	if len(rs) != 1 {
		t.Fatalf("%d results", len(rs))
	}
	return rs[0]
}

func tgt(id, kind, host string, extra map[string]any) map[string]any {
	m := map[string]any{"id": id, "kind": kind, "host": host}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestTCPListeningPortIsOK(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	r := one(t, Target{ID: "a", Kind: "tcp", Host: "127.0.0.1", Port: portOf(t, l.Addr().String())})
	if !r.OK || r.Detail != "connected" || r.ID != "a" {
		t.Errorf("%+v", r)
	}
}

func TestTCPClosedPortIsRefused(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := portOf(t, l.Addr().String())
	l.Close()
	r := one(t, Target{ID: "a", Kind: "tcp", Host: "127.0.0.1", Port: port})
	if r.OK || r.Detail != "refused" {
		t.Errorf("%+v", r)
	}
}

func TestTCPThatNeverAnswersTimesOut(t *testing.T) {
	shortTimeout(t, 150*time.Millisecond)
	old := dialContext
	dialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	t.Cleanup(func() { dialContext = old })
	r := one(t, Target{ID: "a", Kind: "tcp", Host: "192.0.2.1", Port: 80})
	if r.OK || r.Detail != "timeout" {
		t.Errorf("%+v", r)
	}
}

func TestHTTPSHandshakeThatNeverFinishesTimesOut(t *testing.T) {
	shortTimeout(t, 200*time.Millisecond)
	l, _ := net.Listen("tcp", "127.0.0.1:0") // kernel completes the connect; nobody ever accepts or speaks TLS
	defer l.Close()
	r := one(t, Target{ID: "a", Kind: "https", Host: "127.0.0.1", Port: portOf(t, l.Addr().String()), Path: "/"})
	if r.OK || r.Detail != "timeout" {
		t.Errorf("%+v", r)
	}
}

func TestHTTPSWithAnUntrustedCertificateFailsVerification(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer s.Close()
	r := one(t, Target{ID: "a", Kind: "https", Host: "127.0.0.1", Port: portOf(t, s.Listener.Addr().String()), Path: "/"})
	if r.OK || r.Detail != "tls-verify-failed" {
		t.Errorf("%+v", r)
	}
}

func trust(t *testing.T, s *httptest.Server) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	old := rootCAs
	rootCAs = pool
	t.Cleanup(func() { rootCAs = old })
}

func TestHTTPSWithATrustedCertificateReportsTheStatusAndSendsOnlyAUserAgent(t *testing.T) {
	var got http.Header
	var gotPath string
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got, gotPath = req.Header.Clone(), req.URL.Path
		w.WriteHeader(404)
		w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	defer s.Close()
	trust(t, s)
	r := one(t, Target{ID: "a", Kind: "https", Host: "127.0.0.1", SNI: "example.com", Port: portOf(t, s.Listener.Addr().String()), Path: "/health"})
	if !r.OK || r.Detail != "HTTP 404" {
		t.Errorf("a 404 means a server was reached: %+v", r)
	}
	if gotPath != "/health" {
		t.Errorf("path %q", gotPath)
	}
	if ua := got.Get("User-Agent"); ua != "nuit-cloud-agent/0.4.1 connectivity-check" {
		t.Errorf("user agent %q", ua)
	}
	for k := range got {
		if k != "User-Agent" && k != "Connection" {
			t.Errorf("unexpected header %s", k)
		}
	}
	if got.Get("Accept-Encoding") != "" {
		// Go only adds gzip when compression is on; we turned it off.
		t.Errorf("accept-encoding sent: %q", got.Get("Accept-Encoding"))
	}
}

func TestSNIIsWhatTheCertificateIsVerifiedAgainstWhenHostIsAnIP(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer s.Close()
	trust(t, s)
	port := portOf(t, s.Listener.Addr().String())
	if r := one(t, Target{ID: "a", Kind: "https", Host: "127.0.0.1", Port: port, SNI: "example.com", Path: "/"}); !r.OK || r.Detail != "HTTP 200" {
		t.Errorf("right sni: %+v", r)
	}
	if r := one(t, Target{ID: "a", Kind: "https", Host: "127.0.0.1", Port: port, SNI: "other.invalid", Path: "/"}); r.OK || r.Detail != "tls-verify-failed" {
		t.Errorf("wrong sni must fail verification: %+v", r)
	}
}

func TestHTTPSNeverFollowsARedirect(t *testing.T) {
	var hits atomic.Int32
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		http.Redirect(w, req, "/elsewhere", 302)
	}))
	defer s.Close()
	trust(t, s)
	r := one(t, Target{ID: "a", Kind: "https", Host: "127.0.0.1", Port: portOf(t, s.Listener.Addr().String()), Path: "/"})
	if !r.OK || r.Detail != "HTTP 302" || hits.Load() != 1 {
		t.Errorf("%+v hits=%d", r, hits.Load())
	}
}

func TestHTTPSServerErrorIsNotOK(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer s.Close()
	trust(t, s)
	r := one(t, Target{ID: "a", Kind: "https", Host: "127.0.0.1", Port: portOf(t, s.Listener.Addr().String()), Path: "/"})
	if r.OK || r.Detail != "HTTP 503" {
		t.Errorf("%+v", r)
	}
}

func TestHTTPSOnAClosedPortIsRefusedAndOnAPlainPortIsAHandshakeFailure(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := portOf(t, l.Addr().String())
	l.Close()
	if r := one(t, Target{ID: "a", Kind: "https", Host: "127.0.0.1", Port: port, Path: "/"}); r.Detail != "refused" {
		t.Errorf("%+v", r)
	}
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer plain.Close()
	if r := one(t, Target{ID: "a", Kind: "https", Host: "127.0.0.1", Port: portOf(t, plain.Listener.Addr().String()), Path: "/"}); r.OK || r.Detail != "tls-handshake-failed" {
		t.Errorf("%+v", r)
	}
}

func TestDNSReportsTheFirstIPv4AndClassifiesFailure(t *testing.T) {
	old := lookupIP
	t.Cleanup(func() { lookupIP = old })
	lookupIP = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("2001:db8::1"), net.ParseIP("192.0.2.7"), net.ParseIP("192.0.2.8")}, nil
	}
	if r := one(t, Target{ID: "a", Kind: "dns", Host: "example.test"}); !r.OK || r.Detail != "resolved 192.0.2.7" {
		t.Errorf("%+v", r)
	}
	lookupIP = func(context.Context, string) ([]net.IP, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}
	}
	if r := one(t, Target{ID: "a", Kind: "dns", Host: "example.test"}); r.OK || r.Detail != "dns-failed" {
		t.Errorf("%+v", r)
	}
	lookupIP = func(context.Context, string) ([]net.IP, error) { return nil, nil }
	if r := one(t, Target{ID: "a", Kind: "dns", Host: "example.test"}); r.OK || r.Detail != "dns-failed" {
		t.Errorf("%+v", r)
	}
}

func TestResultsAreInTargetOrderAndOneFailureStopsNothing(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadPort := portOf(t, dead.Addr().String())
	dead.Close()
	var ts []Target
	for i := 0; i < 20; i++ {
		p := portOf(t, l.Addr().String())
		if i%2 == 1 {
			p = deadPort
		}
		ts = append(ts, Target{ID: fmt.Sprintf("t%d", i), Kind: "tcp", Host: "127.0.0.1", Port: p})
	}
	rs := Run(context.Background(), ts, "0.4.1")
	for i, r := range rs {
		want := i%2 == 0
		if r.ID != fmt.Sprintf("t%d", i) || r.OK != want {
			t.Errorf("%d: %+v", i, r)
		}
	}
}

func TestAtMostEightRunAtOnce(t *testing.T) {
	var cur, peak atomic.Int32
	var mu sync.Mutex
	ts := make([]Target, 32)
	for i := range ts {
		ts[i] = Target{ID: fmt.Sprintf("t%d", i)}
	}
	rs := runAll(context.Background(), ts, func(context.Context, Target) Result {
		n := cur.Add(1)
		mu.Lock()
		if n > peak.Load() {
			peak.Store(n)
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		cur.Add(-1)
		return Result{OK: true}
	})
	if peak.Load() > 8 || peak.Load() < 2 {
		t.Errorf("peak concurrency %d", peak.Load())
	}
	if len(rs) != 32 || rs[31].ID != "t31" {
		t.Errorf("results %d", len(rs))
	}
}

func TestEachTargetHasItsOwnTimeoutAndTheWholeRunHasACap(t *testing.T) {
	shortTimeout(t, 100*time.Millisecond)
	old := dialContext
	dialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	t.Cleanup(func() { dialContext = old })
	ts := []Target{{ID: "a", Kind: "tcp", Host: "192.0.2.1", Port: 1}, {ID: "b", Kind: "tcp", Host: "192.0.2.2", Port: 1}}
	start := time.Now()
	rs := Run(context.Background(), ts, "0.4.1")
	if time.Since(start) > 2*time.Second || rs[0].Detail != "timeout" || rs[1].Detail != "timeout" {
		t.Errorf("%v %+v", time.Since(start), rs)
	}
	oldO := overall
	overall = 50 * time.Millisecond
	perTarget = 5 * time.Second
	t.Cleanup(func() { overall = oldO })
	start = time.Now()
	rs = Run(context.Background(), ts, "0.4.1")
	if time.Since(start) > 2*time.Second || rs[0].Detail != "timeout" {
		t.Errorf("overall cap not applied: %v %+v", time.Since(start), rs)
	}
}

func TestClassifyMapsErrorsToClassWordsOnly(t *testing.T) {
	cases := map[string]error{
		"refused":     &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
		"reset":       &net.OpError{Op: "read", Err: syscall.ECONNRESET},
		"unreachable": &net.OpError{Op: "dial", Err: syscall.ENETUNREACH},
		"timeout":     context.DeadlineExceeded,
		"dns-failed":  &net.DNSError{Err: "secret-looking text", IsNotFound: true},
		"other":       fmt.Errorf("something with 10.1.2.3 and a token"),
	}
	for want, err := range cases {
		if got := classify(err); got != want {
			t.Errorf("%s: got %s", want, got)
		}
	}
}

// --- validation ---

func good() map[string]any {
	return map[string]any{"targets": []any{
		tgt("a", "tcp", "192.168.1.10", map[string]any{"port": float64(22)}),
		tgt("b", "https", "example.com", map[string]any{"port": float64(8443), "path": "/x?y=1", "sni": "example.com"}),
		tgt("c", "dns", "example.com", nil),
	}}
}

func TestValidateAcceptsTheShapeThePlatformSigns(t *testing.T) {
	ts, err := Validate(good())
	if err != nil || len(ts) != 3 {
		t.Fatalf("%v %v", ts, err)
	}
	if ts[1].Path != "/x?y=1" || ts[1].Port != 8443 || ts[1].SNI != "example.com" || ts[2].Path != "/" {
		t.Errorf("%+v", ts)
	}
	ts, _ = Validate(map[string]any{"targets": []any{tgt("a", "https", "example.com", nil)}})
	if ts[0].Port != 443 || ts[0].Path != "/" {
		t.Errorf("defaults: %+v", ts[0])
	}
	if _, err := Validate(map[string]any{"targets": []any{tgt("a", "tcp", "10.0.0.5", map[string]any{"port": 22})}}); err != nil {
		t.Errorf("a private LAN address and an int port are allowed: %v", err)
	}
}

func TestValidateRefusesWhatIsNotInTheShape(t *testing.T) {
	many := func(n int) map[string]any {
		var l []any
		for i := 0; i < n; i++ {
			l = append(l, tgt(fmt.Sprintf("t%d", i), "dns", "example.com", nil))
		}
		return map[string]any{"targets": l}
	}
	if _, err := Validate(many(32)); err != nil {
		t.Errorf("32 is allowed: %v", err)
	}
	bad := map[string]map[string]any{
		"33 targets":         many(33),
		"no targets":         {"targets": []any{}},
		"missing targets":    {},
		"unknown top key":    {"targets": good()["targets"], "extra": 1},
		"unknown target key": {"targets": []any{tgt("a", "dns", "example.com", map[string]any{"x": 1})}},
		"duplicate ids":      {"targets": []any{tgt("a", "dns", "example.com", nil), tgt("a", "dns", "example.org", nil)}},
		"id uppercase":       {"targets": []any{tgt("A", "dns", "example.com", nil)}},
		"id too long":        {"targets": []any{tgt(strings.Repeat("a", 49), "dns", "example.com", nil)}},
		"id empty":           {"targets": []any{tgt("", "dns", "example.com", nil)}},
		"kind unknown":       {"targets": []any{tgt("a", "udp", "example.com", nil)}},
		"loopback":           {"targets": []any{tgt("a", "tcp", "127.0.0.1", map[string]any{"port": 22})}},
		"loopback other":     {"targets": []any{tgt("a", "tcp", "127.1.2.3", map[string]any{"port": 22})}},
		"link-local":         {"targets": []any{tgt("a", "tcp", "169.254.169.254", map[string]any{"port": 80})}},
		"multicast":          {"targets": []any{tgt("a", "tcp", "224.0.0.1", map[string]any{"port": 80})}},
		"unspecified":        {"targets": []any{tgt("a", "tcp", "0.0.0.0", map[string]any{"port": 80})}},
		"ipv6":               {"targets": []any{tgt("a", "tcp", "::1", map[string]any{"port": 80})}},
		"short ipv4":         {"targets": []any{tgt("a", "tcp", "127.1", map[string]any{"port": 80})}},
		"host with a path":   {"targets": []any{tgt("a", "dns", "example.com/x", nil)}},
		"host with a space":  {"targets": []any{tgt("a", "dns", "exa mple.com", nil)}},
		"host with a shell":  {"targets": []any{tgt("a", "dns", "a;rm -rf.com", nil)}},
		"host too long":      {"targets": []any{tgt("a", "dns", strings.Repeat("a.", 130)+"com", nil)}},
		"host empty":         {"targets": []any{tgt("a", "dns", "", nil)}},
		"tcp without port":   {"targets": []any{tgt("a", "tcp", "example.com", nil)}},
		"port zero":          {"targets": []any{tgt("a", "tcp", "example.com", map[string]any{"port": float64(0)})}},
		"port too big":       {"targets": []any{tgt("a", "tcp", "example.com", map[string]any{"port": float64(65536)})}},
		"port fraction":      {"targets": []any{tgt("a", "tcp", "example.com", map[string]any{"port": 22.5})}},
		"port string":        {"targets": []any{tgt("a", "tcp", "example.com", map[string]any{"port": "22"})}},
		"path no slash":      {"targets": []any{tgt("a", "https", "example.com", map[string]any{"path": "x"})}},
		"path with space":    {"targets": []any{tgt("a", "https", "example.com", map[string]any{"path": "/a b"})}},
		"path with control":  {"targets": []any{tgt("a", "https", "example.com", map[string]any{"path": "/a\r\nHost: x"})}},
		"path too long":      {"targets": []any{tgt("a", "https", "example.com", map[string]any{"path": "/" + strings.Repeat("a", 128)})}},
		"path on tcp":        {"targets": []any{tgt("a", "tcp", "example.com", map[string]any{"port": 1, "path": "/"})}},
		"sni on dns":         {"targets": []any{tgt("a", "dns", "example.com", map[string]any{"sni": "example.com"})}},
		"sni bad":            {"targets": []any{tgt("a", "https", "10.0.0.1", map[string]any{"sni": "a b"})}},
		"sni an address":     {"targets": []any{tgt("a", "https", "10.0.0.1", map[string]any{"sni": "10.0.0.2"})}},
		"target not object":  {"targets": []any{"example.com"}},
		"targets not a list": {"targets": "example.com"},
	}
	for name, p := range bad {
		if ts, err := Validate(p); err == nil {
			t.Errorf("%s was accepted: %+v", name, ts)
		}
	}
	if _, err := Validate(many(33)); err == nil || !strings.Contains(err.Error(), "1 to 32") {
		t.Errorf("error should say why: %v", err)
	}
}

func TestAnInvalidTargetRefusesTheWholeTaskBeforeAnythingIsDialed(t *testing.T) {
	old := dialContext
	dialed := false
	dialContext = func(context.Context, string, string) (net.Conn, error) { dialed = true; return nil, fmt.Errorf("no") }
	t.Cleanup(func() { dialContext = old })
	p := map[string]any{"targets": []any{tgt("a", "tcp", "192.168.1.1", map[string]any{"port": 22}), tgt("b", "tcp", "127.0.0.1", map[string]any{"port": 22})}}
	if _, err := Validate(p); err == nil {
		t.Fatal("accepted")
	}
	if dialed {
		t.Error("something was dialed")
	}
}
