package sshkeyremove

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Public keys and the fingerprints `ssh-keygen -lf` printed for them.
const (
	adminBody   = "AAAAC3NzaC1lZDI1NTE5AAAAIN1EVQeq4/76EOVA+4JJrejxcYhj6TfJADtQh2Q8vPNf" // break-glass
	adminFP     = "SHA256:Ay57aMSdJxO7wU3gJnay11N4bbX6TF138GrApHUBawI"
	unknownBody = "AAAAB3NzaC1yc2EAAAADAQABAAAAgQDK/bws4jcs/QYpWXOfrfM0GxipjIpCMjCSWIphZSTekcpJUwIkvK+G9SxOkqFtnHKllT3C331WBHbTEACGRuHi/O1rSfGSHaQ3zzZz0zUZmSNCt/erHnDtIIc5ecpU24pjMvdYnwfinvF6aN3hGHsGOuesQ4s07tcsfC63xMrBuw=="
	unknownFP   = "SHA256:lDp/Iu2WYOYkwN/WJYf1NAV+lnHYesY5aamC9RjhtQw"
	platBody    = "AAAAC3NzaC1lZDI1NTE5AAAAIJsNe/5LjDAdJS5Ag4hsbVRK5sMWSsiK4nTf4L6Cv5h0" // the registry's active key
	platFP      = "SHA256:j6E/Mq8GF3JR6XHxjzHF9PqXBSRt3IRLXzdn5TBQk04"
	markedBody  = "AAAAC3NzaC1lZDI1NTE5AAAAIDrvaOQTUmziYVt72oMwTzRwV8zuBbYIqeXg8uEQogCn" // on a platform-marked line
	markedFP    = "SHA256:5Eft4G7cGywO66ZniX8vEq00FkH4gD/8E/08or7DS4M"
	absentFP    = "SHA256:mtSvFaYYmAd/Zs3sjPQUHLHRZCAqqLXjlrETgdCfja0"
)

func params(remove ...string) Params {
	return Params{Fingerprints: remove, Keep: []string{platFP}, RequireOneOf: []string{adminFP}}
}

func TestOnlyTheUnmarkedLinesHoldingTheKeysGoAndEveryOtherByteStays(t *testing.T) {
	in := "# who is allowed in\r\n" +
		"\n" +
		"ssh-ed25519 " + platBody + " nuit-cloud:platform\n" +
		"from=\"10.0.0.0/8\",no-pty ssh-rsa " + unknownBody + " someone@laptop\r\n" +
		"ssh-ed25519 " + adminBody + " break-glass\n" +
		"ssh-ed25519 " + markedBody + " nuit-cloud:ops-laptop\n" +
		"ssh-rsa " + unknownBody + "\n" + // the same key again, no comment
		"ssh-ed25519 not-base64!!\n" +
		"   garbage line   \n"
	want := "# who is allowed in\r\n" +
		"\n" +
		"ssh-ed25519 " + platBody + " nuit-cloud:platform\n" +
		"ssh-ed25519 " + adminBody + " break-glass\n" +
		"ssh-ed25519 " + markedBody + " nuit-cloud:ops-laptop\n" +
		"ssh-ed25519 not-base64!!\n" +
		"   garbage line   \n"
	out, res, changed, err := Plan([]byte(in), params(unknownFP, markedFP, absentFP))
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	if string(out) != want {
		t.Errorf("new content:\n%q\nwant\n%q", out, want)
	}
	if res.LinesRemoved != 2 || strings.Join(res.Removed, ",") != unknownFP || strings.Join(res.Managed, ",") != markedFP || strings.Join(res.NotFound, ",") != absentFP {
		t.Errorf("%+v", res)
	}
	raw, _ := json.Marshal(res)
	if strings.Contains(string(raw), "someone") || strings.Contains(string(raw), unknownBody[20:60]) {
		t.Errorf("a key body or a comment reached the result: %s", raw)
	}
	if !strings.Contains(string(raw), `"linesRemoved":2`) {
		t.Errorf("result shape: %s", raw)
	}
}

func TestAFileWithoutAFinalNewlineStaysWithoutOne(t *testing.T) {
	in := "ssh-ed25519 " + adminBody + " a\r\nssh-rsa " + unknownBody + " b"
	out, _, changed, err := Plan([]byte(in), params(unknownFP))
	if err != nil || !changed || string(out) != "ssh-ed25519 "+adminBody+" a" {
		t.Errorf("%q %v %v", out, changed, err)
	}
	in = "ssh-rsa " + unknownBody + " b\nssh-ed25519 " + adminBody + " a"
	out, _, _, _ = Plan([]byte(in), params(unknownFP))
	if string(out) != "ssh-ed25519 "+adminBody+" a" {
		t.Errorf("%q", out)
	}
}

func TestItNeverLeavesNoAdministratorOrBreakGlassKey(t *testing.T) {
	in := "ssh-ed25519 " + platBody + " nuit-cloud:platform\nssh-rsa " + unknownBody + "\n"
	_, _, _, err := Plan([]byte(in), params(unknownFP))
	if err == nil || !strings.Contains(err.Error(), "no administrator or break-glass key") {
		t.Errorf("%v", err)
	}
	// a commented-out admin key is not a key
	_, _, _, err = Plan([]byte("# ssh-ed25519 "+adminBody+"\n"+in), params(unknownFP))
	if err == nil {
		t.Error("a commented-out key counted as an administrator key")
	}
	// the administrator key on a marked line, or behind options, counts
	for _, admin := range []string{"ssh-ed25519 " + adminBody + " nuit-cloud:admin\n", "no-pty ssh-ed25519 " + adminBody + "\n"} {
		if _, _, changed, err := Plan([]byte(admin+in), params(unknownFP)); err != nil || !changed {
			t.Errorf("%q: %v %v", admin, changed, err)
		}
	}
}

func TestAKeepKeyOnALineToRemoveIsRefusedEvenIfParseWasBypassed(t *testing.T) {
	in := "ssh-ed25519 " + adminBody + "\nssh-ed25519 " + platBody + " stray\n"
	p := Params{Fingerprints: []string{platFP}, Keep: []string{platFP}, RequireOneOf: []string{adminFP}}
	if _, _, _, err := Plan([]byte(in), p); err == nil || !strings.Contains(err.Error(), "active keys") {
		t.Errorf("%v", err)
	}
}

func TestNothingToRemoveChangesNothing(t *testing.T) {
	in := "ssh-ed25519 " + platBody + " nuit-cloud:platform\nssh-ed25519 " + markedBody + " nuit-cloud:x\n"
	out, res, changed, err := Plan([]byte(in), params(markedFP, absentFP))
	if err != nil || changed || string(out) != in || res.LinesRemoved != 0 || len(res.Managed) != 1 || len(res.NotFound) != 1 || res.Removed == nil {
		t.Errorf("%q %+v %v %v", out, res, changed, err)
	}
}

func fp(i int) string {
	return "SHA256:" + strings.Repeat(string(rune('A'+i%26)), 42) + string(rune('a'+i/26))
}

func list(n int) []any {
	out := []any{}
	for i := 0; i < n; i++ {
		out = append(out, fp(i))
	}
	return out
}

func TestParseTakesExactlyTheThreeListsOfFingerprints(t *testing.T) {
	good := map[string]any{"fingerprints": []any{unknownFP}, "keep": []any{platFP}, "requireOneOf": []any{adminFP}}
	p, err := Parse(good)
	if err != nil || p.Fingerprints[0] != unknownFP || p.Keep[0] != platFP || p.RequireOneOf[0] != adminFP {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := Parse(map[string]any{"fingerprints": list(16), "keep": list(64)[16:], "requireOneOf": []any{adminFP}}); err != nil {
		t.Errorf("the limits: %v", err)
	}
	with := func(k string, v any) map[string]any {
		m := map[string]any{"fingerprints": []any{unknownFP}, "keep": []any{platFP}, "requireOneOf": []any{adminFP}}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	for name, m := range map[string]map[string]any{
		"extra key":           with("user", "root"),
		"no fingerprints":     with("fingerprints", nil),
		"no keep":             with("keep", nil),
		"no requireOneOf":     with("requireOneOf", nil),
		"empty fingerprints":  with("fingerprints", []any{}),
		"empty keep":          with("keep", []any{}),
		"empty requireOneOf":  with("requireOneOf", []any{}),
		"17 fingerprints":     with("fingerprints", list(17)),
		"65 keep":             with("keep", list(65)),
		"17 requireOneOf":     with("requireOneOf", list(17)),
		"a string, not list":  with("fingerprints", unknownFP),
		"a number":            with("fingerprints", []any{42}),
		"duplicate":           with("fingerprints", []any{unknownFP, unknownFP}),
		"padded":              with("fingerprints", []any{unknownFP[:49] + "="}),
		"short":               with("fingerprints", []any{unknownFP[:49]}),
		"md5":                 with("fingerprints", []any{"MD5:16:27:ac:a5:76:28:2d:36:63:1b:56:4d:eb:df:a6:48"}),
		"newline":             with("fingerprints", []any{unknownFP + "\n" + adminFP}),
		"shell":               with("fingerprints", []any{"SHA256:$(rm -rf /)aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}),
		"bad keep":            with("keep", []any{"ssh-ed25519 " + platBody}),
		"bad requireOneOf":    with("requireOneOf", []any{"%h"}),
		"remove a keep key":   with("keep", []any{platFP, unknownFP}),
		"remove an admin key": with("requireOneOf", []any{adminFP, unknownFP}),
	} {
		if _, err := Parse(m); err == nil {
			t.Errorf("%s was accepted", name)
		} else if strings.Contains(err.Error(), "rm -rf") || strings.Contains(err.Error(), platBody) {
			t.Errorf("%s: the refusal repeats the value: %v", name, err)
		}
	}
}

// machine is a scratch root with /etc/passwd and the login account's authorized_keys.
func machine(t *testing.T, keys string) (root, file string) {
	t.Helper()
	root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	passwd := "root:x:0:0:root:/root:/bin/bash\nnuitcloud:x:1000:1000::/home/nuitcloud:/bin/bash\n"
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "home/nuitcloud/.ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(dir, "authorized_keys")
	if keys != "" {
		if err := os.WriteFile(file, []byte(keys), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, file
}

type call struct{ content, home, user, group, sha string }

func stub(calls *[]call) Writer {
	return func(_ context.Context, content []byte, home, user, group, sha string) error {
		*calls = append(*calls, call{string(content), home, user, group, sha})
		return nil
	}
}

func TestTheRemoverHandsTheWriterTheNewContentAndWhatItRead(t *testing.T) {
	keys := "ssh-ed25519 " + adminBody + "\nssh-rsa " + unknownBody + " x\n"
	root, _ := machine(t, keys)
	var calls []call
	res, err := Remover{Root: root, Write: stub(&calls)}.Remove(context.Background(), params(unknownFP))
	if err != nil || res.LinesRemoved != 1 || len(calls) != 1 {
		t.Fatalf("%+v %v %d", res, err, len(calls))
	}
	c := calls[0]
	if c.content != "ssh-ed25519 "+adminBody+"\n" || c.home != filepath.Join(root, "/home/nuitcloud") || c.user != "nuitcloud" || c.group != "1000" ||
		c.sha != fmt.Sprintf("%x", sha256.Sum256([]byte(keys))) {
		t.Errorf("%+v", c)
	}
}

func TestAMissingFileOrNoAccountOrALinkedDirectoryWritesNothing(t *testing.T) {
	root, _ := machine(t, "")
	var calls []call
	res, err := Remover{Root: root, Write: stub(&calls)}.Remove(context.Background(), params(unknownFP))
	if err != nil || len(res.NotFound) != 1 || len(calls) != 0 {
		t.Errorf("missing file: %+v %v %d", res, err, len(calls))
	}

	root, _ = machine(t, "ssh-rsa "+unknownBody+"\n")
	_ = os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("root:x:0:0:root:/root:/bin/bash\n"), 0o644)
	if _, err := (Remover{Root: root, Write: stub(&calls)}).Remove(context.Background(), params(unknownFP)); err == nil || !strings.Contains(err.Error(), "no account nuitcloud") {
		t.Errorf("no account: %v", err)
	}

	for _, passwd := range []string{
		"nuitcloud:x:1000:1000::/home/nuit%hcloud:/bin/bash\n",
		"nuitcloud:x:1000:1000::/home/../etc:/bin/bash\n",
		"nuitcloud:x:1000:1000::home/nuitcloud:/bin/bash\n",
		"nuitcloud:x:1000:wheel::/home/nuitcloud:/bin/bash\n",
	} {
		_ = os.WriteFile(filepath.Join(root, "etc/passwd"), []byte(passwd), 0o644)
		if _, err := (Remover{Root: root, Write: stub(&calls)}).Remove(context.Background(), params(unknownFP)); err == nil {
			t.Errorf("%q was accepted", passwd)
		}
	}

	root, _ = machine(t, "")
	elsewhere := t.TempDir()
	_ = os.WriteFile(filepath.Join(elsewhere, "authorized_keys"), []byte("ssh-ed25519 "+adminBody+"\nssh-rsa "+unknownBody+"\n"), 0o600)
	_ = os.RemoveAll(filepath.Join(root, "home/nuitcloud/.ssh"))
	_ = os.Symlink(elsewhere, filepath.Join(root, "home/nuitcloud/.ssh"))
	if _, err := (Remover{Root: root, Write: stub(&calls)}).Remove(context.Background(), params(unknownFP)); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("linked .ssh: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("the writer was called: %+v", calls)
	}
}

func TestAWriterFailureIsTheTasksFailure(t *testing.T) {
	root, _ := machine(t, "ssh-ed25519 "+adminBody+"\nssh-rsa "+unknownBody+"\n")
	w := func(context.Context, []byte, string, string, string, string) error { return errors.New("boom") }
	if _, err := (Remover{Root: root, Write: w}).Remove(context.Background(), params(unknownFP)); err == nil || err.Error() != "boom" {
		t.Errorf("%v", err)
	}
}

// The real script, run directly with /bin/sh in a scratch home.
func TestTheRealWriterReplacesTheFileAtomicallyWithMode600(t *testing.T) {
	keys := "# keep me\nssh-ed25519 " + adminBody + " admin\r\nssh-rsa " + unknownBody + " x\nssh-ed25519 " + markedBody + " nuit-cloud:ops\n"
	root, file := machine(t, keys)
	_ = os.Chmod(file, 0o644)
	res, err := Remover{Root: root}.Remove(context.Background(), params(unknownFP))
	if err != nil || res.LinesRemoved != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "# keep me\nssh-ed25519 "+adminBody+" admin\r\nssh-ed25519 "+markedBody+" nuit-cloud:ops\n" {
		t.Errorf("%q", got)
	}
	fi, _ := os.Stat(file)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	assertNoTemp(t, filepath.Dir(file))
}

func TestTheRealWriterRefusesAFileThatChangedOrIsNotPlain(t *testing.T) {
	_, file := machine(t, "ssh-ed25519 "+adminBody+"\n")
	home := filepath.Dir(filepath.Dir(file))
	wrong := strings.Repeat("0", 64)
	err := WriteScript(context.Background(), false, []byte("new\n"), home, "nuitcloud", "1000", wrong)
	if err == nil || err.Error() != "authorized_keys changed while the removal was prepared: nothing was changed" {
		t.Errorf("%v", err)
	}
	if got, _ := os.ReadFile(file); string(got) != "ssh-ed25519 "+adminBody+"\n" {
		t.Errorf("changed: %q", got)
	}
	assertNoTemp(t, filepath.Dir(file))

	// a symbolic-link .ssh
	other := t.TempDir()
	linkHome := t.TempDir()
	_ = os.WriteFile(filepath.Join(other, "authorized_keys"), []byte("x\n"), 0o600)
	_ = os.Symlink(other, filepath.Join(linkHome, ".ssh"))
	if err := WriteScript(context.Background(), false, []byte("new\n"), linkHome, "nuitcloud", "1000", wrong); err == nil || !strings.Contains(err.Error(), "not a plain directory") {
		t.Errorf("linked .ssh: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(other, "authorized_keys")); string(got) != "x\n" {
		t.Errorf("written through the link: %q", got)
	}

	// authorized_keys a symbolic link, or a directory
	_ = os.Remove(file)
	_ = os.Symlink(filepath.Join(other, "authorized_keys"), file)
	if err := WriteScript(context.Background(), false, []byte("new\n"), home, "nuitcloud", "1000", wrong); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("linked file: %v", err)
	}
	_ = os.Remove(file)
	_ = os.Mkdir(file, 0o700)
	if err := WriteScript(context.Background(), false, []byte("new\n"), home, "nuitcloud", "1000", wrong); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory: %v", err)
	}

	// hostile arguments never reach a shell
	for _, a := range [][4]string{{home + "/%h", "nuitcloud", "1000", wrong}, {home, "root", "0", wrong}, {home, "nuitcloud", "1000;id", wrong}, {home, "nuitcloud", "1000", "abc"}, {"relative", "nuitcloud", "1000", wrong}} {
		if err := WriteScript(context.Background(), false, nil, a[0], a[1], a[2], a[3]); err == nil || !strings.Contains(err.Error(), "not usable") {
			t.Errorf("%q: %v", a, err)
		}
	}
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".authorized_keys.nuit-cloud.") {
			t.Errorf("a temporary file was left: %s", e.Name())
		}
	}
}

func TestTheWriterCarriesNothingSystemdWouldExpand(t *testing.T) {
	for _, bad := range []string{"${", "%", "$$"} {
		if strings.Contains(writeScript, bad) {
			t.Errorf("the writer script holds %q, which systemd-run would expand", bad)
		}
	}
}
