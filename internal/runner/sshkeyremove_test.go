package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/sshkeyremove"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
)

const (
	skAdminBody   = "AAAAC3NzaC1lZDI1NTE5AAAAIN1EVQeq4/76EOVA+4JJrejxcYhj6TfJADtQh2Q8vPNf"
	skAdminFP     = "SHA256:Ay57aMSdJxO7wU3gJnay11N4bbX6TF138GrApHUBawI"
	skUnknownBody = "AAAAB3NzaC1yc2EAAAADAQABAAAAgQDK/bws4jcs/QYpWXOfrfM0GxipjIpCMjCSWIphZSTekcpJUwIkvK+G9SxOkqFtnHKllT3C331WBHbTEACGRuHi/O1rSfGSHaQ3zzZz0zUZmSNCt/erHnDtIIc5ecpU24pjMvdYnwfinvF6aN3hGHsGOuesQ4s07tcsfC63xMrBuw=="
	skUnknownFP   = "SHA256:lDp/Iu2WYOYkwN/WJYf1NAV+lnHYesY5aamC9RjhtQw"
	skPlatBody    = "AAAAC3NzaC1lZDI1NTE5AAAAIJsNe/5LjDAdJS5Ag4hsbVRK5sMWSsiK4nTf4L6Cv5h0"
	skPlatFP      = "SHA256:j6E/Mq8GF3JR6XHxjzHF9PqXBSRt3IRLXzdn5TBQk04"
	skAbsentFP    = "SHA256:mtSvFaYYmAd/Zs3sjPQUHLHRZCAqqLXjlrETgdCfja0"
)

var skKeys = "ssh-ed25519 " + skPlatBody + " nuit-cloud:platform\nssh-ed25519 " + skAdminBody + " admin\nssh-rsa " + skUnknownBody + " stranger@somewhere\n"

// sshKeysRunner has a scratch /etc/passwd and authorized_keys and the real writer, run directly with /bin/sh (there is no systemd here).
func sshKeysRunner(t *testing.T) (Runner, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "home/nuitcloud/.ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("nuitcloud:x:1000:1000::/home/nuitcloud:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(file, []byte(skKeys), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRunner()
	r.SSHKeys = sshkeyremove.Remover{Root: root}
	return r, file
}

func sshKeyTask(params map[string]any) *taskverify.Task {
	return &taskverify.Task{Name: "ssh-key-remove", TimeoutSeconds: 30, Params: params}
}

func TestSSHKeyRemoveTakesOutTheUnknownKeyAndReportsOnlyFingerprints(t *testing.T) {
	r, file := sshKeysRunner(t)
	rep := r.Run(context.Background(), sshKeyTask(map[string]any{
		"fingerprints": []any{skUnknownFP, skAbsentFP}, "keep": []any{skPlatFP}, "requireOneOf": []any{skAdminFP},
	}))
	if rep.Status != "done" || *rep.ExitCode != 0 || rep.Output != "removed 1 line(s) holding 1 key(s); 1 not in the file" {
		t.Errorf("%+v", rep)
	}
	raw, _ := json.Marshal(rep.Result)
	if string(raw) != `{"removed":["`+skUnknownFP+`"],"notFound":["`+skAbsentFP+`"],"managed":[],"linesRemoved":1}` {
		t.Errorf("result %s", raw)
	}
	if all, _ := json.Marshal(rep); strings.Contains(string(all), "stranger") || strings.Contains(string(all), skUnknownBody[20:60]) {
		t.Errorf("a key body or comment reached the report: %s", all)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "ssh-ed25519 "+skPlatBody+" nuit-cloud:platform\nssh-ed25519 "+skAdminBody+" admin\n" {
		t.Errorf("%q", got)
	}
}

func TestAHostileSSHKeyRemoveTaskChangesNothing(t *testing.T) {
	r, file := sshKeysRunner(t)
	for _, p := range []map[string]any{
		{"fingerprints": []any{skUnknownFP}, "keep": []any{skPlatFP}, "requireOneOf": []any{skAdminFP}, "user": "root"},
		{"fingerprints": []any{skUnknownFP}, "keep": []any{skPlatFP}},
		{"fingerprints": []any{skPlatFP}, "keep": []any{skPlatFP}, "requireOneOf": []any{skAdminFP}},   // an active registry key
		{"fingerprints": []any{skAdminFP}, "keep": []any{skPlatFP}, "requireOneOf": []any{skAdminFP}},  // the break-glass key
		{"fingerprints": []any{skAdminFP}, "keep": []any{skPlatFP}, "requireOneOf": []any{skAbsentFP}}, // would leave no admin key
		{"fingerprints": []any{"SHA256:../../etc/passwd"}, "keep": []any{skPlatFP}, "requireOneOf": []any{skAdminFP}},
		{"fingerprints": []any{skUnknownFP + "\n"}, "keep": []any{skPlatFP}, "requireOneOf": []any{skAdminFP}},
		{"fingerprints": "ssh-rsa " + skUnknownBody, "keep": []any{skPlatFP}, "requireOneOf": []any{skAdminFP}},
		{},
	} {
		rep := r.Run(context.Background(), sshKeyTask(p))
		if rep.Status != "failed" {
			t.Errorf("%q was accepted: %+v", p, rep)
		}
		if strings.Contains(rep.Output, skUnknownBody[20:60]) {
			t.Errorf("the refusal repeats a key body: %s", rep.Output)
		}
	}
	if got, _ := os.ReadFile(file); string(got) != skKeys {
		t.Errorf("authorized_keys changed: %q", got)
	}
	found := false
	for _, n := range r.Names() {
		found = found || n == "ssh-key-remove"
	}
	if !found {
		t.Error("ssh-key-remove is not in the catalog")
	}
}
