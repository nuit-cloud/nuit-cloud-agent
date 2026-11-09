package runner

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nuit-cloud/nuit-cloud-agent/internal/fwconfirm"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/hostname"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/lanscan"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/profiles"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/runtimeinstall"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/sshconfirm"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/survey"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/taskverify"
	"github.com/nuit-cloud/nuit-cloud-agent/internal/update"
)

func newRunner() Runner {
	return Runner{
		Version: "9.9.9",
		Now:     func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
		Survey:  func(context.Context) survey.Report { return survey.Report{Hostname: "box"} },
	}
}

func TestPingAnswers(t *testing.T) {
	r := newRunner().Run(context.Background(), &taskverify.Task{Name: "ping", TimeoutSeconds: 30})
	if r.Status != "done" || r.Output != "pong" || *r.ExitCode != 0 {
		t.Errorf("%+v", r)
	}
	res := r.Result.(map[string]any)
	if res["agentVersion"] != "9.9.9" || res["at"] != "2026-10-20T12:00:00Z" {
		t.Errorf("%+v", res)
	}
}

func TestSurveyReturnsTheReport(t *testing.T) {
	r := newRunner().Run(context.Background(), &taskverify.Task{Name: "survey", TimeoutSeconds: 120})
	if r.Status != "done" || r.Result.(survey.Report).Hostname != "box" {
		t.Errorf("%+v", r)
	}
}

type fakeProfiles struct {
	res   profiles.Result
	exit  int
	err   error
	calls []string
	env   []string
}

func (f *fakeProfiles) Run(_ context.Context, profile string, apply bool, env ...string) (profiles.Result, int, error) {
	f.env = env
	mode := "check"
	if apply {
		mode = "apply"
	}
	f.calls = append(f.calls, mode+" "+profile)
	r := f.res
	r.Profile, r.Mode = profile, mode
	return r, f.exit, f.err
}

func withProfiles(f *fakeProfiles) Runner {
	r := newRunner()
	r.Profiles = f
	return r
}

func profileTask(name string) *taskverify.Task {
	return &taskverify.Task{Name: name, TimeoutSeconds: 30, Params: map[string]any{"profile": "nuit-cloud-server"}}
}

func TestACheckThatFindsDriftIsDoneBecauseTheDriftIsTheAnswer(t *testing.T) {
	f := &fakeProfiles{exit: 1, res: profiles.Result{Features: []profiles.Feature{{Name: "user", Status: "ok"}, {Name: "ncs_ssh", Status: "drift"}}}}
	r := withProfiles(f).Run(context.Background(), profileTask("profile-check"))
	if r.Status != "done" || *r.ExitCode != 1 || r.Output != "check nuit-cloud-server: 1 of 2 features satisfied; still to do: ncs_ssh (drift)" {
		t.Errorf("%+v", r)
	}
	if len(f.calls) != 1 || f.calls[0] != "check nuit-cloud-server" {
		t.Errorf("a check must not apply: %v", f.calls)
	}
	if r.Result.(profiles.Result).Features[1].Status != "drift" {
		t.Error("the per-feature findings must be sent")
	}
}

func TestAnApplyIsFailedOnlyWhenAFeatureFailedNotWhenAPersonIsNeeded(t *testing.T) {
	good := &fakeProfiles{exit: 0, res: profiles.Result{Features: []profiles.Feature{{Name: "user", Status: "ok"}, {Name: "journal", Status: "applied"}}}}
	r := withProfiles(good).Run(context.Background(), profileTask("profile-apply"))
	if r.Status != "done" || *r.ExitCode != 0 || good.calls[0] != "apply nuit-cloud-server" {
		t.Errorf("%+v %v", r, good.calls)
	}

	// something is left for a person: the task did its job, so it is done, and says what is left
	needsPerson := &fakeProfiles{exit: 1, res: profiles.Result{RebootNeeded: true, Features: []profiles.Feature{{Name: "kvm", Status: "manual"}, {Name: "journal", Status: "applied"}}}}
	r = withProfiles(needsPerson).Run(context.Background(), profileTask("profile-apply"))
	if r.Status != "done" || *r.ExitCode != 1 || r.Output != "apply nuit-cloud-server: 1 of 2 features satisfied; still to do: kvm (manual); a reboot is needed" {
		t.Errorf("%+v", r)
	}
	if r.Result == nil {
		t.Error("the findings must still be sent")
	}

	// a feature that really failed is a failed apply
	broken := &fakeProfiles{exit: 1, res: profiles.Result{Features: []profiles.Feature{{Name: "ncs_packages", Status: "failed"}, {Name: "journal", Status: "applied"}}}}
	if r := withProfiles(broken).Run(context.Background(), profileTask("profile-apply")); r.Status != "failed" || r.Result == nil {
		t.Errorf("%+v", r)
	}

	// the scripts produced nothing at all and exited non-zero: failed
	empty := &fakeProfiles{exit: 2, res: profiles.Result{Features: []profiles.Feature{}}}
	if r := withProfiles(empty).Run(context.Background(), profileTask("profile-apply")); r.Status != "failed" {
		t.Errorf("%+v", r)
	}
}

func TestAProfileThatCannotRunFailsWithTheReason(t *testing.T) {
	for name, err := range map[string]error{"root": profiles.ErrNeedsRoot, "unknown": errors.New(`this agent does not carry a profile named "x"`)} {
		r := withProfiles(&fakeProfiles{err: err}).Run(context.Background(), profileTask("profile-apply"))
		if r.Status != "failed" || r.Output != err.Error() || r.Result != nil {
			t.Errorf("%s: %+v", name, r)
		}
	}
	rn := newRunner() // built without profiles
	if r := rn.Run(context.Background(), profileTask("profile-check")); r.Status != "failed" {
		t.Errorf("%+v", r)
	}
}

func TestAnUnknownTaskFailsAndNothingRuns(t *testing.T) {
	ran := false
	rn := newRunner()
	rn.Survey = func(context.Context) survey.Report { ran = true; return survey.Report{} }
	r := rn.Run(context.Background(), &taskverify.Task{Name: "rm-rf", TimeoutSeconds: 30})
	if r.Status != "failed" || ran {
		t.Errorf("%+v ran=%v", r, ran)
	}
}

func TestATaskThatPanicsIsAFailedTaskNotACrashedAgent(t *testing.T) {
	rn := newRunner()
	rn.Survey = func(context.Context) survey.Report { panic("boom") }
	r := rn.Run(context.Background(), &taskverify.Task{Name: "survey", TimeoutSeconds: 30})
	if r.Status != "failed" || r.Output != "the task panicked: boom" {
		t.Errorf("%+v", r)
	}
}

func TestTheTimeoutReachesTheTask(t *testing.T) {
	rn := newRunner()
	var deadline time.Time
	rn.Survey = func(ctx context.Context) survey.Report { deadline, _ = ctx.Deadline(); return survey.Report{} }
	start := time.Now()
	rn.Run(context.Background(), &taskverify.Task{Name: "survey", TimeoutSeconds: 120})
	if d := deadline.Sub(start); d < 119*time.Second || d > 121*time.Second {
		t.Errorf("deadline in %v, want ~120s", d)
	}
}

type fakeUpdater struct {
	out    update.Outcome
	err    error
	called []update.Params
}

func (f *fakeUpdater) Apply(_ context.Context, p update.Params) (update.Outcome, error) {
	f.called = append(f.called, p)
	return f.out, f.err
}

func updateTask(params map[string]any) *taskverify.Task {
	return &taskverify.Task{Name: "update-agent", TimeoutSeconds: 300, Params: params}
}

var goodUpdate = map[string]any{"version": "0.3.0", "sha256": map[string]any{"amd64": strings.Repeat("a", 64), "arm64": strings.Repeat("b", 64)}}

func TestUpdateAgentInstallsTheBuildAndAsksToRestartAfterReporting(t *testing.T) {
	f := &fakeUpdater{out: update.Outcome{From: "0.2.0", To: "0.3.0", Changed: true}}
	r := newRunner()
	r.Updater, r.Restart = f, &atomic.Bool{}
	report := r.Run(context.Background(), updateTask(goodUpdate))
	if report.Status != "done" || *report.ExitCode != 0 || report.Output != "updated from 0.2.0 to 0.3.0; restarting to start the new version" {
		t.Errorf("%+v", report)
	}
	if !r.RestartRequested() {
		t.Error("a replaced binary must ask to be restarted")
	}
	if len(f.called) != 1 || f.called[0].Version != "0.3.0" || f.called[0].SHA256["amd64"] == "" {
		t.Errorf("the signed parameters must reach the updater: %+v", f.called)
	}
}

func TestUpdateAgentToTheVersionAlreadyRunningIsDoneWithoutRestarting(t *testing.T) {
	r := newRunner()
	r.Updater, r.Restart = &fakeUpdater{out: update.Outcome{From: "0.3.0", To: "0.3.0"}}, &atomic.Bool{}
	report := r.Run(context.Background(), updateTask(goodUpdate))
	if report.Status != "done" || report.Output != "already at 0.3.0" || r.RestartRequested() {
		t.Errorf("%+v restart=%v", report, r.RestartRequested())
	}
}

func TestUpdateAgentFailsWithTheReasonAndNeverRestarts(t *testing.T) {
	for name, tc := range map[string]struct {
		updater *fakeUpdater
		params  map[string]any
		want    string
	}{
		"the updater refuses": {&fakeUpdater{err: errors.New("the download does not match the checksum the platform signed")}, goodUpdate, "does not match"},
		"bad parameters":      {&fakeUpdater{}, map[string]any{"version": "latest"}, "no usable version"},
	} {
		r := newRunner()
		r.Updater, r.Restart = tc.updater, &atomic.Bool{}
		report := r.Run(context.Background(), updateTask(tc.params))
		if report.Status != "failed" || !strings.Contains(report.Output, tc.want) || r.RestartRequested() {
			t.Errorf("%s: %+v", name, report)
		}
		if name == "bad parameters" && len(tc.updater.called) != 0 {
			t.Error("nothing may be tried with parameters that do not validate")
		}
	}
	r := newRunner() // built without an updater
	if report := r.Run(context.Background(), updateTask(goodUpdate)); report.Status != "failed" {
		t.Errorf("%+v", report)
	}
}

func TestTheCatalogIncludesUpdateAgent(t *testing.T) {
	found := false
	for _, n := range newRunner().Names() {
		found = found || n == "update-agent"
	}
	if !found {
		t.Error("the agent must carry update-agent in its catalog")
	}
}

type fakeStager struct {
	err     error
	staged  int
	cleaned int
}

func (f *fakeStager) Stage(_ context.Context, _ runtimeinstall.Params) ([]string, func(), error) {
	f.staged++
	if f.err != nil {
		return nil, nil, f.err
	}
	return []string{"NCS_RUNTIME_BUNDLE=/stage/bundle"}, func() { f.cleaned++ }, nil
}

func runtimeTask(name string) *taskverify.Task {
	hex := func(c string) string { return strings.Repeat(c, 64) }
	return &taskverify.Task{Name: name, TimeoutSeconds: 30, Params: map[string]any{"profile": "nuit-container-service", "runtime": map[string]any{
		"bundleVersion": "v4", "bundleSha256": hex("a"), "containerdVersion": "2.2.10", "containerdSha256": hex("b"), "crictlVersion": "1.37.0", "crictlSha256": hex("d"), "rootFingerprint": hex("c"),
	}}}
}

func TestAProfileApplyThatCarriesTheRuntimeStagesItAndHandsItToTheScripts(t *testing.T) {
	f := &fakeProfiles{res: profiles.Result{Features: []profiles.Feature{{Name: "oci_saas_runtime", Status: "applied"}}}}
	st := &fakeStager{}
	r := withProfiles(f)
	r.Runtime = st
	rep := r.Run(context.Background(), runtimeTask("profile-apply"))
	if rep.Status != "done" || st.staged != 1 || st.cleaned != 1 || len(f.env) != 1 || f.env[0] != "NCS_RUNTIME_BUNDLE=/stage/bundle" {
		t.Errorf("%+v staged=%d cleaned=%d env=%v", rep, st.staged, st.cleaned, f.env)
	}
}

func TestAFailedStageStopsTheApplyBeforeAnythingRuns(t *testing.T) {
	f := &fakeProfiles{}
	r := withProfiles(f)
	r.Runtime = &fakeStager{err: errors.New("does not match the checksum")}
	rep := r.Run(context.Background(), runtimeTask("profile-apply"))
	if rep.Status != "failed" || len(f.calls) != 0 || !strings.Contains(rep.Output, "does not match") {
		t.Errorf("%+v calls=%v", rep, f.calls)
	}
}

func TestACheckNeedsNoDownloadButKnowsWhichBundleIsPinned(t *testing.T) {
	f := &fakeProfiles{}
	st := &fakeStager{}
	r := withProfiles(f)
	r.Runtime = st
	r.Run(context.Background(), runtimeTask("profile-check"))
	if st.staged != 0 || len(f.env) != 1 || f.env[0] != "NCS_RUNTIME_BUNDLE_SHA256="+strings.Repeat("a", 64) {
		t.Errorf("staged=%d env=%v", st.staged, f.env)
	}
}

func TestBadRuntimeParametersAreRefused(t *testing.T) {
	r := withProfiles(&fakeProfiles{})
	r.Runtime = &fakeStager{}
	task := runtimeTask("profile-apply")
	task.Params["runtime"].(map[string]any)["bundleSha256"] = "../../etc/passwd"
	if rep := r.Run(context.Background(), task); rep.Status != "failed" {
		t.Errorf("%+v", rep)
	}
}

func smokeTask(ns string) *taskverify.Task {
	return &taskverify.Task{Name: "runtime-smoke", TimeoutSeconds: 30, Params: map[string]any{"namespace": ns}}
}

func TestTheSmokeTestPassesOnlyWhenItSaysSoAndExitsZero(t *testing.T) {
	r := newRunner()
	r.Smoke = func(_ context.Context, ns string) (string, int, error) {
		return "pod up in " + ns + "\nSMOKE OK\n", 0, nil
	}
	if rep := r.Run(context.Background(), smokeTask("nuit-cloud-platform")); rep.Status != "done" || rep.Result.(map[string]any)["passed"] != true {
		t.Errorf("%+v", rep)
	}
	r.Smoke = func(context.Context, string) (string, int, error) {
		return "GET http://10.89.0.2/ -> 000\nSMOKE FAILED\n", 1, nil
	}
	if rep := r.Run(context.Background(), smokeTask("nuit-cloud-platform")); rep.Status != "failed" || !strings.Contains(rep.Output, "SMOKE FAILED") {
		t.Errorf("%+v", rep)
	}
	r.Smoke = func(context.Context, string) (string, int, error) { return "SMOKE OK but the script died\n", 2, nil }
	if rep := r.Run(context.Background(), smokeTask("x")); rep.Status != "failed" {
		t.Errorf("an exit status of 2 passed: %+v", rep)
	}
}

func TestTheSmokeTestRefusesAnOddNamespaceAndReportsAMissingScript(t *testing.T) {
	r := newRunner()
	called := false
	r.Smoke = func(context.Context, string) (string, int, error) { called = true; return "", 0, nil }
	if rep := r.Run(context.Background(), smokeTask("../etc; rm -rf /")); rep.Status != "failed" || called {
		t.Errorf("%+v called=%v", rep, called)
	}
	r.Smoke = func(context.Context, string) (string, int, error) {
		return "", 0, errors.New("the runtime's smoke test is not on this machine")
	}
	if rep := r.Run(context.Background(), smokeTask("nuit-cloud-platform")); rep.Status != "failed" || !strings.Contains(rep.Output, "not on this machine") {
		t.Errorf("%+v", rep)
	}
}

func TestTheManagementAddressReachesTheScriptsAsEnvironment(t *testing.T) {
	f := &fakeProfiles{res: profiles.Result{Features: []profiles.Feature{{Name: "ncs_address", Status: "applied"}}}}
	r := withProfiles(f)
	r.PlatformURL = "https://agent.example"
	task := &taskverify.Task{Name: "profile-apply", TimeoutSeconds: 30, Params: map[string]any{"profile": "nuit-cloud-address", "network": map[string]any{"managementAddress": "10.10.10.162"}}}
	if rep := r.Run(context.Background(), task); rep.Status != "done" || len(f.env) != 2 || f.env[0] != "NCS_MANAGEMENT_ADDRESS=10.10.10.162" || f.env[1] != "NCS_PLATFORM_URL=https://agent.example" {
		t.Errorf("%+v env=%v", rep, f.env)
	}
	for _, bad := range []string{"", "10.10.10.999", "10.10.10.1; reboot", "fe80::1", "$(id)"} {
		f2 := &fakeProfiles{}
		r2 := withProfiles(f2)
		task.Params["network"] = map[string]any{"managementAddress": bad}
		if rep := r2.Run(context.Background(), task); rep.Status != "failed" || len(f2.calls) != 0 {
			t.Errorf("%q: %+v calls=%v", bad, rep, f2.calls)
		}
	}
}

func TestAnAddressScanReportsWhatAnswered(t *testing.T) {
	r := newRunner()
	r.Scan = func(_ context.Context, first, last string) (lanscan.Result, error) {
		if first != "10.10.10.161" || last != "10.10.10.199" {
			t.Errorf("scanned %s-%s", first, last)
		}
		return lanscan.Result{From: "eth0", Method: "arping", Scanned: 38, Answered: []lanscan.Answer{{Address: "10.10.10.170", MAC: "aa:bb"}}}, nil
	}
	task := &taskverify.Task{Name: "address-scan", TimeoutSeconds: 30, Params: map[string]any{"first": "10.10.10.161", "last": "10.10.10.199"}}
	rep := r.Run(context.Background(), task)
	if rep.Status != "done" || rep.Output != "scanned 38 addresses from eth0 (arping): 1 answered" {
		t.Errorf("%+v", rep)
	}
	r.Scan = func(context.Context, string, string) (lanscan.Result, error) {
		return lanscan.Result{}, errors.New("a scan only looks at private addresses")
	}
	if rep := r.Run(context.Background(), task); rep.Status != "failed" || !strings.Contains(rep.Output, "private") {
		t.Errorf("%+v", rep)
	}
}

func TestTheVLANSliceReachesTheScriptsAsEnvironmentAndBadPartsAreRefused(t *testing.T) {
	good := func() map[string]any {
		return map[string]any{"id": float64(5), "address": "10.20.0.18", "prefix": float64(24), "gateway": "10.20.0.1", "tenantAddresses": []any{"10.20.0.19", "10.20.0.20"}}
	}
	f := &fakeProfiles{res: profiles.Result{Features: []profiles.Feature{{Name: "ncs_vlan", Status: "applied"}}}}
	r := withProfiles(f)
	r.PlatformURL = "https://agent.example"
	task := &taskverify.Task{Name: "profile-apply", TimeoutSeconds: 30, Params: map[string]any{"profile": "nuit-cloud-vlan", "vlan": good()}}
	if rep := r.Run(context.Background(), task); rep.Status != "done" || strings.Join(f.env, "|") != "NCS_VLAN_ID=5|NCS_VLAN_PREFIX=24|NCS_VLAN_ADDRESS=10.20.0.18|NCS_VLAN_GATEWAY=10.20.0.1|NCS_VLAN_TENANT_ADDRESSES=10.20.0.19 10.20.0.20|NCS_PLATFORM_URL=https://agent.example" {
		t.Errorf("%+v env=%v", rep, f.env)
	}
	for name, mutate := range map[string]func(map[string]any){
		"id":         func(m map[string]any) { m["id"] = float64(5000) },
		"prefix":     func(m map[string]any) { m["prefix"] = float64(8) },
		"address":    func(m map[string]any) { m["address"] = "10.20.0.18; reboot" },
		"gateway":    func(m map[string]any) { m["gateway"] = "fe80::1" },
		"no tenant":  func(m map[string]any) { m["tenantAddresses"] = []any{} },
		"bad tenant": func(m map[string]any) { m["tenantAddresses"] = []any{"10.20.0.19", "$(id)"} },
	} {
		v := good()
		mutate(v)
		f2 := &fakeProfiles{}
		task.Params["vlan"] = v
		if rep := withProfiles(f2).Run(context.Background(), task); rep.Status != "failed" || len(f2.calls) != 0 {
			t.Errorf("%s: %+v calls=%v", name, rep, f2.calls)
		}
	}
}

func TestTheKeysThePlatformNamesReachTheScriptsAndBadOnesAreRefused(t *testing.T) {
	fp := "SHA256:" + strings.Repeat("A", 43)
	f := &fakeProfiles{}
	r := withProfiles(f)
	task := &taskverify.Task{Name: "profile-apply", TimeoutSeconds: 30, Params: map[string]any{"profile": "nuit-cloud-ssh", "ssh": map[string]any{"requireFingerprints": []any{fp, "SHA256:" + strings.Repeat("b", 43)}}}}
	if rep := r.Run(context.Background(), task); rep.Status != "done" || len(f.env) != 1 || f.env[0] != "NCS_SSH_REQUIRE_FINGERPRINTS="+fp+" SHA256:"+strings.Repeat("b", 43) {
		t.Errorf("%+v env=%v", rep, f.env)
	}
	for _, bad := range []any{[]any{}, []any{"nope"}, []any{fp + "; reboot"}, "SHA256:x"} {
		f2 := &fakeProfiles{}
		task.Params["ssh"] = map[string]any{"requireFingerprints": bad}
		if rep := withProfiles(f2).Run(context.Background(), task); rep.Status != "failed" || len(f2.calls) != 0 {
			t.Errorf("%v: %+v calls=%v", bad, rep, f2.calls)
		}
	}
}

func TestSSHConfirmReportsWhatTheConfirmerSaid(t *testing.T) {
	r := newRunner()
	r.SSHConfirm = func(context.Context) (sshconfirm.Result, error) {
		return sshconfirm.Result{Armed: true, Confirmed: true, Note: "SSH is hardened and the rollback is cancelled"}, nil
	}
	if rep := r.Run(context.Background(), &taskverify.Task{Name: "ssh-confirm", TimeoutSeconds: 30}); rep.Status != "done" || rep.Output != "SSH is hardened and the rollback is cancelled" {
		t.Errorf("%+v", rep)
	}
	r.SSHConfirm = func(context.Context) (sshconfirm.Result, error) {
		return sshconfirm.Result{Armed: true}, errors.New("sshd does not use the hardened settings")
	}
	if rep := r.Run(context.Background(), &taskverify.Task{Name: "ssh-confirm", TimeoutSeconds: 30}); rep.Status != "failed" {
		t.Errorf("%+v", rep)
	}
}

func TestFirewallParametersBecomeTheScriptsSettingsAndAreCheckedAgain(t *testing.T) {
	env, err := firewallEnvironment(map[string]any{"tcp": []any{float64(80), float64(443)}, "sshFrom": []any{"10.10.10.0/24"}})
	if err != nil || strings.Join(env, "|") != "NCS_FIREWALL_TCP=80,443|NCS_FIREWALL_SSH_FROM=10.10.10.0/24" {
		t.Fatalf("%v %v", env, err)
	}
	for _, bad := range []map[string]any{
		{"tcp": []any{float64(0)}}, {"tcp": []any{float64(70000)}}, {"tcp": []any{"80"}}, {"tcp": []any{80.5}},
		{"sshFrom": []any{"10.0.0.0"}}, {"sshFrom": []any{"300.0.0.0/8"}}, {"sshFrom": []any{"10.0.0.0/33"}}, {"sshFrom": []any{"$(id)/8"}},
	} {
		if _, err := firewallEnvironment(bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}

func TestFirewallConfirmReportsWhatTheConfirmerSaid(t *testing.T) {
	r := Runner{}
	r.FirewallConfirm = func(context.Context) (fwconfirm.Result, error) {
		return fwconfirm.Result{Armed: true, Confirmed: true, Note: "ok"}, nil
	}
	if rep := r.firewallConfirm(context.Background()); rep.Status != "done" {
		t.Errorf("%+v", rep)
	}
	r.FirewallConfirm = func(context.Context) (fwconfirm.Result, error) {
		return fwconfirm.Result{Armed: true}, errors.New("the table is not loaded")
	}
	if rep := r.firewallConfirm(context.Background()); rep.Status != "failed" {
		t.Errorf("%+v", rep)
	}
}

func repairTask(action any) *taskverify.Task {
	return &taskverify.Task{Name: "runtime-repair", TimeoutSeconds: 30, Params: map[string]any{"action": action}}
}

func TestRuntimeRepairOnlyAsksTheRuntimeToListOrEnd(t *testing.T) {
	r := newRunner()
	var asked []string
	r.Repair = func(_ context.Context, action string) (string, int, error) {
		asked = append(asked, action)
		return `{"leftovers":[{"pid":3023962}]}`, 0, nil
	}
	rep := r.Run(context.Background(), repairTask("list"))
	if rep.Status != "done" || rep.Result.(map[string]any)["runtime"].(map[string]any)["leftovers"] == nil {
		t.Errorf("%+v", rep)
	}
	for _, bad := range []any{"kill", "end; rm -rf /", "", nil, 3} {
		if rep := r.Run(context.Background(), repairTask(bad)); rep.Status != "failed" {
			t.Errorf("%v was accepted: %+v", bad, rep)
		}
	}
	if len(asked) != 1 {
		t.Errorf("the runtime was asked %v", asked)
	}
	r.Repair = func(context.Context, string) (string, int, error) {
		return "", 0, errors.New("the container runtime is not on this machine")
	}
	if rep := r.Run(context.Background(), repairTask("end")); rep.Status != "failed" || !strings.Contains(rep.Output, "not on this machine") {
		t.Errorf("%+v", rep)
	}
}

func TestSetHostnameValidatesAgainAndOnlyThenChangesAnything(t *testing.T) {
	r := newRunner()
	var asked []string
	r.SetHostname = func(_ context.Context, name, address string) (hostname.Result, error) {
		asked = append(asked, name+"@"+address)
		return hostname.Result{Previous: "old", Hostname: name, Outcome: "changed", Method: "hostnamectl", Address: address, HostsLine: "address"}, nil
	}
	task := func(p map[string]any) *taskverify.Task {
		return &taskverify.Task{Name: "set-hostname", TimeoutSeconds: 60, Params: p}
	}
	rep := r.Run(context.Background(), task(map[string]any{"hostname": " Box-2 "}))
	res, _ := rep.Result.(map[string]any)
	if rep.Status != "done" || res["hostname"] != "box-2" || res["previous"] != "old" || res["outcome"] != "changed" || res["method"] != "hostnamectl" {
		t.Errorf("%+v", rep)
	}
	rep = r.Run(context.Background(), task(map[string]any{"hostname": "box-3", "address": "10.10.10.170"}))
	res, _ = rep.Result.(map[string]any)
	if rep.Status != "done" || res["address"] != "10.10.10.170" || res["hostsLine"] != "address" {
		t.Errorf("%+v", rep)
	}
	for _, bad := range []map[string]any{{}, {"hostname": "a.b"}, {"hostname": "-a"}, {"hostname": "a", "x": 1}, {"hostname": 3},
		{"hostname": "a", "address": "127.0.0.1"}, {"hostname": "a", "address": "10.0.0.1; rm"}, {"hostname": "a", "address": 5}} {
		if rep := r.Run(context.Background(), task(bad)); rep.Status != "failed" {
			t.Errorf("%v was accepted: %+v", bad, rep)
		}
	}
	if len(asked) != 2 || asked[0] != "box-2@" || asked[1] != "box-3@10.10.10.170" {
		t.Errorf("the machine was asked to change to %v", asked)
	}
	r.SetHostname = func(context.Context, string, string) (hostname.Result, error) {
		return hostname.Result{}, errors.New("no")
	}
	if rep := r.Run(context.Background(), task(map[string]any{"hostname": "box"})); rep.Status != "failed" {
		t.Errorf("%+v", rep)
	}
	found := false
	for _, n := range r.Names() {
		found = found || n == "set-hostname"
	}
	if !found {
		t.Error("set-hostname is not in the catalog")
	}
}

func TestTheResolversReachTheScriptsAsEnvironmentAndOnlyAsAddresses(t *testing.T) {
	f := &fakeProfiles{res: profiles.Result{Features: []profiles.Feature{{Name: "ncs_resolvers", Status: "applied"}}}}
	r := withProfiles(f)
	task := &taskverify.Task{Name: "profile-apply", TimeoutSeconds: 30, Params: map[string]any{"profile": "nuit-cloud-resolvers", "dns": map[string]any{"servers": []any{"10.20.0.22", "10.20.0.67"}}}}
	if rep := r.Run(context.Background(), task); rep.Status != "done" || len(f.env) != 1 || f.env[0] != "NCS_DNS_SERVERS=10.20.0.22 10.20.0.67" {
		t.Errorf("%+v env=%v", rep, f.env)
	}
	for _, bad := range []any{nil, []any{}, []any{"dns.example.com"}, []any{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}, []any{"10.0.0.1; rm"}, []any{"999.1.1.1"}} {
		f.env = nil
		task := &taskverify.Task{Name: "profile-apply", TimeoutSeconds: 30, Params: map[string]any{"profile": "nuit-cloud-resolvers", "dns": map[string]any{"servers": bad}}}
		if rep := r.Run(context.Background(), task); rep.Status != "failed" || len(f.env) != 0 {
			t.Errorf("%v was accepted: %+v env=%v", bad, rep, f.env)
		}
	}
}
