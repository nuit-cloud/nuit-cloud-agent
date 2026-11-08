package servicelog

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func good() map[string]any {
	return map[string]any{"unit": "scheduler", "since": "2026-11-08T10:00:00Z"}
}

func TestValidateRefusesWhatItShould(t *testing.T) {
	cases := map[string]func(p map[string]any){
		"extra key":         func(p map[string]any) { p["cmd"] = "x" },
		"unknown unit":      func(p map[string]any) { p["unit"] = "sshd" },
		"unit as real name": func(p map[string]any) { p["unit"] = "nginx.service" },
		"no unit":           func(p map[string]any) { delete(p, "unit") },
		"no since":          func(p map[string]any) { delete(p, "since") },
		"since no zone":     func(p map[string]any) { p["since"] = "2026-11-08T10:00:00" },
		"since not string":  func(p map[string]any) { p["since"] = float64(5) },
		"until bad":         func(p map[string]any) { p["until"] = "yesterday" },
		"since after until": func(p map[string]any) { p["until"] = "2026-11-08T09:00:00Z" },
		"since equals until": func(p map[string]any) {
			p["until"] = "2026-11-08T10:00:00Z"
		},
		"since after now default": func(p map[string]any) { p["since"] = "2026-11-08T13:00:00Z" },
		"window over 7 days": func(p map[string]any) {
			p["since"] = "2026-10-01T00:00:00Z"
		},
		"lines zero":      func(p map[string]any) { p["lines"] = float64(0) },
		"lines too many":  func(p map[string]any) { p["lines"] = float64(5001) },
		"lines fraction":  func(p map[string]any) { p["lines"] = 1.5 },
		"lines string":    func(p map[string]any) { p["lines"] = "10" },
		"contains empty":  func(p map[string]any) { p["contains"] = "" },
		"contains long":   func(p map[string]any) { p["contains"] = strings.Repeat("a", 65) },
		"contains shell":  func(p map[string]any) { p["contains"] = "a;rm" },
		"contains quote":  func(p map[string]any) { p["contains"] = "a'b" },
		"contains dollar": func(p map[string]any) { p["contains"] = "$(x)" },
		"contains newl":   func(p map[string]any) { p["contains"] = "a\nb" },
		"contains number": func(p map[string]any) { p["contains"] = float64(1) },
	}
	for name, mut := range cases {
		p := good()
		mut(p)
		if _, err := Validate(p, now); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestValidateDefaultsAndAcceptsTheBoundaries(t *testing.T) {
	q, err := Validate(good(), now)
	if err != nil {
		t.Fatal(err)
	}
	if q.Lines != 1000 || !q.Until.Equal(now) || q.Unit != "nuit-cloud-cp-scheduler.service" || q.Contains != "" {
		t.Errorf("%+v", q)
	}
	p := map[string]any{"unit": "tls-front", "since": "2026-11-01T12:00:00Z", "until": "2026-11-08T12:00:00Z", "lines": float64(5000), "contains": strings.Repeat("a", 64)}
	if _, err := Validate(p, now); err != nil {
		t.Errorf("boundary refused: %v", err)
	}
	p = map[string]any{"unit": "probe", "since": "2026-11-08T12:00:00+02:00", "lines": float64(1), "contains": "A-z_.:=/@ 9"}
	q, err = Validate(p, now)
	if err != nil || !q.Since.Equal(time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("%+v %v", q, err)
	}
}

func TestTheUnitTableIsFixed(t *testing.T) {
	want := map[string]string{"scheduler": "nuit-cloud-cp-scheduler.service", "probe": "nuit-cloud-cp-probe.service", "tls-front": "nginx.service"}
	if !reflect.DeepEqual(Units, want) {
		t.Errorf("%v", Units)
	}
}

func TestArgvIsExactAndInUTC(t *testing.T) {
	q, _ := Validate(map[string]any{"unit": "probe", "since": "2026-11-08T12:00:00+02:00", "until": "2026-11-08T13:30:05+02:00"}, now)
	want := []string{"journalctl", "--no-pager", "--utc", "-o", "short-iso", "-u", "nuit-cloud-cp-probe.service", "--since", "2026-11-08 10:00:00", "--until", "2026-11-08 11:30:05"}
	if got := Argv(q); !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
}

func TestScrub(t *testing.T) {
	long := strings.Repeat("Ab3", 10)
	removed := []string{
		"Authorization: Bearer " + long,
		"got token=" + long + " ok",
		"Token: " + long,
		"authorization: " + long,
		"BEARER " + long,
		"enrol code nce_abc123 used",
		"agent nca_xyz",
		"smt_secret1 here",
		"svc_tokenvalue here",
		"key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIabc comment",
		"-----BEGIN CERTIFICATE-----",
	}
	for _, l := range removed {
		got, keep := Scrub(l)
		if !keep || !strings.Contains(got, "[removed]") || strings.Contains(got, long) || strings.Contains(got, "abc123") ||
			strings.Contains(got, "AAAAC3") || strings.Contains(got, "xyz") || strings.Contains(got, "secret1") || strings.Contains(got, "tokenvalue") {
			t.Errorf("%q -> %q", l, got)
		}
	}
	if got, _ := Scrub("got token=" + long + " ok"); got != "got token=[removed] ok" {
		t.Errorf("%q", got)
	}
	for _, l := range []string{"-----BEGIN PRIVATE KEY-----", "x BEGIN RSA PRIVATE KEY y"} {
		if _, keep := Scrub(l); keep {
			t.Errorf("%q kept", l)
		}
	}
	for _, l := range []string{"token=short", "Bearer abc", "instance_name=a", "plain line"} {
		if got, keep := Scrub(l); !keep || got != l {
			t.Errorf("%q changed to %q", l, got)
		}
	}
}

func fake(out, errOut string, err error, seen *[]string) Exec {
	return func(ctx context.Context, argv []string) (string, string, error) {
		if seen != nil {
			*seen = argv
		}
		return out, errOut, err
	}
}

func query(t *testing.T, extra map[string]any) Query {
	p := good()
	for k, v := range extra {
		p[k] = v
	}
	q, err := Validate(p, now)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

const normal = "2026-11-08T10:00:01+0000 host sched[1]: started\n2026-11-08T10:05:00+0000 host sched[1]: tick\n2026-11-08T10:09:59+0000 host sched[1]: done\n"

func TestRunNormalReturnsLinesAndBounds(t *testing.T) {
	var argv []string
	res := Run(context.Background(), fake(normal, "", nil, &argv), query(t, nil))
	if len(res.Lines) != 3 || res.Truncated || res.Note != "" || res.Unit != "nuit-cloud-cp-scheduler.service" {
		t.Fatalf("%+v", res)
	}
	if *res.Earliest != "2026-11-08T10:00:01+0000" || *res.Latest != "2026-11-08T10:09:59+0000" {
		t.Errorf("%v %v", *res.Earliest, *res.Latest)
	}
	if argv[0] != "journalctl" || argv[6] != "nuit-cloud-cp-scheduler.service" {
		t.Errorf("%v", argv)
	}
}

func TestRunEmptyAndNoEntries(t *testing.T) {
	for _, out := range []string{"", "-- No entries --\n"} {
		res := Run(context.Background(), fake(out, "", nil, nil), query(t, nil))
		if len(res.Lines) != 0 || res.Lines == nil || res.Earliest != nil || res.Latest != nil || res.Note != "" || res.Truncated {
			t.Errorf("%q: %+v", out, res)
		}
	}
}

func TestRunNotes(t *testing.T) {
	cases := []struct {
		out, errOut string
		err         error
		note        string
	}{
		{"No journal files were found.\n", "", nil, "no-journal"},
		{"", "No journal files were found.", errors.New("exit 1"), "no-journal"},
		{"", "Failed to open journal: Permission denied", errors.New("exit 1"), "unreadable"},
		{"", "Hint: You are currently not seeing messages from other users and the system.", nil, "unreadable"},
		{"", "Unit nuit-cloud-cp-probe.service could not be found.", errors.New("exit 1"), "unit-not-found"},
	}
	for _, c := range cases {
		res := Run(context.Background(), fake(c.out, c.errOut, c.err, nil), query(t, nil))
		if res.Note != c.note || len(res.Lines) != 0 || res.Lines == nil {
			t.Errorf("%+v -> %+v", c, res)
		}
	}
}

func TestRunScrubsBeforeAnythingLeaves(t *testing.T) {
	tok := strings.Repeat("Zz9", 12)
	out := "2026-11-08T10:00:01+0000 h s[1]: Authorization: Bearer " + tok + "\n" +
		"2026-11-08T10:00:02+0000 h s[1]: enrol nce_ABCDEF123456\n" +
		"2026-11-08T10:00:03+0000 h s[1]: -----BEGIN PRIVATE KEY-----\n" +
		"2026-11-08T10:00:04+0000 h s[1]: fine\n"
	res := Run(context.Background(), fake(out, "", nil, nil), query(t, nil))
	joined := strings.Join(res.Lines, "\n")
	if len(res.Lines) != 3 || strings.Contains(joined, tok) || strings.Contains(joined, "ABCDEF123456") || strings.Contains(joined, "PRIVATE") {
		t.Errorf("%q", joined)
	}
	// the filter cannot be used to probe removed text
	res = Run(context.Background(), fake(out, "", nil, nil), query(t, map[string]any{"contains": "ABCDEF"}))
	if len(res.Lines) != 0 {
		t.Errorf("%v", res.Lines)
	}
}

func TestRunKeepsTheNewestLinesAndSaysTruncated(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "2026-11-08T10:00:%02d+0000 h s[1]: line %d\n", i, i)
	}
	res := Run(context.Background(), fake(b.String(), "", nil, nil), query(t, map[string]any{"lines": float64(3)}))
	if len(res.Lines) != 3 || !res.Truncated || !strings.HasSuffix(res.Lines[0], "line 8") || !strings.HasSuffix(res.Lines[2], "line 10") {
		t.Errorf("%+v", res)
	}
	if *res.Earliest != "2026-11-08T10:00:08+0000" || *res.Latest != "2026-11-08T10:00:10+0000" {
		t.Errorf("%v %v", *res.Earliest, *res.Latest)
	}
}

func TestRunContainsIsALiteralSubstringFilter(t *testing.T) {
	out := "2026-11-08T10:00:01+0000 h s[1]: job=a failed\n2026-11-08T10:00:02+0000 h s[1]: job=b ok\n2026-11-08T10:00:03+0000 h s[1]: job=a ok\n"
	var argv []string
	res := Run(context.Background(), fake(out, "", nil, &argv), query(t, map[string]any{"contains": "job=a"}))
	if len(res.Lines) != 2 || res.Truncated {
		t.Errorf("%v", res.Lines)
	}
	for _, a := range argv {
		if strings.Contains(a, "job=a") || a == "--grep" || a == "-g" {
			t.Errorf("filter leaked into argv: %v", argv)
		}
	}
	res = Run(context.Background(), fake(out, "", nil, nil), query(t, map[string]any{"contains": "job=."}))
	if len(res.Lines) != 0 {
		t.Errorf("filter was treated as a pattern: %v", res.Lines)
	}
}

func TestRunCapsTheTotalSizeDroppingTheOldest(t *testing.T) {
	var b strings.Builder
	pad := strings.Repeat("x", 900)
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "2026-11-08T10:00:00+0000 h s[1]: %04d %s\n", i, pad)
	}
	res := Run(context.Background(), fake(b.String(), "", nil, nil), query(t, map[string]any{"lines": float64(5000)}))
	size := 0
	for _, l := range res.Lines {
		size += len(l) + 1
	}
	if !res.Truncated || size > MaxBytes || len(res.Lines) == 0 || !strings.Contains(res.Lines[len(res.Lines)-1], "0399") || strings.Contains(res.Lines[0], " 0000 ") {
		t.Errorf("truncated=%v size=%d n=%d", res.Truncated, size, len(res.Lines))
	}
}
