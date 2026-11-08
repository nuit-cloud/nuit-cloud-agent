// Package connectivity implements the read-only connectivity-check task: the platform names endpoints, the agent says which
// of them this machine can reach and, when it cannot, why (one class word, never the raw error). Nothing is sent but a TCP
// connect, a TLS handshake and one GET with a User-Agent header; redirects are never followed and no credential is sent.
package connectivity

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	MaxTargets  = 32
	maxParallel = 8
	maxPath     = 128
	maxBody     = 1024
)

// Test hooks (unexported on purpose; the real agent never changes them).
var (
	perTarget     = 5 * time.Second
	overall       = 60 * time.Second
	rootCAs       *x509.CertPool // nil means the system roots
	allowLoopback = false
	dialContext   = (&net.Dialer{}).DialContext
	lookupIP      = func(ctx context.Context, host string) ([]net.IP, error) {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		ips := make([]net.IP, 0, len(addrs))
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
		return ips, err
	}
)

// Target is one validated endpoint.
type Target struct {
	ID, Kind, Host, Path, SNI string
	Port                      int
}

// Result is one endpoint's answer. The JSON shape is the task's result.
type Result struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	MS     int64  `json:"ms"`
	Detail string `json:"detail"`
}

var (
	idRe    = regexp.MustCompile(`^[a-z0-9._-]{1,48}$`)
	nameRe  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*\.?$`)
	numeric = regexp.MustCompile(`^[0-9.]+$`)
	pathRe  = regexp.MustCompile(`^/[\x21-\x7e]*$`)
)

// Validate re-checks what the platform signed, before anything is dialed. Any invalid target refuses the whole task.
func Validate(params map[string]any) ([]Target, error) {
	for k := range params {
		if k != "targets" {
			return nil, errors.New("connectivity-check takes only targets")
		}
	}
	list, ok := params["targets"].([]any)
	if !ok {
		return nil, errors.New("connectivity-check needs targets (a list)")
	}
	if len(list) == 0 || len(list) > MaxTargets {
		return nil, fmt.Errorf("connectivity-check takes 1 to %d targets", MaxTargets)
	}
	seen := map[string]bool{}
	out := make([]Target, 0, len(list))
	for i, raw := range list {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("target %d is not an object", i)
		}
		t, err := validateTarget(m)
		if err != nil {
			return nil, fmt.Errorf("target %d: %w", i, err)
		}
		if seen[t.ID] {
			return nil, fmt.Errorf("target %d: id is not unique", i)
		}
		seen[t.ID] = true
		out = append(out, t)
	}
	return out, nil
}

func validateTarget(m map[string]any) (Target, error) {
	var t Target
	for k := range m {
		switch k {
		case "id", "kind", "host", "port", "path", "sni":
		default:
			return t, errors.New("unknown key")
		}
	}
	var ok bool
	if t.ID, ok = m["id"].(string); !ok || !idRe.MatchString(t.ID) {
		return t, errors.New("id must be 1 to 48 of a-z 0-9 . _ -")
	}
	if t.Kind, ok = m["kind"].(string); !ok || (t.Kind != "tcp" && t.Kind != "https" && t.Kind != "dns") {
		return t, errors.New("kind must be tcp, https or dns")
	}
	if t.Host, ok = m["host"].(string); !ok || !validHost(t.Host) {
		return t, errors.New("host must be a DNS name or a routable IPv4 address (not loopback, link-local, multicast or 0.0.0.0)")
	}
	if v, has := m["port"]; has {
		f, isNum := v.(float64)
		if i, isInt := v.(int); isInt {
			f, isNum = float64(i), true
		}
		if !isNum || f != float64(int(f)) || f < 1 || f > 65535 {
			return t, errors.New("port must be an integer 1 to 65535")
		}
		t.Port = int(f)
	}
	if t.Kind == "tcp" && t.Port == 0 {
		return t, errors.New("a tcp target needs a port")
	}
	if t.Kind == "https" && t.Port == 0 {
		t.Port = 443
	}
	if v, has := m["path"]; has {
		s, isStr := v.(string)
		if t.Kind != "https" || !isStr || len(s) > maxPath || !pathRe.MatchString(s) {
			return t, errors.New("path is only for https: it must start with / and have at most 128 printable characters, no spaces")
		}
		t.Path = s
	}
	if t.Path == "" {
		t.Path = "/"
	}
	if v, has := m["sni"]; has {
		s, isStr := v.(string)
		if t.Kind != "https" || !isStr || len(s) > 253 || !nameRe.MatchString(s) || numeric.MatchString(s) {
			return t, errors.New("sni is only for https and must be a DNS name")
		}
		t.SNI = s
	}
	return t, nil
}

func validHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	if numeric.MatchString(h) {
		ip := net.ParseIP(h)
		if ip == nil || ip.To4() == nil || strings.Count(h, ".") != 3 {
			return false
		}
		if allowLoopback {
			return true
		}
		return !(ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified())
	}
	return nameRe.MatchString(h)
}

// Run checks every target (at most 8 at once, 5 s each, 60 s overall); one failing never stops another. Results are in the
// order of the targets.
func Run(ctx context.Context, targets []Target, version string) []Result {
	ctx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()
	return runAll(ctx, targets, func(ctx context.Context, t Target) Result { return check(ctx, t, version) })
}

func runAll(ctx context.Context, targets []Target, do func(context.Context, Target) Result) []Result {
	results := make([]Result, len(targets))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if p := recover(); p != nil {
					results[i] = Result{ID: t.ID, Detail: "other"}
				}
			}()
			cctx, cancel := context.WithTimeout(ctx, perTarget)
			defer cancel()
			start := time.Now()
			r := do(cctx, t)
			r.ID = t.ID
			r.MS = time.Since(start).Milliseconds()
			results[i] = r
		}()
	}
	wg.Wait()
	return results
}

func check(ctx context.Context, t Target, version string) Result {
	switch t.Kind {
	case "tcp":
		c, err := dialContext(ctx, "tcp", net.JoinHostPort(t.Host, fmt.Sprint(t.Port)))
		if err != nil {
			return Result{Detail: classify(err)}
		}
		c.Close()
		return Result{OK: true, Detail: "connected"}
	case "dns":
		ips, err := lookupIP(ctx, t.Host)
		if err != nil || len(ips) == 0 {
			if err != nil && isTimeout(err) {
				return Result{Detail: "timeout"}
			}
			return Result{Detail: "dns-failed"}
		}
		first := ips[0]
		for _, ip := range ips {
			if ip.To4() != nil {
				first = ip
				break
			}
		}
		return Result{OK: true, Detail: "resolved " + first.String()}
	default:
		return checkHTTPS(ctx, t, version)
	}
}

// classError carries a class decided where the failing step is known.
type classError struct{ class string }

func (e *classError) Error() string { return e.class }

func checkHTTPS(ctx context.Context, t Target, version string) Result {
	serverName := t.SNI
	if serverName == "" {
		serverName = t.Host
	}
	addr := net.JoinHostPort(t.Host, fmt.Sprint(t.Port))
	tr := &http.Transport{
		Proxy:              nil,
		DisableKeepAlives:  true,
		DisableCompression: true,
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			raw, err := dialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, &classError{classify(err)}
			}
			conn := tls.Client(raw, &tls.Config{ServerName: serverName, RootCAs: rootCAs, MinVersion: tls.VersionTLS12})
			if err := conn.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, &classError{classifyTLS(err)}
			}
			return conn, nil
		},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+t.Path, nil)
	if err != nil {
		return Result{Detail: "other"}
	}
	req.Header.Set("User-Agent", "nuit-cloud-agent/"+version+" connectivity-check")
	resp, err := client.Do(req)
	if err != nil {
		var ce *classError
		if errors.As(err, &ce) {
			return Result{Detail: ce.class}
		}
		return Result{Detail: classify(err)}
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, maxBody)
	return Result{OK: resp.StatusCode < 500, Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

func classifyTLS(err error) string {
	var (
		unknown x509.UnknownAuthorityError
		invalid x509.CertificateInvalidError
		host    x509.HostnameError
		verify  *tls.CertificateVerificationError
	)
	switch {
	case errors.As(err, &verify), errors.As(err, &unknown), errors.As(err, &invalid), errors.As(err, &host):
		return "tls-verify-failed"
	case isTimeout(err):
		return "timeout"
	}
	if c := classify(err); c != "other" && c != "reset" {
		return c
	}
	return "tls-handshake-failed"
}

func classify(err error) string {
	var dns *net.DNSError
	switch {
	case err == nil:
		return "other"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "reset"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETDOWN), errors.Is(err, syscall.EHOSTDOWN):
		return "unreachable"
	case errors.As(err, &dns):
		if dns.IsTimeout {
			return "timeout"
		}
		return "dns-failed"
	case isTimeout(err):
		return "timeout"
	}
	return "other"
}
