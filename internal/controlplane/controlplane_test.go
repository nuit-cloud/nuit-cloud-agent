package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/client"
)

const (
	secSched  = "FIXTURE-SCHEDULER-TOKEN-7f3a91c2"
	secKey    = "FIXTURE-TLS-PRIVATE-KEY-b81d44e0"
	secProbeU = "FIXTURE-PROBE-UPDATE-5c0e22aa"
	secProbeB = "FIXTURE-PROBE-BOOTSTRAP-9d17f3b6"
	rev1      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rev2      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var secrets = []string{secSched, secKey, secProbeU, secProbeB}

func goodConfig(revision string) map[string]any {
	return map[string]any{
		"revision":  revision,
		"scheduler": map[string]any{"env": map[string]any{"RENEWAL_SWEEP_TOKEN": secSched, "JOB_REPORT_URL": "http://10.10.10.140:8796/internal/job-run"}},
		"tlsFront": map[string]any{
			"certPem":  "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
			"keyPem":   "-----BEGIN PRIVATE KEY-----\n" + secKey + "\n-----END PRIVATE KEY-----\n",
			"upstream": "10.10.10.140:4173",
		},
		"probe":   map[string]any{"name": "platform-probe", "updateToken": secProbeU + "xxxx", "bootstrapToken": secProbeB + "xxxx"},
		"missing": []any{},
	}
}

// platform is a stand-in for GET /api/agent/control-plane-config.
type platform struct {
	status int
	body   any
	raw    string
	hits   atomic.Int32
	auth   atomic.Value
}

func (p *platform) serve(t *testing.T) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		p.auth.Store(r.Header.Get("Authorization"))
		if r.Method != http.MethodGet || r.URL.Path != ConfigPath {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(p.status)
		if p.raw != "" {
			_, _ = w.Write([]byte(p.raw))
			return
		}
		_ = json.NewEncoder(w).Encode(p.body)
	}))
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL, "9.9.9", true)
	if err != nil {
		t.Fatal(err)
	}
	c.Credential = "nca_credential"
	return c
}

// fixtureBundle is a good bundle for this machine and a fetcher that serves it.
func fixtureBundle(t *testing.T) (Bundle, *memFetcher) {
	t.Helper()
	data := tarball(t, goodEntries("1.2.3"))
	b := Bundle{Version: "1.2.3", Arch: hostArch(), SHA256: sum(data)}
	return b, &memFetcher{files: map[string][]byte{b.File(): data}}
}

func params(t *testing.T, services ...string) Params {
	t.Helper()
	b, _ := fixtureBundle(t)
	list := []any{}
	for _, s := range services {
		list = append(list, s)
	}
	p, err := ParseParams(map[string]any{"services": list, "bundle": map[string]any{"version": b.Version, "arch": b.Arch, "sha256": b.SHA256}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type dirs struct{ conf, install, stage string }

func newProv(t *testing.T, src Source) (Provisioner, dirs, *memFetcher) {
	t.Helper()
	_, f := fixtureBundle(t)
	root := t.TempDir()
	d := dirs{filepath.Join(root, "cp"), filepath.Join(root, "opt"), filepath.Join(root, "stage")}
	return Provisioner{Source: src, Fetcher: f, Dir: d.conf, InstallDir: d.install, StageDir: d.stage, PlatformURL: "https://agent.example"}, d, f
}

func noSecret(t *testing.T, what string, texts ...string) {
	t.Helper()
	for _, text := range texts {
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("%s holds a secret value (%s…)", what, s[:14])
			}
		}
	}
}

func TestParamsAreCheckedAsThePlatformChecksThem(t *testing.T) {
	sha := strings.Repeat("a", 64)
	arch := hostArch()
	bundle := func(m map[string]any) map[string]any {
		b := map[string]any{"version": "1.2.3", "arch": arch, "sha256": sha}
		for k, v := range m {
			if v == nil {
				delete(b, k)
			} else {
				b[k] = v
			}
		}
		return b
	}
	good := map[string]any{"services": []any{"probe", "scheduler"}, "bundle": bundle(nil)}
	p, err := ParseParams(good)
	if err != nil || strings.Join(p.Services, ",") != "scheduler,probe" || p.Bundle != (Bundle{"1.2.3", arch, sha}) {
		t.Fatalf("%+v %v", p, err)
	}
	other := "amd64"
	if arch == "amd64" {
		other = "arm64"
	}
	bad := map[string]map[string]any{
		"extra key":          {"services": []any{"probe"}, "bundle": bundle(nil), "env": map[string]any{}},
		"images (the old)":   {"services": []any{"probe"}, "bundle": bundle(nil), "images": map[string]any{"probe": "p@sha256:" + sha}},
		"no services":        {"bundle": bundle(nil)},
		"empty services":     {"services": []any{}, "bundle": bundle(nil)},
		"unknown service":    {"services": []any{"shell"}, "bundle": bundle(nil)},
		"duplicate":          {"services": []any{"probe", "probe"}, "bundle": bundle(nil)},
		"non-string service": {"services": []any{3}, "bundle": bundle(nil)},
		"no bundle":          {"services": []any{"probe"}},
		"bundle not object":  {"services": []any{"probe"}, "bundle": "x"},
		"bundle extra key":   {"services": []any{"probe"}, "bundle": bundle(map[string]any{"url": "http://evil"})},
		"no version":         {"services": []any{"probe"}, "bundle": bundle(map[string]any{"version": nil})},
		"bad version":        {"services": []any{"probe"}, "bundle": bundle(map[string]any{"version": "../1"})},
		"version with space": {"services": []any{"probe"}, "bundle": bundle(map[string]any{"version": "1 2"})},
		"version too long":   {"services": []any{"probe"}, "bundle": bundle(map[string]any{"version": strings.Repeat("1", 65)})},
		"no arch":            {"services": []any{"probe"}, "bundle": bundle(map[string]any{"arch": nil})},
		"unknown arch":       {"services": []any{"probe"}, "bundle": bundle(map[string]any{"arch": "riscv64"})},
		"the other arch":     {"services": []any{"probe"}, "bundle": bundle(map[string]any{"arch": other})},
		"short sha":          {"services": []any{"probe"}, "bundle": bundle(map[string]any{"sha256": "abc"})},
		"upper-case sha":     {"services": []any{"probe"}, "bundle": bundle(map[string]any{"sha256": strings.Repeat("A", 64)})},
		"sha not string":     {"services": []any{"probe"}, "bundle": bundle(map[string]any{"sha256": 5})},
		"nil":                nil,
	}
	for name, raw := range bad {
		if _, err := ParseParams(raw); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	old := hostArch
	hostArch = func() string { return other }
	defer func() { hostArch = old }()
	if _, err := ParseParams(good); err == nil || !strings.Contains(err.Error(), "this machine is") {
		t.Errorf("a bundle for another architecture was accepted: %v", err)
	}
}

func TestAnApplyInstallsTheBundleReadsTheConfigurationAndWritesRootOnlyFiles(t *testing.T) {
	pf := &platform{status: 200, body: goodConfig(rev1)}
	prov, d, f := newProv(t, pf.serve(t))
	ps := params(t, "scheduler", "tls-front", "probe")
	env, scrub, err := prov.Prepare(context.Background(), ps, true)
	if err != nil {
		t.Fatal(err)
	}
	if pf.auth.Load() != "Bearer nca_credential" {
		t.Errorf("the agent's own credential was not used: %v", pf.auth.Load())
	}
	if f.asked.Load() != 1 {
		t.Errorf("the bundle was downloaded %d times", f.asked.Load())
	}
	if _, err := os.Stat(filepath.Join(d.install, "1.2.3", "ofelia")); err != nil {
		t.Error("the bundle was not unpacked")
	}
	modes := map[string]os.FileMode{"scheduler.env": 0o600, "probe.env": 0o600, "tls/key.pem": 0o600, "tls/cert.pem": 0o644, "revision": 0o644}
	for name, want := range modes {
		st, err := os.Stat(filepath.Join(d.conf, name))
		if err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v %v", name, st, err)
		}
	}
	for _, dir := range []string{d.conf, filepath.Join(d.conf, "tls")} {
		if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
			t.Errorf("%s is %v", dir, st.Mode())
		}
	}
	read := func(n string) string { b, _ := os.ReadFile(filepath.Join(d.conf, n)); return string(b) }
	if got := read("scheduler.env"); got != "JOB_REPORT_URL=\"http://10.10.10.140:8796/internal/job-run\"\nRENEWAL_SWEEP_TOKEN=\""+secSched+"\"\n" {
		t.Errorf("scheduler.env: %q", got)
	}
	if got := read("probe.env"); !strings.Contains(got, "NUIT_CLOUD_API_URL=\"https://agent.example\"\n") || !strings.Contains(got, "DYNDNS_PROBE_NAME=\"platform-probe\"\n") || !strings.Contains(got, "DYNDNS_UPDATE_TOKEN=\""+secProbeU) {
		t.Errorf("probe.env: %q", got)
	}
	if read("revision") != rev1+"\n" || !strings.Contains(read("tls/key.pem"), secKey) {
		t.Error("revision or key not written")
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"NCS_CP_SERVICES=scheduler tls-front probe", "NCS_CP_CONFIG_DIR=" + d.conf, "NCS_CP_INSTALL_DIR=" + d.install, "NCS_CP_REVISION=" + rev1, "NCS_CP_MISSING=", "NCS_CP_BUNDLE_VERSION=1.2.3", "NCS_CP_BUNDLE_SHA256=" + ps.Bundle.SHA256, "NCS_CP_TLS_UPSTREAM=10.10.10.140:4173"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the scripts are not given %s:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "NCS_CP_IMAGE") {
		t.Error("an image variable is still passed")
	}
	noSecret(t, "the environment for the scripts", joined)
	if scrub("x "+secSched+" y") != "x [redacted] y" || scrub("plain") != "plain" {
		t.Error("the scrubber does not remove a secret value")
	}
}

func TestTheHttpsFrontAloneNeedsNoBundle(t *testing.T) {
	pf := &platform{status: 200, body: goodConfig(rev1)}
	prov, d, f := newProv(t, pf.serve(t))
	if _, _, err := prov.Prepare(context.Background(), params(t, "tls-front"), true); err != nil {
		t.Fatal(err)
	}
	if f.asked.Load() != 0 {
		t.Error("the bundle was downloaded for a service that does not use it")
	}
	if _, err := os.Stat(d.install); !os.IsNotExist(err) {
		t.Error("the install directory was made")
	}
	if _, err := os.Stat(filepath.Join(d.conf, "scheduler.env")); !os.IsNotExist(err) {
		t.Error("a file was written for a service the task did not name")
	}
}

func TestACheckFetchesTheConfigurationButNeverWritesOrDownloads(t *testing.T) {
	pf := &platform{status: 200, body: goodConfig(rev1)}
	prov, d, f := newProv(t, pf.serve(t))
	env, _, err := prov.Prepare(context.Background(), params(t, "scheduler", "probe"), false)
	if err != nil || pf.hits.Load() != 1 {
		t.Fatal(err, pf.hits.Load())
	}
	for _, dir := range []string{d.conf, d.install, d.stage} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("a check made %s", dir)
		}
	}
	if f.asked.Load() != 0 {
		t.Error("a check downloaded the bundle")
	}
	if !strings.Contains(strings.Join(env, "\n"), "NCS_CP_REVISION="+rev1) {
		t.Error("the revision the platform has is not passed on for the comparison")
	}
}

func TestAnApplyThatFailsToInstallTheBundleWritesNoSecret(t *testing.T) {
	pf := &platform{status: 200, body: goodConfig(rev1)}
	prov, d, f := newProv(t, pf.serve(t))
	f.files = map[string][]byte{} // the platform has no such file
	_, scrub, err := prov.Prepare(context.Background(), params(t, "scheduler"), true)
	if err == nil {
		t.Fatal("an apply without the bundle succeeded")
	}
	noSecret(t, "the error", err.Error(), scrub(err.Error()))
	if _, statErr := os.Stat(d.conf); !os.IsNotExist(statErr) {
		t.Error("secrets were written although the bundle could not be installed")
	}
}

func inode(t *testing.T, p string) os.FileInfo {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestOnlyFilesWhoseContentChangedAreWrittenAndNothingIsDownloadedAgain(t *testing.T) {
	pf := &platform{status: 200, body: goodConfig(rev1)}
	prov, d, f := newProv(t, pf.serve(t))
	ps := params(t, "scheduler", "tls-front", "probe")
	if _, _, err := prov.Prepare(context.Background(), ps, true); err != nil {
		t.Fatal(err)
	}
	names := []string{"scheduler.env", "probe.env", "tls/key.pem", "tls/cert.pem", "revision"}
	before := map[string]os.FileInfo{}
	for _, n := range names {
		before[n] = inode(t, filepath.Join(d.conf, n))
	}
	if _, _, err := prov.Prepare(context.Background(), ps, true); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		after := inode(t, filepath.Join(d.conf, n))
		if !os.SameFile(before[n], after) || !after.ModTime().Equal(before[n].ModTime()) {
			t.Errorf("%s was rewritten although nothing changed", n)
		}
	}
	if f.asked.Load() != 1 {
		t.Errorf("the bundle was downloaded %d times", f.asked.Load())
	}
	cfg := goodConfig(rev2)
	cfg["probe"].(map[string]any)["updateToken"] = secProbeU + "yyyy"
	pf.body = cfg
	if _, _, err := prov.Prepare(context.Background(), ps, true); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		changed := !os.SameFile(before[n], inode(t, filepath.Join(d.conf, n)))
		if want := n == "probe.env" || n == "revision"; changed != want {
			t.Errorf("%s changed=%v want %v", n, changed, want)
		}
	}
	if err := os.Chmod(filepath.Join(d.conf, "scheduler.env"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prov.Prepare(context.Background(), ps, true); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(d.conf, "scheduler.env")); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
}

func TestWritingLeavesNoTemporaryFileAndNeverHalfAFile(t *testing.T) {
	pf := &platform{status: 200, body: goodConfig(rev1)}
	prov, d, _ := newProv(t, pf.serve(t))
	if _, _, err := prov.Prepare(context.Background(), params(t, "scheduler", "tls-front", "probe"), true); err != nil {
		t.Fatal(err)
	}
	var leftovers []string
	for _, root := range []string{d.conf, d.install, d.stage} {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			base := filepath.Base(p)
			if err == nil && (strings.Contains(base, ".new-") || strings.HasPrefix(base, ".extract-") || strings.HasPrefix(base, "control-plane-")) {
				leftovers = append(leftovers, p)
			}
			return nil
		})
	}
	if len(leftovers) != 0 {
		t.Errorf("temporary files left: %v", leftovers)
	}
	if os.Getuid() != 0 {
		ro := filepath.Join(t.TempDir(), "ro")
		_ = os.MkdirAll(ro, 0o700)
		_ = os.Chmod(ro, 0o500)
		defer os.Chmod(ro, 0o700)
		prov2, _, _ := newProv(t, pf.serve(t))
		prov2.Dir = filepath.Join(ro, "cp")
		_, scrub, err := prov2.Prepare(context.Background(), params(t, "scheduler"), true)
		if err == nil {
			t.Fatal("an unwritable directory was not an error")
		}
		noSecret(t, "the error", err.Error(), scrub(err.Error()))
	}
}

func TestOnlyTheServicesAskedForAreWrittenAndMissingOnesAreNamed(t *testing.T) {
	cfg := goodConfig(rev1)
	delete(cfg, "probe")
	cfg["missing"] = []any{"control-plane/probe", "control-plane/unheard-of"}
	pf := &platform{status: 200, body: cfg}
	prov, d, _ := newProv(t, pf.serve(t))
	env, _, err := prov.Prepare(context.Background(), params(t, "scheduler", "probe"), true)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "NCS_CP_MISSING=control-plane/probe") {
		t.Errorf("the missing secret is not named:\n%s", joined)
	}
	if strings.Contains(joined, "unheard-of") {
		t.Error("a name this agent does not know was passed on")
	}
	if _, err := os.Stat(filepath.Join(d.conf, "probe.env")); !os.IsNotExist(err) {
		t.Error("probe.env was written without a probe secret")
	}
	if _, err := os.Stat(filepath.Join(d.conf, "tls", "key.pem")); !os.IsNotExist(err) {
		t.Error("the https front's key was written although the task did not ask for that service")
	}
	cfg2 := goodConfig(rev1)
	delete(cfg2, "tlsFront")
	prov2, _, _ := newProv(t, (&platform{status: 200, body: cfg2}).serve(t))
	env, _, err = prov2.Prepare(context.Background(), params(t, "tls-front"), false)
	if err != nil || !strings.Contains(strings.Join(env, "\n"), "NCS_CP_MISSING=control-plane/agent-tls") {
		t.Errorf("%v %v", env, err)
	}
}

func TestEveryAnswerThePlatformCanGiveIsHandledAndNoneQuotesASecret(t *testing.T) {
	big := `{"revision":"` + rev1 + `","pad":"` + strings.Repeat("x", MaxConfigBytes) + `"}`
	cases := []struct {
		name   string
		status int
		body   any
		raw    string
		want   string
	}{
		{"401", 401, map[string]any{"error": "That is not a code or credential this platform recognises"}, "", "401"},
		{"403", 403, map[string]any{"error": "A Nuit Cloud Server is not given the platform's service secrets"}, "", "not a control-plane host"},
		{"503", 503, map[string]any{"error": "try again"}, "", "retryable"},
		{"500", 500, nil, "boom", "500"},
		{"malformed json", 200, nil, `{"revision": ` + secSched, "could not be read"},
		{"not an object", 200, nil, `["` + secSched + `"]`, "could not be read"},
		{"no revision", 200, map[string]any{"scheduler": map[string]any{"env": map[string]any{"RENEWAL_SWEEP_TOKEN": secSched}}}, "", "no usable revision"},
		{"bad revision", 200, map[string]any{"revision": "not-a-hash"}, "", "no usable revision"},
		{"oversize", 200, nil, big, "larger than"},
		{"newline in a value", 200, map[string]any{"revision": rev1, "scheduler": map[string]any{"env": map[string]any{"RENEWAL_SWEEP_TOKEN": secSched + "\nINJECTED=1"}}}, "", "RENEWAL_SWEEP_TOKEN cannot be written"},
		{"control character", 200, map[string]any{"revision": rev1, "scheduler": map[string]any{"env": map[string]any{"A_TOKEN": secSched + "\x01"}}}, "", "A_TOKEN cannot be written"},
		{"bad env name", 200, map[string]any{"revision": rev1, "scheduler": map[string]any{"env": map[string]any{"bad name": secSched}}}, "", "not allowed"},
		{"lower-case env name", 200, map[string]any{"revision": rev1, "scheduler": map[string]any{"env": map[string]any{"path": secSched}}}, "", "not allowed"},
		{"bad key", 200, map[string]any{"revision": rev1, "tlsFront": map[string]any{"certPem": "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----", "keyPem": secKey}}, "", "not usable"},
		{"bad probe", 200, map[string]any{"revision": rev1, "probe": map[string]any{"name": "Bad Name", "updateToken": secProbeU + "xxxx", "bootstrapToken": secProbeB + "xxxx"}}, "", "not usable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prov, d, _ := newProv(t, (&platform{status: c.status, body: c.body, raw: c.raw}).serve(t))
			env, scrub, err := prov.Prepare(context.Background(), params(t, "scheduler", "tls-front", "probe"), true)
			if err == nil {
				t.Fatalf("an unusable answer was accepted: %v", env)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not say %q", err, c.want)
			}
			noSecret(t, "the error", err.Error(), scrub(err.Error()))
			for _, dir := range []string{d.conf, d.install} {
				if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
					t.Errorf("%s was made for an answer that was refused", dir)
				}
			}
		})
	}
	// a redirect is never followed
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	cl, _ := client.New(srv.URL, "1", true)
	cl.Credential = "nca_x"
	prov, _, _ := newProv(t, cl)
	if _, _, err := prov.Prepare(context.Background(), params(t, "scheduler"), true); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("%v", err)
	}
}

func TestAnUnreachablePlatformIsAnErrorThatSaysNothingSecret(t *testing.T) {
	cl, _ := client.New("http://127.0.0.1:1", "1", true)
	cl.Credential = "nca_credential"
	prov, _, _ := newProv(t, cl)
	_, _, err := prov.Prepare(context.Background(), params(t, "probe"), true)
	if err == nil || strings.Contains(err.Error(), "nca_credential") {
		t.Errorf("%v", err)
	}
}

func TestAnUpstreamThatIsNotAPrivateAddressIsNotUsed(t *testing.T) {
	for _, u := range []string{"8.8.8.8:4173", "example.com:4173", "10.0.0.1", "10.0.0.1:99999", "10.0.0.1:80;rm", ""} {
		cfg := goodConfig(rev1)
		cfg["tlsFront"].(map[string]any)["upstream"] = u
		prov, _, _ := newProv(t, (&platform{status: 200, body: cfg}).serve(t))
		env, _, err := prov.Prepare(context.Background(), params(t, "tls-front"), false)
		if err != nil || strings.Contains(strings.Join(env, "\n"), "NCS_CP_TLS_UPSTREAM") {
			t.Errorf("%q: %v %v", u, env, err)
		}
	}
}

// parseSystemdEnv models how systemd reads an EnvironmentFile line written by QuoteEnv: NAME="value" with `\\` and `\"` as escapes,
// no expansion of `$` or `%`, and a backslash before a newline joining lines (which QuoteEnv never leaves unescaped).
func parseSystemdEnv(t *testing.T, text string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		name, rest, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(rest, `"`) || !strings.HasSuffix(rest, `"`) || len(rest) < 2 {
			t.Fatalf("not a quoted line: %q", line)
		}
		body := rest[1 : len(rest)-1]
		var v strings.Builder
		for i := 0; i < len(body); i++ {
			switch body[i] {
			case '\\':
				i++
				if i >= len(body) || (body[i] != '\\' && body[i] != '"') {
					t.Fatalf("an escape systemd would read differently in %q", line)
				}
				v.WriteByte(body[i])
			case '"':
				t.Fatalf("an unescaped quote in %q", line)
			default:
				v.WriteByte(body[i])
			}
		}
		out[name] = v.String()
	}
	return out
}

func TestEnvironmentFileValuesSurviveSystemdsReading(t *testing.T) {
	values := map[string]string{
		"PLAIN":     "abc123",
		"DOLLAR":    `pa$$word${HOME}$(id)`,
		"PERCENT":   `100%s %h %% %n`,
		"TRAILING":  `ends with a backslash\`,
		"BACKSLASH": `a\b\\c\"d`,
		"QUOTES":    `say "hi" and 'bye'`,
		"PADDED":    "  spaced  ",
		"EMPTY":     "",
		"HASH":      `# not a comment ; nor this`,
		"EQUALS":    `a=b=c`,
		"UNICODE":   "naïve ✓",
	}
	var text strings.Builder
	for _, name := range []string{"PLAIN", "DOLLAR", "PERCENT", "TRAILING", "BACKSLASH", "QUOTES", "PADDED", "EMPTY", "HASH", "EQUALS", "UNICODE"} {
		text.WriteString(QuoteEnv(name, values[name]))
	}
	if got := parseSystemdEnv(t, text.String()); fmt.Sprint(got) != fmt.Sprint(values) {
		t.Errorf("round trip:\n got %v\nwant %v", got, values)
	}
	// and through the real writer: the same values written to scheduler.env
	env := map[string]any{}
	for k, v := range values {
		env[k] = v
	}
	cfg := goodConfig(rev1)
	cfg["scheduler"] = map[string]any{"env": env}
	prov, d, _ := newProv(t, (&platform{status: 200, body: cfg}).serve(t))
	if _, _, err := prov.Prepare(context.Background(), params(t, "scheduler"), true); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(d.conf, "scheduler.env"))
	if got := parseSystemdEnv(t, string(raw)); fmt.Sprint(got) != fmt.Sprint(values) {
		t.Errorf("scheduler.env round trip:\n got %v\nwant %v", got, values)
	}
	if strings.Count(string(raw), "\n") != len(values) {
		t.Error("a value spilled onto another line")
	}
}
