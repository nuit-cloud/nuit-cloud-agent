// Package servicelog implements the read-only service-log task: lines of the systemd journal of one of the Control Plane
// Server's own services, for a time window. The unit comes from a fixed table, journalctl runs with exec (no shell), the
// optional filter is a literal substring applied here, and every line is scrubbed of credential-shaped text before it leaves.
package servicelog

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultLines = 1000
	MaxLines     = 5000
	MaxWindow    = 7 * 24 * time.Hour
	MaxContains  = 64
	MaxBytes     = 200 * 1024
	Timeout      = 20 * time.Second
	timeFormat   = "2006-01-02 15:04:05"
)

// Units is the fixed table: the request names a key, never a unit.
var Units = map[string]string{
	"scheduler": "nuit-cloud-cp-scheduler.service",
	"probe":     "nuit-cloud-cp-probe.service",
	"tls-front": "nginx.service",
}

// Query is a validated request.
type Query struct {
	Key, Unit string
	Since     time.Time
	Until     time.Time
	Lines     int
	Contains  string
}

// Result is the task's result.
type Result struct {
	Unit      string   `json:"unit"`
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated"`
	Earliest  *string  `json:"earliest"`
	Latest    *string  `json:"latest"`
	Note      string   `json:"note,omitempty"`
}

// Exec runs a command without a shell and returns its stdout and stderr.
type Exec func(ctx context.Context, argv []string) (stdout, stderr string, err error)

var containsRe = regexp.MustCompile(`^[A-Za-z0-9 _.:=/@-]{1,64}$`)

func timeParam(params map[string]any, key string) (time.Time, bool, error) {
	v, present := params[key]
	if !present {
		return time.Time{}, false, nil
	}
	s, ok := v.(string)
	if !ok {
		return time.Time{}, true, fmt.Errorf("service-log %s must be an RFC3339 time", key)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, true, fmt.Errorf("service-log %s must be an RFC3339 time with a zone", key)
	}
	return t.UTC(), true, nil
}

// Validate re-checks what the platform signed, before anything runs. now supplies the default for until.
func Validate(params map[string]any, now time.Time) (Query, error) {
	for k := range params {
		switch k {
		case "unit", "since", "until", "lines", "contains":
		default:
			return Query{}, errors.New("service-log takes only unit, since, until, lines and contains")
		}
	}
	q := Query{Lines: DefaultLines}
	key, _ := params["unit"].(string)
	unit, ok := Units[key]
	if !ok {
		return q, errors.New("service-log unit must be scheduler, probe or tls-front")
	}
	q.Key, q.Unit = key, unit
	since, present, err := timeParam(params, "since")
	if err != nil {
		return q, err
	}
	if !present {
		return q, errors.New("service-log needs since")
	}
	q.Since = since
	until, present, err := timeParam(params, "until")
	if err != nil {
		return q, err
	}
	if !present {
		until = now.UTC()
	}
	q.Until = until
	if !q.Since.Before(q.Until) {
		return q, errors.New("service-log since must be before until")
	}
	if q.Until.Sub(q.Since) > MaxWindow {
		return q, errors.New("service-log window is at most 7 days")
	}
	if v, present := params["lines"]; present {
		var n int
		switch x := v.(type) {
		case float64:
			if x != float64(int(x)) {
				return q, errors.New("service-log lines must be an integer")
			}
			n = int(x)
		case int:
			n = x
		case int64:
			n = int(x)
		default:
			return q, errors.New("service-log lines must be an integer")
		}
		if n < 1 || n > MaxLines {
			return q, fmt.Errorf("service-log lines must be 1 to %d", MaxLines)
		}
		q.Lines = n
	}
	if v, present := params["contains"]; present {
		s, ok := v.(string)
		if !ok || !containsRe.MatchString(s) {
			return q, fmt.Errorf("service-log contains must be 1 to %d characters of letters, digits, space and _ . : = / @ -", MaxContains)
		}
		q.Contains = s
	}
	return q, nil
}

// Argv is the exact journalctl command line for a query.
func Argv(q Query) []string {
	return []string{"journalctl", "--no-pager", "--utc", "-o", "short-iso", "-u", q.Unit,
		"--since", q.Since.UTC().Format(timeFormat), "--until", q.Until.UTC().Format(timeFormat)}
}

var (
	secretRuns = regexp.MustCompile(`(?i)((?:bearer |token=|token:|authorization:)\s*)[A-Za-z0-9_.~+/=-]{24,}`)
	prefixed   = regexp.MustCompile(`\b(?:nce_|nca_|smt_|svc_)[A-Za-z0-9_.~+/=-]*`)
	sshKey     = regexp.MustCompile(`ssh-ed25519 AAAA[A-Za-z0-9+/=]*`)
	pemLine    = regexp.MustCompile(`-----BEGIN.*`)
)

// Scrub removes credential-shaped text from one line. keep is false when the whole line must be dropped.
func Scrub(line string) (string, bool) {
	if strings.Contains(line, "PRIVATE KEY") {
		return "", false
	}
	line = pemLine.ReplaceAllString(line, "[removed]")
	line = secretRuns.ReplaceAllString(line, "${1}[removed]")
	line = prefixed.ReplaceAllString(line, "[removed]")
	line = sshKey.ReplaceAllString(line, "[removed]")
	return line, true
}

func classify(stdout, stderr string, err error) string {
	all := strings.ToLower(stdout + "\n" + stderr)
	switch {
	case strings.Contains(all, "no journal files"):
		return "no-journal"
	case strings.Contains(all, "could not be found"), strings.Contains(all, "not found"), strings.Contains(all, "no such unit"):
		return "unit-not-found"
	case strings.Contains(all, "permission denied"), strings.Contains(all, "insufficient permissions"),
		strings.Contains(all, "not seeing messages from other users"):
		return "unreadable"
	}
	if err != nil {
		return "unreadable"
	}
	return ""
}

func stamp(line string) (string, bool) {
	i := strings.IndexByte(line, ' ')
	if i < 11 || line[4] != '-' || line[7] != '-' || line[10] != 'T' {
		return "", false
	}
	return line[:i], true
}

// Run answers the query. run executes journalctl; tests inject a fake.
func Run(ctx context.Context, run Exec, q Query) Result {
	res := Result{Unit: q.Unit, Lines: []string{}}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	stdout, stderr, err := run(ctx, Argv(q))
	if note := classify(stdout, stderr, err); note != "" {
		res.Note = note
		return res
	}
	var kept []string
	for _, l := range strings.Split(stdout, "\n") {
		l = strings.TrimRight(l, "\r")
		if l == "" || strings.HasPrefix(l, "-- ") {
			continue
		}
		l, ok := Scrub(l)
		if !ok {
			continue
		}
		if q.Contains != "" && !strings.Contains(l, q.Contains) {
			continue
		}
		kept = append(kept, l)
	}
	if len(kept) > q.Lines {
		kept = kept[len(kept)-q.Lines:]
		res.Truncated = true
	}
	total := 0
	start := len(kept)
	for start > 0 && total+len(kept[start-1])+1 <= MaxBytes {
		start--
		total += len(kept[start]) + 1
	}
	if start > 0 {
		res.Truncated = true
		kept = kept[start:]
	}
	if len(kept) > 0 {
		res.Lines = kept
		if s, ok := stamp(kept[0]); ok {
			res.Earliest = &s
		}
		if s, ok := stamp(kept[len(kept)-1]); ok {
			res.Latest = &s
		}
	}
	return res
}
