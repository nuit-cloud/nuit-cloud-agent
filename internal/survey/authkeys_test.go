package survey

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Two public keys and the fingerprints `ssh-keygen -lf` printed for them.
const (
	edBody  = "AAAAC3NzaC1lZDI1NTE5AAAAIN1EVQeq4/76EOVA+4JJrejxcYhj6TfJADtQh2Q8vPNf"
	edFP    = "SHA256:Ay57aMSdJxO7wU3gJnay11N4bbX6TF138GrApHUBawI"
	rsaBody = "AAAAB3NzaC1yc2EAAAADAQABAAAAgQDK/bws4jcs/QYpWXOfrfM0GxipjIpCMjCSWIphZSTekcpJUwIkvK+G9SxOkqFtnHKllT3C331WBHbTEACGRuHi/O1rSfGSHaQ3zzZz0zUZmSNCt/erHnDtIIc5ecpU24pjMvdYnwfinvF6aN3hGHsGOuesQ4s07tcsfC63xMrBuw=="
	rsaFP   = "SHA256:lDp/Iu2WYOYkwN/WJYf1NAV+lnHYesY5aamC9RjhtQw"
)

func TestTheAuthorizedKeysAreReportedByFingerprintWithThePlatformsIds(t *testing.T) {
	text := "# notes\n\n" +
		"ssh-ed25519 " + edBody + " nuit-cloud:platform\n" +
		"from=\"10.0.0.0/8\" ssh-rsa " + rsaBody + " someone@laptop\r\n" +
		"ssh-rsa " + rsaBody + " nuit-cloud:platform extra\n" + // four fields: not the platform's marker
		"ssh-ed25519 " + edBody + " nuit-cloud:Bad\n" + // not an id
		"ssh-ed25519 not-base64!!\n" +
		"garbage\n"
	got := ParseAuthorizedKeys(text)
	if len(got) != 4 {
		t.Fatalf("%d keys: %+v", len(got), got)
	}
	want := []struct{ fp, typ, id string }{{edFP, "ssh-ed25519", "platform"}, {rsaFP, "ssh-rsa", ""}, {rsaFP, "ssh-rsa", ""}, {edFP, "ssh-ed25519", ""}}
	for i, w := range want {
		id := ""
		if got[i].ManagedID != nil {
			id = *got[i].ManagedID
		}
		if got[i].Fingerprint != w.fp || got[i].Type != w.typ || id != w.id {
			t.Errorf("key %d: %+v, want %+v", i, got[i], w)
		}
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), edBody[30:]) || strings.Contains(string(raw), "someone") {
		t.Errorf("a key body or a comment reached the report: %s", raw)
	}
	if !strings.Contains(string(raw), `"managedId":null`) {
		t.Errorf("an unmanaged key must say managedId null: %s", raw)
	}
}

func authSources(root string) Sources {
	return Sources{
		Root:       root,
		Run:        func(context.Context, string, ...string) (string, error) { return "", errors.New("no commands here") },
		Now:        time.Now,
		Hostname:   func() (string, error) { return "box", nil },
		Interfaces: func() ([]net.Interface, error) { return nil, nil },
		Addrs:      func(net.Interface) ([]net.Addr, error) { return nil, nil },
		Statfs:     func(string) (int64, int64, error) { return 0, 0, nil },
		Open:       func(string) error { return nil },
	}
}

func authProblems(r Report) []string {
	var out []string
	for _, p := range r.Problems {
		if strings.HasPrefix(p, "authorized_keys:") {
			out = append(out, p)
		}
	}
	return out
}

func TestTheSurveyReadsTheLoginAccountsKeysAndSaysWhenItCannot(t *testing.T) {
	root := t.TempDir()
	put := func(path, text string, mode os.FileMode) {
		p := filepath.Join(root, path)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	put("/etc/passwd", "root:x:0:0:root:/root:/bin/bash\nnuitcloud:x:1000:1000::/home/nuitcloud:/bin/bash\n", 0o644)

	// no keys file: no keys, and nothing missing from the report
	r := Collect(context.Background(), authSources(root))
	if r.AuthorizedKeys == nil || len(r.AuthorizedKeys) != 0 || len(authProblems(r)) != 0 {
		t.Errorf("no file: %+v %v", r.AuthorizedKeys, r.Problems)
	}

	put("/home/nuitcloud/.ssh/authorized_keys", "ssh-ed25519 "+edBody+" nuit-cloud:ops-laptop\nssh-rsa "+rsaBody+" me\n", 0o600)
	r = Collect(context.Background(), authSources(root))
	if len(r.AuthorizedKeys) != 2 || r.AuthorizedKeys[0].Fingerprint != edFP || r.AuthorizedKeys[0].ManagedID == nil || *r.AuthorizedKeys[0].ManagedID != "ops-laptop" ||
		r.AuthorizedKeys[1].ManagedID != nil || len(authProblems(r)) != 0 {
		t.Errorf("keys: %+v %v", r.AuthorizedKeys, r.Problems)
	}
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), `"authorizedKeys":[{"fingerprint":"`+edFP+`","type":"ssh-ed25519","managedId":"ops-laptop"}`) {
		t.Errorf("the report's shape: %s", raw)
	}

	// a keys file that cannot be read (a directory in its place stands in for a permission error): unknown, and said so
	_ = os.Remove(filepath.Join(root, "/home/nuitcloud/.ssh/authorized_keys"))
	_ = os.MkdirAll(filepath.Join(root, "/home/nuitcloud/.ssh/authorized_keys"), 0o700)
	r = Collect(context.Background(), authSources(root))
	if r.AuthorizedKeys != nil || len(authProblems(r)) != 1 {
		t.Errorf("unreadable: %+v %v", r.AuthorizedKeys, r.Problems)
	}
	raw, _ = json.Marshal(r)
	if !strings.Contains(string(raw), `"authorizedKeys":null`) {
		t.Errorf("an unreadable file must be null, not an empty list: %s", raw)
	}

	// no login account at all
	put("/etc/passwd", "root:x:0:0:root:/root:/bin/bash\n", 0o644)
	r = Collect(context.Background(), authSources(root))
	if p := authProblems(r); r.AuthorizedKeys != nil || len(p) != 1 || !strings.Contains(p[0], "no account nuitcloud") {
		t.Errorf("no account: %+v %v", r.AuthorizedKeys, r.Problems)
	}
}
