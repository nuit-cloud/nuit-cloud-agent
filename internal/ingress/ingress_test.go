package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/client"
)

// The vector the platform's TypeScript computes too (nuit-cloud/nuit-cloud#184): if the two disagree, no apply ever passes.
const vectorHex = "db6702ba0dafa2ae293f27bc197b5df905f911a2f9461d007d4ee04dedb89295"

func TestTheRevisionIsTheSharedVector(t *testing.T) {
	got := Revision([]Site{{File: "00-nuit-cloud-default.conf", Content: "a"}}, []Certificate{{Dir: "/c/x", FullchainPEM: "F", PrivkeyPEM: "K"}})
	if got != vectorHex {
		t.Errorf("revision of the shared vector: %s, want %s", got, vectorHex)
	}
}

func TestTheRevisionSortsSitesAndCertificatesByteWise(t *testing.T) {
	a := Revision([]Site{{"nuit-cloud-b.conf", "2"}, {"00-nuit-cloud-default.conf", "1"}}, []Certificate{{"/c/z", "F", "K"}, {"/c/a", "G", "L"}})
	b := Revision([]Site{{"00-nuit-cloud-default.conf", "1"}, {"nuit-cloud-b.conf", "2"}}, []Certificate{{"/c/a", "G", "L"}, {"/c/z", "F", "K"}})
	if a != b {
		t.Error("the order the platform lists them in changed the revision")
	}
	// the framing keeps a value from running into the next one
	if Revision([]Site{{"nuit-cloud-a.conf", "xy"}}, nil) == Revision([]Site{{"nuit-cloud-a.conf", "x"}}, []Certificate{{"y", "", ""}}) {
		t.Error("two different contents framed to the same bytes")
	}
}

const rev = "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"

func goodParams() map[string]any {
	return map[string]any{"revision": rev, "confDir": "/home/nuitcloud/nginx/conf.d", "certRoot": "/home/nuitcloud/nginx/certs", "container": "nuit-nginx"}
}

func TestTheSignedSettingsAreCheckedAgain(t *testing.T) {
	p, err := ParseParams(goodParams())
	if err != nil || p.Container != "nuit-nginx" || p.ConfDir != "/home/nuitcloud/nginx/conf.d" || p.CertRoot != "/home/nuitcloud/nginx/certs" || p.Revision != rev {
		t.Fatalf("%+v %v", p, err)
	}
	hostile := map[string]func(m map[string]any){
		"an extra key":            func(m map[string]any) { m["command"] = "rm -rf /" },
		"no revision":             func(m map[string]any) { delete(m, "revision") },
		"an upper-case revision":  func(m map[string]any) { m["revision"] = strings.ToUpper(rev) },
		"a short revision":        func(m map[string]any) { m["revision"] = rev[:63] },
		"a relative confDir":      func(m map[string]any) { m["confDir"] = "home/nuitcloud/nginx/conf.d" },
		"a confDir with ..":       func(m map[string]any) { m["confDir"] = "/home/nuitcloud/../../etc" },
		"a confDir with /./":      func(m map[string]any) { m["confDir"] = "/home/./nginx" },
		"a confDir ending in /":   func(m map[string]any) { m["confDir"] = "/home/nuitcloud/nginx/conf.d/" },
		"a confDir with //":       func(m map[string]any) { m["confDir"] = "/home//nginx" },
		"a confDir with a space":  func(m map[string]any) { m["confDir"] = "/home/nuit cloud" },
		"a confDir with ;":        func(m map[string]any) { m["confDir"] = "/home/x;reboot" },
		"a confDir with newline":  func(m map[string]any) { m["confDir"] = "/home/x\n/etc" },
		"a confDir with $":        func(m map[string]any) { m["confDir"] = "/home/$HOME" },
		"a hidden part":           func(m map[string]any) { m["confDir"] = "/home/.ssh" },
		"the root":                func(m map[string]any) { m["certRoot"] = "/" },
		"a certRoot with *":       func(m map[string]any) { m["certRoot"] = "/home/*" },
		"confDir inside certRoot": func(m map[string]any) { m["confDir"] = "/home/nuitcloud/nginx/certs/conf.d" },
		"certRoot inside confDir": func(m map[string]any) { m["certRoot"] = "/home/nuitcloud/nginx/conf.d/certs" },
		"the same directory":      func(m map[string]any) { m["certRoot"] = m["confDir"] },
		"a container with ;":      func(m map[string]any) { m["container"] = "nuit;reboot" },
		"a container with -":      func(m map[string]any) { m["container"] = "-H tcp://evil" },
		"a long container":        func(m map[string]any) { m["container"] = strings.Repeat("a", 64) },
		"a number":                func(m map[string]any) { m["container"] = 7 },
		"a very long path":        func(m map[string]any) { m["confDir"] = "/" + strings.Repeat("a/", 140) + "b" },
	}
	for name, change := range hostile {
		m := goodParams()
		change(m)
		if _, err := ParseParams(m); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for _, v := range []any{nil, "x", []any{}} {
		if _, err := ParseParams(v); err == nil {
			t.Errorf("%v was accepted", v)
		}
	}
}

const (
	keyBody   = "FIXTURE-INGRESS-PRIVATE-KEY-3c9e1d77"
	chainBody = "FIXTURE-INGRESS-CHAIN-BODY-0b5d88e2"
)

func pemCert(body string) string {
	return "-----BEGIN CERTIFICATE-----\n" + body + "\n-----END CERTIFICATE-----\n"
}
func pemKey(body string) string {
	return "-----BEGIN PRIVATE KEY-----\n" + body + "\n-----END PRIVATE KEY-----\n"
}

// answer is the platform's JSON for the given content, with its revision computed as the platform does.
func answer(sites []Site, certs []Certificate) map[string]any {
	s := []any{}
	for _, x := range sites {
		s = append(s, map[string]any{"file": x.File, "content": x.Content})
	}
	c := []any{}
	for _, x := range certs {
		c = append(c, map[string]any{"dir": x.Dir, "fullchainPem": x.FullchainPEM, "privkeyPem": x.PrivkeyPEM})
	}
	return map[string]any{"revision": Revision(sites, certs), "sites": s, "certificates": c, "problems": []any{map[string]any{"route": "x"}}}
}

func wire(t *testing.T, body map[string]any) Wire {
	t.Helper()
	raw, _ := json.Marshal(body)
	var w Wire
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

var (
	goodSites = []Site{{"00-nuit-cloud-default.conf", "# default\n"}, {"nuit-cloud-abc12345-shop.conf", "server { listen 443 ssl; }\n"}}
	goodCerts = []Certificate{{"/home/nuitcloud/nginx/certs/abc12345/shop.example.com", pemCert(chainBody), pemKey(keyBody)},
		{"/home/nuitcloud/nginx/certs/abc12345/*.example.com", pemCert("wild"), pemKey("wildkey")}}
)

func signed(t *testing.T, r string) Params {
	t.Helper()
	m := goodParams()
	m["revision"] = r
	p, err := ParseParams(m)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAConfigurationOfTheSignedRevisionIsAccepted(t *testing.T) {
	body := answer(goodSites, goodCerts)
	c, err := Check(wire(t, body), signed(t, body["revision"].(string)))
	if err != nil || len(c.Sites) != 2 || len(c.Certificates) != 2 || c.Problems != 1 {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestContentThatIsNotTheSignedRevisionIsRefused(t *testing.T) {
	body := answer(goodSites, goodCerts)
	if _, err := Check(wire(t, body), signed(t, rev)); err == nil || !strings.Contains(err.Error(), "not the signed") {
		t.Errorf("a configuration of another revision was accepted: %v", err)
	}
	// content altered after the revision was computed: the recomputed revision is not the signed one
	p := signed(t, body["revision"].(string))
	body["sites"].([]any)[1].(map[string]any)["content"] = "server { listen 443 ssl; include /etc/shadow; }\n"
	if _, err := Check(wire(t, body), p); err == nil {
		t.Error("altered content was accepted under the signed revision")
	}
	// the answer claims the signed revision for other content
	other := answer(goodSites[:1], nil)
	other["revision"] = body["revision"]
	if _, err := Check(wire(t, other), p); err == nil {
		t.Error("an answer whose claimed revision is not its content's was accepted")
	}
}

func TestEveryNameAndDirectoryInTheContentIsCheckedAgainstTheSignedSettings(t *testing.T) {
	root := "/home/nuitcloud/nginx/certs"
	badDirs := []string{"/etc/nginx", root, root + "/", root + "/../../../etc", root + "/a/../../x", root + "/./a", root + "x/a", "/home/nuitcloud/nginx/certsx/a",
		root + "/a b", root + "/a;b", root + "/a\nb", root + "/.hidden", root + "/a/*", root + "/a*.example.com", root + "/**.x", root + "//a", "relative/a", root + "/" + strings.Repeat("a", 260)}
	for _, d := range badDirs {
		certs := []Certificate{{d, pemCert("c"), pemKey("k")}}
		body := answer(goodSites, certs)
		if _, err := Check(wire(t, body), signed(t, body["revision"].(string))); err == nil {
			t.Errorf("the certificate directory %q was accepted", d)
		}
	}
	badFiles := []string{"../evil.conf", "nuit-cloud-a.conf/../../x", "other.conf", "nuit-cloud-.conf", "nuit-cloud-A.conf", "nuit-cloud-a.conf.bak", "nuit-cloud-a b.conf",
		"00-nuit-cloud-default.conf\n", "nuit-cloud-" + strings.Repeat("a", 121) + ".conf", "/etc/nginx/conf.d/nuit-cloud-a.conf"}
	for _, f := range badFiles {
		sites := []Site{{f, "x"}}
		body := answer(sites, nil)
		if _, err := Check(wire(t, body), signed(t, body["revision"].(string))); err == nil {
			t.Errorf("the site file %q was accepted", f)
		}
	}
	bad := map[string]struct {
		sites []Site
		certs []Certificate
	}{
		"a site named twice":        {[]Site{{"nuit-cloud-a.conf", "x"}, {"nuit-cloud-a.conf", "y"}}, nil},
		"a directory named twice":   {goodSites, []Certificate{goodCerts[0], goodCerts[0]}},
		"a NUL in a site":           {[]Site{{"nuit-cloud-a.conf", "x\x00y"}}, nil},
		"an empty site":             {[]Site{{"nuit-cloud-a.conf", ""}}, nil},
		"a key that is not a key":   {goodSites, []Certificate{{root + "/a/b", pemCert("c"), "not a key"}}},
		"a chain that is not a PEM": {goodSites, []Certificate{{root + "/a/b", "rm -rf /", pemKey("k")}}},
		"a NUL in a key":            {goodSites, []Certificate{{root + "/a/b", pemCert("c"), pemKey("k\x00")}}},
	}
	for name, x := range bad {
		body := answer(x.sites, x.certs)
		if _, err := Check(wire(t, body), signed(t, body["revision"].(string))); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// fakeSource stands in for the platform.
type fakeSource struct {
	body  map[string]any
	err   error
	calls *int
}

func (f fakeSource) GetJSON(_ context.Context, path string, _ int64, out any) error {
	if f.calls != nil {
		*f.calls++
	}
	if path != ConfigPath {
		return errors.New("wrong path " + path)
	}
	if f.err != nil {
		return f.err
	}
	raw, _ := json.Marshal(f.body)
	return json.Unmarshal(raw, out)
}

func TestACheckFetchesAndStagesNothing(t *testing.T) {
	calls := 0
	root := filepath.Join(t.TempDir(), "run")
	env, _, cleanup, err := Provisioner{Source: fakeSource{calls: &calls}, StageRoot: root}.Prepare(context.Background(), signed(t, rev), false)
	defer cleanup()
	if err != nil || calls != 0 {
		t.Fatalf("%v, %d fetches", err, calls)
	}
	want := []string{"NCS_INGRESS_REVISION=" + rev, "NCS_INGRESS_CONF_DIR=/home/nuitcloud/nginx/conf.d", "NCS_INGRESS_CERT_ROOT=/home/nuitcloud/nginx/certs", "NCS_INGRESS_CONTAINER=nuit-nginx"}
	if strings.Join(env, "\n") != strings.Join(want, "\n") {
		t.Errorf("%v", env)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Error("a check made a stage")
	}
}

func TestAnApplyStagesTheExactBytesRootOnlyAndCleansUp(t *testing.T) {
	body := answer(goodSites, goodCerts)
	root := filepath.Join(t.TempDir(), "run")
	env, scrub, cleanup, err := Provisioner{Source: fakeSource{body: body}, StageRoot: root}.Prepare(context.Background(), signed(t, body["revision"].(string)), true)
	if err != nil {
		t.Fatal(err)
	}
	var dir string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "NCS_INGRESS_STAGE="); ok {
			dir = v
		}
		if strings.Contains(e, keyBody) || strings.Contains(e, chainBody) {
			t.Error("key material in the environment")
		}
	}
	if !strings.HasPrefix(dir, root+"/") || !strings.Contains(strings.Join(env, "\n"), "NCS_INGRESS_PROBLEMS=1") {
		t.Fatalf("%v", env)
	}
	read := func(rel string) string { b, _ := os.ReadFile(filepath.Join(dir, rel)); return string(b) }
	if read("revision") != body["revision"].(string)+"\n" || read("sites.list") != "00-nuit-cloud-default.conf\nnuit-cloud-abc12345-shop.conf\n" ||
		read("certs.list") != "1 "+goodCerts[1].Dir+"\n2 "+goodCerts[0].Dir+"\n" {
		t.Errorf("lists: %q %q %q", read("revision"), read("sites.list"), read("certs.list"))
	}
	if read("sites/nuit-cloud-abc12345-shop.conf") != goodSites[1].Content || read("certs/2/privkey.pem") != pemKey(keyBody) || read("certs/2/fullchain.pem") != pemCert(chainBody) {
		t.Error("the staged content is not the platform's bytes")
	}
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s is %v", p, info.Mode().Perm())
		}
		return nil
	})
	if s := scrub("nginx said " + keyBody + " and " + pemKey(keyBody)); strings.Contains(s, keyBody) {
		t.Errorf("not scrubbed: %s", s)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the stage is still there after cleanup")
	}
}

func TestARefusedFetchStagesNothingAndSaysWhyPlainly(t *testing.T) {
	body := answer(goodSites, goodCerts)
	for name, c := range map[string]struct {
		src  fakeSource
		want string
	}{
		"403":       {fakeSource{err: &client.StatusError{Status: 403}}, "not the Ingress Service"},
		"503":       {fakeSource{err: &client.StatusError{Status: 503}}, "retryable"},
		"too large": {fakeSource{err: client.ErrTooLarge}, "larger"},
		"mismatch":  {fakeSource{body: answer(goodSites[:1], nil)}, "not the signed"},
	} {
		root := filepath.Join(t.TempDir(), "run")
		_, scrub, cleanup, err := Provisioner{Source: c.src, StageRoot: root}.Prepare(context.Background(), signed(t, body["revision"].(string)), true)
		cleanup()
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(scrub(err.Error()), keyBody) {
			t.Errorf("%s: %v", name, err)
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Errorf("%s: something was staged", name)
		}
	}
}

func TestTheRecordedSettingsAreReadAsStrictlyAsATask(t *testing.T) {
	s, ok := ParseSettings("conf-dir=/home/nuitcloud/nginx/conf.d\ncert-root=/home/nuitcloud/nginx/certs\ncontainer=nuit-nginx\n")
	if !ok || s.Container != "nuit-nginx" || s.ConfDir != "/home/nuitcloud/nginx/conf.d" {
		t.Fatalf("%+v", s)
	}
	for _, bad := range []string{"", "conf-dir=/a\ncert-root=/b\ncontainer=x;reboot\n", "conf-dir=../a\ncert-root=/b\ncontainer=x\n", "conf-dir=/a\ncert-root=/b\ncontainer=x\nshell=sh\n", "conf-dir=/a\ncontainer=x\n"} {
		if _, ok := ParseSettings(bad); ok {
			t.Errorf("%q was accepted", bad)
		}
	}
}
