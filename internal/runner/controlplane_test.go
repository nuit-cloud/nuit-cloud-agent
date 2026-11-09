package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/client"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/controlplane"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/profiles"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
)

const (
	cpSecretSched = "FIXTURE-SCHEDULER-TOKEN-7f3a91c2"
	cpSecretKey   = "FIXTURE-TLS-PRIVATE-KEY-b81d44e0"
	cpSecretProbe = "FIXTURE-PROBE-UPDATE-5c0e22aaxxxx"
	cpRev         = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// fakeSource stands in for the platform's configuration endpoint.
type fakeSource struct {
	err  error
	body map[string]any
}

func (f fakeSource) GetJSON(_ context.Context, _ string, _ int64, out any) error {
	if f.err != nil {
		return f.err
	}
	raw, _ := json.Marshal(f.body)
	return json.Unmarshal(raw, out)
}

// cpArchive is a minimal good bundle; cpFetcher serves it under the name the platform uses.
func cpArchive(t *testing.T) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	put := func(name string, typ byte, body string) {
		h := &tar.Header{Name: name, Typeflag: typ, Mode: 0o644}
		if typ == tar.TypeDir {
			h.Mode = 0o755
		} else {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ != tar.TypeDir {
			_, _ = tw.Write([]byte(body))
		}
	}
	put("VERSION", tar.TypeReg, "1.0.0\n")
	put("ofelia", tar.TypeReg, "binary")
	put("scheduler", tar.TypeDir, "")
	put("scheduler/config.ini", tar.TypeReg, "[job-local \"a\"]\n")
	put("scheduler/monitor-run.sh", tar.TypeReg, "#!/bin/sh\n")
	put("scheduler/report-job.sh", tar.TypeReg, "#!/bin/sh\n")
	put("probe", tar.TypeDir, "")
	put("probe/probe.sh", tar.TypeReg, "#!/bin/sh\n")
	_ = tw.Close()
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	_, _ = zw.Write(raw.Bytes())
	_ = zw.Close()
	return out.Bytes()
}

type cpFetcher struct{ data []byte }

func (f cpFetcher) Fetch(_ context.Context, _, dst, want string, _ int64) (string, error) {
	s := sha256.Sum256(f.data)
	if hex.EncodeToString(s[:]) != want {
		return "", errors.New("does not match the checksum in the signed task")
	}
	return dst, os.WriteFile(dst, f.data, 0o600)
}

func cpConfig() map[string]any {
	return map[string]any{
		"revision":  cpRev,
		"scheduler": map[string]any{"env": map[string]any{"RENEWAL_SWEEP_TOKEN": cpSecretSched}},
		"tlsFront":  map[string]any{"certPem": "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n", "keyPem": "-----BEGIN PRIVATE KEY-----\n" + cpSecretKey + "\n-----END PRIVATE KEY-----\n"},
		"probe":     map[string]any{"name": "platform-probe", "updateToken": cpSecretProbe, "bootstrapToken": "bootstrap-token-0123456789abcdef"},
		"missing":   []any{},
	}
}

func cpTask(t *testing.T, name string, services ...string) *taskverify.Task {
	t.Helper()
	list := []any{}
	for _, s := range services {
		list = append(list, s)
	}
	s := sha256.Sum256(cpArchive(t))
	return &taskverify.Task{Name: name, TimeoutSeconds: 60, Params: map[string]any{"profile": "nuit-cloud-control-plane", "controlPlane": map[string]any{
		"services": list, "bundle": map[string]any{"version": "1.0.0", "arch": runtime.GOARCH, "sha256": hex.EncodeToString(s[:])}}}}
}

func cpRunner(t *testing.T, src fakeSource, f *fakeProfiles) (Runner, string) {
	t.Helper()
	root := t.TempDir()
	r := withProfiles(f)
	r.ControlPlane = controlplane.Provisioner{Source: src, Fetcher: cpFetcher{cpArchive(t)}, Dir: filepath.Join(root, "cp"), InstallDir: filepath.Join(root, "opt"), StageDir: filepath.Join(root, "stage"), PlatformURL: "https://agent.example"}
	return r, root
}

func reportText(t *testing.T, rep client.Report) string {
	t.Helper()
	raw, _ := json.Marshal(rep)
	return rep.Output + "\n" + string(raw)
}

func assertNoSecrets(t *testing.T, what, text string) {
	t.Helper()
	for _, s := range []string{cpSecretSched, cpSecretKey, cpSecretProbe} {
		if strings.Contains(text, s) {
			t.Errorf("%s holds a secret value (%s…)", what, s[:14])
		}
	}
}

func TestTheControlPlaneApplyGivesTheScriptsOnlyPlacesAndNeverASecret(t *testing.T) {
	f := &fakeProfiles{res: profiles.Result{Features: []profiles.Feature{{Name: "ncs_cp_scheduler", Status: "applied"}}}}
	r, root := cpRunner(t, fakeSource{body: cpConfig()}, f)
	rep := r.Run(context.Background(), cpTask(t, "profile-apply", "scheduler", "tls-front", "probe"))
	if rep.Status != "done" || len(f.calls) != 1 || f.calls[0] != "apply nuit-cloud-control-plane" {
		t.Fatalf("%+v %v", rep, f.calls)
	}
	env := strings.Join(f.env, "\n")
	for _, want := range []string{"NCS_CP_SERVICES=scheduler tls-front probe", "NCS_CP_CONFIG_DIR=" + filepath.Join(root, "cp"), "NCS_CP_INSTALL_DIR=" + filepath.Join(root, "opt"), "NCS_CP_REVISION=" + cpRev, "NCS_CP_BUNDLE_VERSION=1.0.0", "NCS_CP_BUNDLE_SHA256="} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %s in\n%s", want, env)
		}
	}
	assertNoSecrets(t, "the environment handed to the scripts", env)
	if b, _ := os.ReadFile(filepath.Join(root, "cp", "scheduler.env")); !strings.Contains(string(b), cpSecretSched) {
		t.Error("the agent did not write the secret itself")
	}
	if _, err := os.Stat(filepath.Join(root, "opt", "1.0.0", "ofelia")); err != nil {
		t.Error("the bundle was not unpacked by the agent")
	}
	assertNoSecrets(t, "the task's report", reportText(t, rep))
}

func TestTheControlPlaneCheckWritesAndDownloadsNothing(t *testing.T) {
	f := &fakeProfiles{exit: 1, res: profiles.Result{Features: []profiles.Feature{{Name: "ncs_cp_scheduler", Status: "drift"}}}}
	r, root := cpRunner(t, fakeSource{body: cpConfig()}, f)
	rep := r.Run(context.Background(), cpTask(t, "profile-check", "scheduler"))
	if rep.Status != "done" || f.calls[0] != "check nuit-cloud-control-plane" {
		t.Fatalf("%+v", rep)
	}
	for _, d := range []string{"cp", "opt", "stage"} {
		if _, err := os.Stat(filepath.Join(root, d)); !os.IsNotExist(err) {
			t.Errorf("a check made %s", d)
		}
	}
}

// a script that prints a secret (it must not, but a result must never carry one if it does) and every failure path
func TestNoSecretReachesTheOutputTheResultOrAnError(t *testing.T) {
	leaky := profiles.Result{
		Output:   "starting with token " + cpSecretSched + "\n" + cpSecretKey,
		Features: []profiles.Feature{{Name: "ncs_cp_probe", Status: "failed", Notes: []string{"the probe env has " + cpSecretProbe}}},
	}
	f := &fakeProfiles{exit: 1, res: leaky}
	r, _ := cpRunner(t, fakeSource{body: cpConfig()}, f)
	for _, name := range []string{"profile-apply", "profile-check"} {
		rep := r.Run(context.Background(), cpTask(t, name, "scheduler", "tls-front", "probe"))
		text := reportText(t, rep)
		assertNoSecrets(t, name+" with a leaky script", text)
		if !strings.Contains(text, "[redacted]") {
			t.Errorf("%s: the leak was not redacted: %s", name, text)
		}
	}
	f = &fakeProfiles{err: errors.New("could not start: token " + cpSecretSched)}
	r, _ = cpRunner(t, fakeSource{body: cpConfig()}, f)
	rep := r.Run(context.Background(), cpTask(t, "profile-apply", "scheduler"))
	if rep.Status != "failed" {
		t.Fatalf("%+v", rep)
	}
	assertNoSecrets(t, "a failed run's report", reportText(t, rep))
	bad := cpConfig()
	bad["scheduler"] = map[string]any{"env": map[string]any{"RENEWAL_SWEEP_TOKEN": cpSecretSched + "\nX=1"}}
	for name, src := range map[string]fakeSource{
		"403":      {err: &client.StatusError{Status: 403, Message: "no"}},
		"503":      {err: &client.StatusError{Status: 503, Message: "later"}},
		"401":      {err: &client.StatusError{Status: 401, Message: "unknown credential"}},
		"unusable": {body: bad},
		"too big":  {err: client.ErrTooLarge},
		"garbled":  {err: client.ErrUnreadable},
		"network":  {err: errors.New("dial tcp 10.0.0.1:443: connection refused")},
	} {
		r, root := cpRunner(t, src, &fakeProfiles{})
		rep := r.Run(context.Background(), cpTask(t, "profile-apply", "scheduler"))
		if rep.Status != "failed" {
			t.Errorf("%s: %+v", name, rep)
		}
		assertNoSecrets(t, name, reportText(t, rep))
		if _, err := os.Stat(filepath.Join(root, "cp")); !os.IsNotExist(err) {
			t.Errorf("%s: something was written", name)
		}
	}
	// a bundle that does not match the signed checksum: failed, nothing written, no secret in the text
	r, root := cpRunner(t, fakeSource{body: cpConfig()}, &fakeProfiles{})
	task := cpTask(t, "profile-apply", "scheduler")
	task.Params["controlPlane"].(map[string]any)["bundle"].(map[string]any)["sha256"] = strings.Repeat("0", 64)
	rep = r.Run(context.Background(), task)
	if rep.Status != "failed" || !strings.Contains(rep.Output, "checksum") {
		t.Errorf("%+v", rep)
	}
	assertNoSecrets(t, "a wrong checksum", reportText(t, rep))
	if _, err := os.Stat(filepath.Join(root, "cp")); !os.IsNotExist(err) {
		t.Error("secrets were written for a bundle that did not match")
	}
}

func TestTheControlPlaneRefusalsAreSaidPlainly(t *testing.T) {
	r, _ := cpRunner(t, fakeSource{err: &client.StatusError{Status: http.StatusForbidden, Message: "x"}}, &fakeProfiles{})
	if rep := r.Run(context.Background(), cpTask(t, "profile-apply", "probe")); rep.Status != "failed" || !strings.Contains(rep.Output, "not a control-plane host") {
		t.Errorf("%+v", rep)
	}
	r, _ = cpRunner(t, fakeSource{err: &client.StatusError{Status: http.StatusServiceUnavailable, Message: "x"}}, &fakeProfiles{})
	if rep := r.Run(context.Background(), cpTask(t, "profile-apply", "probe")); rep.Status != "failed" || !strings.Contains(rep.Output, "retryable") {
		t.Errorf("%+v", rep)
	}
}

func TestAHostileOrWrongControlPlaneParameterChangesNothing(t *testing.T) {
	f := &fakeProfiles{}
	r, root := cpRunner(t, fakeSource{body: cpConfig()}, f)
	sha := strings.Repeat("a", 64)
	b := func() map[string]any {
		return map[string]any{"version": "1.0.0", "arch": runtime.GOARCH, "sha256": sha}
	}
	other := "amd64"
	if runtime.GOARCH == "amd64" {
		other = "arm64"
	}
	wrongArch := b()
	wrongArch["arch"] = other
	extraKey := b()
	extraKey["url"] = "http://evil"
	for name, params := range map[string]map[string]any{
		"none":             {"profile": "nuit-cloud-control-plane"},
		"images (the old)": {"profile": "nuit-cloud-control-plane", "controlPlane": map[string]any{"services": []any{"probe"}, "images": map[string]any{"probe": "p@sha256:" + sha}}},
		"the wrong arch":   {"profile": "nuit-cloud-control-plane", "controlPlane": map[string]any{"services": []any{"probe"}, "bundle": wrongArch}},
		"a bundle key":     {"profile": "nuit-cloud-control-plane", "controlPlane": map[string]any{"services": []any{"probe"}, "bundle": extraKey}},
		"extra key":        {"profile": "nuit-cloud-control-plane", "controlPlane": map[string]any{"services": []any{"probe"}, "bundle": b(), "shell": "rm -rf /"}},
		"unknown":          {"profile": "nuit-cloud-control-plane", "controlPlane": map[string]any{"services": []any{"sshd"}, "bundle": b()}},
		"other, given":     {"profile": "nuit-cloud-server", "controlPlane": map[string]any{"services": []any{"probe"}, "bundle": b()}},
	} {
		rep := r.Run(context.Background(), &taskverify.Task{Name: "profile-apply", TimeoutSeconds: 30, Params: params})
		if rep.Status != "failed" {
			t.Errorf("%s was accepted: %+v", name, rep)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("scripts ran: %v", f.calls)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("something was written: %v", entries)
	}
	plain := withProfiles(&fakeProfiles{})
	if rep := plain.Run(context.Background(), cpTask(t, "profile-apply", "probe")); rep.Status != "failed" {
		t.Errorf("%+v", rep)
	}
}
